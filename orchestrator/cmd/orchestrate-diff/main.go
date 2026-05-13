// orchestrate-diff compares two `orchestrate` output directories and
// emits a regression report — JSON for CI/dashboards, text for terminal
// triage. Exit codes:
//
//	0   no actionable regression
//	2   newly_failed or appeared_failed non-empty (CI gate)
//	64  CLI usage error
//	1   I/O / parse failure
//
// Default --format=text writes to stdout. --format=json emits the
// canonical JSON report. --allow-regression flips the exit code from 2
// to 0 when CI wants to inspect a known-bad diff without failing.
//
// --register <path> appends the AFTER run's snapshot to a persistent
// fingerprint history (creating it if missing). Same content as the
// previous append is deduped; returning to a prior state is appended.
package main

import (
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/sstriker/buildstream-bazel/orchestrator/internal/regression"
)

const (
	exitOK          = 0
	exitRegression  = 2
	exitUsage       = 64
	exitInfraOrLoad = 1
)

func main() {
	fs := flag.NewFlagSet("orchestrate-diff", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)

	var (
		before   = fs.String("before", "", "path to the orchestrator output dir from the earlier run (required)")
		after    = fs.String("after", "", "path to the orchestrator output dir from the later run (required)")
		format   = fs.String("format", "text", "output format: text | json")
		out      = fs.String("out", "", "write the report to this file instead of stdout (optional)")
		allow    = fs.Bool("allow-regression", false, "exit 0 even if newly-failed elements are present (CI escape hatch)")
		register = fs.String("register", "", "path to a persistent fingerprint history; the AFTER run is appended on success (optional)")
	)

	if err := fs.Parse(os.Args[1:]); err != nil {
		os.Exit(exitUsage)
	}
	if *before == "" || *after == "" {
		fmt.Fprintln(os.Stderr, "orchestrate-diff: --before and --after are required")
		fs.Usage()
		os.Exit(exitUsage)
	}
	if *format != "text" && *format != "json" {
		fmt.Fprintf(os.Stderr, "orchestrate-diff: --format must be text or json, got %q\n", *format)
		os.Exit(exitUsage)
	}

	beforeRun, err := regression.LoadRun(*before)
	if err != nil {
		fmt.Fprintf(os.Stderr, "orchestrate-diff: %v\n", err)
		os.Exit(exitInfraOrLoad)
	}
	afterRun, err := regression.LoadRun(*after)
	if err != nil {
		fmt.Fprintf(os.Stderr, "orchestrate-diff: %v\n", err)
		os.Exit(exitInfraOrLoad)
	}

	d := regression.Compute(beforeRun, afterRun)
	fa := regression.Analyze(beforeRun, afterRun)
	rep := regression.BuildReport(d, fa)

	w := os.Stdout
	if *out != "" {
		f, err := os.Create(*out)
		if err != nil {
			fmt.Fprintf(os.Stderr, "orchestrate-diff: open %s: %v\n", *out, err)
			os.Exit(exitInfraOrLoad)
		}
		defer f.Close()
		w = f
	}

	switch *format {
	case "json":
		err = rep.WriteJSON(w)
	case "text":
		err = rep.WriteText(w)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "orchestrate-diff: write report: %v\n", err)
		os.Exit(exitInfraOrLoad)
	}

	if *register != "" {
		hist, err := regression.LoadHistory(*register)
		if err != nil {
			fmt.Fprintf(os.Stderr, "orchestrate-diff: %v\n", err)
			os.Exit(exitInfraOrLoad)
		}
		snap := regression.SnapshotFromRun(afterRun, time.Now().UTC())
		appended := hist.Append(snap)
		if err := hist.Save(*register); err != nil {
			fmt.Fprintf(os.Stderr, "orchestrate-diff: save history: %v\n", err)
			os.Exit(exitInfraOrLoad)
		}
		if appended {
			fmt.Fprintf(os.Stderr, "orchestrate-diff: appended snapshot sig=%s to %s\n",
				snap.Sig[:12], *register)
		} else {
			fmt.Fprintf(os.Stderr, "orchestrate-diff: snapshot sig=%s deduped against latest in %s\n",
				snap.Sig[:12], *register)
		}
	}

	if d.HasRegressions() && !*allow {
		os.Exit(exitRegression)
	}
	os.Exit(exitOK)
}
