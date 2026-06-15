package lower

import (
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/sstriker/buildstream-bazel/converter/internal/ninja"
	"github.com/sstriker/buildstream-bazel/converter/ir"
	"github.com/sstriker/buildstream-bazel/internal/shadow"
)

// liftTracedToolDeclaredOutputs lifts a traced-but-UNRECOGNIZED codegen tool
// inside a `cmake -P` script into a direct-tool genrule, using the wrapping
// custom command's DECLARED outputs (the ninja edge's outs) as the output
// authority. It is the runner-free fallback the codegen path takes when the
// recognizer + the shared execute_process lifts can't claim relOut but the
// trace still gave us a real tool call and the custom command told us exactly
// which files it must produce — "lift whenever there is enough data, in any
// form": the OUTPUT clause is enough data, even when the tool derives its
// outputs from an output-dir flag the argv-output lift can't read.
//
// Unlike the runner-genrule fallback (liftCmakeScriptGenrule), this needs NO
// --cmake-script-runner: it runs the observed tool directly at Bazel build time
// (the tool the project already depends on), not `cmake -P <script>`. The
// output dir the tool wrote to is rewritten to $(RULEDIR); the tool's source
// inputs are staged via $(location).
//
// v1 scope (decline → fall through to bake/runner/refuse, never worse than
// today): build-ROOT declared outputs only (so the genrule lands at the package
// root and the output-dir → $(RULEDIR) rewrite is unambiguous), and exactly ONE
// liftable tool call that writes to the build root (>1 producer is ambiguous).
// Every declared output must exist on disk under the build dir — the trace ran
// the tool, so its real outputs corroborate the declaration, the same on-disk
// backstop the recognizer uses. A sub-package output dir and multi-call chains
// are follow-ups (see ROADMAP).
func (cc *codegenContext) liftTracedToolDeclaredOutputs(b *ninja.Build, calls []shadow.ExecuteProcessCall, cmakeSrc, buildDir, relOut string) (string, bool) {
	declared := genruleOuts(b, buildDir)
	if len(declared) == 0 {
		return "", false
	}
	inDeclared := false
	for _, o := range declared {
		if strings.Contains(o, "/") {
			return "", false // v1: build-root outputs only
		}
		if o == relOut {
			inDeclared = true
		}
	}
	if !inDeclared {
		return "", false
	}
	// On-disk corroboration: the trace ran the tool, so every declared output
	// exists under the build dir. A declaration the trace didn't actually
	// produce declines rather than emitting a genrule Bazel would reject.
	for _, o := range declared {
		if st, err := os.Stat(filepath.Join(buildDir, filepath.FromSlash(o))); err != nil || st.IsDir() {
			return "", false
		}
	}
	anc := execAnchors{hostSrcDir: cmakeSrc, recordedSrcDir: cmakeSrc, hostBuildDir: buildDir, recordedBuildDir: buildDir}
	// Find THE single liftable tool call that writes to the build root.
	var chosen shadow.ExecuteProcessCall
	found := false
	for _, raw := range calls {
		c := normalizeCMakeECall(clearDeadCaptures(raw, cc.DeadCaptureVars))
		if !argvCodegenEligibleRelaxed(c) || !argvToolLiftable(c.Commands[0][0], anc, cc) {
			continue
		}
		if !argvOutputAnchorsBuildRoot(c.Commands[0], anc) {
			continue
		}
		if found {
			return "", false // ambiguous producer — don't guess
		}
		chosen, found = c, true
	}
	if !found {
		return "", false
	}
	srcs, tools, cmd, ok := cc.rewriteTracedToolCmd(chosen.Commands[0], anc)
	if !ok {
		return "", false
	}
	// In-place guard: Bazel rejects a file that is both a src and an out.
	declaredSet := map[string]bool{}
	for _, o := range declared {
		declaredSet[o] = true
	}
	for _, s := range srcs {
		if declaredSet[s] {
			return "", false
		}
	}
	// Idempotency across duplicate trace calls (mirrors the other lifts).
	claimed := 0
	for _, o := range declared {
		if cc.outputClaimed(o) {
			claimed++
		}
	}
	name := genruleNameFor(b, buildDir)
	if claimed == len(declared) {
		cc.SeenBuilds[b] = name
		return name, true
	}
	if claimed > 0 {
		return "", false
	}
	sort.Strings(declared)
	cc.Genrules = append(cc.Genrules, ir.Target{
		Name:         name,
		Kind:         ir.KindGenrule,
		Srcs:         srcs,
		GenruleCmd:   cmd,
		GenruleOuts:  declared,
		GenruleTools: tools,
		Tags:         []string{"cmake-codegen-cmake-script-traced-tool"},
		Visibility:   []string{"//visibility:private"},
	})
	for _, o := range declared {
		cc.OutToGenrule[o] = name
	}
	cc.SeenBuilds[b] = name
	return name, true
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

// rewriteTracedToolCmd renders the genrule cmd for a traced tool argv: the
// build-root output dir → $(RULEDIR); source-tree FILE inputs → srcs +
// $(location); a build-dir input another recovery produces → srcs +
// $(location); an absolute tool path → its basename (PATH portability). A
// source-tree DIRECTORY operand declines (a literal path can't be staged, so a
// dir-scanning tool would see an empty view under the sandbox — the same guard
// rewriteArgvCodegen takes). After building the argv it runs the tool-from-
// target rewrite so a manifest/in-tree tool maps to $(execpath <label>) + tools.
func (cc *codegenContext) rewriteTracedToolCmd(argv []string, anc execAnchors) (srcs, tools []string, cmd string, ok bool) {
	srcSet := map[string]bool{}
	addSrc := func(rel string) {
		if !srcSet[rel] {
			srcSet[rel] = true
			srcs = append(srcs, rel)
		}
	}
	emitKeyed := func(a, repl string) string {
		if strings.HasPrefix(a, "-") {
			if eq := strings.IndexByte(a, '='); eq > 0 {
				return a[:eq+1] + repl
			}
		}
		return repl
	}
	var rewritten []string
	for i, a := range argv {
		if i == 0 {
			if filepath.IsAbs(a) {
				rewritten = append(rewritten, shellQuoteArg(filepath.Base(a)))
			} else {
				rewritten = append(rewritten, shellQuoteArg(a))
			}
			continue
		}
		val := argvFlagValue(a)
		p := stripArgvPathPrefix(val)
		if rel, anchored := executeProcessAnchorOutput(p, anc); anchored {
			if rel == "." || rel == "" {
				rewritten = append(rewritten, emitKeyed(a, "$(RULEDIR)"))
				continue
			}
			// A non-root build-dir operand: a generated input another recovery
			// produces, referenced via its location. (A build-dir OUTPUT under a
			// subdir is out of v1 scope — the build-root guard above declines it.)
			addSrc(rel)
			rewritten = append(rewritten, emitKeyed(a, "$(location "+rel+")"))
			continue
		}
		if rel, anchored := executeProcessAnchorSource(p, anc); anchored {
			if rel == "" || isExistingDir(filepath.Join(anc.hostSrcDir, rel)) {
				return nil, nil, "", false
			}
			if _, err := os.Stat(filepath.Join(anc.hostSrcDir, filepath.FromSlash(rel))); err != nil {
				return nil, nil, "", false
			}
			addSrc(rel)
			rewritten = append(rewritten, emitKeyed(a, "$(location "+rel+")"))
			continue
		}
		rewritten = append(rewritten, shellQuoteArg(a))
	}
	cmd = strings.Join(rewritten, " ")
	cmd, tools = rewriteToolFromTarget(cmd, cc.ArtifactToName, cc.ExecArtifacts, cc.Imports, cc.HostPrefixDir)
	return srcs, tools, cmd, true
}
