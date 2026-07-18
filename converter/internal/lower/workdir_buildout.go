package lower

import (
	"context"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/sstriker/buildstream-bazel/converter/internal/ninja"
	"github.com/sstriker/buildstream-bazel/converter/ir"
)

// tryWorkdirBuildOutGenrule handles the complement of
// tryInSourceWorkdirGenrule: a custom command with an in-SOURCE
// WORKING_DIRECTORY whose outputs land in the BUILD dir. VTK's
// proj.db is the marker shape: `cmake -P generate_proj_db.cmake`
// runs cd'd into the source data dir (so the script's bare
// `include(sql_filelist.cmake)` resolves there), takes its
// build-tree paths as -D args (an intermediate it file(WRITE)s, the
// output db), and a sibling `cmake -E copy` step duplicates the
// output elsewhere in the build tree. The plain emit loses the cd
// (the include fails at exec-root cwd) and leaves the build-relative
// paths pointing nowhere a sandboxed genrule may write.
//
// The lift reconstructs BOTH trees in scratch dirs: srcs materialize
// at their element-root-relative positions under $$tmp (the source
// tree the cd + relative reads need), build-relative path tokens
// re-point under $$bld (the writable build tree), exec-root-anchored
// references ($(location …), <labelRoot>/… source paths) absolutize
// through $$root so the cwd change can't break them, and each
// declared out copies from $$bld to its $(RULEDIR) home afterwards.
//
// When --cmake-script-trace is on and the command is a `cmake -P`
// script, the script runs under the REAL working dir at convert time
// (with path-shaped / declared-output -D values redirected into a
// throwaway scratch to keep the probe's writes out of the source
// tree — see appendScriptTraceReads for the exact rule) and the
// trace's source-class reads join srcs — the channel that catches
// `include(sql_filelist.cmake)`, which no ninja DEPENDS declares.
func tryWorkdirBuildOutGenrule(b *ninja.Build, cmd string, srcs, outs []string, cmakeSrc, buildDir, umbrellaPrefix, bazelPackagePath string, artifactToName map[string]string, cc *codegenContext) (ir.Target, bool) {
	if cc == nil {
		return ir.Target{}, false
	}
	wdAbs := extractCdDir(cmd)
	if wdAbs == "" {
		return ir.Target{}, false
	}
	wd, inside := relativeIfInside(cmakeSrc, wdAbs)
	if !inside {
		return ir.Target{}, false
	}
	// In-source outputs belong to tryInSourceWorkdirGenrule (it runs
	// first); this branch wants the build-dir-output complement.
	if _, inSrc := inSourceOutputs(outs, cmakeSrc); inSrc {
		return ir.Target{}, false
	}
	// inSourceOutputs is all-or-nothing, so a MIXED edge (build-relative
	// outs plus an absolute in-source or out-of-tree out) reaches here.
	// The dual-scratch emit only knows how to land build-dir-RELATIVE
	// outs (they become Bazel outs entries and `cp "$$bld/<o>"` sources;
	// an absolute path is invalid as either) — decline so the edge falls
	// through to the bake / raw-emit / refusal paths.
	for _, o := range outs {
		if filepath.IsAbs(o) {
			return ir.Target{}, false
		}
	}
	// The $$root re-anchoring keys on the exec-root source prefix that
	// rewriteGenruleCmd spells onto source-tree paths — the join of the
	// element's landing package and the umbrella segment (its srcBase).
	// When BOTH are empty (convert-at-root / fidelity-harness mode),
	// source paths come out bare-relative, indistinguishable from the
	// stripped build-dir paths this lift re-points under $$bld — the
	// emitted genrule would point the script and its source reads at an
	// empty scratch dir. Decline instead.
	srcBase := umbrellaPrefix
	if bazelPackagePath != "" {
		srcBase = filepath.ToSlash(filepath.Join(bazelPackagePath, umbrellaPrefix))
	}
	if strings.Trim(srcBase, "/") == "" {
		return ir.Target{}, false
	}

	// Convert-time script trace: discover the reads the cd'd script
	// makes beyond ninja's DEPENDS (best-effort — the script may die
	// at a built-tool exec; reads before that still classify).
	if cc.CMakeScriptTrace && cc.CMakeBinary != "" && usesCmakeScriptMode(cmd) {
		srcs = appendScriptTraceReads(cmd, srcs, outs, cmakeSrc, buildDir, wdAbs, cc)
	}

	body := rewriteGenruleCmd(cmd, cmakeSrc, buildDir, umbrellaPrefix, bazelPackagePath)
	body, tools, toolchains := rewriteToolFromTarget(body, artifactToName, cc.ExecArtifacts, cc.Imports, cc.HostPrefixDir, cc.toolchainTools())
	srcs = dropLiftedToolSrcs(srcs, tools, artifactToName)

	name := genruleNameFor(b, buildDir)
	gen := ir.Target{
		Name:              name,
		Kind:              ir.KindGenrule,
		Srcs:              srcs,
		GenruleOuts:       outs,
		GenruleCmd:        buildWorkdirBuildOutGenrule(body, filepath.ToSlash(wd), srcs, outs, srcBase),
		GenruleTools:      tools,
		GenruleToolchains: toolchains,
		Tags:              []string{"cmake-codegen-standalone-custom-command", "cmake-codegen-workdir-buildout"},
		Visibility:        []string{"//visibility:private"},
	}
	for _, o := range outs {
		cc.OutToGenrule[o] = name
	}
	return gen, true
}

