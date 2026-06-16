package lower

import (
	"strings"
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/ir"
	"github.com/sstriker/buildstream-bazel/internal/shadow"
)

func TestRecordStampCommand_Collision(t *testing.T) {
	mk := func(argv ...string) shadow.ExecuteProcessCall {
		return shadow.ExecuteProcessCall{Commands: [][]string{argv}}
	}
	cc := newCodegenContext()
	// First write wins.
	recordStampCommand(cc, "STABLE_GIT_SHA", mk("git", "rev-parse", "HEAD"))
	// Identical command repeating (idempotent prescan + in-loop) is NOT a collision.
	recordStampCommand(cc, "STABLE_GIT_SHA", mk("git", "rev-parse", "HEAD"))
	if cc.StampKeyCollisions["STABLE_GIT_SHA"] {
		t.Error("identical repeated command must not flag a collision")
	}
	if cc.StampCommands["STABLE_GIT_SHA"] != "git rev-parse HEAD" {
		t.Errorf("first command lost: %q", cc.StampCommands["STABLE_GIT_SHA"])
	}
	// A DIFFERENT command on the same key is a collision; first is kept.
	recordStampCommand(cc, "STABLE_GIT_SHA", mk("git", "describe", "--tags"))
	if !cc.StampKeyCollisions["STABLE_GIT_SHA"] {
		t.Error("distinct command on the same key must flag a collision")
	}
	if cc.StampCommands["STABLE_GIT_SHA"] != "git rev-parse HEAD" {
		t.Errorf("collision must keep the first command, got %q", cc.StampCommands["STABLE_GIT_SHA"])
	}
}

func TestWarnStampKeyCollisions(t *testing.T) {
	// Nil writer / no collisions: silent no-op.
	warnStampKeyCollisions(nil, map[string]bool{"STABLE_GIT_SHA": true}) // must not panic
	var b strings.Builder
	warnStampKeyCollisions(&b, nil)
	if b.Len() != 0 {
		t.Errorf("no collisions should be silent, got %q", b.String())
	}
	// A collision is named, sorted, in one aggregated line.
	b.Reset()
	warnStampKeyCollisions(&b, map[string]bool{"STABLE_GIT_SHA": true, "STABLE_BUILD_ID": true})
	out := b.String()
	if !strings.Contains(out, "STABLE_BUILD_ID, STABLE_GIT_SHA") {
		t.Errorf("collision warning should name sorted keys, got %q", out)
	}
	if strings.Count(out, "\n") != 1 {
		t.Errorf("collisions should aggregate into one line, got %q", out)
	}
}

func TestPropagateStampVars(t *testing.T) {
	// GIT_SHA is the direct stamp var. VERSION copies it; TAG copies
	// VERSION (a chain); UNREL copies a non-stamp var and stays out.
	stampVars := map[string]string{"GIT_SHA": "STABLE_GIT_SHA"}
	assignments := setAssignments(
		"VERSION", "GIT_SHA",
		"TAG", "VERSION", // chain — needs the fixpoint
		"UNREL", "SOMETHING_ELSE",
	)
	propagateStampVars(stampVars, assignments)

	for _, v := range []string{"VERSION", "TAG"} {
		if stampVars[v] != "STABLE_GIT_SHA" {
			t.Errorf("stampVars[%q] = %q, want inherited STABLE_GIT_SHA", v, stampVars[v])
		}
	}
	if _, ok := stampVars["UNREL"]; ok {
		t.Errorf("UNREL copies a non-stamp var; must not become a stamp var")
	}
	if len(stampVars) != 3 { // GIT_SHA + VERSION + TAG
		t.Errorf("stampVars = %v, want exactly GIT_SHA/VERSION/TAG", stampVars)
	}
}

func TestPropagateStampVars_DirectKeyWins(t *testing.T) {
	// A var that is BOTH a direct stamp var and the dst of a copy keeps
	// its own (direct) status key, not an inherited one.
	stampVars := map[string]string{"A": "STABLE_A", "B": "STABLE_B"}
	propagateStampVars(stampVars, setAssignments("A", "B"))
	if stampVars["A"] != "STABLE_A" {
		t.Errorf("direct stamp var A = %q, want its own STABLE_A (not B's)", stampVars["A"])
	}
}

func TestPropagateStampVars_NoStampNoop(t *testing.T) {
	stampVars := map[string]string{}
	propagateStampVars(stampVars, setAssignments("VERSION", "GIT_SHA"))
	if len(stampVars) != 0 {
		t.Errorf("no seed stamp vars => no propagation; got %v", stampVars)
	}
}

// setAssignments builds a SetAssignment slice from flat dst,src pairs.
func setAssignments(pairs ...string) []shadow.SetAssignment {
	var out []shadow.SetAssignment
	for i := 0; i+1 < len(pairs); i += 2 {
		out = append(out, shadow.SetAssignment{Dst: pairs[i], SrcVar: pairs[i+1]})
	}
	return out
}

func TestApplyParentScopeForwards_ReKeysToConsumer(t *testing.T) {
	// `out` is the function-local OUTPUT_VARIABLE recoverExecuteProcess seeded.
	// The forward resolves the caller arg GIT_SHA; it must be RE-KEYED to its
	// own name (STABLE_GIT_SHA), not inherit the meaningless local's STABLE_OUT.
	stampVars := map[string]string{"out": "STABLE_OUT"}
	applyParentScopeForwards(stampVars, nil, []shadow.ParentScopeForward{{Dst: "GIT_SHA", SrcVar: "out"}})
	if stampVars["GIT_SHA"] != "STABLE_GIT_SHA" {
		t.Errorf("forwarded consumer = %q, want re-keyed STABLE_GIT_SHA", stampVars["GIT_SHA"])
	}
}

