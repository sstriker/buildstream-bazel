package reapi

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"

	"github.com/sstriker/buildstream-bazel/internal/cas"
)

// LocalExecutor runs a BuiltAction in-process: it ensures every input
// blob is in the store, materializes the input root to a host tmpdir,
// execs the Command's argv with that tmpdir as cwd, and synthesizes
// an ActionResult from the outputs declared in Command.output_paths.
// On a zero exit it also publishes the ActionResult to the store's
// ActionCache so the next orchestrator run hits.
//
// Used as the default Executor when the orchestrator isn't configured
// with a remote endpoint — `make convert-and-build` and most local-dev
// flows go through LocalExecutor.
type LocalExecutor struct{}

// NewLocalExecutor returns a fresh LocalExecutor. The zero value is
// also usable; the constructor exists so callers who want to wire
// options later don't have to change construction sites.
func NewLocalExecutor() *LocalExecutor { return &LocalExecutor{} }

// envFromCommand returns the env to feed the child: the parent's env
// (so PATH and other host bits are preserved) plus the Command's
// declared EnvironmentVariables, with the Command's entries winning on
// any name collision. Host workers in REAPI typically clear the parent
// env entirely; the in-process LocalExecutor stays permissive because
// cmakerun and other tooling expect a working PATH.
func envFromCommand(cmd *repb.Command) []string {
	env := os.Environ()
	override := make(map[string]string, len(cmd.EnvironmentVariables))
	for _, kv := range cmd.EnvironmentVariables {
		override[kv.Name] = kv.Value
	}
	out := make([]string, 0, len(env)+len(override))
	for _, e := range env {
		key := e
		if i := strings.IndexByte(e, '='); i >= 0 {
			key = e[:i]
		}
		if _, ok := override[key]; ok {
			continue
		}
		out = append(out, e)
	}
	for k, v := range override {
		out = append(out, k+"="+v)
	}
	return out
}

// Execute satisfies the Executor interface.
func (e *LocalExecutor) Execute(ctx context.Context, store cas.Store, built *BuiltAction) (*repb.ActionResult, error) {
	if err := UploadInputs(ctx, store, built); err != nil {
		return nil, fmt.Errorf("local-executor: upload inputs: %w", err)
	}
	workDir, err := os.MkdirTemp("", "local-executor-*")
	if err != nil {
		return nil, fmt.Errorf("local-executor: mkdir work: %w", err)
	}
	defer os.RemoveAll(workDir)

	if err := cas.MaterializeDirectory(ctx, store, built.Action.InputRootDigest, workDir); err != nil {
		return nil, fmt.Errorf("local-executor: materialize input: %w", err)
	}

	if len(built.Command.Arguments) == 0 {
		return nil, fmt.Errorf("local-executor: command has empty argv")
	}
	// Resolve the binary to an absolute path before exec, matching
	// fakecas.ExecutionServer's pattern. Relative argv0 + Cmd.Dir works
	// in the single-Action case but races under concurrent runs (the
	// kernel can return ETXTBSY when another goroutine is mid-write to
	// a different copy of the same source binary).
	argv0 := built.Command.Arguments[0]
	if !filepath.IsAbs(argv0) {
		argv0 = filepath.Join(workDir, built.Command.WorkingDirectory, argv0)
	}
	wd := filepath.Join(workDir, built.Command.WorkingDirectory)

	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, argv0, built.Command.Arguments[1:]...)
	cmd.Dir = wd
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	cmd.Env = envFromCommand(built.Command)

	var exitCode int32
	if runErr := cmd.Run(); runErr != nil {
		// Context cancel/timeout: surface the ctx error so callers can
		// errors.Is(err, context.DeadlineExceeded) — the remote-execute
		// path expects the same shape.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, fmt.Errorf("local-executor: %w", ctxErr)
		}
		// ExitError carries the exit code; anything else (couldn't
		// fork, missing binary, ...) is a Tier-2-style infra failure
		// the caller surfaces as a hard error.
		var ee *exec.ExitError
		if !errors.As(runErr, &ee) {
			return nil, fmt.Errorf("local-executor: run: %w", runErr)
		}
		exitCode = int32(ee.ExitCode())
	}

	ar, err := SynthesizeResult(ctx, store, workDir, built.OutputPaths, exitCode, stdout.Bytes(), stderr.Bytes())
	if err != nil {
		return nil, fmt.Errorf("local-executor: synthesize: %w", err)
	}
	if exitCode == 0 {
		if err := store.UpdateActionResult(ctx, built.ActionDigest, ar); err != nil {
			return nil, fmt.Errorf("local-executor: update AC: %w", err)
		}
	}
	return ar, nil
}