// appendScriptTraceReads runs the command's cmake -P script under its
// real working dir with --trace and appends the classified
// source-class reads to srcs. Relative -D values that are path-shaped
// (contain a slash) or name a declared output are redirected into a
// throwaway scratch dir first so the probe's writes land there rather
// than in the source tree (the cd'd cwd). Slash-less values that
// aren't outputs stay verbatim — they're value-args (-DVERSION=9.4.2),
// and mangling them into scratch paths would change what the script
// does; a script that writes such a value as a cwd-relative file is a
// residual probe-side write this channel accepts. Failures (script
// aborts at a not-yet-built tool) degrade silently to the partial
// trace — this channel only ADDS inputs the static recovery missed.
func appendScriptTraceReads(cmd string, srcs, outs []string, cmakeSrc, buildDir, wdAbs string, cc *codegenContext) []string {
	scriptArg := extractCmakeScriptPath(cmd)
	if scriptArg == "" {
		return srcs
	}
	scriptAbs := scriptArg
	if !filepath.IsAbs(scriptAbs) {
		scriptAbs = filepath.Join(wdAbs, scriptArg)
	}
	scratch, err := os.MkdirTemp("", "bsb-workdir-trace-*")
	if err != nil {
		return srcs
	}
	defer os.RemoveAll(scratch)
	outSet := map[string]bool{}
	for _, o := range outs {
		outSet[path.Clean(filepath.ToSlash(o))] = true
	}
	var dArgs []string
	for _, a := range extractCmakePDashArgs(cmd) {
		if name, val, ok := strings.Cut(a, "="); ok && val != "" &&
			!filepath.IsAbs(val) &&
			(strings.Contains(val, "/") || outSet[path.Clean(filepath.ToSlash(val))]) {
			a = name + "=" + filepath.Join(scratch, filepath.FromSlash(path.Clean(val)))
		}
		dArgs = append(dArgs, a)
	}
	traceRaw, err := TraceCmakeScript(context.Background(), cc.CMakeBinary, scriptAbs, dArgs, wdAbs)
	if err != nil || len(traceRaw) == 0 {
		return srcs
	}
	cls := ClassifyScriptTrace(traceRaw, cmakeSrc, buildDir)
	for _, p := range cls.SourcePaths {
		srcs = appendUnique(srcs, p)
	}
	sort.Strings(srcs)
	return srcs
}

