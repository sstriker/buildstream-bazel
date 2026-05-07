package configurefile_test

import (
	"reflect"
	"testing"

	"github.com/sstriker/cmake-to-bazel/internal/configurefile"
)

// TestExtract_FixtureRoundTrip: the load-bearing test for the
// lift's value-capture half. The fixture template + cmake's
// rendered output must round-trip through Extract → Substitute
// to byte-equal the original rendered output.
func TestExtract_FixtureRoundTrip(t *testing.T) {
	tmpl := []byte(`#ifndef CFGLIB_CONFIG_H
#define CFGLIB_CONFIG_H
#define CFGLIB_VERSION_MAJOR @CFGLIB_VERSION_MAJOR@
#define CFGLIB_VERSION_MINOR @CFGLIB_VERSION_MINOR@
#endif
`)
	rendered := []byte(`#ifndef CFGLIB_CONFIG_H
#define CFGLIB_CONFIG_H
#define CFGLIB_VERSION_MAJOR 1
#define CFGLIB_VERSION_MINOR 2
#endif
`)
	values, err := configurefile.Extract(tmpl, rendered, configurefile.Options{})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	want := map[string]string{
		"CFGLIB_VERSION_MAJOR": "1",
		"CFGLIB_VERSION_MINOR": "2",
	}
	if !reflect.DeepEqual(values, want) {
		t.Errorf("values = %v, want %v", values, want)
	}
	// Round-trip back through Substitute.
	got := configurefile.Substitute(tmpl, values, configurefile.Options{})
	if string(got) != string(rendered) {
		t.Errorf("round-trip mismatch\nrendered:\n%s\ngot:\n%s", rendered, got)
	}
}

func TestExtract_DollarVars(t *testing.T) {
	tmpl := []byte("path: ${PREFIX}/lib\n")
	rendered := []byte("path: /opt/foo/lib\n")
	values, err := configurefile.Extract(tmpl, rendered, configurefile.Options{})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	want := map[string]string{"PREFIX": "/opt/foo"}
	if !reflect.DeepEqual(values, want) {
		t.Errorf("values = %v, want %v", values, want)
	}
}

func TestExtract_AtOnlyIgnoresDollar(t *testing.T) {
	// With AtOnly, ${VAR} is treated as literal in both
	// substitute and extract. So the template's literal
	// `${VAR}` must appear unchanged in rendered.
	tmpl := []byte("at: @VAR@; dollar: ${VAR}\n")
	rendered := []byte("at: X; dollar: ${VAR}\n")
	values, err := configurefile.Extract(tmpl, rendered, configurefile.Options{AtOnly: true})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	want := map[string]string{"VAR": "X"}
	if !reflect.DeepEqual(values, want) {
		t.Errorf("values = %v, want %v", values, want)
	}
}

func TestExtract_CmakedefineTruthyAndFalsy(t *testing.T) {
	tmpl := []byte("#cmakedefine HAVE_FOO\n#cmakedefine HAVE_BAR\n")
	rendered := []byte("#define HAVE_FOO\n/* #undef HAVE_BAR */\n")
	values, err := configurefile.Extract(tmpl, rendered, configurefile.Options{})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	// HAVE_FOO truthy → "1" sentinel; HAVE_BAR falsy → "".
	want := map[string]string{"HAVE_FOO": "1", "HAVE_BAR": ""}
	if !reflect.DeepEqual(values, want) {
		t.Errorf("values = %v, want %v", values, want)
	}
	// Round-trip.
	got := configurefile.Substitute(tmpl, values, configurefile.Options{})
	if string(got) != string(rendered) {
		t.Errorf("round-trip\nwant:\n%s\ngot:\n%s", rendered, got)
	}
}

