package shadow

import (
	"reflect"
	"testing"
)

func TestExtractTargetEventCommands(t *testing.T) {
	trace := `{"args":["TARGET","foo","PRE_LINK","COMMAND","/cmake","-E","echo","prelink","BYPRODUCTS","/src/build/foo_stamp.h","COMMENT","stamp"],"cmd":"add_custom_command","file":"/src/CMakeLists.txt","line":4}
{"args":["TARGET","foo","POST_BUILD","COMMAND","/cmake","-E","touch","/src/build/foo.built","BYPRODUCTS","/src/build/foo.built"],"cmd":"add_custom_command","file":"/src/CMakeLists.txt","line":8}
{"args":["OUTPUT","gen.c","COMMAND","tool"],"cmd":"add_custom_command","file":"/src/CMakeLists.txt","line":12}
{"args":["TARGET","bar","POST_BUILD","COMMAND","echo","hi"],"cmd":"add_custom_command","file":"/elsewhere/CMakeLists.txt","line":3}
`
	got := ExtractTargetEventCommands([]byte(trace), "/src")
	if len(got) != 2 {
		t.Fatalf("got %d TARGET-event commands, want 2 (OUTPUT-form + out-of-tree excluded): %+v", len(got), got)
	}
	if got[0].Target != "foo" || got[0].Event != "PRE_LINK" ||
		!reflect.DeepEqual(got[0].ByProducts, []string{"/src/build/foo_stamp.h"}) ||
		!reflect.DeepEqual(got[0].Commands, [][]string{{"/cmake", "-E", "echo", "prelink"}}) {
		t.Errorf("PRE_LINK call wrong: %+v", got[0])
	}
	if got[1].Target != "foo" || got[1].Event != "POST_BUILD" ||
		!reflect.DeepEqual(got[1].ByProducts, []string{"/src/build/foo.built"}) {
		t.Errorf("POST_BUILD call wrong: %+v", got[1])
	}
}
