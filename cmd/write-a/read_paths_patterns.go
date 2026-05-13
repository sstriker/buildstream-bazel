package main

// Read-paths patterns: per-cmake-element <element>.read-paths.txt
// file (committed alongside the .bst) with glob-style include /
// exclude rules. Replaces the old --read-paths-feedback flow:
//
//   include CMakeLists.txt
//   include cmake/*.cmake
//   include include/**/*.h
//   exclude include/internal/*
//
// Why patterns over feedback:
// - Deterministic: same source → same patterns → same action key.
//   Feedback was non-deterministic across version bumps (a path
//   that wasn't read in run N could become important in run N+1).
// - Survives version bumps: include cmake/*.cmake automatically
//   picks up new entries.
// - Reviewable in PR.
//
// Default when no patterns file exists: every file is real
// (matches the conservative pre-narrowing behaviour). The
// patterns file is an opt-in tightening for elements where the
// action-cache benefit is worth the maintenance burden.

import (
	"os"
	"strings"

	"github.com/sstriker/buildstream-bazel/internal/readpaths"
)

// patternRule and readPathsPatterns are aliases to the shared
// pattern types in internal/readpaths. The shared package
// owns the file-format parser AND the matcher, so cmd/write-a
// (which writes srckey-patterns.txt) and cmd/audit-narrowing
// (which reads it) can never drift on either the syntax or
// the inclusion semantics.
type patternRule = readpaths.Rule
type readPathsPatterns = readpaths.Patterns

// writeNarrowingPatterns writes the resolved per-element pattern
// set to elemPkg/srckey-patterns.txt in read-paths.txt syntax —
// the surface cmd/audit-narrowing reads to compare against the
// action-time read oracle. nil/empty patterns produce an empty
// file (the conservative no-narrow default; audit treats as
// "everything covered").
func writeNarrowingPatterns(elemPkg string, patterns *readPathsPatterns) error {
	return writeFile(elemPkg+"/srckey-patterns.txt", patterns.Format())
}

// withCMakeListsRule prepends an `include CMakeLists.txt` rule to
// patterns so cmake's CMakeLists.txt-always-real special case
// (in applyReadPathsPatterns) shows up as an explicit pattern.
// The audit tool's plain Match() doesn't know about per-kind
// special cases; making the rule explicit at emission time keeps
// the audit kind-agnostic.
//
// Returns patterns unchanged when the input is already nil/empty
// AND no narrowing was applied (a no-narrow element doesn't need
// the special case made explicit because Match returns true for
// every path anyway).
func withCMakeListsRule(patterns *readPathsPatterns) *readPathsPatterns {
	if patterns == nil || len(patterns.Rules) == 0 {
		return patterns
	}
	out := &readPathsPatterns{
		Rules: make([]patternRule, 0, len(patterns.Rules)+1),
	}
	out.Rules = append(out.Rules, patternRule{Include: true, Pattern: "CMakeLists.txt"})
	out.Rules = append(out.Rules, patterns.Rules...)
	return out
}

// loadReadPathsPatterns reads <bstPathWithoutSuffix>.read-paths.txt.
// Returns (nil, nil) when the file is absent. A file that's
// present but empty (no rules, only comments / whitespace)
// produces a Patterns with len(Rules) == 0, which Match treats
// the same as nil — both signal "no narrowing applied;
// applyReadPathsPatterns returns the entire universe as real.
// To narrow to zero you'd need an `exclude **` rule, not an
// empty file.
//
// Parsing delegates to internal/readpaths.Parse so the file
// format is shared with cmd/audit-narrowing without duplicate
// parsers that could drift.
func loadReadPathsPatterns(bstPath string) (*readPathsPatterns, error) {
	patternsPath := strings.TrimSuffix(bstPath, ".bst") + ".read-paths.txt"
	f, err := os.Open(patternsPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	return readpaths.Parse(f, patternsPath)
}

// applyReadPathsPatterns partitions universe (the source-relative
// file paths in the element's source tree) into real vs zero
// according to the rules.
//
// Inclusion semantics delegate to readpaths.Patterns.Match —
// shared with the audit tool. The kind:cmake special case
// (CMakeLists.txt always real) is layered on top here.
func applyReadPathsPatterns(pp *readPathsPatterns, universe []string) (real, zero []string) {
	if pp == nil || len(pp.Rules) == 0 {
		return universe, nil
	}
	for _, p := range universe {
		isReal := pp.Match(p)
		// CMakeLists.txt always real (kind:cmake-specific
		// special case).
		if !isReal && pathBase(p) == "CMakeLists.txt" {
			isReal = true
		}
		if isReal {
			real = append(real, p)
		} else {
			zero = append(zero, p)
		}
	}
	return real, zero
}

// pathBase returns the last `/`-separated segment of p (or p
// itself when no `/` is present).
func pathBase(p string) string {
	for i := len(p) - 1; i >= 0; i-- {
		if p[i] == '/' {
			return p[i+1:]
		}
	}
	return p
}
