package lower

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestArgvFlagValue(t *testing.T) {
	cases := map[string]string{
		"--out-dir=/b/gen": "/b/gen",
		"-o=/x":            "/x",
		"/plain/path":      "/plain/path",
		"-I":               "-I",
		"--flag":           "--flag",
	}
	for in, want := range cases {
		if got := argvFlagValue(in); got != want {
			t.Errorf("argvFlagValue(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestArgvOutputAnchorsBuildRoot(t *testing.T) {
	hostSrc, hostBuild := t.TempDir(), t.TempDir()
	anc := execAnchors{hostSrcDir: hostSrc, recordedSrcDir: hostSrc, hostBuildDir: hostBuild, recordedBuildDir: hostBuild}
	// A --out-dir= pointing at the build root is the writes-to-build-root signal.
	if !argvOutputAnchorsBuildRoot([]string{"sh", "gen.sh", "--out-dir=" + hostBuild, filepath.Join(hostSrc, "in.def")}, anc) {
		t.Errorf("expected build-root output to be detected via --out-dir=")
	}
	// A bare positional build-root dir counts too.
	if !argvOutputAnchorsBuildRoot([]string{"mygen", hostBuild, filepath.Join(hostSrc, "in.def")}, anc) {
		t.Errorf("expected build-root output to be detected via positional dir")
	}
	// No build-root operand (only source inputs) → not detected.
	if argvOutputAnchorsBuildRoot([]string{"mygen", filepath.Join(hostSrc, "in.def")}, anc) {
		t.Errorf("source-only argv should not anchor a build-root output")
	}
}

func TestRewriteTracedToolCmd(t *testing.T) {
	hostSrc, hostBuild := t.TempDir(), t.TempDir()
	writeTree(t, hostSrc, "gen.sh", "#!/bin/sh\n")
	writeTree(t, hostSrc, "greeting.def", "Hi\n")
	anc := execAnchors{hostSrcDir: hostSrc, recordedSrcDir: hostSrc, hostBuildDir: hostBuild, recordedBuildDir: hostBuild}
	cc := newCodegenContext()

	argv := []string{"sh", filepath.Join(hostSrc, "gen.sh"), "--out-dir=" + hostBuild, filepath.Join(hostSrc, "greeting.def")}
	srcs, tools, cmd, ok := cc.rewriteTracedToolCmd(argv, anc)
	if !ok {
		t.Fatal("rewriteTracedToolCmd declined unexpectedly")
	}
	if len(tools) != 0 {
		t.Errorf("a PATH tool (sh) should need no tools=, got %v", tools)
	}
	if want := []string{"gen.sh", "greeting.def"}; !reflect.DeepEqual(srcs, want) {
		t.Errorf("srcs = %v, want %v", srcs, want)
	}
	for _, want := range []string{"sh ", "$(location gen.sh)", "--out-dir=$(RULEDIR)", "$(location greeting.def)"} {
		if !strings.Contains(cmd, want) {
			t.Errorf("cmd %q missing %q", cmd, want)
		}
	}

	// A source-tree DIRECTORY operand can't be staged → decline (matches
	// rewriteArgvCodegen's guard: a dir-scanning tool would see an empty view).
	writeTree(t, hostSrc, "incdir/keep", "")
	argvDir := []string{"mygen", "--out-dir=" + hostBuild, "-I", filepath.Join(hostSrc, "incdir")}
	if _, _, _, ok := cc.rewriteTracedToolCmd(argvDir, anc); ok {
		t.Errorf("expected decline on a source-tree directory operand")
	}
}
