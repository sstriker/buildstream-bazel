package lower

import (
	"strings"
	"testing"

	"path/filepath"

	"github.com/sstriker/buildstream-bazel/internal/shadow"
)

// A generator run with RESULT_VARIABLE (an error check) and no OUTPUT_FILE on a
// dual-use driver (python3) is classified BucketProbe — but it still produces
// files. The probe→codegen override recovers them via the File-API-corroborated
// unspecified-output lift instead of skipping the call. This is the common
// shape: `execute_process(COMMAND python3 gen.py in RESULT_VARIABLE rc)`.
func TestRecoverExecuteProcess_ProbeWithResultVarRecoversCodegen(t *testing.T) {
	hostSrc, hostBuild := t.TempDir(), t.TempDir()
	writeTree(t, hostSrc, "gen.py", "# generator\n")
	writeTree(t, hostSrc, "data.txt", "payload\n")
	writeTree(t, hostBuild, "data.txt.gz", "gzbytes")
	call := shadow.ExecuteProcessCall{
		File:           filepath.Join(hostSrc, "CMakeLists.txt"),
		Line:           5,
		Commands:       [][]string{{"python3", filepath.Join(hostSrc, "gen.py"), filepath.Join(hostSrc, "data.txt")}},
		ResultVariable: "rc", // error check — keeps the call BucketProbe
	}
	if got := Classify(call).Bucket; got != BucketProbe {
		t.Fatalf("precondition: want BucketProbe (dual-use + RESULT_VARIABLE), got %v", got)
	}
	cc := newCodegenContext()
	cc.LiftDerivedCodegen = true
	cc.ConsumedBuildRel = map[string]bool{"data.txt.gz": true}
	outs, refusals := recoverExecuteProcess([]shadow.ExecuteProcessCall{call}, hostSrc, hostSrc, hostBuild, hostBuild, true, nil, nil, nil, nil, cc)
	if len(refusals) != 0 {
		t.Fatalf("refusals: %+v", refusals)
	}
	if len(outs) != 1 || outs[0].RelOutput != "data.txt.gz" {
		t.Fatalf("probe-classified generator should be recovered, not skipped; outs=%+v", outs)
	}
	if cc.OutToGenrule["data.txt.gz"] == "" || len(cc.Genrules) != 1 {
		t.Fatalf("expected a producer for the recovered output; genrules=%+v outToGenrule=%+v", cc.Genrules, cc.OutToGenrule)
	}
}

// A genuine probe with no file output stays skipped — the override self-gates on
// real evidence, so `uname -m OUTPUT_VARIABLE m` recovers nothing and emits no
// genrule/refusal (its value reaches configure_file via dump-vars).
func TestRecoverExecuteProcess_PureProbeStillSkipped(t *testing.T) {
	hostSrc, hostBuild := t.TempDir(), t.TempDir()
	call := shadow.ExecuteProcessCall{
		File:           filepath.Join(hostSrc, "CMakeLists.txt"),
		Line:           5,
		Commands:       [][]string{{"uname", "-m"}},
		OutputVariable: "m",
	}
	cc := newCodegenContext()
	cc.ConsumedBuildRel = map[string]bool{"unrelated.h": true}
	outs, refusals := recoverExecuteProcess([]shadow.ExecuteProcessCall{call}, hostSrc, hostSrc, hostBuild, hostBuild, true, nil, nil, nil, nil, cc)
	if len(outs) != 0 || len(refusals) != 0 || len(cc.Genrules) != 0 {
		t.Fatalf("pure probe must stay skipped; outs=%+v refusals=%+v genrules=%+v", outs, refusals, cc.Genrules)
	}
}

