package lower

import (
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/sstriker/buildstream-bazel/converter/internal/ninja"
	"github.com/sstriker/buildstream-bazel/converter/ir"
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
// Scope (decline → fall through to bake/runner/refuse, never worse than today):
// exactly ONE liftable tool call writing into the declared outputs' directory
// (>1 producer is ambiguous), the declared outputs all share one parent dir (the
// tool's output dir — the build root OR a build SUBDIR; anchorGenruleOutputDir
// Flags maps the `--out=<dir>` flag to $(RULEDIR)[/subdir]), and every declared
// output exists on disk under the build dir — the trace ran the tool, so its
// real outputs corroborate the declaration before we emit a genrule the build
// would otherwise reject. Rarer shapes (a bare positional output DIR, or a
// multi-tool chain whose final output is flag-derived) don't match the single-
// flag-writer selection and fall back gracefully; an argv-NAMED-output chain is
// already handled upstream by recoverExecuteProcess (pass A).
func (cc *codegenContext) recoverTracedToolCommand(b *ninja.Build, calls []shadow.ExecuteProcessCall, cmakeSrc, buildDir, relOut string, g *ninja.Graph) (string, bool) {
	declared := genruleOuts(b, buildDir)
	if len(declared) == 0 {
		return "", false
	}
	inDeclared := false
	outsParent := path.Dir(declared[0])
	for _, o := range declared {
		if o == relOut {
			inDeclared = true
		}
		if path.Dir(o) != outsParent {
			return "", false // v1: the declared outputs share one directory
		}
		if st, err := os.Stat(filepath.Join(buildDir, filepath.FromSlash(o))); err != nil || st.IsDir() {
			return "", false // declared output the trace didn't actually produce
		}
	}
	if !inDeclared {
		return "", false
	}
	anc := execAnchors{hostSrcDir: cmakeSrc, recordedSrcDir: cmakeSrc, hostBuildDir: buildDir, recordedBuildDir: buildDir, outerBuildDirs: cc.OuterBuildDirs}
	var chosen []string
	for _, raw := range calls {
		c := normalizeCMakeECall(clearDeadCaptures(raw, cc.DeadCaptureVars))
		if !argvCodegenEligibleRelaxed(c) || !argvToolLiftable(c.Commands[0][0], anc, cc) {
			continue
		}
		// The producer is the call that writes into the declared outputs' parent
		// directory (build root or a build SUBDIR) — a `--out=<dir>` flag value
		// or a bare positional dir anchoring to outsParent.
		if !argvWritesToDir(c.Commands[0], outsParent, anc) {
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
	_, genName, err := cc.emitRecoveredGenrule(b, strings.Join(chosen, " "), cmakeSrc, buildDir, relOut, g, nil)
	if err != nil {
		return "", false
	}
	if !cc.outputClaimed(relOut) {
		return "", false
	}
	cc.dropSubstitutedWrapperScriptSrc(genName)
	return cc.SeenBuilds[b], true
}

// argvWritesToDir reports whether some argv operand (a `--flag=value` value or a
// bare positional) names the build-relative directory dir — the directory the
// tool was told to write into. It pairs with the custom command's declared
// outputs (which all live under dir): the tool writes there, the declaration
// says what lands there. dir is "." for the build root.
func argvWritesToDir(argv []string, dir string, anc execAnchors) bool {
	for _, a := range argv[1:] {
		rel, anchored := executeProcessAnchorOutput(stripArgvPathPrefix(argvFlagValue(a)), anc)
		if !anchored {
			continue
		}
		if rel == "" {
			rel = "."
		}
		if rel == dir {
			return true
		}
	}
	return false
}

// dropSubstitutedWrapperScriptSrc removes the `cmake -P <script>` WRAPPER script
// from a tool-shape recovery's emitted genrule srcs. The tool-shape recoveries
// (recoverTracedToolCommand / recoverWriteInPlaceTool / recoverTempDirToolRelocate)
// SUBSTITUTE the real tool argv for `cmake -P <script>` and reuse
// emitRecoveredGenrule, but that derives srcs from the ninja edge's inputs
// (genruleSrcs), which still list the now-unused `.cmake` script the wrapper ran.
// The substituted tool is never cmake (argvToolLiftable excludes it) and
// emitRecoveredGenrule never sees a `cmake -P` cmd (recoverGenrule routes those to
// recoverCmakeScriptGenrule), so any `.cmake` src the genrule's cmd does not
// reference is the dead wrapper script — a spurious input that bloats the genrule
// and forces a needless re-run trigger. Drop it.
//
// Scoped to the emitted genrule (looked up by name): a no-op when the emission
// recognized a native rule instead (no genrule by that name) or the genrule
// carries no unreferenced `.cmake` src. Conservative — a `.cmake` the cmd DOES
// reference (a real tool that reads a `.cmake` config by path) is kept.
func (cc *codegenContext) dropSubstitutedWrapperScriptSrc(genName string) {
	if genName == "" {
		return
	}
	for i := range cc.Genrules {
		g := &cc.Genrules[i]
		if g.Name != genName || g.Kind != ir.KindGenrule {
			continue
		}
		kept := g.Srcs[:0:0]
		for _, s := range g.Srcs {
			if strings.HasSuffix(strings.ToLower(s), ".cmake") && !genruleCmdReferencesSrc(g.GenruleCmd, s) {
				continue
			}
			kept = append(kept, s)
		}
		g.Srcs = kept
		return
	}
}

// genruleCmdReferencesSrc reports whether a genrule cmd uses the src `s` — as a
// $(location)/$(execpath) expansion, a bare path occurrence, or its basename. A
// src the cmd never names can't be consumed by the (already-substituted) tool, so
// the wrapper-script drop treats an unreferenced `.cmake` as dead.
func genruleCmdReferencesSrc(cmd, s string) bool {
	if strings.Contains(cmd, "$(location "+s+")") || strings.Contains(cmd, "$(execpath "+s+")") {
		return true
	}
	if strings.Contains(cmd, s) {
		return true
	}
	return strings.Contains(cmd, path.Base(s))
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
