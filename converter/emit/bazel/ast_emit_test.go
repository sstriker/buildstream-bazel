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

// compareExprToString asserts an AST attribute value Formats identically to
// attrExpr/scalarAttrExpr's string rendering (parsed back). Empty string <=>
// nil expr (the attribute is omitted).
func compareExprToString(t *testing.T, got build.Expr, wantStr string) {
	t.Helper()
	if wantStr == "" {
		if got != nil {
			t.Errorf("empty attr should be nil expr, got %#v", got)
		}
		return
	}
	ref, err := build.Parse("BUILD.bazel", []byte("x = "+wantStr+"\n"))
	if err != nil {
		t.Fatalf("parse %q: %v", wantStr, err)
	}
	want := build.Format(ref)
	gotFile := &build.File{Type: build.TypeBuild, Stmt: []build.Expr{
		&build.AssignExpr{LHS: &build.Ident{Name: "x"}, Op: "=", RHS: got},
	}}
	if g := build.Format(gotFile); string(g) != string(want) {
		t.Errorf("attr AST != string %q:\n--- want ---\n%s\n--- got ---\n%s", wantStr, want, g)
	}
}

// The keystone: attrExprAST / scalarAttrExprAST must match attrExpr /
// scalarAttrExpr (list / select / list+select / scalar-select forms).
func TestAttrExprAST_Equivalence(t *testing.T) {
	listCases := []struct {
		flat []string
		sel  map[string][]string
	}{
		{nil, nil},
		{[]string{"a.c", "b.c"}, nil},
		{nil, map[string][]string{"@platforms//os:linux": {"x.c"}}},
		{nil, map[string][]string{"@platforms//os:linux": {"l.c"}, "//conditions:default": {"d.c"}}},
		{[]string{"base.c"}, map[string][]string{"@platforms//os:linux": {"l.c"}, "@platforms//os:windows": {"w.c"}}},
	}
	for _, tc := range listCases {
		compareExprToString(t, attrExprAST(tc.flat, tc.sel), attrExpr(tc.flat, tc.sel))
	}
	scalarCases := []struct {
		flat string
		sel  map[string]string
	}{
		{"", nil},
		{"libfoo.a", nil},
		{"", map[string]string{"@platforms//os:linux": "liblinux.a"}},
		{"libdefault.a", map[string]string{"@platforms//os:linux": "liblinux.a"}},
	}
	for _, tc := range scalarCases {
		compareExprToString(t, scalarAttrExprAST(tc.flat, tc.sel), scalarAttrExpr(tc.flat, tc.sel))
	}
}

func TestASTEmit_ConfigSetting(t *testing.T) {
	for _, tc := range []ir.Target{
		{Kind: ir.KindConfigSetting, Name: "dbg", ConfigSettingFlag: "//flag:opt", ConfigSettingValue: "dbg"},
		{Kind: ir.KindConfigSetting, Name: "rel", ConfigSettingFlag: "@x//f:y", ConfigSettingValue: "1", Visibility: []string{"//v:__pkg__"}},
	} {
		tc := tc
		t.Run(tc.Name, func(t *testing.T) {
			assertKindByteIdentical(t, configSettingExpr(tc), func(b *bytes.Buffer) error { return emitConfigSetting(b, tc) })
		})
	}
}

func TestASTEmit_PickFile(t *testing.T) {
	tc := ir.Target{Kind: ir.KindPickFile, Name: "p", PickSrc: ":group", PickPath: "out/x.h", Tags: []string{"t"}, Visibility: []string{"//v:__pkg__"}}
	assertKindByteIdentical(t, pickFileExpr(tc), func(b *bytes.Buffer) error { return emitPickFile(b, tc) })
}

func TestASTEmit_CCHash(t *testing.T) {
	tc := ir.Target{Kind: ir.KindCCHash, Name: "h", Tags: []string{"manual"},
		CCHash: &ir.CCHashSpec{Src: "in.bin", Name: "FOO_SHA", Algorithm: "SHA256", OutHeader: "foo_sha.h"}}
	call, err := ccHashExpr(tc)
	if err != nil {
		t.Fatal(err)
	}
	assertKindByteIdentical(t, call, func(b *bytes.Buffer) error { return emitCCHash(b, tc) })
}