// Dir-operand class: `mygen --out <builddir>/gen <src>/in.proto` — outputs
// are the on-disk files under the argv directory operand; the re-run
// genrule rewrites the operand to $(RULEDIR)/gen.
func TestLiftUnspecifiedOutputs_DirOperand(t *testing.T) {
	hostSrc, hostBuild := t.TempDir(), t.TempDir()
	writeTree(t, hostSrc, "in.proto", "message X {}\n")
	writeTree(t, hostBuild, "gen/a.h", "A")
	writeTree(t, hostBuild, "gen/b.cc", "B")
	call := argvCall(hostSrc, "mygen", "--out", filepath.Join(hostBuild, "gen"), filepath.Join(hostSrc, "in.proto"))
	cc := newCodegenContext()
	outs, refusals := recoverExecuteProcess([]shadow.ExecuteProcessCall{call}, hostSrc, hostSrc, hostBuild, hostBuild, true, nil, nil, nil, nil, cc)
	if len(refusals) != 0 {
		t.Fatalf("refusals: %+v", refusals)
	}
	if len(outs) != 2 || len(cc.Genrules) != 1 {
		t.Fatalf("outs=%+v genrules=%+v", outs, cc.Genrules)
	}
	g := cc.Genrules[0]
	if len(g.GenruleOuts) != 2 || g.GenruleOuts[0] != "gen/a.h" || g.GenruleOuts[1] != "gen/b.cc" {
		t.Fatalf("dir-operand outs: %v", g.GenruleOuts)
	}
	if !strings.Contains(g.GenruleCmd, `mkdir -p "$(RULEDIR)/gen"`) || !strings.Contains(g.GenruleCmd, "$(RULEDIR)/gen") {
		t.Errorf("dir operand must rewrite to $(RULEDIR)/gen: %q", g.GenruleCmd)
	}
	if len(g.Srcs) != 1 || g.Srcs[0] != "in.proto" {
		t.Errorf("srcs: %v", g.Srcs)
	}
	var sawFacet bool
	for _, tg := range g.Tags {
		if tg == "cmake-codegen-execute-process-dir-outs" {
			sawFacet = true
		}
	}
	if !sawFacet {
		t.Errorf("missing -dir-outs facet: %v", g.Tags)
	}
}

// Two calls naming the same (or nested) directory operand disqualify both —
// first-writer attribution would be a guess.
func TestLiftUnspecifiedOutputs_DirOperandOverlapDeclines(t *testing.T) {
	hostSrc, hostBuild := t.TempDir(), t.TempDir()
	writeTree(t, hostSrc, "a.in", "a")
	writeTree(t, hostSrc, "b.in", "b")
	writeTree(t, hostBuild, "gen/x.h", "X")
	writeTree(t, hostBuild, "gen/sub/y.h", "Y")
	callA := argvCall(hostSrc, "genA", filepath.Join(hostBuild, "gen"), filepath.Join(hostSrc, "a.in"))
	callB := argvCall(hostSrc, "genB", filepath.Join(hostBuild, "gen/sub"), filepath.Join(hostSrc, "b.in"))
	callB.Line = 9 // distinct call site — same-site duplicates count as one claim
	cc := newCodegenContext()
	_, refusals := recoverExecuteProcess([]shadow.ExecuteProcessCall{callA, callB}, hostSrc, hostSrc, hostBuild, hostBuild, true, nil, nil, nil, nil, cc)
	if len(refusals) != 2 || len(cc.Genrules) != 0 {
		t.Fatalf("overlapping dir operands must both decline: refusals=%+v genrules=%+v", refusals, cc.Genrules)
	}
}

// Files under the dir operand that a ninja edge produces (build-time
// codegen) or another recovery already claims are excluded from the outs.
func TestLiftUnspecifiedOutputs_DirOperandExcludesNinjaOuts(t *testing.T) {
	hostSrc, hostBuild := t.TempDir(), t.TempDir()
	writeTree(t, hostSrc, "in.proto", "x")
	writeTree(t, hostBuild, "gen/a.h", "A")
	writeTree(t, hostBuild, "gen/built.o", "O")
	call := argvCall(hostSrc, "mygen", filepath.Join(hostBuild, "gen"), filepath.Join(hostSrc, "in.proto"))
	cc := newCodegenContext()
	cc.NinjaOuts = map[string]bool{"gen/built.o": true}
	_, refusals := recoverExecuteProcess([]shadow.ExecuteProcessCall{call}, hostSrc, hostSrc, hostBuild, hostBuild, true, nil, nil, nil, nil, cc)
	if len(refusals) != 0 || len(cc.Genrules) != 1 {
		t.Fatalf("refusals=%+v genrules=%+v", refusals, cc.Genrules)
	}
	if g := cc.Genrules[0]; len(g.GenruleOuts) != 1 || g.GenruleOuts[0] != "gen/a.h" {
		t.Fatalf("ninja-produced file must be excluded: %v", g.GenruleOuts)
	}
}

