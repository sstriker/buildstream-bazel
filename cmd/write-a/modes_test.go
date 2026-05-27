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
	r, err := deriveModes(in)
	if err != nil {
		t.Fatalf("deriveModes: %v", err)
	}
	if r.cmakeFallback || r.mesonFallback || r.pyprojectFallback {
		t.Errorf("strict defaults should leave all per-kind fallbacks off; got cmake=%v meson=%v py=%v",
			r.cmakeFallback, r.mesonFallback, r.pyprojectFallback)
	}
	if !r.traceRound1 {
		t.Errorf("deployment=auto with no publish+lookup should pick local (traceRound1=true); got false")
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
	r, err := deriveModes(in)
	if err != nil {
		t.Fatalf("deriveModes: %v", err)
	}
	if !r.cmakeFallback || !r.mesonFallback || !r.pyprojectFallback {
		t.Errorf("best-effort with tools wired should enable all per-kind fallbacks; got cmake=%v meson=%v py=%v",
			r.cmakeFallback, r.mesonFallback, r.pyprojectFallback)
	}
	if r.traceRound1 {
		t.Errorf("deployment=auto with publish+lookup should pick production (traceRound1=false); got true")
	}
	if r.deployment != deploymentProduction {
		t.Errorf("resolved deployment = %q, want %q", r.deployment, deploymentProduction)
	}
}

