package lower_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/internal/fileapi"
	"github.com/sstriker/buildstream-bazel/converter/internal/lower"
	"github.com/sstriker/buildstream-bazel/converter/internal/ninja"
	"github.com/sstriker/buildstream-bazel/converter/ir"
)

// TestToIR_CodegenTarget exercises the genrule recovery path against the
// codegen-target sample (Python script -> generated header). Validates:
//
//   - Generated source lookup against the ninja graph succeeds.
//   - The recovered ir.Target has Kind=KindGenrule, the right outs, and the
//     literal Python invocation in GenruleCmd.
//   - cmake-codegen + cmake-codegen-driver=python3 + cmake-codegen-source-only
//     tags all fire.
//   - The consuming cc_library carries has-cmake-codegen and lists the
//     generated header in hdrs.
func TestToIR_CodegenTarget(t *testing.T) {
	r := loadFixture(t, "codegen-target")
	g := loadNinja(t, "codegen-target")

	src, err := filepath.Abs("../../testdata/sample-projects/codegen-target")
	if err != nil {
		t.Fatal(err)
	}
	pkg, err := lower.ToIR(r, g, lower.Options{HostSourceRoot: src})
	if err != nil {
		t.Fatalf("ToIR: %v", err)
	}

	codegen := findTarget(t, pkg, "codegen")
	if codegen.Kind != ir.KindCCLibrary {
		t.Errorf("codegen.Kind = %v, want KindCCLibrary", codegen.Kind)
	}
	if !slicesContain(codegen.Tags, "has-cmake-codegen") {
		t.Errorf("codegen.Tags = %v, want has-cmake-codegen", codegen.Tags)
	}
	if !slicesContain(codegen.Hdrs, "version.h") {
		t.Errorf("codegen.Hdrs = %v, want to contain version.h", codegen.Hdrs)
	}

	// Find the recovered genrule.
	var gen *ir.Target
	for i := range pkg.Targets {
		if pkg.Targets[i].Kind == ir.KindGenrule {
			gen = &pkg.Targets[i]
			break
		}
	}
	if gen == nil {
		t.Fatal("no genrule recovered from codegen-target")
	}
	if !slicesContain(gen.GenruleOuts, "version.h") {
		t.Errorf("genrule.GenruleOuts = %v, want to contain version.h", gen.GenruleOuts)
	}
	// rewriteGenruleCmd strips the /usr/bin/ host-bin prefix so the
	// rendered cmd resolves through $PATH at action time. The bare
	// `python3` token is what survives.
	if !strings.Contains(gen.GenruleCmd, "python3") {
		t.Errorf("genrule.GenruleCmd = %q, want to contain python3", gen.GenruleCmd)
	}
	if !strings.Contains(gen.GenruleCmd, "gen_version.py") {
		t.Errorf("genrule.GenruleCmd = %q, want to contain gen_version.py", gen.GenruleCmd)
	}
	wantTags := []string{"cmake-codegen", "cmake-codegen-driver=python3", "cmake-codegen-source-only"}
	for _, w := range wantTags {
		if !slicesContain(gen.Tags, w) {
			t.Errorf("genrule.Tags = %v, want to contain %q", gen.Tags, w)
		}
	}
	// .py drivers should not produce cmake-codegen-cmake-e.
	if slicesContain(gen.Tags, "cmake-codegen-cmake-e") {
		t.Errorf("genrule.Tags = %v, did not expect cmake-codegen-cmake-e for python driver", gen.Tags)
	}
}

