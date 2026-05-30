package lower

import (
	"path/filepath"
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/internal/cmakerun"
	"github.com/sstriker/buildstream-bazel/internal/shadow"
)

// TestRecoverFileGenerate_LiteralProbe_TwoPass exercises the
// generalized-genex two-pass consumer at the file(GENERATE) OUTPUT
// path site. The OUTPUT carries a genex the Go-side evaluator can't
// resolve ($<TARGET_PROPERTY:app,GEN_DIR>); pass 1 records a probe
// request and drops the call, pass 2 supplies the resolution and
// the call lifts to a genrule whose outs is the resolved path.
func TestRecoverFileGenerate_LiteralProbe_TwoPass(t *testing.T) {
	template := "#define X 1\n"
	rendered := []byte("#define X 1\n")
	// The probe resolves the OUTPUT genex to "generated/x.h"; the
	// rendered bytes live there on disk so the lift can read them.
	hostSrc, hostBuild := fileGenerateTestSetup(t, "src/x.h.in", template, "generated/x.h", rendered)

	// OUTPUT path with an arbitrary genex outside the structural set.
	genexOutput := filepath.Join(hostBuild, "$<TARGET_PROPERTY:app,GEN_DIR>/x.h")
	mkCalls := func() []shadow.FileGenerateCall {
		return []shadow.FileGenerateCall{{
			File:     filepath.Join(hostSrc, "CMakeLists.txt"),
			Output:   genexOutput,
			Input:    filepath.Join(hostSrc, "src/x.h.in"),
			HasInput: true,
		}}
	}

	// --- Pass 1: sink collects, call is dropped. ---
	sink := &LiteralProbeSink{}
	cc1 := newCodegenContext()
	cc1.LiteralProbeSink = sink
	out1, err := recoverFileGenerate(mkCalls(), hostSrc, hostSrc, hostBuild, hostBuild, true, nil, nil, nil, cc1)
	if err != nil {
		t.Fatalf("pass 1 recover: %v", err)
	}
	if len(out1) != 0 {
		t.Fatalf("pass 1 should drop the unresolved-OUTPUT call, got %+v", out1)
	}
	reqs := sink.Requests()
	if len(reqs) != 1 {
		t.Fatalf("pass 1 should record exactly one probe request, got %d: %+v", len(reqs), reqs)
	}
	if reqs[0].Literal != genexOutput {
		t.Fatalf("recorded literal = %q, want %q", reqs[0].Literal, genexOutput)
	}

	// --- Pass 2: resolution supplied, call lifts. ---
	resolvedOutput := filepath.Join(hostBuild, "generated/x.h")
	cc2 := newCodegenContext()
	cc2.LiteralResolutions = map[string]cmakerun.LiteralResolution{
		reqs[0].Hash(): {PerConfig: map[string]string{"": resolvedOutput}},
	}
	out2, err := recoverFileGenerate(mkCalls(), hostSrc, hostSrc, hostBuild, hostBuild, true, nil, nil, nil, cc2)
	if err != nil {
		t.Fatalf("pass 2 recover: %v", err)
	}
	if len(out2) != 1 || out2[0].RelOutput != "generated/x.h" {
		t.Fatalf("pass 2 should lift the call to outs generated/x.h, got %+v", out2)
	}
	if len(cc2.Genrules) != 1 {
		t.Fatalf("pass 2 Genrules: %+v", cc2.Genrules)
	}
	if g := cc2.Genrules[0]; len(g.GenruleOuts) != 1 || g.GenruleOuts[0] != "generated/x.h" {
		t.Fatalf("pass 2 genrule outs = %v, want [generated/x.h]", g.GenruleOuts)
	}
}

// TestRecoverFileGenerate_LiteralProbe_PerConfigDivergenceDrops
// confirms that a per-config-divergent resolution (no single static
// path) falls through to the drop path at the OUTPUT site rather
// than emitting an ambiguous genrule outs.
func TestRecoverFileGenerate_LiteralProbe_PerConfigDivergenceDrops(t *testing.T) {
	template := "#define X 1\n"
	rendered := []byte("#define X 1\n")
	hostSrc, hostBuild := fileGenerateTestSetup(t, "src/x.h.in", template, "generated/x.h", rendered)
	genexOutput := filepath.Join(hostBuild, "$<TARGET_PROPERTY:app,GEN_DIR>/x.h")
	calls := []shadow.FileGenerateCall{{
		File:     filepath.Join(hostSrc, "CMakeLists.txt"),
		Output:   genexOutput,
		Input:    filepath.Join(hostSrc, "src/x.h.in"),
		HasInput: true,
	}}

	req := cmakerun.LiteralProbeRequest{Literal: genexOutput}
	cc := newCodegenContext()
	cc.LiteralResolutions = map[string]cmakerun.LiteralResolution{
		req.Hash(): {PerConfig: map[string]string{
			"Release": filepath.Join(hostBuild, "rel/x.h"),
			"Debug":   filepath.Join(hostBuild, "dbg/x.h"),
		}},
	}
	out, err := recoverFileGenerate(calls, hostSrc, hostSrc, hostBuild, hostBuild, true, nil, nil, nil, cc)
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("per-config-divergent OUTPUT should drop, got %+v", out)
	}
}

// TestRecoverFileGenerate_LiteralProbe_NilSinkUnchanged confirms the
// single-pass path (no sink, no resolutions) still drops an
// unresolved-OUTPUT call exactly as before — the two-pass wiring is
// inert when unused.
func TestRecoverFileGenerate_LiteralProbe_NilSinkUnchanged(t *testing.T) {
	template := "#define X 1\n"
	rendered := []byte("#define X 1\n")
	hostSrc, hostBuild := fileGenerateTestSetup(t, "src/x.h.in", template, "generated/x.h", rendered)
	calls := []shadow.FileGenerateCall{{
		File:     filepath.Join(hostSrc, "CMakeLists.txt"),
		Output:   filepath.Join(hostBuild, "$<TARGET_PROPERTY:app,GEN_DIR>/x.h"),
		Input:    filepath.Join(hostSrc, "src/x.h.in"),
		HasInput: true,
	}}
	cc := newCodegenContext() // no sink, no resolutions
	out, err := recoverFileGenerate(calls, hostSrc, hostSrc, hostBuild, hostBuild, true, nil, nil, nil, cc)
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("single-pass unresolved-OUTPUT should drop, got %+v", out)
	}
}
