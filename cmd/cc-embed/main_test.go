package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEncode_StringMode(t *testing.T) {
	h, s := encode([]byte("hello"), "greet", false, false, "", "")
	if !strings.Contains(h, "extern const char *greet;") {
		t.Errorf("header missing string decl:\n%s", h)
	}
	if !strings.Contains(h, "#ifndef greet_h") || !strings.Contains(h, "#endif") {
		t.Errorf("header missing include guard:\n%s", h)
	}
	if !strings.Contains(s, `#include "greet.h"`) {
		t.Errorf("source missing self-include:\n%s", s)
	}
	if !strings.Contains(s, "const char *greet =") || !strings.Contains(s, `"hello"`) {
		t.Errorf("source missing definition:\n%s", s)
	}
}

func TestEscapeCString(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{`plain`, `plain`},
		{`a\b`, `a\\b`},
		{`say "hi"`, `say \"hi\"`},
		{"tab\there", `tab\there`},
		{"cr\rhere", `cr\rhere`},
		{"line1\nline2", "line1\\n\"\n\"line2"}, // \n + physical break + reopened literal
	}
	for _, tc := range tests {
		if got := escapeCString([]byte(tc.in)); got != tc.want {
			t.Errorf("escapeCString(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestStringMode_RoundTrips verifies the C string literal the tool emits
// has a runtime value equal to the input (the faithfulness contract) by
// reversing the escaping the same way a C compiler concatenating adjacent
// literals would.
func TestStringMode_RoundTrips(t *testing.T) {
	inputs := []string{"hello", "a\\b\"c", "multi\nline\ntext", "tab\tand\rcr", ""}
	for _, in := range inputs {
		_, s := encode([]byte(in), "x", false, false, "", "")
		lit := extractStringLiteral(t, s)
		if got := unescapeC(lit); got != in {
			t.Errorf("round-trip failed for %q: got %q", in, got)
		}
	}
}

func TestEncode_BinaryMode(t *testing.T) {
	h, s := encode([]byte{0x01, 0x02, 0xff}, "blob", true, false, "", "")
	if !strings.Contains(h, "extern const unsigned char blob[3];") {
		t.Errorf("header missing binary decl:\n%s", h)
	}
	if !strings.Contains(s, "const unsigned char blob[3] = {") {
		t.Errorf("source missing binary def:\n%s", s)
	}
	for _, want := range []string{"0x01", "0x02", "0xff"} {
		if !strings.Contains(s, want) {
			t.Errorf("source missing byte %s:\n%s", want, s)
		}
	}
}

func TestEncode_BinaryNulTerminate(t *testing.T) {
	h, s := encode([]byte{0x41}, "blob", true, true, "", "")
	if !strings.Contains(h, "extern const unsigned char blob[2];") {
		t.Errorf("nul-terminate should bump size to 2:\n%s", h)
	}
	if !strings.HasSuffix(strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(s), "};")), "0x41,\n0x00") &&
		!strings.Contains(s, "0x41,0x00") {
		t.Errorf("nul-terminate should append 0x00:\n%s", s)
	}
}

func TestEncode_ExportSymbolAndHeader(t *testing.T) {
	h, _ := encode([]byte("x"), "sym", false, false, "MYLIB_EXPORT", "mylib/export.h")
	if !strings.Contains(h, `#include "mylib/export.h"`) {
		t.Errorf("header missing export include:\n%s", h)
	}
	if !strings.Contains(h, "MYLIB_EXPORT extern const char *sym;") {
		t.Errorf("header missing export-symbol decl:\n%s", h)
	}
}

func TestRun_Validation(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "in.txt")
	if err := os.WriteFile(in, []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	ho := filepath.Join(dir, "out.h")
	so := filepath.Join(dir, "out.c")

	if err := run("", "n", ho, so, false, false, "", ""); err == nil {
		t.Error("missing --input should error")
	}
	if err := run(in, "n", ho, so, false, true, "", ""); err == nil {
		t.Error("--nul-terminate without --binary should error")
	}
	if err := run(in, "n", ho, so, false, false, "", ""); err != nil {
		t.Errorf("valid invocation errored: %v", err)
	}
	if _, err := os.Stat(ho); err != nil {
		t.Errorf("header not written: %v", err)
	}
	if _, err := os.Stat(so); err != nil {
		t.Errorf("source not written: %v", err)
	}
}

// --- test helpers: reverse the tool's C-string escaping ---

// extractStringLiteral pulls the concatenated string-literal body out of
// the `const char *x =\n"...";` definition, joining adjacent literals the
// way a C compiler would (dropping the `"<physical newline>"` seams).
func extractStringLiteral(t *testing.T, source string) string {
	t.Helper()
	i := strings.Index(source, "=\n\"")
	if i < 0 {
		t.Fatalf("no string definition in:\n%s", source)
	}
	body := source[i+3:] // past `=\n"`
	end := strings.LastIndex(body, "\";")
	if end < 0 {
		t.Fatalf("unterminated string literal in:\n%s", source)
	}
	body = body[:end]
	// Join adjacent literals: the emitter writes `\n"` + physical newline +
	// `"` at each source newline; drop the `"\n"` seam to concatenate.
	return strings.ReplaceAll(body, "\"\n\"", "")
}

// unescapeC interprets the C escapes the tool emits (\\, \", \n, \r, \t).
func unescapeC(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) {
			switch s[i+1] {
			case '\\':
				b.WriteByte('\\')
			case '"':
				b.WriteByte('"')
			case 'n':
				b.WriteByte('\n')
			case 'r':
				b.WriteByte('\r')
			case 't':
				b.WriteByte('\t')
			default:
				b.WriteByte(s[i+1])
			}
			i++
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}
