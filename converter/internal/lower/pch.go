package lower

import (
	"path/filepath"
	"strings"

	"github.com/sstriker/buildstream-bazel/converter/internal/fileapi"
	"github.com/sstriker/buildstream-bazel/converter/ir"
)

// This file is the target_precompile_headers (PCH) forced-include lift.
//
// cmake's PCH machinery is TWO effects welded together: (1) a forced
// include — every TU in the target is compiled as if `#include
// "cmake_pch.h[xx]"` were its first line, where that generated header
// #includes the declared PCH headers in order — and (2) a compile-speed
// optimization (the .gch/.pch precompilation of that header). Effect (1)
// is a CORRECTNESS input: sources legitimately rely on the PCH for
// declarations and macros they never #include themselves (stdafx-style
// codebases). Effect (2) is not — the same TUs compile identically,
// just slower, without the precompiled artifact.
//
// Bazel cc_library has no PCH attribute, so the lift preserves effect
// (1) by emitting a MIRROR of cmake's generated cmake_pch.h[xx] — a
// synthesized header carrying `#pragma GCC system_header` plus the
// declared #includes in order, materialized by a write_file rule — and
// force-including that ONE file at the cmake_pch pair's ORIGINAL
// compile-line position. Mirroring the header (rather than expanding
// the list into N `-include` pairs, the previous shape) reproduces
// cmake's semantics by construction:
//
//   - warnings INSIDE the declared PCH headers stay suppressed (the
//     system_header pragma propagates to the files it includes), so
//     -Werror projects behave identically;
//   - the single -include occupies cmake's exact argv position, so a
//     target that also adds its own non-PCH forced include keeps
//     cmake's forced-include processing order;
//   - per-config-VARYING header lists materialize as per-config
//     mirrors selected per `//config:*` arm (multiconfig.go's
//     perConfigPCHArms).
//
// The speed half stays operator-side (a cc_toolchain feature / custom
// rule; see docs/operator-toolchain-features.md). The
// `cmake-codegen-pch` tag + bazelidiom audit finding keep that residual
// auditable.

// pchResolver looks up the ordered target_precompile_headers entries of
// the named cmake target for one language. ToIR builds it over the
// primary configuration's targets; the lift uses it to resolve a
// REUSE_FROM consumer's `-include <owner>.dir/.../cmake_pch.*` compile
// fragment back to the OWNING target's header list — the consumer's own
// codemodel CompileGroup.PrecompileHeaders is null under REUSE_FROM, so
// the fragment is the only signal it carries.
type pchResolver func(targetName, language string) []fileapi.CompilePCH

// pchLiftCtx bundles the inputs the forced-include mirror needs.
// pkgPath is the element's repo-root-relative landing package
// (--bazel-package-path): compile actions run from Bazel's EXEC ROOT,
// and copts are passed verbatim (unlike the `includes` attribute, which
// Bazel package-prefixes), so a `-include` argument naming an in-element
// file must carry the exec-root form `<pkgPath>/<rel>` — the same
// re-anchoring rewriteGenruleCmd applies to genrule cmds. Empty pkgPath
// (standalone convert, element at the workspace root) keeps bare
// element-relative args. cc registers the mirror write_file rules
// (OutToGenrule dedups them, so a REUSE_FROM consumer shares the
// owner's mirror instead of duplicating it).
type pchLiftCtx struct {
	resolve    pchResolver
	cmakeSrc   string
	cmakeBuild string
	reanchor   func(string) string
	pkgPath    string
	cc         *codegenContext
}

// pchMirrorOut returns the element-relative path of the mirror header
// for one (owner target, language, config) triple. The extension
// follows cmake's own naming (cmake_pch.hxx for CXX, cmake_pch.h
// otherwise); the dedicated cmake_pch/ tree keeps the synthesized file
// from colliding with project sources. config is empty for the
// baseline mirror and the cell name for a per-config one (the
// config-varying-list shape).
func pchMirrorOut(owner, language, config string) string {
	ext := "h"
	if strings.EqualFold(language, "CXX") {
		ext = "hxx"
	}
	dir := "cmake_pch/" + sanitizeOutputName(owner)
	if config != "" {
		dir += "/" + sanitizeOutputName(config)
	}
	return dir + "/cmake_pch." + ext
}

