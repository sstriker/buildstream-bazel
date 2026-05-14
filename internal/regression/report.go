package regression

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// Report bundles a Diff with its FailureAnalytics so JSON consumers see
// one document per `orchestrate-diff` run. JSON is the canonical
// schema; text rendering is for terminal triage and is allowed to
// rephrase or summarize.
type Report struct {
	Version          int               `json:"version"`
	Before           runRef            `json:"before"`
	After            runRef            `json:"after"`
	Summary          summary           `json:"summary"`
	Details          map[string]Detail `json:"details,omitempty"`
	FailureAnalytics *FailureAnalytics `json:"failure_analytics,omitempty"`
}

type runRef struct {
	Path string `json:"path"`
}

type summary struct {
	StableCount        int      `json:"stable_count"`
	NewlyFailed        []string `json:"newly_failed,omitempty"`
	NewlyPassed        []string `json:"newly_passed,omitempty"`
	FingerprintDrifted []string `json:"fingerprint_drifted,omitempty"`
	StillFailed        []string `json:"still_failed,omitempty"`
	FailureCodeChanged []string `json:"failure_code_changed,omitempty"`
	AppearedConverted  []string `json:"appeared_converted,omitempty"`
	AppearedFailed     []string `json:"appeared_failed,omitempty"`
	Disappeared        []string `json:"disappeared,omitempty"`
}

// BuildReport composes a JSON-shaped report from a Diff plus
// FailureAnalytics. Either input can be nil (BuildReport(nil, nil)
// returns a minimal version-stamped doc).
func BuildReport(d *Diff, fa *FailureAnalytics) *Report {
	r := &Report{Version: 1}
	if d == nil {
		return r
	}
	r.Before = runRef{Path: d.BeforePath}
	r.After = runRef{Path: d.AfterPath}
	r.Summary = summary{
		StableCount:        d.StableCount,
		NewlyFailed:        d.NewlyFailed,
		NewlyPassed:        d.NewlyPassed,
		FingerprintDrifted: d.FingerprintDrifted,
		StillFailed:        d.StillFailed,
		FailureCodeChanged: d.FailureCodeChanged,
		AppearedConverted:  d.AppearedConverted,
		AppearedFailed:     d.AppearedFailed,
		Disappeared:        d.Disappeared,
	}
	if len(d.Details) > 0 {
		r.Details = d.Details
	}
	if fa != nil {
		r.FailureAnalytics = fa
	}
	return r
}

// WriteJSON pretty-prints the report. Caller decides whether to write
// to stdout, a file, or buffer for tests.
func (r *Report) WriteJSON(w io.Writer) error {
	body, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	_, err = w.Write(append(body, '\n'))
	return err
}

// WriteText renders an operator-friendly summary. Sections are ordered
// by triage priority: regressions first (newly_failed, appeared_failed,
// failure_code_changed), then drift, then good news.
//
// The text format is *not* a stable contract — operators may rely on
// the section headers but parsers should consume JSON instead.
func (r *Report) WriteText(w io.Writer) error {
	pf := func(format string, args ...any) error {
		_, err := fmt.Fprintf(w, format, args...)
		return err
	}
	if err := pf("regression diff\n  before: %s\n  after:  %s\n\n", r.Before.Path, r.After.Path); err != nil {
		return err
	}

	sections := []struct {
		header string
		items  []string
		why    string
	}{
		{"newly failed (regression)", r.Summary.NewlyFailed,
			"converted in `before`, Tier-1 in `after`"},
		{"appeared failed", r.Summary.AppearedFailed,
			"new element that landed broken"},
		{"failure code changed", r.Summary.FailureCodeChanged,
			"failed in both runs but with a different code"},
		{"fingerprint drifted", r.Summary.FingerprintDrifted,
			"converted in both, but output bytes differ"},
		{"still failed (unchanged)", subtract(r.Summary.StillFailed, r.Summary.FailureCodeChanged),
			"failed in both runs, same code"},
		{"newly passed", r.Summary.NewlyPassed,
			"failed in `before`, converted in `after`"},
		{"appeared converted", r.Summary.AppearedConverted,
			"new element that converted cleanly"},
		{"disappeared", r.Summary.Disappeared,
			"present in `before`, absent in `after` (rename or removal)"},
	}
	for _, s := range sections {
		if len(s.items) == 0 {
			continue
		}
		if err := pf("%s (%d) — %s\n", s.header, len(s.items), s.why); err != nil {
			return err
		}
		for _, n := range s.items {
			if err := pf("  - %s\n", n); err != nil {
				return err
			}
		}
		if err := pf("\n"); err != nil {
			return err
		}
	}

	if err := pf("stable: %d converted with identical fingerprint\n", r.Summary.StableCount); err != nil {
		return err
	}

	if r.FailureAnalytics != nil {
		fa := r.FailureAnalytics
		if len(fa.CodesAppeared)+len(fa.CodesDisappeared)+len(fa.CodesChurned) > 0 {
			if err := pf("\nfailure-code churn\n"); err != nil {
				return err
			}
			if len(fa.CodesAppeared) > 0 {
				if err := pf("  appeared:    %s\n", strings.Join(fa.CodesAppeared, ", ")); err != nil {
					return err
				}
			}
			if len(fa.CodesDisappeared) > 0 {
				if err := pf("  disappeared: %s\n", strings.Join(fa.CodesDisappeared, ", ")); err != nil {
					return err
				}
			}
			if len(fa.CodesChurned) > 0 {
				if err := pf("  churned:     %s\n", strings.Join(fa.CodesChurned, ", ")); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func subtract(all, sub []string) []string {
	skip := map[string]bool{}
	for _, s := range sub {
		skip[s] = true
	}
	out := make([]string, 0, len(all))
	for _, s := range all {
		if !skip[s] {
			out = append(out, s)
		}
	}
	return out
}
