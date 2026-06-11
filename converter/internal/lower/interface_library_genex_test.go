package lower

import (
	"reflect"
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/internal/cmakerun"
	"github.com/sstriker/buildstream-bazel/internal/genexeval"
	"github.com/sstriker/buildstream-bazel/internal/shadow"
)

// TestLowerInterfaceLibraries_GenexDefineReconciledFromProbe pins
// the intent-preservation fix: an INTERFACE define carrying a genex
// the Go-side evaluator can't crack ($<$<CONFIG:Release>:RELEASE_ONLY=1>)
// is no longer dropped — it's replaced by cmake's own resolved
// INTERFACE_COMPILE_DEFINITIONS captured by the structural probe.
func TestLowerInterfaceLibraries_GenexDefineReconciledFromProbe(t *testing.T) {
	decoded := &shadow.Decoded{
		AddLibraries: []shadow.AddLibraryCall{
			{Name: "iface", Type: "INTERFACE"},
		},
		CompileDefinitions: []shadow.TargetCompileCall{
			{
				Target: "iface",
				Groups: []shadow.TargetCompileGroup{
					{Visibility: "INTERFACE", Items: []string{
						"PLAIN_DEF=1",
						"$<$<CONFIG:Release>:RELEASE_ONLY=1>",
						"$<$<CONFIG:Debug>:DEBUG_ONLY=1>",
					}},
				},
			},
		},
	}
	// cmake's own resolved value for the configured (Release) build.
	genexTargets := map[string]genexeval.TargetInfo{
		"iface": {Type: "INTERFACE_LIBRARY", InterfaceCompileDefinitions: "PLAIN_DEF=1;RELEASE_ONLY=1"},
	}
	got := lowerInterfaceLibraries(decoded, map[string]bool{}, "/src", "/src", "/src", genexTargets, nil, &codegenContext{HeaderWalkCache: map[string][]string{}, MissingIncludeDirs: map[string]bool{}})
	if len(got) != 1 {
		t.Fatalf("want 1 interface lib; got %d", len(got))
	}
	want := []string{"PLAIN_DEF=1", "RELEASE_ONLY=1"}
	if !reflect.DeepEqual(got[0].Defines, want) {
		t.Fatalf("defines = %v; want %v (the genex define must be resolved from the probe, not dropped)", got[0].Defines, want)
	}
}

// TestLowerInterfaceLibraries_GenexDefineDroppedWithoutProbe pins
// the unchanged fallback: with no probe data, an unresolvable genex
// define is still dropped (no literal $<...> reaches the compiler),
// exactly as before — the reconciliation is purely additive.
func TestLowerInterfaceLibraries_GenexDefineDroppedWithoutProbe(t *testing.T) {
	decoded := &shadow.Decoded{
		AddLibraries: []shadow.AddLibraryCall{
			{Name: "iface", Type: "INTERFACE"},
		},
		CompileDefinitions: []shadow.TargetCompileCall{
			{
				Target: "iface",
				Groups: []shadow.TargetCompileGroup{
					{Visibility: "INTERFACE", Items: []string{
						"PLAIN_DEF=1",
						"$<$<CONFIG:Release>:RELEASE_ONLY=1>",
					}},
				},
			},
		},
	}
	got := lowerInterfaceLibraries(decoded, map[string]bool{}, "/src", "/src", "/src", nil, nil, &codegenContext{HeaderWalkCache: map[string][]string{}, MissingIncludeDirs: map[string]bool{}})
	if len(got) != 1 {
		t.Fatalf("want 1 interface lib; got %d", len(got))
	}
	want := []string{"PLAIN_DEF=1"}
	if !reflect.DeepEqual(got[0].Defines, want) {
		t.Fatalf("defines = %v; want %v (no probe → genex define dropped, plain define kept)", got[0].Defines, want)
	}
}

// TestBuildGenexTargets_FoldsInterfaceLibraryProbe pins the
// supporting change in buildGenexTargets: a probe for an
// INTERFACE_LIBRARY (which the codemodel omits from targets[]) is
// folded into the result map rather than skipped, so the resolved
// INTERFACE_* aggregates reach lowerInterfaceLibraries.
func TestBuildGenexTargets_FoldsInterfaceLibraryProbe(t *testing.T) {
	probes := []cmakerun.GenexProbe{
		{
			Name: "iface",
			Type: "INTERFACE_LIBRARY",
			Interface: map[string]string{
				"COMPILE_DEFINITIONS": "PLAIN_DEF=1;RELEASE_ONLY=1",
			},
		},
		{
			// A non-INTERFACE probe with no codemodel entry must
			// still be skipped (codemodel is ground truth there).
			Name: "ghost",
			Type: "STATIC_LIBRARY",
			Interface: map[string]string{
				"COMPILE_DEFINITIONS": "SHOULD_NOT_APPEAR=1",
			},
		},
	}
	got := buildGenexTargets(nil, "", probes, nil, nil, "")
	ti, ok := got["iface"]
	if !ok {
		t.Fatalf("iface INTERFACE_LIBRARY probe should be folded into genexTargets; got keys %v", keysOfTargetInfo(got))
	}
	if ti.InterfaceCompileDefinitions != "PLAIN_DEF=1;RELEASE_ONLY=1" {
		t.Fatalf("iface InterfaceCompileDefinitions = %q; want PLAIN_DEF=1;RELEASE_ONLY=1", ti.InterfaceCompileDefinitions)
	}
	if _, ok := got["ghost"]; ok {
		t.Fatalf("non-INTERFACE probe with no codemodel entry must be skipped; ghost leaked in")
	}
}

func keysOfTargetInfo(m map[string]genexeval.TargetInfo) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
