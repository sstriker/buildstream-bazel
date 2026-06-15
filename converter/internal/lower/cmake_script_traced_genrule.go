package lower

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/sstriker/buildstream-bazel/converter/internal/ninja"
	"github.com/sstriker/buildstream-bazel/internal/shadow"
)

// recoverTracedToolCommand recovers a traced-but-unrecognized codegen tool
// inside a `cmake -P` wrapper by SUBSTITUTING the real tool command (recovered
// from the script trace) for `cmake -P <script>` and reusing the shared genrule
// emission (emitRecoveredGenrule). The wrapping custom command's DECLARED
// outputs (the ninja edge's outs) are the authority — "lift whenever there is
// enough data, in any form": the OUTPUT clause is enough, even when the tool
// derives its outputs from an output-dir flag no recognizer claims.
//
// Reuse, not parallel logic: emitRecoveredGenrule runs the same
// rewriteGenruleCmd / rewriteToolFromTarget / output-dir + file anchoring /
// recognizeOrGenrule the ordinary custom-command path uses, so a recognized tool
// still lowers to its native rule and an unrecognized one to a direct-tool
// genrule — neither needs --cmake-script-runner (we run the real tool, not
// `cmake -P`).
//
// v1 scope (decline → fall through to bake/runner/refuse, never worse than
// today): exactly ONE liftable tool call that writes into the build tree (>1
// producer is ambiguous), and every declared output exists on disk under the
// build dir — the trace ran the tool, so its real outputs corroborate the
// declaration before we emit a genrule the build would otherwise reject.
func (cc *codegenContext) recoverTracedToolCommand(b *ninja.Build, calls []shadow.ExecuteProcessCall, cmakeSrc, buildDir, relOut string, g *ninja.Graph) (string, bool) {
	declared := genruleOuts(b, buildDir)
	if len(declared) == 0 {
		return "", false
	}
	inDeclared := false
	for _, o := range declared {
		if o == relOut {
			inDeclared = true
		}
		if st, err := os.Stat(filepath.Join(buildDir, filepath.FromSlash(o))); err != nil || st.IsDir() {
			return "", false // declared output the trace didn't actually produce
		}
	}
	if !inDeclared {
		return "", false
	}
	anc := execAnchors{hostSrcDir: cmakeSrc, recordedSrcDir: cmakeSrc, hostBuildDir: buildDir, recordedBuildDir: buildDir}
	var chosen []string
	for _, raw := range calls {
		c := normalizeCMakeECall(clearDeadCaptures(raw, cc.DeadCaptureVars))
		if !argvCodegenEligibleRelaxed(c) || !argvToolLiftable(c.Commands[0][0], anc, cc) {
			continue
		}
		if !argvOutputAnchorsBuildRoot(c.Commands[0], anc) {
			continue
		}
		if chosen != nil {
			return "", false // ambiguous producer — don't guess
		}
		chosen = c.Commands[0]
	}
	if chosen == nil {
		return "", false
	}
	// Substitute the traced tool argv for `cmake -P <script>` and reuse the
	// shared emission. emitRecoveredGenrule declares the edge's outs, so relOut
	// is registered (OutToGenrule, or OutToNativeConsumerDep on a recognizer
	// match); confirm that before claiming success.
	if _, _, err := cc.emitRecoveredGenrule(b, strings.Join(chosen, " "), cmakeSrc, buildDir, relOut, g); err != nil {
		return "", false
	}
	if !cc.outputClaimed(relOut) {
		return "", false
	}
	return cc.SeenBuilds[b], true
}

// argvOutputAnchorsBuildRoot reports whether some argv operand (or the value of
// a `--flag=value` form) names the BUILD ROOT — the directory the tool was told
// to write into. It's the signal that pairs with the custom command's declared
// build-root outputs: the tool writes there, the declaration says what lands
// there. A `--flag=<builddir>` and a bare positional `<builddir>` both count.
func argvOutputAnchorsBuildRoot(argv []string, anc execAnchors) bool {
	for _, a := range argv[1:] {
		if rel, anchored := executeProcessAnchorOutput(stripArgvPathPrefix(argvFlagValue(a)), anc); anchored && (rel == "." || rel == "") {
			return true
		}
	}
	return false
}

// argvFlagValue returns the value of a `-flag=value` token, or the token itself
// when it isn't a flag assignment. Used to reach the path inside an
// `--out-dir=<dir>` operand.
func argvFlagValue(a string) string {
	if strings.HasPrefix(a, "-") {
		if eq := strings.IndexByte(a, '='); eq > 0 {
			return a[eq+1:]
		}
	}
	return a
}
