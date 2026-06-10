package lower

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/ir"
	"github.com/sstriker/buildstream-bazel/internal/shadow"
)

// prov is a source-root-relative Provenance for a CMakeLists declaration.
func prov(line int) ir.Provenance {
	return ir.Provenance{File: "CMakeLists.txt", Line: line, Command: "add_library"}
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
	pkg := &ir.Package{Targets: []ir.Target{{Name: "foo", Kind: ir.KindCCLibrary, Provenance: prov(6)}}}

	recoverSourceComments(pkg, dir, dir, "", nil, nil, nil)

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
	pkg := &ir.Package{Targets: []ir.Target{
		{Name: "a", Provenance: prov(2)},
		{Name: "b", Provenance: prov(2)},
	}}

	recoverSourceComments(pkg, dir, dir, "", nil, nil, nil)

	for _, tg := range pkg.Targets {
		if tg.LeadingComment != nil {
			t.Errorf("shared-site target %q got comment %q; want none", tg.Name, tg.LeadingComment)
		}
	}
}

// TestRecoverSourceComments_MacroCallSiteCarried: a target declared inside a
// macro body carries the comment above the macro INVOCATION (its CallSite),
// not the (absent) comment above the body line.
func TestRecoverSourceComments_MacroCallSiteCarried(t *testing.T) {
	dir := t.TempDir()
	cml := filepath.Join(dir, "CMakeLists.txt")
	body := "macro(add_widget name)\n" + // 1
		"  add_library(${name} STATIC ${name}.c)\n" + // 2 (body: Provenance)
		"endmacro()\n" + // 3
		"\n" + // 4
		"# The widget lib, via the helper macro.\n" + // 5 leading for the call (line 6)
		"add_widget(widget)  # macro-made\n" // 6 (invocation: CallSite)
	if err := os.WriteFile(cml, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	pkg := &ir.Package{Targets: []ir.Target{{
		Name:       "widget",
		Kind:       ir.KindCCLibrary,
		Provenance: prov(2),
		CallSite:   ir.Provenance{File: "CMakeLists.txt", Line: 6, Command: "add_widget"},
	}}}

	recoverSourceComments(pkg, dir, dir, "", nil, nil, nil)

	if got := pkg.Targets[0].LeadingComment; len(got) != 1 || got[0] != "# The widget lib, via the helper macro." {
		t.Errorf("leading comment = %q, want the call-site comment", got)
	}
	if got := pkg.Targets[0].TrailingComment; got != "# macro-made" {
		t.Errorf("trailing comment = %q, want %q", got, "# macro-made")
	}
}

// TestRecoverSourceComments_PerInvocationCallSites: a helper invoked twice
// yields two targets sharing one body line (Provenance) but with distinct
// CallSites — each carries its OWN invocation's comment. (Without call
// sites this was the shared-site-skipped case.)
func TestRecoverSourceComments_PerInvocationCallSites(t *testing.T) {
	dir := t.TempDir()
	cml := filepath.Join(dir, "CMakeLists.txt")
	body := "macro(add_widget name)\n" + // 1
		"  add_library(${name} STATIC ${name}.c)\n" + // 2 (shared body line)
		"endmacro()\n" + // 3
		"# the a lib\n" + // 4
		"add_widget(a)\n" + // 5
		"# the b lib\n" + // 6
		"add_widget(b)\n" // 7
	if err := os.WriteFile(cml, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	call := func(line int) ir.Provenance {
		return ir.Provenance{File: "CMakeLists.txt", Line: line, Command: "add_widget"}
	}
	pkg := &ir.Package{Targets: []ir.Target{
		{Name: "a", Provenance: prov(2), CallSite: call(5)},
		{Name: "b", Provenance: prov(2), CallSite: call(7)},
	}}

	recoverSourceComments(pkg, dir, dir, "", nil, nil, nil)

	if got := pkg.Targets[0].LeadingComment; len(got) != 1 || got[0] != "# the a lib" {
		t.Errorf("a's leading comment = %q, want [# the a lib]", got)
	}
	if got := pkg.Targets[1].LeadingComment; len(got) != 1 || got[0] != "# the b lib" {
		t.Errorf("b's leading comment = %q, want [# the b lib]", got)
	}
}

// TestRecoverSourceComments_SharedCallSiteSkipped: one macro invocation that
// declares TWO targets — the call site is shared, so neither target gets the
// comment (same ambiguity policy as shared declaration sites).
func TestRecoverSourceComments_SharedCallSiteSkipped(t *testing.T) {
	dir := t.TempDir()
	cml := filepath.Join(dir, "CMakeLists.txt")
	body := "# both libs at once\n" + // 1
		"add_widget_pair(a b)\n" // 2 (one invocation, two targets)
	if err := os.WriteFile(cml, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	call := ir.Provenance{File: "CMakeLists.txt", Line: 2, Command: "add_widget_pair"}
	pkg := &ir.Package{Targets: []ir.Target{
		{Name: "a", Provenance: prov(9), CallSite: call},
		{Name: "b", Provenance: prov(9), CallSite: call},
	}}

	recoverSourceComments(pkg, dir, dir, "", nil, nil, nil)

	for _, tg := range pkg.Targets {
		if tg.LeadingComment != nil {
			t.Errorf("shared-call-site target %q got comment %q; want none", tg.Name, tg.LeadingComment)
		}
	}
}

func TestRecoverSourceComments_NoHeaderWhenNoFile(t *testing.T) {
	// hostSrc points at a dir with no CMakeLists.txt → no header, no panic.
	pkg := &ir.Package{Targets: []ir.Target{{Name: "foo"}}}
	recoverSourceComments(pkg, t.TempDir(), "", "", nil, nil, nil)
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

	recoverSourceComments(pkg, dir, dir, "", nil, cmds, nil)

	if got := pkg.Targets[0].LeadingComment; len(got) != 1 || got[0] != "# generate the parser tables" {
		t.Errorf("genrule leading comment = %q, want [# generate the parser tables]", got)
	}
	if pkg.Targets[0].Provenance.Line != 2 || pkg.Targets[0].Provenance.Command != "add_custom_command" {
		t.Errorf("genrule provenance = %+v, want line 2 add_custom_command", pkg.Targets[0].Provenance)
	}
}

// TestRecoverSourceComments_CodegenGenrule_MacroCallSite: a genrule whose
// add_custom_command ran inside a macro body carries the comment above the
// macro INVOCATION (the trace-recovered call site), not the body's; the
// genrule is stamped with Provenance (the body line) and CallSite (the
// invocation) so the breadcrumb leads with the invocation too.
func TestRecoverSourceComments_CodegenGenrule_MacroCallSite(t *testing.T) {
	dir := t.TempDir()
	cml := filepath.Join(dir, "CMakeLists.txt")
	body := "macro(make_table out)\n" + // 1
		"  # codegen inside the macro\n" + // 2
		"  add_custom_command(OUTPUT ${out} COMMAND gen)\n" + // 3 (body: Provenance)
		"endmacro()\n" + // 4
		"\n" + // 5
		"# Build the LUT via the helper macro.\n" + // 6 leading for the call (line 7)
		"make_table(gen/tables.inc)  # lut genrule\n" // 7 (invocation: CallSite)
	if err := os.WriteFile(cml, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	pkg := &ir.Package{Targets: []ir.Target{{
		Name:        "gen_tables",
		Kind:        ir.KindGenrule,
		GenruleOuts: []string{"sub/gen/tables.inc"},
	}}}
	cmds := []shadow.AddCustomCommandCall{{
		File: cml, Line: 3, Outputs: []string{"gen/tables.inc"},
		CallFile: cml, CallLine: 7, CallCmd: "make_table",
	}}

	recoverSourceComments(pkg, dir, dir, "", nil, cmds, nil)

	if got := pkg.Targets[0].LeadingComment; len(got) != 1 || got[0] != "# Build the LUT via the helper macro." {
		t.Errorf("genrule leading comment = %q, want the call-site comment", got)
	}
	if got := pkg.Targets[0].TrailingComment; got != "# lut genrule" {
		t.Errorf("genrule trailing comment = %q, want %q", got, "# lut genrule")
	}
	if p := pkg.Targets[0].Provenance; p.Line != 3 || p.Command != "add_custom_command" {
		t.Errorf("genrule provenance = %+v, want line 3 add_custom_command", p)
	}
	if c := pkg.Targets[0].CallSite; c.Line != 7 || c.Command != "make_table" {
		t.Errorf("genrule call site = %+v, want line 7 make_table", c)
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
