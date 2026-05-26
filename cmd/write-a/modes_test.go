package main

import (
	"strings"
	"testing"
)

func TestDeriveModes_StrictDefault(t *testing.T) {
	in := modeFlags{
		fidelity:   fidelityStrict,
		deployment: deploymentAuto,
		explicit:   map[string]bool{},
	}
	r, cmakeFB, mesonFB, pyprojectFB, traceR1, err := deriveModes(in)
	if err != nil {
		t.Fatalf("deriveModes: %v", err)
	}
	if cmakeFB || mesonFB || pyprojectFB {
		t.Errorf("strict defaults should leave all per-kind fallbacks off; got cmake=%v meson=%v py=%v",
			cmakeFB, mesonFB, pyprojectFB)
	}
	if !traceR1 {
		t.Errorf("deployment=auto with no publish+lookup should pick local (traceR1=true); got false")
	}
	if r.deployment != deploymentLocal {
		t.Errorf("resolved deployment = %q, want %q", r.deployment, deploymentLocal)
	}
	if len(r.downgrades) != 0 {
		t.Errorf("strict + auto with no tools should yield no downgrade notes; got %v", r.downgrades)
	}
}

func TestDeriveModes_BestEffortEnablesFallbacksWhenToolsWired(t *testing.T) {
	in := modeFlags{
		fidelity:            fidelityBestEffort,
		deployment:          deploymentAuto,
		buildTracer:         "/bin/build-tracer",
		tracePublish:        "/bin/trace-publish",
		traceLookup:         "/bin/trace-lookup",
		convertElementMeson: "/bin/convert-element-meson",
		pyprojectConverter:  "/bin/convert-element-pyproject",
		explicit:            map[string]bool{},
	}
	r, cmakeFB, mesonFB, pyprojectFB, traceR1, err := deriveModes(in)
	if err != nil {
		t.Fatalf("deriveModes: %v", err)
	}
	if !cmakeFB || !mesonFB || !pyprojectFB {
		t.Errorf("best-effort with tools wired should enable all per-kind fallbacks; got cmake=%v meson=%v py=%v",
			cmakeFB, mesonFB, pyprojectFB)
	}
	if traceR1 {
		t.Errorf("deployment=auto with publish+lookup should pick production (traceR1=false); got true")
	}
	if r.deployment != deploymentProduction {
		t.Errorf("resolved deployment = %q, want %q", r.deployment, deploymentProduction)
	}
}

func TestDeriveModes_BestEffortWithoutToolsDowngrades(t *testing.T) {
	// best-effort but no tracer/publish/lookup wired ⇒ cmake fallback
	// can't engage. Should surface a downgrade note, not error.
	in := modeFlags{
		fidelity:   fidelityBestEffort,
		deployment: deploymentAuto,
		explicit:   map[string]bool{},
	}
	r, cmakeFB, _, _, _, err := deriveModes(in)
	if err != nil {
		t.Fatalf("deriveModes: %v", err)
	}
	if cmakeFB {
		t.Errorf("cmake fallback should stay off when tools aren't wired (best-effort downgrades to strict for that kind)")
	}
	if len(r.downgrades) == 0 {
		t.Errorf("expected at least one downgrade note explaining the missing tools")
	}
	found := false
	for _, n := range r.downgrades {
		if strings.Contains(n, "kind:cmake fallback unavailable") {
			found = true
		}
	}
	if !found {
		t.Errorf("downgrade notes %v do not mention kind:cmake fallback unavailable", r.downgrades)
	}
}