// buildWorkdirBuildOutGenrule assembles the dual-scratch cmd. Path
// token classes inside body (space-delimited, with $(…) groups
// protected; quoted-space args carry the same caveat as the rest of
// this family):
//
//   - $(location/execpath …) and <srcBase>/… exec-root references
//     absolutize via $$root (the pre-cd exec root); srcBase is the
//     same package+umbrella join rewriteGenruleCmd anchored the
//     source-tree paths with (the caller guarantees it's non-empty);
//   - relative path tokens — build-tree references whose absolute
//     prefix rewriteGenruleCmd stripped — re-point under $$bld,
//     path.Clean'd (cmake spells outputs with `lib/../` noise);
//     declared outs copy from there to $(RULEDIR) afterwards. A
//     slash-containing token qualifies on shape alone; a slash-less
//     token (a build-ROOT-level output like `proj.db`) only when it
//     names a declared out — bare words are otherwise flags, tool
//     names, or wd-relative reads the cd already resolves.
//
// Extension-less $$bld paths pre-create as directories (the `cmake
// -E copy <file> <dir>` recovered-cp shape needs the dest dir);
// every other $$bld path pre-creates its parent.
func buildWorkdirBuildOutGenrule(body, wd string, srcs, outs []string, srcBase string) string {
	outSet := map[string]bool{}
	for _, o := range outs {
		outSet[path.Clean(o)] = true
	}
	labelRootPrefix := ""
	if p := strings.Trim(srcBase, "/"); p != "" {
		labelRootPrefix = p + "/"
	}

	var bldPaths []string
	seenBld := map[string]bool{}
	protected := protectMakeRefs(body)
	tokens := strings.Split(protected, " ")
	for i, tok := range tokens {
		flagName := ""
		val := tok
		if strings.HasPrefix(tok, "-D") {
			if n, v, ok := strings.Cut(tok, "="); ok {
				flagName, val = n+"=", v
			}
		}
		switch {
		case strings.HasPrefix(val, "$\x00("): // protected $(…) group
			tokens[i] = flagName + "$$root/" + val
		case labelRootPrefix != "" && strings.HasPrefix(val, labelRootPrefix):
			tokens[i] = flagName + "$$root/" + val
		case !strings.HasPrefix(val, "-") && !filepath.IsAbs(val) && !strings.Contains(val, "\x00") &&
			(strings.Contains(val, "/") || outSet[path.Clean(val)]):
			clean := path.Clean(val)
			tokens[i] = flagName + "$$bld/" + clean
			if !seenBld[clean] {
				seenBld[clean] = true
				bldPaths = append(bldPaths, clean)
			}
		}
	}
	body = restoreMakeRefs(strings.Join(tokens, " "))

	var b strings.Builder
	b.WriteString(`root="$$PWD" && tmp="$$(mktemp -d)" && bld="$$(mktemp -d)"`)
	for _, s := range srcs {
		if d := path.Dir(s); d != "." {
			b.WriteString(` && mkdir -p "$$tmp/` + d + `"`)
		}
		b.WriteString(` && cp "$(execpath ` + s + `)" "$$tmp/` + s + `"`)
	}
	for _, p := range bldPaths {
		if path.Ext(p) == "" && !outSet[p] {
			b.WriteString(` && mkdir -p "$$bld/` + p + `"`)
		} else if d := path.Dir(p); d != "." {
			b.WriteString(` && mkdir -p "$$bld/` + d + `"`)
		}
	}
	b.WriteString(` && ( cd "$$tmp/` + wd + `" && ` + body + ` )`)
	for _, o := range outs {
		if d := path.Dir(o); d != "." {
			b.WriteString(` && mkdir -p "$(RULEDIR)/` + d + `"`)
		}
		b.WriteString(` && cp "$$bld/` + o + `" "$(RULEDIR)/` + o + `"`)
	}
	return b.String()
}

// protectMakeRefs masks the space inside $(location …)/$(execpath …)
// groups so space-tokenization can't split them; restoreMakeRefs
// inverts. The mask also marks the token as a make-ref for the
// classifier ('\x00(' prefix after the '$').
func protectMakeRefs(s string) string {
	s = strings.ReplaceAll(s, "$(location ", "$\x00(location\x01")
	return strings.ReplaceAll(s, "$(execpath ", "$\x00(execpath\x01")
}

func restoreMakeRefs(s string) string {
	s = strings.ReplaceAll(s, "\x00(location\x01", "(location ")
	return strings.ReplaceAll(s, "\x00(execpath\x01", "(execpath ")
}
