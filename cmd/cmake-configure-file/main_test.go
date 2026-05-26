package main

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"

	"github.com/sstriker/buildstream-bazel/internal/configurefile"
)

// TestRun_FixtureMatch round-trips the configure-file sample
// project's template through the CLI: write template + values
// JSON to disk, invoke run(), assert output matches cmake's
// rendered config.h byte-for-byte.
func TestRun_FixtureMatch(t *testing.T) {
	tmp := t.TempDir()
	tmplPath := filepath.Join(tmp, "config.h.in")
	if err := os.WriteFile(tmplPath, []byte(`#ifndef CFGLIB_CONFIG_H
#define CFGLIB_CONFIG_H
#define CFGLIB_VERSION_MAJOR @CFGLIB_VERSION_MAJOR@
#define CFGLIB_VERSION_MINOR @CFGLIB_VERSION_MINOR@
#endif
`), 0o644); err != nil {
		t.Fatal(err)
	}
	valuesPath := filepath.Join(tmp, "values.json")
	if err := os.WriteFile(valuesPath,
		[]byte(`{"CFGLIB_VERSION_MAJOR":"1","CFGLIB_VERSION_MINOR":"2"}`),
		0o644); err != nil {
		t.Fatal(err)
	}
	outPath := filepath.Join(tmp, "config.h")
	if err := run(valuesPath, "", "", nil, nil, tmplPath, false, "", outPath, configurefile.Options{}); err != nil {
		t.Fatalf("run: %v", err)
	}
	body, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatal(err)
	}
	want := `#ifndef CFGLIB_CONFIG_H
#define CFGLIB_CONFIG_H
#define CFGLIB_VERSION_MAJOR 1
#define CFGLIB_VERSION_MINOR 2
#endif
`
	if string(body) != want {
		t.Errorf("rendered output mismatch\nwant:\n%s\ngot:\n%s", want, body)
	}
}

