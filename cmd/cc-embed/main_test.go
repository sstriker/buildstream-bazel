package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEncode_StringMode(t *testing.T) {
	h, s := encode([]byte("hello"), "greet", "greet.h", false, false, "", "")
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
	inputs := []string{
		"hello", "a\\b\"c", "multi\nline\ntext", "tab\tand\rcr", "",
		"ctrl\x00\x01\x1f\x7fbytes",     // control bytes -> \NNN octal, must round-trip
		"caf\xc3\xa9 \xe2\x9c\x93 high", // >= 0x80 bytes (UTF-8) -> octal, byte-faithful
	}
	for _, in := range inputs {
		_, s := encode([]byte(in), "x", "x.h", false, false, "", "")
		lit := extractStringLiteral(t, s)
		if got := unescapeC(lit); got != in {
			t.Errorf("round-trip failed for %q: got %q", in, got)
		}
	}
}

func TestEncode_BinaryMode(t *testing.T) {
	h, s := encode([]byte{0x01, 0x02, 0xff}, "blob", "blob.h", true, false, "", "")
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
	h, s := encode([]byte{0x41}, "blob", "blob.h", true, true, "", "")
	if !strings.Contains(h, "extern const unsigned char blob[2];") {
		t.Errorf("nul-terminate should bump size to 2:\n%s", h)
	}
	if !strings.HasSuffix(strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(s), "};")), "0x41,\n0x00") &&
		!strings.Contains(s, "0x41,0x00") {
		t.Errorf("nul-terminate should append 0x00:\n%s", s)
	}
}

// TestEncode_HeaderIncludeDecoupledFromSymbol confirms the generated
// source self-includes the actual header basename, not "<symbol>.h" — so
// the output filename isn't constrained by the symbol name.
func TestEncode_HeaderIncludeDecoupledFromSymbol(t *testing.T) {
	_, s := encode([]byte("x"), "my_symbol", "custom_name.h", false, false, "", "")
	if !strings.Contains(s, `#include "custom_name.h"`) {
		t.Errorf("source should #include the actual header basename:\n%s", s)
	}
	if strings.Contains(s, `#include "my_symbol.h"`) {
		t.Errorf("source should not hardcode <symbol>.h:\n%s", s)
	}
}

// TestEncode_GuardAvoidsLeadingUnderscore confirms a leading-underscore
// symbol doesn't produce a reserved-identifier include guard, while normal
// names keep their stable `<name>_h` guard.
func TestEncode_GuardAvoidsLeadingUnderscore(t *testing.T) {
	h, _ := encode([]byte("x"), "_foo", "_foo.h", false, false, "", "")
	if strings.Contains(h, "#ifndef _foo_h") {
		t.Errorf("guard should not begin with underscore (reserved):\n%s", h)
	}
	if !strings.Contains(h, "#ifndef CCEMBED_foo_h") {
		t.Errorf("leading-underscore name should get a prefixed guard:\n%s", h)
	}
	h2, _ := encode([]byte("x"), "foo", "foo.h", false, false, "", "")
	if !strings.Contains(h2, "#ifndef foo_h") {
		t.Errorf("normal-name guard should stay stable:\n%s", h2)
	}
}

func TestEncode_ExportSymbolAndHeader(t *testing.T) {
	h, _ := encode([]byte("x"), "sym", "sym.h", false, false, "MYLIB_EXPORT", "mylib/export.h")
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
	if err := run(in, "n", ho, so, false, false, "SYM", ""); err == nil {
		t.Error("--export-symbol without --export-header should error")
	}
	if err := run(in, "n", ho, so, false, false, "", "hdr.h"); err == nil {
		t.Error("--export-header without --export-symbol should error")
	}
	for _, bad := range []string{"has space", "has-dash", "1leading", "semi;colon"} {
		if err := run(in, bad, ho, so, false, false, "", ""); err == nil {
			t.Errorf("invalid C identifier --name %q should error", bad)
		}
	}
	// Injection guards on the verbatim-emitted export args.
	if err := run(in, "n", ho, so, false, false, "BAD\nSYM", "h.h"); err == nil {
		t.Error("--export-symbol with a newline should error")
	}
	if err := run(in, "n", ho, so, false, false, "SYM", "bad\"hdr.h"); err == nil {
		t.Error("--export-header with a quote should error")
	}
	// Empty input in binary mode would emit a zero-length array.
	empty := filepath.Join(dir, "empty.txt")
	if err := os.WriteFile(empty, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := run(empty, "n", ho, so, true, false, "", ""); err == nil {
		t.Error("--binary on empty input should error (zero-length array)")
	}
	if err := run(empty, "n", ho, so, true, true, "", ""); err != nil {
		t.Errorf("--binary --nul-terminate on empty input should be OK: %v", err)
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

// unescapeC interprets the C escapes the tool emits (\\, \", \n, \r, \t,
// and \NNN octal for control bytes).
func unescapeC(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) {
			c := s[i+1]
			if c >= '0' && c <= '7' {
				// Octal escape: up to 3 digits (matches the tool's \%03o).
				val, j := 0, i+1
				for j < len(s) && j < i+4 && s[j] >= '0' && s[j] <= '7' {
					val = val*8 + int(s[j]-'0')
					j++
				}
				b.WriteByte(byte(val))
				i = j - 1
				continue
			}
			switch c {
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
				b.WriteByte(c)
			}
			i++
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}
