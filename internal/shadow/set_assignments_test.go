package shadow

import (
	"strings"
	"testing"
)

func TestExtractSetAssignments(t *testing.T) {
	trace := strings.Join([]string{
		// verbatim copies (kept):
		`{"cmd":"set","args":["VERSION","${GIT_SHA}"],"file":"/src/CMakeLists.txt","line":10}`,
		`{"cmd":"set","args":["TAG","${VERSION}","PARENT_SCOPE"],"file":"/src/CMakeLists.txt","line":11}`,
		`{"cmd":"set","args":["CACHED","${GIT_SHA}","CACHE","STRING","doc"],"file":"/src/CMakeLists.txt","line":12}`,
		// not copies (skipped):
		`{"cmd":"set","args":["LITERAL","abc123"],"file":"/src/CMakeLists.txt","line":13}`,
		`{"cmd":"set","args":["CONCAT","v${GIT_SHA}"],"file":"/src/CMakeLists.txt","line":14}`,
		`{"cmd":"set","args":["LIST","${GIT_SHA}","extra"],"file":"/src/CMakeLists.txt","line":15}`,
		`{"cmd":"set","args":["TWO","${A}${B}"],"file":"/src/CMakeLists.txt","line":16}`,
		// FORCE / INTERNAL without a preceding CACHE are plain list
		// elements, not copy tails — set(X ${Y} FORCE) sets X to the list
		// "${Y};FORCE", so these must NOT match as verbatim copies.
		`{"cmd":"set","args":["FORCED","${GIT_SHA}","FORCE"],"file":"/src/CMakeLists.txt","line":18}`,
		`{"cmd":"set","args":["INT","${GIT_SHA}","INTERNAL"],"file":"/src/CMakeLists.txt","line":19}`,
		// out of the project tree (skipped when sourceRoot is set):
		`{"cmd":"set","args":["INTERNAL_X","${GIT_SHA}"],"file":"/usr/share/cmake/Modules/Foo.cmake","line":1}`,
		// not a set():
		`{"cmd":"message","args":["${GIT_SHA}"],"file":"/src/CMakeLists.txt","line":17}`,
	}, "\n")

	got := ExtractSetAssignments([]byte(trace), "/src")
	want := []SetAssignment{
		{Dst: "VERSION", SrcVar: "GIT_SHA", File: "/src/CMakeLists.txt", Line: 10},
		{Dst: "TAG", SrcVar: "VERSION", File: "/src/CMakeLists.txt", Line: 11},
		{Dst: "CACHED", SrcVar: "GIT_SHA", File: "/src/CMakeLists.txt", Line: 12},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d assignments, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("assignment[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// TestExtractSetAssignments_TrailingSlashSourceRoot guards the CLI
// --source-root-with-trailing-slash case: inSourceTree would otherwise
// treat `/src/` as outside-tree and drop every in-tree copy.
func TestExtractSetAssignments_TrailingSlashSourceRoot(t *testing.T) {
	trace := `{"cmd":"set","args":["VERSION","${GIT_SHA}"],"file":"/src/CMakeLists.txt","line":10}`
	got := ExtractSetAssignments([]byte(trace), "/src/")
	if len(got) != 1 || got[0].Dst != "VERSION" || got[0].SrcVar != "GIT_SHA" {
		t.Errorf("trailing-slash sourceRoot dropped the in-tree copy: got %+v", got)
	}
}

func TestSoleVarRef(t *testing.T) {
	cases := []struct {
		in   string
		name string
		ok   bool
	}{
		{"${GIT_SHA}", "GIT_SHA", true},
		{"${X}", "X", true},
		{"v${X}", "", false},    // concatenation
		{"${X}${Y}", "", false}, // two refs
		{"${X}-s", "", false},   // trailing text
		{"${${Y}}", "", false},  // nested
		{"plain", "", false},    // no ref
		{"${}", "", false},      // empty name
		{"", "", false},         // empty
	}
	for _, c := range cases {
		name, ok := soleVarRef(c.in)
		if name != c.name || ok != c.ok {
			t.Errorf("soleVarRef(%q) = (%q,%v), want (%q,%v)", c.in, name, ok, c.name, c.ok)
		}
	}
}
