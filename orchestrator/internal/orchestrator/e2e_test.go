//go:build e2e

// e2e_test runs the orchestrator against the real convert-element-cmake binary
// and the fdsdk-subset fixture. Both kind:cmake elements (hello,
// uses-hello) should convert cleanly under bwrap.
//
// Gated behind the `e2e` build tag because it depends on cmake + bwrap
// being installed; CI's e2e job invokes it as part of the M3a acceptance
// suite.
package orchestrator_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sstriker/buildstream-bazel/orchestrator/internal/orchestrator"
)

func TestE2E_Orchestrate_StubSubset(t *testing.T) {
	conv, err := exec.LookPath("convert-element-cmake")
	if err != nil {
		// CI builds the binary into build/bin/ via the Makefile; fall back
		// to that location so `make e2e-orchestrate` works without the
		// binary being on $PATH.
		repoRoot, _ := filepath.Abs("../../..")
		fallback := filepath.Join(repoRoot, "build", "bin", "convert-element-cmake")
		if _, ferr := os.Stat(fallback); ferr == nil {
			conv = fallback
		} else {
			t.Skipf("convert-element-cmake not found (PATH=%v fallback=%v)", err, ferr)
		}
	}

	proj, g := mustLoadFixture(t)
	out := t.TempDir()

	res, err := orchestrator.Run(context.Background(), orchestrator.Options{
		Project:         proj,
		Graph:           g,
		Out:             out,
		ConverterBinary: conv,
		Log:             testLog{t},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	want := []string{"components/hello", "components/uses-hello"}
	if !sliceEqual(res.Converted, want) {
		t.Errorf("Converted = %v, want %v", res.Converted, want)
	}
	if len(res.Failed) != 0 {
		t.Errorf("Failed = %v, want []", res.Failed)
	}

	// Per-element artifacts that real convert-element-cmake produces.
	// `cmake-config/` is in synth-prefix layout (lib/cmake/<Pkg>/...) —
	// see internal/synthprefix.BuildSlice + the converter's
	// --out-bundle-dir flow.
	for _, want := range []string{
		"elements/components/hello/BUILD.bazel",
		"elements/components/hello/cmake-config/lib/cmake/hello/helloConfig.cmake",
		"elements/components/hello/cmake-config/lib/cmake/hello/helloTargets.cmake",
		"elements/components/hello/cmake-config/lib/cmake/hello/helloTargets-release.cmake",
		"elements/components/uses-hello/BUILD.bazel",
		"elements/components/uses-hello/cmake-config/lib/cmake/uses_hello/uses_helloConfig.cmake",
	} {
		if _, err := os.Stat(filepath.Join(out, want)); err != nil {
			t.Errorf("missing %s: %v", want, err)
		}
	}

	// Diagnostics on failure: dump the actual `out` tree + the
	// per-element converter outputs the orchestrator should have
	// staged. The test fails consistently in CI but not locally; the
	// extra context surfaces what the runner actually sees so we can
	// narrow the gap without pinging tmate / scrolling 600-line
	// transcripts. No-op on a green run because t.Failed() is false
	// then.
	if t.Failed() {
		dumpTree(t, out, "out")
	}

	helloBuild := mustReadFile(t, filepath.Join(out, "elements", "components", "hello", "BUILD.bazel"))
	if !strings.Contains(string(helloBuild), `name = "hello"`) {
		t.Errorf("hello BUILD.bazel doesn't declare hello target: %s", helloBuild)
	}

	// Architectural acceptance: uses_hello_bin's deps must include both
	// the in-element dep (:uses_hello) and the cross-element label
	// (//elements/components/hello). The latter only ends up in the
	// codemodel's link.commandFragments as an absolute /opt/prefix path,
	// resolved via the imports manifest's link_paths field.
	// Phase 3's buildtools canonicalizer shortens `//pkg:pkg` to
	// `//pkg` when target name matches package basename.
	usesBuild := mustReadFile(t, filepath.Join(out, "elements", "components", "uses-hello", "BUILD.bazel"))
	for _, want := range []string{
		`":uses_hello"`,
		`"//elements/components/hello"`,
	} {
		if !strings.Contains(string(usesBuild), want) {
			t.Errorf("uses-hello BUILD.bazel missing %s\n%s", want, usesBuild)
		}
	}
}

// dumpTree walks rootDir and prints every entry's relative path,
// type, mode, and size. Called from t.Failed() blocks so a CI
// transcript carries the actual filesystem state for diagnosis
// (the failure is consistent in CI but not locally; we want CI
// to show us what it sees rather than guessing). No-op when the
// directory doesn't exist.
func dumpTree(t *testing.T, rootDir, label string) {
	t.Helper()
	t.Logf("--- dumpTree(%s) at %s ---", label, rootDir)
	if _, err := os.Stat(rootDir); err != nil {
		t.Logf("  (root not present: %v)", err)
		return
	}
	_ = filepath.WalkDir(rootDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			t.Logf("  walk-err %s: %v", path, err)
			return nil
		}
		rel, _ := filepath.Rel(rootDir, path)
		if rel == "." {
			rel = ""
		}
		info, ierr := d.Info()
		if ierr != nil {
			t.Logf("  %s [stat-err: %v]", rel, ierr)
			return nil
		}
		kind := "F"
		if d.IsDir() {
			kind = "D"
		}
		if info.Mode()&os.ModeSymlink != 0 {
			kind = "L"
		}
		t.Logf("  %s %04o %8d %s", kind, info.Mode().Perm(), info.Size(), rel)
		return nil
	})
}
