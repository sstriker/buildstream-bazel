package shadow

import (
	"strings"
	"testing"
)

// TestExtractParentScopeForwards models the git_describe() / SDL shape from a
// real non-expanded trace: a helper defined function(get_git_sha _var) whose
// body runs execute_process(... OUTPUT_VARIABLE out ...) and hands the value
// back via set(${_var} "${out}" PARENT_SCOPE), called as get_git_sha(GIT_SHA).
// Body command events fire at the DEFINITION's file:line; the call event fires
// at the call site. The forward resolves ${_var} -> the call argument GIT_SHA.
func TestExtractParentScopeForwards(t *testing.T) {
	trace := strings.Join([]string{
		`{"cmd":"function","args":["get_git_sha","_var"],"file":"/src/CMakeLists.txt","line":12}`,
		`{"cmd":"endfunction","args":[],"file":"/src/CMakeLists.txt","line":19}`,
		// the call site (frame 1):
		`{"cmd":"get_git_sha","args":["GIT_SHA"],"file":"/src/CMakeLists.txt","line":21}`,
		// body events (frame 2, attributed to the def by line range 12..19):
		`{"cmd":"execute_process","args":["COMMAND","git","rev-parse","HEAD","OUTPUT_VARIABLE","out"],"file":"/src/CMakeLists.txt","line":13}`,
		`{"cmd":"set","args":["${_var}","${out}","PARENT_SCOPE"],"file":"/src/CMakeLists.txt","line":18}`,
	}, "\n")

	got := ExtractParentScopeForwards([]byte(trace), "/src")
	want := []ParentScopeForward{
		{Dst: "GIT_SHA", SrcVar: "out", File: "/src/CMakeLists.txt", Line: 21},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d forwards, want %d: %+v", len(got), len(want), got)
	}
	if got[0] != want[0] {
		t.Errorf("forward = %+v, want %+v", got[0], want[0])
	}
}

// TestExtractParentScopeForwards_MultipleCallSites: every call site of the
// helper binds the parameter to its own argument, so each gets its own
// resolved forward (all sharing the function-local SrcVar).
func TestExtractParentScopeForwards_MultipleCallSites(t *testing.T) {
	trace := strings.Join([]string{
		`{"cmd":"function","args":["stamp","outvar"],"file":"/src/CMakeLists.txt","line":5}`,
		`{"cmd":"set","args":["${outvar}","${rev}","PARENT_SCOPE"],"file":"/src/CMakeLists.txt","line":7}`,
		`{"cmd":"endfunction","args":[],"file":"/src/CMakeLists.txt","line":8}`,
		`{"cmd":"stamp","args":["GIT_SHA"],"file":"/src/CMakeLists.txt","line":10}`,
		`{"cmd":"stamp","args":["BUILD_REV"],"file":"/src/CMakeLists.txt","line":11}`,
	}, "\n")

	got := ExtractParentScopeForwards([]byte(trace), "/src")
	wantDsts := map[string]bool{"GIT_SHA": true, "BUILD_REV": true}
	if len(got) != 2 {
		t.Fatalf("got %d forwards, want 2: %+v", len(got), got)
	}
	for _, f := range got {
		if !wantDsts[f.Dst] {
			t.Errorf("unexpected Dst %q (want GIT_SHA or BUILD_REV)", f.Dst)
		}
		if f.SrcVar != "rev" {
			t.Errorf("forward SrcVar = %q, want rev", f.SrcVar)
		}
	}
}

