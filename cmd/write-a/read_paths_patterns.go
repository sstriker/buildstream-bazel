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
	"bufio"
	"fmt"
	"os"
	"strings"

	"github.com/sstriker/cmake-to-bazel/internal/readpaths"
)

// patternRule and readPathsPatterns are aliases to the shared
// pattern types in internal/readpaths. Aliased rather than
// re-declared so cmd/audit-narrowing and cmd/write-a stay byte-
// equivalent on the matcher (the file format and rule semantics
// are an interop contract between the two binaries via the
// per-element srckey-patterns.txt artifact).
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
// Returns (nil, nil) when the file is absent — that's the default
// "entire tree real" case, distinct from a file present but
// empty (which is technically a narrowing to zero matches).
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

	pp := &readPathsPatterns{}
	scanner := bufio.NewScanner(f)
	lineNum := 0
	for scanner.Scan() {
		lineNum++
		raw := strings.TrimSpace(scanner.Text())
		// Skip blanks + comments.
		if raw == "" || strings.HasPrefix(raw, "#") {
			continue
		}
		fields := strings.Fields(raw)
		if len(fields) < 2 {
			return nil, fmt.Errorf("%s:%d: expected '<include|exclude> <pattern>', got %q", patternsPath, lineNum, raw)
		}
		var include bool
		switch fields[0] {
		case "include":
			include = true
		case "exclude":
			include = false
		default:
			return nil, fmt.Errorf("%s:%d: unknown rule %q (want include or exclude)", patternsPath, lineNum, fields[0])
		}
		// Pattern is everything after the first field (allows
		// patterns containing spaces — unusual but legal).
		pattern := strings.Join(fields[1:], " ")
		pp.Rules = append(pp.Rules, patternRule{Include: include, Pattern: pattern})
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return pp, nil
}

// applyReadPathsPatterns partitions universe (the source-relative
// file paths in the element's source tree) into real vs zero
// according to the rules.
//
// Algorithm:
//
//   - Start with every path "real" iff at least one include rule
//     is present and matches; "zero" otherwise. With no include
//     rules at all, default-to-real (so a patterns file with only
//     exclude lines acts as "include everything except these"
//     rather than "include nothing").
//   - Then apply exclude rules in order: any path matching an
//     exclude rule flips to zero.
//   - CMakeLists.txt files are always real regardless of rules
//     (cmake parses the entry CMakeLists before any user pattern
//     can ever fire; auto-including matches the feedback flow's
//     same auto-include).
func applyReadPathsPatterns(pp *readPathsPatterns, universe []string) (real, zero []string) {
	if pp == nil || len(pp.Rules) == 0 {
		return universe, nil
	}
	hasInclude := false
	for _, r := range pp.Rules {
		if r.Include {
			hasInclude = true
			break
		}
	}
	for _, p := range universe {
		isReal := !hasInclude // default-to-real when only exclude rules
		for _, r := range pp.Rules {
			if !r.Include {
				continue
			}
			if matchPattern(r.Pattern, p) {
				isReal = true
				break
			}
		}
		if isReal {
			for _, r := range pp.Rules {
				if r.Include {
					continue
				}
				if matchPattern(r.Pattern, p) {
					isReal = false
					break
				}
			}
		}
		// CMakeLists.txt always real.
		if !isReal {
			base := p
			for i := len(p) - 1; i >= 0; i-- {
				if p[i] == '/' {
					base = p[i+1:]
					break
				}
			}
			if base == "CMakeLists.txt" {
				isReal = true
			}
		}
		if isReal {
			real = append(real, p)
		} else {
			zero = append(zero, p)
		}
	}
	return real, zero
}

// matchPattern matches a path against a glob pattern with ** support.
//
// Pattern grammar (POSIX-glob superset):
//   - * matches any sequence of characters except /
//   - ** matches any sequence of characters including / (zero or more
//     path components)
//   - ? matches one character except /
//   - all other characters match literally
//
// Implementation: walk the pattern + path together, dispatching on
// special characters. ** is implemented via try-and-backtrack.
func matchPattern(pattern, path string) bool {
	return matchPatternRec(pattern, path)
}

func matchPatternRec(pattern, path string) bool {
	for len(pattern) > 0 {
		// ** — match any number of characters (including /), then
		// recurse on remainder.
		if strings.HasPrefix(pattern, "**") {
			rest := pattern[2:]
			if rest == "" {
				return true // trailing ** matches everything
			}
			// Special-case "**/X": ** can match zero path
			// components (i.e., the **/ collapses to nothing) so
			// "include/**/*.h" matches "include/foo.h" too.
			if strings.HasPrefix(rest, "/") {
				if matchPatternRec(rest[1:], path) {
					return true
				}
			}
			// Try every possible split of path.
			for i := 0; i <= len(path); i++ {
				if matchPatternRec(rest, path[i:]) {
					return true
				}
			}
			return false
		}
		c := pattern[0]
		switch c {
		case '*':
			rest := pattern[1:]
			// * matches any non-/ chars, then recurse on remainder.
			if rest == "" {
				return !strings.Contains(path, "/")
			}
			for i := 0; i <= len(path); i++ {
				if i > 0 && path[i-1] == '/' {
					return false
				}
				if matchPatternRec(rest, path[i:]) {
					return true
				}
			}
			return false
		case '?':
			if len(path) == 0 || path[0] == '/' {
				return false
			}
			pattern = pattern[1:]
			path = path[1:]
		default:
			if len(path) == 0 || path[0] != c {
				return false
			}
			pattern = pattern[1:]
			path = path[1:]
		}
	}
	return len(path) == 0
}
