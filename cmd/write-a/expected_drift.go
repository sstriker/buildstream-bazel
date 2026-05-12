package main

// Per-element expected-drift sibling: <elem>.expected-drift.txt
// committed alongside the .bst, declares paths the narrowing-
// undercoverage audit may legitimately report (typically .h.in
// templates the configure_file lift refused; see the
// `cmake-codegen-lifted` audit tag for the inverse query).
//
// Loaded by write-a (parallel to <elem>.read-paths.txt),
// staged into project A as `srckey-expected-drift.txt`
// alongside `srckey-patterns.txt`, and consumed by
// `cmd/audit-narrowing --allowlist=<path>` to subtract
// expected entries from the miss list before reporting.
//
// Default when no expected-drift file exists: nil allowlist;
// every miss the oracle reports is real drift. Adding entries
// is a deliberate per-path declaration that survives PR review.

import (
	"os"
	"strings"

	"github.com/sstriker/cmake-to-bazel/internal/readpaths"
)

// loadExpectedDrift reads <bstPathWithoutSuffix>.expected-drift.txt.
// Returns (nil, nil) when the file is absent — the no-allowlist
// default. A file that's present but contains only comments /
// blanks produces an empty Allowlist (functionally equivalent
// to nil for the audit, but write-a still emits the file so
// the project-A shape is uniform).
//
// Parsing delegates to readpaths.ParseAllowlist so the file
// format is shared with cmd/audit-narrowing — no chance of
// the writer and reader drifting on syntax.
func loadExpectedDrift(bstPath string) (*readpaths.Allowlist, error) {
	driftPath := strings.TrimSuffix(bstPath, ".bst") + ".expected-drift.txt"
	f, err := os.Open(driftPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	return readpaths.ParseAllowlist(f, driftPath)
}
