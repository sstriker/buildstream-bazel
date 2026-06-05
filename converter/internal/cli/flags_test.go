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
func TestParse_StrictTraceImpliedByFidelityStrict(t *testing.T) {
	var stderr bytes.Buffer

	// Default --fidelity=strict implicitly enables --strict-trace.
	args, code := Parse([]string{"--source-root", "/proj"}, &stderr)
	if code != ExitSuccess {
		t.Fatalf("default parse failed: code=%d stderr=%q", code, stderr.String())
	}
	if !args.StrictTrace {
		t.Errorf("StrictTrace = false under default --fidelity=strict; want true (the dial implies it)")
	}

	// Explicit --strict-trace=false wins over the dial-derived default.
	stderr.Reset()
	args, code = Parse([]string{"--source-root", "/proj", "--strict-trace=false"}, &stderr)
	if code != ExitSuccess {
		t.Fatalf("explicit-false parse failed: code=%d stderr=%q", code, stderr.String())
	}
	if args.StrictTrace {
		t.Errorf("explicit --strict-trace=false should override dial-derived default; got true")
	}

	// --fidelity=best-effort doesn't imply --strict-trace.
	stderr.Reset()
	args, code = Parse([]string{"--source-root", "/proj", "--fidelity=best-effort"}, &stderr)
	if code != ExitSuccess {
		t.Fatalf("best-effort parse failed: code=%d stderr=%q", code, stderr.String())
	}
	if args.StrictTrace {
		t.Errorf("StrictTrace = true under --fidelity=best-effort; want false (only strict implies it)")
	}
}

func TestParse_DiagnosticsImpliesProbeDistroHardeningAndVerify(t *testing.T) {
	var stderr bytes.Buffer

	// --diagnostics implicitly enables probe-distro-hardening, verify,
	// and ignore-rejections-for-diagnostics.
	args, code := Parse([]string{"--source-root", "/proj", "--diagnostics"}, &stderr)
	if code != ExitSuccess {
		t.Fatalf("diagnostics parse failed: code=%d stderr=%q", code, stderr.String())
	}
	if !args.IgnoreRejectionsForDiagnostics {
		t.Errorf("--diagnostics should imply --ignore-rejections-for-diagnostics")
	}
	if !args.ProbeDistroHardening {
		t.Errorf("--diagnostics should imply --probe-distro-hardening")
	}
	if !args.Verify {
		t.Errorf("--diagnostics should imply --verify")
	}

	// Explicit --verify=false wins over the dial-derived default.
	stderr.Reset()
	args, code = Parse([]string{"--source-root", "/proj", "--diagnostics", "--verify=false"}, &stderr)
	if code != ExitSuccess {
		t.Fatalf("diagnostics + explicit verify=false parse failed: code=%d stderr=%q", code, stderr.String())
	}
	if args.Verify {
		t.Errorf("explicit --verify=false should override dial-derived default; got true")
	}

	// Without --diagnostics, the dial-implied switches stay off (only
	// the explicit values flow through).
	stderr.Reset()
	args, code = Parse([]string{"--source-root", "/proj"}, &stderr)
	if code != ExitSuccess {
		t.Fatalf("no-diagnostics parse failed: code=%d stderr=%q", code, stderr.String())
	}
	if args.ProbeDistroHardening {
		t.Errorf("ProbeDistroHardening = true without --diagnostics; want false")
	}
	if args.Verify {
		t.Errorf("Verify = true without --diagnostics; want false")
	}
}

// TestParse_CMakeScriptRunner covers the cmake-P-lift opt-in
// flag. Empty by default; non-empty enables the genrule lift
// of add_custom_command(... cmake -P ...) shapes.
func TestParse_CMakeScriptRunner(t *testing.T) {
	var stderr bytes.Buffer
	args, code := Parse([]string{
		"--source-root", "/proj",
		"--cmake-script-runner=//tools:cmake-script-runner",
	}, &stderr)
	if code != ExitSuccess {
		t.Fatalf("parse failed: code=%d stderr=%q", code, stderr.String())
	}
	if args.CMakeScriptRunner != "//tools:cmake-script-runner" {
		t.Errorf("CMakeScriptRunner=%q; want //tools:cmake-script-runner", args.CMakeScriptRunner)
	}

	// Default empty.
	stderr.Reset()
	args, _ = Parse([]string{"--source-root", "/proj"}, &stderr)
	if args.CMakeScriptRunner != "" {
		t.Errorf("CMakeScriptRunner default=%q; want empty", args.CMakeScriptRunner)
	}
}