func TestRun_AtOnlyFlag(t *testing.T) {
	tmp := t.TempDir()
	tmplPath := filepath.Join(tmp, "in.txt")
	if err := os.WriteFile(tmplPath, []byte("at: @VAR@; dollar: ${VAR}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	valuesPath := filepath.Join(tmp, "values.json")
	if err := os.WriteFile(valuesPath, []byte(`{"VAR":"X"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	outPath := filepath.Join(tmp, "out.txt")
	if err := run(valuesPath, "", "", nil, nil, tmplPath, false, "", outPath, configurefile.Options{AtOnly: true}); err != nil {
		t.Fatalf("run: %v", err)
	}
	body, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatal(err)
	}
	want := "at: X; dollar: ${VAR}\n"
	if string(body) != want {
		t.Errorf("@ONLY mismatch\nwant:%q\ngot:%q", want, body)
	}
}

func TestRun_NullValuesIsEmpty(t *testing.T) {
	tmp := t.TempDir()
	tmplPath := filepath.Join(tmp, "in.txt")
	if err := os.WriteFile(tmplPath, []byte("hello @MISSING@ world\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	valuesPath := filepath.Join(tmp, "values.json")
	if err := os.WriteFile(valuesPath, []byte("null"), 0o644); err != nil {
		t.Fatal(err)
	}
	outPath := filepath.Join(tmp, "out.txt")
	if err := run(valuesPath, "", "", nil, nil, tmplPath, false, "", outPath, configurefile.Options{}); err != nil {
		t.Fatalf("run: %v", err)
	}
	body, _ := os.ReadFile(outPath)
	want := "hello  world\n"
	if string(body) != want {
		t.Errorf("null values\nwant:%q\ngot:%q", want, body)
	}
}

func TestRun_MissingValuesFile(t *testing.T) {
	tmp := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmp, "in.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := run(filepath.Join(tmp, "no-such-values.json"),
		"",
		"",
		nil,
		nil,
		filepath.Join(tmp, "in.txt"),
		false,
		"",
		filepath.Join(tmp, "out.txt"),
		configurefile.Options{})
	if err == nil {
		t.Fatal("expected error on missing values file")
	}
}

// TestRun_ContentBase64Mode covers the file(GENERATE CONTENT ...)
// lift path: the template body arrives inline as a base64 blob
// rather than via a positional <input> file, and substitution
// otherwise behaves the same as the INPUT form.
func TestRun_ContentBase64Mode(t *testing.T) {
	tmp := t.TempDir()
	valuesPath := filepath.Join(tmp, "values.json")
	if err := os.WriteFile(valuesPath, []byte(`{"BANNER":"hi"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	tmpl := "#define BANNER \"@BANNER@\"\n"
	blob := base64.StdEncoding.EncodeToString([]byte(tmpl))
	outPath := filepath.Join(tmp, "out.h")
	if err := run(valuesPath, "", "", nil, nil, "", true, blob, outPath, configurefile.Options{}); err != nil {
		t.Fatalf("run: %v", err)
	}
	body, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatal(err)
	}
	want := "#define BANNER \"hi\"\n"
	if string(body) != want {
		t.Errorf("rendered output mismatch\nwant:%q\ngot:%q", want, body)
	}
}

// TestRun_ContentBase64Empty exercises the
// `file(GENERATE CONTENT "")` shape: the CONTENT keyword is
// present but the body is the empty string. hasContent=true +
// content="" must produce an empty output file, not fall through
// to the no-template branch (which would write whatever zero-
// value bytes Substitute returns for a nil template — still
// empty, but only by coincidence; the explicit branch makes
// the intent unambiguous).
func TestRun_ContentBase64Empty(t *testing.T) {
	tmp := t.TempDir()
	valuesPath := filepath.Join(tmp, "values.json")
	if err := os.WriteFile(valuesPath, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	outPath := filepath.Join(tmp, "out.txt")
	// Empty body: base64 of "" is the empty string.
	if err := run(valuesPath, "", "", nil, nil, "", true, "", outPath, configurefile.Options{}); err != nil {
		t.Fatalf("run: %v", err)
	}
	body, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(body) != 0 {
		t.Errorf("empty CONTENT should produce empty output; got %q", body)
	}
}

// TestRun_RejectsInvariantViolations asserts the run-side
// guard that backs main's CLI gate: a programmatic caller (or
// a future CLI reshuffle) that forgets to set one of inPath /
// hasContent — or sets both — must surface an explicit error
// rather than silently rendering an empty template (which
// produces a deceptively well-formed output file).
func TestRun_RejectsInvariantViolations(t *testing.T) {
	tmp := t.TempDir()
	valuesPath := filepath.Join(tmp, "values.json")
	if err := os.WriteFile(valuesPath, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	outPath := filepath.Join(tmp, "out.txt")

	// Neither set.
	if err := run(valuesPath, "", "", nil, nil, "", false, "", outPath, configurefile.Options{}); err == nil {
		t.Errorf("neither inPath nor hasContent: expected error")
	}
	// Both set.
	tmplPath := filepath.Join(tmp, "in.txt")
	if err := os.WriteFile(tmplPath, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := run(valuesPath, "", "", nil, nil, tmplPath, true, "", outPath, configurefile.Options{}); err == nil {
		t.Errorf("both inPath and hasContent: expected error")
	}
}

// TestRun_ContentBase64Malformed asserts the decoder surfaces a
// clear error on a broken blob rather than silently treating it
// as an empty template.
func TestRun_ContentBase64Malformed(t *testing.T) {
	tmp := t.TempDir()
	valuesPath := filepath.Join(tmp, "values.json")
	if err := os.WriteFile(valuesPath, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := run(valuesPath, "", "", nil, nil, "", true, "not!!!valid?base64", filepath.Join(tmp, "out.h"), configurefile.Options{})
	if err == nil {
		t.Fatal("expected error on malformed --content-base64")
	}
}

// TestRun_GenexValues covers the structured-base64 lift's
// Bazel-time path: a template carrying top-level `$<...>`
// literals is replayed against a captured-at-convert-time
// values dict mapping each literal to its cmake-resolved bytes.
// Mirrors the file(GENERATE) CopyOnly + genex-replay shape the
// lifter produces.
func TestRun_GenexValues(t *testing.T) {
	tmp := t.TempDir()
	tmplPath := filepath.Join(tmp, "tmpl.txt")
	if err := os.WriteFile(tmplPath,
		[]byte("// build: $<CONFIG>\n#define ARCH \"$<PLATFORM_ID:Linux>\"\n"),
		0o644); err != nil {
		t.Fatal(err)
	}
	valuesPath := filepath.Join(tmp, "values.json")
	if err := os.WriteFile(valuesPath, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	genexValuesPath := filepath.Join(tmp, "genex.json")
	if err := os.WriteFile(genexValuesPath,
		[]byte(`{"$<CONFIG>":"Release","$<PLATFORM_ID:Linux>":"1"}`),
		0o644); err != nil {
		t.Fatal(err)
	}
	outPath := filepath.Join(tmp, "out.txt")
	if err := run(valuesPath, genexValuesPath, "", nil, nil, tmplPath, false, "", outPath, configurefile.Options{CopyOnly: true}); err != nil {
		t.Fatalf("run: %v", err)
	}
	body, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatal(err)
	}
	want := "// build: Release\n#define ARCH \"1\"\n"
	if string(body) != want {
		t.Errorf("rendered mismatch\nwant: %q\ngot:  %q", want, body)
	}
}

// TestRun_GenexValues_MissingKey: if the template carries a
// top-level genex with no matching entry in the values dict,
// the tool errors out rather than silently emitting a literal
// `$<...>` in the output (which a Bazel consumer reading the
// generated header would see as a syntax error or worse, a
// silently wrong value). Soundness gate the lifter relies on.
func TestRun_GenexValues_MissingKey(t *testing.T) {
	tmp := t.TempDir()
	tmplPath := filepath.Join(tmp, "tmpl.txt")
	if err := os.WriteFile(tmplPath, []byte("$<CONFIG>\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	valuesPath := filepath.Join(tmp, "values.json")
	if err := os.WriteFile(valuesPath, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	genexValuesPath := filepath.Join(tmp, "genex.json")
	if err := os.WriteFile(genexValuesPath, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	outPath := filepath.Join(tmp, "out.txt")
	err := run(valuesPath, genexValuesPath, "", nil, nil, tmplPath, false, "", outPath, configurefile.Options{CopyOnly: true})
	if err == nil {
		t.Fatal("expected error when genex values dict is missing a literal the template carries")
	}
}

// TestRun_GenexContext covers the (a)-shape lift's Bazel-time
// path: a template carrying `$<...>` is parsed by the Go-side
// genex evaluator and resolved against the captured Context.
// Mirrors the file(GENERATE) CopyOnly + genex-evaluate shape
// the lifter produces.
func TestRun_GenexContext(t *testing.T) {
	tmp := t.TempDir()
	tmplPath := filepath.Join(tmp, "tmpl.txt")
	if err := os.WriteFile(tmplPath,
		[]byte("// build: $<CONFIG>\n#define IS_LINUX $<PLATFORM_ID:Linux>\n#define DEBUG $<IF:$<CONFIG:Debug>,1,0>\n"),
		0o644); err != nil {
		t.Fatal(err)
	}
	valuesPath := filepath.Join(tmp, "values.json")
	if err := os.WriteFile(valuesPath, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	genexContextPath := filepath.Join(tmp, "genex-context.json")
	if err := os.WriteFile(genexContextPath,
		[]byte(`{"config":"Release","platform_id":"Linux","compiler_id":{"C":"GNU","CXX":"GNU"}}`),
		0o644); err != nil {
		t.Fatal(err)
	}
	outPath := filepath.Join(tmp, "out.txt")
	if err := run(valuesPath, "", genexContextPath, nil, nil, tmplPath, false, "", outPath, configurefile.Options{CopyOnly: true}); err != nil {
		t.Fatalf("run: %v", err)
	}
	body, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatal(err)
	}
	want := "// build: Release\n#define IS_LINUX 1\n#define DEBUG 0\n"
	if string(body) != want {
		t.Errorf("rendered mismatch\nwant: %q\ngot:  %q", want, body)
	}
}

// TestRun_GenexContext_TargetProperty covers the (a) lift's
// Bazel-time TARGET_PROPERTY path: the captured Targets dict
// in the JSON sidecar populates genexeval.Context.Targets and
// `$<TARGET_PROPERTY:t,p>` resolves at runtime.
func TestRun_GenexContext_TargetProperty(t *testing.T) {
	tmp := t.TempDir()
	tmplPath := filepath.Join(tmp, "tmpl.txt")
	if err := os.WriteFile(tmplPath,
		[]byte("// fglib is a $<TARGET_PROPERTY:fglib,TYPE>\n// imported? $<TARGET_PROPERTY:fglib,IMPORTED>\n"),
		0o644); err != nil {
		t.Fatal(err)
	}
	valuesPath := filepath.Join(tmp, "values.json")
	if err := os.WriteFile(valuesPath, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	genexContextPath := filepath.Join(tmp, "ctx.json")
	if err := os.WriteFile(genexContextPath,
		[]byte(`{"targets":{"fglib":{"type":"STATIC_LIBRARY","imported":false}}}`),
		0o644); err != nil {
		t.Fatal(err)
	}
	outPath := filepath.Join(tmp, "out.txt")
	if err := run(valuesPath, "", genexContextPath, nil, nil, tmplPath, false, "", outPath, configurefile.Options{CopyOnly: true}); err != nil {
		t.Fatalf("run: %v", err)
	}
	body, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatal(err)
	}
	want := "// fglib is a STATIC_LIBRARY\n// imported? FALSE\n"
	if string(body) != want {
		t.Errorf("rendered mismatch\nwant: %q\ngot:  %q", want, body)
	}
}

// TestRun_GenexContext_TargetFile covers the (a) lift's
// Bazel-time TARGET_FILE path: --target-file=<name>=<path>
// flags populate Context.Targets[name].FileLocation, and
// `$<TARGET_FILE:t>` resolves to those bytes verbatim. The
// loaded Context's FileLocation (typically empty per the
// lifter's marshal convention) is overwritten by the flag.
func TestRun_GenexContext_TargetFile(t *testing.T) {
	tmp := t.TempDir()
	tmplPath := filepath.Join(tmp, "tmpl.txt")
	if err := os.WriteFile(tmplPath,
		[]byte("// foo lives at $<TARGET_FILE:foo>\n// bar lives at $<TARGET_FILE:bar>\n"),
		0o644); err != nil {
		t.Fatal(err)
	}
	valuesPath := filepath.Join(tmp, "values.json")
	if err := os.WriteFile(valuesPath, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Context carries the targets but no FileLocation (matches
	// the lifter's marshal-shape; FileLocation is wire-omitted).
	genexContextPath := filepath.Join(tmp, "ctx.json")
	if err := os.WriteFile(genexContextPath,
		[]byte(`{"targets":{"foo":{"type":"STATIC_LIBRARY"},"bar":{"type":"SHARED_LIBRARY"}}}`),
		0o644); err != nil {
		t.Fatal(err)
	}
	outPath := filepath.Join(tmp, "out.txt")
	// --target-file flags are the load-bearing wire for the
	// Bazel-time resolution.
	targetFiles := map[string]string{
		"foo": "/build/libfoo.a",
		"bar": "bazel-bin/pkg/libbar.so",
	}
	if err := run(valuesPath, "", genexContextPath, targetFiles, nil, tmplPath, false, "", outPath, configurefile.Options{CopyOnly: true}); err != nil {
		t.Fatalf("run: %v", err)
	}
	body, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatal(err)
	}
	want := "// foo lives at /build/libfoo.a\n// bar lives at bazel-bin/pkg/libbar.so\n"
	if string(body) != want {
		t.Errorf("rendered mismatch\nwant: %q\ngot:  %q", want, body)
	}
}

// TestRun_GenexContext_TargetFile_MissingFlagErrors: a template
// with $<TARGET_FILE:foo> but no --target-file=foo=... flag
// surfaces the evaluator's UnsupportedError via the run-level
// "evaluate genex" wrapping. The lifter's contract is to always
// pass --target-file for each TARGET_FILE reference; this test
// pins the failure mode when that contract is violated.
func TestRun_GenexContext_TargetFile_MissingFlagErrors(t *testing.T) {
	tmp := t.TempDir()
	tmplPath := filepath.Join(tmp, "tmpl.txt")
	if err := os.WriteFile(tmplPath, []byte("$<TARGET_FILE:foo>\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	valuesPath := filepath.Join(tmp, "values.json")
	if err := os.WriteFile(valuesPath, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	genexContextPath := filepath.Join(tmp, "ctx.json")
	if err := os.WriteFile(genexContextPath, []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	outPath := filepath.Join(tmp, "out.txt")
	// Pass nil targetFiles → no override happens → eval refuses.
	if err := run(valuesPath, "", genexContextPath, nil, nil, tmplPath, false, "", outPath, configurefile.Options{CopyOnly: true}); err == nil {
		t.Fatal("expected error when --target-file flag missing")
	}
}

// TestRun_GenexContext_TargetObjects covers the (a) lift's
// Bazel-time TARGET_OBJECTS path: --target-objects=<name>=<paths>
// flags populate Context.Targets[name].Objects, and
// `$<TARGET_OBJECTS:t>` resolves to those bytes joined by `;`
// (cmake's native list shape). The wire delimiter is `:` (colon)
// because cmake's native `;` is both list-separator AND statement-
// terminator — picking a different shell-safe character keeps the
// `$(locations :t) | tr ' ' ':'` lifter pipeline clean.
//
// Test fixture: three .o paths joined by `:` get rewritten to
// `;`-joined when the evaluator emits them.
func TestRun_GenexContext_TargetObjects(t *testing.T) {
	tmp := t.TempDir()
	tmplPath := filepath.Join(tmp, "tmpl.txt")
	if err := os.WriteFile(tmplPath,
		[]byte("// objs: $<TARGET_OBJECTS:objlib>\n"),
		0o644); err != nil {
		t.Fatal(err)
	}
	valuesPath := filepath.Join(tmp, "values.json")
	if err := os.WriteFile(valuesPath, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Context carries the target with Type but no Objects (the
	// lifter's marshal-shape — Objects can be populated from the
	// recorded probe but typically the Bazel-time --target-objects
	// is what's load-bearing).
	genexContextPath := filepath.Join(tmp, "ctx.json")
	if err := os.WriteFile(genexContextPath,
		[]byte(`{"targets":{"objlib":{"type":"OBJECT_LIBRARY"}}}`),
		0o644); err != nil {
		t.Fatal(err)
	}
	outPath := filepath.Join(tmp, "out.txt")
	// Wire shape: colon-delimited paths.
	targetObjects := map[string]string{
		"objlib": "/build/CMakeFiles/objlib.dir/a.c.o:/build/CMakeFiles/objlib.dir/b.c.o:/build/CMakeFiles/objlib.dir/c.c.o",
	}
	if err := run(valuesPath, "", genexContextPath, nil, targetObjects, tmplPath, false, "", outPath, configurefile.Options{CopyOnly: true}); err != nil {
		t.Fatalf("run: %v", err)
	}
	body, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatal(err)
	}
	// Output: cmake's native semicolon-joined list shape.
	want := "// objs: /build/CMakeFiles/objlib.dir/a.c.o;/build/CMakeFiles/objlib.dir/b.c.o;/build/CMakeFiles/objlib.dir/c.c.o\n"
	if string(body) != want {
		t.Errorf("rendered mismatch\nwant: %q\ngot:  %q", want, body)
	}
}

// TestRun_GenexContext_TargetObjects_MissingFlagErrors: a template
// with $<TARGET_OBJECTS:foo> but no --target-objects=foo=... flag
// AND no Objects in the loaded Context surfaces the evaluator's
// UnsupportedError via the run-level "evaluate genex" wrapping.
// The lifter's contract is to always pass --target-objects for
// each TARGET_OBJECTS reference; this test pins the failure mode
// when that contract is violated.
func TestRun_GenexContext_TargetObjects_MissingFlagErrors(t *testing.T) {
	tmp := t.TempDir()
	tmplPath := filepath.Join(tmp, "tmpl.txt")
	if err := os.WriteFile(tmplPath, []byte("$<TARGET_OBJECTS:foo>\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	valuesPath := filepath.Join(tmp, "values.json")
	if err := os.WriteFile(valuesPath, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	genexContextPath := filepath.Join(tmp, "ctx.json")
	if err := os.WriteFile(genexContextPath,
		[]byte(`{"targets":{"foo":{"type":"OBJECT_LIBRARY"}}}`),
		0o644); err != nil {
		t.Fatal(err)
	}
	outPath := filepath.Join(tmp, "out.txt")
	// Pass nil targetObjects → no override happens → eval refuses
	// because Objects field is empty.
	if err := run(valuesPath, "", genexContextPath, nil, nil, tmplPath, false, "", outPath, configurefile.Options{CopyOnly: true}); err == nil {
		t.Fatal("expected error when --target-objects flag missing")
	}
}

// TestTargetObjectsFlag_Parsing covers the flag's Set() rejections:
// missing `=`, empty name, empty paths. The lifter never emits any
// of these shapes, but the rejection messages surface lifter bugs
// at the CLI rather than silently passing through.
func TestTargetObjectsFlag_Parsing(t *testing.T) {
	cases := []struct {
		in      string
		wantErr bool
	}{
		{"objlib=/a:/b:/c", false},
		{"objlib=/single", false}, // single path (no colon) is valid.
		{"missingEq", true},
		{"=missingName", true},
		{"emptyPaths=", true},
	}
	for _, c := range cases {
		f := &targetObjectsFlag{}
		err := f.Set(c.in)
		if c.wantErr && err == nil {
			t.Errorf("Set(%q): expected error", c.in)
		}
		if !c.wantErr && err != nil {
			t.Errorf("Set(%q): unexpected error %v", c.in, err)
		}
	}
}

// TestTargetObjectsFlag_Accumulates covers the repeated-flag
// semantics: multiple --target-objects entries land in the map,
// duplicate names overwrite (last-wins, matching targetFileFlag's
// shape). The CLI's flag.Var ABI calls Set() once per occurrence.
func TestTargetObjectsFlag_Accumulates(t *testing.T) {
	f := &targetObjectsFlag{}
	if err := f.Set("a=/x:/y"); err != nil {
		t.Fatal(err)
	}
	if err := f.Set("b=/p"); err != nil {
		t.Fatal(err)
	}
	if err := f.Set("a=/z"); err != nil { // overwrites
		t.Fatal(err)
	}
	if got := f.byName["a"]; got != "/z" {
		t.Errorf("byName[a] = %q; want /z (last-wins)", got)
	}
	if got := f.byName["b"]; got != "/p" {
		t.Errorf("byName[b] = %q; want /p", got)
	}
}

// TestRun_GenexContext_UnsupportedOpErrors: an evaluator op
// that returns UnsupportedError (target-evaluator-dependent
// `$<TARGET_FILE:...>`) bubbles up as "evaluate genex: ..."
// from run. The lifter relies on this propagation to refuse
// the (a) shape and try (b) / legacy next.
func TestRun_GenexContext_UnsupportedOpErrors(t *testing.T) {
	tmp := t.TempDir()
	tmplPath := filepath.Join(tmp, "tmpl.txt")
	if err := os.WriteFile(tmplPath, []byte("$<TARGET_FILE:foo>\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	valuesPath := filepath.Join(tmp, "values.json")
	if err := os.WriteFile(valuesPath, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	genexContextPath := filepath.Join(tmp, "ctx.json")
	if err := os.WriteFile(genexContextPath, []byte(`{"config":"Release"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	outPath := filepath.Join(tmp, "out.txt")
	err := run(valuesPath, "", genexContextPath, nil, nil, tmplPath, false, "", outPath, configurefile.Options{CopyOnly: true})
	if err == nil {
		t.Fatal("expected error on $<TARGET_FILE:...>")
	}
}
