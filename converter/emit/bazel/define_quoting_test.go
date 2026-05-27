package bazel_test

import (
	"strings"
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/emit/bazel"
	"github.com/sstriker/buildstream-bazel/converter/ir"
)

// TestEmit_DefinesWithEmbeddedQuotes locks the fix for the lz4
// (and other vintage cmake) shape `add_definitions(-DFOO="1.8.0")`
// — the codemodel reports the define as the literal bytes
// `FOO="1.8.0"`, and emit must escape the embedded quotes so the
// resulting BUILD.bazel parses. The single-line strList previously
// wrote raw bytes (the multi-line arm already used %q), producing
// `defines = ["FOO="1.8.0""]` which Bazel rejects.
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
	wantDefine := `"LZ4_VERSION=\"1.8.0\""`
	if !strings.Contains(body, wantDefine) {
		t.Errorf("emitted body missing escaped define %q; full body:\n%s", wantDefine, body)
	}
	// Negative: the raw-bytes form should NOT appear.
	if strings.Contains(body, `"LZ4_VERSION="1.8.0""`) {
		t.Errorf("emitted body contains unescaped double-quoted define; full body:\n%s", body)
	}
}

// TestEmit_DefinesWithBackslash also escapes (codemodel may carry
// Windows-style paths in defines like INSTALL_PREFIX="C:\foo\bar").
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
	if !strings.Contains(string(out), `"P=\"C:\\\\foo\""`) {
		t.Errorf("backslash escaping wrong; body:\n%s", string(out))
	}
}
