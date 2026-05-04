package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSrckey_AutotoolsContentNarrowing covers the property the
// 2-phase srckey design hinges on: editing a content-irrelevant
// file (.c) leaves srckey unchanged; editing a content-relevant
// file (Makefile.in / .h) changes it; adding/removing any file
// changes it regardless of patterns.
func TestSrckey_AutotoolsContentNarrowing(t *testing.T) {
	// Stage a minimal autotools tree.
	tree := t.TempDir()
	mustWrite(t, filepath.Join(tree, "configure"), "#!/bin/sh\necho configure\n")
	mustWrite(t, filepath.Join(tree, "Makefile.in"), "all: foo\nfoo: foo.o\n\tcc -o foo foo.o\n")
	mustWrite(t, filepath.Join(tree, "foo.c"), "int main(void){return 0;}\n")
	mustWrite(t, filepath.Join(tree, "foo.h"), "extern int x;\n")

	elem := &element{
		Name: "demo",
		Sources: []resolvedSource{
			{Kind: "local", AbsPath: tree},
		},
	}

	patterns := autotoolsSrckeyPatterns()

	baseline, baselineBreakdown, err := computeSrckey(elem, patterns)
	if err != nil {
		t.Fatal(err)
	}
	if baseline == "" {
		t.Fatal("baseline srckey empty")
	}

	// foo.c content edit (added comment) — should NOT change srckey.
	mustWrite(t, filepath.Join(tree, "foo.c"), "/* comment */\nint main(void){return 0;}\n")
	after, _, err := computeSrckey(elem, patterns)
	if err != nil {
		t.Fatal(err)
	}
	if after != baseline {
		t.Errorf("srckey changed after .c edit; expected stable\nbaseline: %s\nafter:    %s\n--breakdown--\n%s", baseline, after, baselineBreakdown)
	}

	// foo.h content edit — SHOULD change srckey.
	mustWrite(t, filepath.Join(tree, "foo.h"), "extern int x;\nextern int y;\n")
	afterH, _, err := computeSrckey(elem, patterns)
	if err != nil {
		t.Fatal(err)
	}
	if afterH == baseline {
		t.Errorf(".h edit should have invalidated srckey; both = %s", baseline)
	}

	// Restore foo.h, then add a new .c file — SHOULD change srckey
	// (file list grew; a Makefile.in wildcard might pick it up).
	mustWrite(t, filepath.Join(tree, "foo.h"), "extern int x;\n")
	mustWrite(t, filepath.Join(tree, "bar.c"), "int bar(void){return 1;}\n")
	afterAdd, _, err := computeSrckey(elem, patterns)
	if err != nil {
		t.Fatal(err)
	}
	if afterAdd == baseline {
		t.Errorf("adding bar.c should have invalidated srckey; both = %s", baseline)
	}

	// Remove the added file — should return to baseline (assuming
	// foo.c was also restored to its baseline content).
	mustWrite(t, filepath.Join(tree, "foo.c"), "int main(void){return 0;}\n")
	if err := os.Remove(filepath.Join(tree, "bar.c")); err != nil {
		t.Fatal(err)
	}
	final, _, err := computeSrckey(elem, patterns)
	if err != nil {
		t.Fatal(err)
	}
	if final != baseline {
		t.Errorf("srckey didn't return to baseline after restore; want %s got %s", baseline, final)
	}
}

// TestSrckey_BreakdownIsCanonical covers that the breakdown
// drives the hash deterministically — same tree => same
// breakdown => same hash, regardless of filesystem readdir
// order.
func TestSrckey_BreakdownIsCanonical(t *testing.T) {
	tree := t.TempDir()
	// Create files in non-alphabetical order; readdir order is
	// filesystem-dependent. The breakdown's sort by path should
	// give a canonical ordering.
	mustWrite(t, filepath.Join(tree, "z.h"), "z\n")
	mustWrite(t, filepath.Join(tree, "a.h"), "a\n")
	mustWrite(t, filepath.Join(tree, "m.h"), "m\n")

	elem := &element{Name: "x", Sources: []resolvedSource{{Kind: "local", AbsPath: tree}}}
	hash, breakdown, err := computeSrckey(elem, autotoolsSrckeyPatterns())
	if err != nil {
		t.Fatal(err)
	}

	// Verify entries appear sorted.
	lines := strings.Split(strings.TrimSpace(breakdown), "\n")
	if len(lines) != 3 {
		t.Fatalf("expected 3 entries in breakdown, got %d:\n%s", len(lines), breakdown)
	}
	if !strings.HasPrefix(lines[0], "a.h\t") || !strings.HasPrefix(lines[1], "m.h\t") || !strings.HasPrefix(lines[2], "z.h\t") {
		t.Errorf("breakdown not sorted alphabetically:\n%s", breakdown)
	}
	if hash == "" {
		t.Error("hash empty")
	}

	// Recompute — must match.
	hash2, _, err := computeSrckey(elem, autotoolsSrckeyPatterns())
	if err != nil {
		t.Fatal(err)
	}
	if hash != hash2 {
		t.Errorf("recompute differed: %s vs %s", hash, hash2)
	}
}

// TestSrckey_NoPatternsHashesContent guards the conservative
// default: nil patterns means every file's content contributes.
// Comment-only edits in any file invalidate srckey when there's
// no narrowing.
func TestSrckey_NoPatternsHashesContent(t *testing.T) {
	tree := t.TempDir()
	mustWrite(t, filepath.Join(tree, "src.c"), "int x = 1;\n")

	elem := &element{Name: "x", Sources: []resolvedSource{{Kind: "local", AbsPath: tree}}}
	before, _, err := computeSrckey(elem, nil)
	if err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(tree, "src.c"), "/* comment */\nint x = 1;\n")
	after, _, err := computeSrckey(elem, nil)
	if err != nil {
		t.Fatal(err)
	}
	if before == after {
		t.Errorf("nil patterns should hash content; .c edit didn't invalidate srckey")
	}
}

// mustWrite is a test helper — fails the test on write error.
// Strict-mode mkdir ensures the parent dir exists.
func mustWrite(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
}
