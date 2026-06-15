package lower

import (
	"context"

	"github.com/sstriker/buildstream-bazel/converter/internal/ninja"
	"github.com/sstriker/buildstream-bazel/internal/shadow"
)

// recoverCmakeScriptCodegen recovers codegen hidden inside a user-authored
// `cmake -P <script>` wrapper — P2 of the trace-led recursive wrapper-codegen
// coverage (see ROADMAP). P1 unwraps a cmake-GENERATED wrapper whose real
// command the add_custom_command trace already records; this handles the case
// where the real tool (protoc, …) lives in an `execute_process` INSIDE the
// script, invisible to both the ninja edge (it just reads `cmake -P <script>`)
// and the add_custom_command record (it names the wrapper, not the tool).
//
// It re-traces the script at convert time, harvests its execute_process calls,
// and routes each through the execute_process codegen recognizer
// (liftRecognizedExecuteProcessCodegen): a recognized tool whose derived outputs
// corroborate against the configure's on-disk artifacts under buildDir lowers to
// its native rule, and consumers are rewired off the generated source via
// OutToNativeConsumerDep — the same package-wide rewriteNativeRuleConsumers pass
// the other codegen paths use. Returns (name, true) ONLY when the consumed
// output relOut is among the recovered outputs; otherwise the caller falls
// through to bake / runner / refuse, so the wrapper case is never worse than
// today.
//
// Opt-in + offline-safe: gated on RecognizeCodegen + CMakeScriptTrace + a usable
// cmake. Re-running the script carries the same side-effect risk documented on
// TraceCmakeScript / --cmake-script-trace; with the flag off (or cmake absent)
// this declines and the legacy script paths handle the edge. The on-disk
// corroboration in liftRecognizedExecuteProcessCodegen is the backstop: a script
// whose tool writes a relative / scratch-dir output (not the real buildDir)
// fails the stat and declines, rather than mis-emitting.
func (cc *codegenContext) recoverCmakeScriptCodegen(b *ninja.Build, cmd, scriptArg, cmakeSrc, buildDir, relOut string) (string, bool) {
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
	// on the script's path: liftRecognizedExecuteProcessCodegen anchors the
	// tool's inputs under the source root (an out-of-tree input yields no srcs →
	// the recognizer declines) and corroborates the derived outputs on disk under
	// buildDir. (Gating on the script's location is the over-strict check the
	// runner/bake genrule path needs — it stages the script AS a src — but it
	// would wrongly drop valid recoveries here.)
	dArgs := extractCmakePDashArgs(cmd)
	traceRaw, err := TraceCmakeScript(context.Background(), cc.CMakeBinary, scriptArg, dArgs, "")
	if err != nil {
		return "", false
	}
	// Harvest EVERY execute_process in the script trace, not just in-source-tree
	// call sites: when we re-trace a script, all of its calls are "the script's"
	// by definition, so the source-tree filter ExtractExecuteProcess applies (to
	// separate project calls from cmake-internal/subproject ones in the MAIN
	// configure trace) is the wrong filter here — it would drop the calls of a
	// script that lives outside the source root. Decode splits in-tree
	// (ExecuteProcesses) from out-of-tree (OutOfTreeExecuteProcesses); take both.
	// cmake-internal probe noise that rides along declines harmlessly (the
	// recognizer only claims corroborated codegen tools).
	dec := shadow.Decode(traceRaw, cmakeSrc, nil)
	calls := append(append([]shadow.ExecuteProcessCall(nil), dec.ExecuteProcesses...), dec.OutOfTreeExecuteProcesses...)
	if len(calls) == 0 {
		return "", false
	}
	// Mirror recoverExecuteProcess's preprocessing so the recognizer sees the
	// same normalized calls (dead captures cleared, cmake -E wrappers folded).
	anc := execAnchors{hostSrcDir: cmakeSrc, recordedSrcDir: cmakeSrc, hostBuildDir: buildDir, recordedBuildDir: buildDir}
	recovered := map[string]bool{}
	for _, c := range calls {
		c = normalizeCMakeECall(clearDeadCaptures(c, cc.DeadCaptureVars))
		if rels, ok := liftRecognizedExecuteProcessCodegen(c, anc, cc); ok {
			for _, r := range rels {
				recovered[r] = true
			}
		}
	}
	if !recovered[relOut] {
		return "", false
	}
	// Same contract as recoverGenrule's recognized branch: register SeenBuilds so
	// a sibling consumer of this edge reuses, and return the (unemitted) genrule
	// identity. The consumer's reference to relOut is stripped + rewired to the
	// native rule by rewriteNativeRuleConsumers via OutToNativeConsumerDep; no
	// OutToGenrule entry is made, so nothing depends on this name.
	name := genruleNameFor(b, buildDir)
	cc.SeenBuilds[b] = name
	return name, true
}
