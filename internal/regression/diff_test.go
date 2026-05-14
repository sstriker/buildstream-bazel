package regression_test

import (
	"testing"

	"github.com/sstriker/buildstream-bazel/internal/regression"
)

// helper that builds a Run inline. The diff layer doesn't care about the
// disk format so we don't need to round-trip through stageRun for these
// tests.
func runFromOutcomes(name string, outcomes ...regression.Outcome) *regression.Run {
	r := &regression.Run{
		Path:     "/synthetic/" + name,
		Outcomes: map[string]regression.Outcome{},
	}
	for _, o := range outcomes {
		r.Outcomes[o.Element] = o
	}
	return r
}

func conv(name, fp string) regression.Outcome {
	return regression.Outcome{
		Element:   name,
		Converted: &regression.ConvertedOutcome{Fingerprint: fp},
	}
}

func failed(name, code, msg string) regression.Outcome {
	return regression.Outcome{
		Element: name,
		Failure: &regression.FailureOutcome{Tier: 1, Code: code, Message: msg},
	}
}

func TestCompute_StableElementsCountedNotListed(t *testing.T) {
	a := runFromOutcomes("a", conv("x", "fp1"), conv("y", "fp2"))
	b := runFromOutcomes("b", conv("x", "fp1"), conv("y", "fp2"))
	d := regression.Compute(a, b)
	if d.StableCount != 2 {
		t.Errorf("StableCount = %d, want 2", d.StableCount)
	}
	if len(d.FingerprintDrifted) != 0 {
		t.Errorf("FingerprintDrifted = %v, want []", d.FingerprintDrifted)
	}
	if len(d.Details) != 0 {
		t.Errorf("Details = %v, want empty (stable elements not detailed)", d.Details)
	}
	if d.HasRegressions() {
		t.Errorf("HasRegressions = true on all-stable diff")
	}
}

func TestCompute_FingerprintDriftFlagged(t *testing.T) {
	a := runFromOutcomes("a", conv("x", "fp1"))
	b := runFromOutcomes("b", conv("x", "fp2"))
	d := regression.Compute(a, b)
	if len(d.FingerprintDrifted) != 1 || d.FingerprintDrifted[0] != "x" {
		t.Errorf("FingerprintDrifted = %v", d.FingerprintDrifted)
	}
	det, ok := d.Details["x"]
	if !ok {
		t.Fatal("x not in Details")
	}
	if det.Before.Converted.Fingerprint != "fp1" || det.After.Converted.Fingerprint != "fp2" {
		t.Errorf("Detail before/after = %+v / %+v", det.Before.Converted, det.After.Converted)
	}
	// Drift alone isn't a regression at the CI gate level — that's by
	// design (see HasRegressions).
	if d.HasRegressions() {
		t.Errorf("HasRegressions = true on fingerprint drift only")
	}
}

func TestCompute_NewlyFailedIsRegression(t *testing.T) {
	a := runFromOutcomes("a", conv("x", "fp1"))
	b := runFromOutcomes("b", failed("x", "configure-failed", "boom"))
	d := regression.Compute(a, b)
	if len(d.NewlyFailed) != 1 || d.NewlyFailed[0] != "x" {
		t.Errorf("NewlyFailed = %v", d.NewlyFailed)
	}
	if !d.HasRegressions() {
		t.Errorf("HasRegressions = false on newly-failed; want true")
	}
}

func TestCompute_NewlyPassedRecorded(t *testing.T) {
	a := runFromOutcomes("a", failed("x", "configure-failed", "boom"))
	b := runFromOutcomes("b", conv("x", "fp1"))
	d := regression.Compute(a, b)
	if len(d.NewlyPassed) != 1 || d.NewlyPassed[0] != "x" {
		t.Errorf("NewlyPassed = %v", d.NewlyPassed)
	}
	if d.HasRegressions() {
		t.Errorf("HasRegressions = true on newly-passed only")
	}
}

