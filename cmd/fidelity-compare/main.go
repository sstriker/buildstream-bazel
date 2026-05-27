// Package main implements fidelity-compare, a CLI for comparing
// the artifact produced by `cmake --build` against the artifact
// produced by `bazel build` from convert-element-cmake's output.
//
// The comparison is symbol-set-tier: extract exported and undefined
// symbols via nm, classify each delta as benign / impactful /
// configuration-mismatch per the rules documented in
// docs/known-deltas.md, and exit 0 when no impactful deltas remain
// after applying the per-fixture allowlist.
//
// Why a tool rather than inline shell: the classifier rules (FORTIFY
// _chk patterns, C++ template-instantiation pairing, allowlist
// filtering) are tedious to express in awk and even more tedious to
// maintain as the rules grow. A Go binary with structured input/
// output lets the per-fixture e2e gates stay one-liners.
//
// Invocation:
//
//	fidelity-compare \
//	    --cmake-artifact <path-to-libfoo.a or similar>
//	    --bazel-artifact <path-to-libfoo.a>
//	    [--allowlist <file>]
//	    [--report <json-output>]
//
// Exit codes:
//
//	0   no impactful deltas (campaign claim holds for this fixture)
//	1   impactful deltas surfaced — bug or new benign category to allowlist
//	64  CLI usage error
//	65  tool-side error (nm missing, artifact unreadable)
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/sstriker/buildstream-bazel/cmd/fidelity-compare/internal/classifier"
)

const (
	exitOK     = 0
	exitImpact = 1
	exitUsage  = 64
	exitTool   = 65
)

func main() {
	fs := flag.NewFlagSet("fidelity-compare", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	cmakeArtifact := fs.String("cmake-artifact", "", "path to the cmake-built artifact (typically libfoo.a or libfoo.so)")
	bazelArtifact := fs.String("bazel-artifact", "", "path to the Bazel-built artifact (typically bazel-bin/libfoo.a)")
	allowlist := fs.String("allowlist", "", "optional per-fixture benign-delta allowlist file; one entry per line, '#' starts a comment")
	report := fs.String("report", "", "optional path to write the structured comparison report as JSON")

	if err := fs.Parse(os.Args[1:]); err != nil {
		os.Exit(exitUsage)
	}
	if *cmakeArtifact == "" || *bazelArtifact == "" {
		fmt.Fprintln(os.Stderr, "fidelity-compare: both --cmake-artifact and --bazel-artifact are required")
		fs.Usage()
		os.Exit(exitUsage)
	}

	allowed, err := classifier.LoadAllowlist(*allowlist)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fidelity-compare: load allowlist: %v\n", err)
		os.Exit(exitTool)
	}

	rep, err := classifier.Compare(*cmakeArtifact, *bazelArtifact, allowed)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fidelity-compare: %v\n", err)
		os.Exit(exitTool)
	}

	if *report != "" {
		body, mErr := json.MarshalIndent(rep, "", "  ")
		if mErr != nil {
			fmt.Fprintf(os.Stderr, "fidelity-compare: marshal report: %v\n", mErr)
			os.Exit(exitTool)
		}
		if wErr := os.WriteFile(*report, append(body, '\n'), 0o644); wErr != nil {
			fmt.Fprintf(os.Stderr, "fidelity-compare: write report: %v\n", wErr)
			os.Exit(exitTool)
		}
	}

	fmt.Fprint(os.Stderr, rep.FormatForOperator())
	if rep.HasImpactful() {
		os.Exit(exitImpact)
	}
	os.Exit(exitOK)
}
