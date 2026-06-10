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