func TestCompute_StillFailedSplitsByCodeChange(t *testing.T) {
	a := runFromOutcomes("a",
		failed("x", "configure-failed", "boom"),
		failed("y", "unsupported-target-type", "UTILITY"),
	)
	b := runFromOutcomes("b",
		failed("x", "configure-failed", "different message but same code"),
		failed("y", "unsupported-custom-command", "now a different code"),
	)
	d := regression.Compute(a, b)
	if !sliceEq(d.StillFailed, []string{"x", "y"}) {
		t.Errorf("StillFailed = %v", d.StillFailed)
	}
	if !sliceEq(d.FailureCodeChanged, []string{"y"}) {
		t.Errorf("FailureCodeChanged = %v, want [y]", d.FailureCodeChanged)
	}
}

func TestCompute_AppearAndDisappear(t *testing.T) {
	a := runFromOutcomes("a", conv("gone", "fp1"))
	b := runFromOutcomes("b",
		conv("new-good", "fp2"),
		failed("new-bad", "configure-failed", "boom"),
	)
	d := regression.Compute(a, b)
	if !sliceEq(d.Disappeared, []string{"gone"}) {
		t.Errorf("Disappeared = %v", d.Disappeared)
	}
	if !sliceEq(d.AppearedConverted, []string{"new-good"}) {
		t.Errorf("AppearedConverted = %v", d.AppearedConverted)
	}
	if !sliceEq(d.AppearedFailed, []string{"new-bad"}) {
		t.Errorf("AppearedFailed = %v", d.AppearedFailed)
	}
	// Newly-appeared-failed counts as a regression: a new element
	// landing broken should show up in the CI signal.
	if !d.HasRegressions() {
		t.Errorf("HasRegressions = false on newly-appeared-failed; want true")
	}
}

func TestCompute_BucketsAreMutuallyExclusive(t *testing.T) {
	// Every per-element appearance lands in exactly one of the headline
	// buckets so callers can iterate one slice without double-counting.
	// FailureCodeChanged is the only intentional sub-tag.
	a := runFromOutcomes("a",
		conv("stable", "fp1"),
		conv("drift", "fp1"),
		conv("nf", "fp1"),
		failed("np", "x", "msg"),
		failed("sf", "x", "msg"),
		failed("scc", "old", "msg"),
		conv("gone", "fp1"),
	)
	b := runFromOutcomes("b",
		conv("stable", "fp1"),
		conv("drift", "fp2"),
		failed("nf", "configure-failed", "msg"),
		conv("np", "fp1"),
		failed("sf", "x", "msg"),
		failed("scc", "new", "msg"),
		conv("appeared-good", "fp1"),
		failed("appeared-bad", "x", "msg"),
	)
	d := regression.Compute(a, b)

	allNames := func() []string {
		var out []string
		out = append(out, d.FingerprintDrifted...)
		out = append(out, d.NewlyFailed...)
		out = append(out, d.NewlyPassed...)
		out = append(out, d.StillFailed...)
		out = append(out, d.AppearedConverted...)
		out = append(out, d.AppearedFailed...)
		out = append(out, d.Disappeared...)
		return out
	}()
	seen := map[string]int{}
	for _, n := range allNames {
		seen[n]++
	}
	for n, count := range seen {
		if count != 1 {
			t.Errorf("%s appears in %d headline buckets; should be exactly 1", n, count)
		}
	}
	// And every non-stable element from either run must show up in some
	// headline bucket.
	expected := []string{"drift", "nf", "np", "sf", "scc", "appeared-good", "appeared-bad", "gone"}
	for _, n := range expected {
		if seen[n] != 1 {
			t.Errorf("%s missing from headline buckets", n)
		}
	}
	if d.StableCount != 1 {
		t.Errorf("StableCount = %d, want 1 (only \"stable\")", d.StableCount)
	}
}
