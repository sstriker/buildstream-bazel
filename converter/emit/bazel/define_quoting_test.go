package bazel_test

import (
	"strings"
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/emit/bazel"
	"github.com/sstriker/buildstream-bazel/converter/ir"
)

// TestEmit_DefinesWithEmbeddedQuotes locks the fix for the lz4 (and other
// vintage cmake) shape `add_definitions(-DFOO="1.8.0")`: the codemodel reports
// the define as the literal bytes `FOO="1.8.0"`, and the emit must encode it so
// the C macro actually ends up the STRING "1.8.0".
//
// Two layers matter: (1) the BUILD must parse as Starlark, and (2) Bazel's
// `defines` attribute applies Bourne-shell TOKENIZATION, which strips unescaped
// quotes. The old single-escaped form `"FOO=\"1.8.0\""` parsed fine but
// tokenized down to `-DFOO=1.8.0` (a bare token, not the C string) — the
// compile-commands fidelity lens caught exactly this on VTK (VTK_PARSE_VERSION,
// LZ4_VERSION, H5_ZLIB_HEADER all reached the compiler unquoted). The fix
// backslash-escapes each quote so it survives tokenization: the BUILD carries
// `"FOO=\\\"1.8.0\\\""` → Starlark value `FOO=\"1.8.0\"` → shell → `FOO="1.8.0"`.
func TestEmit_DefinesWithEmbeddedQuotes(t *testing.T) {
	pkg := &ir.Package{
		Name: "lz4",
		Targets: []ir.Target{{
			Name:    "lz4",
			Kind:    ir.KindCCLibrary,
			Srcs:    []string{"lz4.c"},
			Defines: []string{`LZ4_VERSION="1.8.0"`, "NDEBUG"},
		}},
	}
	out, err := bazel.Emit(pkg)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	body := string(out)
	// Tokenization-surviving form: backslash-escaped quotes.
	wantDefine := `"LZ4_VERSION=\\\"1.8.0\\\""`
	if !strings.Contains(body, wantDefine) {
		t.Errorf("emitted body missing tokenization-safe define %q; full body:\n%s", wantDefine, body)
	}
	// Negative: the single-escaped form (which Bazel tokenizes back to an
	// unquoted -DLZ4_VERSION=1.8.0) must NOT appear.
	if strings.Contains(body, `["LZ4_VERSION=\"1.8.0\""`) || strings.Contains(body, ` "LZ4_VERSION=\"1.8.0\"",`) {
		t.Errorf("emitted body contains the tokenization-losing single-escaped define; full body:\n%s", body)
	}
}

// TestEmit_DefinesWithBackslash also escapes (codemodel may carry Windows-style
// paths in defines like INSTALL_PREFIX="C:\foo"). The value `P="C:\\foo"` must
// reach the compiler as the C string "C:\foo".
func TestEmit_DefinesWithBackslash(t *testing.T) {
	pkg := &ir.Package{
		Name: "x",
		Targets: []ir.Target{{
			Name:    "x",
			Kind:    ir.KindCCLibrary,
			Srcs:    []string{"x.c"},
			Defines: []string{`P="C:\\foo"`},
		}},
	}
	out, err := bazel.Emit(pkg)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if !strings.Contains(string(out), `"P=\\\"C:\\\\foo\\\""`) {
		t.Errorf("backslash escaping wrong; body:\n%s", string(out))
	}
}

// TestEmit_CoptsWithEmbeddedQuotes extends the tokenization-survival escaping
// to `copts`: Bazel shell-tokenizes copts too, so a `-Dmacro="value"` that
// arrives via target_compile_options (rather than the structured defines list)
// would have its quotes stripped down to a bare token without escaping.
func TestEmit_CoptsWithEmbeddedQuotes(t *testing.T) {
	pkg := &ir.Package{
		Name: "x",
		Targets: []ir.Target{{
			Name:  "x",
			Kind:  ir.KindCCLibrary,
			Srcs:  []string{"x.c"},
			Copts: []string{`-DVER="1.0"`, "-O2"},
		}},
	}
	out, err := bazel.Emit(pkg)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	body := string(out)
	if !strings.Contains(body, `"-DVER=\\\"1.0\\\""`) {
		t.Errorf("copt quote-escaping missing; body:\n%s", body)
	}
	// An ordinary flag is untouched (no quote/space → no-op).
	if !strings.Contains(body, `"-O2"`) {
		t.Errorf("plain copt mangled; body:\n%s", body)
	}
}

// TestEmit_LinkoptsWithEmbeddedQuotes is the linkopts sibling — linkopts are
// shell-tokenized as well.
func TestEmit_LinkoptsWithEmbeddedQuotes(t *testing.T) {
	pkg := &ir.Package{
		Name: "x",
		Targets: []ir.Target{{
			Name:     "x",
			Kind:     ir.KindCCLibrary,
			Srcs:     []string{"x.c"},
			LinkOpts: []string{`-Wl,--defsym=FOO="bar"`},
		}},
	}
	out, err := bazel.Emit(pkg)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if !strings.Contains(string(out), `"-Wl,--defsym=FOO=\\\"bar\\\""`) {
		t.Errorf("linkopt quote-escaping missing; body:\n%s", string(out))
	}
}

// TestEmit_DefineWithEmbeddedSpace locks the whitespace half of the
// tokenization contract: a define whose quoted value contains a space (one
// logical define) must survive as a SINGLE token. Quote-escaping alone leaves
// the space, which Bazel's word-splitter would break into two -D tokens; the
// space must be backslash-escaped too. `GREETING="a b"` → BUILD text
// `GREETING=\"a\ b\"` → Starlark value `GREETING=\"a\ b\"` → tokenizer →
// `GREETING="a b"`.
func TestEmit_DefineWithEmbeddedSpace(t *testing.T) {
	pkg := &ir.Package{
		Name: "x",
		Targets: []ir.Target{{
			Name:    "x",
			Kind:    ir.KindCCLibrary,
			Srcs:    []string{"x.c"},
			Defines: []string{`GREETING="a b"`},
		}},
	}
	out, err := bazel.Emit(pkg)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if !strings.Contains(string(out), `"GREETING=\\\"a\\ b\\\""`) {
		t.Errorf("embedded-space define not escaped to a single token; body:\n%s", string(out))
	}
}

// TestEmit_TokenizationLeavesShellMetacharsLiteral guards the negative half:
// `;` / `|` are NOT word separators to Bazel's tokenizer, so they must pass
// through verbatim (only the surrounding quotes are escaped), not gain a
// spurious backslash.
func TestEmit_TokenizationLeavesShellMetacharsLiteral(t *testing.T) {
	pkg := &ir.Package{
		Name: "x",
		Targets: []ir.Target{{
			Name:    "x",
			Kind:    ir.KindCCLibrary,
			Srcs:    []string{"x.c"},
			Defines: []string{`LIST="a;b|c"`},
		}},
	}
	out, err := bazel.Emit(pkg)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	body := string(out)
	if !strings.Contains(body, `"LIST=\\\"a;b|c\\\""`) {
		t.Errorf("metachars should stay literal (only quotes escaped); body:\n%s", body)
	}
	if strings.Contains(body, `\;`) || strings.Contains(body, `\|`) {
		t.Errorf("`;`/`|` were escaped but are not word separators; body:\n%s", body)
	}
}