func TestDeriveModes_ExplicitOverridesMode(t *testing.T) {
	// strict + explicit --cmake-round2-fallback=true ⇒ cmake fallback on.
	in := modeFlags{
		fidelity:            fidelityStrict,
		deployment:          deploymentAuto,
		cmakeRound2Fallback: true,
		explicit:            map[string]bool{"cmake-round2-fallback": true},
	}
	_, cmakeFB, _, _, _, err := deriveModes(in)
	if err != nil {
		t.Fatalf("deriveModes: %v", err)
	}
	if !cmakeFB {
		t.Errorf("explicit --cmake-round2-fallback=true under strict should still enable cmake fallback")
	}

	// best-effort + explicit --cmake-round2-fallback=false ⇒ cmake fallback off.
	in2 := modeFlags{
		fidelity:            fidelityBestEffort,
		deployment:          deploymentAuto,
		buildTracer:         "/bin/build-tracer",
		tracePublish:        "/bin/trace-publish",
		traceLookup:         "/bin/trace-lookup",
		cmakeRound2Fallback: false,
		explicit:            map[string]bool{"cmake-round2-fallback": true},
	}
	_, cmakeFB2, _, _, _, err := deriveModes(in2)
	if err != nil {
		t.Fatalf("deriveModes (best-effort, explicit false): %v", err)
	}
	if cmakeFB2 {
		t.Errorf("explicit --cmake-round2-fallback=false under best-effort should leave cmake fallback off")
	}
}

func TestDeriveModes_DeploymentLocal(t *testing.T) {
	in := modeFlags{
		fidelity:     fidelityStrict,
		deployment:   deploymentLocal,
		tracePublish: "/bin/trace-publish",
		traceLookup:  "/bin/trace-lookup",
		explicit:     map[string]bool{},
	}
	r, _, _, _, traceR1, err := deriveModes(in)
	if err != nil {
		t.Fatalf("deriveModes: %v", err)
	}
	if !traceR1 {
		t.Errorf("deployment=local should force traceR1=true even when publish+lookup are wired")
	}
	if r.deployment != deploymentLocal {
		t.Errorf("resolved deployment = %q, want %q", r.deployment, deploymentLocal)
	}
}

func TestDeriveModes_DeploymentProductionRejectsTraceRound1(t *testing.T) {
	in := modeFlags{
		fidelity:    fidelityStrict,
		deployment:  deploymentProduction,
		traceRound1: true,
		explicit:    map[string]bool{"trace-round1": true},
	}
	_, _, _, _, _, err := deriveModes(in)
	if err == nil {
		t.Fatalf("deriveModes accepted --deployment=production with --trace-round1; want error")
	}
	if !strings.Contains(err.Error(), "incompatible") {
		t.Errorf("error %q should explain the deployment / round-1 conflict", err)
	}
}

func TestDeriveModes_RejectsUnknownEnumValues(t *testing.T) {
	_, _, _, _, _, err := deriveModes(modeFlags{fidelity: "lax", deployment: deploymentAuto, explicit: map[string]bool{}})
	if err == nil || !strings.Contains(err.Error(), "--fidelity") {
		t.Errorf("expected --fidelity validation error, got %v", err)
	}
	_, _, _, _, _, err = deriveModes(modeFlags{fidelity: fidelityStrict, deployment: "staging", explicit: map[string]bool{}})
	if err == nil || !strings.Contains(err.Error(), "--deployment") {
		t.Errorf("expected --deployment validation error, got %v", err)
	}
}

func TestDeriveModes_AutoWithPlatformsNotesDowngrade(t *testing.T) {
	// --platforms requires production, but deployment=auto with no
	// publish+lookup downgrades to local. Surface that as a note so
	// the operator sees why their --platforms is about to fail.
	in := modeFlags{
		fidelity:      fidelityStrict,
		deployment:    deploymentAuto,
		platformsJSON: "/path/to/platforms.json",
		explicit:      map[string]bool{},
	}
	r, _, _, _, traceR1, err := deriveModes(in)
	if err != nil {
		t.Fatalf("deriveModes: %v", err)
	}
	if !traceR1 {
		t.Errorf("auto with no publish+lookup should pick local")
	}
	matched := false
	for _, n := range r.downgrades {
		if strings.Contains(n, "--deployment=auto picked local") {
			matched = true
		}
	}
	if !matched {
		t.Errorf("expected a downgrade note about --platforms wanting production; got %v", r.downgrades)
	}
}
