package lower

import (
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/internal/ninja"
)

// prewarmScriptBakes must serialize chained scripts (libpng's gensrc →
// genout shape: one script's output is the next one's input) while
// fanning independent scripts out in the first wave, and the bake must
// consume the cached results without re-executing. Uses `cmake -E true`…
// cmake isn't guaranteed on unit-test hosts, so exercise the WAVE
// assignment + cache shape with a fake cmake (a shell true) — the exec
// itself is runScriptExec, shared verbatim with the serial path.
func TestPrewarmScriptBakes_WavesAndCache(t *testing.T) {
	g := &ninja.Graph{
		Rules: map[string]*ninja.Rule{
			"CUSTOM_COMMAND": {Name: "CUSTOM_COMMAND", Bindings: map[string]string{"command": "$COMMAND"}},
		},
	}
	mk := func(out, cmd string, ins ...string) *ninja.Build {
		return &ninja.Build{
			Outputs:  []string{out},
			Rule:     "CUSTOM_COMMAND",
			Inputs:   ins,
			Bindings: map[string]string{"COMMAND": cmd},
		}
	}
	// a and b independent; c consumes a's output (must run in wave 1).
	a := mk("gen/a.c", "/usr/bin/cmake -DX=1 -P /src/gen_a.cmake")
	b := mk("gen/b.c", "/usr/bin/cmake -P /src/gen_b.cmake")
	c := mk("gen/c.c", "/usr/bin/cmake -P /src/gen_c.cmake", "gen/a.c")
	g.Builds = []*ninja.Build{a, b, c}

	cc := newCodegenContext()
	cc.CMakeBinary = "/bin/true" // every "script" run succeeds instantly
	prewarmScriptBakes(cc, g, t.TempDir())

	if len(cc.ScriptBakeRuns) != 3 {
		t.Fatalf("ScriptBakeRuns = %d entries, want 3: %v", len(cc.ScriptBakeRuns), cc.ScriptBakeRuns)
	}
	for _, bd := range []*ninja.Build{a, b, c} {
		if err, ok := cc.ScriptBakeRuns[bd]; !ok || err != nil {
			t.Errorf("build %v: cached result = (%v, %v), want (nil, true)", bd.Outputs, err, ok)
		}
	}
}

// scriptBakeWaves must compute MEMOIZED longest-path levels: a shared
// visited-set would under-level diamond graphs, letting a consumer land in
// its own producer's wave (the review's counterexample: chain Z→Y→X with
// Q(X) and P(X, Q) — a shared seen-set gives P level 3, tying Q; the true
// level is 4).
func TestScriptBakeWaves_DiamondLevels(t *testing.T) {
	mk := func(out string, ins ...string) scriptBakeCandidate {
		return scriptBakeCandidate{b: &ninja.Build{Outputs: []string{out}, Inputs: ins}}
	}
	cands := []scriptBakeCandidate{
		mk("z"),           // 0: level 0
		mk("y", "z"),      // 1: level 1
		mk("x", "y"),      // 2: level 2
		mk("q", "x"),      // 3: level 3
		mk("p", "x", "q"), // 4: level 4 — must sit strictly past Q
	}
	producedBy := map[string]int{"z": 0, "y": 1, "x": 2, "q": 3, "p": 4}
	got := scriptBakeWaves(cands, producedBy)
	want := []int{0, 1, 2, 3, 4}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("cand %d (%v): wave %d, want %d (full: %v)", i, cands[i].b.Outputs, got[i], want[i], got)
		}
	}
	if got[4] <= got[3] {
		t.Errorf("consumer P (wave %d) must run strictly after its producer Q (wave %d)", got[4], got[3])
	}
}
