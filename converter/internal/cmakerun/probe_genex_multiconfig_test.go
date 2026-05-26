package cmakerun

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestProbeGenex_MultiConfig_LiveCMake exercises probe-genex.cmake
// against a real Ninja Multi-Config build with two configurations
// (Release + ASan). The bug this test pins: pre-fix, the hook
// emitted file(GENERATE OUTPUT "<dir>/file.txt" ...) — one OUTPUT
// path total, but cmake's Multi-Config generator evaluates the
// CONTENT once per config. That's exactly the "Evaluation file to
// be written multiple times with different content" fatal cmake
// surfaces at generation time, which forced PR #229's sanitizer-
// features render gate to pass `--probe-genex=false` as a narrow
// workaround.
//
// Post-fix, the hook's OUTPUT carries `$<CONFIG>` (e.g.
// file.Release.txt + file.ASan.txt) so each config has its own
// OUTPUT path, the file(GENERATE) declarations stop colliding, and
// the read side collapses the per-config values back to a single
// GenexProbe.File when they agree (the common case — a target
// with no per-config postfix produces the same artifact path
// under every config).
//
// The test requires cmake + ninja on PATH and skips cleanly when
// either is absent.
func TestProbeGenex_MultiConfig_LiveCMake(t *testing.T) {
	if _, err := exec.LookPath("cmake"); err != nil {
		t.Skip("cmake not on PATH; skipping probe-genex multi-config live test")
	}
	if _, err := exec.LookPath("ninja"); err != nil {
		t.Skip("ninja not on PATH; multi-config needs the Ninja generator")
	}
	major, minor, _, err := AssertVersion(context.Background())
	if err != nil {
		t.Skipf("cmake below codemodel-v2 floor: %v", err)
	}
	if major < 3 || (major == 3 && minor < 24) {
		t.Skipf("cmake %d.%d below probe-genex floor (3.24+); skipping", major, minor)
	}

	src, err := filepath.Abs("../../testdata/sample-projects/probe-genex-utility")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(src); err != nil {
		t.Fatalf("fixture missing: %v", err)
	}
	hook, err := filepath.Abs("probe-genex.cmake")
	if err != nil {
		t.Fatal(err)
	}

	buildDir := t.TempDir()
	cmd := exec.CommandContext(context.Background(), "cmake",
		"-S", src,
		"-B", buildDir,
		"-G", "Ninja Multi-Config",
		"-DCMAKE_CONFIGURATION_TYPES=Release;ASan",
		"-DCMAKE_C_FLAGS_ASAN=-fsanitize=address -g -O1",
		"-DCMAKE_PROJECT_TOP_LEVEL_INCLUDES="+hook,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("cmake (Multi-Config) failed with probe-genex hook:\n%s\nerror: %v", out, err)
	}

	// Both per-config OUTPUT files should exist for the
	// artifact-producing STATIC_LIBRARY target. This is the
	// regression anchor for the multi-config compose bug —
	// pre-fix, cmake's generation step would have errored before
	// either file landed.
	for _, cfg := range []string{"Release", "ASan"} {
		path := filepath.Join(buildDir, "cmake-to-bazel.genex", "realtarget", "file."+cfg+".txt")
		if _, err := os.Stat(path); err != nil {
			t.Errorf("expected per-config probe file at %q: %v", path, err)
		}
	}

	// Reader should ingest the multi-config probe layout cleanly:
	// no PerConfigMismatchError bubbles to the orchestrator, the
	// per-target Type is captured, and INTERFACE_*/Properties
	// that resolve uniformly across configs land on the matching
	// GenexProbe fields. Under Ninja Multi-Config the on-disk
	// TARGET_FILE / TARGET_FILE_DIR routinely diverge (each
	// config has its own artifact subdir of CMAKE_BINARY_DIR), so
	// the File trio MAY be empty even after a successful read —
	// the contract for the multi-config compose fix is "no fatal,
	// rest of the probe still usable", not "File trio always
	// populated."
	probes, err := ReadGenexProbe(buildDir)
	if err != nil {
		t.Fatalf("ReadGenexProbe should not fail on multi-config; got %v", err)
	}
	var real *GenexProbe
	for i := range probes {
		if probes[i].Name == "realtarget" {
			real = &probes[i]
			break
		}
	}
	if real == nil {
		t.Fatalf("no realtarget probe in %v", probes)
	}
	if real.Type != "STATIC_LIBRARY" {
		t.Errorf("realtarget Type = %q, want STATIC_LIBRARY (config-invariant probe should survive)", real.Type)
	}
}

// TestProbeGenex_MultiConfig_DivergenceDroppedNotFatal covers the
// other half of the multi-config compose contract: when
// probe-genex captures genuinely different per-config values for
// one basename (a target with per-config OUTPUT_NAME,
// CMAKE_<CONFIG>_POSTFIX, or just the routine Ninja Multi-Config
// case where each config's artifacts land in a per-config subdir),
// ReadGenexProbe drops the diverging field silently and returns
// the rest of the probe. The lift's downstream consumers
// (genexeval, applyProbeGenexProperties) treat the missing field
// the same as "probe didn't run" and fall back to (b) / legacy
// rather than aborting the whole conversion.
//
// Synthesizes the on-disk layout directly — no cmake involvement —
// because the cmake-side divergence trigger varies across versions
// and the path under test is the reader, not the cmake-side hook.
// The live-cmake half of the contract is pinned by
// TestProbeGenex_MultiConfig_LiveCMake above (which routinely
// produces file_dir divergence under Ninja Multi-Config).
func TestProbeGenex_MultiConfig_DivergenceDroppedNotFatal(t *testing.T) {
	buildDir := t.TempDir()
	tgtDir := filepath.Join(buildDir, "cmake-to-bazel.genex", "postfixed")
	if err := os.MkdirAll(tgtDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Synthesizes the on-disk layout a CMAKE_DEBUG_POSTFIX="d"
	// target would produce: Release artifact libpostfixed.a vs
	// Debug artifact libpostfixedd.a.
	files := map[string]string{
		"type.txt":              "STATIC_LIBRARY",
		"file.Release.txt":      "/build/Release/libpostfixed.a",
		"file.Debug.txt":        "/build/Debug/libpostfixedd.a",
		"file_name.Release.txt": "libpostfixed.a",
		"file_name.Debug.txt":   "libpostfixedd.a",
		"file_dir.Release.txt":  "/build/Release",
		"file_dir.Debug.txt":    "/build/Debug",
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(tgtDir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	probes, err := ReadGenexProbe(buildDir)
	if err != nil {
		t.Fatalf("ReadGenexProbe: divergence should not be fatal; got %v", err)
	}
	if len(probes) != 1 {
		t.Fatalf("want 1 probe; got %d", len(probes))
	}
	p := probes[0]
	if p.Name != "postfixed" || p.Type != "STATIC_LIBRARY" {
		t.Errorf("probe header lost on divergence: %+v", p)
	}
	if p.File != "" || p.FileDir != "" || p.FileName != "" {
		t.Errorf("diverging fields should drop to empty; got %+v", p)
	}
}
