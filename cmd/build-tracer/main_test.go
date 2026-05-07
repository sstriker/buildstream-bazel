package main_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestBuildTracer_E2E confirms build-tracer wraps a command
// under strace and produces a trace artifact containing the
// expected execve line. Skipped if strace isn't available
// on PATH (CI containers without ptrace permission would
// trip the run; this test gates on the host being capable).
func TestBuildTracer_E2E(t *testing.T) {
	if _, err := exec.LookPath("strace"); err != nil {
		t.Skip("strace not on PATH; skipping")
	}

	tmp := t.TempDir()
	bin := filepath.Join(tmp, "build-tracer")
	out := filepath.Join(tmp, "trace.log")

	build := exec.Command("go", "build", "-o", bin, ".")
	build.Dir = mustDir(t)
	if err := build.Run(); err != nil {
		t.Fatalf("go build: %v", err)
	}

	// Run a trivial command under the tracer; assert the
	// trace records its execve. /bin/true picks because it
	// has minimal subprocess noise.
	cmd := exec.Command(bin, "--out="+out, "--", "/bin/true")
	if err := cmd.Run(); err != nil {
		t.Fatalf("build-tracer run: %v", err)
	}
	body, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read trace: %v", err)
	}
	if !strings.Contains(string(body), "execve(\"/bin/true\"") {
		t.Errorf("trace missing /bin/true execve\n--body--\n%s", body)
	}
}

// TestBuildTracer_NativeCapturesForkedExecve is a stronger
// check than _E2E: it runs `sh -c '/bin/true'` so the cmd
// path forks (sh) before exec'ing the leaf (/bin/true). The
// native backend has to follow the fork via
// PTRACE_O_TRACEFORK/CLONE and capture the leaf's execve.
// Skipped on non-amd64 Linux where the native backend isn't
// compiled in.
func TestBuildTracer_NativeCapturesForkedExecve(t *testing.T) {
	if _, err := exec.LookPath("strace"); err != nil {
		// Test relies on `go build` + ptrace working on the
		// host. strace's presence approximates "this kernel
		// allows ptrace from a parent." Skip if absent.
		t.Skip("strace not on PATH; gating native test on the same host capability")
	}
	tmp := t.TempDir()
	bin := filepath.Join(tmp, "build-tracer")
	out := filepath.Join(tmp, "trace.log")

	build := exec.Command("go", "build", "-o", bin, ".")
	build.Dir = mustDir(t)
	if err := build.Run(); err != nil {
		t.Fatalf("go build: %v", err)
	}

	cmd := exec.Command(bin, "--out="+out, "--", "/bin/sh", "-c", "/bin/true")
	if err := cmd.Run(); err != nil {
		t.Fatalf("build-tracer run: %v", err)
	}
	body, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	// Both the wrapping shell AND the forked leaf should
	// appear. The leaf's execve is what proves fork-following
	// works (we synthesize the wrapping shell's exec from
	// cmd.Args at startup; the leaf can only come from a
	// PTRACE_EVENT_FORK + child syscall stop).
	for _, want := range []string{"execve(\"/bin/sh\"", "execve(\"/bin/true\""} {
		if !strings.Contains(string(body), want) {
			t.Errorf("native trace missing %q\n--body--\n%s", want, body)
		}
	}
}

// TestBuildTracer_StraceFallback confirms the strace shim is
// reachable via --strace, so non-amd64 hosts (or ones where
// the native backend has issues) have a working fallback.
func TestBuildTracer_StraceFallback(t *testing.T) {
	if _, err := exec.LookPath("strace"); err != nil {
		t.Skip("strace not on PATH; skipping")
	}
	tmp := t.TempDir()
	bin := filepath.Join(tmp, "build-tracer")
	out := filepath.Join(tmp, "trace.log")

	build := exec.Command("go", "build", "-o", bin, ".")
	build.Dir = mustDir(t)
	if err := build.Run(); err != nil {
		t.Fatalf("go build: %v", err)
	}
	cmd := exec.Command(bin, "--strace", "--out="+out, "--", "/bin/true")
	if err := cmd.Run(); err != nil {
		t.Fatalf("build-tracer --strace run: %v", err)
	}
	body, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `execve("/bin/true"`) {
		t.Errorf("strace-fallback trace missing /bin/true execve\n--body--\n%s", body)
	}
}

// TestBuildTracer_PropagatesExit confirms a non-zero exit from
// the wrapped command surfaces from build-tracer too. ptrace
// permissions can suppress this on hardened sandboxes; the
// test skips when the strace invocation itself fails for
// non-exit reasons.
func TestBuildTracer_PropagatesExit(t *testing.T) {
	if _, err := exec.LookPath("strace"); err != nil {
		t.Skip("strace not on PATH; skipping")
	}

	tmp := t.TempDir()
	bin := filepath.Join(tmp, "build-tracer")
	out := filepath.Join(tmp, "trace.log")

	build := exec.Command("go", "build", "-o", bin, ".")
	build.Dir = mustDir(t)
	if err := build.Run(); err != nil {
		t.Fatalf("go build: %v", err)
	}

	cmd := exec.Command(bin, "--out="+out, "--", "/bin/false")
	err := cmd.Run()
	if err == nil {
		t.Fatalf("expected non-zero exit; got nil")
	}
	if ee, ok := err.(*exec.ExitError); ok {
		if ee.ExitCode() == 0 {
			t.Errorf("expected non-zero exit; got 0")
		}
	}
}

