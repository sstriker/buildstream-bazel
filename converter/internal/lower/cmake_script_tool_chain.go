package lower

import (
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/sstriker/buildstream-bazel/converter/internal/ninja"
	"github.com/sstriker/buildstream-bazel/converter/ir"
	"github.com/sstriker/buildstream-bazel/internal/shadow"
)

// recoverToolChain recovers a multi-stage codegen pipeline hidden inside a
// re-traced `cmake -P` wrapper: a sequence of tool calls where each writes an
// intermediate the next consumes, ending in the custom command's DECLARED
// output. When an intermediate lives OUTSIDE the build dir (a system tempdir —
// `execute_process(mktemp -d …)` or a hardcoded `/tmp/…`), the per-step recovery
// (recoverExecuteProcess) can't anchor it as a Bazel artifact: the producing
// step's genrule can't declare it and the consuming step's genrule leaks the
// absolute convert-time `/tmp/…` path (which doesn't exist at Bazel build time).
// The chain is the unit of recovery: fold every stage into ONE genrule whose cmd
// runs the tools in order, with each transient intermediate a CWD-relative file
// (never a declared Bazel output) and only the real source inputs + final
// declared outputs anchored.
//
// Runs BEFORE recoverExecuteProcess (like recoverTempDirToolRelocate) so its
// claim wins. Gated tightly so it ONLY owns the non-anchorable-intermediate
// shape — it declines (→ recoverExecuteProcess's per-step chaining, which is
// correct for a build-dir intermediate) unless: every call is a single liftable
// tool command; every declared output is written by some stage; at least one
// intermediate lives outside BOTH the source root and the build dir; and the
// intermediates' basenames don't collide.
func (cc *codegenContext) recoverToolChain(b *ninja.Build, calls []shadow.ExecuteProcessCall, cmakeSrc, buildDir, relOut string, g *ninja.Graph) (string, bool) {
	if len(calls) < 2 {
		return "", false // need a real chain (≥2 stages)
	}
	declared, declaredSet, ok := chainDeclaredOutputs(b, buildDir, relOut)
	if !ok {
		return "", false
	}
	anc := execAnchors{hostSrcDir: cmakeSrc, recordedSrcDir: cmakeSrc, hostBuildDir: buildDir, recordedBuildDir: buildDir, outerBuildDirs: cc.OuterBuildDirs}
	stages, ok := cc.chainStages(calls, anc)
	if !ok {
		return "", false
	}
	intermediates, ok := chainIntermediates(stages, anc, declaredSet, declared)
	if !ok {
		return "", false
	}
	cwdName, ok := chainCwdNames(intermediates)
	if !ok {
		return "", false
	}
	// Drop pure side-effect stages (e.g. `mktemp -d`, whose output is captured to
	// a variable, not a file the chain reads) so the folded cmd carries no dead
	// commands. A real chain still needs ≥2 contributing stages.
	stages = liveChainStages(stages, anc, declaredSet, intermediates)
	if len(stages) < 2 {
		return "", false
	}
	cmd, srcs := foldChainCmd(stages, anc, declaredSet, cwdName)

	gen := ir.Target{
		Name:        genruleNameFor(b, buildDir),
		Kind:        ir.KindGenrule,
		GenruleCmd:  cmd,
		GenruleOuts: declared,
		Srcs:        srcs,
		Tags:        []string{"cmake-codegen", "cmake-codegen-tool-chain"},
		Visibility:  []string{"//visibility:private"},
	}
	cc.Genrules = append(cc.Genrules, gen)
	cc.SeenBuilds[b] = gen.Name
	for _, o := range declared {
		cc.OutToGenrule[o] = gen.Name
	}
	return gen.Name, true
}

