package lower

import (
	"testing"

	"github.com/sstriker/buildstream-bazel/internal/shadow"
)

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
