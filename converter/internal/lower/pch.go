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
// (1) by expanding the declared header list into ordered `-include`
// copts pairs, and leaves effect (2) to the operator (a cc_toolchain
// feature / custom rule; see docs/operator-toolchain-features.md). The
// `cmake-codegen-pch` tag + bazelidiom audit finding keep that residual
// auditable.
//
// Known fidelity residue, accepted for v1: cmake's generated cmake_pch
// header carries `#pragma GCC system_header`, so warnings INSIDE the
// declared PCH headers are suppressed under cmake but fire under the
// direct `-include`. If a corpus member trips on that (e.g. -Werror),
// the fallback is materializing a literal mirror of cmake_pch.h[xx]
// and force-including that one file instead.

// pchResolver looks up the ordered target_precompile_headers entries of
// the named cmake target for one language. ToIR builds it over the
// primary configuration's targets; the lift uses it to resolve a
// REUSE_FROM consumer's `-include <owner>.dir/.../cmake_pch.*` compile
// fragment back to the OWNING target's header list — the consumer's own
// codemodel CompileGroup.PrecompileHeaders is null under REUSE_FROM, so
// the fragment is the only signal it carries.
type pchResolver func(targetName, language string) []fileapi.CompilePCH

// pchLiftCtx bundles the inputs the forced-include expansion needs.
// pkgPath is the element's repo-root-relative landing package
// (--bazel-package-path): compile actions run from Bazel's EXEC ROOT,
// and copts are passed verbatim (unlike the `includes` attribute, which
// Bazel package-prefixes), so a `-include` argument naming an in-element
// file must carry the exec-root form `<pkgPath>/<rel>` — the same
// re-anchoring rewriteGenruleCmd applies to genrule cmds. Empty pkgPath
// (standalone convert, element at the workspace root) keeps bare
// element-relative args.
type pchLiftCtx struct {
	resolve    pchResolver
	cmakeSrc   string
	cmakeBuild string
	reanchor   func(string) string
	pkgPath    string
}

// forcedIncludeCopts expands the cmake_pch artifacts splitCompileFragments
// detected (and withheld from copts) into the forced-include copts pairs
// that preserve cmake's PCH include semantics:
//
//	-include <hdr-1> -include <hdr-2> ...
//
// in the declared order (cmake_pch.h[xx] #includes them in that order, and
// forced includes are processed left-to-right). The header list comes from
// the compile group's own PrecompileHeaders when present (the declaring
// target — exact and config-resolved), else from resolve() keyed by the
// artifact path's owning `CMakeFiles/<target>.dir/` segment (the REUSE_FROM
// consumer case).
//
// stageHdrs returns the source-tree headers the expansion references, as
// reanchored element-relative paths, so the caller can ensure they're staged
// as action inputs. The declaring target already carries them (cmake lists
// PCH headers in the target's Sources), but a REUSE_FROM consumer does not.
func (c pchLiftCtx) forcedIncludeCopts(artifacts []string, cg fileapi.CompileGroup) (copts, stageHdrs []string) {
	if len(artifacts) == 0 {
		return nil, nil
	}
	seen := map[string]bool{}
	for _, art := range artifacts {
		entries := cg.PrecompileHeaders
		if len(entries) == 0 && c.resolve != nil {
			if owner := pchArtifactOwner(art, c.cmakeBuild); owner != "" {
				entries = c.resolve(owner, cg.Language)
			}
		}
		for _, e := range entries {
			arg, stage := c.includeArg(e.Header)
			if arg == "" || seen[arg] {
				continue
			}
			seen[arg] = true
			copts = append(copts, "-include", arg)
			if stage != "" {
				stageHdrs = append(stageHdrs, stage)
			}
		}
	}
	return copts, stageHdrs
}

// apply is the rule-level application of the lift, shared by lowerTarget's
// main compile-group path and splitCompileGroups' per-language subs: expand
// the withheld artifacts into forced-include copts, stage the expansion's
// source-tree headers into the rule's hdrs (append-if-missing — the
// declaring target already carries them via t.Sources; a REUSE_FROM
// consumer doesn't), and tag the user-visible target. The tag matters
// especially for the REUSE_FROM shape, whose codemodel PrecompileHeaders is
// null — without the artifact-driven tag it would lose its PCH silently.
func (c pchLiftCtx) apply(artifacts []string, cg fileapi.CompileGroup,
	copts, hdrs []string, irt *ir.Target) (newCopts, newHdrs []string) {
	pchCopts, pchHdrs := c.forcedIncludeCopts(artifacts, cg)
	copts = append(copts, pchCopts...)
	for _, h := range pchHdrs {
		if !stringSliceContains(hdrs, h) {
			hdrs = append(hdrs, h)
		}
	}
	if len(artifacts) > 0 && !stringSliceContains(irt.Tags, "cmake-codegen-pch") {
		irt.Tags = append(irt.Tags, "cmake-codegen-pch")
	}
	return copts, hdrs
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

// includeArg maps one codemodel precompileHeaders entry onto the argument
// of a `-include` copt, plus (when the entry is a source-tree file) the
// element-relative path the caller should stage as an action input.
//
// The entry shapes mirror what cmake writes into cmake_pch.h[xx]:
//
//   - `<vector>`   (angle form)    → `vector`, resolved via the include search
//     chain (`-include` falls back to the <...> chain after the quote chain).
//   - `"other.h"`  (verbatim form) → `other.h`, resolved via the target's
//     include paths the lift already replicates.
//   - absolute source-tree path    → exec-root path `<pkgPath>/<element-rel>`
//     (compile actions run from the exec root; see pchLiftCtx.pkgPath), with
//     the header itself staged via stage.
//   - absolute build-dir path      → exec-root path of the generated header
//     (`<pkgPath>/<build-rel>`; resolves once its genrule stages it — the
//     configure_file/codegen lifts own that half).
//   - other absolute path          → kept verbatim (out-of-tree system
//     header; cmake_pch baked the same absolute path).
//   - bare relative path           → kept verbatim (a generator-expression
//     result cmake didn't resolve; it rides the include search chain).
func (c pchLiftCtx) includeArg(h string) (arg, stage string) {
	switch {
	case h == "":
		return "", ""
	case strings.HasPrefix(h, "<") && strings.HasSuffix(h, ">"):
		return strings.TrimSuffix(strings.TrimPrefix(h, "<"), ">"), ""
	case strings.HasPrefix(h, `"`):
		return stripBalancedQuotes(h), ""
	case filepath.IsAbs(h):
		if rel, ok := relativeIfInside(c.cmakeSrc, h); ok {
			if c.reanchor != nil {
				rel = c.reanchor(rel)
			}
			return c.execRootPath(rel), rel
		}
		if rel, ok := relativeIfInsideRelaxed(c.cmakeBuild, h); ok {
			return c.execRootPath(rel), ""
		}
		return h, ""
	default:
		return h, ""
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
// The forced-include semantics themselves ride the baseline copts via
// the forcedIncludeCopts lift, not the per-config arms.
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
