// audit-narrowing diffs a per-element narrowing pattern set against
// one or more action-time read oracles and reports paths the oracle
// flags as read but the patterns leave name-only — the
// undercoverage drift surface that, if uncaught, would let a content
// edit silently cache-hit when it shouldn't.
//
// Two oracle inputs, either or both:
//
//	--cmake-reads=<path>   JSON array of source-relative paths
//	                       produced by convert-element's
//	                       --out-cmake-configure-reads (build.ninja's
//	                       RERUN_CMAKE deps projected onto the source
//	                       tree).
//	--trace=<path>         A canonicalized trace.log (build-tracer
//	                       output, post-canonicalize); openat lines
//	                       are extracted via tracenorm.ExtractReads.
//
// The pattern set comes from --patterns=<path>, a read-paths.txt-
// format file (write-a writes one per element alongside srckey
// artifacts).
//
// Output: --out=<path> receives one source-relative path per line,
// sorted, listing every oracle path the patterns DON'T cover. Empty
// file = clean (all reads accounted for in the cache key). Exit
// status is always 0; the report is the signal. CI gates that want
// hard-fail-on-drift can `[ ! -s undercomplete.txt ]` and fail when
// it isn't empty.
//
// Pattern set is the source of truth for "what's covered"; oracle
// is "what was actually read". Soundness invariant the audit tests:
// for each P in oracle, patterns.Match(P) == true. Misses ⇒
// undercoverage.
//
// Both oracles are themselves incomplete (see package doc on
// internal/tracenorm and converter/internal/ninja); a non-empty
// report is necessary-but-not-sufficient evidence of drift, and an
// empty report is necessary-but-not-sufficient evidence of
// soundness. Treat as a high-signal lower bound.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"

	"github.com/sstriker/cmake-to-bazel/internal/readpaths"
	"github.com/sstriker/cmake-to-bazel/internal/tracenorm"
)

func main() {
	patternsPath := flag.String("patterns", "", "path to a read-paths.txt-format pattern file (the per-element pattern set to test). Required.")
	cmakeReads := flag.String("cmake-reads", "", "path to a JSON array of source-relative paths from convert-element's --out-cmake-configure-reads. Optional.")
	tracePath := flag.String("trace", "", "path to a canonicalized trace.log (build-tracer output post-canonicalize); openat read events are extracted via tracenorm.ExtractReads. Optional.")
	allowlistPath := flag.String("allowlist", "", "path to a srckey-expected-drift.txt-format file (one source-relative path per line, `#` comments). Entries are subtracted from the miss list before the report is written. Optional; absent → no allowlist filtering.")
	outPath := flag.String("out", "", "destination for the undercoverage report (one path per line, sorted). Required.")
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: audit-narrowing --patterns=<file> [--cmake-reads=<file>] [--trace=<file>] [--allowlist=<file>] --out=<file>")
		flag.PrintDefaults()
	}
	flag.Parse()

	if *patternsPath == "" || *outPath == "" {
		flag.Usage()
		os.Exit(2)
	}
	if *cmakeReads == "" && *tracePath == "" {
		fmt.Fprintln(os.Stderr, "audit-narrowing: must supply at least one oracle (--cmake-reads or --trace)")
		os.Exit(2)
	}

	if err := run(*patternsPath, *cmakeReads, *tracePath, *allowlistPath, *outPath); err != nil {
		fmt.Fprintf(os.Stderr, "audit-narrowing: %v\n", err)
		os.Exit(1)
	}
}

func run(patternsPath, cmakeReadsPath, tracePath, allowlistPath, outPath string) error {
	pp, err := loadPatterns(patternsPath)
	if err != nil {
		return fmt.Errorf("load patterns: %w", err)
	}

	allow, err := loadAllowlist(allowlistPath)
	if err != nil {
		return fmt.Errorf("load allowlist: %w", err)
	}

	reads, err := loadOracle(cmakeReadsPath, tracePath)
	if err != nil {
		return err
	}

	miss := []string{}
	for _, p := range reads {
		if pp.Match(p) {
			continue
		}
		if allow.Contains(p) {
			// Operator declared this path expected; the audit
			// stays silent for it. The `cmake-codegen-lifted`
			// inverse-tag query helps reviewers spot which
			// entries should be removed once a future lift
			// covers them.
			continue
		}
		miss = append(miss, p)
	}
	sort.Strings(miss)

	return writeReport(outPath, miss)
}

func loadPatterns(path string) (*readpaths.Patterns, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return readpaths.Parse(f, path)
}

// loadAllowlist reads an expected-drift file. Empty path
// returns a nil Allowlist (Contains always-false; every miss
// becomes real drift). A path that points at a missing file
// returns an error — passing --allowlist=<file> with a typo'd
// path should surface fast, not silently behave as if no
// allowlist were declared.
func loadAllowlist(path string) (*readpaths.Allowlist, error) {
	if path == "" {
		return nil, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return readpaths.ParseAllowlist(f, path)
}

// loadOracle returns the deduped union of the oracle path sets. At
// least one of cmakeReadsPath / tracePath is non-empty (caller
// validated). When both are supplied, the union spreads coverage
// across the two oracle types — a path flagged by either gates
// undercoverage-fail.
func loadOracle(cmakeReadsPath, tracePath string) ([]string, error) {
	seen := map[string]struct{}{}
	if cmakeReadsPath != "" {
		paths, err := loadCmakeReads(cmakeReadsPath)
		if err != nil {
			return nil, fmt.Errorf("load cmake-reads %s: %w", cmakeReadsPath, err)
		}
		for _, p := range paths {
			seen[p] = struct{}{}
		}
	}
	if tracePath != "" {
		paths, err := loadTraceReads(tracePath)
		if err != nil {
			return nil, fmt.Errorf("load trace %s: %w", tracePath, err)
		}
		for _, p := range paths {
			seen[p] = struct{}{}
		}
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out, nil
}

func loadCmakeReads(path string) ([]string, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var paths []string
	if err := json.Unmarshal(body, &paths); err != nil {
		return nil, fmt.Errorf("parse JSON: %w", err)
	}
	return paths, nil
}

func loadTraceReads(path string) ([]string, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return tracenorm.ExtractReads(body), nil
}

func writeReport(path string, miss []string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	w := io.Writer(f)
	for _, p := range miss {
		if _, err := fmt.Fprintln(w, p); err != nil {
			return err
		}
	}
	return nil
}
