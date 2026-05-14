//go:build e2e

// toolchain_skip_e2e_test exercises the configure-skip optimization:
// run convert-element-cmake twice against the same source, once
// without --toolchain-cmake-file and once with derive-toolchain's
// output, and assert the with-file run's cmake-configure time is
// shorter.
//
// Why this is the right gate: the toolchain.cmake's only purpose is
// to skip cmake's compiler-detection probe, a measurable fraction of
// every configure. If with-file isn't faster than without, either we
// generated the wrong file or wired -DCMAKE_TOOLCHAIN_FILE wrong;
// either way the test fires.
//
// Re-homed from orchestrator/internal/orchestrator/ as part of the
// orchestrator absorption (docs/design/orchestrator-absorption.md).
// The orchestrator version summed Result.Timings.TotalCMakeConfigureSecs
// across a multi-element run — but that number is just the sum of
// what each convert-element-cmake invocation already reports in its
// own --out-timings JSON (cmake_configure_seconds). Calling the
// converter directly drops the orchestrator middleman and measures
// the exact same thing per-element.
//
// Conservative assertion: best-of-N(with) < best-of-N(without). The
// configure of one small fixture is easily inverted by co-tenant
// noise on shared CI runners; best-of-N takes the lowest measurement
// from each pass — closer to the uninterfered configure time, which
// is what the optimization actually affects. Passes are interleaved
// so monotonic runner-load drift biases both equally.
//
// Gated behind the `e2e` build tag; needs real cmake +
// convert-element-cmake + derive-toolchain. CI runs it via
// `make e2e-toolchain-skip`. Shares lookupConverter / mustRun /
// testLog with fidelity_e2e_test.go (same package).
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// toolchainSkipRuns is the number of measurements per pass. Best-of-N
// over N=3 reduces noisy-neighbour flakiness substantially; bump if
// flakes recur.
const toolchainSkipRuns = 3

func TestE2E_Toolchain_SkipReducesConfigureTime(t *testing.T) {
	conv := lookupConverter(t)
	deriveBin := lookupDeriveToolchain(t)
	if _, err := exec.LookPath("cmake"); err != nil {
		t.Skipf("cmake not on PATH: %v", err)
	}

	src, err := filepath.Abs("../../testdata/sample-projects/hello-world")
	if err != nil {
		t.Fatal(err)
	}

	// Derive toolchain.cmake once, up front — it's deterministic from
	// the host's cmake; deriving inside the measurement loop would
	// just add noise.
	tcFile := deriveToolchainCMake(t, deriveBin)

	// Interleave the passes (A, B, A, B, …) so monotonic drift in
	// runner load biases both equally instead of penalising whichever
	// ran second.
	var bestA, bestB float64
	for i := 0; i < toolchainSkipRuns; i++ {
		a := convertConfigureSecs(t, conv, src, "")
		if i == 0 || a < bestA {
			bestA = a
		}
		b := convertConfigureSecs(t, conv, src, tcFile)
		if i == 0 || b < bestB {
			bestB = b
		}
		t.Logf("run %d/%d: configure without=%.3fs with=%.3fs", i+1, toolchainSkipRuns, a, b)
	}

	if bestB >= bestA {
		t.Errorf("toolchain.cmake did not reduce configure time across %d runs:\n"+
			"  best-of-%d without: %.3fs\n"+
			"  best-of-%d with:    %.3fs",
			toolchainSkipRuns, toolchainSkipRuns, bestA, toolchainSkipRuns, bestB)
	}
	improvement := bestA - bestB
	t.Logf("toolchain.cmake configure-time win (best-of-%d): %.3fs absolute, %.1f%% relative",
		toolchainSkipRuns, improvement, 100.0*improvement/bestA)
}

// convertConfigureSecs runs convert-element-cmake against src and
// returns the cmake_configure_seconds it reports in --out-timings.
// When tcFile is non-empty it's passed via --toolchain-cmake-file.
func convertConfigureSecs(t *testing.T, conv, src, tcFile string) float64 {
	t.Helper()
	out := t.TempDir()
	timingsPath := filepath.Join(out, "timings.json")
	args := []string{
		"--source-root", src,
		"--out-build", filepath.Join(out, "BUILD.bazel"),
		"--out-timings", timingsPath,
	}
	if tcFile != "" {
		args = append(args, "--toolchain-cmake-file", tcFile)
	}
	mustRun(t, exec.CommandContext(context.Background(), conv, args...))

	body, err := os.ReadFile(timingsPath)
	if err != nil {
		t.Fatalf("read timings: %v", err)
	}
	var tm struct {
		CMakeConfigureSecs float64 `json:"cmake_configure_seconds"`
	}
	if err := json.Unmarshal(body, &tm); err != nil {
		t.Fatalf("parse timings %s: %v", timingsPath, err)
	}
	if tm.CMakeConfigureSecs <= 0 {
		t.Fatalf("converter reported non-positive cmake_configure_seconds (%.3f) — "+
			"did it take the offline --reply-dir path?", tm.CMakeConfigureSecs)
	}
	return tm.CMakeConfigureSecs
}

// deriveToolchainCMake runs cmake against the hello-world sample to
// produce a File API reply, then derive-toolchain against the reply
// to emit toolchain.cmake. Returns the absolute path to the file.
//
// Real cmake invocation (not bwrap-sandboxed) — derive-toolchain is
// host-side tooling that runs once per host.
func deriveToolchainCMake(t *testing.T, deriveBin string) string {
	t.Helper()
	hostSrc, err := filepath.Abs("../../testdata/sample-projects/hello-world")
	if err != nil {
		t.Fatal(err)
	}
	build := t.TempDir()
	// Stage File API queries so the reply carries toolchains-v1 +
	// cache-v2 (what derive-toolchain needs).
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

// lookupDeriveToolchain returns derive-toolchain from PATH, falling
// back to build/bin/ (repo root is three levels up from this dir).
func lookupDeriveToolchain(t *testing.T) string {
	t.Helper()
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
