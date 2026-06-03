package toolchainscan

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestParseDeclared_Forms(t *testing.T) {
	const bzl = `
load("@bazel_tools//tools/cpp:cc_toolchain_config_lib.bzl", "feature", "flag_set")

def _impl(ctx):
    return [
        feature(name = "opt", enabled = True),     # keyword name (the norm)
        feature("dbg"),                            # positional name
        feature(name = "asan", flag_sets = [x]),   # keyword among others
        feature(name = "san_" + ctx.attr.mode),    # computed → NOT enumerable
        flag_set(actions = ["c-compile"]),         # not feature() → ignored
    ]
`
	dir := t.TempDir()
	path := filepath.Join(dir, "cc_toolchain_config.bzl")
	if err := os.WriteFile(path, []byte(bzl), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := ParseDeclared(path)
	if err != nil {
		t.Fatalf("ParseDeclared: %v", err)
	}
	// Sorted union of LITERAL feature names; the computed one is absent.
	want := []string{"asan", "dbg", "opt"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ParseDeclared = %v, want %v", got, want)
	}
}

func TestParseDeclared_DirUnionsBzlFiles(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("a.bzl", `def _a(ctx): return [feature(name = "lto")]`)
	write("b.bzl", `def _b(ctx): return [feature(name = "pic"), feature(name = "lto")]`)
	write("ignore.txt", `feature(name = "not_bzl")`) // not a .bzl → skipped

	got, err := ParseDeclared(dir)
	if err != nil {
		t.Fatalf("ParseDeclared(dir): %v", err)
	}
	want := []string{"lto", "pic"} // union, deduped, sorted; "not_bzl" excluded
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ParseDeclared(dir) = %v, want %v", got, want)
	}
}

func TestParseDeclared_NoFeaturesIsEmptyNotError(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "x.bzl"), []byte(`def f(): pass`), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := ParseDeclared(dir)
	if err != nil {
		t.Fatalf("ParseDeclared: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected empty, got %v", got)
	}
}

func TestParseDeclared_MissingPathErrors(t *testing.T) {
	if _, err := ParseDeclared(filepath.Join(t.TempDir(), "nope")); err == nil {
		t.Error("expected error for missing path")
	}
}

// TestParseDeclared_PositionalAndKeywordEdges locks featureName's rule: a
// positional name is valid only as the first argument (trailing keywords are
// fine); a call with no name (keyword-only, no name=) yields nothing.
func TestParseDeclared_PositionalAndKeywordEdges(t *testing.T) {
	const bzl = `
def _impl(ctx):
    return [
        feature("rel", enabled = True),    # positional first + trailing kw → "rel"
        feature(enabled = True),           # no name at all → skipped
        feature(name = "x", enabled = True),
    ]
`
	dir := t.TempDir()
	path := filepath.Join(dir, "c.bzl")
	if err := os.WriteFile(path, []byte(bzl), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := ParseDeclared(path)
	if err != nil {
		t.Fatalf("ParseDeclared: %v", err)
	}
	want := []string{"rel", "x"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ParseDeclared = %v, want %v", got, want)
	}
}
