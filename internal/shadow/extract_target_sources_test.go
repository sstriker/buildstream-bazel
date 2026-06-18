package shadow

import (
	"reflect"
	"testing"
)

func TestExtractTargetSourcesCalls(t *testing.T) {
	const src, build = "/proj/src", "/proj/build"
	// Kept: a source-tree call and a build-tree recipe call. Dropped: the FILE_SET
	// form (header-set shape), an add_library (not target_sources), a CMakeFiles
	// try_compile-scratch call, and a prefix-module call.
	trace := `{"args":["app","PRIVATE","/proj/src/a.c"],"cmd":"target_sources","file":"/proj/src/CMakeLists.txt","line":3}
{"args":["app","PRIVATE","/proj/build/gen.c"],"cmd":"target_sources","file":"/proj/build/recipe.cmake","line":1}
{"args":["app","PUBLIC","FILE_SET","HEADERS","BASE_DIRS","/proj/src","FILES","/proj/src/h.h"],"cmd":"target_sources","file":"/proj/src/CMakeLists.txt","line":7}
{"args":["other","STATIC","/proj/src/o.c"],"cmd":"add_library","file":"/proj/src/CMakeLists.txt","line":9}
{"args":["scratch","PRIVATE","/proj/build/CMakeFiles/x.c"],"cmd":"target_sources","file":"/proj/build/CMakeFiles/CMakeScratch/try.cmake","line":2}
{"args":["m","PRIVATE","/x.c"],"cmd":"target_sources","file":"/usr/share/cmake-4.3/Modules/Foo.cmake","line":4}
`
	got := ExtractTargetSourcesCalls([]byte(trace), src, build)
	if len(got) != 2 {
		t.Fatalf("got %d (FILE_SET / add_library / CMakeFiles-scratch / prefix all dropped): %+v", len(got), got)
	}
	if got[0].Target != "app" || !reflect.DeepEqual(got[0].Sources, []string{"/proj/src/a.c"}) || got[0].File != "/proj/src/CMakeLists.txt" {
		t.Errorf("call 0 (source-tree) wrong: %+v", got[0])
	}
	// The build-tree recipe's target_sources is captured (inProjectScope, build root set).
	if got[1].Target != "app" || !reflect.DeepEqual(got[1].Sources, []string{"/proj/build/gen.c"}) || got[1].File != "/proj/build/recipe.cmake" {
		t.Errorf("build-tree recipe target_sources not captured correctly: %+v", got[1])
	}
	// Without a build root it falls back to source-tree-only — the recipe call drops.
	if got0 := ExtractTargetSourcesCalls([]byte(trace), src, ""); len(got0) != 1 || got0[0].File != "/proj/src/CMakeLists.txt" {
		t.Errorf("source-tree-only fallback should keep only the source-tree call; got %+v", got0)
	}
}
