package bazel

import "testing"

// TestCompiledSourceExtsDriftGuard pins compiledSourceExts and documents its
// intended relationship to lower.ccSourceExts (the lowering-side compiled-source
// classifier). The two sets must agree on which extensions name a compiled
// translation unit: a file recognized as a TU in lowering but not in split-emit
// (or vice versa) gets classified inconsistently between the two passes. They
// are intentionally NOT auto-derived from each other (separate packages,
// separate concerns), so this guard and its twin in converter/internal/lower
// (TestCCSourceExtsDriftGuard) make both sets load-bearing under test: editing
// either one trips its guard, forcing whoever changes it to reconcile the pair.
//
// Known, intentional deltas vs lower.ccSourceExts (update here AND in the lower
// guard if these change):
//   - compiledSourceExts handles ".S" case-insensitively in split.go rather than
//     listing it (a capital-S asm is preprocessed then assembled).
//   - compiledSourceExts omits ".sx", which lower.ccSourceExts carries. This is
//     a pre-existing divergence flagged for review (PR description): if a project
//     ships .sx translation units, split-emit would not recognize them as
//     compiled sources. The guard pins the current behavior; reconcile the two
//     sets if the omission turns out to be unintentional.
func TestCompiledSourceExtsDriftGuard(t *testing.T) {
	want := map[string]bool{
		".c": true, ".cc": true, ".cpp": true, ".cxx": true, ".c++": true,
		".cu": true, ".cl": true, ".cppm": true, ".ixx": true,
		".s": true, ".asm": true,
	}
	for e := range want {
		if !compiledSourceExts[e] {
			t.Errorf("compiledSourceExts is missing %q — reconcile with lower.ccSourceExts and update this guard", e)
		}
	}
	for e := range compiledSourceExts {
		if !want[e] {
			t.Errorf("compiledSourceExts gained unexpected %q — reconcile with lower.ccSourceExts and update this guard", e)
		}
	}
}
