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
	got := ExtractTargetEventCommands([]byte(trace))
	// OUTPUT-form is still excluded (not a TARGET event); the out-of-tree TARGET
	// event IS now included — a stamp hook is output-bearing and may legitimately
	// be defined outside the source root (build-tree recipe / cmake module), so it
	// is no longer location-gated at extraction.
	if len(got) != 3 {
		t.Fatalf("got %d TARGET-event commands, want 3 (OUTPUT-form excluded, out-of-tree included): %+v", len(got), got)
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
	if got[2].Target != "bar" || got[2].Event != "POST_BUILD" || got[2].File != "/elsewhere/CMakeLists.txt" {
		t.Errorf("out-of-tree TARGET event should be included now: %+v", got[2])
	}
}