// TestParse_CMakeScriptTrace covers the opt-in trace-based
// dep discovery flag for the cmake-P lift.
func TestParse_CMakeScriptTrace(t *testing.T) {
	var stderr bytes.Buffer
	args, code := Parse([]string{
		"--source-root", "/proj",
		"--cmake-script-runner=//tools:cmake-script-runner",
		"--cmake-script-trace",
	}, &stderr)
	if code != ExitSuccess {
		t.Fatalf("parse failed: code=%d stderr=%q", code, stderr.String())
	}
	if !args.CMakeScriptTrace {
		t.Errorf("CMakeScriptTrace=false; want true")
	}

	// Default off.
	stderr.Reset()
	args, _ = Parse([]string{"--source-root", "/proj"}, &stderr)
	if args.CMakeScriptTrace {
		t.Errorf("CMakeScriptTrace default=true; want false")
	}
}

// TestParse_CMakeScriptBake covers the convert-time-bake
// opt-in for the cmake -P lift.
func TestParse_CMakeScriptBake(t *testing.T) {
	var stderr bytes.Buffer
	args, code := Parse([]string{
		"--source-root", "/proj",
		"--cmake-script-bake",
	}, &stderr)
	if code != ExitSuccess {
		t.Fatalf("parse failed: code=%d stderr=%q", code, stderr.String())
	}
	if !args.CMakeScriptBake {
		t.Errorf("CMakeScriptBake=false; want true")
	}
}

// TestParse_IgnoreRejectionsForDiagnostics covers the diagnostic-
// mode flag pair: --ignore-rejections-for-diagnostics flips the bool
// and --rejections-report carries the JSON sidecar path.
func TestParse_IgnoreRejectionsForDiagnostics(t *testing.T) {
	var stderr bytes.Buffer
	args, code := Parse([]string{
		"--source-root", "/proj",
		"--ignore-rejections-for-diagnostics",
		"--rejections-report=/tmp/rej.json",
	}, &stderr)
	if code != ExitSuccess {
		t.Fatalf("parse failed: code=%d stderr=%q", code, stderr.String())
	}
	if !args.IgnoreRejectionsForDiagnostics {
		t.Errorf("IgnoreRejectionsForDiagnostics=false; want true")
	}
	if args.RejectionsReport != "/tmp/rej.json" {
		t.Errorf("RejectionsReport=%q; want /tmp/rej.json", args.RejectionsReport)
	}

	// Default off.
	stderr.Reset()
	args, code = Parse([]string{"--source-root", "/proj"}, &stderr)
	if code != ExitSuccess {
		t.Fatalf("parse failed: code=%d stderr=%q", code, stderr.String())
	}
	if args.IgnoreRejectionsForDiagnostics {
		t.Errorf("IgnoreRejectionsForDiagnostics default=true; want false")
	}
	if args.RejectionsReport != "" {
		t.Errorf("RejectionsReport default=%q; want empty", args.RejectionsReport)
	}
}

// TestParse_ProbeDistroHardening pins the diagnostic-mode flag
// that runs the host-cc hardening probe.
func TestParse_ProbeDistroHardening(t *testing.T) {
	var stderr bytes.Buffer
	args, code := Parse([]string{"--source-root", "/proj", "--probe-distro-hardening"}, &stderr)
	if code != ExitSuccess {
		t.Fatalf("parse failed: code=%d stderr=%q", code, stderr.String())
	}
	if !args.ProbeDistroHardening {
		t.Errorf("ProbeDistroHardening=false; want true")
	}

	// Default off.
	stderr.Reset()
	args, _ = Parse([]string{"--source-root", "/proj"}, &stderr)
	if args.ProbeDistroHardening {
		t.Errorf("ProbeDistroHardening default=true; want false")
	}
}

// TestParse_BuildTypesCommaSlice covers the Phase 5 multi-config
// CLI flag: --build-types takes a comma-separated list and pins
// the entries (preserving order, dropping empties).
func TestParse_BuildTypesCommaSlice(t *testing.T) {
	var stderr bytes.Buffer
	args, code := Parse([]string{"--source-root", "/proj", "--build-types=Debug,Release,RelWithDebInfo"}, &stderr)
	if code != ExitSuccess {
		t.Fatalf("parse failed: code=%d stderr=%q", code, stderr.String())
	}
	want := []string{"Debug", "Release", "RelWithDebInfo"}
	if len(args.BuildTypes) != len(want) {
		t.Fatalf("BuildTypes len: got %d want %d (%v)", len(args.BuildTypes), len(want), args.BuildTypes)
	}
	for i := range want {
		if args.BuildTypes[i] != want[i] {
			t.Errorf("BuildTypes[%d] = %q; want %q", i, args.BuildTypes[i], want[i])
		}
	}
}

