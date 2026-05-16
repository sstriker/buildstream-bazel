//go:build e2e

// toolchain_e2e_test exercises the toolchain.cmake plumbing end
// to end: derive-toolchain emits a toolchain.cmake from a real
// cmake probe, the test runs cmake configure with that file via
// -DCMAKE_TOOLCHAIN_FILE, and asserts the file's cache vars
// survive into the resulting CMakeCache.txt. A sibling test
// runs convert-element-cmake with --toolchain-cmake-file and
// asserts the converter accepts + uses the file without
// erroring.
//
// What this replaces (and why):
//
// This file replaces the historical TestE2E_Toolchain_SkipReducesConfigureTime
// which asserted best-of-N(with) < best-of-N(without) at
// wall-clock scale. That test was noise-bound on a small
// fixture (hello-world's total cmake configure is ~180ms,
// while toolchain.cmake's actual savings on this fixture is
// ~0ms — empirically verified, see commit log) and routinely
// flaked when CI co-tenant noise inverted the order by 1-2ms.
//
// The OPTIMIZATION the toolchain.cmake encodes does still
// matter (worker-pool runs without per-host cmake cache, larger
// multi-language graphs, etc.). What's NOT measurable on the
// hello-world fixture is the wall-clock win itself — so the
// test was measuring a downstream symptom of the contract, not
// the contract. This file tests the contract:
//
//   - toolchain.cmake gets generated and is syntactically loadable
//     by cmake (TestE2E_Toolchain_LoadedByCmake)
//   - the converter wires --toolchain-cmake-file to cmake's
//     -DCMAKE_TOOLCHAIN_FILE end to end without erroring
//     (TestE2E_Toolchain_ConverterAcceptsToolchainFile)
//
// The argv-shape wiring (--toolchain-cmake-file → -DCMAKE_TOOLCHAIN_FILE=...)
// is unit-tested at converter/internal/cmakerun/argv_test.go.
// The toolchain.cmake CONTENT is unit-tested at
// converter/internal/emit/cmaketoolchain/emit_test.go.

package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestE2E_Toolchain_LoadedByCmake asserts the deterministic
// behavioral contract: cmake actually loads the toolchain.cmake
// supplied via -DCMAKE_TOOLCHAIN_FILE, and its FORCE'd cache
// vars survive into the resulting CMakeCache.txt.
//
// Technique: append a uniquely-named CACHE INTERNAL variable to
// the derived toolchain.cmake. cmake serializes every INTERNAL
// cache entry to CMakeCache.txt at the end of configure, so
// sentinel-present in CMakeCache.txt ⇒ cmake loaded the file.
// Sentinel-absent ⇒ the file wasn't loaded, OR cmake stripped
// the FORCE var, OR the configure silently failed. Negative
// control: a parallel configure WITHOUT the toolchain file must
// NOT contain the sentinel, ruling out host environment leaks.
func TestE2E_Toolchain_LoadedByCmake(t *testing.T) {
	deriveBin := lookupDeriveToolchain(t)
	if _, err := exec.LookPath("cmake"); err != nil {
		t.Skipf("cmake not on PATH: %v", err)
	}

	src, err := filepath.Abs("../../testdata/sample-projects/hello-world")
	if err != nil {
		t.Fatal(err)
	}

	tcFile := deriveToolchainCMake(t, deriveBin)

	// Surface a clearer failure than ENOENT-from-append if
	// derive-toolchain ever exits 0 but produces no file. The
	// cmd.Run() inside deriveToolchainCMake catches the common
	// case; this is defense-in-depth for the edge case where
	// derive-toolchain succeeds silently.
	if _, err := os.Stat(tcFile); err != nil {
		t.Fatalf("derive-toolchain produced no toolchain.cmake at %q: %v", tcFile, err)
	}

	// Append a CACHE INTERNAL sentinel unique to this test run.
	// cmake INTERNAL cache entries always serialize to
	// CMakeCache.txt; presence iff cmake loaded the file.
	sentinel := newToolchainSentinel(t)
	sentinelLine := fmt.Sprintf("\nset(CMAKE_TO_BAZEL_TC_SENTINEL %q CACHE INTERNAL \"\")\n", sentinel)
	if err := appendToFile(tcFile, sentinelLine); err != nil {
		t.Fatalf("append sentinel to toolchain.cmake: %v", err)
	}

	// Treatment: cmake configure WITH the toolchain.cmake.
	buildWith := t.TempDir()
	mustRun(t, exec.CommandContext(context.Background(), "cmake",
		"-S", src, "-B", buildWith, "-G", "Ninja",
		"-DCMAKE_TOOLCHAIN_FILE="+tcFile))

	cache, err := os.ReadFile(filepath.Join(buildWith, "CMakeCache.txt"))
	if err != nil {
		t.Fatalf("read with-toolchain CMakeCache.txt: %v", err)
	}
	if !bytes.Contains(cache, []byte(sentinel)) {
		t.Errorf("with-toolchain CMakeCache.txt missing sentinel %q\n"+
			"toolchain.cmake's CACHE INTERNAL var did not survive cmake configure;\n"+
			"either the file was not loaded or cmake stripped the entry", sentinel)
	}

	// Negative control: cmake configure WITHOUT the toolchain
	// file must NOT contain the sentinel.
	buildWithout := t.TempDir()
	mustRun(t, exec.CommandContext(context.Background(), "cmake",
		"-S", src, "-B", buildWithout, "-G", "Ninja"))

	cacheNo, err := os.ReadFile(filepath.Join(buildWithout, "CMakeCache.txt"))
	if err != nil {
		t.Fatalf("read without-toolchain CMakeCache.txt: %v", err)
	}
	if bytes.Contains(cacheNo, []byte(sentinel)) {
		t.Errorf("without-toolchain CMakeCache.txt unexpectedly contains sentinel %q;\n"+
			"sentinel should be unique to this test run and only present when toolchain.cmake is loaded", sentinel)
	}
}

