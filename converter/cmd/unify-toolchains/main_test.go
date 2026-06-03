package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/internal/fileapi"
	"github.com/sstriker/buildstream-bazel/converter/internal/toolchain"
	"github.com/sstriker/buildstream-bazel/converter/internal/toolchain/probejson"
)

// TestRun_EndToEnd exercises the whole flow: fixture probe.json
// artifacts → unify-toolchains → repo-root with the four expected
// files. Uses the recorded hello-world fileapi reply as the per-cell
// content (one reply, multiple platforms × variants — sufficient
// for shape verification; full per-platform divergence is for the
// real-cmake render gate).
func TestRun_EndToEnd(t *testing.T) {
	tmp := t.TempDir()
	probeDir := filepath.Join(tmp, "cells")
	if err := os.MkdirAll(probeDir, 0o755); err != nil {
		t.Fatal(err)
	}

	r, err := fileapi.Load("../../testdata/fileapi/hello-world")
	if err != nil {
		t.Fatalf("Load fileapi fixture: %v", err)
	}
	// Mint per-cell probe.json files for two platforms × three
	// variants. Each cell embeds the same hello-world reply (the
	// observe-fold below reduces this to a baseline + empty deltas
	// per platform, which is what we want for a shape test).
	platforms := []string{"linux_x86_64", "linux_aarch64"}
	variants := []toolchain.Variant{
		{Name: "baseline"},
		{Name: "debug", CacheVars: map[string]string{"CMAKE_BUILD_TYPE": "Debug"}},
		{Name: "release", CacheVars: map[string]string{"CMAKE_BUILD_TYPE": "Release"}},
	}
	for _, p := range platforms {
		for _, v := range variants {
			body, err := probejson.Marshal(v, r)
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			name := p + "." + v.Name + ".probe.json"
			if err := os.WriteFile(filepath.Join(probeDir, name), body, 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}

	platsPath := filepath.Join(tmp, "platforms.json")
	if err := os.WriteFile(platsPath, []byte(`[
		{"name": "linux_x86_64", "constraints": ["@platforms//os:linux", "@platforms//cpu:x86_64"]},
		{"name": "linux_aarch64", "constraints": ["@platforms//os:linux", "@platforms//cpu:arm64"]}
	]`), 0o644); err != nil {
		t.Fatal(err)
	}

	repoRoot := filepath.Join(tmp, "repo")
	args := []string{
		"--probe-cells", probeDir,
		"--platforms-json", platsPath,
		"--repo-root", repoRoot,
	}
	if err := run(args); err != nil {
		t.Fatalf("run: %v", err)
	}

	// Each expected file lands at the right path.
	for _, rel := range []string{
		"platforms/BUILD.bazel",
		"toolchains/BUILD.bazel",
		"toolchains/cc_toolchain_config.bzl",
		".bazelrc",
	} {
		full := filepath.Join(repoRoot, rel)
		if _, err := os.Stat(full); err != nil {
			t.Errorf("missing: %s (%v)", rel, err)
		}
	}

	// .bazelrc carries the operator-overrides try-import + sanitizer
	// aliases + platform aliases.
	rc, err := os.ReadFile(filepath.Join(repoRoot, ".bazelrc"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"try-import %workspace%/user.bazelrc",
		"build:asan --features=asan",
		"build:linux_x86_64 --platforms=//platforms:linux_x86_64",
		"build:linux_aarch64 --platforms=//platforms:linux_aarch64",
	} {
		if !strings.Contains(string(rc), want) {
			t.Errorf(".bazelrc missing %q\n%s", want, rc)
		}
	}

	// toolchains/BUILD.bazel: per-platform toolchain() instances, which
	// register_toolchains("//toolchains:all") activates via the ":all"
	// package wildcard (no target named "all" — that would shadow it).
	tcB, err := os.ReadFile(filepath.Join(repoRoot, "toolchains/BUILD.bazel"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`name = "linux_x86_64_toolchain"`,
		`name = "linux_aarch64_toolchain"`,
	} {
		if !strings.Contains(string(tcB), want) {
			t.Errorf("toolchains/BUILD.bazel missing %q", want)
		}
	}
	// The emitted package must NOT contain a target named "all": it would
	// shadow the register_toolchains("//toolchains:all") package wildcard
	// and break registration at analysis.
	if strings.Contains(string(tcB), `name = "all"`) {
		t.Errorf("toolchains/BUILD.bazel must not define a target named \"all\"\n%s", tcB)
	}
}

// TestRun_ElementSignal_FoldsBuiltinDirs exercises --element-signal:
// a per-element toolchain-signal reply dir carrying an implicit
// include directory the probe matrix never saw gets folded into the
// platform's cc_toolchain_config cxx_builtin_include_directories.
// Single-platform run, so the association heuristic's "the signal
// directory belongs to that one platform" fast path applies.
func TestRun_ElementSignal_FoldsBuiltinDirs(t *testing.T) {
	tmp := t.TempDir()
	probeDir := filepath.Join(tmp, "cells")
	if err := os.MkdirAll(probeDir, 0o755); err != nil {
		t.Fatal(err)
	}

	r, err := fileapi.Load("../../testdata/fileapi/hello-world")
	if err != nil {
		t.Fatal(err)
	}
	body, err := probejson.Marshal(toolchain.Variant{Name: "baseline"}, r)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(probeDir, "linux_x86_64.baseline.probe.json"), body, 0o644); err != nil {
		t.Fatal(err)
	}

	platsPath := filepath.Join(tmp, "platforms.json")
	if err := os.WriteFile(platsPath, []byte(`[
		{"name": "linux_x86_64", "constraints": ["@platforms//os:linux", "@platforms//cpu:x86_64"]}
	]`), 0o644); err != nil {
		t.Fatal(err)
	}

	// Element-signal directory: a copy of the hello-world reply with
	// one extra implicit include dir the probe matrix never saw.
	const extraDir = "/opt/vendored-sdk/include"
	sigParent := filepath.Join(tmp, "signals")
	copyReplyWithExtraInclude(t, "../../testdata/fileapi/hello-world", filepath.Join(sigParent, "libgreet"), extraDir)

	repoRoot := filepath.Join(tmp, "repo")
	args := []string{
		"--probe-cells", probeDir,
		"--platforms-json", platsPath,
		"--repo-root", repoRoot,
		"--element-signal", sigParent,
	}
	if err := run(args); err != nil {
		t.Fatalf("run: %v", err)
	}

	tcB, err := os.ReadFile(filepath.Join(repoRoot, "toolchains/BUILD.bazel"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(tcB), extraDir) {
		t.Errorf("toolchains/BUILD.bazel missing folded element-signal include dir %q\n%s", extraDir, tcB)
	}
}

// copyReplyWithExtraInclude copies a cmake fileapi reply dir from src
// to dst, appending extra to every toolchain's implicit
// includeDirectories along the way. The index still references the
// (unchanged) object filenames, so fileapi.Load resolves the patched
// reply normally.
func copyReplyWithExtraInclude(t *testing.T, src, dst, extra string) {
	t.Helper()
	if err := os.MkdirAll(dst, 0o755); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		b, err := os.ReadFile(filepath.Join(src, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if strings.HasPrefix(e.Name(), "toolchains-v1-") {
			b = patchToolchainsInclude(t, b, extra)
		}
		if err := os.WriteFile(filepath.Join(dst, e.Name()), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func patchToolchainsInclude(t *testing.T, body []byte, extra string) []byte {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatal(err)
	}
	tcs, ok := doc["toolchains"].([]any)
	if !ok {
		t.Fatal("toolchains json: no toolchains array")
	}
	for _, tc := range tcs {
		comp := tc.(map[string]any)["compiler"].(map[string]any)
		impl, ok := comp["implicit"].(map[string]any)
		if !ok {
			impl = map[string]any{}
			comp["implicit"] = impl
		}
		inc, _ := impl["includeDirectories"].([]any)
		impl["includeDirectories"] = append(inc, extra)
	}
	out, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// TestRegisterToolchainsCallPresent covers the tolerance of the
// MODULE.bazel banner check: substring-only matching produced
// false negatives on legitimate formatting variants. The regexp
// must accept whitespace + single/double quotes + multi-arg calls.
func TestRegisterToolchainsCallPresent(t *testing.T) {
	cases := map[string]bool{
		// canonical
		`register_toolchains("//toolchains:all")`: true,
		// single quotes
		`register_toolchains('//toolchains:all')`: true,
		// whitespace
		"register_toolchains(\n    \"//toolchains:all\",\n)": true,
		// extra args before / after
		`register_toolchains(":foo", "//toolchains:all")`: true,
		`register_toolchains("//toolchains:all", ":bar")`: true,
		// not present
		``:                                    false,
		`module(name="x")`:                    false,
		`register_toolchains("//foo:bar")`:    false,
		`register_toolchains("//toolchains")`: false, // wrong target
		// commented-out occurrences must NOT count as registered.
		`# register_toolchains("//toolchains:all")`:                          false,
		"   # register_toolchains(\"//toolchains:all\")\nmodule(name=\"x\")": false,
		"#register_toolchains('//toolchains:all')":                           false,
		"# leading comment\n#register_toolchains('//toolchains:all')\n":      false,
		// inline comment AFTER a real call still counts as registered.
		`register_toolchains("//toolchains:all") # ok`: true,
		// inline comment with the call inside it must NOT count.
		`module()  # register_toolchains("//toolchains:all")`:                          false,
		`load(":foo.bzl", "x")  # register_toolchains('//toolchains:all') in template`: false,
	}
	for body, want := range cases {
		if got := registerToolchainsCallPresent([]byte(body)); got != want {
			t.Errorf("registerToolchainsCallPresent(%q) = %v, want %v", body, got, want)
		}
	}
}

// TestLoadPlatforms_RejectsUnsafeNames: platform names from the
// JSON manifest become Bazel target names, .bazelrc --config
// aliases, and probe filename halves. loadPlatforms must reject
// anything outside the [a-zA-Z0-9_-] charset early with a
// clear error rather than letting the bad name surface as a
// confusing "no probe cells found" warning or a Bazel parse
// error in toolchains/BUILD.bazel.
func TestLoadPlatforms_RejectsUnsafeNames(t *testing.T) {
	cases := map[string]string{
		"with dot":   `[{"name": "linux.x86_64", "constraints": ["@platforms//os:linux"]}]`,
		"with slash": `[{"name": "linux/x86_64", "constraints": ["@platforms//os:linux"]}]`,
		"with colon": `[{"name": "linux:x86_64", "constraints": ["@platforms//os:linux"]}]`,
		"with space": `[{"name": "linux x86_64", "constraints": ["@platforms//os:linux"]}]`,
	}
	for label, body := range cases {
		t.Run(label, func(t *testing.T) {
			tmp := t.TempDir()
			path := filepath.Join(tmp, "platforms.json")
			if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := loadPlatforms(path); err == nil {
				t.Errorf("expected error for %s", label)
			}
		})
	}
}

// TestGroupProbeCells_RejectsMalformedFilenames: filenames whose
// stem has the dot at the boundary slip past a naive `dot > 0`
// check. <platform>..probe.json (empty variant) and
// <platform>.probe.json (no second half before the .probe.json
// suffix) both need to be rejected explicitly so malformed
// artifacts don't get folded under the wrong platform.
func TestGroupProbeCells_RejectsMalformedFilenames(t *testing.T) {
	cases := map[string]string{
		"empty variant": "linux_x86_64..probe.json",
		"no platform":   ".linux_x86_64.probe.json",
	}
	plats := []platformSpec{
		{Name: "linux_x86_64", Constraints: []string{"@platforms//os:linux"}},
	}
	for label, fname := range cases {
		t.Run(label, func(t *testing.T) {
			tmp := t.TempDir()
			// Stub probe.json content; the parser only looks at
			// filename in the rejection path so the body's contents
			// don't matter.
			if err := os.WriteFile(filepath.Join(tmp, fname), []byte(`{"schemaVersion":1}`), 0o644); err != nil {
				t.Fatal(err)
			}
			_, err := groupProbeCells(tmp, plats)
			if err == nil {
				t.Errorf("expected error for %s; got nil", label)
			}
			if !strings.Contains(err.Error(), "<platform>.<variant>") {
				t.Errorf("error %q missing the format hint", err)
			}
		})
	}
}

// TestGroupProbeCells_RejectsUnsafeKit: Variant.Kit flows into emitted
// Bazel target names (the <platform>_<kit> slug), so a kit carrying
// label-unsafe characters must be rejected at the decode boundary — with
// the offending cell named — rather than producing an unparsable
// platforms/ or toolchains/ BUILD file downstream.
func TestGroupProbeCells_RejectsUnsafeKit(t *testing.T) {
	r, err := fileapi.Load("../../testdata/fileapi/hello-world")
	if err != nil {
		t.Fatalf("Load fileapi fixture: %v", err)
	}
	plats := []platformSpec{
		{Name: "linux_x86_64", Constraints: []string{"@platforms//os:linux"}},
	}
	for label, kit := range map[string]string{
		"with space": "gcc 13",
		"with colon": "gcc:13",
		"with slash": "gcc/13",
		"with dot":   "gcc.13",
	} {
		t.Run(label, func(t *testing.T) {
			tmp := t.TempDir()
			body, err := probejson.Marshal(toolchain.Variant{Name: "baseline", Kit: kit}, r)
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			// Filename uses a safe variant half; the unsafe identity is
			// inside the cell's Variant.Kit, which only surfaces on decode.
			if err := os.WriteFile(filepath.Join(tmp, "linux_x86_64.baseline.probe.json"), body, 0o644); err != nil {
				t.Fatal(err)
			}
			_, err = groupProbeCells(tmp, plats)
			if err == nil {
				t.Fatalf("expected error for kit %q; got nil", kit)
			}
			if !strings.Contains(err.Error(), "invalid kit") {
				t.Errorf("error %q should mention the invalid kit", err)
			}
		})
	}
}

// TestRun_NoCellsForPlatform demonstrates the "skip-with-warning"
// behaviour: a platform listed in --platforms-json with no probe
// cells in --probe-cells is dropped from the output (with a
// stderr warning) rather than producing an empty cc_toolchain.
func TestRun_NoCellsForPlatform(t *testing.T) {
	tmp := t.TempDir()
	probeDir := filepath.Join(tmp, "cells")
	if err := os.MkdirAll(probeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	r, err := fileapi.Load("../../testdata/fileapi/hello-world")
	if err != nil {
		t.Fatal(err)
	}
	body, err := probejson.Marshal(toolchain.Variant{Name: "baseline"}, r)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(probeDir, "linux_x86_64.baseline.probe.json"), body, 0o644); err != nil {
		t.Fatal(err)
	}

	// Platforms include linux_aarch64 but no aarch64 cells exist.
	platsPath := filepath.Join(tmp, "platforms.json")
	if err := os.WriteFile(platsPath, []byte(`[
		{"name": "linux_x86_64", "constraints": ["@platforms//os:linux", "@platforms//cpu:x86_64"]},
		{"name": "linux_aarch64", "constraints": ["@platforms//os:linux", "@platforms//cpu:arm64"]}
	]`), 0o644); err != nil {
		t.Fatal(err)
	}
	repoRoot := filepath.Join(tmp, "repo")
	args := []string{
		"--probe-cells", probeDir,
		"--platforms-json", platsPath,
		"--repo-root", repoRoot,
	}
	if err := run(args); err != nil {
		t.Fatalf("run: %v", err)
	}
	platsB, err := os.ReadFile(filepath.Join(repoRoot, "platforms/BUILD.bazel"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(platsB), `name = "linux_aarch64"`) {
		t.Error("aarch64 platform was emitted despite having no probe cells")
	}
	if !strings.Contains(string(platsB), `name = "linux_x86_64"`) {
		t.Error("x86_64 platform missing")
	}
}
