package lower

import (
	"context"
	"io"
	"os"
	"os/exec"
	"runtime"
	"sync"
	"time"

	"github.com/sstriker/buildstream-bazel/converter/internal/ninja"
)

// Parallel pre-warm for the cmake -P script bakes (--cmake-script-bake).
//
// The conversion-latency profile (VTK, multi-config) showed translation
// wall time dominated by SEQUENTIAL cmake -P subprocess waits: 238 script
// runs ≈ 95s of a 126s translation, with the converter on-CPU for only a
// fraction of it. The runs are independent processes — vtkEncodeString /
// vtkHashSource each transform one source-tree file into one output — so
// they parallelize cleanly EXCEPT where one script consumes another's
// output (libpng's gensrc → genout → genchk chain, the same relation
// bakeProducerChain walks serially).
//
// prewarmScriptBakes therefore executes the candidates in dependency WAVES:
// candidates whose inputs are produced by other candidates wait for their
// producers' wave; within a wave a bounded worker pool runs the scripts
// concurrently. Results land in cc.ScriptBakeRuns keyed by the build
// statement, and bakeCmakeScriptGenrule consults the cache before its own
// serial exec — so the lazy recovery path keeps its exact contract (same
// argv, workDir, env, failure surface) and an un-prewarmed build (e.g. a
// shape the candidate scan missed) still runs serially.
//
// Scripts run in the shared build dir, same as the serial bake. Two
// same-wave scripts racing on a shared scratch path is theoretically
// possible for pathological scripts; the bake is opt-in
// (--cmake-script-bake) and the corpus shapes (VTK encode/hash, libpng's
// CHAINED scripts — serialized by the wave order) are disjoint-output.

// runScriptExec is the one true cmake -P execution: shared by the serial
// bake fallback and the pre-warm pool so both run byte-identical commands.
// The 60s timeout and the minimal sandboxed env mirror the historical
// inline exec.
func runScriptExec(cmakeBin string, argv []string, workDir string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	exe := exec.CommandContext(ctx, cmakeBin, argv...)
	exe.Dir = workDir
	exe.Env = []string{
		"HOME=" + workDir,
		"LC_ALL=C",
		"PATH=" + os.Getenv("PATH"),
	}
	exe.Stdout = io.Discard
	exe.Stderr = io.Discard
	return exe.Run()
}

// scriptBakeCandidate is one pre-warm unit: a CUSTOM_COMMAND build whose
// command is a bakeable `cmake -P` shape, with the exec plan the serial
// bake would construct for it.
type scriptBakeCandidate struct {
	b       *ninja.Build
	argv    []string
	workDir string
}

// scriptBakeWaves computes each candidate's dependency wave: the memoized
// longest producer-path level (0 = no candidate-produced inputs). Memoized
// per node — a shared visited-set would UNDER-level diamond graphs (a node
// reached first through a sibling's walk would re-report level 0, letting a
// consumer land in its own producer's wave: P consuming both X and Q(X)
// must sit one past Q, not tie it). A separate on-stack set guards the
// defensive cycle case (impossible in a valid ninja graph).
func scriptBakeWaves(cands []scriptBakeCandidate, producedBy map[string]int) []int {
	levels := make([]int, len(cands))
	for i := range levels {
		levels[i] = -1
	}
	onStack := make([]bool, len(cands))
	var levelOf func(i int) int
	levelOf = func(i int) int {
		if levels[i] >= 0 {
			return levels[i]
		}
		if onStack[i] {
			return 0
		}
		onStack[i] = true
		lvl := 0
		for _, in := range cands[i].b.Inputs {
			if p, ok := producedBy[in]; ok && p != i {
				if l := levelOf(p) + 1; l > lvl {
					lvl = l
				}
			}
		}
		for _, in := range cands[i].b.ImplicitInputs {
			if p, ok := producedBy[in]; ok && p != i {
				if l := levelOf(p) + 1; l > lvl {
					lvl = l
				}
			}
		}
		onStack[i] = false
		levels[i] = lvl
		return lvl
	}
	for i := range cands {
		levelOf(i)
	}
	return levels
}

// prewarmScriptBakes scans g for bakeable cmake -P custom commands and
// executes them in dependency waves with a bounded pool, filling
// cc.ScriptBakeRuns. No-op without a real build dir, a cmake binary, or
// the bake flag (callers gate on cc.CMakeScriptBake).
func prewarmScriptBakes(cc *codegenContext, g *ninja.Graph, buildDir string) {
	if g == nil || buildDir == "" || cc.CMakeBinary == "" {
		return
	}
	// Collect candidates + index their outputs for the wave edges. The
	// genruleOuts gate mirrors the serial bake's own pre-exec bail so the
	// pre-warm never executes a script the serial path wouldn't. One
	// deliberate over-execution remains: a script that a native recognizer
	// (cc_embed / cc_hash) intercepts ahead of the bake never execs
	// serially, but the pre-warm can't know the recognizer outcome up
	// front and runs it anyway — wasted (idempotent) subprocess work when
	// both opt-ins overlap, accepted for the parallel win.
	var cands []scriptBakeCandidate
	producedBy := map[string]int{} // output path -> index into cands
	for _, b := range g.Builds {
		if b.Rule != "CUSTOM_COMMAND" {
			continue
		}
		cmd, ok := ninja.CommandFor(g, b)
		if !ok || cmd == "" {
			continue
		}
		script := extractCmakeScriptPath(cmd)
		if script == "" {
			continue
		}
		if len(genruleOuts(b, buildDir)) == 0 {
			continue
		}
		var workDir string
		if cd := extractCdDir(cmd); cd != "" && dirExists(cd) {
			workDir = cd
		} else {
			workDir = buildDir
		}
		argv := append([]string{}, extractCmakePDashArgs(cmd)...)
		argv = append(argv, "-P", script)
		argv = append(argv, extractCmakePScriptPositionalArgs(cmd)...)
		idx := len(cands)
		cands = append(cands, scriptBakeCandidate{b: b, argv: argv, workDir: workDir})
		for _, out := range b.Outputs {
			producedBy[out] = idx
		}
	}
	if len(cands) == 0 {
		return
	}
	// Wave assignment: a candidate consuming another candidate's output
	// runs strictly after it (longest-path level — the libpng chain
	// serializes; the VTK fan-out all lands in wave 0).
	wave := scriptBakeWaves(cands, producedBy)
	maxWave := 0
	for _, w := range wave {
		if w > maxWave {
			maxWave = w
		}
	}
	if cc.ScriptBakeRuns == nil {
		cc.ScriptBakeRuns = make(map[*ninja.Build]error, len(cands))
	}
	workers := runtime.NumCPU()
	if workers < 2 {
		workers = 2
	}
	var mu sync.Mutex
	for w := 0; w <= maxWave; w++ {
		sem := make(chan struct{}, workers)
		var wg sync.WaitGroup
		for i := range cands {
			if wave[i] != w {
				continue
			}
			wg.Add(1)
			sem <- struct{}{}
			go func(c scriptBakeCandidate) {
				defer wg.Done()
				defer func() { <-sem }()
				err := runScriptExec(cc.CMakeBinary, c.argv, c.workDir)
				mu.Lock()
				cc.ScriptBakeRuns[c.b] = err
				mu.Unlock()
			}(cands[i])
		}
		wg.Wait()
	}
}
