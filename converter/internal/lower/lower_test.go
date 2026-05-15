package lower_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/internal/fileapi"
	"github.com/sstriker/buildstream-bazel/converter/internal/lower"
	"github.com/sstriker/buildstream-bazel/converter/ir"
)

const helloWorldFixture = "../../testdata/fileapi/hello-world"

func TestToIR_HelloWorld(t *testing.T) {
	r, err := fileapi.Load(helloWorldFixture)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	// The codemodel records an absolute source-root path that may not exist
	// at test time (the fixture was recorded on a different machine). Override
	// to the on-disk hello-world sample so header discovery works.
	src, err := filepath.Abs("../../testdata/sample-projects/hello-world")
	if err != nil {
		t.Fatalf("abs: %v", err)
	}

	pkg, err := lower.ToIR(r, nil, lower.Options{HostSourceRoot: src})
	if err != nil {
		t.Fatalf("ToIR: %v", err)
	}

	if pkg.Name != "hello" {
		t.Errorf("Package.Name = %q, want hello", pkg.Name)
	}
	if got := len(pkg.Targets); got != 1 {
		t.Fatalf("Targets = %d, want 1", got)
	}

	tgt := pkg.Targets[0]
	if tgt.Name != "hello" {
		t.Errorf("Target.Name = %q, want hello", tgt.Name)
	}
	if tgt.Kind != ir.KindCCLibrary {
		t.Errorf("Target.Kind = %v, want KindCCLibrary", tgt.Kind)
	}
	if !tgt.Linkstatic {
		t.Errorf("Linkstatic = false; STATIC_LIBRARY should set linkstatic=True")
	}
	if want := []string{"hello.c"}; !equal(tgt.Srcs, want) {
		t.Errorf("Srcs = %v, want %v", tgt.Srcs, want)
	}
	if want := []string{"include/hello.h"}; !equal(tgt.Hdrs, want) {
		t.Errorf("Hdrs = %v, want %v", tgt.Hdrs, want)
	}
	if want := []string{"include"}; !equal(tgt.Includes, want) {
		t.Errorf("Includes = %v, want %v", tgt.Includes, want)
	}
	// Release flags from CMAKE_C_FLAGS_RELEASE are "-O3 -DNDEBUG"; we split
	// them into copts=["-O3"] and defines=["NDEBUG"].
	if !contains(tgt.Copts, "-O3") {
		t.Errorf("Copts = %v, want to contain -O3", tgt.Copts)
	}
	if !contains(tgt.Defines, "NDEBUG") {
		t.Errorf("Defines = %v, want to contain NDEBUG", tgt.Defines)
	}
	for _, c := range tgt.Copts {
		if c == "-DNDEBUG" {
			t.Errorf("Copts contains -DNDEBUG; should be lifted to Defines")
		}
	}
	if tgt.InstallDest != "lib" {
		t.Errorf("InstallDest = %q, want lib", tgt.InstallDest)
	}
	if want := []string{"//visibility:public"}; !equal(tgt.Visibility, want) {
		t.Errorf("Visibility = %v, want %v", tgt.Visibility, want)
	}
}

// TestToIR_ElidesAbsoluteBuildDirSource covers the
// header-only-shim pattern where a project writes a placeholder
// source under ${CMAKE_BINARY_DIR} (e.g. via `file(WRITE
// ${CMAKE_BINARY_DIR}/dummy.cpp "")`) and adds it to an
// otherwise-header-only library. cmake's codemodel records the
// absolute build-dir path verbatim but doesn't flag it as
// IsGenerated (file(WRITE) outputs aren't marked generated
// unless the project explicitly sets the property). Without
// filtering, the absolute /tmp/<convert-element-build>/...
// path lands in irt.Srcs and the rendered BUILD.bazel refers
// to a file that's gone before Bazel ever runs the rule.
//
// Expected behaviour: the build-dir-rooted source is dropped
// from srcs and the target picks up the audit tag
// `cmake-elided-build-dir-source` so operators can query for
// affected targets.
func TestToIR_ElidesAbsoluteBuildDirSource(t *testing.T) {
	r := &fileapi.Reply{
		Codemodel: fileapi.Codemodel{
			Paths: fileapi.CodemodelPaths{
				Source: "/src",
				Build:  "/tmp/convert-element-build-abc123",
			},
			Configurations: []fileapi.Configuration{{
				Name:    "Release",
				Targets: []fileapi.ConfigTargetRef{{Name: "foo", Id: "foo::@1"}},
			}},
		},
		Targets: map[string]fileapi.Target{
			"foo::@1": {
				Name: "foo",
				Type: "STATIC_LIBRARY",
				Sources: []fileapi.TargetSource{
					{Path: "real.c", CompileGroupIndex: 0},
					{Path: "/tmp/convert-element-build-abc123/dummy.cpp", CompileGroupIndex: 0},
				},
				CompileGroups: []fileapi.CompileGroup{{
					Language:      "C",
					SourceIndexes: []int{0, 1},
				}},
			},
		},
	}
	pkg, err := lower.ToIR(r, nil, lower.Options{HostSourceRoot: "/src"})
	if err != nil {
		t.Fatalf("ToIR: %v", err)
	}
	if len(pkg.Targets) != 1 {
		t.Fatalf("Targets = %d, want 1", len(pkg.Targets))
	}
	tgt := pkg.Targets[0]
	for _, s := range tgt.Srcs {
		if filepath.IsAbs(s) {
			t.Errorf("Srcs contains absolute path %q; expected build-dir source to be dropped", s)
		}
		if strings.HasPrefix(s, "/tmp/") || strings.Contains(s, "convert-element-build-") {
			t.Errorf("Srcs leaked the build-dir tmp path: %q", s)
		}
	}
	if !contains(tgt.Srcs, "real.c") {
		t.Errorf("Srcs = %v, want to contain real.c (the non-build-dir source)", tgt.Srcs)
	}
	if !contains(tgt.Tags, "cmake-elided-build-dir-source") {
		t.Errorf("Tags = %v, want to contain cmake-elided-build-dir-source", tgt.Tags)
	}
}

// TestToIR_NoElidedTagWhenAllSourcesClean is a no-regression
// guard: the elision tag only fires when at least one
// build-dir-rooted source was actually dropped. Clean targets
// keep their existing tag set.
func TestToIR_NoElidedTagWhenAllSourcesClean(t *testing.T) {
	r := &fileapi.Reply{
		Codemodel: fileapi.Codemodel{
			Paths: fileapi.CodemodelPaths{
				Source: "/src",
				Build:  "/tmp/convert-element-build-abc123",
			},
			Configurations: []fileapi.Configuration{{
				Name:    "Release",
				Targets: []fileapi.ConfigTargetRef{{Name: "foo", Id: "foo::@1"}},
			}},
		},
		Targets: map[string]fileapi.Target{
			"foo::@1": {
				Name: "foo",
				Type: "STATIC_LIBRARY",
				Sources: []fileapi.TargetSource{
					{Path: "real.c", CompileGroupIndex: 0},
				},
				CompileGroups: []fileapi.CompileGroup{{
					Language:      "C",
					SourceIndexes: []int{0},
				}},
			},
		},
	}
	pkg, err := lower.ToIR(r, nil, lower.Options{HostSourceRoot: "/src"})
	if err != nil {
		t.Fatalf("ToIR: %v", err)
	}
	tgt := pkg.Targets[0]
	if contains(tgt.Tags, "cmake-elided-build-dir-source") {
		t.Errorf("Tags = %v, did not expect cmake-elided-build-dir-source", tgt.Tags)
	}
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func contains(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}
