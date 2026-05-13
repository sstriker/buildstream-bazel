package shadow_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/sstriker/buildstream-bazel/internal/shadow"
)

func TestDefaultAllowlist(t *testing.T) {
	m := shadow.DefaultAllowlist()
	cases := []struct {
		path string
		want bool
	}{
		{"CMakeLists.txt", true},
		{"src/CMakeLists.txt", true},
		{"include/foo.h", false},
		{"src/foo.c", false},
		{"src/foo.cpp", false},
		{"cmake/FindFoo.cmake", true},             // .cmake extension
		{"cmake/Helper.cmake", true},              // also under cmake/
		{"src/cmake/Helper.cmake", true},          // cmake/ deep
		{"src/CMake/Helper.cmake", true},          // capitalized variant
		{"src/cmake_modules/Helper.txt", true},    // dir match wins over ext
		{"src/cmake_modules/notreally.bin", true}, // dir match
		{"include/config.h.in", true},             // .in extension
		{"include/config.cmake.in", true},         // .cmake.in
		{"VERSION", true},
		{"COPYING", true},
		{"docs/README.md", false},
		{"resources/data.bin", false},
	}
	for _, c := range cases {
		if got := m.Allowed(c.path); got != c.want {
			t.Errorf("Allowed(%q) = %v, want %v", c.path, got, c.want)
		}
	}
}

func TestBuild_HelloWorld(t *testing.T) {
	src, err := filepath.Abs("../../converter/testdata/sample-projects/hello-world")
	if err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(t.TempDir(), "shadow")
	if err := shadow.Build(src, dst, shadow.DefaultAllowlist()); err != nil {
		t.Fatalf("Build: %v", err)
	}

	// CMakeLists.txt is allowlisted -> real content.
	got, err := os.ReadFile(filepath.Join(dst, "CMakeLists.txt"))
	if err != nil {
		t.Fatalf("read shadow CMakeLists: %v", err)
	}
	want, err := os.ReadFile(filepath.Join(src, "CMakeLists.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Errorf("shadow CMakeLists content mismatch")
	}

	// hello.c is NOT allowlisted -> zero-byte stub.
	if info, err := os.Stat(filepath.Join(dst, "hello.c")); err != nil {
		t.Fatalf("stub hello.c missing: %v", err)
	} else if info.Size() != 0 {
		t.Errorf("hello.c size = %d, want 0 (stubbed)", info.Size())
	}

	// include/hello.h is NOT allowlisted -> zero-byte stub, but path exists
	// (header discovery walks by extension, not by content).
	if info, err := os.Stat(filepath.Join(dst, "include", "hello.h")); err != nil {
		t.Fatalf("stub hello.h missing: %v", err)
	} else if info.Size() != 0 {
		t.Errorf("hello.h size = %d, want 0 (stubbed)", info.Size())
	}
}

func TestBuild_RefusesExistingDst(t *testing.T) {
	src, err := filepath.Abs("../../converter/testdata/sample-projects/hello-world")
	if err != nil {
		t.Fatal(err)
	}
	dst := t.TempDir() // already exists
	err = shadow.Build(src, dst, shadow.DefaultAllowlist())
	if err == nil {
		t.Errorf("Build to existing dir should error")
	}
}

func TestExtractReadPaths(t *testing.T) {
	trace := []byte(`{"file":"/src/CMakeLists.txt","line":4,"cmd":"include","args":["cmake/Helpers.cmake"]}
{"file":"/src/CMakeLists.txt","line":7,"cmd":"file","args":["READ","include/version.h","CONTENT"]}
not json
{"file":"/src/CMakeLists.txt","line":10,"cmd":"add_library","args":["foo","STATIC","foo.c"]}
{"file":"/src/cmake/Helpers.cmake","line":3,"cmd":"configure_file","args":["templates/config.h.in","${CMAKE_BINARY_DIR}/config.h"]}
{"file":"/src/CMakeLists.txt","line":15,"cmd":"include","args":["/usr/share/cmake-3.28/Modules/Foo.cmake"]}
`)
	got := shadow.ExtractReadPaths(trace, "/src")
	want := []string{
		"cmake/Helpers.cmake",
		"cmake/templates/config.h.in",
		"include/version.h",
	}
	if len(got) != len(want) {
		t.Fatalf("ExtractReadPaths = %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}
