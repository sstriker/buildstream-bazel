package regression

import "sort"

// Diff is the typed comparison between two Runs (`before` and `after`).
//
// The classifier puts each element into exactly one Category bucket per
// Diff, so callers can iterate one slice without double-counting.
// Element names within each bucket are sorted for stable output.
type Diff struct {
	BeforePath string
	AfterPath  string

	// Stable: converted in both runs with identical fingerprint. The
	// architecturally-claimed common case; we expose it as a count
	// rather than a name list to keep reports legible at project scale.
	StableCount int

	// FingerprintDrifted: converted in both runs but the per-element
	// fingerprint differs. Real change in the converter's output.
	FingerprintDrifted []string

	// NewlyFailed: converted in `before`, Tier-1 failed in `after`.
	// The CI signal operators care most about.
	NewlyFailed []string

	// NewlyPassed: Tier-1 failed in `before`, converted in `after`.
	NewlyPassed []string

	// StillFailed: Tier-1 failed in both runs. Bucket exists so churn
	// queries can distinguish "was already broken" from "broke now".
	StillFailed []string

	// FailureCodeChanged: Tier-1 failed in both runs, but with a
	// different failure code (or different message-prefix in M5+).
	// Subset of StillFailed for triage convenience.
	FailureCodeChanged []string

	// AppearedConverted: not in `before`, converted in `after`. New
	// element in the source tree that immediately succeeded.
	AppearedConverted []string

	// AppearedFailed: not in `before`, Tier-1 failed in `after`.
	AppearedFailed []string

	// Disappeared: present in `before`, absent in `after`. Element
	// removed from the source tree, OR a rename our diff doesn't
	// understand yet (open question 1 in docs/m4-plan.md).
	Disappeared []string

	// Details holds the per-element before/after pair for any element
	// the buckets above mention. Reports include this so operators
	// don't have to grep both runs separately. Stable iteration via
	// sorted Name lists in each bucket.
	Details map[string]Detail
}

// Detail is a per-element before/after pair.
type Detail struct {
	Element string   `json:"element"`
	Before  *Outcome `json:"before,omitempty"`
	After   *Outcome `json:"after,omitempty"`
}

// Compute classifies every element into exactly one bucket and produces
// a Diff. Callers can run analytics over the buckets afterward (see
// analytics.go for failure-code aggregation).
func Compute(before, after *Run) *Diff {
	d := &Diff{
		BeforePath: before.Path,
		AfterPath:  after.Path,
		Details:    map[string]Detail{},
	}

	allNames := unionNames(before, after)
	for _, name := range allNames {
		bOC, hasB := before.Outcomes[name]
		aOC, hasA := after.Outcomes[name]
		var detail Detail
		detail.Element = name
		if hasB {
			b := bOC
			detail.Before = &b
		}
		if hasA {
			a := aOC
			detail.After = &a
		}
		d.classify(name, detail, hasB, hasA, &bOC, &aOC)
	}

	sort.Strings(d.FingerprintDrifted)
	sort.Strings(d.NewlyFailed)
	sort.Strings(d.NewlyPassed)
	sort.Strings(d.StillFailed)
	sort.Strings(d.FailureCodeChanged)
	sort.Strings(d.AppearedConverted)
	sort.Strings(d.AppearedFailed)
	sort.Strings(d.Disappeared)
	return d
}

// classify is the bucket assignment logic. Each element lands in
// exactly one of the headline buckets; FailureCodeChanged is an
// additional sub-tag on StillFailed for triage convenience.
func (d *Diff) classify(name string, detail Detail, hasB, hasA bool, b, a *Outcome) {
	switch {
	case !hasB && hasA:
		// Element appeared this run.
		if a.Failure != nil {
			d.AppearedFailed = append(d.AppearedFailed, name)
		} else {
			d.AppearedConverted = append(d.AppearedConverted, name)
		}
		d.Details[name] = detail
		return
	case hasB && !hasA:
		d.Disappeared = append(d.Disappeared, name)
		d.Details[name] = detail
		return
	}

	// Both runs include the element.
	switch {
	case b.Converted != nil && a.Converted != nil:
		// Stable success or fingerprint drift.
		if b.Converted.Fingerprint == a.Converted.Fingerprint {
			d.StableCount++
			return // not in Details: stable elements are common; keep reports tight
		}
		d.FingerprintDrifted = append(d.FingerprintDrifted, name)
		d.Details[name] = detail
	case b.Converted != nil && a.Failure != nil:
		d.NewlyFailed = append(d.NewlyFailed, name)
		d.Details[name] = detail
	case b.Failure != nil && a.Converted != nil:
		d.NewlyPassed = append(d.NewlyPassed, name)
		d.Details[name] = detail
	case b.Failure != nil && a.Failure != nil:
		d.StillFailed = append(d.StillFailed, name)
		d.Details[name] = detail
		if b.Failure.Code != a.Failure.Code {
			d.FailureCodeChanged = append(d.FailureCodeChanged, name)
		}
	}
}

// HasRegressions reports whether the diff carries operator-actionable
// regressions: elements that newly failed or newly appeared in the
// failed bucket. CI integrations gate on this.
func (d *Diff) HasRegressions() bool {
	return len(d.NewlyFailed) > 0 || len(d.AppearedFailed) > 0
}

// unionNames returns the sorted union of element names across both runs.
func unionNames(before, after *Run) []string {
	seen := map[string]struct{}{}
	for n := range before.Outcomes {
		seen[n] = struct{}{}
	}
	for n := range after.Outcomes {
		seen[n] = struct{}{}
	}
	out := make([]string, 0, len(seen))
	for n := range seen {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}
