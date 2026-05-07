//go:build e2e

// e2e_toolchain_skip exercises the configure-skip optimization end-
// to-end: run the orchestrator twice against the fdsdk-subset, once
// without --toolchain-cmake-file and once with derive-toolchain's
// output, and assert the second pass's cumulative cmake_configure
// time is shorter than the first.
//
// Why this is the right gate: the toolchain.cmake's only purpose is
// to skip cmake's compiler-detection probe, which is a measurable
// fraction of every per-element configure. If with-file isn't faster
// than without, either we generated the wrong file or we wired
// -DCMAKE_TOOLCHAIN_FILE wrong; either way the test fires.
//
// Conservative assertion: best-of-N(B) < best-of-N(A). The fixture
// is small (3 cmake elements; cumulative configure ~few seconds)
// so single-shot wall-clock is easily inverted by co-tenant noise on
// shared CI runners. Best-of-N takes the lowest measurement from each
// pass — closer to the platonic uninterfered configure time, which
// is what the optimization actually affects. The per-pass ratio is
// logged for operator visibility but not asserted as a number.
package orchestrator_test

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/sstriker/cmake-to-bazel/orchestrator/internal/orchestrator"
)

// toolchainSkipRuns is the number of measurements taken per pass.
// Best-of-N over N=3 reduces noisy-neighbor flakiness from O(20%)
// per-shot to O(1%) for the comparison; bumping to 5 helps further
// at proportionally more runtime. Keep at 3 unless flakes recur.
const toolchainSkipRuns = 3

func TestE2E_Toolchain_SkipReducesConfigureTime(t *testing.T) {
	conv := lookupConverter(t)
	deriveBin := lookupDeriveToolchain(t)

	// Derive the toolchain.cmake once, up front. It's deterministic
	// from the host's cmake; running it inside the measurement loop
	// would just add noise.
	tcFile := deriveToolchainCMake(t, deriveBin)

	// Interleave the two passes (A, B, A, B, ...) rather than running
	// all of one then all of the other. Interleaving cancels any
	// monotonic drift in runner load over the test's wall-clock
	// window — if the runner gets steadily busier, the bias hits both
	// passes equally instead of penalising whichever ran second.
	var bestA, bestB float64
	for i := 0; i < toolchainSkipRuns; i++ {
		outA := t.TempDir()
		resA := runOrchestratorWithoutToolchain(t, outA, conv)
		logTimings(t, fmt.Sprintf("pass A (no toolchain.cmake) run %d/%d", i+1, toolchainSkipRuns), resA)
		if i == 0 || resA.Timings.TotalCMakeConfigureSecs < bestA {
			bestA = resA.Timings.TotalCMakeConfigureSecs
		}

		outB := t.TempDir()
		resB := runOrchestratorWithToolchain(t, outB, conv, tcFile)
		logTimings(t, fmt.Sprintf("pass B (with toolchain.cmake) run %d/%d", i+1, toolchainSkipRuns), resB)
		if i == 0 || resB.Timings.TotalCMakeConfigureSecs < bestB {
			bestB = resB.Timings.TotalCMakeConfigureSecs
		}
	}

	if bestB >= bestA {
		t.Errorf("toolchain.cmake did not reduce configure time across %d runs:\n"+
			"  best-of-%d pass A (without): %.2fs\n"+
			"  best-of-%d pass B (with):    %.2fs",
			toolchainSkipRuns, toolchainSkipRuns, bestA, toolchainSkipRuns, bestB)
	}

	improvement := bestA - bestB
	pct := 100.0 * improvement / bestA
	t.Logf("toolchain.cmake configure-time win (best-of-%d): %.2fs absolute, %.1f%% relative",
		toolchainSkipRuns, improvement, pct)
}

func runOrchestratorWithoutToolchain(t *testing.T, out, conv string) *orchestrator.Result {
	proj, g := mustLoadFixture(t)
	res, err := orchestrator.Run(context.Background(), orchestrator.Options{
		Project:         proj,
		Graph:           g,
		Out:             out,
		ConverterBinary: conv,
		Concurrency:     1, // serial keeps timing assertions clean
		Log:             testLog{t},
	})
	if err != nil {
		t.Fatalf("orchestrator (no toolchain): %v", err)
	}
	if len(res.Failed) != 0 {
		t.Fatalf("Failed = %v, want []", res.Failed)
	}
	return res
}

