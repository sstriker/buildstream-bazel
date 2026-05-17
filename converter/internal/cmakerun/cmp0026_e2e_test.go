package cmakerun

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestConfigure_CMP0026_HintSurfaces is the end-to-end coverage
// for the cmake 4.x CMP0026 hint added in #165. It drives
// Configure against a real fixture whose CMakeLists.txt uses
// `get_target_property(... LOCATION)` (the pre-3.0 idiom yasm
// and other vintage packages still rely on) and asserts the
// surfaced error carries the [hint] block pointing at the
// patch_cmds workaround.
//
// cmake 3.x emits a CMP0026 deprecation warning but still
// resolves the property; configure succeeds and the hint
// doesn't fire. cmake 4.x removed the OLD behaviour and
// fatal-errors. The test detects the cmake major version on
// PATH and skips on anything below 4.x so it stays neutral
// against the pinned 3.28.3 dev-loop and against the older
// 3.31.x the GH runners ship by default.
//
// CI: the "End-to-end (latest cmake — non-blocking)" job
// installs cmake 4.x and runs this test explicitly. The
// "Build + unit tests" job uses the pinned 3.28.3 and this
// test skips there.
func TestConfigure_CMP0026_HintSurfaces(t *testing.T) {
	ctx := context.Background()

	if _, err := exec.LookPath("cmake"); err != nil {
		t.Skip("cmake not on PATH; skipping live CMP0026 e2e")
	}
	major, minor, patch, err := AssertVersion(ctx)
	if err != nil {
		// AssertVersion errors when cmake is below the
		// codemodel-v2 floor (3.20). Older cmakes can't drive
		// our configure path at all, never mind exercise the
		// CMP0026 hint.
		t.Skipf("cmake below codemodel-v2 floor: %v", err)
	}
	if major < 4 {
		t.Skipf("cmake %d.%d.%d does not fatal-error on CMP0026 (need >= 4.x); skipping hint e2e",
			major, minor, patch)
	}

	srcRoot, err := filepath.Abs("../../testdata/sample-projects/cmp0026-tripping")
	if err != nil {
		t.Fatalf("abs src: %v", err)
	}
	buildDir, err := os.MkdirTemp("", "cmp0026-e2e-*")
	if err != nil {
		t.Fatalf("tmp build dir: %v", err)
	}
	defer os.RemoveAll(buildDir)

	var stderrBuf bytes.Buffer
	_, configureErr := Configure(ctx, Options{
		SourceRoot: srcRoot,
		BuildDir:   buildDir,
		Stdout:     &stderrBuf,
		Stderr:     &stderrBuf,
	})
	if configureErr == nil {
		t.Fatalf("Configure unexpectedly succeeded against the cmp0026-tripping fixture under cmake %d.%d.%d; cmake 4.x should fatal-error",
			major, minor, patch)
	}
	msg := configureErr.Error()
	// The hint block is appended by annotateConfigureFailure.
	// Its three load-bearing pieces are the "[hint]" marker,
	// the CMP0026 name (so operators can grep for it), and a
	// reference to the patch_cmds workaround.
	for _, want := range []string{"[hint]", "CMP0026", "patch_cmds"} {
		if !strings.Contains(msg, want) {
			t.Errorf("Configure error missing %q; full message:\n%s", want, msg)
		}
	}
}

// TestConfigure_CMP0026Shim_RescuesFatalError is the live coverage
// for the #208 opt-in shim. Same fixture as above; with
// Options.CMP0026Shim true the get_target_property override turns
// the LOCATION read into a $<TARGET_FILE:...> generator expression
// and configure should succeed under cmake 4.x. Skips under cmake
// 3.x for the same reason as the hint test (3.x still resolves
// LOCATION natively; the shim's there's-no-fatal-error-to-rescue
// guard makes the test vacuous).
func TestConfigure_CMP0026Shim_RescuesFatalError(t *testing.T) {
	ctx := context.Background()

	if _, err := exec.LookPath("cmake"); err != nil {
		t.Skip("cmake not on PATH; skipping live CMP0026 shim e2e")
	}
	major, minor, patch, err := AssertVersion(ctx)
	if err != nil {
		t.Skipf("cmake below codemodel-v2 floor: %v", err)
	}
	if major < 4 {
		t.Skipf("cmake %d.%d.%d does not fatal-error on CMP0026 (need >= 4.x); shim has nothing to rescue",
			major, minor, patch)
	}

	srcRoot, err := filepath.Abs("../../testdata/sample-projects/cmp0026-tripping")
	if err != nil {
		t.Fatalf("abs src: %v", err)
	}
	buildDir, err := os.MkdirTemp("", "cmp0026-shim-e2e-*")
	if err != nil {
		t.Fatalf("tmp build dir: %v", err)
	}
	defer os.RemoveAll(buildDir)

	var stderrBuf bytes.Buffer
	_, configureErr := Configure(ctx, Options{
		SourceRoot:  srcRoot,
		BuildDir:    buildDir,
		CMP0026Shim: true,
		Stdout:      &stderrBuf,
		Stderr:      &stderrBuf,
	})
	if configureErr != nil {
		t.Fatalf("Configure with --cmp0026-shim against cmp0026-tripping fixture under cmake %d.%d.%d unexpectedly failed: %v\nstderr:\n%s",
			major, minor, patch, configureErr, stderrBuf.String())
	}
}
