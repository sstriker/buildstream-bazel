package main

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/internal/cli"
)

// TestE2E_OutDebugBundle_CapturesOnLoweringFailure is the regression guard
// for the review finding on #575: the primary thing you want to debug is a
// FAILED conversion, but on the fresh-configure path the temp build dir is
// deleted by a deferred RemoveAll on the error return. The capture is
// registered as a LATER defer than that RemoveAll, so LIFO fires it FIRST —
// the bundle must survive a Tier-1 lowering refusal.
//
// cmake-script-mode-refusal configures cleanly (so a File API reply lands)
// but Tier-1-refuses during lowering in strict mode — exactly the
// configure-succeeds / lowering-fails case.
func TestE2E_OutDebugBundle_CapturesOnLoweringFailure(t *testing.T) {
	if _, err := exec.LookPath("cmake"); err != nil {
		t.Skipf("cmake not on PATH: %v", err)
	}
	repoRoot, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	fixture := filepath.Join(repoRoot, "converter", "testdata", "sample-projects", "cmake-script-mode-refusal")
	tmp := t.TempDir()
	bundle := filepath.Join(tmp, "bundle")

	args, code := cli.Parse([]string{
		"--source-root", fixture,
		"--out-build", filepath.Join(tmp, "BUILD.bazel"),
		"--out-debug-bundle", bundle,
	}, io.Discard)
	if code != cli.ExitSuccess {
		t.Fatalf("cli.Parse failed: code=%d", code)
	}

	// The fixture refuses, so run returns a Tier-1 error — and the
	// fresh-configure temp dir is torn down on that return.
	if runErr := run(args); runErr == nil {
		t.Fatal("expected a Tier-1 refusal from cmake-script-mode-refusal; got nil (fixture no longer refuses?)")
	}

	// Despite the failure + the temp-dir cleanup, the bundle must carry the
	// primary inputs captured before RemoveAll fired.
	for _, want := range []string{"trace.jsonl", "build.ninja", "BUNDLE-README.txt"} {
		if _, err := os.Stat(filepath.Join(bundle, want)); err != nil {
			t.Errorf("debug bundle missing %s after a failed conversion: %v", want, err)
		}
	}
	matches, _ := filepath.Glob(filepath.Join(bundle, ".cmake", "api", "v1", "reply", "codemodel-v2-*.json"))
	if len(matches) == 0 {
		t.Error("debug bundle missing the codemodel reply after a failed conversion")
	}
}
