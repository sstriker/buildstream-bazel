package configurefile_test

import (
	"strings"
	"testing"

	"github.com/sstriker/cmake-to-bazel/internal/configurefile"
)

func TestSubstitute_AtVarsBasic(t *testing.T) {
	tmpl := []byte("#define VERSION_MAJOR @MAJOR@\n#define VERSION_MINOR @MINOR@\n")
	values := map[string]string{"MAJOR": "1", "MINOR": "2"}
	got := string(configurefile.Substitute(tmpl, values, configurefile.Options{}))
	want := "#define VERSION_MAJOR 1\n#define VERSION_MINOR 2\n"
	if got != want {
		t.Errorf("Substitute mismatch\nwant:\n%s\ngot:\n%s", want, got)
	}
}

// TestSubstitute_FixtureMatch byte-equal against the recorded
// `configure-file` sample-project. This is the load-bearing
// test: the lift's whole purpose is to produce the same bytes
// cmake produces, otherwise the genrule emits a different
// config.h than the project expects.
func TestSubstitute_FixtureMatch(t *testing.T) {
	tmpl := []byte(`#ifndef CFGLIB_CONFIG_H
#define CFGLIB_CONFIG_H
#define CFGLIB_VERSION_MAJOR @CFGLIB_VERSION_MAJOR@
#define CFGLIB_VERSION_MINOR @CFGLIB_VERSION_MINOR@
#endif
`)
	values := map[string]string{
		"CFGLIB_VERSION_MAJOR": "1",
		"CFGLIB_VERSION_MINOR": "2",
	}
	want := `#ifndef CFGLIB_CONFIG_H
#define CFGLIB_CONFIG_H
#define CFGLIB_VERSION_MAJOR 1
#define CFGLIB_VERSION_MINOR 2
#endif
`
	got := string(configurefile.Substitute(tmpl, values, configurefile.Options{}))
	if got != want {
		t.Errorf("fixture mismatch\nwant:\n%s\ngot:\n%s", want, got)
	}
}

func TestSubstitute_DollarVars(t *testing.T) {
	tmpl := []byte("path: ${PREFIX}/lib\n")
	values := map[string]string{"PREFIX": "/opt/foo"}
	got := string(configurefile.Substitute(tmpl, values, configurefile.Options{}))
	want := "path: /opt/foo/lib\n"
	if got != want {
		t.Errorf("dollar-var mismatch\nwant:\n%s\ngot:\n%s", want, got)
	}
}

func TestSubstitute_AtOnlySkipsDollarVars(t *testing.T) {
	tmpl := []byte("at: @VAR@; dollar: ${VAR}\n")
	values := map[string]string{"VAR": "X"}
	got := string(configurefile.Substitute(tmpl, values, configurefile.Options{AtOnly: true}))
	want := "at: X; dollar: ${VAR}\n"
	if got != want {
		t.Errorf("@ONLY mismatch\nwant:\n%s\ngot:\n%s", want, got)
	}
}

func TestSubstitute_UnknownVarBecomesEmpty(t *testing.T) {
	tmpl := []byte("hello @MISSING@ world\n")
	got := string(configurefile.Substitute(tmpl, nil, configurefile.Options{}))
	want := "hello  world\n"
	if got != want {
		t.Errorf("unknown var\nwant:%q\ngot:%q", want, got)
	}
}

func TestSubstitute_AtMarkerNotConsumedWhenInvalidIdent(t *testing.T) {
	// `@foo` (no closing @) and `@123` (digit start) shouldn't
	// be recognized as @VAR@ markers.
	cases := map[string]string{
		"email: foo@bar.com\n":      "email: foo@bar.com\n",
		"weird: @123abc@\n":         "weird: @123abc@\n",
		"open: @VAR but no close\n": "open: @VAR but no close\n",
	}
	for in, want := range cases {
		got := string(configurefile.Substitute([]byte(in), map[string]string{"VAR": "X", "abc": "Y"}, configurefile.Options{}))
		if got != want {
			t.Errorf("invalid-ident handling\nin:  %q\nwant:%q\ngot: %q", in, want, got)
		}
	}
}

func TestSubstitute_CmakedefineTruthy(t *testing.T) {
	tmpl := []byte("#cmakedefine HAVE_FOO\n#cmakedefine HAVE_BAR\n")
	values := map[string]string{
		"HAVE_FOO": "1",
		// HAVE_BAR unset → falsy.
	}
	got := string(configurefile.Substitute(tmpl, values, configurefile.Options{}))
	want := "#define HAVE_FOO\n/* #undef HAVE_BAR */\n"
	if got != want {
		t.Errorf("#cmakedefine truthy/falsy\nwant:\n%s\ngot:\n%s", want, got)
	}
}