// TestToIR_CodegenTarget_RefusesScript rejects a synthetic build statement
// that drives ${CMAKE_COMMAND} -P with the architectural-refusal code.
func TestToIR_CodegenTarget_RefusesScript(t *testing.T) {
	const ninjaSrc = `rule CUSTOM_COMMAND
  command = $COMMAND

build /build/x.h: CUSTOM_COMMAND
  COMMAND = cd /build && /usr/bin/cmake -P /src/scripts/gen.cmake /build/x.h
`
	g, err := ninja.Parse(strings.NewReader(ninjaSrc), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	r := &fileapi.Reply{
		Codemodel: fileapi.Codemodel{
			Paths: fileapi.CodemodelPaths{Build: "/build", Source: "/src"},
			Configurations: []fileapi.Configuration{{
				Name:    "Release",
				Targets: []fileapi.ConfigTargetRef{{Name: "lib", Id: "lib::@1"}},
			}},
		},
		Targets: map[string]fileapi.Target{
			"lib::@1": {
				Name: "lib",
				Type: "STATIC_LIBRARY",
				Sources: []fileapi.TargetSource{{
					Path:        "/build/x.h",
					IsGenerated: true,
				}},
			},
		},
	}
	_, err = lower.ToIR(r, g, lower.Options{HostSourceRoot: "/src"})
	if err == nil {
		t.Fatal("expected unsupported-custom-command-script, got nil")
	}
	if !strings.Contains(err.Error(), "unsupported-custom-command-script") {
		t.Errorf("err = %v, want unsupported-custom-command-script", err)
	}
}

// TestToIR_CodegenTarget_RefusesScriptWithLeadingCacheVars is the
// follow-on to TestToIR_CodegenTarget_RefusesScript covering the
// shape libpng (and similarly any package that pre-resolves the
// output basename inside its cmake script) uses:
//
//	cd <build> && cmake -DOUTPUT=foo.h -P scripts/gen.cmake
//
// The pre-#142 detection looked for the literal `/usr/bin/cmake -P `
// or `${CMAKE_COMMAND} -P ` substring, which slips when any
// `-D<var>=<val>` cache argument precedes `-P`. The genrule then
// landed in BUILD.bazel with a `cmd` referencing absolute build-dir
// paths that don't survive past convert-element-cmake's tmp-dir
// cleanup, breaking the rendered output at Bazel build time.
func TestToIR_CodegenTarget_RefusesScriptWithLeadingCacheVars(t *testing.T) {
	const ninjaSrc = `rule CUSTOM_COMMAND
  command = $COMMAND

build /build/x.h: CUSTOM_COMMAND
  COMMAND = cd /build && /usr/bin/cmake -DOUTPUT=x.h -P /build/scripts/gen.cmake
`
	g, err := ninja.Parse(strings.NewReader(ninjaSrc), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	r := &fileapi.Reply{
		Codemodel: fileapi.Codemodel{
			Paths: fileapi.CodemodelPaths{Build: "/build", Source: "/src"},
			Configurations: []fileapi.Configuration{{
				Name:    "Release",
				Targets: []fileapi.ConfigTargetRef{{Name: "lib", Id: "lib::@1"}},
			}},
		},
		Targets: map[string]fileapi.Target{
			"lib::@1": {
				Name: "lib",
				Type: "STATIC_LIBRARY",
				Sources: []fileapi.TargetSource{{
					Path:        "/build/x.h",
					IsGenerated: true,
				}},
			},
		},
	}
	_, err = lower.ToIR(r, g, lower.Options{HostSourceRoot: "/src"})
	if err == nil {
		t.Fatal("expected unsupported-custom-command-script, got nil")
	}
	if !strings.Contains(err.Error(), "unsupported-custom-command-script") {
		t.Errorf("err = %v, want unsupported-custom-command-script", err)
	}
}

// TestToIR_CmakeScriptModeRefusal_RealFixture exercises the
// refusal end-to-end against a real recorded fileapi reply for
// a CMakeLists.txt that drives `${CMAKE_COMMAND} -DOUTPUT=... -P
// scripts/gen.cmake` from add_custom_command. The synthetic
// ninja test above pins the detection logic; this one pins the
// integration with the bytes cmake actually emits — the exact
// substring shape is sensitive to cmake's argv ordering and to
// whether the recording machine's `cmake` is at `/usr/bin/cmake`
// vs. somewhere else on PATH. Without an empirical fixture a
// future cmake re-ordering could quietly slip past the detector
// even with the substring/tokeniser unit tests passing.
func TestToIR_CmakeScriptModeRefusal_RealFixture(t *testing.T) {
	r := loadFixture(t, "cmake-script-mode-refusal")
	g := loadNinja(t, "cmake-script-mode-refusal")

	src, err := filepath.Abs("../../testdata/sample-projects/cmake-script-mode-refusal")
	if err != nil {
		t.Fatal(err)
	}
	_, err = lower.ToIR(r, g, lower.Options{HostSourceRoot: src})
	if err == nil {
		t.Fatal("expected unsupported-custom-command-script against the real fixture, got nil")
	}
	if !strings.Contains(err.Error(), "unsupported-custom-command-script") {
		t.Errorf("err = %v, want unsupported-custom-command-script", err)
	}
}

// TestToIR_StandaloneCustomCommand exercises the Phase 4
// standalone-genrule emission against the
// standalone-custom-command sample (add_custom_command whose
// output is consumed only by an add_custom_target — no
// cc_library sources reference it). Validates:
//
//   - With Options.EmitStandaloneCustomCommands = true, the
//     standalone walker fires and emits a genrule whose outs
//     contain the standalone_stamp.txt output.
//   - The emitted genrule carries the
//     cmake-codegen-standalone-custom-command tag so Phase 7
//     audit pipelines can inventory it.
//   - The companion cc_library (`stub`) is emitted unchanged
//     alongside the standalone genrule.
//   - With EmitStandaloneCustomCommands = false (the
//     lower.Options zero value), the standalone walker stays
//     quiet — no genrule for the standalone edge. Pins the
//     two-tier default contract: the library-side zero value
//     stays opt-in so in-process callers keep their existing
//     emission shape; the CLI graduated to default-on.
func TestToIR_StandaloneCustomCommand(t *testing.T) {
	r := loadFixture(t, "standalone-custom-command")
	g := loadNinja(t, "standalone-custom-command")

	src, err := filepath.Abs("../../testdata/sample-projects/standalone-custom-command")
	if err != nil {
		t.Fatal(err)
	}

	// Opt-in path (mirrors what the CLI's default-on shape
	// delivers): the walker fires and the standalone edge
	// surfaces as a genrule.
	pkg, err := lower.ToIR(r, g, lower.Options{
		HostSourceRoot:               src,
		EmitStandaloneCustomCommands: true,
	})
	if err != nil {
		t.Fatalf("ToIR: %v", err)
	}

	// The companion cc_library should be there regardless.
	stub := findTarget(t, pkg, "stub")
	if stub.Kind != ir.KindCCLibrary {
		t.Errorf("stub.Kind = %v, want KindCCLibrary", stub.Kind)
	}

	// Walk the targets to find the standalone genrule. Naming
	// uses the sanitized first-output stem, prefixed with
	// `custom_command_`.
	var standalone *ir.Target
	for i := range pkg.Targets {
		if pkg.Targets[i].Kind != ir.KindGenrule {
			continue
		}
		if !slicesContain(pkg.Targets[i].Tags, "cmake-codegen-standalone-custom-command") {
			continue
		}
		standalone = &pkg.Targets[i]
		break
	}
	if standalone == nil {
		t.Fatal("no standalone genrule emitted (cmake-codegen-standalone-custom-command tag missing)")
	}
	if !slicesContain(standalone.GenruleOuts, "standalone_stamp.txt") {
		t.Errorf("standalone.GenruleOuts = %v, want standalone_stamp.txt", standalone.GenruleOuts)
	}
	if !strings.Contains(standalone.Name, "standalone_stamp") {
		t.Errorf("standalone.Name = %q, want it to reference standalone_stamp", standalone.Name)
	}

	// Opt-out path: with EmitStandaloneCustomCommands left at
	// the Go zero value (false), no standalone genrule should
	// emit. Pins the library-side default: in-process callers
	// constructing lower.Options{...} literals keep their
	// existing emission shape.
	pkgOff, err := lower.ToIR(r, g, lower.Options{HostSourceRoot: src})
	if err != nil {
		t.Fatalf("ToIR (opt-out): %v", err)
	}
	for _, tgt := range pkgOff.Targets {
		for _, tag := range tgt.Tags {
			if tag == "cmake-codegen-standalone-custom-command" {
				t.Errorf("opt-out shape unexpectedly emitted standalone genrule: target %q", tgt.Name)
			}
		}
	}
}

// ----- helpers ----------------------------------------------------------

func loadFixture(t *testing.T, name string) *fileapi.Reply {
	t.Helper()
	r, err := fileapi.Load("../../testdata/fileapi/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func loadNinja(t *testing.T, name string) *ninja.Graph {
	t.Helper()
	dir := "../../testdata/fileapi/" + name
	g, err := ninja.ParseFile(dir + "/build.ninja")
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func findTarget(t *testing.T, pkg *ir.Package, name string) *ir.Target {
	t.Helper()
	for i := range pkg.Targets {
		if pkg.Targets[i].Name == name {
			return &pkg.Targets[i]
		}
	}
	t.Fatalf("no target named %q in package", name)
	return nil
}

func slicesContain(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}
