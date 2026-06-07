package lower

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/internal/fileapi"
	"github.com/sstriker/buildstream-bazel/converter/ir"
	"github.com/sstriker/buildstream-bazel/internal/shadow"
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

	recoverSourceComments(pkg, r, dir, dir, "", nil, nil, nil)

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

	recoverSourceComments(pkg, r, dir, dir, "", nil, nil, nil)

	for _, tg := range pkg.Targets {
		if tg.LeadingComment != nil {
			t.Errorf("shared-site target %q got comment %q; want none", tg.Name, tg.LeadingComment)
		}
	}
}

func TestRecoverSourceComments_NoHeaderWhenNoFile(t *testing.T) {
	// hostSrc points at a dir with no CMakeLists.txt → no header, no panic.
	pkg := &ir.Package{Targets: []ir.Target{{Name: "foo"}}}
	recoverSourceComments(pkg, &fileapi.Reply{}, t.TempDir(), "", "", nil, nil, nil)
	if len(pkg.HeaderComments) != 0 {
		t.Errorf("expected no header comments, got %q", pkg.HeaderComments)
	}
}

// TestRecoverSourceComments_CodegenGenrule covers the "comments before a
// codegen" case: a genrule matched to its add_custom_command trace call by
// output basename gets the call site's leading comment + Provenance.
func TestRecoverSourceComments_CodegenGenrule(t *testing.T) {
	dir := t.TempDir()
	cml := filepath.Join(dir, "CMakeLists.txt")
	body := "# generate the parser tables\n" + // 1 leading for the custom command (line 2)
		"add_custom_command(OUTPUT gen/tables.inc COMMAND gen ARGS)\n" // 2
	if err := os.WriteFile(cml, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	pkg := &ir.Package{Targets: []ir.Target{{
		Name:        "gen_tables",
		Kind:        ir.KindGenrule,
		GenruleOuts: []string{"sub/gen/tables.inc"}, // package-relative; basename matches
	}}}
	cmds := []shadow.AddCustomCommandCall{{File: cml, Line: 2, Outputs: []string{"gen/tables.inc"}}}

	recoverSourceComments(pkg, &fileapi.Reply{}, dir, dir, "", nil, cmds, nil)

	if got := pkg.Targets[0].LeadingComment; len(got) != 1 || got[0] != "# generate the parser tables" {
		t.Errorf("genrule leading comment = %q, want [# generate the parser tables]", got)
	}
	if pkg.Targets[0].Provenance.Line != 2 || pkg.Targets[0].Provenance.Command != "add_custom_command" {
		t.Errorf("genrule provenance = %+v, want line 2 add_custom_command", pkg.Targets[0].Provenance)
	}
}

// TestBuildCodegenSiteIndex_AmbiguousDropped: a basename produced by two
// different sites is dropped rather than misattributed.
func TestBuildCodegenSiteIndex_AmbiguousDropped(t *testing.T) {
	cmds := []shadow.AddCustomCommandCall{
		{File: "a.txt", Line: 1, Outputs: []string{"dup.inc"}},
		{File: "b.txt", Line: 9, Outputs: []string{"dup.inc"}},
		{File: "c.txt", Line: 3, Outputs: []string{"unique.inc"}},
	}
	idx := buildCodegenSiteIndex(nil, cmds, nil)
	if _, ok := idx["dup.inc"]; ok {
		t.Error("ambiguous basename dup.inc should be dropped")
	}
	if s, ok := idx["unique.inc"]; !ok || s.line != 3 {
		t.Errorf("unique.inc = %+v, want line 3", s)
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
