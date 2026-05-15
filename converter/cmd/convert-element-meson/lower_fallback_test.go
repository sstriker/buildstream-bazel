package main

import (
	"strings"
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/emit/bazel"
	"github.com/sstriker/buildstream-bazel/converter/ir"
)

func ptr(s string) *string { return &s }

// TestEmitFallbackPlaceholder_RealFixtureShape covers the canonical
// shape: one static lib (devel + libdir_static), one shared lib
// (runtime + libdir_shared), one executable (runtime + bindir),
// plus an install_headers entry. Asserts that:
//
//   - the extract genrule's outs cover every install path,
//   - cc_import emits static_library / shared_library against the
//     resolved paths,
//   - sh_binary emits srcs against the executable's install path,
//   - headers fold into every library's hdrs (the v1 coarse fold).
func TestEmitFallbackPlaceholder_RealFixtureShape(t *testing.T) {
	intro := &Introspect{
		ProjectInfo: ProjectInfo{Name: "greet"},
		BuildOptions: []BuildOption{
			{Name: "prefix", Section: "directory", Value: "/"},
			{Name: "bindir", Section: "directory", Value: "bin"},
			{Name: "libdir", Section: "directory", Value: "lib"},
			{Name: "includedir", Section: "directory", Value: "include"},
		},
		InstallPlan: InstallPlan{
			Targets: map[string]InstallPlanEntry{
				"/bd/libgreet.a": {
					Destination: "{libdir_static}/libgreet.a",
					Tag:         "devel",
					Subproject:  nil,
				},
				"/bd/libgreetshared.so": {
					Destination: "{libdir_shared}/libgreetshared.so",
					Tag:         "runtime",
					Subproject:  nil,
				},
				"/bd/greet-bin": {
					Destination: "{bindir}/greet-bin",
					Tag:         "runtime",
					Subproject:  nil,
				},
			},
			Headers: map[string]InstallPlanEntry{
				"/src/include/greet.h": {
					Destination: "{includedir}/greet.h",
					Tag:         "devel",
					Subproject:  nil,
				},
			},
		},
	}
	pkg, err := emitFallbackPlaceholder(intro, LowerOptions{SourceRoot: "/src"})
	if err != nil {
		t.Fatalf("emitFallbackPlaceholder: %v", err)
	}
	out, err := bazel.Emit(pkg)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	got := string(out)

	// Extract genrule + per-target stubs.
	for _, want := range []string{
		`name = "_install_tree_extract"`,
		`"install_tree.tar"`,
		`"install_tree/lib/libgreet.a"`,
		`"install_tree/lib/libgreetshared.so"`,
		`"install_tree/bin/greet-bin"`,
		`"install_tree/include/greet.h"`,
		// Static lib stub.
		`name = "greet"`,
		`static_library = "install_tree/lib/libgreet.a"`,
		// Shared lib stub.
		`name = "greetshared"`,
		`shared_library = "install_tree/lib/libgreetshared.so"`,
		// Executable stub.
		`name = "greet-bin"`,
		`srcs = ["install_tree/bin/greet-bin"]`,
		// Header fold: every library carries the header.
		`hdrs = ["install_tree/include/greet.h"]`,
		// Tags for audit queries.
		`meson-codegen-target-fallback`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing marker %q\n--- output ---\n%s", want, got)
		}
	}

	// Static lib should be cc_import; sh_binary for the exe.
	var sawCCImport, sawShBinary, sawExtract bool
	for _, tgt := range pkg.Targets {
		switch tgt.Name {
		case "greet", "greetshared":
			if tgt.Kind != ir.KindCCImport {
				t.Errorf("target %q kind=%v want KindCCImport", tgt.Name, tgt.Kind)
			}
			sawCCImport = true
		case "greet-bin":
			if tgt.Kind != ir.KindShBinary {
				t.Errorf("target greet-bin kind=%v want KindShBinary", tgt.Kind)
			}
			sawShBinary = true
		case "_install_tree_extract":
			if tgt.Kind != ir.KindGenrule {
				t.Errorf("extract kind=%v want KindGenrule", tgt.Kind)
			}
			sawExtract = true
		}
	}
	if !sawCCImport {
		t.Errorf("no cc_import stub emitted")
	}
	if !sawShBinary {
		t.Errorf("no sh_binary stub emitted")
	}
	if !sawExtract {
		t.Errorf("no extract genrule emitted")
	}
}

