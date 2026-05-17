package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/emit/bazel"
	"github.com/sstriker/buildstream-bazel/converter/internal/cli"
	"github.com/sstriker/buildstream-bazel/converter/internal/failure"
	"github.com/sstriker/buildstream-bazel/converter/ir"
)

// TestWrapEmitError_PreservesTypedTier1 asserts that wrapEmitError
// passes through a typed Tier-1 error unchanged. The pre-emit
// constraint pass in bazel.EmitWithOptions already produces
// *failure.Error for the #193/#194 hazard family; the wrap must
// not re-wrap those (or it would mask the constraint code under
// `bazel-canonicalize-failed`, breaking the orchestrator's dedup
// key contract).
func TestWrapEmitError_PreservesTypedTier1(t *testing.T) {
	original := failure.New(failure.UnsupportedTargetType, "synthetic upstream failure")
	wrapped := wrapEmitError(original)

	var got *failure.Error
	if !errors.As(wrapped, &got) {
		t.Fatalf("wrapped error is not *failure.Error: %T (%v)", wrapped, wrapped)
	}
	if got.Code != failure.UnsupportedTargetType {
		t.Errorf("wrap clobbered the code: got %q, want %q (typed Tier-1 should pass through)",
			got.Code, failure.UnsupportedTargetType)
	}
}

// TestWrapEmitError_WrapsRawAsBazelCanonicalizeFailed asserts that
// any non-Tier-1 error from EmitWithOptions becomes a typed
// `bazel-canonicalize-failed` so the orchestrator gets a stable
// dedup code and operators get a structured failure.json instead
// of an exit-65 with no output (#210).
func TestWrapEmitError_WrapsRawAsBazelCanonicalizeFailed(t *testing.T) {
	original := errors.New("synthetic raw error from buildtools parse")
	wrapped := wrapEmitError(original)

	var got *failure.Error
	if !errors.As(wrapped, &got) {
		t.Fatalf("wrapped error is not *failure.Error: %T (%v)", wrapped, wrapped)
	}
	if got.Code != failure.BazelCanonicalizeFailed {
		t.Errorf("wrap produced wrong code: got %q, want %q",
			got.Code, failure.BazelCanonicalizeFailed)
	}
	if !strings.Contains(got.Message, "synthetic raw error from buildtools parse") {
		t.Errorf("wrap dropped the underlying message: %q", got.Message)
	}
}

// TestRun_CanonicalizeFailure_E2E covers the full #210 integration
// path with no test hooks: drive bazel.EmitWithOptions against a
// real IR but pass a malformed Header so canonicalize's buildtools
// Parse rejects the assembled body, then mirror what run() does on
// that error (wrapEmitError + handleError) and assert the
// operator-visible surface — exit code AND failure.json — match
// the documented bazel-canonicalize-failed contract.
//
// Using Options.Header as the failure injector keeps the test
// hook-free: Header is a public field on bazel.Options, and an
// invalid value flows through the same canonicalize path the
// #210 reproduction did. The default CLI doesn't set Header, but
// the canonicalize step itself doesn't care about the source of
// the bytes — exercising the failure via Header is functionally
// equivalent to exercising it via the cmake-value smuggling
// shape the issue described.
func TestRun_CanonicalizeFailure_E2E(t *testing.T) {
	pkg := &ir.Package{
		Name: "x",
		Targets: []ir.Target{{
			Name: "lib",
			Kind: ir.KindCCLibrary,
			Srcs: []string{"a.c"},
		}},
	}

	// Force canonicalize to fail by prepending malformed Starlark
	// to the assembled body. EmitWithOptions appends every
	// template-rendered rule AFTER this header verbatim, then
	// hands the whole buffer to canonicalize; buildtools' parser
	// rejects the first line.
	_, emitErr := bazel.EmitWithOptions(pkg, bazel.Options{
		Header: "not valid starlark: this is malformed (\n",
	})
	if emitErr == nil {
		t.Fatal("EmitWithOptions accepted a malformed-Header IR; the test setup no longer triggers the failure path")
	}
	if !strings.Contains(emitErr.Error(), "canonicalize") {
		t.Fatalf("emit error doesn't look like a canonicalize failure (test no longer covers #210): %v", emitErr)
	}

	// Mirror run()'s error wrap; assert the typed code lands.
	wrapped := wrapEmitError(emitErr)
	var tier1 *failure.Error
	if !errors.As(wrapped, &tier1) {
		t.Fatalf("wrapEmitError did not produce a typed failure: %T (%v)", wrapped, wrapped)
	}
	if tier1.Code != failure.BazelCanonicalizeFailed {
		t.Fatalf("wrap produced wrong code: got %q, want %q",
			tier1.Code, failure.BazelCanonicalizeFailed)
	}

	// Drive handleError so the failure.json file write + exit
	// code are exercised end-to-end (the same path the binary
	// takes when run() returns).
	failurePath := filepath.Join(t.TempDir(), "failure.json")
	args := cli.Args{OutFailure: failurePath}
	exit := handleError(args, wrapped)
	if exit != cli.ExitTier1 {
		t.Errorf("handleError returned exit code %d, want %d (ExitTier1)", exit, cli.ExitTier1)
	}

	body, err := os.ReadFile(failurePath)
	if err != nil {
		t.Fatalf("read failure.json: %v", err)
	}
	var payload struct {
		Tier    int    `json:"tier"`
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("parse failure.json: %v\nbody:\n%s", err, body)
	}
	if payload.Tier != 1 {
		t.Errorf("failure.json tier = %d, want 1", payload.Tier)
	}
	if payload.Code != string(failure.BazelCanonicalizeFailed) {
		t.Errorf("failure.json code = %q, want %q",
			payload.Code, string(failure.BazelCanonicalizeFailed))
	}
	if !strings.Contains(payload.Message, "canonicalize") {
		t.Errorf("failure.json message doesn't carry the canonicalize diagnostic: %q",
			payload.Message)
	}
}

// TestRun_CanonicalizeFailure_NoOutFailureFlag covers the binary's
// behaviour when --out-failure isn't passed: handleError should
// still exit Tier-1 (so the orchestrator's dedup-by-exit-code
// path works) without trying to write the file. Guards against a
// regression where the wrap path assumed OutFailure was always
// set.
func TestRun_CanonicalizeFailure_NoOutFailureFlag(t *testing.T) {
	wrapped := wrapEmitError(errors.New("synthetic canonicalize miss"))

	exit := handleError(cli.Args{}, wrapped) // empty OutFailure
	if exit != cli.ExitTier1 {
		t.Errorf("handleError returned exit code %d, want %d (ExitTier1)", exit, cli.ExitTier1)
	}
}