func TestASTEmit_Filegroup(t *testing.T) {
	plain := ir.Target{Kind: ir.KindFilegroup, Name: "g", Srcs: []string{"b.txt", "a.txt"}, FilegroupOutputGroup: "hdrs", Visibility: []string{"//v:__pkg__"}}
	glob := ir.Target{Kind: ir.KindFilegroup, Name: "g", FilegroupGlob: []string{"**/*.h", "*.inc"}}
	for _, tc := range []ir.Target{plain, glob} {
		tc := tc
		t.Run(tc.Name, func(t *testing.T) {
			call, err := filegroupExpr(tc)
			if err != nil {
				t.Fatal(err)
			}
			assertKindByteIdentical(t, call, func(b *bytes.Buffer) error { return emitFilegroup(b, tc) })
		})
	}
}

func TestASTEmit_ShBinary(t *testing.T) {
	tc := ir.Target{Kind: ir.KindShBinary, Name: "s", Srcs: []string{"run.sh"}, Tags: []string{"manual"}}
	assertKindByteIdentical(t, shBinaryExpr(tc), func(b *bytes.Buffer) error { return emitShBinary(b, tc) })
}

func TestASTEmit_CCImport(t *testing.T) {
	for _, tc := range []ir.Target{
		{Kind: ir.KindCCImport, Name: "i", StaticLibrary: "libfoo.a", Hdrs: []string{"foo.h"}},
		{Kind: ir.KindCCImport, Name: "i", SharedLibrary: "libfoo.so", Visibility: []string{"//v:__pkg__"}},
	} {
		tc := tc
		t.Run(tc.StaticLibrary+tc.SharedLibrary, func(t *testing.T) {
			assertKindByteIdentical(t, ccImportExpr(tc), func(b *bytes.Buffer) error { return emitCCImport(b, tc) })
		})
	}
}

func TestASTEmit_Genrule(t *testing.T) {
	cases := []ir.Target{
		{Kind: ir.KindGenrule, Name: "g", GenruleOuts: []string{"out.h"}, GenruleCmd: "$(location //t:x) $@"},
		{Kind: ir.KindGenrule, Name: "g", Srcs: []string{"in.txt"}, GenruleOuts: []string{"o.h"},
			GenruleCmd: "cp $< $@", GenruleTools: []string{"//t:x"}, Tags: []string{"manual"}, Visibility: []string{"//v:__pkg__"}},
		// Long outs list (single-line form > 60) must force multi-line, matching strList.
		{Kind: ir.KindGenrule, Name: "g", GenruleCmd: "x",
			GenruleOuts: []string{"gen/aaaaaaaa.h", "gen/bbbbbbbb.h", "gen/cccccccc.h", "gen/dddddddd.h"}},
	}
	for i, tc := range cases {
		tc := tc
		t.Run(string(rune('a'+i)), func(t *testing.T) {
			assertKindByteIdentical(t, genruleExpr(tc), func(b *bytes.Buffer) error { return emitGenrule(b, tc) })
		})
	}
}

func TestASTEmit_CCEmbed(t *testing.T) {
	cases := []ir.Target{
		{Kind: ir.KindCCEmbed, Name: "e", CCEmbed: &ir.CCEmbedSpec{Src: "a.bin", Symbol: "kA", OutHeader: "a.h", OutSource: "a.c"}},
		{Kind: ir.KindCCEmbed, Name: "e", Tags: []string{"manual"}, CCEmbed: &ir.CCEmbedSpec{
			Src: "a.bin", Symbol: "kA", OutHeader: "a.h", OutSource: "a.c",
			Binary: true, NulTerminate: true, ExportSymbol: "EXP", ExportHeader: "exp.h"}},
	}
	for i, tc := range cases {
		tc := tc
		t.Run(string(rune('a'+i)), func(t *testing.T) {
			call, err := ccEmbedExpr(tc)
			if err != nil {
				t.Fatal(err)
			}
			assertKindByteIdentical(t, call, func(b *bytes.Buffer) error { return emitCCEmbed(b, tc) })
		})
	}
}

