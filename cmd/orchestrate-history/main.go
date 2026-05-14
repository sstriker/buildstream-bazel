// orchestrate-history queries a persistent fingerprint history written
// by orchestrate-diff --register. Two query shapes:
//
//	churny --window N    — list elements that moved across the last N
//	                       snapshots. Default N=2 (compare last 2).
//	drift --element NAME — print the named element's history oldest
//	                       first.
//
// Output is text by default (one element per line). --format=json emits
// a structured doc that CI dashboards can ingest.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/sstriker/buildstream-bazel/internal/regression"
)

const (
	exitOK          = 0
	exitUsage       = 64
	exitInfraOrLoad = 1
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "orchestrate-history: subcommand required (churny | drift)")
		os.Exit(exitUsage)
	}
	switch os.Args[1] {
	case "churny":
		os.Exit(churny(os.Args[2:]))
	case "drift":
		os.Exit(drift(os.Args[2:]))
	default:
		fmt.Fprintf(os.Stderr, "orchestrate-history: unknown subcommand %q\n", os.Args[1])
		os.Exit(exitUsage)
	}
}

func churny(args []string) int {
	fs := flag.NewFlagSet("churny", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	registry := fs.String("registry", "", "path to fingerprints.json (required)")
	window := fs.Int("window", 2, "number of trailing snapshots to compare (>=2)")
	format := fs.String("format", "text", "output format: text | json")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if *registry == "" {
		fmt.Fprintln(os.Stderr, "orchestrate-history churny: --registry required")
		return exitUsage
	}
	hist, err := regression.LoadHistory(*registry)
	if err != nil {
		fmt.Fprintf(os.Stderr, "orchestrate-history churny: %v\n", err)
		return exitInfraOrLoad
	}
	names := hist.ChurnyElements(*window)
	switch *format {
	case "json":
		body, _ := json.MarshalIndent(map[string]any{
			"version":         1,
			"window":          *window,
			"snapshots_total": len(hist.Snapshots),
			"churned":         names,
		}, "", "  ")
		fmt.Println(string(body))
	default:
		for _, n := range names {
			fmt.Println(n)
		}
	}
	return exitOK
}

func drift(args []string) int {
	fs := flag.NewFlagSet("drift", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	registry := fs.String("registry", "", "path to fingerprints.json (required)")
	elem := fs.String("element", "", "element name to inspect (required)")
	format := fs.String("format", "text", "output format: text | json")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if *registry == "" || *elem == "" {
		fmt.Fprintln(os.Stderr, "orchestrate-history drift: --registry and --element required")
		return exitUsage
	}
	hist, err := regression.LoadHistory(*registry)
	if err != nil {
		fmt.Fprintf(os.Stderr, "orchestrate-history drift: %v\n", err)
		return exitInfraOrLoad
	}
	rows := hist.DriftFor(*elem)
	switch *format {
	case "json":
		body, _ := json.MarshalIndent(map[string]any{
			"version": 1,
			"element": *elem,
			"history": rows,
		}, "", "  ")
		fmt.Println(string(body))
	default:
		if len(rows) == 0 {
			fmt.Fprintf(os.Stderr, "no history for %s\n", *elem)
			return exitOK
		}
		for i, r := range rows {
			snap := hist.Snapshots[indexOf(hist, *elem, i)]
			state := r.Fingerprint
			if r.Failed {
				state = "FAILED:" + r.Code
			}
			fmt.Printf("%s\t%s\t%s\n", snap.Timestamp, snap.Sig[:12], state)
		}
	}
	return exitOK
}

// indexOf finds the snapshot index where the i-th appearance of elem
// in hist.DriftFor(elem) was sourced from. Linear, but the alternative
// (returning indexed pairs from DriftFor) bloats the API for what is
// only a CLI-formatting concern.
func indexOf(hist *regression.History, elem string, occurrence int) int {
	count := 0
	for i, snap := range hist.Snapshots {
		for _, e := range snap.Elements {
			if e.Name == elem {
				if count == occurrence {
					return i
				}
				count++
				break
			}
		}
	}
	return 0
}
