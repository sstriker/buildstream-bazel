package lower

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/ir"
	"github.com/sstriker/buildstream-bazel/internal/shadow"
)

func argvCall(hostSrc string, argv ...string) shadow.ExecuteProcessCall {
	return shadow.ExecuteProcessCall{
		File:     filepath.Join(hostSrc, "CMakeLists.txt"),
		Line:     5,
		Commands: [][]string{argv},
	}
}

func writeTree(t *testing.T, root, rel, body string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Row 1: plain positional in/out — `tool /src/in.txt /build/gen/out.h` lifts
// to a genrule with the input in srcs and the output in outs, both
// $(location)-referenced in cmd.
func TestLiftArgvFileProducing_Positional(t *testing.T) {
	hostSrc, hostBuild := t.TempDir(), t.TempDir()
	writeTree(t, hostSrc, "in.txt", "x\n")
	writeTree(t, hostBuild, "gen/out.h", "#define X 1\n")
	call := argvCall(hostSrc, "/opt/tools/mygen", filepath.Join(hostSrc, "in.txt"), filepath.Join(hostBuild, "gen/out.h"))
	cc := newCodegenContext()
	outs, refusals := recoverExecuteProcess([]shadow.ExecuteProcessCall{call}, hostSrc, hostSrc, hostBuild, hostBuild, true, nil, nil, nil, nil, cc)
	if len(refusals) != 0 {
		t.Fatalf("refusals: %+v", refusals)
	}
	if len(outs) != 1 || outs[0].RelOutput != "gen/out.h" {
		t.Fatalf("outs: %+v", outs)
	}
	if len(cc.Genrules) != 1 {
		t.Fatalf("Genrules: %+v", cc.Genrules)
	}
	g := cc.Genrules[0]
	if g.Kind != ir.KindGenrule || len(g.GenruleOuts) != 1 || g.GenruleOuts[0] != "gen/out.h" {
		t.Fatalf("genrule shape: %+v", g)
	}
	if len(g.Srcs) != 1 || g.Srcs[0] != "in.txt" {
		t.Errorf("srcs: %v", g.Srcs)
	}
	if !strings.Contains(g.GenruleCmd, "$(location in.txt)") || !strings.Contains(g.GenruleCmd, "$(location gen/out.h)") {
		t.Errorf("cmd: %q", g.GenruleCmd)
	}
	if strings.Contains(g.GenruleCmd, "/opt/tools/") || !strings.Contains(g.GenruleCmd, "mygen ") {
		t.Errorf("abs tool should strip to basename: %q", g.GenruleCmd)
	}
	var sawFacet bool
	for _, tg := range g.Tags {
		if tg == "cmake-codegen-execute-process-argv-outs" {
			sawFacet = true
		}
	}
	if !sawFacet {
		t.Errorf("missing -argv-outs facet: %v", g.Tags)
	}
}

// Row 2: keyed operands (dd if=/of=) classify by path part, re-emit with the
// key preserved.
func TestLiftArgvFileProducing_KeyedOperands(t *testing.T) {
	hostSrc, hostBuild := t.TempDir(), t.TempDir()
	writeTree(t, hostSrc, "in.bin", "data")
	writeTree(t, hostBuild, "out.bin", "data")
	call := argvCall(hostSrc, "dd", "if="+filepath.Join(hostSrc, "in.bin"), "of="+filepath.Join(hostBuild, "out.bin"))
	cc := newCodegenContext()
	_, refusals := recoverExecuteProcess([]shadow.ExecuteProcessCall{call}, hostSrc, hostSrc, hostBuild, hostBuild, true, nil, nil, nil, nil, cc)
	if len(refusals) != 0 || len(cc.Genrules) != 1 {
		t.Fatalf("refusals=%+v genrules=%+v", refusals, cc.Genrules)
	}
	cmd := cc.Genrules[0].GenruleCmd
	if !strings.Contains(cmd, "if=$(location in.bin)") || !strings.Contains(cmd, "of=$(location out.bin)") {
		t.Errorf("keyed operands not preserved: %q", cmd)
	}
}

// Row 4: relative operands resolve against the build root (cmake's process
// cwd); a relative non-file stays a literal flag value.
func TestLiftArgvFileProducing_RelativeOperands(t *testing.T) {
	hostSrc, hostBuild := t.TempDir(), t.TempDir()
	writeTree(t, hostSrc, "in.txt", "x\n")
	writeTree(t, hostBuild, "rel/out.h", "#define X 1\n")
	call := argvCall(hostSrc, "mygen", "--mode", "fast", filepath.Join(hostSrc, "in.txt"), "rel/out.h")
	cc := newCodegenContext()
	_, refusals := recoverExecuteProcess([]shadow.ExecuteProcessCall{call}, hostSrc, hostSrc, hostBuild, hostBuild, true, nil, nil, nil, nil, cc)
	if len(refusals) != 0 || len(cc.Genrules) != 1 {
		t.Fatalf("refusals=%+v genrules=%+v", refusals, cc.Genrules)
	}
	g := cc.Genrules[0]
	if len(g.GenruleOuts) != 1 || g.GenruleOuts[0] != "rel/out.h" {
		t.Fatalf("relative out not anchored at the build root: %+v", g.GenruleOuts)
	}
	if !strings.Contains(g.GenruleCmd, " fast ") {
		t.Errorf("relative non-file must stay a literal flag value: %q", g.GenruleCmd)
	}
}

// Row 5: multiple outputs → one multi-out genrule.
func TestLiftArgvFileProducing_MultipleOutputs(t *testing.T) {
	hostSrc, hostBuild := t.TempDir(), t.TempDir()
	writeTree(t, hostSrc, "in.txt", "x\n")
	writeTree(t, hostBuild, "a.h", "A")
	writeTree(t, hostBuild, "b.h", "B")
	call := argvCall(hostSrc, "mygen", filepath.Join(hostSrc, "in.txt"), filepath.Join(hostBuild, "a.h"), filepath.Join(hostBuild, "b.h"))
	cc := newCodegenContext()
	outs, refusals := recoverExecuteProcess([]shadow.ExecuteProcessCall{call}, hostSrc, hostSrc, hostBuild, hostBuild, true, nil, nil, nil, nil, cc)
	if len(refusals) != 0 {
		t.Fatalf("refusals: %+v", refusals)
	}
	if len(outs) != 2 || len(cc.Genrules) != 1 || len(cc.Genrules[0].GenruleOuts) != 2 {
		t.Fatalf("multi-out shape: outs=%+v genrules=%+v", outs, cc.Genrules)
	}
}

// Row 6: a build-dir input produced by an earlier recovery is a generated
// INPUT, not an output — chains resolve in trace order.
func TestLiftArgvFileProducing_ChainedGeneratedInput(t *testing.T) {
	hostSrc, hostBuild := t.TempDir(), t.TempDir()
	writeTree(t, hostSrc, "in.txt", "x\n")
	writeTree(t, hostBuild, "stage1.txt", "s1")
	writeTree(t, hostBuild, "stage2.h", "s2")
	callA := argvCall(hostSrc, "genA", filepath.Join(hostSrc, "in.txt"), filepath.Join(hostBuild, "stage1.txt"))
	callB := argvCall(hostSrc, "genB", filepath.Join(hostBuild, "stage1.txt"), filepath.Join(hostBuild, "stage2.h"))
	cc := newCodegenContext()
	_, refusals := recoverExecuteProcess([]shadow.ExecuteProcessCall{callA, callB}, hostSrc, hostSrc, hostBuild, hostBuild, true, nil, nil, nil, nil, cc)
	if len(refusals) != 0 || len(cc.Genrules) != 2 {
		t.Fatalf("refusals=%+v genrules=%d", refusals, len(cc.Genrules))
	}
	var b *ir.Target
	for i := range cc.Genrules {
		if cc.Genrules[i].GenruleOuts[0] == "stage2.h" {
			b = &cc.Genrules[i]
		}
	}
	if b == nil {
		t.Fatalf("stage2 genrule missing: %+v", cc.Genrules)
	}
	if len(b.Srcs) != 1 || b.Srcs[0] != "stage1.txt" {
		t.Errorf("chained generated input should ride srcs: %v", b.Srcs)
	}
	if len(b.GenruleOuts) != 1 {
		t.Errorf("stage1 must NOT be re-classified as stage2's output: %v", b.GenruleOuts)
	}
}

// Row 7: an absolute build-dir path absent from disk is unclassifiable —
// decline, the call refuses loudly.
func TestLiftArgvFileProducing_UnclassifiableBuildPathDeclines(t *testing.T) {
	hostSrc, hostBuild := t.TempDir(), t.TempDir()
	writeTree(t, hostSrc, "in.txt", "x\n")
	call := argvCall(hostSrc, "mygen", filepath.Join(hostSrc, "in.txt"), filepath.Join(hostBuild, "never-written.h"))
	cc := newCodegenContext()
	_, refusals := recoverExecuteProcess([]shadow.ExecuteProcessCall{call}, hostSrc, hostSrc, hostBuild, hostBuild, true, nil, nil, nil, nil, cc)
	if len(refusals) != 1 {
		t.Fatalf("want a loud refusal; got %+v (genrules %+v)", refusals, cc.Genrules)
	}
}

// Row 8 (deferred variant): a build-dir DIRECTORY operand declines.
func TestLiftArgvFileProducing_OutputDirDeclines(t *testing.T) {
	hostSrc, hostBuild := t.TempDir(), t.TempDir()
	writeTree(t, hostSrc, "in.txt", "x\n")
	if err := os.MkdirAll(filepath.Join(hostBuild, "gen"), 0o755); err != nil {
		t.Fatal(err)
	}
	call := argvCall(hostSrc, "mygen", filepath.Join(hostSrc, "in.txt"), filepath.Join(hostBuild, "gen"))
	cc := newCodegenContext()
	_, refusals := recoverExecuteProcess([]shadow.ExecuteProcessCall{call}, hostSrc, hostSrc, hostBuild, hostBuild, true, nil, nil, nil, nil, cc)
	if len(refusals) != 1 || len(cc.Genrules) != 0 {
		t.Fatalf("dir operand must decline: refusals=%+v genrules=%+v", refusals, cc.Genrules)
	}
}

// Row 9: in-place (the output path is also a staged input) declines.
func TestLiftArgvFileProducing_InPlaceDeclines(t *testing.T) {
	hostSrc, hostBuild := t.TempDir(), t.TempDir()
	writeTree(t, hostBuild, "gen/x.h", "X")
	// A second call reading AND writing gen/x.h: the path is on disk (so it
	// classifies as output) and also appears as the produced input of the
	// same argv — the rewrite stages it as src, the in-place check declines.
	cc := newCodegenContext()
	cc.OutToGenrule["gen/x.h"] = "" // pretend an earlier recovery produced it... then it's an input only
	call := argvCall(hostSrc, "fixup", filepath.Join(hostBuild, "gen/x.h"), filepath.Join(hostBuild, "gen/x.h"))
	_, refusals := recoverExecuteProcess([]shadow.ExecuteProcessCall{call}, hostSrc, hostSrc, hostBuild, hostBuild, true, nil, nil, nil, nil, cc)
	if len(cc.Genrules) != 0 {
		t.Fatalf("in-place shape must not emit: %+v", cc.Genrules)
	}
	_ = refusals
}

// A configure-BUILT tool (argv[0] anchored in the build dir) declines: it
// isn't on PATH at re-run time and srcs-staging a build artifact via
// $(location) without tools=/executable bits is wrong.
func TestLiftArgvFileProducing_BuildDirToolDeclines(t *testing.T) {
	hostSrc, hostBuild := t.TempDir(), t.TempDir()
	writeTree(t, hostSrc, "in.txt", "x\n")
	writeTree(t, hostBuild, "bin/gen", "#!/bin/sh\n")
	writeTree(t, hostBuild, "out.h", "X")
	for name, tool := range map[string]string{
		"absolute": filepath.Join(hostBuild, "bin/gen"),
		"relative": "bin/gen", // resolves against the build-root cwd
	} {
		cc := newCodegenContext()
		call := argvCall(hostSrc, tool, filepath.Join(hostSrc, "in.txt"), filepath.Join(hostBuild, "out.h"))
		_, refusals := recoverExecuteProcess([]shadow.ExecuteProcessCall{call}, hostSrc, hostSrc, hostBuild, hostBuild, true, nil, nil, nil, nil, cc)
		if len(refusals) != 1 || len(cc.Genrules) != 0 {
			t.Errorf("%s build-dir tool must decline: refusals=%+v genrules=%+v", name, refusals, cc.Genrules)
		}
	}
}

// A source-tree DIRECTORY operand declines: an unstaged literal dir is
// absent/empty under sandboxing, and a dir-scanning generator exiting 0
// over the empty view would be a SILENT divergence.
func TestLiftArgvFileProducing_SourceDirOperandDeclines(t *testing.T) {
	hostSrc, hostBuild := t.TempDir(), t.TempDir()
	writeTree(t, hostSrc, "proto/in.txt", "x\n")
	writeTree(t, hostBuild, "out.h", "X")
	call := argvCall(hostSrc, "mygen", filepath.Join(hostSrc, "proto"), filepath.Join(hostBuild, "out.h"))
	cc := newCodegenContext()
	_, refusals := recoverExecuteProcess([]shadow.ExecuteProcessCall{call}, hostSrc, hostSrc, hostBuild, hostBuild, true, nil, nil, nil, nil, cc)
	if len(refusals) != 1 || len(cc.Genrules) != 0 {
		t.Fatalf("source dir operand must decline: refusals=%+v genrules=%+v", refusals, cc.Genrules)
	}
}

// Two calls sharing one output path never yield two genrules claiming the
// same out: the second call sees the registered path as a generated INPUT
// (and the defensive partial-overlap decline backs the invariant).
func TestLiftArgvFileProducing_SharedOutputSingleProducer(t *testing.T) {
	hostSrc, hostBuild := t.TempDir(), t.TempDir()
	writeTree(t, hostSrc, "in.txt", "x\n")
	writeTree(t, hostBuild, "shared.h", "S")
	writeTree(t, hostBuild, "second.h", "T")
	callA := argvCall(hostSrc, "genA", filepath.Join(hostSrc, "in.txt"), filepath.Join(hostBuild, "shared.h"))
	callB := argvCall(hostSrc, "genB", filepath.Join(hostBuild, "shared.h"), filepath.Join(hostBuild, "second.h"))
	cc := newCodegenContext()
	_, refusals := recoverExecuteProcess([]shadow.ExecuteProcessCall{callA, callB}, hostSrc, hostSrc, hostBuild, hostBuild, true, nil, nil, nil, nil, cc)
	if len(refusals) != 0 {
		t.Fatalf("refusals: %+v", refusals)
	}
	claims := map[string]int{}
	for _, g := range cc.Genrules {
		for _, o := range g.GenruleOuts {
			claims[o]++
		}
	}
	if claims["shared.h"] != 1 || claims["second.h"] != 1 {
		t.Fatalf("each out must have exactly one producer: %v (genrules %+v)", claims, cc.Genrules)
	}
}

// Eligibility gates: keywords keep declining (the ROADMAP expansion order).
func TestLiftArgvFileProducing_KeywordsDecline(t *testing.T) {
	hostSrc, hostBuild := t.TempDir(), t.TempDir()
	writeTree(t, hostSrc, "in.txt", "x\n")
	writeTree(t, hostBuild, "out.h", "X")
	call := argvCall(hostSrc, "mygen", filepath.Join(hostSrc, "in.txt"), filepath.Join(hostBuild, "out.h"))
	call.WorkingDirectory = hostBuild
	cc := newCodegenContext()
	_, refusals := recoverExecuteProcess([]shadow.ExecuteProcessCall{call}, hostSrc, hostSrc, hostBuild, hostBuild, true, nil, nil, nil, nil, cc)
	if len(refusals) != 1 || len(cc.Genrules) != 0 {
		t.Fatalf("WORKING_DIRECTORY must keep declining: refusals=%+v", refusals)
	}
}
