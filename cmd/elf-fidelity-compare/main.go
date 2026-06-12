// Package main implements elf-fidelity-compare, a CLI for comparing the
// DYNAMIC-section / ABI surface of an ELF artifact produced by `cmake --build`
// against the one produced by `bazel build` from convert-element-cmake's output.
// It handles both artifact kinds the converter produces — shared libraries
// (.so) and executables (PIE / ET_EXEC).
//
// It is the dynamic-section sibling of cmd/fidelity-compare (which compares
// exported-SYMBOL sets via nm): this tool reads SONAME, DT_NEEDED, symbol
// versioning (.gnu.version_d), and DT_RPATH/DT_RUNPATH via `readelf`, classifies
// each delta benign / impactful per docs/fidelity-deltas.md, and exits 0 when no
// impactful deltas remain after the per-member allowlist. (SONAME and
// .gnu.version_d are library-specific and no-op cleanly on an executable.)
//
// Invocation:
//
//	elf-fidelity-compare \
//	    --cmake-artifact <path-to-libfoo.so or executable> \
//	    --bazel-artifact <path-to-libfoo.so or executable> \
//	    [--allowlist <file>] [--report <json-output>]
//
// Exit codes:
//
//	0   no impactful deltas
//	1   impactful deltas surfaced — bug or new benign category to allowlist
//	64  CLI usage error
//	65  tool-side error (readelf missing, artifact unreadable)
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/sstriker/buildstream-bazel/cmd/elf-fidelity-compare/internal/elfclassifier"
)

const (
	exitOK     = 0
	exitImpact = 1
	exitUsage  = 64
	exitTool   = 65
)

func main() {
	fs := flag.NewFlagSet("elf-fidelity-compare", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	cmakeArtifact := fs.String("cmake-artifact", "", "path to the cmake-built shared object (libfoo.so)")
	bazelArtifact := fs.String("bazel-artifact", "", "path to the Bazel-built shared object (bazel-bin/.../libfoo.so)")
	allowlist := fs.String("allowlist", "", "optional per-member benign-delta allowlist file; one entry per line, '#' starts a comment")
	report := fs.String("report", "", "optional path to write the structured comparison report as JSON")

	if err := fs.Parse(os.Args[1:]); err != nil {
		os.Exit(exitUsage)
	}
	if *cmakeArtifact == "" || *bazelArtifact == "" {
		fmt.Fprintln(os.Stderr, "elf-fidelity-compare: both --cmake-artifact and --bazel-artifact are required")
		fs.Usage()
		os.Exit(exitUsage)
	}

	allowed, err := elfclassifier.LoadAllowlist(*allowlist)
	if err != nil {
		fmt.Fprintf(os.Stderr, "elf-fidelity-compare: load allowlist: %v\n", err)
		os.Exit(exitTool)
	}

	rep, err := elfclassifier.Compare(*cmakeArtifact, *bazelArtifact, allowed)
	if err != nil {
		fmt.Fprintf(os.Stderr, "elf-fidelity-compare: %v\n", err)
		os.Exit(exitTool)
	}

	if *report != "" {
		body, mErr := json.MarshalIndent(rep, "", "  ")
		if mErr != nil {
			fmt.Fprintf(os.Stderr, "elf-fidelity-compare: marshal report: %v\n", mErr)
			os.Exit(exitTool)
		}
		if wErr := os.WriteFile(*report, append(body, '\n'), 0o644); wErr != nil {
			fmt.Fprintf(os.Stderr, "elf-fidelity-compare: write report: %v\n", wErr)
			os.Exit(exitTool)
		}
	}

	fmt.Fprint(os.Stderr, rep.FormatForOperator())
	if rep.HasImpactful() {
		os.Exit(exitImpact)
	}
	os.Exit(exitOK)
}
