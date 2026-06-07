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
