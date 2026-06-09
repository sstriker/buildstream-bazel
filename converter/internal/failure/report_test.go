package failure

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// captureStderr swaps os.Stderr for a temp file across fn and returns what was
// written, so the framing assertions don't depend on an injectable writer.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "stderr")
	if err != nil {
		t.Fatalf("temp: %v", err)
	}
	orig := os.Stderr
	os.Stderr = f
	defer func() { os.Stderr = orig }()
	fn()
	if err := f.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	b, err := os.ReadFile(f.Name())
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return string(b)
}

func TestReportTier1_Tier1WritesEnvelope(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "nested", "failure.json")
	err := New(ConfigureFailed, "cmake bailed: %d", 7)

	var got bool
	stderr := captureStderr(t, func() {
		got = ReportTier1(err, "convert-element-cmake", out, true)
	})

	if !got {
		t.Fatal("ReportTier1 returned false for a Tier-1 *Error, want true")
	}
	if want := "convert-element-cmake: configure-failed: cmake bailed: 7\n"; stderr != want {
		t.Errorf("stderr = %q, want %q", stderr, want)
	}

	// The on-disk envelope is the orchestrator's contract: {tier,code,message}
	// with a trailing newline. Parent dirs are created as needed.
	body, rerr := os.ReadFile(out)
	if rerr != nil {
		t.Fatalf("envelope not written: %v", rerr)
	}
	if !strings.HasSuffix(string(body), "\n") {
		t.Errorf("envelope missing trailing newline: %q", body)
	}
	var env map[string]any
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("envelope not valid JSON: %v", err)
	}
	if env["tier"] != float64(1) {
		t.Errorf("tier = %v, want 1", env["tier"])
	}
	if env["code"] != "configure-failed" {
		t.Errorf("code = %v, want configure-failed", env["code"])
	}
	if env["message"] != "cmake bailed: 7" {
		t.Errorf("message = %v, want %q", env["message"], "cmake bailed: 7")
	}
}

func TestReportTier1_NonTier1NoEnvelope(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "failure.json")

	var got bool
	stderr := captureStderr(t, func() {
		got = ReportTier1(errors.New("disk on fire"), "prog", out, true)
	})

	if got {
		t.Error("ReportTier1 returned true for a plain error, want false")
	}
	if want := "prog: disk on fire\n"; stderr != want {
		t.Errorf("stderr = %q, want %q", stderr, want)
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Errorf("envelope written for a non-Tier-1 error; want none (stat err: %v)", err)
	}
}

// writeFailure=false (e.g. --probe mode) suppresses the envelope but still
// frames stderr and reports the Tier-1 classification.
func TestReportTier1_SuppressedByWriteFailureFalse(t *testing.T) {
	out := filepath.Join(t.TempDir(), "failure.json")

	var got bool
	captureStderr(t, func() {
		got = ReportTier1(New(UnresolvedInclude, "no such header"), "prog", out, false)
	})

	if !got {
		t.Error("ReportTier1 returned false for a Tier-1 *Error, want true")
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Errorf("envelope written despite writeFailure=false; want none (stat err: %v)", err)
	}
}

// An empty outFailure path suppresses the write even when writeFailure is true.
func TestReportTier1_EmptyOutPathNoWrite(t *testing.T) {
	var got bool
	captureStderr(t, func() {
		got = ReportTier1(New(MissingTraceData, "no trace"), "prog", "", true)
	})
	if !got {
		t.Error("ReportTier1 returned false for a Tier-1 *Error, want true")
	}
	// No path to assert against; the contract is simply "no panic, no write".
}
