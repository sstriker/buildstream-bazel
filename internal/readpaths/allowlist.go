package readpaths

// Allowlist: per-element "expected drift" file declaring paths
// the narrowing-undercoverage audit may legitimately report.
//
// Lives next to srckey-patterns.txt in the same syntax family
// — `#` comments + blank lines are ignored, every other line
// is one exact-match source-relative path. No glob grammar:
// each entry is a deliberate per-path declaration that says
// "yes, this file is read at action time AND yes, that's
// expected behavior for v1." The exact-match shape keeps the
// allowlist reviewable in PR (a glob could mask unrelated
// drift the operator didn't intend to silence).
//
// Typical entries are templates that the configure_file lift
// refuses (Substitute hasn't modeled some option, or the
// values dump filtered a referenced variable). The audit
// flags these correctly; the operator adds the path here as a
// deliberate "yes, this is the legacy bytes-embedded shape,
// the .h.in content stays in srckey." The
// `cmake-codegen-lifted` tag is the inverse audit query:
// genrules WITHOUT that tag are the elements whose .h.in
// templates legitimately live in the allowlist.
//
// Round-trip with the audit's report: an audit miss line and
// an allowlist line have identical syntax, so silencing a new
// drift entry is `cat audit-report.txt >> <bst-dir>/<elem>.expected-drift.txt`
// — the source-of-truth file lives next to the element's
// `<elem>.bst` and `<elem>.read-paths.txt` and gets committed
// alongside them. write-a stages the file as
// `srckey-expected-drift.txt` in project A's per-element
// directory (a build artifact, not edited directly);
// scripts/audit-narrowing-walk.sh writes the per-element
// `audit-report.txt` containing the unsuppressed drift the
// operator should triage. Workflow: edit the source-of-truth
// `.expected-drift.txt` in the .bst directory, not the
// staged copy in project A — modulo manual review of which
// paths to actually accept.
//
// nil/empty allowlist signals "no expected drift declared".

import (
	"bufio"
	"fmt"
	"io"
	"sort"
	"strings"
)

// Allowlist is the parsed expected-drift file.
type Allowlist struct {
	paths map[string]struct{}
}

// NewAllowlist returns an empty allowlist. Nil receivers on
// Contains / Len / Format work too — both are safe to call on
// a nil pointer — so most callers can leave the slot zero-
// valued when no allowlist is declared.
func NewAllowlist() *Allowlist {
	return &Allowlist{paths: map[string]struct{}{}}
}

// ParseAllowlist reads an expected-drift-format stream. label
// is used in error messages (typically the source path or
// "<inline>").
//
// Parsing rules:
//   - Lines whose first non-whitespace char is `#` are
//     full-line comments and are dropped.
//   - Blank / whitespace-only lines are dropped.
//   - Every other line is one exact-match source-relative path.
//   - Leading + trailing whitespace is trimmed (so an indented
//     entry stays equal to the audit's report); whitespace
//     INSIDE the path is an error — the audit's reports are
//     slash-separated and whitespace-free, and an internally-
//     spaced entry would never match anything the oracle
//     reports.
//   - Trailing inline comments are recognized in the specific
//     form ` #` (space-then-hash); the text from there to
//     end-of-line is dropped. A `#` without a preceding space
//     stays part of the path (so a path like
//     `weird#dir/foo.h` is parsed as-is).
func ParseAllowlist(r io.Reader, label string) (*Allowlist, error) {
	a := NewAllowlist()
	scanner := bufio.NewScanner(r)
	lineNum := 0
	for scanner.Scan() {
		lineNum++
		raw := strings.TrimSpace(scanner.Text())
		if raw == "" || strings.HasPrefix(raw, "#") {
			continue
		}
		// Strip trailing inline comments: `path/foo.h # known unliftable`.
		if i := strings.Index(raw, " #"); i >= 0 {
			raw = strings.TrimSpace(raw[:i])
			if raw == "" {
				continue
			}
		}
		if strings.ContainsAny(raw, " \t") {
			// Path component containing whitespace would either
			// be a typo or a malicious filename; the source-tree
			// paths the audit emits are slash-separated and
			// whitespace-free, so flag this as malformed rather
			// than silently storing something the audit will
			// never match.
			return nil, fmt.Errorf("%s:%d: whitespace in allowlist entry %q (expected one source-relative path per line)", label, lineNum, raw)
		}
		a.paths[raw] = struct{}{}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("%s: %w", label, err)
	}
	return a, nil
}

// Contains reports whether p is on the allowlist. nil receivers
// always return false (the no-allowlist default — every audit
// miss is real drift).
func (a *Allowlist) Contains(p string) bool {
	if a == nil || a.paths == nil {
		return false
	}
	_, ok := a.paths[p]
	return ok
}

// Len returns the number of allowlisted paths. nil receivers
// return 0.
func (a *Allowlist) Len() int {
	if a == nil {
		return 0
	}
	return len(a.paths)
}

// Format serializes the allowlist to its canonical on-disk
// representation: one path per line, sorted lexically, every
// line (including the last) terminated by `\n`. The trailing
// newline keeps the format compatible with text-tool conventions
// — `cat`, `grep`, `wc -l`, and editors treat the last line as
// terminated. nil receivers and empty allowlists both return a
// nil slice (`len() == 0`, distinct from an explicit `[]byte{}`
// but interchangeable with it for io.Writer / string()
// purposes — `string(nil) == ""`). write-a emits the file
// unconditionally for shape predictability via
// `string(allowlist.Format())`, and an empty file round-trips
// through ParseAllowlist to an empty Allowlist.
func (a *Allowlist) Format() []byte {
	if a == nil || len(a.paths) == 0 {
		return nil
	}
	keys := make([]string, 0, len(a.paths))
	for k := range a.paths {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(k)
		b.WriteByte('\n')
	}
	return []byte(b.String())
}