// TestE2E_Toolchain_ConverterAcceptsToolchainFile asserts the
// converter (convert-element-cmake) accepts the
// --toolchain-cmake-file flag end to end — runs cmake, produces
// a valid BUILD.bazel, no errors. Coupled with the unit-level
// argv shape coverage (converter/internal/cmakerun/argv_test.go)
// and the cmake-loads-file coverage above, this gate catches
// converter-side integration bugs (the converter building the
// right argv but failing to invoke cmake with it).
//
// Compared to the historical SkipReducesConfigureTime test:
// same setup (derive-toolchain → toolchain.cmake → run the
// converter) but the assertion is "the run completes
// successfully + produces the expected output" instead of "the
// run completes in less wall-clock time."
func TestE2E_Toolchain_ConverterAcceptsToolchainFile(t *testing.T) {
	conv := lookupConverter(t)
	deriveBin := lookupDeriveToolchain(t)
	if _, err := exec.LookPath("cmake"); err != nil {
		t.Skipf("cmake not on PATH: %v", err)
	}

	src, err := filepath.Abs("../../testdata/sample-projects/hello-world")
	if err != nil {
		t.Fatal(err)
	}
	tcFile := deriveToolchainCMake(t, deriveBin)

	// Run convert-element-cmake with --toolchain-cmake-file. The
	// mustRun helper fails the test on non-zero exit.
	out := t.TempDir()
	mustRun(t, exec.CommandContext(context.Background(), conv,
		"--source-root", src,
		"--out-build", filepath.Join(out, "BUILD.bazel"),
		"--toolchain-cmake-file", tcFile,
	))

	// The converter's primary output landed.
	if _, err := os.Stat(filepath.Join(out, "BUILD.bazel")); err != nil {
		t.Errorf("converter ran but produced no BUILD.bazel: %v", err)
	}
}

// newToolchainSentinel returns a per-test-run unique string for
// use as a sentinel CACHE INTERNAL var name+value. Random bytes
// keep parallel test invocations on the same host from
// colliding sentinels in unrelated cache files.
func newToolchainSentinel(t *testing.T) string {
	t.Helper()
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}
	return "cmake-to-bazel-tc-loaded-" + hex.EncodeToString(buf)
}

// appendToFile writes body to the end of path. Used to mutate
// the derived toolchain.cmake with a per-test sentinel.
func appendToFile(path, body string) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(body)
	return err
}

// deriveToolchainCMake runs cmake against the hello-world sample
// to produce a File API reply, then derive-toolchain against the
// reply to emit toolchain.cmake. Returns the absolute path to
// the file.
//
// Real cmake invocation (not bwrap-sandboxed) — derive-toolchain
// is host-side tooling that runs once per host.
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

// lookupDeriveToolchain returns derive-toolchain from PATH,
// falling back to build/bin/ (repo root is three levels up from
// this dir).
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