// TestExtractParentScopeForwards_Negatives covers the shapes that must NOT
// resolve to a forward.
func TestExtractParentScopeForwards_Negatives(t *testing.T) {
	cases := []struct {
		name  string
		trace []string
	}{
		{
			name: "no PARENT_SCOPE — the write never escapes to the caller",
			trace: []string{
				`{"cmd":"function","args":["f","_v"],"file":"/src/CMakeLists.txt","line":1}`,
				`{"cmd":"set","args":["${_v}","${out}"],"file":"/src/CMakeLists.txt","line":2}`,
				`{"cmd":"endfunction","args":[],"file":"/src/CMakeLists.txt","line":3}`,
				`{"cmd":"f","args":["X"],"file":"/src/CMakeLists.txt","line":5}`,
			},
		},
		{
			name: "dst is a literal, not a param ref — handled by ExtractSetAssignments",
			trace: []string{
				`{"cmd":"function","args":["f","_v"],"file":"/src/CMakeLists.txt","line":1}`,
				`{"cmd":"set","args":["LITERAL","${out}","PARENT_SCOPE"],"file":"/src/CMakeLists.txt","line":2}`,
				`{"cmd":"endfunction","args":[],"file":"/src/CMakeLists.txt","line":3}`,
				`{"cmd":"f","args":["X"],"file":"/src/CMakeLists.txt","line":5}`,
			},
		},
		{
			name: "ref is not a declared parameter (e.g. a global)",
			trace: []string{
				`{"cmd":"function","args":["f","_v"],"file":"/src/CMakeLists.txt","line":1}`,
				`{"cmd":"set","args":["${OTHER}","${out}","PARENT_SCOPE"],"file":"/src/CMakeLists.txt","line":2}`,
				`{"cmd":"endfunction","args":[],"file":"/src/CMakeLists.txt","line":3}`,
				`{"cmd":"f","args":["X"],"file":"/src/CMakeLists.txt","line":5}`,
			},
		},
		{
			name: "set outside any function body (no enclosing def)",
			trace: []string{
				`{"cmd":"function","args":["f","_v"],"file":"/src/CMakeLists.txt","line":1}`,
				`{"cmd":"endfunction","args":[],"file":"/src/CMakeLists.txt","line":3}`,
				`{"cmd":"set","args":["${_v}","${out}","PARENT_SCOPE"],"file":"/src/CMakeLists.txt","line":10}`,
				`{"cmd":"f","args":["X"],"file":"/src/CMakeLists.txt","line":5}`,
			},
		},
		{
			name: "call argument is itself a ${...} ref — needs another binding level",
			trace: []string{
				`{"cmd":"function","args":["f","_v"],"file":"/src/CMakeLists.txt","line":1}`,
				`{"cmd":"set","args":["${_v}","${out}","PARENT_SCOPE"],"file":"/src/CMakeLists.txt","line":2}`,
				`{"cmd":"endfunction","args":[],"file":"/src/CMakeLists.txt","line":3}`,
				`{"cmd":"f","args":["${INDIRECT}"],"file":"/src/CMakeLists.txt","line":5}`,
			},
		},
		{
			name: "out-of-tree function definition is dropped under sourceRoot",
			trace: []string{
				`{"cmd":"function","args":["f","_v"],"file":"/usr/share/cmake/Modules/Foo.cmake","line":1}`,
				`{"cmd":"set","args":["${_v}","${out}","PARENT_SCOPE"],"file":"/usr/share/cmake/Modules/Foo.cmake","line":2}`,
				`{"cmd":"endfunction","args":[],"file":"/usr/share/cmake/Modules/Foo.cmake","line":3}`,
				`{"cmd":"f","args":["X"],"file":"/src/CMakeLists.txt","line":5}`,
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ExtractParentScopeForwards([]byte(strings.Join(c.trace, "\n")), "/src")
			if len(got) != 0 {
				t.Errorf("expected no forwards, got %+v", got)
			}
		})
	}
}

// TestRecoverFunctionDefs_StackPairing checks that nested-in-trace
// out-of-tree defs keep the stack balanced so the in-tree def's body range is
// recovered correctly. (cmake's own modules define helpers between the user's
// function and endfunction only if includes are processed inside the body at
// definition time — they aren't — so in practice defs are sequential; this
// guards the stack against an unbalanced endfunction.)
func TestRecoverFunctionDefs_StackPairing(t *testing.T) {
	trace := strings.Join([]string{
		`{"cmd":"function","args":["modfn","a"],"file":"/usr/share/cmake/M.cmake","line":1}`,
		`{"cmd":"endfunction","args":[],"file":"/usr/share/cmake/M.cmake","line":2}`,
		`{"cmd":"function","args":["userfn","_var"],"file":"/src/CMakeLists.txt","line":12}`,
		`{"cmd":"endfunction","args":[],"file":"/src/CMakeLists.txt","line":19}`,
	}, "\n")
	defs := recoverFunctionDefs(ParseTrace([]byte(trace)), "/src")
	if len(defs) != 1 {
		t.Fatalf("got %d in-tree defs, want 1: %+v", len(defs), defs)
	}
	d := defs[0]
	if d.name != "userfn" || d.startLine != 12 || d.endLine != 19 || len(d.params) != 1 || d.params[0] != "_var" {
		t.Errorf("recovered def = %+v, want userfn _var [12,19]", d)
	}
}
