package shadow

import (
	"reflect"
	"testing"
)

func TestExtractTargetSourcesCalls(t *testing.T) {
	// Two real target_sources events (one from a build-tree recipe .cmake — NOT
	// source-tree-gated), one FILE_SET form (skipped), one add_library (ignored).
	trace := `{"args":["app","PRIVATE","/src/a.c"],"cmd":"target_sources","file":"/src/CMakeLists.txt","line":3}
{"args":["app","PRIVATE","/build/gen.c"],"cmd":"target_sources","file":"/build/recipe.cmake","line":1}
{"args":["app","PUBLIC","FILE_SET","HEADERS","BASE_DIRS","/src","FILES","/src/h.h"],"cmd":"target_sources","file":"/src/CMakeLists.txt","line":7}
{"args":["other","STATIC","/src/o.c"],"cmd":"add_library","file":"/src/CMakeLists.txt","line":9}
`
	got := ExtractTargetSourcesCalls([]byte(trace))
	if len(got) != 2 {
		t.Fatalf("got %d target_sources calls, want 2 (FILE_SET skipped, add_library ignored): %+v", len(got), got)
	}
	if got[0].Target != "app" || !reflect.DeepEqual(got[0].Sources, []string{"/src/a.c"}) || got[0].File != "/src/CMakeLists.txt" {
		t.Errorf("call 0 wrong: %+v", got[0])
	}
	// The build-tree recipe's target_sources is captured (location-independent).
	if got[1].Target != "app" || !reflect.DeepEqual(got[1].Sources, []string{"/build/gen.c"}) || got[1].File != "/build/recipe.cmake" {
		t.Errorf("build-tree recipe target_sources not captured correctly: %+v", got[1])
	}
}
