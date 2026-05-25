package configfold_test

import (
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/internal/configfold"
	"github.com/sstriker/buildstream-bazel/converter/internal/fileapi"
)

// TestProject_SingleTarget_DefinesPartition covers the canonical
// case: one target with a common define + per-config defines.
func TestProject_SingleTarget_DefinesPartition(t *testing.T) {
	byCfg := map[string]map[string]fileapi.Target{
		"foo::@": {
			"Release": fileapi.Target{
				Name: "foo",
				CompileGroups: []fileapi.CompileGroup{{
					Language: "C",
					Defines: []fileapi.CompileDefine{
						{Define: "NDEBUG"},
						{Define: "FOO=1"},
					},
				}},
			},
			"Debug": fileapi.Target{
				Name: "foo",
				CompileGroups: []fileapi.CompileGroup{{
					Language: "C",
					Defines: []fileapi.CompileDefine{
						{Define: "FOO=1"},
						{Define: "DEBUG=1"},
					},
				}},
			},
		},
	}
	out := configfold.Project(byCfg, []string{"Release", "Debug"})
	if len(out) != 1 {
		t.Fatalf("want 1 target; got %d", len(out))
	}
	fold := out[0]
	if fold.Name != "foo" {
		t.Errorf("Name: %q", fold.Name)
	}
	// Baseline: FOO=1 — observed in both configs with same key.
	if !fold.Defines.Baseline["FOO=1"] {
		t.Errorf("FOO=1 should be baseline; got %v", fold.Defines.Baseline)
	}
	// Deltas: NDEBUG → Release; DEBUG=1 → Debug.
	if !fold.Defines.Deltas["Release"]["NDEBUG"] {
		t.Errorf("NDEBUG should be Release delta; got %v", fold.Defines.Deltas)
	}
	if !fold.Defines.Deltas["Debug"]["DEBUG=1"] {
		t.Errorf("DEBUG=1 should be Debug delta; got %v", fold.Defines.Deltas)
	}
	// Baseline shouldn't contain the per-config-only entries.
	if fold.Defines.Baseline["NDEBUG"] || fold.Defines.Baseline["DEBUG=1"] {
		t.Errorf("Baseline leaked per-config defines: %v", fold.Defines.Baseline)
	}
}

// TestProject_TargetAbsentFromConfig covers the phantom-target
// case: target present in Debug but absent in Release. The target
// still surfaces in the output but all its facts route to the
// Debug deltas (cells that didn't observe the target contribute
// nothing).
func TestProject_TargetAbsentFromConfig(t *testing.T) {
	byCfg := map[string]map[string]fileapi.Target{
		"slow_test::@": {
			"Debug": fileapi.Target{
				Name: "slow_test",
				CompileGroups: []fileapi.CompileGroup{{
					Language: "CXX",
					Defines:  []fileapi.CompileDefine{{Define: "SLOW=1"}},
				}},
			},
		},
	}
	out := configfold.Project(byCfg, []string{"Debug", "Release"})
	if len(out) != 1 {
		t.Fatalf("want 1 target; got %d", len(out))
	}
	fold := out[0]
	// SLOW=1 isn't observed in Release, so it can't be baseline.
	if fold.Defines.Baseline["SLOW=1"] {
		t.Errorf("SLOW=1 should be Debug-only delta, not baseline")
	}
	if !fold.Defines.Deltas["Debug"]["SLOW=1"] {
		t.Errorf("SLOW=1 should be Debug delta; got %v", fold.Defines.Deltas)
	}
}