// TestEmitFallbackPlaceholder_SubprojectSkipped asserts that
// install-plan rows tagged with a non-null subproject are filtered
// out — native lowering refuses subprojects entirely, and the
// fallback shouldn't surface their artefacts as labels (they
// aren't part of the consumer-visible install contract).
func TestEmitFallbackPlaceholder_SubprojectSkipped(t *testing.T) {
	intro := &Introspect{
		ProjectInfo: ProjectInfo{Name: "p"},
		BuildOptions: []BuildOption{
			{Name: "libdir", Section: "directory", Value: "lib"},
		},
		InstallPlan: InstallPlan{
			Targets: map[string]InstallPlanEntry{
				"/bd/libmain.a": {
					Destination: "{libdir_static}/libmain.a",
					Tag:         "devel",
					Subproject:  nil,
				},
				"/bd/subprojects/foo/libsub.a": {
					Destination: "{libdir_static}/libsub.a",
					Tag:         "devel",
					Subproject:  ptr("foo"),
				},
			},
		},
	}
	pkg, err := emitFallbackPlaceholder(intro, LowerOptions{})
	if err != nil {
		t.Fatalf("emitFallbackPlaceholder: %v", err)
	}
	// Exactly one library stub + one extract genrule.
	var names []string
	for _, tgt := range pkg.Targets {
		names = append(names, tgt.Name)
	}
	for _, want := range []string{"main", "_install_tree_extract"} {
		found := false
		for _, n := range names {
			if n == want {
				found = true
			}
		}
		if !found {
			t.Errorf("expected target %q in %v", want, names)
		}
	}
	for _, unwanted := range []string{"sub"} {
		for _, n := range names {
			if n == unwanted {
				t.Errorf("subproject target %q should have been filtered out (names=%v)", unwanted, names)
			}
		}
	}
}

// TestEmitFallbackPlaceholder_SharedSONameVariants verifies the
// SONAME-versioned shared-library basename matchers — meson
// often ships versioned shared libs (libfoo.so.1, libfoo.so.1.2.3,
// libfoo.dylib on macOS). Each should:
//  1. classify as artefactSharedLib,
//  2. strip the lib prefix + version suffix to derive the target name,
//  3. emit cc_import with shared_library pointing at the versioned path.
func TestEmitFallbackPlaceholder_SharedSONameVariants(t *testing.T) {
	cases := []struct {
		name        string
		destination string
		wantName    string
		wantPath    string
	}{
		{"unversioned-so", "{libdir_shared}/libfoo.so", "foo", "install_tree/lib/libfoo.so"},
		{"major-soname", "{libdir_shared}/libfoo.so.1", "foo", "install_tree/lib/libfoo.so.1"},
		{"full-soname", "{libdir_shared}/libfoo.so.1.2.3", "foo", "install_tree/lib/libfoo.so.1.2.3"},
		{"macos-dylib", "{libdir_shared}/libfoo.dylib", "foo", "install_tree/lib/libfoo.dylib"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			intro := &Introspect{
				BuildOptions: []BuildOption{
					{Name: "libdir", Section: "directory", Value: "lib"},
				},
				InstallPlan: InstallPlan{
					Targets: map[string]InstallPlanEntry{
						"/bd/x": {Destination: c.destination, Tag: "runtime"},
					},
				},
			}
			pkg, err := emitFallbackPlaceholder(intro, LowerOptions{})
			if err != nil {
				t.Fatalf("emit: %v", err)
			}
			var sawStub bool
			for _, tgt := range pkg.Targets {
				if tgt.Name == c.wantName {
					if tgt.Kind != ir.KindCCImport {
						t.Errorf("kind=%v want KindCCImport", tgt.Kind)
					}
					if tgt.SharedLibrary != c.wantPath {
						t.Errorf("SharedLibrary=%q want %q", tgt.SharedLibrary, c.wantPath)
					}
					sawStub = true
				}
			}
			if !sawStub {
				t.Errorf("no stub for target %q", c.wantName)
			}
		})
	}
}

// TestEmitFallbackPlaceholder_UnresolvedPlaceholderDropped asserts
// that a destination with an unknown placeholder ({weirdthing})
// is silently skipped rather than emitting a stub with literal
// "{weirdthing}" in the path. The install_tree.tar genrule
// wouldn't produce such a path, so a stub claiming it would fail
// at consumer build time.
func TestEmitFallbackPlaceholder_UnresolvedPlaceholderDropped(t *testing.T) {
	intro := &Introspect{
		BuildOptions: []BuildOption{
			{Name: "libdir", Section: "directory", Value: "lib"},
		},
		InstallPlan: InstallPlan{
			Targets: map[string]InstallPlanEntry{
				"/bd/libok.a": {Destination: "{libdir_static}/libok.a", Tag: "devel"},
				"/bd/libbad":  {Destination: "{weirdthing}/libbad.a", Tag: "devel"},
			},
		},
	}
	pkg, err := emitFallbackPlaceholder(intro, LowerOptions{})
	if err != nil {
		t.Fatalf("emit: %v", err)
	}
	for _, tgt := range pkg.Targets {
		if tgt.Name == "bad" {
			t.Errorf("stub emitted for unresolved-placeholder target: %+v", tgt)
		}
		if strings.Contains(tgt.StaticLibrary, "{") {
			t.Errorf("StaticLibrary still contains placeholder: %q", tgt.StaticLibrary)
		}
	}
}

