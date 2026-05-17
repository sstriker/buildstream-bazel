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

// TestRun_CanonicalizeFailure_E2E covers the full #210 integration
// path with no test hooks: drive bazel.EmitWithOptions against a
// real IR but pass a malformed Header so canonicalize's buildtools
// Parse rejects the assembled body, then verify the emit error is
// already a typed Tier-1 *failure.Error (canonicalize wraps at the
// failure site, not in this binary's main.go — keeps the typed-code
// contract local) and that handleError serialises it to a
// failure.json carrying the documented bazel-canonicalize-failed
// shape.
//
// Using Options.Header as the failure injector keeps the test
// hook-free: Header is a public field on bazel.Options, and an
// invalid value flows through the same canonicalize path the
// cmake-value-smuggling shape in #210's reproduction did. The
// canonicalize step itself doesn't care about the bytes' source —
// triggering via Header is functionally equivalent.
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

	// Assert canonicalize wrapped at the failure site rather than
	// expecting a downstream caller to recognise the shape. This
	// is the load-bearing assertion against the review observation
	// on PR #211 that an `errors.As != typed` fallback in main.go
	// would mis-code constraint-pass errors as
	// bazel-canonicalize-failed.
	var tier1 *failure.Error
	if !errors.As(emitErr, &tier1) {
		t.Fatalf("EmitWithOptions returned untyped %T (%v); want pre-typed *failure.Error from canonicalize", emitErr, emitErr)
	}
	if tier1.Code != failure.BazelCanonicalizeFailed {
		t.Fatalf("EmitWithOptions returned code %q, want %q",
			tier1.Code, failure.BazelCanonicalizeFailed)
	}
	if !strings.Contains(tier1.Message, "canonicalize") {
		t.Errorf("error message dropped the canonicalize tag: %q", tier1.Message)
	}

	// Drive handleError so the failure.json file write + exit
	// code are exercised end-to-end (the same path the binary
	// takes when run() returns).
	failurePath := filepath.Join(t.TempDir(), "failure.json")
	args := cli.Args{OutFailure: failurePath}
	exit := handleError(args, emitErr)
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
// path works) without trying to write the file.
func TestRun_CanonicalizeFailure_NoOutFailureFlag(t *testing.T) {
	tier1 := failure.New(failure.BazelCanonicalizeFailed, "synthetic")
	exit := handleError(cli.Args{}, tier1) // empty OutFailure
	if exit != cli.ExitTier1 {
		t.Errorf("handleError returned exit code %d, want %d (ExitTier1)", exit, cli.ExitTier1)
	}
}