// chainDeclaredOutputs returns the edge's declared outputs (+ a set) when they
// hold the chain invariants: relOut among them and all on disk (the trace
// produced them). ok=false otherwise.
func chainDeclaredOutputs(b *ninja.Build, buildDir, relOut string) ([]string, map[string]bool, bool) {
	declared := genruleOuts(b, buildDir)
	if len(declared) == 0 {
		return nil, nil, false
	}
	set := map[string]bool{}
	inDeclared := false
	for _, o := range declared {
		set[o] = true
		if o == relOut {
			inDeclared = true
		}
		if st, err := os.Stat(filepath.Join(buildDir, filepath.FromSlash(o))); err != nil || st.IsDir() {
			return nil, nil, false
		}
	}
	if !inDeclared {
		return nil, nil, false
	}
	return declared, set, true
}

// chainStages normalizes the calls into argvs, requiring every one to be a
// single liftable tool command (a non-tool stage — a probe, a configure —
// breaks the fold). ok=false on the first non-conforming call.
func (cc *codegenContext) chainStages(calls []shadow.ExecuteProcessCall, anc execAnchors) ([][]string, bool) {
	stages := make([][]string, 0, len(calls))
	for _, raw := range calls {
		c := normalizeCMakeECall(clearDeadCaptures(raw, cc.DeadCaptureVars))
		if !argvStructurallyLiftableInWrapper(c) || !argvToolLiftable(c.Commands[0][0], anc, cc) {
			return nil, false
		}
		stages = append(stages, c.Commands[0])
	}
	return stages, true
}

// chainIntermediates walks the stages in trace (execution) order and returns the
// set of transient intermediate paths (absolute, on disk, outside source+build).
// ok=false unless at least one such intermediate exists (else the chain is
// all-anchorable — recoverExecuteProcess owns it) AND every declared output is
// written by some stage.
func chainIntermediates(stages [][]string, anc execAnchors, declaredSet map[string]bool, declared []string) (map[string]bool, bool) {
	intermediates := map[string]bool{}
	declaredWritten := map[string]bool{}
	for _, argv := range stages {
		for i, a := range argv {
			if i == 0 {
				continue // the tool driver
			}
			raw := stripArgvPathPrefix(a)
			if _, ok := executeProcessAnchorSource(raw, anc); ok {
				continue // a source-tree input
			}
			if rel, ok := executeProcessAnchorOutput(raw, anc); ok {
				if declaredSet[rel] {
					declaredWritten[rel] = true
					continue
				}
				// A build-dir NON-declared path (an anchorable intermediate, or a
				// build-dir input) is one the per-step recovery CAN anchor. The
				// fold gives it no CWD name, so emitting it would leak its absolute
				// convert-time path. Decline the whole fold and let the per-step
				// chaining own the chain — so the fold only fires when EVERY
				// intermediate is non-anchorable (outside source + build).
				return nil, false
			}
			if !filepath.IsAbs(raw) {
				continue // a relative literal / flag
			}
			// An absolute operand outside source + build. A regular file on disk
			// is a transient intermediate (the trace produced it). Anything else —
			// a directory, or a path that no longer exists — the fold can't
			// classify, and emitting it verbatim would leak an absolute path, so
			// decline (per-step recovery takes the chain).
			if st, err := os.Stat(raw); err != nil || st.IsDir() {
				return nil, false
			}
			intermediates[raw] = true
		}
	}
	if len(intermediates) == 0 {
		return nil, false
	}
	for _, o := range declared {
		if !declaredWritten[o] {
			return nil, false
		}
	}
	return intermediates, true
}

// liveChainStages keeps the stages that contribute to the chain — those whose
// argv references a declared output or a transient intermediate. A pure
// side-effect stage (e.g. `mktemp -d`, whose output is captured into a variable
// rather than a file operand) drops out.
func liveChainStages(stages [][]string, anc execAnchors, declaredSet, intermediates map[string]bool) [][]string {
	var live [][]string
	for _, argv := range stages {
		for i, a := range argv {
			if i == 0 {
				continue
			}
			raw := stripArgvPathPrefix(a)
			if intermediates[raw] {
				live = append(live, argv)
				break
			}
			if rel, ok := executeProcessAnchorOutput(raw, anc); ok && declaredSet[rel] {
				live = append(live, argv)
				break
			}
		}
	}
	return live
}