func TestSubstitute_CmakedefineWithValue(t *testing.T) {
	tmpl := []byte("#cmakedefine FOO @FOO_VAL@\n#cmakedefine BAR @BAR_VAL@\n")
	values := map[string]string{
		"FOO":     "yes",
		"FOO_VAL": "42",
		"BAR":     "",
		"BAR_VAL": "99",
	}
	got := string(configurefile.Substitute(tmpl, values, configurefile.Options{}))
	want := "#define FOO 42\n/* #undef BAR */\n"
	if got != want {
		t.Errorf("#cmakedefine FOO @VAL@\nwant:\n%s\ngot:\n%s", want, got)
	}
}

func TestSubstitute_Cmakedefine01(t *testing.T) {
	tmpl := []byte("#cmakedefine01 USE_FOO\n#cmakedefine01 USE_BAR\n")
	values := map[string]string{
		"USE_FOO": "1",
		"USE_BAR": "0",
	}
	got := string(configurefile.Substitute(tmpl, values, configurefile.Options{}))
	want := "#define USE_FOO 1\n#define USE_BAR 0\n"
	if got != want {
		t.Errorf("#cmakedefine01\nwant:\n%s\ngot:\n%s", want, got)
	}
}

func TestSubstitute_Cmakedefine_ManyFalsyValues(t *testing.T) {
	cases := map[string]string{
		"":             "/* #undef FOO */",
		"0":            "/* #undef FOO */",
		"OFF":          "/* #undef FOO */",
		"off":          "/* #undef FOO */",
		"No":           "/* #undef FOO */",
		"FALSE":        "/* #undef FOO */",
		"NOTFOUND":     "/* #undef FOO */",
		"FOO-NOTFOUND": "/* #undef FOO */",
		"IGNORE":       "/* #undef FOO */",
		"N":            "/* #undef FOO */",
		"1":            "#define FOO",
		"ON":           "#define FOO",
		"yes":          "#define FOO",
		"true":         "#define FOO",
		"hello":        "#define FOO",
	}
	for value, want := range cases {
		tmpl := []byte("#cmakedefine FOO\n")
		got := strings.TrimRight(string(configurefile.Substitute(tmpl, map[string]string{"FOO": value}, configurefile.Options{})), "\n")
		if got != want {
			t.Errorf("FOO=%q → %q, want %q", value, got, want)
		}
	}
}

func TestSubstitute_PreservesIndent(t *testing.T) {
	tmpl := []byte("    #cmakedefine FOO\n\t#cmakedefine BAR\n")
	values := map[string]string{"FOO": "1"}
	got := string(configurefile.Substitute(tmpl, values, configurefile.Options{}))
	want := "    #define FOO\n\t/* #undef BAR */\n"
	if got != want {
		t.Errorf("indent preservation\nwant:\n%q\ngot:\n%q", want, got)
	}
}

func TestSubstitute_NoTrailingNewlineWhenTemplateLacks(t *testing.T) {
	tmpl := []byte("@FOO@") // no terminating newline
	values := map[string]string{"FOO": "bar"}
	got := string(configurefile.Substitute(tmpl, values, configurefile.Options{}))
	want := "bar"
	if got != want {
		t.Errorf("trailing-newline preservation\nwant:%q\ngot:%q", want, got)
	}
}

func TestSubstitute_EmptyTemplate(t *testing.T) {
	got := configurefile.Substitute(nil, nil, configurefile.Options{})
	if len(got) != 0 {
		t.Errorf("empty template should produce empty output; got %q", got)
	}
}

// TestSubstitute_NoRecursiveExpansion documents the v1 scope
// limitation: values that themselves contain @VAR@ markers do
// not get re-expanded. cmake's real configure_file does
// bounded recursive expansion; we don't (yet). If a fixture
// surfaces this limitation, the substitute function grows a
// re-expand loop with cmake's depth limit (~16).
func TestSubstitute_NoRecursiveExpansion(t *testing.T) {
	tmpl := []byte("@OUTER@\n")
	values := map[string]string{
		"OUTER": "@INNER@",
		"INNER": "real-value",
	}
	got := string(configurefile.Substitute(tmpl, values, configurefile.Options{}))
	want := "@INNER@\n" // single-pass; no re-expansion
	if got != want {
		t.Errorf("recursive expansion (expected NOT to recurse)\nwant:%q\ngot:%q", want, got)
	}
}