func TestDeriveModes_BestEffortWithoutToolsDowngrades(t *testing.T) {
	in := modeFlags{
		fidelity:   fidelityBestEffort,
		deployment: deploymentAuto,
		explicit:   map[string]bool{},
	}
	r, err := deriveModes(in)
	if err != nil {
		t.Fatalf("deriveModes: %v", err)
	}
	if r.cmakeFallback {
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

func TestDeriveModes_BestEffortWithFuseSourcesNoCmakeFallback(t *testing.T) {
	// --use-fuse-sources is incompatible with cmake-round2-fallback (see
	// main.go's explicit-flag rejection). deriveModes must NOT
	// auto-enable cmake fallback under best-effort + fuse, even with
	// every other tool wired.
	in := modeFlags{
		fidelity:       fidelityBestEffort,
		deployment:     deploymentAuto,
		buildTracer:    "/bin/build-tracer",
		tracePublish:   "/bin/trace-publish",
		traceLookup:    "/bin/trace-lookup",
		useFuseSources: true,
		explicit:       map[string]bool{},
	}
	r, err := deriveModes(in)
	if err != nil {
		t.Fatalf("deriveModes: %v", err)
	}
	if r.cmakeFallback {
		t.Errorf("cmake fallback should stay off under --use-fuse-sources to avoid the FUSE-template incompatibility")
	}
	found := false
	for _, n := range r.downgrades {
		if strings.Contains(n, "--use-fuse-sources") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected a downgrade note explaining the fuse-sources interaction; got %v", r.downgrades)
	}
}

func TestDeriveModes_ExplicitOverridesMode(t *testing.T) {
	in := modeFlags{
		fidelity:            fidelityStrict,
		deployment:          deploymentAuto,
		cmakeRound2Fallback: true,
		explicit:            map[string]bool{"cmake-round2-fallback": true},
	}
	r, err := deriveModes(in)
	if err != nil {
		t.Fatalf("deriveModes: %v", err)
	}
	if !r.cmakeFallback {
		t.Errorf("explicit --cmake-round2-fallback=true under strict should still enable cmake fallback")
	}

	in2 := modeFlags{
		fidelity:            fidelityBestEffort,
		deployment:          deploymentAuto,
		buildTracer:         "/bin/build-tracer",
		tracePublish:        "/bin/trace-publish",
		traceLookup:         "/bin/trace-lookup",
		cmakeRound2Fallback: false,
		explicit:            map[string]bool{"cmake-round2-fallback": true},
	}
	r2, err := deriveModes(in2)
	if err != nil {
		t.Fatalf("deriveModes (best-effort, explicit false): %v", err)
	}
	if r2.cmakeFallback {
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
	r, err := deriveModes(in)
	if err != nil {
		t.Fatalf("deriveModes: %v", err)
	}
	if !r.traceRound1 {
		t.Errorf("deployment=local should force traceRound1=true even when publish+lookup are wired")
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
	_, err := deriveModes(in)
	if err == nil {
		t.Fatalf("deriveModes accepted --deployment=production with --trace-round1=true; want error")
	}
	if !strings.Contains(err.Error(), "incompatible") {
		t.Errorf("error %q should explain the deployment / round-1 conflict", err)
	}
}

func TestDeriveModes_DeploymentLocalRejectsTraceRound1False(t *testing.T) {
	in := modeFlags{
		fidelity:    fidelityStrict,
		deployment:  deploymentLocal,
		traceRound1: false,
		explicit:    map[string]bool{"trace-round1": true},
	}
	_, err := deriveModes(in)
	if err == nil {
		t.Fatalf("deriveModes accepted --deployment=local with --trace-round1=false; want error")
	}
	if !strings.Contains(err.Error(), "incompatible") {
		t.Errorf("error %q should explain the deployment / round-1 conflict", err)
	}
}

func TestDeriveModes_RejectsUnknownEnumValues(t *testing.T) {
	_, err := deriveModes(modeFlags{fidelity: "lax", deployment: deploymentAuto, explicit: map[string]bool{}})
	if err == nil || !strings.Contains(err.Error(), "--fidelity") {
		t.Errorf("expected --fidelity validation error, got %v", err)
	}
	_, err = deriveModes(modeFlags{fidelity: fidelityStrict, deployment: "staging", explicit: map[string]bool{}})
	if err == nil || !strings.Contains(err.Error(), "--deployment") {
		t.Errorf("expected --deployment validation error, got %v", err)
	}
}

func TestDeriveModes_AutoWithPlatformsNotesDowngrade(t *testing.T) {
	in := modeFlags{
		fidelity:      fidelityStrict,
		deployment:    deploymentAuto,
		platformsJSON: "/path/to/platforms.json",
		explicit:      map[string]bool{},
	}
	r, err := deriveModes(in)
	if err != nil {
		t.Fatalf("deriveModes: %v", err)
	}
	if !r.traceRound1 {
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

func TestDeriveModes_BakeInDefaultsAndValidation(t *testing.T) {
	// Empty bakeIn resolves to "warn" (matches the CLI default).
	in := modeFlags{
		fidelity:   fidelityStrict,
		deployment: deploymentAuto,
		explicit:   map[string]bool{},
	}
	r, err := deriveModes(in)
	if err != nil {
		t.Fatalf("deriveModes: %v", err)
	}
	if r.bakeIn != bakeInWarn {
		t.Errorf("empty bakeIn should resolve to %q; got %q", bakeInWarn, r.bakeIn)
	}

	// Explicit values pass through.
	for _, v := range []string{bakeInAllow, bakeInWarn, bakeInReject} {
		in.bakeIn = v
		r, err = deriveModes(in)
		if err != nil {
			t.Errorf("deriveModes(bakeIn=%q): %v", v, err)
			continue
		}
		if r.bakeIn != v {
			t.Errorf("bakeIn=%q resolved to %q", v, r.bakeIn)
		}
	}

	// Unknown values reject.
	in.bakeIn = "bogus"
	_, err = deriveModes(in)
	if err == nil || !strings.Contains(err.Error(), "--bake-in") {
		t.Errorf("expected --bake-in validation error, got %v", err)
	}
}

func TestDeriveModes_AutoWithBestEffortDerivedFallbackTriggersDowngradeNote(t *testing.T) {
	// best-effort with the meson converter wired but trace publish/
	// lookup absent: mesonFB derives to false (tracePipelineToolsReady
	// is false) but the operator's intent was best-effort, so the auto
	// downgrade note should fire. Before the fix this checked raw
	// m.mesonRound2Fallback (always false here) and missed the case.
	in := modeFlags{
		fidelity:            fidelityBestEffort,
		deployment:          deploymentAuto,
		convertElementMeson: "/bin/convert-element-meson",
		explicit:            map[string]bool{},
	}
	r, err := deriveModes(in)
	if err != nil {
		t.Fatalf("deriveModes: %v", err)
	}
	matchedMeson := false
	for _, n := range r.downgrades {
		if strings.Contains(n, "kind:meson fallback unavailable") {
			matchedMeson = true
		}
	}
	if !matchedMeson {
		t.Errorf("expected per-kind meson downgrade note when best-effort + converter wired + tracer missing; got %v", r.downgrades)
	}
}
