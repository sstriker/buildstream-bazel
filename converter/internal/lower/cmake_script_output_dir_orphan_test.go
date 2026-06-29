package lower

import (
	"reflect"
	"testing"
)

// TestUnclaimedConsumedOrphans pins the demand-side set: consumed build-dir
// sources MINUS anything a ninja edge produces MINUS anything already claimed —
// the only orphans the OUTPUT_DIR attribution may declare.
func TestUnclaimedConsumedOrphans(t *testing.T) {
	cc := &codegenContext{
		ConsumedBuildRel: map[string]bool{
			"gen/foo.c":      true, // orphan: consumed, no ninja edge, unclaimed
			"gen/foo.h":      true, // orphan
			"gen/built.o":    true, // a ninja edge produces it — NOT an orphan
			"gen/other.c":    true, // already claimed by another recovery
			"gen/manifest.x": true, // orphan (kept; the caller's dir/disk gate filters it)
		},
		NinjaOuts:    map[string]bool{"gen/built.o": true},
		OutToGenrule: map[string]string{"gen/other.c": "gen_other"},
	}
	got := cc.unclaimedConsumedOrphans()
	want := []string{"gen/foo.c", "gen/foo.h", "gen/manifest.x"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("unclaimedConsumedOrphans() = %v, want %v", got, want)
	}
}

// TestUnclaimedConsumedOrphans_Empty pins that with no demand (or everything
// produced/claimed) the orphan set is empty — the early decline.
func TestUnclaimedConsumedOrphans_Empty(t *testing.T) {
	cc := &codegenContext{
		ConsumedBuildRel: map[string]bool{"gen/x.c": true},
		NinjaOuts:        map[string]bool{"gen/x.c": true},
	}
	if got := cc.unclaimedConsumedOrphans(); len(got) != 0 {
		t.Fatalf("expected no orphans when every consumed source is a ninja out; got %v", got)
	}
}

// TestRecoverOutputDirOrphanEdges_GateOff pins that the pass is a strict no-op
// when the codegen flags are off (no re-trace, no cc mutation) — the
// "degrades to today" guarantee. No cmake binary is needed: the gate short-
// circuits before any re-trace.
func TestRecoverOutputDirOrphanEdges_GateOff(t *testing.T) {
	cc := &codegenContext{
		ConsumedBuildRel: map[string]bool{"gen/foo.c": true},
		// RecognizeCodegen / CMakeScriptTrace / CMakeBinary all unset.
	}
	cc.recoverOutputDirOrphanEdges(nil, "/src", "/build")
	if len(cc.Genrules) != 0 || len(cc.OutToGenrule) != 0 {
		t.Fatalf("gate-off pass must not emit; got genrules=%d outToGenrule=%d",
			len(cc.Genrules), len(cc.OutToGenrule))
	}
}
