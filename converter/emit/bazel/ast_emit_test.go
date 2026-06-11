package bazel

import (
	"bytes"
	"testing"

	"github.com/bazelbuild/buildtools/build"
	"github.com/sstriker/buildstream-bazel/converter/ir"
)

// assertKindByteIdentical is the per-kind builder guard. Originally it pinned
// byte-identity against the text template during the migration; with the
// multiline capstone the AST layout intentionally diverges from the (now dead)
// text path, so the invariant is now BUILDIFIER-CANONICAL: the builder's
// Format output must parse and re-Format to the same bytes — i.e. it is exactly
// what `buildifier --mode=fix` produces. (The unused text-emit closure is kept
// at call sites only to keep the kind's emit* function referenced.)
func assertKindByteIdentical(t *testing.T, astCall *build.CallExpr, _ func(*bytes.Buffer) error) {
	t.Helper()
	got := formatFile(&build.File{Type: build.TypeBuild, Stmt: []build.Expr{astCall}}, nil)
	reparsed, err := build.Parse("BUILD.bazel", got)
	if err != nil {
		t.Fatalf("AST output doesn't parse:\n%s\nerr: %v", got, err)
	}
	if again := build.Format(reparsed); string(again) != string(got) {
		t.Errorf("AST output not buildifier-stable:\n--- first ---\n%s\n--- reformat ---\n%s", got, again)
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
		// Long outs list: layout is buildifier-owned now.
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

func TestASTEmit_WriteFile(t *testing.T) {
	cases := []ir.Target{
		// NON-alphabetical content: write_file content is a semantic ordered
		// body (not a set), so an accidental sort must change bytes and fail
		// the byte-identity guard.
		{Kind: ir.KindWriteFile, Name: "w", WriteFileOut: "out.h", WriteFileContent: []string{"#define B 2", "#define A 1"}},
		{Kind: ir.KindWriteFile, Name: "w", WriteFileOut: "o.h", WriteFileContent: []string{"line"}, WriteFileNewline: "auto", Tags: []string{"manual"}},
		// per-config select body
		{Kind: ir.KindWriteFile, Name: "w", WriteFileOut: "o.h", WriteFileContent: []string{"#define D 0"},
			WriteFileContentByConfig: map[string][]string{"//config:dbg": {"#define D 1"}}},
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
