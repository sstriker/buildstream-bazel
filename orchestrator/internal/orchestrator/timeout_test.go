package orchestrator_test

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/sstriker/buildstream-bazel/orchestrator/internal/orchestrator"
)

// TestRun_PerElementTimeout: stub sleeps longer than the per-element
// cap; orchestrator surfaces a Tier-2/3 error mentioning the element.
// The error path goes through ctx.WithTimeout -> exec.CommandContext
// killing the subprocess -> convertOne returning err -> driveElements
// recording it as the run-level error.
func TestRun_PerElementTimeout(t *testing.T) {
	stub := os.Args[0]
	t.Setenv("ORCHESTRATOR_STUB_CONVERTER", "1")
	t.Setenv("ORCHESTRATOR_STUB_MODE", "success")
	t.Setenv("ORCHESTRATOR_STUB_SLEEP", "10s") // way more than the timeout

	proj, g := mustLoadFixture(t)
	out := t.TempDir()

	start := time.Now()
	_, err := orchestrator.Run(context.Background(), orchestrator.Options{
		Project:           proj,
		Graph:             g,
		Out:               out,
		ConverterBinary:   stub,
		Concurrency:       1,
		PerElementTimeout: 200 * time.Millisecond,
		Log:               testLog{t},
	})
	elapsed := time.Since(start)
	if err == nil {
		t.Fatalf("expected timeout error, got nil after %v", elapsed)
	}
	// First element (components/hello) should hit the timeout long
	// before the 10s sleep would finish.
	if elapsed > 5*time.Second {
		t.Errorf("orchestrator took %v; timeout did not fire", elapsed)
	}
	// Cancellation surfaces as ctx.DeadlineExceeded somewhere in the
	// error chain, OR as a "signal: killed"-style exec error. Either
	// is acceptable evidence that the per-element cap fired.
	hasDeadline := errors.Is(err, context.DeadlineExceeded)
	hasKilled := strings.Contains(err.Error(), "killed") || strings.Contains(err.Error(), "deadline")
	if !hasDeadline && !hasKilled {
		t.Errorf("err = %v; expected DeadlineExceeded or 'killed'/'deadline'", err)
	}
}

// TestRun_PerElementTimeout_PropagatesToAction: the timeout configured
// on the runner must end up in the BuiltAction's Action.Timeout, so
// remote workers enforce the same cap. Easiest way to verify is to
// run a remote-execute pass and check the cached AC entry's
// referenced Action.timeout value — but exposing that requires
// reaching into reapi internals. Simpler: rebuild via reapi.Build
// directly with the same Inputs and assert the proto field.
func TestRun_PerElementTimeout_PropagatesToAction(t *testing.T) {
	// This is a unit test of the wiring, not a full Run. See
	// internal/reapi/action_test.go for the proto-level assertion.
	t.Skip("see internal/reapi/action_test.go TestBuild_TimeoutSetsAction")
}