func runOrchestratorWithToolchain(t *testing.T, out, conv, tcFile string) *orchestrator.Result {
	proj, g := mustLoadFixture(t)
	res, err := orchestrator.Run(context.Background(), orchestrator.Options{
		Project:            proj,
		Graph:              g,
		Out:                out,
		ConverterBinary:    conv,
		ToolchainCMakeFile: tcFile,
		Concurrency:        1,
		Log:                testLog{t},
	})
	if err != nil {
		t.Fatalf("orchestrator (with toolchain): %v", err)
	}
	if len(res.Failed) != 0 {
		t.Fatalf("Failed = %v, want []", res.Failed)
	}
	return res
}

// deriveToolchainCMake runs cmake against the converter's hello-world
// sample to produce a reply, then derive-toolchain against the reply
// to emit toolchain.cmake. Returns the absolute path to the file.
//
// Real cmake invocation (not bwrap-sandboxed) — derive-toolchain is
// host-side tooling that runs once per host, separate from the
// orchestrator's hermetic pipeline.
func deriveToolchainCMake(t *testing.T, deriveBin string) string {
	hostSrc, err := filepath.Abs("../../../converter/testdata/sample-projects/hello-world")
	if err != nil {
		t.Fatal(err)
	}
	build := t.TempDir()
	// Stage File API queries so the reply contains toolchains-v1 +
	// cache-v2 (what FromReply needs).
	queryDir := filepath.Join(build, ".cmake", "api", "v1", "query")
	if err := os.MkdirAll(queryDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{"codemodel-v2", "toolchains-v1", "cmakeFiles-v1", "cache-v2"} {
		if err := os.WriteFile(filepath.Join(queryDir, q), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command("cmake", "-S", hostSrc, "-B", build, "-G", "Ninja")
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	if err := cmd.Run(); err != nil {
		t.Fatalf("cmake configure for derive-toolchain probe: %v\n%s", err, buf.String())
	}

	// derive-toolchain --reply-dir <build>/.cmake/api/v1/reply --out <tmp>
	tcOut := t.TempDir()
	reply := filepath.Join(build, ".cmake", "api", "v1", "reply")
	cmd = exec.Command(deriveBin, "--reply-dir", reply, "--out", tcOut)
	cmd.Stdout = testLog{t}
	cmd.Stderr = testLog{t}
	if err := cmd.Run(); err != nil {
		t.Fatalf("derive-toolchain: %v", err)
	}
	return filepath.Join(tcOut, "toolchain.cmake")
}

func lookupConverter(t *testing.T) string {
	if p, err := exec.LookPath("convert-element"); err == nil {
		return p
	}
	repoRoot, _ := filepath.Abs("../../..")
	fallback := filepath.Join(repoRoot, "build", "bin", "convert-element")
	if _, err := os.Stat(fallback); err == nil {
		return fallback
	}
	t.Skip("convert-element not on PATH and not in build/bin/")
	return ""
}

func lookupDeriveToolchain(t *testing.T) string {
	if p, err := exec.LookPath("derive-toolchain"); err == nil {
		return p
	}
	repoRoot, _ := filepath.Abs("../../..")
	fallback := filepath.Join(repoRoot, "build", "bin", "derive-toolchain")
	if _, err := os.Stat(fallback); err == nil {
		return fallback
	}
	t.Skip("derive-toolchain not on PATH and not in build/bin/ — run `make derive-toolchain` first")
	return ""
}

func logTimings(t *testing.T, label string, res *orchestrator.Result) {
	t.Helper()
	t.Logf("%s: cmake=%.2fs translate=%.2fs total=%.2fs ratio=%.2f",
		label,
		res.Timings.TotalCMakeConfigureSecs,
		res.Timings.TotalTranslationSecs,
		res.Timings.TotalConverterSecs,
		res.Timings.ConfigureToTranslationRatio,
	)
	_ = fmt.Sprintf // keep fmt imported in case future log lines need it
}