func TestExtract_CmakedefineWithValue(t *testing.T) {
	tmpl := []byte("#cmakedefine FOO @FOO_VAL@\n#cmakedefine BAR @BAR_VAL@\n")
	rendered := []byte("#define FOO 42\n/* #undef BAR */\n")
	values, err := configurefile.Extract(tmpl, rendered, configurefile.Options{})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	// FOO truthy with VAL=42; BAR falsy.
	if values["FOO"] != "1" && !isTruthy(values["FOO"]) {
		t.Errorf("FOO should be truthy; got %q", values["FOO"])
	}
	if values["FOO_VAL"] != "42" {
		t.Errorf("FOO_VAL = %q, want \"42\"", values["FOO_VAL"])
	}
	if values["BAR"] != "" {
		t.Errorf("BAR should be falsy (empty); got %q", values["BAR"])
	}
	// Round-trip.
	got := configurefile.Substitute(tmpl, values, configurefile.Options{})
	if string(got) != string(rendered) {
		t.Errorf("round-trip\nwant:\n%s\ngot:\n%s", rendered, got)
	}
}

func TestExtract_Cmakedefine01(t *testing.T) {
	tmpl := []byte("#cmakedefine01 USE_FOO\n#cmakedefine01 USE_BAR\n")
	rendered := []byte("#define USE_FOO 1\n#define USE_BAR 0\n")
	values, err := configurefile.Extract(tmpl, rendered, configurefile.Options{})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	want := map[string]string{"USE_FOO": "1", "USE_BAR": "0"}
	if !reflect.DeepEqual(values, want) {
		t.Errorf("values = %v, want %v", values, want)
	}
}

func TestExtract_LiteralLineMustMatch(t *testing.T) {
	// A line with no markers must be byte-equal in template
	// and rendered. If the user shipped cmake with a different
	// .h.in (e.g., they edited the template manually
	// post-configure and re-ran the converter), the literal
	// drift would surface here as an error, which is the
	// right outcome (Extract refuses; caller falls back to
	// base64).
	tmpl := []byte("// fixed comment\n@VAR@\n")
	rendered := []byte("// DIFFERENT comment\nvalue\n")
	if _, err := configurefile.Extract(tmpl, rendered, configurefile.Options{}); err == nil {
		t.Error("expected literal-drift error")
	}
}

func TestExtract_LineCountMismatch(t *testing.T) {
	tmpl := []byte("@A@\n@B@\n")
	rendered := []byte("X\nY\nZ\n") // extra line
	if _, err := configurefile.Extract(tmpl, rendered, configurefile.Options{}); err == nil {
		t.Error("expected line-count error")
	}
}

func TestExtract_AdjacentMarkersFail(t *testing.T) {
	// Adjacent markers without a literal anchor between them
	// are alignment-ambiguous. extractPlain greedily assigns
	// the entire span to the first marker; the verification
	// pass (round-trip Substitute) then fails because the
	// second marker's value is empty. Ensures the caller
	// falls back to base64 rather than emit a wrong genrule.
	tmpl := []byte("@A@@B@\n")
	rendered := []byte("AlphaBeta\n")
	if _, err := configurefile.Extract(tmpl, rendered, configurefile.Options{}); err == nil {
		t.Error("expected ambiguous-alignment error")
	}
}

func TestExtract_RepeatedMarkerSameValue(t *testing.T) {
	tmpl := []byte("first: @VAR@\nsecond: @VAR@\n")
	rendered := []byte("first: X\nsecond: X\n")
	values, err := configurefile.Extract(tmpl, rendered, configurefile.Options{})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if values["VAR"] != "X" {
		t.Errorf("VAR = %q, want %q", values["VAR"], "X")
	}
}

func TestExtract_RepeatedMarkerConflictingValues(t *testing.T) {
	tmpl := []byte("first: @VAR@\nsecond: @VAR@\n")
	rendered := []byte("first: X\nsecond: Y\n")
	if _, err := configurefile.Extract(tmpl, rendered, configurefile.Options{}); err == nil {
		t.Error("expected conflicting-value error")
	}
}

