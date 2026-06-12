package lower

import (
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/sstriker/buildstream-bazel/converter/ir"
)

// This file is the response-file half of the wrap-hierarchy genrule
// family (VTK's vtkWrapHierarchy shape, generalized): custom commands
// whose tool reads @<file> response files and positional data files
// that are THEMSELVES file(GENERATE) bakes. Three coordinated
// mechanical fixes make the recovered genrule executable:
//
//  1. dropNinjaDepfilePlumbing — the recovered cmd carries ninja's
//     incrementality machinery (`-MF <out>.d` plus an `&&`-chained
//     `cmake -E cmake_transform_depfile …` with an absolute cmake
//     path). Bazel genrules don't consume depfiles; the plumbing
//     writes outside the declared outs and references a host cmake.
//  2. rewriteGeneratedSrcRefs — the cmd references staged generated
//     srcs by their build-dir-relative spelling (`@Common/Core/
//     CMakeFiles/x.args`), which doesn't resolve from the action's
//     exec-root cwd. Rewriting to `$(location <src>)` makes Bazel
//     hand the action the real staged path — and split's
//     relocateGenruleSrcs relabels the reference in lockstep when
//     the genrule and the producing package diverge.
//  3. reanchorResponseContent — the BAKED response-file content
//     embeds convert-time absolute paths. Source-tree paths
//     re-anchor to the exec-root-relative element form
//     (<labelRoot>/<rel>); build-dir paths (generated headers) have
//     no static exec-root spelling — they live under the
//     configuration's output root — so they re-anchor to a
//     @BSB_GENDIR@ marker the CONSUMING genrule substitutes with
//     $(GENDIR) via a sed preamble at action time (fix 2 emits the
//     preamble for marker-carrying srcs, keyed on
//     cc.GendirMarkedOuts).

// bsbGendirMarker is the deterministic stand-in for the
// configuration-dependent output root in baked response-file
// content. Baking $(GENDIR) literally would be wrong (write_file
// content is not Make-var expanded); baking the convert-time build
// dir is non-deterministic across runs AND dangling at build time.
const bsbGendirMarker = "@BSB_GENDIR@"

// dropNinjaDepfilePlumbing strips ninja-incrementality depfile
// machinery from a recovered custom-command cmd: any `&&`-chained
// segment invoking `cmake -E cmake_transform_depfile`, and `-MF
// <path>` flag pairs whose path ends in `.d` and is not a declared
// out. Both are byproducts of cmake's Ninja generator wiring depfile
// support into the wrapping tool invocation — semantically inert for
// a sandboxed genrule whose inputs are fully declared.
func dropNinjaDepfilePlumbing(cmd string, outs []string) string {
	outSet := map[string]bool{}
	for _, o := range outs {
		outSet[o] = true
	}
	segments := strings.Split(cmd, " && ")
	kept := segments[:0]
	for _, seg := range segments {
		if strings.Contains(seg, " -E cmake_transform_depfile ") {
			continue
		}
		kept = append(kept, dropMFDepfileFlag(seg, outSet))
	}
	return strings.Join(kept, " && ")
}

// dropMFDepfileFlag removes `-MF <path>.d` pairs from one command
// segment, keeping any pair whose path is a declared output (then
// the depfile is a real product, not plumbing).
func dropMFDepfileFlag(seg string, outs map[string]bool) string {
	tokens := strings.Fields(seg)
	kept := tokens[:0]
	for i := 0; i < len(tokens); i++ {
		if tokens[i] == "-MF" && i+1 < len(tokens) &&
			strings.HasSuffix(tokens[i+1], ".d") && !outs[tokens[i+1]] {
			i++
			continue
		}
		kept = append(kept, tokens[i])
	}
	return strings.Join(kept, " ")
}

// rewriteGeneratedSrcRefs rewrites cmd references to GENERATED srcs
// (entries another recovered rule produces, per cc.OutToGenrule) so
// the action reads them from their real staged paths:
//
//   - plain generated srcs: `@<src>` → `@$(location <src>)`, bare
//     `<src>` → `$(location <src>)`;
//   - marker-carrying srcs (cc.GendirMarkedOuts — baked response
//     files whose content embeds @BSB_GENDIR@): routed through a sed
//     preamble that substitutes the marker with $(GENDIR) into a
//     scratch copy, and the cmd references the scratch copy.
//
// Longest src first so an entry that prefixes another can't
// half-rewrite it. Only whole-token occurrences rewrite (boundary
// guard via replaceBareToken / the explicit @ form).
func rewriteGeneratedSrcRefs(cmd string, srcs []string, cc *codegenContext) string {
	if cc == nil {
		return cmd
	}
	gen := make([]string, 0, len(srcs))
	for _, s := range srcs {
		if cc.OutToGenrule[s] != "" {
			gen = append(gen, s)
		}
	}
	if len(gen) == 0 {
		return cmd
	}
	sort.Slice(gen, func(i, j int) bool { return len(gen[i]) > len(gen[j]) })

	var sedLines []string
	for _, s := range gen {
		if !strings.Contains(cmd, s) {
			continue
		}
		var ref string
		if cc.GendirMarkedOuts[s] {
			scratch := "$$BSB_RD/" + sanitizePathToNameStem(s)
			sedLines = append(sedLines, fmt.Sprintf(
				`sed -e 's|%s|$(GENDIR)|g' $(location %s) > %s`, bsbGendirMarker, s, scratch))
			ref = scratch
		} else {
			ref = "$(location " + s + ")"
		}
		cmd = strings.ReplaceAll(cmd, "@"+s, "@"+ref)
		cmd = replaceBareToken(cmd, s, ref)
	}
	if len(sedLines) == 0 {
		return cmd
	}
	return "BSB_RD=$$(mktemp -d) && " + strings.Join(sedLines, " && ") + " && " + cmd
}