// chainCwdNames assigns each intermediate a unique CWD-relative transient name
// (its basename). ok=false on a basename collision (don't guess) or an empty
// basename.
func chainCwdNames(intermediates map[string]bool) (map[string]string, bool) {
	cwdName := map[string]string{}
	used := map[string]bool{}
	for raw := range intermediates {
		base := path.Base(filepath.ToSlash(raw))
		if base == "" || base == "." || used[base] {
			return nil, false
		}
		used[base] = true
		cwdName[raw] = base
	}
	return cwdName, true
}

// foldChainCmd renders the folded genrule cmd + srcs: each stage's argv is
// rewritten — source inputs → $(location <rel>) (+ srcs), declared outputs →
// $(RULEDIR)/<rel>, intermediates → their CWD name (a transient), the tool
// driver kept (abs → basename) — and the stages are joined with ` && `, with a
// leading mkdir for any output/intermediate subdirectory.
func foldChainCmd(stages [][]string, anc execAnchors, declaredSet map[string]bool, cwdName map[string]string) (string, []string) {
	var srcs, mkdirs []string
	srcSeen, mkdirSeen := map[string]bool{}, map[string]bool{}
	addMkdir := func(d string) {
		if d != "." && d != "" && !mkdirSeen[d] {
			mkdirSeen[d] = true
			mkdirs = append(mkdirs, d)
		}
	}
	var parts []string
	for _, argv := range stages {
		toks := make([]string, len(argv))
		for i, a := range argv {
			if i == 0 {
				toks[i] = chainToolToken(a)
				continue
			}
			prefix, raw := splitArgvPathPrefix(a)
			if rel, ok := executeProcessAnchorSource(raw, anc); ok {
				if !srcSeen[rel] {
					srcSeen[rel] = true
					srcs = append(srcs, rel)
				}
				toks[i] = prefix + "$(location " + rel + ")"
				continue
			}
			if rel, ok := executeProcessAnchorOutput(raw, anc); ok && declaredSet[rel] {
				addMkdir("$(RULEDIR)/" + path.Dir(rel))
				toks[i] = prefix + "$(RULEDIR)/" + rel
				continue
			}
			if name, ok := cwdName[raw]; ok {
				addMkdir(path.Dir(filepath.ToSlash(name)))
				toks[i] = prefix + name
				continue
			}
			toks[i] = a // a literal / flag
		}
		parts = append(parts, strings.Join(toks, " "))
	}
	sort.Strings(srcs)
	var cmd strings.Builder
	for _, d := range mkdirs {
		cmd.WriteString("mkdir -p " + d + " && ")
	}
	cmd.WriteString(strings.Join(parts, " && "))
	return cmd.String(), srcs
}

// chainToolToken renders a tool driver for the folded chain cmd: an absolute
// tool path drops to its basename (the hoist's portability policy — a host tool
// like python3/perl is on PATH), a bare driver stays as-is.
func chainToolToken(tool string) string {
	if filepath.IsAbs(tool) {
		return path.Base(filepath.ToSlash(tool))
	}
	return tool
}

// splitArgvPathPrefix splits a `key=path` operand into ("key=", "path"); a plain
// operand returns ("", operand). Mirrors stripArgvPathPrefix but keeps the
// prefix so a rewritten `-o=path` / `out=path` operand stays intact.
func splitArgvPathPrefix(a string) (prefix, pathPart string) {
	if eq := strings.IndexByte(a, '='); eq > 0 && !strings.ContainsAny(a[:eq], "/\\") {
		return a[:eq+1], a[eq+1:]
	}
	return "", a
}
