package lower

import (
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/internal/fileapi"
)

// macroBacktrace builds the backtrace of an add_library that ran inside a
// macro body: node 1 is the user's invocation, node 2 the body line.
func macroBacktrace() fileapi.BacktraceGraph {
	parent0, parent1 := 0, 1
	return fileapi.BacktraceGraph{
		Commands: []string{"add_widget", "add_library"},
		Files:    []string{"CMakeLists.txt", "cmake/helpers.cmake"},
		Nodes: []fileapi.BacktraceNode{
			{File: 0},
			{File: 0, Line: 10, Command: 0, Parent: &parent0},
			{File: 1, Line: 3, Command: 1, Parent: &parent1},
		},
	}
}

// TestTargetProvenance_MacroInvocationYieldsCallSite: a target declared
// inside a macro body keeps the body line as Provenance (the precise
// declaring command) and gets the user's invocation as CallSite.
func TestTargetProvenance_MacroInvocationYieldsCallSite(t *testing.T) {
	tgt := &fileapi.Target{Backtrace: 2, BacktraceGraph: macroBacktrace()}
	decl, call := targetProvenance(tgt, "", "")
	if decl.File != "cmake/helpers.cmake" || decl.Line != 3 || decl.Command != "add_library" {
		t.Errorf("decl = %+v; want cmake/helpers.cmake:3 add_library", decl)
	}
	if call.File != "CMakeLists.txt" || call.Line != 10 || call.Command != "add_widget" {
		t.Errorf("call site = %+v; want CMakeLists.txt:10 add_widget", call)
	}
}

// TestTargetProvenance_DirectDeclarationHasNoCallSite: a target declared
// straight in a CMakeLists (backtrace points at a user frame with no
// deeper user caller) gets a zero CallSite — Provenance IS the call.
func TestTargetProvenance_DirectDeclarationHasNoCallSite(t *testing.T) {
	parent0 := 0
	tgt := &fileapi.Target{
		Backtrace: 1,
		BacktraceGraph: fileapi.BacktraceGraph{
			Commands: []string{"add_library"},
			Files:    []string{"CMakeLists.txt"},
			Nodes: []fileapi.BacktraceNode{
				{File: 0},
				{File: 0, Line: 6, Command: 0, Parent: &parent0},
			},
		},
	}
	decl, call := targetProvenance(tgt, "", "")
	if decl.File != "CMakeLists.txt" || decl.Line != 6 {
		t.Errorf("decl = %+v; want CMakeLists.txt:6", decl)
	}
	if !call.IsZero() {
		t.Errorf("call site = %+v; want zero for a direct declaration", call)
	}
}

// TestTargetProvenance_IncludedFileDeclarationHasNoCallSite: a target
// declared DIRECTLY at the top level of an include()d project .cmake file
// is user-authored where it stands — the include() line is a scope change,
// not an invocation, so no CallSite. (Otherwise several targets declared in
// one included file would collapse onto the shared include() site and the
// ambiguity guard would drop comments each carries from its own line.)
func TestTargetProvenance_IncludedFileDeclarationHasNoCallSite(t *testing.T) {
	parent0, parent1 := 0, 1
	tgt := &fileapi.Target{
		Backtrace: 2,
		BacktraceGraph: fileapi.BacktraceGraph{
			Commands: []string{"include", "add_library"},
			Files:    []string{"CMakeLists.txt", "cmake/extra.cmake"},
			Nodes: []fileapi.BacktraceNode{
				{File: 0},
				{File: 0, Line: 3, Command: 0, Parent: &parent0},
				{File: 1, Line: 5, Command: 1, Parent: &parent1},
			},
		},
	}
	decl, call := targetProvenance(tgt, "", "")
	if decl.File != "cmake/extra.cmake" || decl.Line != 5 {
		t.Errorf("decl = %+v; want cmake/extra.cmake:5", decl)
	}
	if !call.IsZero() {
		t.Errorf("call site = %+v; want zero — include() is not an invocation", call)
	}
}

