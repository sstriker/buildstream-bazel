package cli

import (
	"bytes"
	"strings"
	"testing"
)

// TestParse_RequiresEntryPoint pins that one of --source-root /
// --reply-dir / --cmake-build-dir is required.
func TestParse_RequiresEntryPoint(t *testing.T) {
	var stderr bytes.Buffer
	args, code := Parse([]string{}, &stderr)
	if code != ExitUsage {
		t.Errorf("code = %d, want ExitUsage (%d)", code, ExitUsage)
	}
	if args.SourceRoot != "" || args.ReplyDir != "" || args.CMakeBuildDir != "" {
		t.Errorf("expected zero entry-point fields, got %+v", args)
	}
	if !strings.Contains(stderr.String(), "--source-root") {
		t.Errorf("stderr should name the entry-point flags, got %q", stderr.String())
	}
}

// TestParse_AcceptsSourceRoot pins the live-cmake invocation
// path.
func TestParse_AcceptsSourceRoot(t *testing.T) {
	var stderr bytes.Buffer
	args, code := Parse([]string{"--source-root", "/proj"}, &stderr)
	if code != ExitSuccess {
		t.Fatalf("code = %d, stderr=%q", code, stderr.String())
	}
	if args.SourceRoot != "/proj" {
		t.Errorf("SourceRoot = %q, want /proj", args.SourceRoot)
	}
}

// TestParse_AcceptsReplyDir pins the legacy offline path.
func TestParse_AcceptsReplyDir(t *testing.T) {
	var stderr bytes.Buffer
	args, code := Parse([]string{"--reply-dir", "/build/.cmake/api/v1/reply"}, &stderr)
	if code != ExitSuccess {
		t.Fatalf("code = %d, stderr=%q", code, stderr.String())
	}
	if args.ReplyDir != "/build/.cmake/api/v1/reply" {
		t.Errorf("ReplyDir = %q, want /build/.cmake/api/v1/reply", args.ReplyDir)
	}
	if args.CMakeBuildDir != "" {
		t.Errorf("CMakeBuildDir should stay empty when --reply-dir set, got %q", args.CMakeBuildDir)
	}
}

// TestParse_CMakeBuildDirDerivesReplyDir pins the friendly
// alias: --cmake-build-dir <path> derives ReplyDir as
// <path>/.cmake/api/v1/reply so downstream code only reads
// ReplyDir.
func TestParse_CMakeBuildDirDerivesReplyDir(t *testing.T) {
	var stderr bytes.Buffer
	args, code := Parse([]string{"--cmake-build-dir", "/build"}, &stderr)
	if code != ExitSuccess {
		t.Fatalf("code = %d, stderr=%q", code, stderr.String())
	}
	if want := "/build/.cmake/api/v1/reply"; args.ReplyDir != want {
		t.Errorf("ReplyDir = %q, want %q (derived from --cmake-build-dir)", args.ReplyDir, want)
	}
	if args.CMakeBuildDir != "/build" {
		t.Errorf("CMakeBuildDir = %q, want /build", args.CMakeBuildDir)
	}
}

// TestParse_RejectsSourceRootPlusReplyDir pins that the
// two-or-more-entry-points conflict surfaces as ExitUsage.
func TestParse_RejectsSourceRootPlusReplyDir(t *testing.T) {
	var stderr bytes.Buffer
	_, code := Parse([]string{"--source-root", "/proj", "--reply-dir", "/reply"}, &stderr)
	if code != ExitUsage {
		t.Errorf("code = %d, want ExitUsage", code)
	}
	if !strings.Contains(stderr.String(), "incompatible") {
		t.Errorf("stderr should explain the conflict, got %q", stderr.String())
	}
}

// TestParse_RejectsSourceRootPlusCMakeBuildDir is the
// symmetric guard.
func TestParse_RejectsSourceRootPlusCMakeBuildDir(t *testing.T) {
	var stderr bytes.Buffer
	_, code := Parse([]string{"--source-root", "/proj", "--cmake-build-dir", "/build"}, &stderr)
	if code != ExitUsage {
		t.Errorf("code = %d, want ExitUsage", code)
	}
}

// TestParse_RejectsReplyDirPlusCMakeBuildDir pins that the two
// aliases can't both be set (they'd disagree on the canonical
// ReplyDir path).
func TestParse_RejectsReplyDirPlusCMakeBuildDir(t *testing.T) {
	var stderr bytes.Buffer
	_, code := Parse([]string{"--reply-dir", "/r", "--cmake-build-dir", "/b"}, &stderr)
	if code != ExitUsage {
		t.Errorf("code = %d, want ExitUsage", code)
	}
	if !strings.Contains(stderr.String(), "aliases") {
		t.Errorf("stderr should explain the aliases conflict, got %q", stderr.String())
	}
}

// TestParse_StrictTraceParsesAsBool pins that --strict-trace
// is wired through and is false by default.
func TestParse_StrictTraceParsesAsBool(t *testing.T) {
	var stderr bytes.Buffer

	// Default: false.
	args, code := Parse([]string{"--source-root", "/proj"}, &stderr)
	if code != ExitSuccess {
		t.Fatalf("default parse failed: code=%d stderr=%q", code, stderr.String())
	}
	if args.StrictTrace {
		t.Errorf("StrictTrace = true by default; want false")
	}

	// Explicit: true.
	stderr.Reset()
	args, code = Parse([]string{"--source-root", "/proj", "--strict-trace"}, &stderr)
	if code != ExitSuccess {
		t.Fatalf("explicit parse failed: code=%d stderr=%q", code, stderr.String())
	}
	if !args.StrictTrace {
		t.Errorf("StrictTrace = false after --strict-trace; want true")
	}
}