// TestEmitFallbackPlaceholder_EmptyPlanProducesEmptyPackage covers
// the degenerate case: an element whose install plan has no
// `targets` rows. The fallback returns a package with no
// stubs + no extract genrule. main.go's caller checks
// len(InstallPlan.Targets) > 0 before invoking the fallback, so
// this state shouldn't surface in practice — the test pins the
// internal contract.
func TestEmitFallbackPlaceholder_EmptyPlanProducesEmptyPackage(t *testing.T) {
	intro := &Introspect{
		ProjectInfo:  ProjectInfo{Name: "empty"},
		BuildOptions: []BuildOption{{Name: "libdir", Section: "directory", Value: "lib"}},
	}
	pkg, err := emitFallbackPlaceholder(intro, LowerOptions{})
	if err != nil {
		t.Fatalf("emit: %v", err)
	}
	if len(pkg.Targets) != 0 {
		t.Errorf("expected zero targets for empty install plan; got %d: %+v", len(pkg.Targets), pkg.Targets)
	}
}

// TestClassifyArtefact_TruthTable pins the (tag, basename) →
// artefact-kind dispatch.
func TestClassifyArtefact_TruthTable(t *testing.T) {
	cases := []struct {
		tag      string
		basename string
		want     artefactKind
	}{
		{"devel", "libfoo.a", artefactStaticLib},
		{"runtime", "libfoo.a", artefactStaticLib},
		{"runtime", "libfoo.so", artefactSharedLib},
		{"devel", "libfoo.so.1", artefactSharedLib},
		{"runtime", "libfoo.so.1.2.3", artefactSharedLib},
		{"runtime", "libfoo.dylib", artefactSharedLib},
		{"runtime", "greet-bin", artefactExecutable},
		{"runtime", "tool", artefactExecutable},
		{"devel", "greet-bin", artefactUnknown}, // devel-tagged binary: unusual, skip
		{"man", "greet.1", artefactUnknown},
		{"i18n", "greet.mo", artefactUnknown},
	}
	for _, c := range cases {
		t.Run(c.tag+"_"+c.basename, func(t *testing.T) {
			got := classifyArtefact(c.tag, c.basename)
			if got != c.want {
				t.Errorf("classifyArtefact(%q, %q) = %v, want %v", c.tag, c.basename, got, c.want)
			}
		})
	}
}

// TestResolvePlaceholders covers the placeholder-substitution
// loop: known placeholders resolve via the dirs map; unknown
// placeholders surface as "" (the caller drops the entry); empty
// inputs return "".
func TestResolvePlaceholders(t *testing.T) {
	dirs := map[string]string{
		"libdir":     "lib",
		"bindir":     "bin",
		"includedir": "include",
	}
	cases := []struct {
		in   string
		want string
	}{
		{"", ""},
		{"{libdir}/libfoo.a", "lib/libfoo.a"},
		{"{libdir}/sub/libfoo.a", "lib/sub/libfoo.a"},
		{"{bindir}/foo", "bin/foo"},
		{"{includedir}/foo.h", "include/foo.h"},
		// Unknown placeholder → ""
		{"{weirdthing}/x", ""},
		// Mixed: one known, one unknown → ""
		{"{libdir}/{weirdthing}.a", ""},
		// No placeholders at all: pass-through.
		{"lib/libfoo.a", "lib/libfoo.a"},
	}
	// dirs has libdir; classifier expects libdir_static / libdir_shared
	// derivation. The dirValuesFromOptions helper adds those aliases;
	// for this lower-level test we exercise resolve directly.
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			got := resolvePlaceholders(c.in, dirs)
			if got != c.want {
				t.Errorf("resolvePlaceholders(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// TestDirValuesFromOptions_AliasesLibdir asserts the
// libdir_static / libdir_shared aliasing inside
// dirValuesFromOptions — these placeholder names appear in
// intro-install_plan.json but are NOT first-class entries in
// intro-buildoptions's directory section (meson exposes only
// `libdir`). The helper backfills the aliases so resolve hits.
func TestDirValuesFromOptions_AliasesLibdir(t *testing.T) {
	opts := []BuildOption{
		{Name: "libdir", Section: "directory", Value: "lib"},
		{Name: "prefix", Section: "directory", Value: "/"},
		// Non-directory section should be ignored.
		{Name: "c_std", Section: "compiler", Value: "c11"},
	}
	got := dirValuesFromOptions(opts)
	for _, k := range []string{"libdir", "libdir_static", "libdir_shared", "prefix"} {
		if _, ok := got[k]; !ok {
			t.Errorf("dirValuesFromOptions missing key %q in result %+v", k, got)
		}
	}
	if got["libdir_static"] != "lib" || got["libdir_shared"] != "lib" {
		t.Errorf("libdir aliases not equal to libdir value: %+v", got)
	}
	if _, ok := got["c_std"]; ok {
		t.Errorf("dirValuesFromOptions leaked non-directory option c_std: %+v", got)
	}
}
