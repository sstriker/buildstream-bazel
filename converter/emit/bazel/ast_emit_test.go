package bazel

import (
	"bytes"
	"testing"

	"github.com/bazelbuild/buildtools/build"
	"github.com/sstriker/buildstream-bazel/converter/ir"
)

// assertKindByteIdentical is the per-kind migration guard: the AST builder's
// output must Format byte-identically to the text template's
// text -> Parse -> Format output for the same target. emitText writes the
// kind's current template rendering; astCall is the AST builder's CallExpr.
func assertKindByteIdentical(t *testing.T, astCall *build.CallExpr, emitText func(*bytes.Buffer) error) {
	t.Helper()
	var buf bytes.Buffer
	if err := emitText(&buf); err != nil {
		t.Fatalf("text emit: %v", err)
	}
	ref, err := build.Parse("BUILD.bazel", buf.Bytes())
	if err != nil {
		t.Fatalf("parse text:\n%s\nerr: %v", buf.String(), err)
	}
	want := formatFile(ref, nil)
	got := formatFile(&build.File{Type: build.TypeBuild, Stmt: []build.Expr{astCall}}, nil)
	if string(got) != string(want) {
		t.Errorf("AST output differs from text template:\n--- want (text) ---\n%s\n--- got (AST) ---\n%s", want, got)
	}
}

func TestASTEmit_Alias(t *testing.T) {
	cases := []ir.Target{
		{Kind: ir.KindAlias, Name: "a", AliasActual: "//bar:baz"},
		{Kind: ir.KindAlias, Name: "a", AliasActual: ":impl", Tags: []string{"manual", "no-ide"}},
		{Kind: ir.KindAlias, Name: "a", AliasActual: "@ext//x:y", Visibility: []string{"//pkg:__subpackages__"}},
		{Kind: ir.KindAlias, Name: "a", AliasActual: ":x", Tags: []string{"t"}, Visibility: []string{"//v:__pkg__"}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.AliasActual, func(t *testing.T) {
			assertKindByteIdentical(t, aliasExpr(tc), func(b *bytes.Buffer) error { return emitAlias(b, tc) })
		})
	}
}

func TestASTEmit_BoolFlag(t *testing.T) {
	cases := []ir.Target{
		{Kind: ir.KindBoolFlag, Name: "f", BoolFlagDefault: false},
		{Kind: ir.KindBoolFlag, Name: "f", BoolFlagDefault: true},
		{Kind: ir.KindBoolFlag, Name: "f", BoolFlagDefault: true, Tags: []string{"manual"}},
		{Kind: ir.KindBoolFlag, Name: "f", BoolFlagDefault: false, Visibility: []string{"//v:__pkg__"}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(boolIdent(tc.BoolFlagDefault).(*build.Ident).Name, func(t *testing.T) {
			assertKindByteIdentical(t, boolFlagExpr(tc), func(b *bytes.Buffer) error { return emitBoolFlag(b, tc) })
		})
	}
}