// TestProject_IncludesAndLinkFragments covers the other two fact
// families: includes (path strings) and link fragments
// (role-prefixed to keep "library" vs "flag" fragments separate).
func TestProject_IncludesAndLinkFragments(t *testing.T) {
	byCfg := map[string]map[string]fileapi.Target{
		"app::@": {
			"Release": fileapi.Target{
				Name: "app",
				CompileGroups: []fileapi.CompileGroup{{
					Language: "C",
					Includes: []fileapi.CompileInclude{
						{Path: "/src/include"},
						{Path: "/src/release-only"},
					},
				}},
				Link: &fileapi.TargetLink{
					Language: "C",
					CommandFragments: []fileapi.CommandFragment{
						{Fragment: "-lz", Role: "libraries"},
						{Fragment: "-O2", Role: "flags"},
					},
				},
			},
			"Debug": fileapi.Target{
				Name: "app",
				CompileGroups: []fileapi.CompileGroup{{
					Language: "C",
					Includes: []fileapi.CompileInclude{
						{Path: "/src/include"},
					},
				}},
				Link: &fileapi.TargetLink{
					Language: "C",
					CommandFragments: []fileapi.CommandFragment{
						{Fragment: "-lz", Role: "libraries"},
					},
				},
			},
		},
	}
	out := configfold.Project(byCfg, []string{"Release", "Debug"})
	fold := out[0]
	if !fold.Includes.Baseline["/src/include"] {
		t.Errorf("/src/include should be baseline include; got %v", fold.Includes)
	}
	if !fold.Includes.Deltas["Release"]["/src/release-only"] {
		t.Errorf("release-only include should be Release delta; got %v", fold.Includes.Deltas)
	}
	if !fold.LinkFragments.Baseline["libraries|-lz"] {
		t.Errorf("-lz library should be baseline; got %v", fold.LinkFragments.Baseline)
	}
	if !fold.LinkFragments.Deltas["Release"]["flags|-O2"] {
		t.Errorf("-O2 flag should be Release delta; got %v", fold.LinkFragments.Deltas)
	}
}

// TestProject_LanguagePrefixedCompileFragments confirms the
// per-language prefix lets the same fragment partition independently
// per language — e.g. -std=gnu++17 under CXX shouldn't be confused
// with a stray -std=gnu++17 under C.
func TestProject_LanguagePrefixedCompileFragments(t *testing.T) {
	byCfg := map[string]map[string]fileapi.Target{
		"mixed::@": {
			"Release": fileapi.Target{
				Name: "mixed",
				CompileGroups: []fileapi.CompileGroup{
					{
						Language:                "C",
						CompileCommandFragments: []fileapi.CommandFragment{{Fragment: "-O2"}},
					},
					{
						Language:                "CXX",
						CompileCommandFragments: []fileapi.CommandFragment{{Fragment: "-O2"}, {Fragment: "-std=gnu++17"}},
					},
				},
			},
			"Debug": fileapi.Target{
				Name: "mixed",
				CompileGroups: []fileapi.CompileGroup{
					{
						Language:                "C",
						CompileCommandFragments: []fileapi.CommandFragment{{Fragment: "-O2"}},
					},
				},
			},
		},
	}
	out := configfold.Project(byCfg, []string{"Release", "Debug"})
	fold := out[0]
	// C|-O2: in both configs → baseline.
	if !fold.CompileFragments.Baseline["C|-O2"] {
		t.Errorf("C|-O2 should be baseline; got %v", fold.CompileFragments)
	}
	// CXX|-O2: only in Release → Release delta.
	if !fold.CompileFragments.Deltas["Release"]["CXX|-O2"] {
		t.Errorf("CXX|-O2 should be Release delta; got %v", fold.CompileFragments.Deltas)
	}
	// CXX|-std=gnu++17: only in Release → Release delta.
	if !fold.CompileFragments.Deltas["Release"]["CXX|-std=gnu++17"] {
		t.Errorf("CXX|-std=gnu++17 should be Release delta")
	}
}

// TestProject_DeterministicOrder confirms target output order is
// sorted by id so callers can use the slice index as a stable key.
func TestProject_DeterministicOrder(t *testing.T) {
	byCfg := map[string]map[string]fileapi.Target{
		"zzz::@": {"Release": fileapi.Target{Name: "zzz"}},
		"aaa::@": {"Release": fileapi.Target{Name: "aaa"}},
		"mid::@": {"Release": fileapi.Target{Name: "mid"}},
	}
	out := configfold.Project(byCfg, []string{"Release"})
	want := []string{"aaa", "mid", "zzz"}
	for i, fold := range out {
		if fold.Name != want[i] {
			t.Errorf("out[%d].Name = %q, want %q", i, fold.Name, want[i])
		}
	}
}