// responseFileGeneratedHdrs returns the generated header outputs a
// genrule's marker-carrying response files make reachable: for each
// src whose bake content carries `-I` roots under the
// @BSB_GENDIR@/<labelRoot>/ marker, every cc.OutToGenrule output
// under that root with a header-ish extension. In the cmake build
// those headers sit in the build-dir include root the `-I` names
// (vtkValueFromString.h does `#include "vtkCommonCoreModule.h"`, the
// configure-time export header) — the tool resolves them implicitly,
// so the ninja edge never declares them as inputs and the recovered
// genrule's srcs miss them. Mirroring the build-dir root's generated
// header set into srcs restores cmake's visibility; everything is
// label-addressable because each out is a recovered rule's product.
func responseFileGeneratedHdrs(srcs []string, cc *codegenContext, bazelPackagePath string) []string {
	if cc == nil || len(cc.GendirMarkedOuts) == 0 {
		return nil
	}
	contentByOut := map[string][]string{}
	for i := range cc.Genrules {
		t := &cc.Genrules[i]
		if t.Kind == ir.KindWriteFile && t.WriteFileOut != "" {
			contentByOut[t.WriteFileOut] = t.WriteFileContent
		}
	}
	rootPrefix := bsbGendirMarker + "/"
	if p := strings.Trim(bazelPackagePath, "/"); p != "" {
		rootPrefix += p + "/"
	}
	roots := map[string]bool{}
	for _, s := range srcs {
		if !cc.GendirMarkedOuts[s] {
			continue
		}
		for _, line := range contentByOut[s] {
			d, ok := strings.CutPrefix(strings.TrimSpace(line), "-I")
			if !ok {
				continue
			}
			d = strings.Trim(d, "'\"")
			if rel, ok := strings.CutPrefix(d, rootPrefix); ok && rel != "" {
				roots[strings.TrimSuffix(rel, "/")+"/"] = true
			}
		}
	}
	if len(roots) == 0 {
		return nil
	}
	existing := make(map[string]bool, len(srcs))
	for _, s := range srcs {
		existing[s] = true
	}
	var add []string
	for out := range cc.OutToGenrule {
		if existing[out] || !isHeaderishPath(out) {
			continue
		}
		for root := range roots {
			if strings.HasPrefix(out, root) {
				add = append(add, out)
				break
			}
		}
	}
	sort.Strings(add)
	return add
}

// isHeaderishPath reports whether a path carries a header-family
// extension (the include-resolution surface; matches the converter's
// header recognition family).
func isHeaderishPath(p string) bool {
	switch strings.ToLower(filepath.Ext(p)) {
	case ".h", ".hh", ".hpp", ".hxx", ".inc", ".inl", ".ipp", ".def":
		return true
	}
	return false
}

// stripConvertTimePathsCfg is reanchorConvertTimePaths' policy
// (prefix removal → package-relative) extended to the per-config
// scratch dirs (<buildDir>-cfg-<name>) the per-config bake passes
// configure into — those carry the suffix the plain prefix strip
// can't match, so a per-config body would otherwise keep raw scratch
// paths its primary never had.
func stripConvertTimePathsCfg(content, recordedSrcDir, recordedBuildDir string) string {
	if recordedBuildDir != "" {
		re := regexp.MustCompile(regexp.QuoteMeta(recordedBuildDir) + `(-cfg-[A-Za-z0-9_.+-]+)?/`)
		content = re.ReplaceAllString(content, "")
	}
	if recordedSrcDir != "" {
		content = strings.ReplaceAll(content, strings.TrimSuffix(recordedSrcDir, "/")+"/", "")
	}
	return content
}

// reanchorResponseContent rewrites convert-time absolute paths in
// baked file(GENERATE) content into deterministic, action-resolvable
// forms: source-tree paths to the exec-root-relative element form
// (<labelRoot>/<rel>), build-dir paths — including the per-config
// re-configure dirs (<buildDir>-cfg-<name>) — to
// @BSB_GENDIR@/<labelRoot>/<rel>. Returns the rewritten body and
// whether a build-dir path (the marker) was emitted, so the bake
// site can register the out in cc.GendirMarkedOuts for the consumer
// preamble. No-op (and marked=false) when the prefixes are empty or
// absent from the body.
func reanchorResponseContent(body []byte, recordedSrcDir, recordedBuildDir, labelRoot string) ([]byte, bool) {
	content := string(body)
	marked := false
	root := strings.Trim(labelRoot, "/")
	prefix := ""
	if root != "" {
		prefix = root + "/"
	}
	if recordedBuildDir != "" {
		re := regexp.MustCompile(regexp.QuoteMeta(recordedBuildDir) + `(-cfg-[A-Za-z0-9_.+-]+)?/`)
		if re.MatchString(content) {
			content = re.ReplaceAllString(content, bsbGendirMarker+"/"+prefix)
			marked = true
		}
	}
	if recordedSrcDir != "" {
		content = strings.ReplaceAll(content, strings.TrimSuffix(recordedSrcDir, "/")+"/", prefix)
	}
	return []byte(content), marked
}
