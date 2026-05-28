package main

import (
	"strings"
	"testing"
)

func TestDeriveModes_StrictDefault(t *testing.T) {
	in := modeFlags{
		fidelity:   fidelityStrict,
		bakeIn:     bakeInWarn,
		deployment: deploymentAuto,
		explicit:   map[string]bool{},
	}
	r, err := deriveModes(in)
	if err != nil {
		t.Fatalf("deriveModes: %v", err)
	}
	if r.fidelity != fidelityStrict || r.bakeIn != bakeInWarn {
		t.Errorf("default dials = (%q, %q), want (%q, %q)", r.fidelity, r.bakeIn, fidelityStrict, bakeInWarn)
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

func TestDeriveModes_EmptyDialsResolveToDefaults(t *testing.T) {
	// Empty strings on every enum dial should resolve via convmode
	// to the documented defaults, so tests building modeFlags by
	// hand don't have to set every field.
	in := modeFlags{deployment: deploymentAuto, explicit: map[string]bool{}}
	r, err := deriveModes(in)
	if err != nil {
		t.Fatalf("deriveModes: %v", err)
	}
	if r.fidelity != fidelityStrict {
		t.Errorf("empty fidelity = %q, want %q", r.fidelity, fidelityStrict)
	}
	if r.bakeIn != bakeInWarn {
		t.Errorf("empty bakeIn = %q, want %q", r.bakeIn, bakeInWarn)
	}
}

func TestDeriveModes_AutoPicksProductionWhenToolsWired(t *testing.T) {
	in := modeFlags{
		fidelity:     fidelityBestEffort,
		bakeIn:       bakeInWarn,
		deployment:   deploymentAuto,
		buildTracer:  "/bin/build-tracer",
		tracePublish: "/bin/trace-publish",
		traceLookup:  "/bin/trace-lookup",
		explicit:     map[string]bool{},
	}
	r, err := deriveModes(in)
	if err != nil {
		t.Fatalf("deriveModes: %v", err)
	}
	if r.traceRound1 {
		t.Errorf("auto with publish+lookup should pick production (traceRound1=false); got true")
	}
	if r.deployment != deploymentProduction {
		t.Errorf("resolved deployment = %q, want %q", r.deployment, deploymentProduction)
	}
}

func TestDeriveModes_AutoDowngradeNotePromptedByBestEffort(t *testing.T) {
	// Pass-through: deriveModes doesn't second-guess what best-
	// effort means per-kind, but it does surface a downgrade note
	// when auto picks local AND any indicator (best-effort,
	// explicit fallback flag, trace converter, platforms) says
	// "this run wanted round-2".
	in := modeFlags{
		fidelity:   fidelityBestEffort,
		bakeIn:     bakeInWarn,
		deployment: deploymentAuto,
		explicit:   map[string]bool{},
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
		t.Errorf("expected downgrade note explaining the auto pick; got %v", r.downgrades)
	}
}

func TestDeriveModes_DeploymentLocalForcesTraceRound1(t *testing.T) {
	in := modeFlags{
		fidelity:     fidelityStrict,
		bakeIn:       bakeInWarn,
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

func TestDeriveModes_ProductionRejectsTraceRound1True(t *testing.T) {
	in := modeFlags{
		fidelity:    fidelityStrict,
		bakeIn:      bakeInWarn,
		deployment:  deploymentProduction,
		traceRound1: true,
		explicit:    map[string]bool{"trace-round1": true},
	}
	_, err := deriveModes(in)
	if err == nil || !strings.Contains(err.Error(), "incompatible") {
		t.Errorf("expected production + trace-round1=true contradiction; got %v", err)
	}
}

func TestDeriveModes_LocalRejectsTraceRound1False(t *testing.T) {
	in := modeFlags{
		fidelity:    fidelityStrict,
		bakeIn:      bakeInWarn,
		deployment:  deploymentLocal,
		traceRound1: false,
		explicit:    map[string]bool{"trace-round1": true},
	}
	_, err := deriveModes(in)
	if err == nil || !strings.Contains(err.Error(), "incompatible") {
		t.Errorf("expected local + trace-round1=false contradiction; got %v", err)
	}
}

func TestDeriveModes_BakeInRejectUnderFuseSourcesDowngrades(t *testing.T) {
	in := modeFlags{
		fidelity:       fidelityStrict,
		bakeIn:         bakeInReject,
		deployment:     deploymentAuto,
		useFuseSources: true,
		explicit:       map[string]bool{},
	}
	r, err := deriveModes(in)
	if err != nil {
		t.Fatalf("deriveModes: %v", err)
	}
	matched := false
	for _, n := range r.downgrades {
		if strings.Contains(n, "--use-fuse-sources") && strings.Contains(n, "reject") {
			matched = true
		}
	}
	if !matched {
		t.Errorf("expected downgrade note about bake-in=reject under FUSE; got %v", r.downgrades)
	}
}

func TestDeriveModes_RejectsUnknownEnumValues(t *testing.T) {
	cases := []struct {
		name string
		in   modeFlags
		want string
	}{
		{"bad fidelity", modeFlags{fidelity: "lax", bakeIn: bakeInWarn, deployment: deploymentAuto, explicit: map[string]bool{}}, "--fidelity"},
		{"bad bake-in", modeFlags{fidelity: fidelityStrict, bakeIn: "bogus", deployment: deploymentAuto, explicit: map[string]bool{}}, "--bake-in"},
		{"bad deployment", modeFlags{fidelity: fidelityStrict, bakeIn: bakeInWarn, deployment: "staging", explicit: map[string]bool{}}, "--deployment"},
	}
	for _, c := range cases {
		_, err := deriveModes(c.in)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: expected error containing %q; got %v", c.name, c.want, err)
		}
	}
}
