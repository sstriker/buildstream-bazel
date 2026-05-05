package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestRound2_EmptyTraceDirEmitsPlaceholder verifies the boot-phase
// signal: with --trace-dir pointing at a non-existent / empty dir,
// the converter writes a placeholder BUILD.bazel.out (so the
// project-A converter genrule's declared output exists) and exits
// cleanly. Project B's coarse install genrule remains the
// buildable target; the converter doesn't try to fabricate fine-
// grained cc rules from no input.
func TestRound2_EmptyTraceDirEmitsPlaceholder(t *testing.T) {
	bin := buildSelf(t)
	tmp := t.TempDir()
	outBuild := filepath.Join(tmp, "BUILD.bazel.out")
	cmd := exec.Command(bin, "--trace-dir", filepath.Join(tmp, "absent"), "--out-build", outBuild)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("converter: %v\n%s", err, out)
	}
	body, err := os.ReadFile(outBuild)
	if err != nil {
		t.Fatalf("read BUILD.bazel.out: %v", err)
	}
	for _, want := range []string{
		"Round-2 boot phase",
		"no trace published",
	} {
		if !strings.Contains(string(body), want) {
			t.Errorf("placeholder missing %q\n%s", want, body)
		}
	}
	for _, banned := range []string{"cc_library", "cc_binary"} {
		if strings.Contains(string(body), banned) {
			t.Errorf("placeholder unexpectedly contains %q", banned)
		}
	}
}

// TestRound2_PopulatedTraceDirRunsFineMode wires up a synthetic
// trace.log in --trace-dir and verifies the converter dispatches
// to the existing fine-mode parse path (cc_binary in the output
// for a link event).
func TestRound2_PopulatedTraceDirRunsFineMode(t *testing.T) {
	bin := buildSelf(t)
	tmp := t.TempDir()
	traceDir := filepath.Join(tmp, "trace")
	if err := os.MkdirAll(traceDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Minimal trace: one cc compile-and-link event.
	if err := os.WriteFile(filepath.Join(traceDir, "trace.log"),
		[]byte(`execve("/usr/bin/cc", ["cc", "-o", "greet", "greet.c"], 0x0) = 0`+"\n"),
		0o644); err != nil {
		t.Fatal(err)
	}
	outBuild := filepath.Join(tmp, "BUILD.bazel.out")
	cmd := exec.Command(bin, "--trace-dir", traceDir, "--out-build", outBuild)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("converter: %v\n%s", err, out)
	}
	body, err := os.ReadFile(outBuild)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`cc_binary(`,
		`name = "greet"`,
		`srcs = ["greet.c"]`,
	} {
		if !strings.Contains(string(body), want) {
			t.Errorf("fine-mode output missing %q\n%s", want, body)
		}
	}
}

// buildSelf compiles the convert-element-autotools binary into the
// test's tempdir. Used by tests that exercise the binary's flag
// handling end-to-end (the in-process tests cover the parser /
// emitter; these cover the dispatch layer).
func buildSelf(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()
	bin := filepath.Join(tmp, "convert-element-autotools")
	cmd := exec.Command("go", "build", "-o", bin, ".")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	return bin
}
