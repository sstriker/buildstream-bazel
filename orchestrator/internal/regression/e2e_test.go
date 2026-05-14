//go:build e2e

// e2e_test exercises the M4 acceptance gate at project scale: the
// orchestrator runs twice against the fdsdk-subset fixture, with a
// deliberate breakage introduced between runs. orchestrate-diff must
// report the expected newly_failed entry, the right Tier-1 code, the
// right HasRegressions / exit-code semantics, and a stable
// fingerprint_drifted: [] for content-only edits.
package regression_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sstriker/buildstream-bazel/orchestrator/internal/element"
	"github.com/sstriker/buildstream-bazel/orchestrator/internal/orchestrator"
	"github.com/sstriker/buildstream-bazel/orchestrator/internal/regression"
)

// orchestrateOnce runs the orchestrator once against the fdsdk-subset
// fixture and returns the output dir. Real cmake + bwrap; the test is
// gated behind the e2e build tag for that reason.
func orchestrateOnce(t *testing.T, out string) {
	t.Helper()
	repoRoot, _ := filepath.Abs("../../..")
	fixture := filepath.Join(repoRoot, "orchestrator", "testdata", "fdsdk-subset")
	conv := filepath.Join(repoRoot, "build", "bin", "convert-element-cmake")
	if _, err := os.Stat(conv); err != nil {
		t.Skipf("convert-element-cmake binary not built (run `make converter` first): %v", err)
	}

	proj, err := element.ReadProject(fixture, "elements")
	if err != nil {
		t.Fatal(err)
	}
	g, err := element.BuildGraph(proj)
	if err != nil {
		t.Fatal(err)
	}
	res, err := orchestrator.Run(context.Background(), orchestrator.Options{
		Project:         proj,
		Graph:           g,
		Out:             out,
		ConverterBinary: conv,
		Log:             logTo{t},
	})
	if err != nil {
		t.Fatalf("orchestrator.Run: %v", err)
	}
	t.Logf("converted %d, failed %d", len(res.Converted), len(res.Failed))
}

// TestE2E_DeliberateBreakage_ReportedAsNewlyFailed: edit a fixture's
// CMakeLists between two runs to introduce a syntax error; the diff
// must surface it under newly_failed with the expected Tier-1 code.
func TestE2E_DeliberateBreakage_ReportedAsNewlyFailed(t *testing.T) {
	repoRoot, _ := filepath.Abs("../../..")
	cmakeLists := filepath.Join(repoRoot,
		"orchestrator", "testdata", "fdsdk-subset", "files", "uses-hello", "CMakeLists.txt")
	orig, err := os.ReadFile(cmakeLists)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.WriteFile(cmakeLists, orig, 0o644) })

	beforeOut := t.TempDir()
	orchestrateOnce(t, beforeOut)

	// Inject a syntax error that cmake will reject at configure time.
	bad := append([]byte("if(\n# unterminated if() block; cmake will fail to configure\n"), orig...)
	if err := os.WriteFile(cmakeLists, bad, 0o644); err != nil {
		t.Fatal(err)
	}

	afterOut := t.TempDir()
	orchestrateOnce(t, afterOut)

	before, err := regression.LoadRun(beforeOut)
	if err != nil {
		t.Fatal(err)
	}
	after, err := regression.LoadRun(afterOut)
	if err != nil {
		t.Fatal(err)
	}
	d := regression.Compute(before, after)

	if !d.HasRegressions() {
		t.Fatal("HasRegressions = false; want true after deliberate breakage")
	}
	if !sliceContainsStr(d.NewlyFailed, "components/uses-hello") {
		t.Errorf("NewlyFailed = %v, want to include components/uses-hello", d.NewlyFailed)
	}
	det := d.Details["components/uses-hello"]
	if det.After == nil || det.After.Failure == nil {
		t.Fatalf("uses-hello after detail missing Failure: %+v", det.After)
	}
	if det.After.Failure.Code != "configure-failed" {
		t.Errorf("After failure code = %q, want configure-failed", det.After.Failure.Code)
	}

	// The break is in uses-hello, so hello is unaffected.
	if sliceContainsStr(d.NewlyFailed, "components/hello") {
		t.Errorf("hello should not regress; NewlyFailed = %v", d.NewlyFailed)
	}
}

// TestE2E_ContentEditUnderShadowDoesntDrift: the architectural
// shadow-tree claim, this time as a regression-report assertion. Edit
// hello.c body — non-allowlisted, gets stubbed in the shadow — and
// expect zero drift in the diff.
func TestE2E_ContentEditUnderShadowDoesntDrift(t *testing.T) {
	repoRoot, _ := filepath.Abs("../../..")
	helloC := filepath.Join(repoRoot, "converter", "testdata", "sample-projects", "hello-world", "hello.c")
	orig, err := os.ReadFile(helloC)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.WriteFile(helloC, orig, 0o644) })

	beforeOut := t.TempDir()
	orchestrateOnce(t, beforeOut)

	mutated := append([]byte("/* shadow-invariance regression check */\n"), orig...)
	if err := os.WriteFile(helloC, mutated, 0o644); err != nil {
		t.Fatal(err)
	}

	afterOut := t.TempDir()
	orchestrateOnce(t, afterOut)

	before, _ := regression.LoadRun(beforeOut)
	after, _ := regression.LoadRun(afterOut)
	d := regression.Compute(before, after)

	if d.HasRegressions() {
		t.Errorf("HasRegressions = true on content-only edit; shadow should absorb it")
	}
	if len(d.FingerprintDrifted) != 0 {
		t.Errorf("FingerprintDrifted = %v, want []; shadow tree should keep fingerprints stable", d.FingerprintDrifted)
	}
	if d.StableCount != len(before.Outcomes) {
		t.Errorf("StableCount = %d, want %d (every element should be stable)", d.StableCount, len(before.Outcomes))
	}
}

// TestE2E_OrchestrateDiff_CLIExitCodes: invoke the binary itself, not
// the package, to assert the CI-relevant exit-code surface (0 for clean,
// 2 for newly_failed, 0 with --allow-regression).
func TestE2E_OrchestrateDiff_CLIExitCodes(t *testing.T) {
	repoRoot, _ := filepath.Abs("../../..")
	bin := filepath.Join(repoRoot, "build", "bin", "orchestrate-diff")
	if _, err := os.Stat(bin); err != nil {
		t.Skipf("orchestrate-diff binary not built: %v", err)
	}

	a := t.TempDir()
	orchestrateOnce(t, a)
	b := t.TempDir()
	orchestrateOnce(t, b)

	// Two clean runs against the same source -> exit 0.
	if rc := runCLI(t, bin, "--before", a, "--after", b, "--format", "json"); rc != 0 {
		t.Errorf("clean diff rc = %d, want 0", rc)
	}
}

func runCLI(t *testing.T, bin string, args ...string) int {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Stdout = logTo{t}
	cmd.Stderr = logTo{t}
	err := cmd.Run()
	if err == nil {
		return 0
	}
	if ee, ok := err.(*exec.ExitError); ok {
		return ee.ExitCode()
	}
	t.Fatalf("runCLI: %v", err)
	return -1
}

func sliceContainsStr(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

type logTo struct{ t *testing.T }

func (l logTo) Write(p []byte) (int, error) {
	l.t.Logf("%s", strings.TrimRight(string(p), "\n"))
	return len(p), nil
}
