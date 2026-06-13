package lower

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sstriker/buildstream-bazel/internal/shadow"
)

// TestClearDeadCaptures: dead variables clear their channel; live ones
// stay; an empty dead set is a no-op identity.
func TestClearDeadCaptures(t *testing.T) {
	call := shadow.ExecuteProcessCall{
		OutputVariable:  "_quiet",
		ErrorVariable:   "_quiet_err",
		ResultVariable:  "_rc",
		ResultsVariable: "_rcs",
	}
	if got := clearDeadCaptures(call, nil); callCaptureCleared(call, got) {
		t.Errorf("nil dead set must be identity; got %+v", got)
	}
	dead := map[string]bool{"_quiet": true, "_rcs": true}
	got := clearDeadCaptures(call, dead)
	if got.OutputVariable != "" || got.ResultsVariable != "" {
		t.Errorf("dead channels not cleared: %+v", got)
	}
	if got.ErrorVariable != "_quiet_err" || got.ResultVariable != "_rc" {
		t.Errorf("live channels must stay: %+v", got)
	}
	if !callCaptureCleared(call, got) {
		t.Error("callCaptureCleared must report the clearing")
	}
	if callCaptureCleared(call, call) {
		t.Error("identical calls must not report clearing")
	}
}

// TestRecoverExecuteProcess_DeadCaptureStampSkips: a stamp whose
// OUTPUT_VARIABLE is proven dead (silencing) SKIPS — no refusal, no
// StampVars pollution — while the same call with a live variable
// refuses AND records the variable into the capture sink for the
// driver's dead-capture pass.
func TestRecoverExecuteProcess_DeadCaptureStampSkips(t *testing.T) {
	hostSrc := t.TempDir()
	mkCall := func() []shadow.ExecuteProcessCall {
		return []shadow.ExecuteProcessCall{{
			File:           filepath.Join(hostSrc, "CMakeLists.txt"),
			Line:           4,
			Commands:       [][]string{{"git", "rev-parse", "HEAD"}},
			OutputVariable: "_sha",
		}}
	}

	// Live capture: refuses, sink records.
	ccLive := newCodegenContext()
	ccLive.CaptureRefusalSink = map[string]bool{}
	_, refusals := recoverExecuteProcess(mkCall(), hostSrc, hostSrc, "", "/build", false, nil, nil, ccLive)
	if len(refusals) != 1 {
		t.Fatalf("live capture must refuse: %+v", refusals)
	}
	if !ccLive.CaptureRefusalSink["_sha"] {
		t.Errorf("capture sink must record the refused variable: %v", ccLive.CaptureRefusalSink)
	}

	// Dead capture: skips silently.
	ccDead := newCodegenContext()
	ccDead.DeadCaptureVars = map[string]bool{"_sha": true}
	_, refusals = recoverExecuteProcess(mkCall(), hostSrc, hostSrc, "", "/build", false, nil, nil, ccDead)
	if len(refusals) != 0 {
		t.Fatalf("dead capture must skip, got refusals: %+v", refusals)
	}
	if _, polluted := ccDead.StampVars["_sha"]; polluted {
		t.Error("dead capture must not register a stamp var")
	}
}