// strListExpr must match strList's >60-char multi-line rule across the
// boundary, so a list rendered inline by strList stays inline and one strList
// wraps stays wrapped.
func TestStrListExpr_MatchesStrList(t *testing.T) {
	cases := [][]string{
		{},
		{"a"},
		{"short", "list"},
		{"aaaaaaaaaa", "bbbbbbbbbb", "cccccccccc", "dddddddddd"}, // ~ boundary
		{"a/very/long/path/one.cc", "a/very/long/path/two.cc", "three.cc"},
	}
	for _, c := range cases {
		compareExprToString(t, strListExpr(c), strList(c))
	}
}

func TestASTEmit_WriteFile(t *testing.T) {
	cases := []ir.Target{
		// NON-alphabetical content: write_file content is a semantic ordered
		// body (not a set), so an accidental sort must change bytes and fail
		// the byte-identity guard.
		{Kind: ir.KindWriteFile, Name: "w", WriteFileOut: "out.h", WriteFileContent: []string{"#define B 2", "#define A 1"}},
		{Kind: ir.KindWriteFile, Name: "w", WriteFileOut: "o.h", WriteFileContent: []string{"line"}, WriteFileNewline: "auto", Tags: []string{"manual"}},
		// per-config select body — multi-line NON-alphabetical content in both
		// the default body and the config arm, so an accidental sort in either
		// the flat path or selectListExpr's arm path changes bytes.
		{Kind: ir.KindWriteFile, Name: "w", WriteFileOut: "o.h", WriteFileContent: []string{"#define Z 0", "#define A 0"},
			WriteFileContentByConfig: map[string][]string{"//config:dbg": {"#define Z 1", "#define A 1"}}},
	}
	for i, tc := range cases {
		tc := tc
		t.Run(string(rune('a'+i)), func(t *testing.T) {
			assertKindByteIdentical(t, writeFileExpr(tc), func(b *bytes.Buffer) error { return emitWriteFile(b, tc) })
		})
	}
}

func TestASTEmit_PkgFiles(t *testing.T) {
	cases := []ir.Target{
		{Kind: ir.KindPkgFiles, Name: "p", Srcs: []string{"a.h", "b.h"}, PkgPrefix: "include"},
		{Kind: ir.KindPkgFiles, Name: "p", Srcs: []string{"inc"}, PkgSrcsGlob: true, PkgStripPrefix: "inc"},
		{Kind: ir.KindPkgFiles, Name: "p", Srcs: []string{"x.h"}, PkgRenames: map[string]string{"x.h": "renamed/x.h"}},
	}
	for i, tc := range cases {
		tc := tc
		t.Run(string(rune('a'+i)), func(t *testing.T) {
			assertKindByteIdentical(t, pkgFilesExpr(tc), func(b *bytes.Buffer) error { return emitPkgFiles(b, tc) })
		})
	}
}

func TestASTEmit_CMakeConfigureFile(t *testing.T) {
	cases := []ir.Target{
		{Kind: ir.KindCMakeConfigureFile, Name: "c", CMakeConfigureFile: &ir.CMakeConfigureFileSpec{
			Out: "config.h", Template: "config.h.in", Values: map[string]string{"VERSION": "1.0"}, Tool: "//tools:ccf"}},
		{Kind: ir.KindCMakeConfigureFile, Name: "c", Tags: []string{"manual"}, CMakeConfigureFile: &ir.CMakeConfigureFileSpec{
			Out: "c.h", Content: "#define X @X@\n", Values: map[string]string{"X": "1"}, Tool: "//tools:ccf",
			AtOnly: true, EscapeQuotes: true, NewlineStyle: "LF",
			StampValues: map[string]string{"GIT_SHA": "STABLE_GIT_SHA"},
			TargetFiles: map[string]string{"//x:y": "f"}}},
		{Kind: ir.KindCMakeConfigureFile, Name: "c", CMakeConfigureFile: &ir.CMakeConfigureFileSpec{
			Out: "c.h", Template: "c.h.in", Values: map[string]string{}, Tool: "//tools:ccf",
			GenexValuesPerConfig: map[string]map[string]string{"//config:dbg": {"OPT": "0"}, "//config:rel": {"OPT": "3"}}}},
	}
	for i, tc := range cases {
		tc := tc
		t.Run(string(rune('a'+i)), func(t *testing.T) {
			call, err := cmakeConfigureFileExpr(tc)
			if err != nil {
				t.Fatal(err)
			}
			assertKindByteIdentical(t, call, func(b *bytes.Buffer) error { return emitCMakeConfigureFile(b, tc) })
		})
	}
}
