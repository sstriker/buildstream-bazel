package lower

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/internal/ninja"
	"github.com/sstriker/buildstream-bazel/internal/shadow"
)

// TestWriteInPlaceProducer pins the producer selection: the single liftable tool
// whose WORKING_DIRECTORY anchors to the declared outputs' dir wins; no match,
// a non-anchoring workdir, or an ambiguous second producer declines.
func TestWriteInPlaceProducer(t *testing.T) {
	build := t.TempDir()
	src := t.TempDir()
	anc := execAnchors{hostSrcDir: src, recordedSrcDir: src, hostBuildDir: build, recordedBuildDir: build}
	cc := &codegenContext{}
	outDir := filepath.Join(build, "gen")

	call := func(workdir string, argv ...string) shadow.ExecuteProcessCall {
		return shadow.ExecuteProcessCall{Commands: [][]string{argv}, WorkingDirectory: workdir}
	}

	// A single tool whose WORKING_DIRECTORY == the output dir is selected.
	argv, wd, ok := cc.writeInPlaceProducer(
		[]shadow.ExecuteProcessCall{call(outDir, "python3", filepath.Join(src, "tool.py"))},
		anc, "gen")
	if !ok || wd != outDir || len(argv) != 2 || argv[0] != "python3" {
		t.Fatalf("expected the in-place tool selected; got argv=%v wd=%q ok=%v", argv, wd, ok)
	}

	// A tool with no WORKING_DIRECTORY is not a write-in-place producer.
	if _, _, ok := cc.writeInPlaceProducer(
		[]shadow.ExecuteProcessCall{call("", "python3", filepath.Join(src, "tool.py"))},
		anc, "gen"); ok {
		t.Error("a tool with empty WORKING_DIRECTORY must not match write-in-place")
	}

	// A WORKING_DIRECTORY that anchors to a DIFFERENT dir than outsParent declines.
	if _, _, ok := cc.writeInPlaceProducer(
		[]shadow.ExecuteProcessCall{call(filepath.Join(build, "other"), "python3", filepath.Join(src, "tool.py"))},
		anc, "gen"); ok {
		t.Error("a WORKING_DIRECTORY not equal to the output dir must decline")
	}

	// Two producers writing into the output dir is ambiguous → decline.
	if _, _, ok := cc.writeInPlaceProducer(
		[]shadow.ExecuteProcessCall{
			call(outDir, "python3", filepath.Join(src, "a.py")),
			call(outDir, "python3", filepath.Join(src, "b.py")),
		}, anc, "gen"); ok {
		t.Error("two in-place producers must decline (ambiguous)")
	}
}

// TestWriteInPlaceProducer_BuildRoot pins that a WORKING_DIRECTORY equal to the
// build root (outsParent == ".") matches when the declared outputs sit at the
// build root.
func TestWriteInPlaceProducer_BuildRoot(t *testing.T) {
	build := t.TempDir()
	src := t.TempDir()
	anc := execAnchors{hostSrcDir: src, recordedSrcDir: src, hostBuildDir: build, recordedBuildDir: build}
	cc := &codegenContext{}
	argv, wd, ok := cc.writeInPlaceProducer(
		[]shadow.ExecuteProcessCall{{Commands: [][]string{{"python3", filepath.Join(src, "tool.py")}}, WorkingDirectory: build}},
		anc, ".")
	if !ok || wd != build || argv[0] != "python3" {
		t.Fatalf("expected build-root in-place tool selected; got argv=%v wd=%q ok=%v", argv, wd, ok)
	}
}

// TestWriteInPlaceDeclared pins the declared-output invariants: relOut present,
// shared parent dir, and every output on disk; a missing output or a split dir
// declines.
func TestWriteInPlaceDeclared(t *testing.T) {
	build := t.TempDir()
	if err := os.MkdirAll(filepath.Join(build, "gen"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"gen/a.c", "gen/b.c"} {
		if err := os.WriteFile(filepath.Join(build, filepath.FromSlash(f)), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	b := &ninja.Build{Outputs: []string{"gen/a.c", "gen/b.c"}}
	declared, outsParent, ok := writeInPlaceDeclared(b, build, "gen/a.c")
	if !ok || outsParent != "gen" || len(declared) != 2 {
		t.Fatalf("expected ok with outsParent=gen; got declared=%v outsParent=%q ok=%v", declared, outsParent, ok)
	}

	// relOut not among the declared outputs declines.
	if _, _, ok := writeInPlaceDeclared(b, build, "gen/missing.c"); ok {
		t.Error("relOut not in declared outputs must decline")
	}

	// A declared output missing on disk declines (the trace didn't produce it).
	b2 := &ninja.Build{Outputs: []string{"gen/a.c", "gen/ghost.c"}}
	if _, _, ok := writeInPlaceDeclared(b2, build, "gen/a.c"); ok {
		t.Error("a declared output absent on disk must decline")
	}
}
