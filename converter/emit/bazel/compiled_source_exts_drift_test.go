package bazel

import (
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/internal/lower"
)

// compiledSourceExtsLowerOnly lists extensions intentionally present in
// lowering's lower.CCSourceExts but deliberately absent from split-emit's
// compiledSourceExts. Every divergence between the two compiled-source sets must
// be enumerated here with a rationale — TestCompiledSourceExtsMatchesLowering
// fails on any un-enumerated difference, so adding an extension to one set
// forces either adding it to the other or justifying the asymmetry here.
//
//   - ".sx": a preprocessed-assembly TU lowering recognizes (CCSourceExts) but
//     split-emit's compiledSourceExts currently omits. PRE-EXISTING divergence,
//     flagged for review (PR #506): if a project ships .sx TUs, split-emit won't
//     classify them as compiled. Remove this entry once the sets are reconciled.
var compiledSourceExtsLowerOnly = map[string]bool{
	".sx": true,
}

// TestCompiledSourceExtsMatchesLowering is the real drift guard: it mechanically
// enforces that split-emit's compiledSourceExts equals lowering's
// lower.CCSourceExts minus the enumerated, justified deltas. Unlike a per-set
// content pin, this fails whenever the two sets diverge in an un-enumerated way —
// so a future edit to either set (in either package) that isn't mirrored in the
// other trips the test.
func TestCompiledSourceExtsMatchesLowering(t *testing.T) {
	want := map[string]bool{}
	for e := range lower.CCSourceExts {
		if !compiledSourceExtsLowerOnly[e] {
			want[e] = true
		}
	}
	for e := range want {
		if !compiledSourceExts[e] {
			t.Errorf("compiledSourceExts is missing %q (present in lower.CCSourceExts and not listed as a known delta) — add it to compiledSourceExts or to compiledSourceExtsLowerOnly with a rationale", e)
		}
	}
	for e := range compiledSourceExts {
		if !want[e] {
			t.Errorf("compiledSourceExts has %q not in lower.CCSourceExts — add it to lower.CCSourceExts or reconcile the two sets", e)
		}
	}
	// Guard the delta list itself: every lower-only ext must actually be in
	// lowering's set (else the entry is stale) and must NOT be in compiledSourceExts.
	for e := range compiledSourceExtsLowerOnly {
		if !lower.CCSourceExts[e] {
			t.Errorf("compiledSourceExtsLowerOnly lists %q but it's not in lower.CCSourceExts — remove the stale delta entry", e)
		}
		if compiledSourceExts[e] {
			t.Errorf("compiledSourceExtsLowerOnly lists %q but compiledSourceExts also has it — the sets no longer diverge here; drop the delta entry", e)
		}
	}
}

// TestIsCompiledSourceExtBehavior makes the documented classifier behavior
// load-bearing: isCompiledSourceExt drives whether a generated TU is excluded
// from a synthesized header library, so the case-insensitive ".S" handling and
// the ".sx" omission must hold (this catches regressions like dropping the
// strings.ToLower normalization).
func TestIsCompiledSourceExtBehavior(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		{"foo.cpp", true},
		{"foo.c", true},
		{"foo.asm", true},
		{"foo.s", true},
		{"kernel/amax.S", true}, // capital-S asm: matched case-insensitively
		{"foo.sx", false},       // intentionally not recognized (see lower-only delta)
		{"foo.h", false},        // header, not a compiled TU
		{"foo.hpp", false},      // header
		{"noext", false},        // no extension
	}
	for _, c := range cases {
		if got := isCompiledSourceExt(c.path); got != c.want {
			t.Errorf("isCompiledSourceExt(%q) = %v, want %v", c.path, got, c.want)
		}
	}
}
