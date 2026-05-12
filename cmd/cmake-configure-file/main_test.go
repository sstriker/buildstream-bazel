package main

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"

	"github.com/sstriker/cmake-to-bazel/internal/configurefile"
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
	if err := run(valuesPath, tmplPath, "", outPath, configurefile.Options{}); err != nil {
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
	if err := run(valuesPath, tmplPath, "", outPath, configurefile.Options{AtOnly: true}); err != nil {
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
	if err := run(valuesPath, tmplPath, "", outPath, configurefile.Options{}); err != nil {
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
		filepath.Join(tmp, "in.txt"),
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
	if err := run(valuesPath, "", blob, outPath, configurefile.Options{}); err != nil {
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

// TestRun_ContentBase64Malformed asserts the decoder surfaces a
// clear error on a broken blob rather than silently treating it
// as an empty template.
func TestRun_ContentBase64Malformed(t *testing.T) {
	tmp := t.TempDir()
	valuesPath := filepath.Join(tmp, "values.json")
	if err := os.WriteFile(valuesPath, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := run(valuesPath, "", "not!!!valid?base64", filepath.Join(tmp, "out.h"), configurefile.Options{})
	if err == nil {
		t.Fatal("expected error on malformed --content-base64")
	}
}
