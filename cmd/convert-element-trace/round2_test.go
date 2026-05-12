package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sstriker/cmake-to-bazel/converter/ir"
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

// TestRound2_OutIRJSONEmptyTraceDirEmitsEmptyPackage covers the
// declared-outputs invariant for the multi-platform fold: when
// --out-ir-json is set, the converter must produce that file even
// in the round-2 boot phase (no published trace), or the
// per-platform fan-out's Bazel action fails on missing output.
// An empty ir.Package mirrors the placeholder BUILD.bazel.out's
// "no recoverable targets" semantic — fold-element composes
// empties to empty.
func TestRound2_OutIRJSONEmptyTraceDirEmitsEmptyPackage(t *testing.T) {
	bin := buildSelf(t)
	tmp := t.TempDir()
	outBuild := filepath.Join(tmp, "BUILD.bazel.out")
	outIR := filepath.Join(tmp, "nested", "ir.json")
	cmd := exec.Command(bin,
		"--trace-dir", filepath.Join(tmp, "absent"),
		"--out-build", outBuild,
		"--out-ir-json", outIR,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("converter: %v\n%s", err, out)
	}
	body, err := os.ReadFile(outIR)
	if err != nil {
		t.Fatalf("read ir.json: %v", err)
	}
	var pkg ir.Package
	if err := json.Unmarshal(body, &pkg); err != nil {
		t.Fatalf("unmarshal ir.json: %v\n%s", err, body)
	}
	if len(pkg.Targets) != 0 {
		t.Errorf("boot-phase ir.json: want 0 targets, got %d", len(pkg.Targets))
	}
}

// TestRound2_OutIRJSONFineModeEmitsRecoveredTargets verifies the
// happy path: a populated trace.log produces an ir.Package whose
// Targets match the recovered cc_binary/cc_library — the same
// rules emitBuild renders to BUILD.bazel.out, just shipped as IR
// so fold-element can compose multiple platforms' outputs.
func TestRound2_OutIRJSONFineModeEmitsRecoveredTargets(t *testing.T) {
	bin := buildSelf(t)
	tmp := t.TempDir()
	traceDir := filepath.Join(tmp, "trace")
	if err := os.MkdirAll(traceDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(traceDir, "trace.log"),
		[]byte(`execve("/usr/bin/cc", ["cc", "-o", "greet", "greet.c"], 0x0) = 0`+"\n"),
		0o644); err != nil {
		t.Fatal(err)
	}
	outBuild := filepath.Join(tmp, "BUILD.bazel.out")
	outIR := filepath.Join(tmp, "ir.json")
	cmd := exec.Command(bin,
		"--trace-dir", traceDir,
		"--out-build", outBuild,
		"--out-ir-json", outIR,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("converter: %v\n%s", err, out)
	}
	body, err := os.ReadFile(outIR)
	if err != nil {
		t.Fatalf("read ir.json: %v", err)
	}
	var pkg ir.Package
	if err := json.Unmarshal(body, &pkg); err != nil {
		t.Fatalf("unmarshal ir.json: %v\n%s", err, body)
	}
	if len(pkg.Targets) != 1 {
		t.Fatalf("ir.json: want 1 target, got %d (%+v)", len(pkg.Targets), pkg.Targets)
	}
	got := pkg.Targets[0]
	if got.Kind != ir.KindCCBinary {
		t.Errorf("ir.json: target.Kind=%v, want %v", got.Kind, ir.KindCCBinary)
	}
	if got.Name != "greet" {
		t.Errorf("ir.json: target.Name=%q, want greet", got.Name)
	}
	if len(got.Srcs) != 1 || got.Srcs[0] != "greet.c" {
		t.Errorf("ir.json: target.Srcs=%v, want [greet.c]", got.Srcs)
	}
}

// buildSelf compiles the convert-element-trace binary into the
// test's tempdir. Used by tests that exercise the binary's flag
// handling end-to-end (the in-process tests cover the parser /
// emitter; these cover the dispatch layer).
func buildSelf(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()
	bin := filepath.Join(tmp, "convert-element-trace")
	cmd := exec.Command("go", "build", "-o", bin, ".")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	return bin
}