// TestBuildTracer_SourceRootCapturesOpenatNative verifies that
// --source-root opts the native backend into capturing openat
// reads, that canonicalize rewrites the path source-relative,
// and that the volatile fd return value gets stripped. End-to-
// end exercise of the trace-side configure-time read oracle.
func TestBuildTracer_SourceRootCapturesOpenatNative(t *testing.T) {
	if _, err := exec.LookPath("strace"); err != nil {
		t.Skip("strace not on PATH; gating native test on host ptrace capability")
	}
	tmp := t.TempDir()
	bin := filepath.Join(tmp, "build-tracer")
	out := filepath.Join(tmp, "trace.log")
	srcRoot := filepath.Join(tmp, "src")
	if err := os.Mkdir(srcRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	probePath := filepath.Join(srcRoot, "probe.txt")
	if err := os.WriteFile(probePath, []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	build := exec.Command("go", "build", "-o", bin, ".")
	build.Dir = mustDir(t)
	if err := build.Run(); err != nil {
		t.Fatalf("go build: %v", err)
	}

	// /bin/cat opens the probe file via openat(AT_FDCWD, ...).
	// With --source-root pointing at our tmp src dir, the
	// canonical trace should carry the rewritten path
	// "probe.txt" with the fd suffix replaced by `?`.
	cmd := exec.Command(bin, "--source-root="+srcRoot, "--out="+out, "--", "/bin/cat", probePath)
	cmd.Stdout = nil
	if err := cmd.Run(); err != nil {
		t.Fatalf("build-tracer run: %v", err)
	}
	body, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	got := string(body)
	if !strings.Contains(got, `openat(AT_FDCWD, "probe.txt"`) {
		t.Errorf("trace missing source-relative openat for probe.txt\n--body--\n%s", got)
	}
	if !strings.Contains(got, " = ?") {
		t.Errorf("trace missing fd-stripped suffix `= ?`\n--body--\n%s", got)
	}
	// Out-of-tree opens (libc.so, /etc/passwd, etc.) must not
	// appear — the canonicalize filter drops them.
	if strings.Contains(got, "libc.so") || strings.Contains(got, "/etc/") {
		t.Errorf("trace leaked out-of-tree openat lines\n--body--\n%s", got)
	}
}

// TestBuildTracer_SourceRootCapturesOpenatStrace mirrors the
// native test against the --strace fallback path.
func TestBuildTracer_SourceRootCapturesOpenatStrace(t *testing.T) {
	if _, err := exec.LookPath("strace"); err != nil {
		t.Skip("strace not on PATH; skipping")
	}
	tmp := t.TempDir()
	bin := filepath.Join(tmp, "build-tracer")
	out := filepath.Join(tmp, "trace.log")
	srcRoot := filepath.Join(tmp, "src")
	if err := os.Mkdir(srcRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	probePath := filepath.Join(srcRoot, "probe.txt")
	if err := os.WriteFile(probePath, []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	build := exec.Command("go", "build", "-o", bin, ".")
	build.Dir = mustDir(t)
	if err := build.Run(); err != nil {
		t.Fatalf("go build: %v", err)
	}

	cmd := exec.Command(bin, "--strace", "--source-root="+srcRoot, "--out="+out, "--", "/bin/cat", probePath)
	cmd.Stdout = nil
	if err := cmd.Run(); err != nil {
		t.Fatalf("build-tracer --strace run: %v", err)
	}
	body, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	got := string(body)
	if !strings.Contains(got, `openat(AT_FDCWD, "probe.txt"`) {
		t.Errorf("strace-fallback trace missing source-relative openat for probe.txt\n--body--\n%s", got)
	}
	if !strings.Contains(got, " = ?") {
		t.Errorf("strace-fallback trace missing fd-stripped suffix `= ?`\n--body--\n%s", got)
	}
}

// TestBuildTracer_NoSourceRootSkipsOpenat confirms the legacy
// AC byte schema: without --source-root, openat events are
// dropped entirely so existing AC entries (computed against
// execve-only traces) remain valid.
func TestBuildTracer_NoSourceRootSkipsOpenat(t *testing.T) {
	if _, err := exec.LookPath("strace"); err != nil {
		t.Skip("strace not on PATH; skipping")
	}
	tmp := t.TempDir()
	bin := filepath.Join(tmp, "build-tracer")
	out := filepath.Join(tmp, "trace.log")
	probePath := filepath.Join(tmp, "probe.txt")
	if err := os.WriteFile(probePath, []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	build := exec.Command("go", "build", "-o", bin, ".")
	build.Dir = mustDir(t)
	if err := build.Run(); err != nil {
		t.Fatalf("go build: %v", err)
	}

	cmd := exec.Command(bin, "--out="+out, "--", "/bin/cat", probePath)
	cmd.Stdout = nil
	if err := cmd.Run(); err != nil {
		t.Fatalf("build-tracer run: %v", err)
	}
	body, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "openat(") {
		t.Errorf("legacy mode leaked openat into trace.log\n--body--\n%s", body)
	}
}

func mustDir(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	return wd
}