// TestTargetProvenance_MacroInsideIncludedFile: a macro INVOKED at the top
// level of an include()d file — the call site is that invocation (inside
// the included file), not the include() line above it.
func TestTargetProvenance_MacroInsideIncludedFile(t *testing.T) {
	parent0, parent1, parent2 := 0, 1, 2
	tgt := &fileapi.Target{
		Backtrace: 3,
		BacktraceGraph: fileapi.BacktraceGraph{
			Commands: []string{"include", "add_widget", "add_library"},
			Files:    []string{"CMakeLists.txt", "cmake/extra.cmake", "cmake/helpers.cmake"},
			Nodes: []fileapi.BacktraceNode{
				{File: 0},
				{File: 0, Line: 3, Command: 0, Parent: &parent0},  // include()
				{File: 1, Line: 8, Command: 1, Parent: &parent1},  // add_widget() call
				{File: 2, Line: 21, Command: 2, Parent: &parent2}, // macro body add_library
			},
		},
	}
	decl, call := targetProvenance(tgt, "", "")
	if decl.File != "cmake/helpers.cmake" || decl.Line != 21 {
		t.Errorf("decl = %+v; want cmake/helpers.cmake:21", decl)
	}
	if call.File != "cmake/extra.cmake" || call.Line != 8 || call.Command != "add_widget" {
		t.Errorf("call site = %+v; want cmake/extra.cmake:8 add_widget (stop below the include)", call)
	}
}

// TestTargetProvenance_NoBacktrace: no backtrace (cmake < 3.21 replies)
// yields zero Provenance and zero CallSite — nothing to recover from.
func TestTargetProvenance_NoBacktrace(t *testing.T) {
	tgt := &fileapi.Target{}
	decl, call := targetProvenance(tgt, "", "")
	if !decl.IsZero() || !call.IsZero() {
		t.Errorf("decl=%+v call=%+v; want both zero without a backtrace", decl, call)
	}
}

func TestReanchorProvenanceFile(t *testing.T) {
	const (
		cmakeSrc   = "/tmp/proj/src"
		cmakeBuild = "/tmp/proj/build"
	)
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"already relative passes through",
			"CMakeLists.txt", "CMakeLists.txt"},
		{"inside cmakeSrc — strip",
			"/tmp/proj/src/sub/CMakeLists.txt", "sub/CMakeLists.txt"},
		{"inside cmakeBuild — strip (configure_file shape)",
			"/tmp/proj/build/generated/CMakeLists.txt", "generated/CMakeLists.txt"},
		{"inside cmakeSrc parent (third-party sibling)",
			"/tmp/proj/third-party/foo/CMakeLists.txt", "third-party/foo/CMakeLists.txt"},
		{"completely outside — passes through",
			"/opt/external/foo/CMakeLists.txt", "/opt/external/foo/CMakeLists.txt"},
		{"empty passes through",
			"", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := reanchorProvenanceFile(tc.in, cmakeSrc, cmakeBuild); got != tc.want {
				t.Errorf("reanchorProvenanceFile(%q):\n  got:  %q\n  want: %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestReanchorProvenanceFile_DegenerateRoot(t *testing.T) {
	// Guard: when cmakeSrc is the filesystem root, parent("/") ==
	// "/" and would otherwise match every absolute path. The
	// function should refuse to over-anchor in that case.
	got := reanchorProvenanceFile("/etc/passwd", "/", "/")
	if got != "/etc/passwd" {
		t.Errorf("filesystem-root cmakeSrc: got %q; want unchanged /etc/passwd", got)
	}
}

func TestReanchorProvenanceFile_EmptyAnchors(t *testing.T) {
	// Absolute path + empty anchors: leave alone.
	got := reanchorProvenanceFile("/abs/path/CMakeLists.txt", "", "")
	if got != "/abs/path/CMakeLists.txt" {
		t.Errorf("empty anchors should leave abs path unchanged; got %q", got)
	}
}
