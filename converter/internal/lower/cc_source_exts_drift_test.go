package lower

import "testing"

// TestCCSourceExtsDriftGuard pins ccSourceExts (the lowering-side compiled-source
// extension set) and documents its intended relationship to
// emit/bazel.compiledSourceExts. The two must agree on which extensions name a
// compiled translation unit; see the twin guard
// (bazel.TestCompiledSourceExtsDriftGuard) for the rationale and the known deltas
// (split-emit handles ".S" case-insensitively and omits ".sx"). Editing either
// set trips its guard so the pair is reconciled deliberately.
func TestCCSourceExtsDriftGuard(t *testing.T) {
	want := map[string]bool{
		".cc": true, ".cpp": true, ".cxx": true, ".c++": true, ".c": true,
		".cu": true, ".cl": true, ".cppm": true, ".ixx": true,
		".s": true, ".sx": true, ".asm": true,
	}
	for e := range want {
		if !ccSourceExts[e] {
			t.Errorf("ccSourceExts is missing %q — reconcile with emit/bazel.compiledSourceExts and update this guard", e)
		}
	}
	for e := range ccSourceExts {
		if !want[e] {
			t.Errorf("ccSourceExts gained unexpected %q — reconcile with emit/bazel.compiledSourceExts and update this guard", e)
		}
	}
}
