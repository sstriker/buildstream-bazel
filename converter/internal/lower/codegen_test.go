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
	if !strings.Contains(gen.GenruleCmd, "/usr/bin/python3") {
		t.Errorf("genrule.GenruleCmd = %q, want to contain /usr/bin/python3", gen.GenruleCmd)
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
