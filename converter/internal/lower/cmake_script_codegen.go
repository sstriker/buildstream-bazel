package lower

import (
	"context"
	"path/filepath"
	"strings"

	"github.com/sstriker/buildstream-bazel/converter/internal/ninja"
	"github.com/sstriker/buildstream-bazel/internal/shadow"
)

// maxScriptRecursionDepth bounds the `cmake -P` wrapper recursion
// (expandCommandSources). Real wrapper nesting is shallow — a generator script
// that shells out to one more generator script — so a generous cap catches a
// pathological / cyclic chain without truncating any genuine case. A branch that
// hits the cap contributes no calls, degrading to not-recovered (the
// bake/runner/refuse fallback), never worse than today.
const maxScriptRecursionDepth = 8

// recoverCmakeScriptCodegen recovers codegen hidden inside a user-authored
// `cmake -P <script>` wrapper — the trace-led recursive wrapper-codegen coverage
// (see ROADMAP). The earlier slices handled two levels: a cmake-GENERATED
// wrapper is unwrapped from the add_custom_command trace's real per-COMMAND argv
// (no re-exec), and a USER-authored `cmake -P` script is re-traced ONE level so
// its execute_process codegen is recognized. This is the FULL recursion (P3):
//
//   - expandCommandSources drives the descent. It re-traces the script and,
//     whenever a harvested execute_process is ITSELF a `cmake -P <inner>`
//     wrapper (a script that shells out to another script), recurses into the
//     inner script too — looping until every call bottoms out at a real tool
//     (protoc, …), with a visited-set (resolved script path) + depth cap.
//   - The bottomed-out leaf calls route through the SHARED recoverExecuteProcess
//     rather than only the codegen recognizer, so the script's `cmake -E` /
//     file() / configure_file / argv-declared / unspecified-output codegen is
//     recovered too — the same dispatch the main configure trace uses.
//
// Outputs land in cc (OutToGenrule for a recovered genrule, OutToNativeConsumerDep
// for a recognized native rule), so the consumer's reference to relOut is rewired
// by the same package-wide passes the other codegen paths rely on. Refusals from
// recoverExecuteProcess are discarded here: a leaf call we can't recover isn't an
// error, it just doesn't contribute relOut. Returns (name, true) ONLY when relOut
// is among the recovered outputs; otherwise the caller falls through to
// bake / runner / refuse, so the wrapper case is never worse than today.
//
// Opt-in + offline-safe: gated on RecognizeCodegen + CMakeScriptTrace + a usable
// cmake. Re-running the script (and its nested scripts) carries the same
// side-effect risk documented on TraceCmakeScript / --cmake-script-trace; with
// the flag off (or cmake absent) this declines and the legacy script paths handle
// the edge. The on-disk corroboration in the output-producing lifts is the
// backstop: a tool whose output isn't the real buildDir fails the stat and
// declines, rather than mis-emitting.
//
// NB: a nested `cmake -S … -B …` configure INSIDE a script still declines here.
// The nested-build lift (lowerNestedBuilds) runs as a top-level warm second pass
// that has already completed by the time a target's generated source re-traces
// its script, so such a call records into the sink too late to lift and falls
// through to refuse — never worse than today. See ROADMAP for that residual.
func (cc *codegenContext) recoverCmakeScriptCodegen(b *ninja.Build, cmd, scriptArg, cmakeSrc, buildDir, relOut string, g *ninja.Graph) (string, bool) {
	if cc == nil || !cc.RecognizeCodegen || !cc.CMakeScriptTrace || cc.CMakeBinary == "" || buildDir == "" {
		return "", false
	}
	if scriptArg == "" {
		return "", false
	}
	// NB: do NOT gate on the SCRIPT living under the source root. The script is
	// only re-traced at convert time and never becomes a Bazel input — the
	// recovered native rule references the recognized tool's SOURCE inputs (the
	// `.proto`), not the script — so a build-dir-generated / vendored script can
	// still drive an in-tree codegen tool. Correctness is gated downstream, not
	// on the script's path: the output-producing lifts anchor the tool's inputs
	// under the source root (an out-of-tree input yields no srcs → the recognizer
	// declines) and corroborate the derived outputs on disk under buildDir.
	// (Gating on the script's location is the over-strict check the runner/bake
	// genrule path needs — it stages the script AS a src — but it would wrongly
	// drop valid recoveries here.)
	dArgs := extractCmakePDashArgs(cmd)
	calls := cc.expandCommandSources(scriptArg, dArgs, cmakeSrc, buildDir)
	if len(calls) == 0 {
		return "", false
	}
	// Route the leaf calls through the shared recovery. cmakeVars /
	// forwardedStampVars are nil: the script's own -D args drove the trace
	// expansion already, and the codegen lifts key on the tool + on-disk
	// evidence, not on captured configure vars. A probe/stamp call that rides
	// along refuses harmlessly (discarded). liftEnabled mirrors the main
	// configure trace so a script's configure_file output lifts when opted in.
	outs, _ := recoverExecuteProcess(calls, cmakeSrc, cmakeSrc, buildDir, buildDir, cc.LiftConfigureFile, nil, nil, cc)
	for _, o := range outs {
		if o.RelOutput == relOut {
			// Same contract as recoverGenrule's recognized branch: register
			// SeenBuilds so a sibling consumer of this edge reuses, and return the
			// (unemitted) genrule identity. The consumer's reference to relOut is
			// stripped + rewired to the producing rule by the package-wide passes
			// (rewriteNativeRuleConsumers via OutToNativeConsumerDep, or the
			// OutToGenrule generated-source resolution); the returned name is
			// otherwise ignored by recoverGenrule's caller.
			name := genruleNameFor(b, buildDir)
			cc.SeenBuilds[b] = name
			return name, true
		}
	}
	// The recognizer + the shared lifts didn't claim relOut, but the wrapping
	// custom command DECLARES its outputs — enough data to lift even an
	// unrecognized tool. Substitute the traced tool command and reuse the shared
	// genrule emission (no cmake runner) before falling through to
	// bake/runner/refuse.
	if name, ok := cc.recoverTracedToolCommand(b, calls, cmakeSrc, buildDir, relOut, g); ok {
		return name, true
	}
	return "", false
}

