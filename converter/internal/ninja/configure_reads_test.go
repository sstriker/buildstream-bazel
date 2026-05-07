package ninja_test

import (
	"io"
	"os"
	"reflect"
	"testing"

	"github.com/sstriker/cmake-to-bazel/converter/internal/ninja"
)

func TestProjectToSourceTree_FiltersStdlibAndBuildTree(t *testing.T) {
	inputs := []string{
		// In-source absolute → keep as src/main.c.
		"/work/src/main.c",
		// In-source absolute → keep as include/foo.h.
		"/work/include/foo.h",
		// cmake-stdlib (outside source root) → drop.
		"/usr/share/cmake-3.28/Modules/CMakeCInformation.cmake",
		// Build-tree relative → drop (resolves into /tmp/build).
		"CMakeCache.txt",
		// Build-tree relative → drop.
		"CMakeFiles/3.28.3/CMakeCCompiler.cmake",
		// Outside source root via .. → drop.
		"/other/place/file",
	}
	got := ninja.ProjectToSourceTree(inputs, "/work", "/tmp/build")
	want := []string{
		"include/foo.h",
		"src/main.c",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ProjectToSourceTree = %v, want %v", got, want)
	}
}

// TestProjectToSourceTree_BuildDirInsideSourceRoot covers the
// in-tree build-dir layout (sourceRoot=/work, buildDir=/work/build).
// Without an explicit buildDir-exclude pass, relative inputs like
// CMakeCache.txt would resolve to /work/build/CMakeCache.txt, which
// IS inside /work, and leak into the projected oracle as
// "build/CMakeCache.txt" — a configure output, not a source.
func TestProjectToSourceTree_BuildDirInsideSourceRoot(t *testing.T) {
	inputs := []string{
		// In-source absolute → keep.
		"/work/src/main.c",
		// Build-tree relative → drop (resolves to /work/build/CMakeCache.txt).
		"CMakeCache.txt",
		// Build-tree relative → drop.
		"CMakeFiles/3.28.3/CMakeCCompiler.cmake",
		// Build-tree absolute (the path that triggered the
		// original leak): also drop.
		"/work/build/CMakeFiles/cmake.check_cache",
	}
	got := ninja.ProjectToSourceTree(inputs, "/work", "/work/build")
	want := []string{"src/main.c"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("in-tree-buildDir leak\nwant: %v\ngot:  %v", want, got)
	}
}

func TestProjectToSourceTree_DedupesAndSorts(t *testing.T) {
	inputs := []string{
		"/work/b.txt",
		"/work/a.txt",
		"/work/b.txt", // dup
		"/work/sub/c.txt",
	}
	got := ninja.ProjectToSourceTree(inputs, "/work", "/tmp/build")
	want := []string{"a.txt", "b.txt", "sub/c.txt"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ProjectToSourceTree = %v, want %v", got, want)
	}
}

func TestProjectToSourceTree_EmptyOrIncompleteContext(t *testing.T) {
	cases := map[string]struct {
		inputs     []string
		sourceRoot string
		buildDir   string
	}{
		"nil inputs":      {nil, "/work", "/tmp/build"},
		"empty inputs":    {[]string{}, "/work", "/tmp/build"},
		"no source root":  {[]string{"/work/a"}, "", "/tmp/build"},
		"no build dir":    {[]string{"/work/a"}, "/work", ""},
		"all out-of-tree": {[]string{"/usr/share/cmake-3.28/Modules/X.cmake"}, "/work", "/tmp/build"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := ninja.ProjectToSourceTree(tc.inputs, tc.sourceRoot, tc.buildDir); got != nil {
				t.Errorf("ProjectToSourceTree = %v, want nil", got)
			}
		})
	}
}

func TestProjectToSourceTree_HelloWorldFixture(t *testing.T) {
	// End-to-end: parse the recorded build.ninja, project against the
	// recorded source-root-equivalent path. The fixture's RERUN_CMAKE
	// references the user CMakeLists.txt under
	//   /home/user/cmake-to-bazel/converter/testdata/sample-projects/hello-world/
	// plus ~70 cmake-stdlib modules and a couple of build-tree files.
	// After projection, only CMakeLists.txt should remain.
	f, err := os.Open("../../testdata/fileapi/hello-world/build.ninja")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	g, err := ninja.Parse(f, "", &ninja.Parser{
		FileResolver: func(parentDir, path string) (io.ReadCloser, error) {
			return nil, nil // skip rules.ninja include
		},
	})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	const sourceRoot = "/home/user/cmake-to-bazel/converter/testdata/sample-projects/hello-world"
	const buildDir = "/tmp/tmp.gjGh7fY0W5"
	got := ninja.ProjectToSourceTree(g.ReconfigureInputs(), sourceRoot, buildDir)
	want := []string{"CMakeLists.txt"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ProjectToSourceTree on hello-world = %v, want %v", got, want)
	}
}
