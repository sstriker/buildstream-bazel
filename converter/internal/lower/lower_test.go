package lower_test

import (
	"path/filepath"
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
