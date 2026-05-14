package regression

import "sort"

// FailureAnalytics aggregates Tier-1 codes across both runs of a Diff.
// Tracks which codes appeared/disappeared/changed counts so operators
// can spot churn even when individual element bucket counts look stable.
type FailureAnalytics struct {
	// BeforeCodes / AfterCodes count Tier-1 occurrences per code in
	// each run. Includes only elements present in that run.
	BeforeCodes map[string]int `json:"before_codes,omitempty"`
	AfterCodes  map[string]int `json:"after_codes,omitempty"`

	// CodesAppeared lists codes present in After but not Before.
	// CodesDisappeared lists the inverse. Both sorted for stable output.
	CodesAppeared    []string `json:"codes_appeared,omitempty"`
	CodesDisappeared []string `json:"codes_disappeared,omitempty"`

	// CodesChurned lists codes whose count differs between runs (and
	// that exist in both). New codes go in CodesAppeared instead.
	CodesChurned []string `json:"codes_churned,omitempty"`
}

// Analyze walks before/after Runs (carried via the Diff) and produces
// per-code aggregations. Pure function over the supplied Runs; doesn't
// touch disk.
func Analyze(before, after *Run) *FailureAnalytics {
	a := &FailureAnalytics{
		BeforeCodes: countCodes(before),
		AfterCodes:  countCodes(after),
	}

	for code := range a.BeforeCodes {
		if _, ok := a.AfterCodes[code]; !ok {
			a.CodesDisappeared = append(a.CodesDisappeared, code)
		}
	}
	for code, n := range a.AfterCodes {
		bn, inBefore := a.BeforeCodes[code]
		switch {
		case !inBefore:
			a.CodesAppeared = append(a.CodesAppeared, code)
		case bn != n:
			a.CodesChurned = append(a.CodesChurned, code)
		}
	}
	sort.Strings(a.CodesAppeared)
	sort.Strings(a.CodesDisappeared)
	sort.Strings(a.CodesChurned)
	return a
}

func countCodes(r *Run) map[string]int {
	out := map[string]int{}
	if r == nil {
		return out
	}
	for _, oc := range r.Outcomes {
		if oc.Failure == nil {
			continue
		}
		out[oc.Failure.Code]++
	}
	return out
}