// Stem class: a consumed build-dir orphan whose name derives from the argv
// input (`data.txt → data.txt.gz`) bakes from the configure-written bytes.
func TestLiftUnspecifiedOutputs_StemBake(t *testing.T) {
	hostSrc, hostBuild := t.TempDir(), t.TempDir()
	writeTree(t, hostSrc, "data.txt", "payload\n")
	writeTree(t, hostBuild, "data.txt.gz", "gzbytes")
	call := argvCall(hostSrc, "compressor", filepath.Join(hostSrc, "data.txt"))
	cc := newCodegenContext()
	cc.ConsumedBuildRel = map[string]bool{"data.txt.gz": true}
	outs, refusals := recoverExecuteProcess([]shadow.ExecuteProcessCall{call}, hostSrc, hostSrc, hostBuild, hostBuild, true, nil, nil, nil, nil, cc)
	if len(refusals) != 0 {
		t.Fatalf("refusals: %+v", refusals)
	}
	if len(outs) != 1 || outs[0].RelOutput != "data.txt.gz" {
		t.Fatalf("outs: %+v", outs)
	}
	if cc.OutToGenrule["data.txt.gz"] == "" {
		t.Fatalf("orphan must register a producer: %+v", cc.OutToGenrule)
	}
	if len(cc.Genrules) != 1 {
		t.Fatalf("genrules: %+v", cc.Genrules)
	}
	var sawFacet bool
	for _, tg := range cc.Genrules[0].Tags {
		if tg == "cmake-codegen-execute-process-derived-bake" {
			sawFacet = true
		}
	}
	if !sawFacet {
		t.Errorf("missing -derived-bake facet: %v", cc.Genrules[0].Tags)
	}
}

// With --lift-derived-codegen, a stem-matched orphan in a SUBDIR re-runs as a
// live genrule (cd $(RULEDIR) + mkdir -p the parent) rather than freezing the
// configure-written bytes — the subdir no longer forces the bake fallback.
func TestLiftUnspecifiedOutputs_StemRerunSubdir(t *testing.T) {
	hostSrc, hostBuild := t.TempDir(), t.TempDir()
	writeTree(t, hostSrc, "data.txt", "payload\n")
	writeTree(t, hostBuild, "gen/data.txt.gz", "gzbytes")
	call := argvCall(hostSrc, "compressor", filepath.Join(hostSrc, "data.txt"))
	cc := newCodegenContext()
	cc.LiftDerivedCodegen = true
	cc.ConsumedBuildRel = map[string]bool{"gen/data.txt.gz": true}
	outs, refusals := recoverExecuteProcess([]shadow.ExecuteProcessCall{call}, hostSrc, hostSrc, hostBuild, hostBuild, true, nil, nil, nil, nil, cc)
	if len(refusals) != 0 {
		t.Fatalf("refusals: %+v", refusals)
	}
	if len(outs) != 1 || outs[0].RelOutput != "gen/data.txt.gz" || len(cc.Genrules) != 1 {
		t.Fatalf("outs=%+v genrules=%+v", outs, cc.Genrules)
	}
	g := cc.Genrules[0]
	if len(g.GenruleOuts) != 1 || g.GenruleOuts[0] != "gen/data.txt.gz" {
		t.Fatalf("subdir out: %v", g.GenruleOuts)
	}
	if !strings.Contains(g.GenruleCmd, `cd "$(RULEDIR)"`) || !strings.Contains(g.GenruleCmd, `mkdir -p gen`) {
		t.Errorf("subdir rerun must cd $(RULEDIR) and pre-create the dir: %q", g.GenruleCmd)
	}
	var sawRerun, sawBake bool
	for _, tg := range g.Tags {
		switch tg {
		case "cmake-codegen-execute-process-derived-rerun":
			sawRerun = true
		case "cmake-codegen-execute-process-derived-bake":
			sawBake = true
		}
	}
	if !sawRerun || sawBake {
		t.Errorf("want -derived-rerun (live genrule), not -derived-bake: %v", g.Tags)
	}
}

