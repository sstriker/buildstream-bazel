// build-tracer is the in-action process tracer for the
// trace-driven autotools-to-Bazel converter (see
// docs/architecture.md). Wraps a build invocation
// in a process tracer; the resulting trace artifact is what
// convert-element-trace reads to recover Bazel targets.
//
// Two backends:
//
//   - Default: native ptrace (linux/amd64). Forks the build
//     command with PTRACE_TRACEME, follows fork/vfork/clone,
//     captures every successful execve's argv via PTRACE_PEEKDATA.
//     Output mirrors strace's text format so the converter
//     parser is unchanged.
//   - Fallback: strace shim (any platform). When --strace is
//     passed, invokes the host strace; useful on platforms
//     where the native backend isn't available, or for
//     comparison testing.
//
// Usage:
//
//	build-tracer --out=<trace.log> -- <cmd> [args...]
//	build-tracer --strace --out=<trace.log> -- <cmd> [args...]
//
// Exits with the wrapped command's exit status. The trace
// artifact is written even if the build fails, so failure
// modes that surface during recovery (link errors against
// cross-element libs, missing source files in the staged
// tree, etc.) can be inspected post-hoc.
package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// prefixSubFlag is a flag.Value collecting repeatable
// `--normalize-prefix=FROM=TO` flags. Each invocation pushes
// one substitution; canonicalize applies them per line.
//
// FROM is typically the value of an action-time mktemp
// (`$INSTALL_ROOT`, `$BUILD_ROOT`, `$DEP_PREFIX`) — paths
// whose bytes vary across bazel invocations even when the
// underlying build is identical. TO is a stable placeholder
// the trace embeds instead.
type prefixSubFlag []prefixSub

func (p *prefixSubFlag) String() string {
	parts := make([]string, len(*p))
	for i, s := range *p {
		parts[i] = s.From + "=" + s.To
	}
	return strings.Join(parts, ",")
}

func (p *prefixSubFlag) Set(s string) error {
	idx := strings.Index(s, "=")
	if idx <= 0 {
		return fmt.Errorf("--normalize-prefix expects FROM=TO; got %q", s)
	}
	*p = append(*p, prefixSub{From: s[:idx], To: s[idx+1:]})
	return nil
}

func main() {
	out := flag.String("out", "", "path to write the trace artifact (canonical strace text format — pids stripped, gcc temp paths replaced with stable placeholders)")
	useStrace := flag.Bool("strace", false, "use the host strace binary instead of the native ptrace backend (fallback for non-linux/amd64 hosts)")
	sourceRoot := flag.String("source-root", "", "absolute path to the element's source tree. When set, the tracer also captures openat reads (in addition to execve), and canonicalize filters them to source-relative paths with the volatile fd return value stripped — the trace doubles as a configure-time read oracle (see internal/tracenorm package doc). When empty, openat events are skipped entirely; trace.log byte schema matches the legacy execve-only shape and existing AC entries stay valid.")
	var prefixSubs prefixSubFlag
	flag.Var(&prefixSubs, "normalize-prefix", "FROM=TO substitution applied to every trace line. Repeatable. Used to neutralize action-time mktemp paths (INSTALL_ROOT / BUILD_ROOT / DEP_PREFIX) whose bytes vary across bazel invocations.")
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: build-tracer [--strace] [--source-root=PATH] [--normalize-prefix=FROM=TO ...] --out=<path> -- <cmd> [args...]")
		flag.PrintDefaults()
	}
	flag.Parse()

	if *out == "" {
		flag.Usage()
		os.Exit(2)
	}
	args := flag.Args()
	if len(args) == 0 {
		flag.Usage()
		os.Exit(2)
	}

	// Both backends write the raw strace-format trace to a
	// temp file; canonicalize() rewrites it into the form
	// Bazel hashes (pid-stripped, stable temp-path placeholders)
	// at --out. Done here in main so both backends benefit
	// from the same normalization.
	rawFile, err := os.CreateTemp("", "build-tracer-raw-*.log")
	if err != nil {
		fmt.Fprintf(os.Stderr, "build-tracer: tempfile: %v\n", err)
		os.Exit(1)
	}
	rawPath := rawFile.Name()
	rawFile.Close()
	defer os.Remove(rawPath)

	var exitCode int
	if *useStrace || !nativeBackendAvailable() {
		exitCode = runStrace(rawPath, args, *sourceRoot != "")
	} else {
		exitCode = runNative(rawPath, args, *sourceRoot != "")
	}

	// Canonicalize even on non-zero exit — partial traces from
	// failing builds are still useful for post-mortem.
	if err := canonicalizeWith(rawPath, *out, []prefixSub(prefixSubs), *sourceRoot); err != nil {
		fmt.Fprintf(os.Stderr, "build-tracer: canonicalize: %v\n", err)
		if exitCode == 0 {
			exitCode = 1
		}
	}
	os.Exit(exitCode)
}

// runStrace invokes the host strace binary as a thin wrapper
// around the build command. Used as the fallback backend when
// the native ptrace path isn't available.
//
// captureReads = true broadens the syscall set to include
// openat events alongside execve. Strace itself does not filter
// openat by path or by access mode; canonicalize (called
// downstream with the same source-root) drops openat lines
// outside the source tree and strips the volatile fd return
// value. The resulting canonical trace doubles as a configure-
// time read oracle.
func runStrace(out string, args []string, captureReads bool) int {
	traceSet := "execve"
	if captureReads {
		traceSet = "execve,openat"
	}
	straceArgs := []string{
		"-f", // follow forks
		"-e", "trace=" + traceSet,
		"-s", "4096", // long enough for argv strings
		"--signal=none", // skip signal noise
		"-o", out,       // trace destination
		"--",
	}
	straceArgs = append(straceArgs, args...)

	cmd := exec.Command("strace", straceArgs...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	cmd.Env = os.Environ()
	if err := cmd.Run(); err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return ee.ExitCode()
		}
		fmt.Fprintf(os.Stderr, "build-tracer: %v\n", err)
		return 1
	}
	return 0
}
