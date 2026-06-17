package shadow

import "testing"

func TestExtractIncludeCalls(t *testing.T) {
	trace := `{"args":["/build/gen/recipe.cmake"],"cmd":"include","file":"/src/CMakeLists.txt","line":12}
{"args":["/usr/share/cmake/Modules/Foo.cmake"],"cmd":"include","file":"/src/CMakeLists.txt","line":3}
{"args":["x","PUBLIC","inc"],"cmd":"target_include_directories","file":"/src/CMakeLists.txt","line":4}
not-json banner line
{"args":[],"cmd":"include","file":"/src/CMakeLists.txt","line":99}
`
	got := ExtractIncludeCalls([]byte(trace))
	if len(got) != 2 {
		t.Fatalf("got %d include calls, want 2: %+v", len(got), got)
	}
	if got[0].Path != "/build/gen/recipe.cmake" || got[0].File != "/src/CMakeLists.txt" || got[0].Line != 12 {
		t.Errorf("first include call wrong: %+v", got[0])
	}
	if got[1].Path != "/usr/share/cmake/Modules/Foo.cmake" {
		t.Errorf("second include call wrong: %+v", got[1])
	}
}
