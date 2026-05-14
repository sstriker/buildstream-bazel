package regression_test

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/sstriker/buildstream-bazel/internal/regression"
)

func TestAnalyze_CountsAppearedDisappearedChurned(t *testing.T) {
	a := runFromOutcomes("a",
		failed("x", "configure-failed", ""),
		failed("y", "unsupported-target-type", ""),
		failed("z", "unsupported-target-type", ""),
	)
	b := runFromOutcomes("b",
		failed("x", "configure-failed", ""),        // unchanged code+count
		failed("y", "unsupported-target-type", ""), // count unchanged but...
		failed("w", "fileapi-missing", ""),         // ... new code "fileapi-missing"
	)
	fa := regression.Analyze(a, b)

	// "configure-failed": 1 -> 1 (unchanged)
	// "unsupported-target-type": 2 -> 1 (churn)
	// "fileapi-missing": 0 -> 1 (appeared)
	if !sliceEq(fa.CodesAppeared, []string{"fileapi-missing"}) {
		t.Errorf("CodesAppeared = %v", fa.CodesAppeared)
	}
	if !sliceEq(fa.CodesChurned, []string{"unsupported-target-type"}) {
		t.Errorf("CodesChurned = %v", fa.CodesChurned)
	}
	if len(fa.CodesDisappeared) != 0 {
		t.Errorf("CodesDisappeared = %v, want []", fa.CodesDisappeared)
	}
	if fa.BeforeCodes["configure-failed"] != 1 || fa.AfterCodes["configure-failed"] != 1 {
		t.Errorf("configure-failed counts = %d/%d, want 1/1",
			fa.BeforeCodes["configure-failed"], fa.AfterCodes["configure-failed"])
	}
}

func TestBuildReport_RoundtripsJSON(t *testing.T) {
	a := runFromOutcomes("a",
		conv("stable", "fp1"),
		conv("drift", "fp1"),
		failed("scc", "old", "msg"),
	)
	b := runFromOutcomes("b",
		conv("stable", "fp1"),
		conv("drift", "fp2"),
		failed("scc", "new", "msg"),
		failed("appeared-bad", "configure-failed", "boom"),
	)
	d := regression.Compute(a, b)
	rep := regression.BuildReport(d, regression.Analyze(a, b))

	var buf bytes.Buffer
	if err := rep.WriteJSON(&buf); err != nil {
		t.Fatal(err)
	}
	// Round-trip: unmarshal, marshal, byte-equal output.
	var anyDoc map[string]any
	if err := json.Unmarshal(buf.Bytes(), &anyDoc); err != nil {
		t.Fatal(err)
	}
	if anyDoc["version"].(float64) != 1 {
		t.Errorf("version = %v", anyDoc["version"])
	}

	for _, want := range []string{
		`"newly_failed"`, // implied empty -> omitted
		`"fingerprint_drifted": [`,
		`"appeared_failed": [`,
		`"failure_analytics": {`,
		`"appeared-bad"`,
	} {
		if !strings.Contains(buf.String(), want) {
			// "newly_failed" was a typo bait — the omitempty rule should make it absent.
			if want == `"newly_failed"` {
				continue
			}
			t.Errorf("JSON missing %q\n%s", want, buf.String())
		}
	}
}

func TestWriteText_IncludesPriorityHeaders(t *testing.T) {
	a := runFromOutcomes("a", conv("x", "fp1"))
	b := runFromOutcomes("b", failed("x", "configure-failed", "boom"))
	rep := regression.BuildReport(regression.Compute(a, b), nil)

	var buf bytes.Buffer
	if err := rep.WriteText(&buf); err != nil {
		t.Fatal(err)
	}
	body := buf.String()
	for _, want := range []string{
		"regression diff",
		"newly failed (regression)",
		"  - x",
		"stable: 0 converted",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("text output missing %q:\n%s", want, body)
		}
	}
}
