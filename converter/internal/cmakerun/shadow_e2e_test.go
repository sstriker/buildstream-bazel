//go:build e2e

package cmakerun_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/emit/bazel"
	"github.com/sstriker/buildstream-bazel/converter/internal/cmakerun"
	"github.com/sstriker/buildstream-bazel/converter/internal/fileapi"
	"github.com/sstriker/buildstream-bazel/converter/internal/lower"
	"github.com/sstriker/buildstream-bazel/internal/shadow"
)

// TestE2E_HelloWorld_ShadowTree validates the architectural keystone: running
// cmake against the path-only shadow tree must produce a BUILD.bazel
// byte-identical to running it against the real tree. If they diverge the
// shadow allowlist is missing a file cmake actually reads — emit a triage
// hint pointing the operator at trace.jsonl.
func TestE2E_HelloWorld_ShadowTree(t *testing.T) {
	realSrc, err := filepath.Abs("../../testdata/sample-projects/hello-world")
	if err != nil {
		t.Fatal(err)
	}

	realOut := runConvert(t, realSrc, false)
	t.Logf("real-tree BUILD.bazel:\n%s", realOut)

	// Build the shadow tree.
	shadowSrc := filepath.Join(t.TempDir(), "shadow")
	if err := shadow.Build(realSrc, shadowSrc, shadow.DefaultAllowlist()); err != nil {
		t.Fatalf("shadow.Build: %v", err)
	}

	shadowOut := runConvert(t, shadowSrc, true)

	// Scrub absolute source paths before comparison: realSrc and shadowSrc
	// differ, but both should reduce to a stable form.
	realOut = []byte(strings.ReplaceAll(string(realOut), realSrc, "<SRC>"))
	shadowOut = []byte(strings.ReplaceAll(string(shadowOut), shadowSrc, "<SRC>"))

	if string(realOut) != string(shadowOut) {
		t.Errorf("shadow-tree output diverges from real-tree output\n--- real ---\n%s\n--- shadow ---\n%s", realOut, shadowOut)
	}
}

// runConvert spins a fresh build dir, runs cmake, lowers the reply, and
// emits BUILD.bazel. Returns the emitter output. If trace is true, cmake's
// --trace-redirect output is captured and the extracted source-relative
// read paths are logged for triage.
func runConvert(t *testing.T, src string, trace bool) []byte {
	t.Helper()
	buildDir := t.TempDir()
	tracePath := ""
	if trace {
		tracePath = filepath.Join(buildDir, "trace.jsonl")
	}

	reply, err := cmakerun.Configure(t.Context(), cmakerun.Options{
		SourceRoot: src,
		BuildDir:   buildDir,
		TracePath:  tracePath,
		Stdout:     testWriter{t},
		Stderr:     testWriter{t},
	})
	if err != nil {
		t.Fatalf("Configure: %v", err)
	}

	if tracePath != "" {
		raw, err := os.ReadFile(tracePath)
		if err != nil {
			t.Logf("trace not produced at %s: %v", tracePath, err)
		} else {
			reads := shadow.ExtractReadPaths(raw, src)
			t.Logf("trace read paths under %s: %v", src, reads)
		}
	}

	r, err := fileapi.Load(reply.Path)
	if err != nil {
		t.Fatalf("fileapi.Load: %v", err)
	}
	pkg, err := lower.ToIR(r, nil, lower.Options{HostSourceRoot: src})
	if err != nil {
		t.Fatalf("ToIR: %v", err)
	}
	out, err := bazel.Emit(pkg)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	return out
}