// expandCommandSources re-traces a `cmake -P <script>` and returns its
// execute_process calls with nested `cmake -P` wrappers FLATTENED to their leaf
// tool calls — the P3 recursion driver. When a harvested call is itself a
// `cmake -P <inner>` wrapper (a script that calls a script), it is replaced by
// the inner script's own calls, recursively, until every returned call is a leaf
// (a real tool, not another cmake -P). A visited-set keyed on the resolved
// script path breaks cycles and a depth cap bounds runaway nesting. Returns RAW
// calls — the caller's recoverExecuteProcess does the normalization (dead
// captures cleared, cmake -E folded) every consumer expects.
func (cc *codegenContext) expandCommandSources(scriptArg string, dArgs []string, cmakeSrc, buildDir string) []shadow.ExecuteProcessCall {
	return cc.expandScriptCalls(scriptArg, dArgs, cmakeSrc, buildDir, map[string]bool{}, 0)
}

func (cc *codegenContext) expandScriptCalls(scriptArg string, dArgs []string, cmakeSrc, buildDir string, visited map[string]bool, depth int) []shadow.ExecuteProcessCall {
	if depth > maxScriptRecursionDepth {
		return nil
	}
	key := scriptArg
	if abs, err := filepath.Abs(scriptArg); err == nil {
		key = abs
	}
	if visited[key] {
		return nil
	}
	visited[key] = true

	traceRaw, err := TraceCmakeScript(context.Background(), cc.CMakeBinary, scriptArg, dArgs, "")
	if err != nil {
		return nil
	}
	// Harvest EVERY execute_process in the script trace, not just in-source-tree
	// call sites: when we re-trace a script, all of its calls are "the script's"
	// by definition, so the source-tree filter ExtractExecuteProcess applies (to
	// separate project calls from cmake-internal/subproject ones in the MAIN
	// configure trace) is the wrong filter here — it would drop the calls of a
	// script that lives outside the source root. Decode splits in-tree
	// (ExecuteProcesses) from out-of-tree (OutOfTreeExecuteProcesses); take both.
	dec := shadow.Decode(traceRaw, cmakeSrc, nil)
	harvested := append(append([]shadow.ExecuteProcessCall(nil), dec.ExecuteProcesses...), dec.OutOfTreeExecuteProcesses...)

	var leaves []shadow.ExecuteProcessCall
	for _, c := range harvested {
		if inner, innerD, ok := nestedCmakeScriptCall(c); ok {
			leaves = append(leaves, cc.expandScriptCalls(inner, innerD, cmakeSrc, buildDir, visited, depth+1)...)
			continue
		}
		leaves = append(leaves, c)
	}
	return leaves
}

// nestedCmakeScriptCall reports whether an execute_process call is itself a
// `cmake [-D…] -P <script>` wrapper — the nesting the recursion descends into —
// and returns the inner script path plus its `-D <var>=<val>` cache args (cmake
// exposes everything after the script as CMAKE_ARGV positionals; the -D pairs
// are what parameter-driven codegen scripts read). Mirrors usesCmakeScriptMode's
// driver gate, but on the argv form (execute_process records an argv, not a
// shell string). The ${CMAKE_COMMAND} literal is accepted alongside a resolved
// cmake path because a trace may carry either depending on expansion.
func nestedCmakeScriptCall(call shadow.ExecuteProcessCall) (script string, dArgs []string, ok bool) {
	if len(call.Commands) == 0 || len(call.Commands[0]) == 0 {
		return "", nil, false
	}
	argv := call.Commands[0]
	if executeProcessDriverBasename(argv[0]) != "cmake" && argv[0] != "${CMAKE_COMMAND}" {
		return "", nil, false
	}
	pIdx := -1
	for i := 1; i < len(argv); i++ {
		if argv[i] == "-P" && i+1 < len(argv) {
			pIdx = i + 1
			break
		}
	}
	if pIdx < 0 {
		return "", nil, false
	}
	script = argv[pIdx]
	for i := 1; i < len(argv); i++ {
		if i == pIdx || i == pIdx-1 { // the script path and its `-P`
			continue
		}
		tok := argv[i]
		if tok == "-D" && i+1 < len(argv) {
			dArgs = append(dArgs, "-D", argv[i+1])
			i++
			continue
		}
		if strings.HasPrefix(tok, "-D") {
			dArgs = append(dArgs, tok)
		}
	}
	return script, dArgs, true
}
