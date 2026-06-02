package shadow

import (
	"reflect"
	"testing"
)

// ExtractFileGlobs must recover user-written file(GLOB)/file(GLOB_RECURSE)
// calls from a --trace-expand stream (patterns already absolute +
// wildcarded), distinguish GLOB from GLOB_RECURSE, flag RELATIVE, and drop
// cmake-internal globs (outside the source tree). The trace shapes here
// mirror a real Ninja configure of a file(GLOB)→add_custom_command project.
func TestExtractFileGlobs(t *testing.T) {
	trace := `{"args":["GLOB","inputs","/src/data/*.txt"],"cmd":"file","file":"/src/CMakeLists.txt","line":3}
{"args":["GLOB_RECURSE","deep_inputs","/src/lib/*.in"],"cmd":"file","file":"/src/CMakeLists.txt","line":4}
{"args":["GLOB","rel_inputs","RELATIVE","/src","/src/etc/*.cfg"],"cmd":"file","file":"/src/CMakeLists.txt","line":5}
{"args":["GLOB","cm_internal","/usr/share/cmake-3.28/Modules/*.cmake"],"cmd":"file","file":"/usr/share/cmake-3.28/Modules/Probe.cmake","line":9}
{"args":["READ","/src/version.h","CONTENT"],"cmd":"file","file":"/src/CMakeLists.txt","line":7}`

	got := ExtractFileGlobs([]byte(trace), "/src")
	want := []FileGlobCall{
		{File: "/src/CMakeLists.txt", Line: 3, Var: "inputs", Patterns: []string{"/src/data/*.txt"}, Recurse: false, Relative: false, RawArgs: []string{"GLOB", "inputs", "/src/data/*.txt"}},
		{File: "/src/CMakeLists.txt", Line: 4, Var: "deep_inputs", Patterns: []string{"/src/lib/*.in"}, Recurse: true, Relative: false, RawArgs: []string{"GLOB_RECURSE", "deep_inputs", "/src/lib/*.in"}},
		{File: "/src/CMakeLists.txt", Line: 5, Var: "rel_inputs", Patterns: []string{"/src/etc/*.cfg"}, Recurse: false, Relative: true, RawArgs: []string{"GLOB", "rel_inputs", "RELATIVE", "/src", "/src/etc/*.cfg"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ExtractFileGlobs:\n  got:  %+v\n  want: %+v", got, want)
	}
}