func TestApplyParentScopeForwards_PreservesVolatilePrefix(t *testing.T) {
	// A forwarded `date` stamp (VOLATILE_ source key) stays volatile so it
	// doesn't bust the action cache, but restems to the consumer's name.
	stampVars := map[string]string{"out": "VOLATILE_OUT"}
	applyParentScopeForwards(stampVars, nil, []shadow.ParentScopeForward{{Dst: "BUILD_DATE", SrcVar: "out"}})
	if stampVars["BUILD_DATE"] != "VOLATILE_BUILD_DATE" {
		t.Errorf("forwarded volatile consumer = %q, want VOLATILE_BUILD_DATE", stampVars["BUILD_DATE"])
	}
}

func TestApplyParentScopeForwards_SeedsFurtherCopies(t *testing.T) {
	// The resolved consumer must be marked BEFORE propagateStampVars so a
	// further verbatim copy of it (`set(VERSION ${GIT_SHA})`) propagates too.
	stampVars := map[string]string{"out": "STABLE_OUT"}
	applyParentScopeForwards(stampVars, nil, []shadow.ParentScopeForward{{Dst: "GIT_SHA", SrcVar: "out"}})
	propagateStampVars(stampVars, setAssignments("VERSION", "GIT_SHA"))
	if stampVars["VERSION"] != "STABLE_GIT_SHA" {
		t.Errorf("VERSION = %q, want inherited STABLE_GIT_SHA from the forwarded consumer", stampVars["VERSION"])
	}
}

func TestApplyParentScopeForwards_NonStampSrcIgnored(t *testing.T) {
	// A forward whose source var was never a stamp leaves stampVars untouched.
	stampVars := map[string]string{"out": "STABLE_OUT"}
	applyParentScopeForwards(stampVars, nil, []shadow.ParentScopeForward{{Dst: "X", SrcVar: "not_a_stamp"}})
	if _, ok := stampVars["X"]; ok {
		t.Errorf("X forwards a non-stamp var; must not become a stamp var: %v", stampVars)
	}
}

func TestApplyParentScopeForwards_DirectKeyWins(t *testing.T) {
	// A consumer that already carries a direct key keeps it (not overwritten).
	stampVars := map[string]string{"out": "STABLE_OUT", "GIT_SHA": "STABLE_GIT_SHA"}
	applyParentScopeForwards(stampVars, nil, []shadow.ParentScopeForward{{Dst: "GIT_SHA", SrcVar: "out"}})
	if stampVars["GIT_SHA"] != "STABLE_GIT_SHA" {
		t.Errorf("GIT_SHA = %q, want its existing STABLE_GIT_SHA", stampVars["GIT_SHA"])
	}
}

func TestApplyParentScopeForwards_ReKeysCommand(t *testing.T) {
	// The producing command follows the re-key: the forwarded consumer's NEW
	// key (STABLE_GIT_SHA) inherits the source key's command (the helper's
	// `git describe`), and the generic function-local source key is dropped so
	// the emitted status script names only the consumer key.
	stampVars := map[string]string{"out": "STABLE_OUT"}
	stampCommands := map[string]string{"STABLE_OUT": "git describe --tags"}
	applyParentScopeForwards(stampVars, stampCommands, []shadow.ParentScopeForward{{Dst: "GIT_SHA", SrcVar: "out"}})
	if stampCommands["STABLE_GIT_SHA"] != "git describe --tags" {
		t.Errorf("re-keyed command = %q, want the source command under STABLE_GIT_SHA", stampCommands["STABLE_GIT_SHA"])
	}
	if _, ok := stampCommands["STABLE_OUT"]; ok {
		t.Errorf("the function-local source key should be dropped; got %v", stampCommands)
	}
}

func TestPopulateWorkspaceStatusSink(t *testing.T) {
	pkg := &ir.Package{Targets: []ir.Target{
		{Name: "plain", Kind: ir.KindCCLibrary}, // no spec
		{Name: "gen_version_h", Kind: ir.KindCMakeConfigureFile, CMakeConfigureFile: &ir.CMakeConfigureFileSpec{
			StampValues: map[string]string{"GIT_SHA": "STABLE_GIT_SHA", "BUILD_DATE": "VOLATILE_BUILD_DATE"},
		}},
	}}
	stampCommands := map[string]string{
		"STABLE_GIT_SHA":      "git rev-parse HEAD",
		"VOLATILE_BUILD_DATE": "date -u +%Y-%m-%d",
		"STABLE_UNUSED":       "git describe", // recorded but no template reads it
	}
	sink := map[string]string{"STALE": "x"} // must be reset
	populateWorkspaceStatusSink(sink, pkg, stampCommands)

	if _, stale := sink["STALE"]; stale {
		t.Error("sink not reset before population")
	}
	if sink["STABLE_GIT_SHA"] != "git rev-parse HEAD" || sink["VOLATILE_BUILD_DATE"] != "date -u +%Y-%m-%d" {
		t.Errorf("referenced keys not populated: %v", sink)
	}
	if _, ok := sink["STABLE_UNUSED"]; ok {
		t.Errorf("a recorded-but-unreferenced key must not enter the status script: %v", sink)
	}
}
