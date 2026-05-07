// Package tracenorm — reads.go: extract source-relative read paths
// from a canonicalized trace.
//
// The build-tracer (when invoked with --source-root=PATH) emits
// openat events alongside execve events. CanonicalizeWith /
// CanonicalizeBytesWith filters those events against the same
// SourceRoot, rewriting the path source-relative and stripping the
// volatile fd return value. ExtractReads pulls the resulting source-
// relative read set back out for downstream consumers — primarily
// the audit step that compares the trace oracle against per-kind
// srckey patterns to flag undercoverage drift.
//
// Sibling oracle to converter/internal/ninja's
// ProjectToSourceTree(g.ReconfigureInputs(), ...): the cmake-side
// pulls reads from build.ninja's RERUN_CMAKE deps; the autotools/
// make/etc. side pulls reads from the build-tracer's openat events.

package tracenorm

import (
	"bufio"
	"regexp"
	"sort"
	"strings"
)

// canonicalOpenatRE matches openat lines after CanonicalizeWith
// has rewritten them: pid stripped, path source-relative (slash
// form), `= <fd>` replaced with `= ?`. Only the path is captured;
// flags + suffix are ignored.
//
// Format produced by openatLine:
//
//	openat(AT_FDCWD, "<source-relative path>", O_RDONLY|...) = ?
var canonicalOpenatRE = regexp.MustCompile(`^openat\(AT_FDCWD, "((?:[^"\\]|\\.)*)"`)

// ExtractReads walks a canonicalized trace and returns the set of
// source-relative paths recorded as openat reads. Output is sorted,
// deduplicated, and slash-form. Empty input or a trace without
// openat lines returns nil.
//
// Callers compare the result against per-kind srckey patterns
// (autotoolsSrckeyPatterns, makeSrckeyPatterns, etc.) to flag
// undercoverage drift — paths the oracle says were read at action
// time but the patterns leave name-only.
//
// This consumes a CANONICALIZED trace, not a raw one — the path
// rewriting + filtering is done at canonicalize time so the AC
// digest reflects the same view the audit step sees.
func ExtractReads(canonicalTrace []byte) []string {
	if len(canonicalTrace) == 0 {
		return nil
	}
	seen := map[string]struct{}{}
	scanner := bufio.NewScanner(strings.NewReader(string(canonicalTrace)))
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "openat(") {
			continue
		}
		m := canonicalOpenatRE.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		path := unquoteStrace(m[1])
		if path == "" {
			continue
		}
		seen[path] = struct{}{}
	}
	if len(seen) == 0 {
		return nil
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