// ensureMirror materializes (once) the mirror header for an owner's
// declared PCH list and returns its element-relative out path plus the
// source-tree headers the mirror references (reanchored element-relative;
// the caller stages them as action inputs of the CONSUMING rule — the
// declaring target already carries them via its codemodel Sources, a
// REUSE_FROM consumer does not). Repeated calls (the REUSE_FROM
// consumer after the owner, per-language subs) reuse the registered
// rule and still return the staging list.
func (c pchLiftCtx) ensureMirror(owner, language, config string, entries []fileapi.CompilePCH) (outRel string, stageHdrs []string) {
	outRel = pchMirrorOut(owner, language, config)
	lines := []string{
		"/* Mirror of cmake's generated cmake_pch (target_precompile_headers,",
		"   target " + owner + "): force-included first into every TU, exactly",
		"   as cmake compiles. The system_header pragma propagates to the",
		"   included headers, matching cmake's warning suppression. */",
		"#pragma GCC system_header",
	}
	for _, e := range entries {
		line, stage := c.includeLine(e.Header)
		if line == "" {
			continue
		}
		lines = append(lines, line)
		if stage != "" {
			stageHdrs = append(stageHdrs, stage)
		}
	}
	if c.cc == nil {
		return outRel, stageHdrs
	}
	if _, exists := c.cc.OutToGenrule[outRel]; exists {
		return outRel, stageHdrs
	}
	body := []byte(strings.Join(lines, "\n") + "\n")
	name := "pch_" + sanitizePathToNameStem(outRel)
	tags := []string{"cmake-codegen", "cmake-codegen-pch"}
	c.cc.Genrules = append(c.cc.Genrules, bakeFileTarget(name, outRel, body, tags))
	c.cc.OutToGenrule[outRel] = name
	return outRel, stageHdrs
}

// apply is the rule-level application of the lift, shared by lowerTarget's
// main compile-group path and splitCompileGroups' per-language subs.
// splitCompileFragments leaves each cmake_pch `-include` pair IN PLACE
// (positional fidelity); apply rewrites the artifact argument to the
// mirror's exec-root path, materializes the mirror rule, stages the
// mirror (and, for REUSE_FROM consumers, the source-tree headers it
// references) into srcs, and tags the user-visible target. The tag
// matters especially for the REUSE_FROM shape, whose codemodel
// PrecompileHeaders is null — without the artifact-driven handling it
// would lose its PCH silently.
//
// Staging slot: inputs not already present land in SRCS, not hdrs — they
// are compile inputs of this rule's own TUs only (the forced include), and
// hdrs would export them to dependents, the include-over-grant shape the
// emit-side `cmake-include-over-grant` warning exists to flag.
func (c pchLiftCtx) apply(artifacts []string, cg fileapi.CompileGroup,
	copts, srcs, hdrs []string, irt *ir.Target) (newCopts, newSrcs []string) {
	if len(artifacts) == 0 {
		return copts, srcs
	}
	stage := []string{}
	seenArg := map[string]bool{}
	out := copts[:0]
	for i := 0; i < len(copts); i++ {
		tok := copts[i]
		if (tok != "-include" && tok != "-include-pch") || i+1 >= len(copts) || !isCMakePCHPath(copts[i+1]) {
			out = append(out, tok)
			continue
		}
		art := copts[i+1]
		i++
		entries := cg.PrecompileHeaders
		owner := pchArtifactOwner(art, c.cmakeBuild)
		if owner == "" {
			owner = irt.Name
		}
		if len(entries) == 0 && c.resolve != nil {
			entries = c.resolve(owner, cg.Language)
		}
		if len(entries) == 0 {
			// No declared list recoverable (orphan artifact): drop the
			// raw build-dir pair — emitting a convert-time path would
			// never resolve — and leave the tag below as the audit trail.
			continue
		}
		mirrorRel, hdrsToStage := c.ensureMirror(owner, cg.Language, "", entries)
		arg := c.execRootPath(mirrorRel)
		if seenArg[arg] {
			continue
		}
		seenArg[arg] = true
		// -include-pch expects a precompiled artifact; the mirror is a
		// plain header, so the flag normalizes to -include.
		out = append(out, "-include", arg)
		stage = append(stage, mirrorRel)
		stage = append(stage, hdrsToStage...)
	}
	copts = out
	for _, h := range stage {
		if !stringSliceContains(srcs, h) && !stringSliceContains(hdrs, h) {
			srcs = append(srcs, h)
		}
	}
	if !stringSliceContains(irt.Tags, "cmake-codegen-pch") {
		irt.Tags = append(irt.Tags, "cmake-codegen-pch")
	}
	return copts, srcs
}

// pchArtifactOwner extracts the owning target name from a cmake_pch artifact
// path — `<build>/[<subdir>/]CMakeFiles/<target>.dir/[<config>/]cmake_pch.*`.
// Returns "" when the path doesn't carry the per-target CMakeFiles layout.
func pchArtifactOwner(artifact, cmakeBuild string) string {
	rel, ok := relativeIfInsideRelaxed(cmakeBuild, artifact)
	if !ok {
		rel = artifact
	}
	owner, _, ok := findCMakeFilesDir(rel)
	if !ok {
		return ""
	}
	return owner
}

