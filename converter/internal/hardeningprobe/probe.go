// Package hardeningprobe detects distro-default compiler hardening
// flags that cmake silently inherits from the host cc's spec file
// (Debian / Ubuntu, RHEL, etc.) but Bazel's hermetic cc_toolchain
// does NOT reproduce by default.
//
// Surfaced by the convert-and-build artifact comparison of zlib:
// cmake's libz.a referenced __snprintf_chk / __vsnprintf_chk /
// __stack_chk_fail; Bazel's libzlibstatic.a from the converted
// BUILD.bazel did not. cmake invoked /usr/bin/cc directly, which
// applies -D_FORTIFY_SOURCE=2 -fstack-protector-strong via the
// distro spec file even when no CFLAGS env var is set; Bazel's
// hermetic cc_toolchain has no equivalent default.
//
// The probe compiles a tiny stub with the host cc, scans the
// resulting object file's undefined-symbol set, and infers which
// hardening flags were applied. Operators get a stderr warning
// naming the specific flags + a remediation recipe so the
// downstream `bazel build` output matches cmake's at the symbol-
// reference level (modulo the inherent toolchain delta).
//
// Diagnostic-only: no BUILD.bazel emit-side behaviour changes.
// The closure of the actual flag delta is a cc_toolchain feature
// definition (separate, larger PR).
package hardeningprobe

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Result is the structured probe output. Empty Flags means no
// distro hardening defaults were detected — the convert host's
// cc behaves like Bazel's hermetic toolchain (typical for
// hand-rolled / cross-compile toolchains).
type Result struct {
	// CC is the compiler path the probe invoked.
	CC string

	// Flags maps the inferred hardening flag (e.g.
	// "-D_FORTIFY_SOURCE=2", "-fstack-protector-strong") to the
	// observed-symbol evidence (e.g. "__snprintf_chk").
	Flags map[string]string

	// Err carries any structured error from the probe path.
	// Skipped probes (cc not on PATH, compile failure) return a
	// nil Result + non-nil Err so the caller can surface or
	// ignore.
	Err error
}

// Empty reports whether the probe found any hardening defaults.
func (r *Result) Empty() bool { return r == nil || len(r.Flags) == 0 }

// FormatForOperator renders the result as a multi-line stderr
// warning. Recipe-bearing: each detected flag carries the
// remediation suggestion ("apply -D_FORTIFY_SOURCE=2 to BUILD
// copts" or equivalent toolchain feature wiring).
func (r *Result) FormatForOperator() string {
	if r.Empty() {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "convert-host cc (%s) applies distro hardening defaults that Bazel's\n", r.CC)
	fmt.Fprintf(&b, "hermetic cc_toolchain does NOT reproduce by default; expect symbol-set\n")
	fmt.Fprintf(&b, "deltas between the cmake-produced artifact and the Bazel-rebuilt one:\n")
	// Stable order.
	keys := make([]string, 0, len(r.Flags))
	for k := range r.Flags {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(&b, "  - %s (evidence: %s)\n", k, r.Flags[k])
	}
	fmt.Fprintf(&b, "To match cmake's output under Bazel, either add these to copts on the\n")
	fmt.Fprintf(&b, "affected rules, or wire them as features on the converted cc_toolchain\n")
	fmt.Fprintf(&b, "(latter preferred — keeps BUILD.bazel hermetic and host-state-free).\n")
	return b.String()
}