func TestExtract_EmptyTemplateAndRendered(t *testing.T) {
	values, err := configurefile.Extract(nil, nil, configurefile.Options{})
	if err != nil {
		t.Fatalf("Extract empty: %v", err)
	}
	if len(values) != 0 {
		t.Errorf("empty template should produce empty values; got %v", values)
	}
}

// TestExtract_CopyOnly_EmptyValues: COPYONLY templates surface
// no values; the verify-pass ensures rendered == template
// (modulo NewlineStyle).
func TestExtract_CopyOnly_EmptyValues(t *testing.T) {
	tmpl := []byte("verbatim @FOO@ ${BAR}\n#cmakedefine HAVE_X\n")
	rendered := tmpl // COPYONLY: rendered IS template.
	values, err := configurefile.Extract(tmpl, rendered, configurefile.Options{CopyOnly: true})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if len(values) != 0 {
		t.Errorf("COPYONLY values should be empty; got %v", values)
	}
}

func TestExtract_CopyOnly_DriftIsError(t *testing.T) {
	tmpl := []byte("@FOO@\n")
	rendered := []byte("changed\n")
	if _, err := configurefile.Extract(tmpl, rendered, configurefile.Options{CopyOnly: true}); err == nil {
		t.Error("expected COPYONLY drift error")
	}
}

// TestExtract_EscapeQuotes_RecoverValuesWithQuotes: when
// ESCAPE_QUOTES is on, the rendered output has `\"` for any
// `"` in the substituted value. Extract undoes the escape so
// the recovered value is the original.
func TestExtract_EscapeQuotes_RecoverValuesWithQuotes(t *testing.T) {
	tmpl := []byte(`literal "kept" @FOO@`)
	rendered := []byte(`literal "kept" with \"quotes\"`)
	values, err := configurefile.Extract(tmpl, rendered, configurefile.Options{EscapeQuotes: true})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	want := `with "quotes"`
	if values["FOO"] != want {
		t.Errorf("FOO = %q, want %q", values["FOO"], want)
	}
}

// TestExtract_NewlineStyle_CRLFRendered: when the rendered
// output uses CRLF line endings (mirroring NEWLINE_STYLE CRLF),
// Extract's line splitter aligns 1:1 with the LF template.
func TestExtract_NewlineStyle_CRLFRendered(t *testing.T) {
	tmpl := []byte("@A@\n@B@\n")
	rendered := []byte("1\r\n2\r\n")
	values, err := configurefile.Extract(tmpl, rendered, configurefile.Options{NewlineStyle: configurefile.NewlineCRLF})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if values["A"] != "1" || values["B"] != "2" {
		t.Errorf("recovered values = %v, want A=1 B=2", values)
	}
}

// TestExtract_RecursiveExpansion: when a value itself contains
// a marker, cmake recursively expands it. Substitute matches
// (recursionLimit loop). Extract should accept rendered
// outputs whose recovered values produce the same output via
// the verify-pass.
func TestExtract_RecursiveExpansion(t *testing.T) {
	tmpl := []byte("@OUTER@\n")
	rendered := []byte("real-value\n")
	// Extract sees the literal output "real-value" and
	// recovers OUTER="real-value". The verify-pass
	// Substitute(template, {OUTER:"real-value"}, opts) →
	// "real-value\n", byte-equal. We don't recover the
	// transitive INNER → real-value relationship, but the
	// verify-pass ensures soundness.
	values, err := configurefile.Extract(tmpl, rendered, configurefile.Options{})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if values["OUTER"] != "real-value" {
		t.Errorf("OUTER = %q, want %q", values["OUTER"], "real-value")
	}
}

// isTruthy is the same check Substitute uses; mirrored here
// for the test assertion shape.
func isTruthy(v string) bool {
	if v == "" {
		return false
	}
	switch v {
	case "0", "OFF", "NO", "FALSE", "N", "IGNORE", "NOTFOUND":
		return false
	}
	return true
}