// An orphan claimed by more than one call is assigned to neither.
func TestLiftUnspecifiedOutputs_MultiClaimDeclines(t *testing.T) {
	hostSrc, hostBuild := t.TempDir(), t.TempDir()
	writeTree(t, hostSrc, "data.txt", "payload\n")
	writeTree(t, hostBuild, "data.txt.gz", "gz")
	callA := argvCall(hostSrc, "compressA", filepath.Join(hostSrc, "data.txt"))
	callB := argvCall(hostSrc, "compressB", filepath.Join(hostSrc, "data.txt"))
	callB.Line = 9 // distinct call site — same-site duplicates count as one claim
	cc := newCodegenContext()
	cc.ConsumedBuildRel = map[string]bool{"data.txt.gz": true}
	_, refusals := recoverExecuteProcess([]shadow.ExecuteProcessCall{callA, callB}, hostSrc, hostSrc, hostBuild, hostBuild, true, nil, nil, nil, nil, cc)
	if len(refusals) != 2 || len(cc.Genrules) != 0 {
		t.Fatalf("multi-claimed orphan must decline both: refusals=%+v genrules=%+v", refusals, cc.Genrules)
	}
}

// An orphan that a ninja edge produces is build-time codegen, not a
// configure-time orphan — never stem-claimed.
func TestLiftUnspecifiedOutputs_NinjaProducedNotClaimed(t *testing.T) {
	hostSrc, hostBuild := t.TempDir(), t.TempDir()
	writeTree(t, hostSrc, "data.txt", "payload\n")
	writeTree(t, hostBuild, "data.txt.gz", "gz")
	call := argvCall(hostSrc, "compressor", filepath.Join(hostSrc, "data.txt"))
	cc := newCodegenContext()
	cc.ConsumedBuildRel = map[string]bool{"data.txt.gz": true}
	cc.NinjaOuts = map[string]bool{"data.txt.gz": true}
	_, refusals := recoverExecuteProcess([]shadow.ExecuteProcessCall{call}, hostSrc, hostSrc, hostBuild, hostBuild, true, nil, nil, nil, nil, cc)
	if len(refusals) != 1 || len(cc.Genrules) != 0 {
		t.Fatalf("ninja-produced orphan must not be claimed: refusals=%+v genrules=%+v", refusals, cc.Genrules)
	}
}

// A bare relative word naming no build file (old-style tar flags,
// subcommands, mode words) stays a literal string argument — the
// argv-codegen discriminator — instead of declining the dir-operand lift.
func TestLiftUnspecifiedOutputs_BareWordStaysLiteral(t *testing.T) {
	hostSrc, hostBuild := t.TempDir(), t.TempDir()
	writeTree(t, hostSrc, "payload.tar", "T")
	writeTree(t, hostBuild, "gen/a.h", "A")
	call := argvCall(hostSrc, "tar", "xf", filepath.Join(hostSrc, "payload.tar"), filepath.Join(hostBuild, "gen"))
	cc := newCodegenContext()
	_, refusals := recoverExecuteProcess([]shadow.ExecuteProcessCall{call}, hostSrc, hostSrc, hostBuild, hostBuild, true, nil, nil, nil, nil, cc)
	if len(refusals) != 0 || len(cc.Genrules) != 1 {
		t.Fatalf("old-style flag word must not decline the lift: refusals=%+v genrules=%+v", refusals, cc.Genrules)
	}
	g := cc.Genrules[0]
	if !strings.Contains(g.GenruleCmd, " xf ") {
		t.Errorf("bare word should stay literal: %q", g.GenruleCmd)
	}
	if len(g.GenruleOuts) != 1 || g.GenruleOuts[0] != "gen/a.h" {
		t.Errorf("outs: %v", g.GenruleOuts)
	}
}

