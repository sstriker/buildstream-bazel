package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeCCHashBin stages a fake cc-hash binary and returns its path, so
// stageCCHashTool has a real file to copy.
func fakeCCHashBin(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "cc-hash")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin
}

// TestWriter_CCHash_LiftWired covers --cc-hash-bin: the converter genrule
// threads --lift-cc-hash=true, and the binary stages into project A AND
// project B's tools/ and is exported, so the cc_hash rule's //tools:cc-hash
// tool label resolves downstream wherever the rule lands. Mirrors
// TestWriter_CCEmbed_LiftWired (reuses its both-projects render helper).
func TestWriter_CCHash_LiftWired(t *testing.T) {
	prev := cmakeConfig
	cmakeConfig.ccHashBin = fakeCCHashBin(t)
	t.Cleanup(func() { cmakeConfig = prev })

	r := renderCmakeProjectWithCCEmbed(t)

	if !strings.Contains(r.genrule, "--lift-cc-hash=true") {
		t.Errorf("converter genrule missing --lift-cc-hash=true:\n%s", r.genrule)
	}
	for _, p := range []struct {
		side, tools, out string
	}{
		{"A", r.toolsA, r.outA},
		{"B", r.toolsB, r.outB},
	} {
		if !strings.Contains(p.tools, `"cc-hash"`) {
			t.Errorf("project %s tools/BUILD.bazel missing cc-hash export:\n%s", p.side, p.tools)
		}
		info, err := os.Stat(filepath.Join(p.out, "tools", "cc-hash"))
		if err != nil {
			t.Fatalf("cc-hash not staged into project %s tools/: %v", p.side, err)
		}
		if info.Mode().Perm()&0o111 == 0 {
			t.Errorf("staged cc-hash in project %s is not executable: mode %v", p.side, info.Mode())
		}
	}
}

// TestWriter_CCHash_OffShapeUnchanged pins that the default (flag off) render
// threads no --lift-cc-hash and stages no cc-hash tool — the byte-shape
// guarantee for the untouched path.
func TestWriter_CCHash_OffShapeUnchanged(t *testing.T) {
	prev := cmakeConfig
	cmakeConfig.ccHashBin = ""
	t.Cleanup(func() { cmakeConfig = prev })

	r := renderCmakeProjectWithCCEmbed(t)

	if strings.Contains(r.genrule, "lift-cc-hash") {
		t.Errorf("converter genrule unexpectedly mentions lift-cc-hash with flag off:\n%s", r.genrule)
	}
	for _, p := range []struct {
		side, tools, out string
	}{
		{"A", r.toolsA, r.outA},
		{"B", r.toolsB, r.outB},
	} {
		if strings.Contains(p.tools, `"cc-hash"`) {
			t.Errorf("project %s tools/BUILD.bazel unexpectedly exports cc-hash with flag off:\n%s", p.side, p.tools)
		}
		if _, err := os.Stat(filepath.Join(p.out, "tools", "cc-hash")); !os.IsNotExist(err) {
			t.Errorf("project %s staged a cc-hash tool with flag off (err=%v)", p.side, err)
		}
	}
}
