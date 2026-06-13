package shadow

import (
	"strings"
	"testing"
)

// TestExtractVariableReads pins the read shapes the dead-capture
// analysis depends on: ${} dereferences anywhere, bare identifiers in
// auto-dereferencing commands, and — critically — that the capture
// KEYWORD positions of an execute_process call do NOT count as reads
// (else every capture would be self-referencing and never dead).
func TestExtractVariableReads(t *testing.T) {
	trace := strings.Join([]string{
		// The capture site itself: bare var names in a non-deref
		// command must NOT count as reads.
		`{"cmd":"execute_process","args":["COMMAND","tool","OUTPUT_VARIABLE","_quiet","ERROR_VARIABLE","_quiet_err","RESULT_VARIABLE","_rc"],"file":"/src/CMakeLists.txt","line":3}`,
		// ${} dereference in an arbitrary command.
		`{"cmd":"message","args":["STATUS","ver: ${_ver}"],"file":"/src/CMakeLists.txt","line":5}`,
		// Bare identifier auto-deref in if().
		`{"cmd":"if","args":["NOT","_rc_checked","EQUAL","0"],"file":"/src/CMakeLists.txt","line":7}`,
		// foreach IN LISTS bare deref.
		`{"cmd":"foreach","args":["x","IN","LISTS","items"],"file":"/src/CMakeLists.txt","line":9}`,
		// list() reads its list var bare.
		`{"cmd":"list","args":["LENGTH","mylist","n"],"file":"/src/CMakeLists.txt","line":11}`,
		// $ENV{} is a different namespace; must not count for HOME.
		`{"cmd":"set","args":["P","$ENV{HOME}/x"],"file":"/src/CMakeLists.txt","line":13}`,
		// Generator expressions carry no ${}; must not panic or match.
		`{"cmd":"target_compile_options","args":["t","PRIVATE","$<$<CONFIG:Release>:-O3>"],"file":"/src/CMakeLists.txt","line":15}`,
	}, "\n")

	reads := ExtractVariableReads([]byte(trace))

	for _, want := range []string{"_ver", "_rc_checked", "items", "mylist", "x", "n", "NOT", "EQUAL", "LENGTH"} {
		if !reads[want] {
			t.Errorf("expected %q in the read set (conservative over-count includes keywords)", want)
		}
	}
	for _, dead := range []string{"_quiet", "_quiet_err", "_rc", "HOME"} {
		if reads[dead] {
			t.Errorf("%q must NOT be a read (capture keyword position / ENV namespace)", dead)
		}
	}
}