// Duplicate trace entries of the same call (configure re-evaluation: same
// file:line) count as ONE claim — they don't self-disqualify, and the
// second entry reuses the first's genrule.
func TestLiftUnspecifiedOutputs_DuplicateCallReuses(t *testing.T) {
	hostSrc, hostBuild := t.TempDir(), t.TempDir()
	writeTree(t, hostSrc, "in.proto", "x")
	writeTree(t, hostBuild, "gen/a.h", "A")
	call := argvCall(hostSrc, "mygen", filepath.Join(hostBuild, "gen"), filepath.Join(hostSrc, "in.proto"))
	cc := newCodegenContext()
	_, refusals := recoverExecuteProcess([]shadow.ExecuteProcessCall{call, call}, hostSrc, hostSrc, hostBuild, hostBuild, true, nil, nil, nil, nil, cc)
	if len(refusals) != 0 || len(cc.Genrules) != 1 {
		t.Fatalf("duplicate call must reuse, not decline or double-emit: refusals=%+v genrules=%d", refusals, len(cc.Genrules))
	}
}

// A configure-built tool (argv[0] in the build dir) declines the dir-class
// re-run, same policy as the argv-codegen lift.
func TestLiftUnspecifiedOutputs_BuildDirToolDeclines(t *testing.T) {
	hostSrc, hostBuild := t.TempDir(), t.TempDir()
	writeTree(t, hostSrc, "in.proto", "x")
	writeTree(t, hostBuild, "bin/gen", "#!/bin/sh\n")
	writeTree(t, hostBuild, "gen/a.h", "A")
	call := argvCall(hostSrc, filepath.Join(hostBuild, "bin/gen"), filepath.Join(hostBuild, "gen"), filepath.Join(hostSrc, "in.proto"))
	cc := newCodegenContext()
	_, refusals := recoverExecuteProcess([]shadow.ExecuteProcessCall{call}, hostSrc, hostSrc, hostBuild, hostBuild, true, nil, nil, nil, nil, cc)
	if len(refusals) != 1 || len(cc.Genrules) != 0 {
		t.Fatalf("build-dir tool must decline: refusals=%+v genrules=%+v", refusals, cc.Genrules)
	}
}

func TestUsableUnspecOutDir(t *testing.T) {
	for rel, want := range map[string]bool{
		"gen":             true,
		"gen/sub":         true,
		"":                false,
		".":               false,
		"CMakeFiles":      false,
		"CMakeFiles/x":    false,
		"CMakeFilesNot/x": true,
	} {
		if got := usableUnspecOutDir(rel); got != want {
			t.Errorf("usableUnspecOutDir(%q) = %v, want %v", rel, got, want)
		}
	}
}

func TestDerivedNameMatch(t *testing.T) {
	for _, tc := range []struct {
		inputs []string
		orphan string
		want   bool
	}{
		{[]string{"data.txt"}, "data.txt.gz", true},  // full name + suffix
		{[]string{"foo.proto"}, "foo.pb.cc", true},   // stem + '.' suffix
		{[]string{"foo.proto"}, "foo_gen.h", true},   // stem + '_' suffix
		{[]string{"foo.proto"}, "foobar.h", false},   // stem + bare run-on
		{[]string{"a.c"}, "ab.h", false},             // stem too short (<3)
		{[]string{"data.txt"}, "data.txt", false},    // identical, no suffix
		{[]string{"other.in"}, "unrelated.h", false}, // no relation
		{[]string{}, "anything.h", false},            // no inputs
		{[]string{"version.py"}, "version.h", true},  // generator stem + new ext
	} {
		got := derivedNameMatch(tc.inputs, tc.orphan)
		if got != tc.want {
			t.Errorf("derivedNameMatch(%v, %q) = %v, want %v", tc.inputs, tc.orphan, got, tc.want)
		}
	}
}