// TestRecoverExecuteProcess_FileProducingKeywords pins the upgraded
// keyword gates: TIMEOUT no longer refuses; a source-tree INPUT_FILE
// becomes a declared src with a stdin redirect; an in-build-dir
// ERROR_FILE becomes a second declared out with a stderr redirect; the
// same-file ERROR_FILE merges streams; the unliftable forms (build-dir
// stdin, out-of-tree stderr) still refuse with precise reasons.
func TestRecoverExecuteProcess_FileProducingKeywords(t *testing.T) {
	hostSrc := t.TempDir()
	if err := os.WriteFile(filepath.Join(hostSrc, "in.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	base := shadow.ExecuteProcessCall{
		File:       filepath.Join(hostSrc, "CMakeLists.txt"),
		Line:       7,
		Commands:   [][]string{{"sed", "-e", "s/a/b/"}},
		OutputFile: "/build/gen/out.h",
	}

	t.Run("timeout-input-error", func(t *testing.T) {
		call := base
		call.Timeout = "30"
		call.InputFile = filepath.Join(hostSrc, "in.txt")
		call.ErrorFile = "/build/gen/out.err"
		cc := newCodegenContext()
		_, refusals := recoverExecuteProcess([]shadow.ExecuteProcessCall{call}, hostSrc, hostSrc, "", "/build", false, nil, nil, cc)
		if len(refusals) != 0 {
			t.Fatalf("expected lift: %+v", refusals)
		}
		if len(cc.Genrules) != 1 {
			t.Fatalf("Genrules: %+v", cc.Genrules)
		}
		g := cc.Genrules[0]
		if len(g.Srcs) != 1 || g.Srcs[0] != "in.txt" {
			t.Errorf("srcs: %v want [in.txt]", g.Srcs)
		}
		if !strings.Contains(g.GenruleCmd, `< "$(location in.txt)"`) {
			t.Errorf("cmd missing stdin redirect: %q", g.GenruleCmd)
		}
		if len(g.GenruleOuts) != 2 || g.GenruleOuts[0] != "gen/out.h" || g.GenruleOuts[1] != "gen/out.err" {
			t.Errorf("outs: %v want [gen/out.h gen/out.err]", g.GenruleOuts)
		}
		if !strings.Contains(g.GenruleCmd, `2> "$(location gen/out.err)"`) {
			t.Errorf("cmd missing stderr redirect: %q", g.GenruleCmd)
		}
		if cc.OutToGenrule["gen/out.err"] != g.Name {
			t.Errorf("stderr out not registered: %v", cc.OutToGenrule)
		}
	})

	t.Run("error-file-merge", func(t *testing.T) {
		call := base
		call.ErrorFile = call.OutputFile
		cc := newCodegenContext()
		_, refusals := recoverExecuteProcess([]shadow.ExecuteProcessCall{call}, hostSrc, hostSrc, "", "/build", false, nil, nil, cc)
		if len(refusals) != 0 {
			t.Fatalf("expected lift: %+v", refusals)
		}
		g := cc.Genrules[0]
		if !strings.Contains(g.GenruleCmd, `2>&1`) || len(g.GenruleOuts) != 1 {
			t.Errorf("same-file ERROR_FILE must merge streams into the single out: %q outs=%v", g.GenruleCmd, g.GenruleOuts)
		}
	})

	t.Run("build-dir-stdin-refuses", func(t *testing.T) {
		call := base
		call.InputFile = "/build/other/in.txt"
		cc := newCodegenContext()
		_, refusals := recoverExecuteProcess([]shadow.ExecuteProcessCall{call}, hostSrc, hostSrc, "", "/build", false, nil, nil, cc)
		if len(refusals) != 1 || !strings.Contains(refusals[0].Reason, "INPUT_FILE") {
			t.Fatalf("build-dir stdin must refuse with an INPUT_FILE reason: %+v", refusals)
		}
	})

	t.Run("out-of-tree-stderr-refuses", func(t *testing.T) {
		call := base
		call.ErrorFile = "/elsewhere/x.err"
		cc := newCodegenContext()
		_, refusals := recoverExecuteProcess([]shadow.ExecuteProcessCall{call}, hostSrc, hostSrc, "", "/build", false, nil, nil, cc)
		if len(refusals) != 1 || !strings.Contains(refusals[0].Reason, "ERROR_FILE") {
			t.Fatalf("out-of-tree stderr must refuse with an ERROR_FILE reason: %+v", refusals)
		}
	})
}

// TestRecoverExecuteProcess_DeadCaptureLiveErrorChannelStillRefuses:
// the dead-capture skip requires EVERY channel clear — a live
// ERROR_VARIABLE (it survived clearing because the configure reads it)
// keeps the loud refusal even when the OUTPUT_VARIABLE was proven dead.
func TestRecoverExecuteProcess_DeadCaptureLiveErrorChannelStillRefuses(t *testing.T) {
	hostSrc := t.TempDir()
	cc := newCodegenContext()
	cc.DeadCaptureVars = map[string]bool{"_quiet": true}
	calls := []shadow.ExecuteProcessCall{{
		File:           filepath.Join(hostSrc, "CMakeLists.txt"),
		Line:           4,
		Commands:       [][]string{{"git", "rev-parse", "HEAD"}},
		OutputVariable: "_quiet",
		ErrorVariable:  "GIT_ERR", // live: not in the dead set
	}}
	_, refusals := recoverExecuteProcess(calls, hostSrc, hostSrc, "", "/build", false, nil, nil, cc)
	if len(refusals) != 1 {
		t.Fatalf("a live ERROR_VARIABLE must keep the refusal: %+v", refusals)
	}
}
