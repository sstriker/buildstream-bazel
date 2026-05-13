package readpaths_test

import (
	"strings"
	"testing"

	"github.com/sstriker/buildstream-bazel/internal/readpaths"
)

// TestParseAllowlist_HappyPath exercises the common shape: a
// short list of source-relative paths with `#` comments and
// blank lines interspersed.
func TestParseAllowlist_HappyPath(t *testing.T) {
	src := `# Templates that didn't lift (legacy bytes-embedded shape).
src/legacy/foo.h.in
include/bar.h.in

# Trailing-inline comments are stripped.
include/baz.h.in # the dump-vars filtered VAR
`
	a, err := readpaths.ParseAllowlist(strings.NewReader(src), "test")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if a.Len() != 3 {
		t.Fatalf("Len: %d want 3", a.Len())
	}
	for _, want := range []string{"src/legacy/foo.h.in", "include/bar.h.in", "include/baz.h.in"} {
		if !a.Contains(want) {
			t.Errorf("Contains(%q) = false want true", want)
		}
	}
	if a.Contains("not/listed.h") {
		t.Errorf("Contains(not-listed) = true")
	}
}

// TestParseAllowlist_RejectsWhitespaceInPath: source-tree paths
// are slash-separated and whitespace-free; an entry with a
// space (or a tab) is either a typo or a comment shape the
// stripper didn't catch.
func TestParseAllowlist_RejectsWhitespaceInPath(t *testing.T) {
	src := "with space.h\n"
	if _, err := readpaths.ParseAllowlist(strings.NewReader(src), "test"); err == nil {
		t.Errorf("expected error on whitespace-bearing entry")
	}
}

// TestAllowlist_NilSafe asserts the Contains / Len / Format
// methods are no-ops on a nil receiver. Callers can leave the
// allowlist slot zero-valued (no `if a != nil` guard needed at
// every use site).
func TestAllowlist_NilSafe(t *testing.T) {
	var a *readpaths.Allowlist
	if a.Contains("any") {
		t.Errorf("nil.Contains: true want false")
	}
	if a.Len() != 0 {
		t.Errorf("nil.Len: %d want 0", a.Len())
	}
	if got := a.Format(); got != nil {
		t.Errorf("nil.Format: %q want nil", got)
	}
}

// TestAllowlist_RoundTrip asserts that Format → ParseAllowlist
// preserves the entry set. Important because write-a's stager
// writes the formatted bytes and the audit's allowlist consumer
// re-parses them; any drift would cause silent false positives.
func TestAllowlist_RoundTrip(t *testing.T) {
	a, err := readpaths.ParseAllowlist(strings.NewReader("c.h\na.h\nb/x.h\n"), "test")
	if err != nil {
		t.Fatal(err)
	}
	formatted := string(a.Format())
	// Format sorts lexically — a < b/x < c.
	want := "a.h\nb/x.h\nc.h\n"
	if formatted != want {
		t.Errorf("Format mismatch\nwant:%q\ngot: %q", want, formatted)
	}
	b, err := readpaths.ParseAllowlist(strings.NewReader(formatted), "round-trip")
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"a.h", "b/x.h", "c.h"} {
		if !b.Contains(p) {
			t.Errorf("round-trip lost %q", p)
		}
	}
}

// TestAllowlist_AuditMissRoundTrip captures the design promise
// that audit-narrowing's report format and the allowlist format
// are identical: an operator who wants to silence a new entry
// can append the audit's stdout to the allowlist file
// verbatim. This test asserts that contract by running the
// shape both directions.
func TestAllowlist_AuditMissRoundTrip(t *testing.T) {
	// What audit-narrowing writes: one path per line, sorted.
	auditReport := "src/foo.h.in\nsrc/legacy/bar.h.in\n"
	a, err := readpaths.ParseAllowlist(strings.NewReader(auditReport), "audit")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !a.Contains("src/foo.h.in") || !a.Contains("src/legacy/bar.h.in") {
		t.Errorf("audit-format paths not loaded: %+v", a)
	}
}
