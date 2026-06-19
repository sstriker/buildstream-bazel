package shadow

import (
	"reflect"
	"testing"
)

func TestExtractTargetSourcesCalls(t *testing.T) {
	// include(recipe) sets the active recipe; the target_sources that follow are
	// attributed to it by trace order (Recipe). A `;`-delimited source list is
	// split. A prefix-module call is KEPT (location is irrelevant — the tie gates
	// on OutToGenrule). Only CMakeFiles try_compile scratch is dropped.
	trace := `{"args":["/proj/build/recipe.cmake"],"cmd":"include","file":"/proj/src/CMakeLists.txt","line":2}
{"args":["app","PRIVATE","/proj/build/a.cpp;/proj/build/b.cpp"],"cmd":"target_sources","file":"/proj/build/recipe.cmake","line":1}
{"args":["scratch","PRIVATE","x.c"],"cmd":"target_sources","file":"/proj/build/CMakeFiles/CMakeScratch/try.cmake","line":1}
{"args":["/usr/share/cmake-4.3/Modules/Foo.cmake"],"cmd":"include","file":"/proj/src/CMakeLists.txt","line":3}
{"args":["m","PRIVATE","y.c"],"cmd":"target_sources","file":"/usr/share/cmake-4.3/Modules/Foo.cmake","line":1}
`
	got := ExtractTargetSourcesCalls([]byte(trace))
	if len(got) != 2 {
		t.Fatalf("want 2 (scratch dropped, prefix kept): got %d %+v", len(got), got)
	}
	// Causal recipe + semicolon split.
	if got[0].Target != "app" || got[0].Recipe != "/proj/build/recipe.cmake" ||
		!reflect.DeepEqual(got[0].Sources, []string{"/proj/build/a.cpp", "/proj/build/b.cpp"}) {
		t.Errorf("recipe call wrong (causal Recipe / `;`-split): %+v", got[0])
	}
	// Prefix-module call kept, attributed to the prefix include by trace order.
	if got[1].Target != "m" || got[1].Recipe != "/usr/share/cmake-4.3/Modules/Foo.cmake" {
		t.Errorf("prefix-module target_sources should be kept (location-agnostic): %+v", got[1])
	}
}
