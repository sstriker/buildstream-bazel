package lower

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/internal/fileapi"
	"github.com/sstriker/buildstream-bazel/converter/ir"
)

// makeReply builds a minimal Reply with one target per (name, line) declared
// in `file`, all sharing one BacktraceGraph. Reply.Targets is keyed by target
// id (here, the name).
func makeReply(file string, targets map[string]int) *fileapi.Reply {
	g := fileapi.BacktraceGraph{
		Commands: []string{"add_library"},
		Files:    []string{file},
		Nodes:    []fileapi.BacktraceNode{{}}, // node 0 is the unused root
	}
	type spec struct {
		name string
		node int
	}
	var specs []spec
	for name, line := range targets {
		idx := len(g.Nodes)
		g.Nodes = append(g.Nodes, fileapi.BacktraceNode{File: 0, Line: line, Command: 0})
		specs = append(specs, spec{name: name, node: idx})
	}
	r := &fileapi.Reply{Targets: map[string]fileapi.Target{}}
	for _, s := range specs {
		r.Targets[s.name] = fileapi.Target{Name: s.name, Backtrace: s.node, BacktraceGraph: g}
	}
	return r
}

func TestRecoverSourceComments_LeadingAndHeader(t *testing.T) {
	dir := t.TempDir()
	cml := filepath.Join(dir, "CMakeLists.txt")
	body := "# Copyright 2026 the authors\n" + // 1 file header
		"\n" + // 2
		"cmake_minimum_required(VERSION 3.20)\n" + // 3
		"\n" + // 4
		"# the core library\n" + // 5 leading for foo (line 6)
		"add_library(foo STATIC foo.c)\n" // 6
	if err := os.WriteFile(cml, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	r := makeReply(cml, map[string]int{"foo": 6})
	pkg := &ir.Package{Targets: []ir.Target{{Name: "foo", Kind: ir.KindCCLibrary}}}

	recoverSourceComments(pkg, r, dir)

	if got := pkg.Targets[0].LeadingComment; len(got) != 1 || got[0] != "# the core library" {
		t.Errorf("leading comment = %q, want [# the core library]", got)
	}
	if len(pkg.HeaderComments) == 0 || pkg.HeaderComments[0] != "Copyright 2026 the authors" {
		t.Errorf("header = %q, want first entry 'Copyright 2026 the authors'", pkg.HeaderComments)
	}
}

// TestRecoverSourceComments_SharedSiteSkipped: two targets declared at the same
// line (a helper invoked twice) must NOT get the body comment — ambiguous.
func TestRecoverSourceComments_SharedSiteSkipped(t *testing.T) {
	dir := t.TempDir()
	cml := filepath.Join(dir, "CMakeLists.txt")
	body := "# helper body comment\n" + // 1
		"add_library(${name} STATIC ${srcs})\n" // 2 (one body line, two targets)
	if err := os.WriteFile(cml, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	r := makeReply(cml, map[string]int{"a": 2, "b": 2})
	pkg := &ir.Package{Targets: []ir.Target{{Name: "a"}, {Name: "b"}}}

	recoverSourceComments(pkg, r, dir)

	for _, tg := range pkg.Targets {
		if tg.LeadingComment != nil {
			t.Errorf("shared-site target %q got comment %q; want none", tg.Name, tg.LeadingComment)
		}
	}
}

func TestRecoverSourceComments_NoHeaderWhenNoFile(t *testing.T) {
	// hostSrc points at a dir with no CMakeLists.txt → no header, no panic.
	pkg := &ir.Package{Targets: []ir.Target{{Name: "foo"}}}
	recoverSourceComments(pkg, &fileapi.Reply{}, t.TempDir())
	if len(pkg.HeaderComments) != 0 {
		t.Errorf("expected no header comments, got %q", pkg.HeaderComments)
	}
}

func TestStripCommentPrefix(t *testing.T) {
	cases := map[string]string{
		"# Copyright": "Copyright",
		"#x":          "x",
		"##":          "#",
		"#  indented": " indented",
		"  # spaced":  "spaced",
	}
	for in, want := range cases {
		if got := stripCommentPrefix(in); got != want {
			t.Errorf("stripCommentPrefix(%q) = %q, want %q", in, got, want)
		}
	}
}