// includeLine maps one codemodel precompileHeaders entry onto the
// `#include` line the mirror header carries, plus (when the entry is a
// source-tree file) the element-relative path the caller should stage
// as an action input.
//
// The entry shapes mirror what cmake writes into cmake_pch.h[xx]:
//
//   - `<vector>`   (angle form)    → `#include <vector>`.
//   - `"other.h"`  (verbatim form) → `#include "other.h"`, resolved via the
//     target's include paths the lift already replicates.
//   - absolute source-tree path    → `#include "<pkgPath>/<element-rel>"`
//     (compile actions run from the exec root; see pchLiftCtx.pkgPath),
//     with the header itself staged via stage.
//   - absolute build-dir path      → exec-root include of the generated
//     header (resolves once its genrule stages it — the
//     configure_file/codegen lifts own that half).
//   - other absolute path          → kept verbatim (out-of-tree system
//     header; cmake_pch baked the same absolute path).
//   - bare relative path           → kept verbatim (a generator-expression
//     result cmake didn't resolve; it rides the include search chain).
func (c pchLiftCtx) includeLine(h string) (line, stage string) {
	switch {
	case h == "":
		return "", ""
	case strings.HasPrefix(h, "<") && strings.HasSuffix(h, ">"):
		return "#include " + h, ""
	case strings.HasPrefix(h, `"`):
		return "#include \"" + stripBalancedQuotes(h) + "\"", ""
	case filepath.IsAbs(h):
		if rel, ok := relativeIfInside(c.cmakeSrc, h); ok {
			if c.reanchor != nil {
				rel = c.reanchor(rel)
			}
			return "#include \"" + c.execRootPath(rel) + "\"", rel
		}
		if rel, ok := relativeIfInsideRelaxed(c.cmakeBuild, h); ok {
			return "#include \"" + c.execRootPath(rel) + "\"", ""
		}
		return "#include \"" + h + "\"", ""
	default:
		return "#include \"" + h + "\"", ""
	}
}

// execRootPath turns an element-relative path into the form a verbatim copt
// must carry to resolve from Bazel's exec root: prefixed with the landing
// package when one is declared, bare otherwise.
func (c pchLiftCtx) execRootPath(rel string) string {
	if c.pkgPath == "" || c.pkgPath == "." {
		return rel
	}
	return c.pkgPath + "/" + rel
}

// filterPCHCoptArm strips cmake PCH machinery tokens from one multi-config
// copts select() arm. The per-config fold is token-set-shaped (configfold
// tokenizes fragments and partitions per token), so a config-varying
// `-include <build>/CMakeFiles/<t>.dir/<Config>/cmake_pch.hxx` pair surfaces
// as two unpaired facts; left alone they'd render as a raw convert-time
// build-dir path in the arm. Drop the artifact path always; drop the bare
// `-include` / `-include-pch` flag only when the arm held a PCH artifact and
// no other non-flag token remains that could be the flag's argument (a
// genuine per-config forced include of a project header keeps its pair).
// The forced-include semantics themselves ride the baseline copts via the
// mirror lift; a per-config-VARYING declared list additionally re-expands
// per arm via perConfigPCHArms (multiconfig.go).
func filterPCHCoptArm(values []string) []string {
	hadPCH := false
	out := values[:0]
	for _, v := range values {
		if isCMakePCHPath(v) {
			hadPCH = true
			continue
		}
		out = append(out, v)
	}
	if !hadPCH {
		return out
	}
	nonFlag := false
	for _, v := range out {
		if !strings.HasPrefix(v, "-") {
			nonFlag = true
			break
		}
	}
	if nonFlag {
		return out
	}
	filtered := out[:0]
	for _, v := range out {
		if v == "-include" || v == "-include-pch" {
			continue
		}
		filtered = append(filtered, v)
	}
	return filtered
}

// multiConfigPCHCtx reconstructs the lift context for the multi-config
// per-config-divergence pass (perConfigPCHArms) from assemble-level
// state: same fields lowerTarget's per-target pchLiftCtx carries — the
// umbrella reanchor derives from hostSrc/cmakeSrc exactly as
// targetLowerCtx.umbrellaReanchor does.
func multiConfigPCHCtx(ti targetIndexes, opts Options, cc *codegenContext, cmakeSrc, cmakeBuild, hostSrc string) *pchLiftCtx {
	reanchor := func(rel string) string { return rel }
	if hostSrc != "" && hostSrc != cmakeSrc {
		if prefix, inside := relativeIfInside(hostSrc, cmakeSrc); inside && prefix != "" && prefix != "." {
			reanchor = func(rel string) string {
				if rel == "" {
					return rel
				}
				return filepath.Join(prefix, rel)
			}
		}
	}
	return &pchLiftCtx{
		resolve:    ti.pchResolve,
		cmakeSrc:   cmakeSrc,
		cmakeBuild: cmakeBuild,
		reanchor:   reanchor,
		pkgPath:    opts.BazelPackagePath,
		cc:         cc,
	}
}
