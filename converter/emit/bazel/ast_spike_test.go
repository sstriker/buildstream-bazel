package bazel

import (
	"testing"

	"github.com/bazelbuild/buildtools/build"
)

// AST-direct emit spike: prove that constructing a rule via buildtools'
// NewRule + SetAttr and Format-ing it produces BYTE-IDENTICAL output to the
// current text -> build.Parse -> build.Format pipeline, and benchmark the
// build.Parse cost the AST path would eliminate. This de-risks the
// "AST-direct BUILD emit" ROADMAP item before any production rewrite.

func sStr(v string) build.Expr { return &build.StringExpr{Value: v} }
func sList(vs ...string) build.Expr {
	items := make([]build.Expr, len(vs))
	for i, v := range vs {
		items[i] = sStr(v)
	}
	return &build.ListExpr{List: items}
}

// buildCcLibraryAST constructs a representative cc_library the way an
// AST-direct emitter would. Attributes are SetAttr'd in arbitrary order; the
// Format rewriter reorders them per tables.NamePriority, exactly as the
// current pipeline relies on.
func buildCcLibraryAST() *build.File {
	call := &build.CallExpr{X: &build.Ident{Name: "cc_library"}}
	r := build.NewRule(call)
	r.SetAttr("visibility", sList("//visibility:public"))
	r.SetAttr("deps", sList(":bar", "@abseil-cpp//absl/strings"))
	r.SetAttr("copts", sList("-Wall", "-fno-exceptions"))
	r.SetAttr("hdrs", sList("foo.h", "foo_internal.h"))
	r.SetAttr("srcs", sList("foo.cc", "bar.cc"))
	r.SetAttr("name", sStr("foo"))
	r.SetAttr("linkstatic", &build.Ident{Name: "True"})
	return &build.File{Type: build.TypeBuild, Stmt: []build.Expr{call}}
}

// the same rule as text, in a DELIBERATELY non-canonical attribute order and
// spacing — the text path the current emitter feeds through Parse + Format.
const ccLibraryText = `cc_library(
    name = "foo",
    srcs = ["foo.cc", "bar.cc"],
    hdrs = ["foo.h", "foo_internal.h"],
    copts = ["-Wall", "-fno-exceptions"],
    deps = [":bar", "@abseil-cpp//absl/strings"],
    visibility = ["//visibility:public"],
    linkstatic = True,
)
`

func TestASTEmitSpike_ByteIdentical(t *testing.T) {
	// Reference: the current pipeline (text -> Parse -> Format).
	ref, err := build.Parse("BUILD.bazel", []byte(ccLibraryText))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	want := build.Format(ref)

	// Candidate: AST-direct (NewRule + SetAttr -> Format).
	got := build.Format(buildCcLibraryAST())

	if string(got) != string(want) {
		t.Errorf("AST-direct output differs from text->Parse->Format:\n--- want ---\n%s\n--- got ---\n%s", want, got)
	}
}

func BenchmarkEmit_TextParseFormat(b *testing.B) {
	src := []byte(ccLibraryText)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		f, err := build.Parse("BUILD.bazel", src)
		if err != nil {
			b.Fatal(err)
		}
		_ = build.Format(f)
	}
}

func BenchmarkEmit_ASTDirect(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = build.Format(buildCcLibraryAST())
	}
}

// Mixed-assembly spike: can we convert only the HOT rule kinds to AST-direct
// and leave the rare kinds on the text path, assembling both into ONE
// *build.File? If a File built from [AST rule, parsed-from-text rule] Formats
// byte-identically to the whole two-rule text parsed+formatted, the migration
// is INCREMENTAL (convert the high-volume kinds first, defer the long tail) —
// not all-or-nothing. This is the trade-off pivot.
func TestASTEmitSpike_MixedAssembly(t *testing.T) {
	genruleText := `genrule(
    name = "gen",
    srcs = ["in.txt"],
    outs = ["out.h"],
    cmd = "$(location //tools:g) $< > $@",
    tools = ["//tools:g"],
)
`
	// Whole-file reference: both rules as one text blob -> Parse -> Format.
	whole := ccLibraryText + "\n" + genruleText
	refFile, err := build.Parse("BUILD.bazel", []byte(whole))
	if err != nil {
		t.Fatalf("parse whole: %v", err)
	}
	want := build.Format(refFile)

	// Mixed: cc_library via AST, genrule parsed from its own text snippet,
	// assembled into one File.
	astFile := buildCcLibraryAST()
	genFile, err := build.Parse("BUILD.bazel", []byte(genruleText))
	if err != nil {
		t.Fatalf("parse gen snippet: %v", err)
	}
	mixed := &build.File{Type: build.TypeBuild}
	mixed.Stmt = append(mixed.Stmt, astFile.Stmt...) // AST cc_library
	mixed.Stmt = append(mixed.Stmt, genFile.Stmt...) // parsed genrule
	got := build.Format(mixed)

	if string(got) != string(want) {
		t.Errorf("mixed assembly differs from whole-file parse (incremental migration NOT byte-identical):\n--- want ---\n%s\n--- got ---\n%s", want, got)
	}
}
