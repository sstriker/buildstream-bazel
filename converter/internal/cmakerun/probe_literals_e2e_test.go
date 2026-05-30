//go:build e2e

package cmakerun_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/internal/cmakerun"
)

// TestE2E_LiteralProbe_WarmSecondPass proves the generalized genex
// literal probe end-to-end through the real Configure API: a first
// (cold) configure warms the build dir, then a second configure
// against the SAME build dir — carrying a LiteralProbes request for
// an ARBITRARY literal outside the structural probe's fixed set
// ($<TARGET_PROPERTY:app,CUSTOM_PROP>) — resolves it via cmake's own
// evaluator. This is the warm-second-pass contract the converter's
// two-pass genex resolution rests on.
func TestE2E_LiteralProbe_WarmSecondPass(t *testing.T) {
	src := t.TempDir()
	writeFile(t, filepath.Join(src, "CMakeLists.txt"), `
cmake_minimum_required(VERSION 3.20)
project(litprobe LANGUAGES C)
add_executable(app main.c)
set_target_properties(app PROPERTIES CUSTOM_PROP "hello-custom-42")
`)
	writeFile(t, filepath.Join(src, "main.c"), "int main(void){return 0;}\n")

	buildDir := t.TempDir()

	// Pass 1: cold configure, warms the cache.
	if _, err := cmakerun.Configure(t.Context(), cmakerun.Options{
		SourceRoot: src,
		BuildDir:   buildDir,
		Stdout:     testWriter{t},
		Stderr:     testWriter{t},
	}); err != nil {
		t.Fatalf("pass 1 Configure: %v", err)
	}

	// Pass 2: warm reconfigure of the SAME build dir, injecting the
	// literal probe for an arbitrary literal the structural probe
	// does not cover.
	req := cmakerun.LiteralProbeRequest{
		Literal: "$<TARGET_PROPERTY:app,CUSTOM_PROP>",
		Target:  "app",
	}
	if _, err := cmakerun.Configure(t.Context(), cmakerun.Options{
		SourceRoot:    src,
		BuildDir:      buildDir,
		LiteralProbes: []cmakerun.LiteralProbeRequest{req},
		Stdout:        testWriter{t},
		Stderr:        testWriter{t},
	}); err != nil {
		t.Fatalf("pass 2 Configure: %v", err)
	}

	got, err := cmakerun.ReadLiteralProbe(buildDir)
	if err != nil {
		t.Fatalf("ReadLiteralProbe: %v", err)
	}
	res, ok := got[req.Hash()]
	if !ok {
		t.Fatalf("no resolution for %q (hash %s); got keys %v", req.Literal, req.Hash(), keysOf(got))
	}
	val, unified := res.Unified()
	if !unified {
		t.Fatalf("expected single-config unified value, got per-config %v", res.PerConfig)
	}
	if val != "hello-custom-42" {
		t.Fatalf("resolved %q, want hello-custom-42", val)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func keysOf(m map[string]cmakerun.LiteralResolution) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