// TestParse_BuildTypesDropsEmpties pins the "stray comma" handling
// (`--build-types=,Debug,,Release,` → [Debug, Release]).
func TestParse_BuildTypesDropsEmpties(t *testing.T) {
	var stderr bytes.Buffer
	args, code := Parse([]string{"--source-root", "/proj", "--build-types=,Debug,,Release,"}, &stderr)
	if code != ExitSuccess {
		t.Fatalf("parse failed: code=%d stderr=%q", code, stderr.String())
	}
	want := []string{"Debug", "Release"}
	if len(args.BuildTypes) != len(want) {
		t.Fatalf("BuildTypes: got %v want %v", args.BuildTypes, want)
	}
}

// TestParse_BuildType covers the single-config --build-type path.
func TestParse_BuildType(t *testing.T) {
	var stderr bytes.Buffer
	args, code := Parse([]string{"--source-root", "/proj", "--build-type=Debug"}, &stderr)
	if code != ExitSuccess {
		t.Fatalf("parse failed: code=%d stderr=%q", code, stderr.String())
	}
	if args.BuildType != "Debug" {
		t.Errorf("BuildType = %q; want Debug", args.BuildType)
	}
	if len(args.BuildTypes) != 0 {
		t.Errorf("BuildTypes should be empty when --build-type set; got %v", args.BuildTypes)
	}
}

// TestParse_CmakeDefine covers the repeatable --cmake-define happy path:
// multiple KEY=VALUE entries accumulate in order, and a bare KEY (no '=')
// is accepted (cmake reads it as KEY="").
func TestParse_CmakeDefine(t *testing.T) {
	var stderr bytes.Buffer
	args, code := Parse([]string{
		"--source-root", "/proj",
		"--cmake-define", "CMAKE_CXX_FLAGS=-w",
		"--cmake-define", "GLM_ENABLE_CXX_17=ON",
		"--cmake-define", "BARE_KEY",
	}, &stderr)
	if code != ExitSuccess {
		t.Fatalf("parse failed: code=%d stderr=%q", code, stderr.String())
	}
	want := []string{"CMAKE_CXX_FLAGS=-w", "GLM_ENABLE_CXX_17=ON", "BARE_KEY"}
	if len(args.CmakeDefines) != len(want) {
		t.Fatalf("CmakeDefines = %v; want %v", args.CmakeDefines, want)
	}
	for i, w := range want {
		if args.CmakeDefines[i] != w {
			t.Errorf("CmakeDefines[%d] = %q; want %q", i, args.CmakeDefines[i], w)
		}
	}
}

// TestParse_CmakeDefineEmptyKeyRejected pins that a "=VALUE" entry (empty
// KEY) is a clean usage error, not a downstream cmake "-D=VALUE" failure.
func TestParse_CmakeDefineEmptyKeyRejected(t *testing.T) {
	var stderr bytes.Buffer
	_, code := Parse([]string{"--source-root", "/proj", "--cmake-define", "=VALUE"}, &stderr)
	if code != ExitUsage {
		t.Fatalf("code = %d; want ExitUsage (%d)", code, ExitUsage)
	}
	if !strings.Contains(stderr.String(), "--cmake-define") {
		t.Errorf("stderr should name the offending flag, got %q", stderr.String())
	}
}

// TestParse_CmakeDefineWhitespaceKeyRejected pins that a KEY carrying
// whitespace (e.g. "CMAKE_CXX_FLAGS =-w") is a clean usage error rather than
// silently setting the wrong cache variable.
func TestParse_CmakeDefineWhitespaceKeyRejected(t *testing.T) {
	for _, bad := range []string{"CMAKE_CXX_FLAGS =-w", " LEADING=1", "TRAIL\t=1"} {
		var stderr bytes.Buffer
		_, code := Parse([]string{"--source-root", "/proj", "--cmake-define", bad}, &stderr)
		if code != ExitUsage {
			t.Fatalf("%q: code = %d; want ExitUsage (%d)", bad, code, ExitUsage)
		}
		if !strings.Contains(stderr.String(), "whitespace") {
			t.Errorf("%q: stderr should mention whitespace, got %q", bad, stderr.String())
		}
	}
}

// TestParse_CmakeDefineReservedRejected pins that a cache var the converter
// owns through a dedicated flag (CMAKE_BUILD_TYPE / CMAKE_CONFIGURATION_TYPES)
// is rejected at parse time rather than conflicting at configure time.
func TestParse_CmakeDefineReservedRejected(t *testing.T) {
	for _, key := range []string{"CMAKE_BUILD_TYPE", "CMAKE_CONFIGURATION_TYPES"} {
		var stderr bytes.Buffer
		_, code := Parse([]string{"--source-root", "/proj", "--cmake-define", key + "=Debug"}, &stderr)
		if code != ExitUsage {
			t.Fatalf("%s: code = %d; want ExitUsage (%d)", key, code, ExitUsage)
		}
		if !strings.Contains(stderr.String(), "reserved") {
			t.Errorf("%s: stderr should explain it is reserved, got %q", key, stderr.String())
		}
	}
}