// FlagSummary renders the detected hardening flags as a compact,
// stable-ordered comma-joined string (e.g.
// "-D_FORTIFY_SOURCE=2, -fstack-protector-strong") for a one-line
// operator note. Empty result → "none".
func (r *Result) FlagSummary() string {
	if r.Empty() {
		return "none"
	}
	keys := make([]string, 0, len(r.Flags))
	for k := range r.Flags {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return strings.Join(keys, ", ")
}

// Probe compiles a tiny C stub with the given cc and infers
// distro hardening defaults from the resulting object file's
// undefined-symbol set. ccPath defaults to "cc" when empty.
//
// 10-second hard timeout. Errors are returned in Result.Err
// rather than panicking — the convert pipeline shouldn't fail
// because a probe couldn't run.
func Probe(ccPath string) *Result {
	if ccPath == "" {
		ccPath = "cc"
	}
	resolved, err := exec.LookPath(ccPath)
	if err != nil {
		return &Result{CC: ccPath, Err: fmt.Errorf("cc not found on PATH: %w", err)}
	}

	tmpDir, err := os.MkdirTemp("", "hardening-probe-*")
	if err != nil {
		return &Result{CC: resolved, Err: fmt.Errorf("mktmpdir: %w", err)}
	}
	defer os.RemoveAll(tmpDir)

	srcPath := filepath.Join(tmpDir, "probe.c")
	// The stub exercises three classic hardening triggers:
	//   - sprintf into a fixed buffer (FORTIFY's _chk wrapper)
	//   - a small local-buffer write (stack-protector canary)
	//   - vsnprintf via varargs (FORTIFY again, separate symbol)
	src := `
#include <stdio.h>
#include <stdarg.h>
#include <string.h>

void probe_sprintf(char *out, const char *s) {
    char buf[64];
    sprintf(buf, "%s", s);
    strcpy(out, buf);
}

void probe_vsnprintf(char *out, size_t n, const char *fmt, ...) {
    va_list ap;
    va_start(ap, fmt);
    vsnprintf(out, n, fmt, ap);
    va_end(ap);
}
`
	if err := os.WriteFile(srcPath, []byte(src), 0o644); err != nil {
		return &Result{CC: resolved, Err: fmt.Errorf("write probe.c: %w", err)}
	}
	objPath := filepath.Join(tmpDir, "probe.o")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// -O2 is the Release-ish optimization level distro CFLAGS
	// typically target; matches what cmake invokes for
	// CMAKE_BUILD_TYPE=Release.
	var stderr bytes.Buffer
	cc := exec.CommandContext(ctx, resolved, "-O2", "-c", srcPath, "-o", objPath)
	cc.Stderr = &stderr
	if err := cc.Run(); err != nil {
		return &Result{CC: resolved, Err: fmt.Errorf("compile probe.c failed: %w (stderr: %s)", err, stderr.String())}
	}

	// Read the .o file's undefined-symbol references via nm.
	nmCtx, nmCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer nmCancel()
	var nmOut bytes.Buffer
	nm := exec.CommandContext(nmCtx, "nm", "-u", objPath)
	nm.Stdout = &nmOut
	if err := nm.Run(); err != nil {
		return &Result{CC: resolved, Err: fmt.Errorf("nm -u failed: %w", err)}
	}

	return &Result{CC: resolved, Flags: classifyHardeningSymbols(nmOut.String())}
}

// classifyHardeningSymbols maps `nm -u` output to a flag→evidence
// map. Each detected symbol pattern names the most-likely
// cc-line flag that produced it.
func classifyHardeningSymbols(nmOutput string) map[string]string {
	flags := map[string]string{}
	// Patterns matched against the symbol name (post-whitespace).
	// FORTIFY's _chk family covers sprintf / snprintf / vsnprintf
	// / memcpy / strcpy / etc.; presence of any _chk reference
	// implies -D_FORTIFY_SOURCE >= 1 (the level is hard to infer
	// from symbols alone; the level=2 form is the common default).
	for _, line := range strings.Split(nmOutput, "\n") {
		tok := strings.TrimSpace(line)
		if tok == "" {
			continue
		}
		// nm format: "                 U __symbol_name" (the
		// leading address column is empty for undefined symbols).
		// Walk to the last whitespace-separated token.
		fields := strings.Fields(tok)
		if len(fields) == 0 {
			continue
		}
		sym := fields[len(fields)-1]
		switch {
		case strings.HasSuffix(sym, "_chk"):
			if _, ok := flags["-D_FORTIFY_SOURCE=2"]; !ok {
				flags["-D_FORTIFY_SOURCE=2"] = sym
			}
		case sym == "__stack_chk_fail" || sym == "__stack_chk_guard":
			if _, ok := flags["-fstack-protector-strong"]; !ok {
				flags["-fstack-protector-strong"] = sym
			}
		}
	}
	return flags
}
