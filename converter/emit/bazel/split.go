package bazel

import (
	"bytes"
	"fmt"
	"hash/fnv"
	"sort"
	"strings"

	"github.com/sstriker/buildstream-bazel/converter/ir"
)

// EmitSplit renders a lowered *ir.Package as one BUILD.bazel per
// directory (the "gazelle model"), mirroring the
// CMakeLists/add_subdirectory layout the converter recorded in
// pkg.SubPackages. It returns a map keyed by element-root-relative
// sub-package directory — "" for the root package, "src/util" for a
// sub-directory — whose values are the rendered BUILD.bazel bytes for
// that package.
//
// The transform is the heart of --split-packages. It partitions the
// package's targets by their declaring directory, synthesizes a header
// cc_library per include-root so cmake's single-`-I`-root include
// semantics survive the split, and rewrites every intra-element dep /
// source path to the cross-package label form. It then calls the
// EXISTING EmitWithOptions once per package group (no new IR tree type,
// no emit-internals fork) with a per-package BazelPackagePath so the
// `# gazelle:cc_search` directives frame correctly.
//
// SourceKey regime: when opts.SourceKey != "", srcs/hdrs are emitted as
// @src_<key>//: absolute labels by EmitWithOptions and are
// package-location-independent. The transform leaves those paths
// element-root-relative (it never re-relativizes them); only the local
// (SourceKey=="") regime trims paths to the sub-package.
//
// Install-derived & synthesized targets (filegroups, cc_import,
// cmake_config_bundle, aliases, genrules, interface libs, per-language
// sub-libraries) carry no SubPackages entry and stay in the root
// package; their cross-element labels keep the element-root form.
func EmitSplit(pkg *ir.Package, opts Options) (map[string][]byte, error) {
	local := opts.SourceKey == ""

	// 1. Discover the package set and synthesize header libraries.
	//    headerLibs maps an include-root dir → the synthesized header
	//    cc_library's bare name; the package dir for that lib is the
	//    include-root dir itself.
	plan := planSplit(pkg, local)
	plan.setBase(strings.Trim(opts.BazelPackagePath, "/"))

	// 2. Partition real targets by their declaring directory and
	//    rewrite each target's srcs / hdrs / deps for the split layout.
	groups := map[string][]ir.Target{} // dir → targets
	// exportsByDir collects per-package exports_files() needs raised by
	// cross-package source references in the local regime.
	exportsByDir := map[string]map[string]struct{}{}

	// Ensure every header-lib package and every declaring dir exists as
	// a key so empty packages still render (rare, but keeps the set
	// stable).
	ensure := func(dir string) {
		if _, ok := groups[dir]; !ok {
			groups[dir] = nil
		}
	}

	// Synthesized build-time glob() filegroups backing file(GLOB)-sourced
	// genrules, plus the labels to splice into each producing genrule's srcs.
	globFilegroups, globLabels := plan.globSrcFilegroups(pkg)

	// Per-producing-package generated-header wrapper libraries, plus the
	// wrapper labels to splice into each consumer's deps (a consumer that
	// #includes a tablegen .inc).
	genHdrWrappers, genHdrLabels, err := plan.generatedHeaderWrappers(pkg)
	if err != nil {
		return nil, err
	}

	for _, t := range pkg.Targets {
		dir := plan.targetDir(t.Name)
		// Synthesized output-producing rules (genrule custom-command
		// recovery, write_file configure_file/file(GENERATE) bakes,
		// cmake_configure_file lifts) carry no SubPackages entry, so
		// targetDir defaults them to root. But a rule's output must
		// live in the rule's OWN Bazel package — a root rule whose
		// out is "include/llvm/Config/config.h" both collides with
		// the //include package's boundary ("output file conflicts
		// with another package") and is unreachable from a consumer
		// in //include that lists the file as a generated hdr
		// ("missing input file '//include:llvm/Config/config.h'").
		// Place the rule in the package that owns its (first / sole)
		// output so the out is package-local and a same-package
		// consumer resolves the bare-name srcs/hdrs reference (e.g.
		// eigen's compile_<snippet> cc_binaries off a configure_file
		// genrule; LLVM's per-package header libs off the config.h
		// write_file bakes). Only the local regime re-relativizes the
		// out (rewriteTarget below); the SourceKey regime keeps
		// element-root-relative paths.
		if local {
			if out := primaryGeneratedOutput(t); out != "" {
				if od := plan.deepestPkg(out); od != "" {
					dir = od
				}
			}
		}
		ensure(dir)
		// Drop file(GLOB)-covered srcs before rewriting: split serves them
		// via the synthesized glob() filegroup, so listing them explicitly
		// (relativized + exported) would be redundant. Guarded on the labels
		// actually existing, so the inputs are never silently lost.
		src := t
		if len(t.GlobSrcGroups) > 0 && len(globLabels[t.Name]) > 0 {
			src = dropGlobSrcFiles(t)
		}
		rt := rewriteTarget(src, dir, plan, local, exportsByDir)
		// Splice the synthesized file(GLOB) glob-filegroup labels into this
		// genrule's srcs (full labels, so no further rewriting), and drop
		// the now-consumed metadata.
		if labels := globLabels[t.Name]; len(labels) > 0 {
			rt.Srcs = append(append([]string(nil), rt.Srcs...), labels...)
			sort.Strings(rt.Srcs)
			rt.GlobSrcGroups = nil
		}
		// Splice the generated-header wrapper labels into this consumer's
		// deps (full cross-package labels, so no further rewriting). The
		// keep-marker pass tags them `# keep` so a gazelle maintenance pass
		// — which can't resolve the generated .inc to a target — preserves
		// the edge.
		if labels := genHdrLabels[t.Name]; len(labels) > 0 {
			rt.Deps = append(append([]string(nil), rt.Deps...), labels...)
			sort.Strings(rt.Deps)
		}
		// A filegroup / pkg_files whose only srcs were bare packaged
		// directories (dropped above) would render as an empty, useless
		// rule — and pkg_files/filegroup both require a non-empty srcs
		// (the template renders `srcs = ,` for an empty list, which is
		// unparseable). Skip it. install(FILES)/install(DIRECTORY) of a
		// dir that became its own package is served by the
		// layout-independent install-root path.
		if (rt.Kind == ir.KindFilegroup || rt.Kind == ir.KindPkgFiles) &&
			len(rt.Srcs) == 0 && len(t.Srcs) > 0 {
			continue
		}
		groups[dir] = append(groups[dir], rt)
	}

	// Add the synthesized header libraries to their include-root
	// packages.
	for inc, name := range plan.headerLibs {
		ensure(inc)
		groups[inc] = append(groups[inc], plan.headerLibTarget(inc, name, local, exportsByDir))
	}

	// Add the per-package root-walk header libs + their aggregate. These are
	// populated only for the multi-package RootInclude shape (abseil's
	// element-root grant spanning many packages); the single-package shape
	// (glm) leaves them empty and restores the prefix on the target itself.
	for owner, name := range plan.rootHdrLibs {
		ensure(owner)
		groups[owner] = append(groups[owner], plan.rootHdrLibTarget(owner, name))
	}
	if plan.rootHdrAgg != "" {
		ensure("")
		groups[""] = append(groups[""], plan.rootHdrAggTarget())
	}

	// Add the synthesized file(GLOB) glob() filegroups to their owning
	// packages (referenced by the globbing genrules above).
	for d, fgs := range globFilegroups {
		ensure(d)
		groups[d] = append(groups[d], fgs...)
	}

	// Add the synthesized generated-header wrapper libraries to their
	// producing packages (depended on by the consumers spliced above).
	for d, ws := range genHdrWrappers {
		ensure(d)
		groups[d] = append(groups[d], ws...)
	}

	// 3. Render each package group via the shared EmitWithOptions.
	base := strings.Trim(opts.BazelPackagePath, "/")
	out := map[string][]byte{}
	for dir, targets := range groups {
		sub := &ir.Package{
			Name:       pkg.Name,
			SourceRoot: pkg.SourceRoot,
			Targets:    targets,
		}
		// HeaderComments only on the root package.
		if dir == "" {
			sub.HeaderComments = pkg.HeaderComments
		}
		subOpts := opts
		subOpts.BazelPackagePath = joinPkgPath(base, dir)
		body, err := EmitWithOptions(sub, subOpts)
		if err != nil {
			return nil, fmt.Errorf("split-packages: emit %q: %w", dirKey(dir), err)
		}
		// Append any exports_files() this package owes cross-package
		// source consumers.
		if needs := exportsByDir[dir]; len(needs) > 0 {
			body, err = appendExportsFiles(body, needs)
			if err != nil {
				return nil, fmt.Errorf("split-packages: exports_files %q: %w", dirKey(dir), err)
			}
		}
		out[dir] = body
	}
	return out, nil
}

// primaryGeneratedOutput returns the element-root-relative output path
// EmitSplit uses to decide which package a synthesized output-producing
// rule belongs in: the first genrule out, the write_file out, or the
// cmake_configure_file out. Empty for non-producing kinds (real
// cc_library / cc_binary targets, whose package comes from SubPackages).
func primaryGeneratedOutput(t ir.Target) string {
	switch t.Kind {
	case ir.KindGenrule:
		if len(t.GenruleOuts) > 0 {
			return t.GenruleOuts[0]
		}
	case ir.KindWriteFile:
		return t.WriteFileOut
	case ir.KindCMakeConfigureFile:
		if t.CMakeConfigureFile != nil {
			return t.CMakeConfigureFile.Out
		}
	}
	return ""
}

// splitPlan is the precomputed split layout: per-target declaring dir,
// the synthesized header-library names per include-root, and the set of
// headers physically under each include-root.
type splitPlan struct {
	sub        map[string]string   // target name → declaring dir ("" = root)
	headerLibs map[string]string   // include-root dir → header-lib name
	headersIn  map[string][]string // include-root dir → element-root-relative header paths under it
	base       string              // repo-root-relative element package path (label base)
	pkgs       []string            // every sub-package dir (incl. ""), longest-first

	// Root-walk (element-root include) header libraries. A target whose
	// include root is the ELEMENT ROOT (RootInclude — cmake's
	// target_include_directories(${PROJECT_SOURCE_DIR})) has the "" entry
	// dropped from Includes (Bazel rejects includes=[""]) and its headers
	// folded into Hdrs by the discoverHeaders walk. When those headers span a
	// SINGLE package, rewriteTarget restores the prefix with include_prefix on
	// the target itself (the glm path). When they span MULTIPLE packages under
	// split, include_prefix can't apply, so the element-root header surface is
	// re-homed into per-package header libs (each carrying its package's
	// headers with include_prefix=<pkg>) aggregated behind one lib that every
	// RootInclude target depends on. These maps drive that synthesis; they stay
	// empty (and the glm path stays in force) unless the root-walk set is
	// genuinely multi-package.
	rootHdrLibs map[string]string   // owning package dir → root-walk header-lib name
	rootHdrsIn  map[string][]string // owning package dir → element-root-relative header paths
	rootHdrAgg  string              // aggregate lib name in the root package ("" when no multi-package root walk)

	// genOuts is the set of element-root-relative paths that are GENERATED by
	// some rule in the package (a genrule out or a write_file out), as opposed
	// to on-disk source files. A cross-package reference to a generated file
	// resolves to the producing rule's output target directly, so it must NOT
	// also be raised in the owning package's exports_files() — Bazel errors
	// "source file X conflicts with existing generated file from rule …". Used
	// by headerLibTarget to suppress the exports_files() need for the OpenBLAS
	// codegen sources (driver/<n>/CMakeFiles/<routine>.c, written by write_file)
	// it rolls into the root include-root header lib.
	genOuts map[string]bool

	// genOutProducer maps a generated output path (genrule out / write_file out)
	// to the NAME of the rule that produces it, so a cross-package consumer of
	// that output can publicize the producer (see publicize).
	genOutProducer map[string]string

	// publicize is the set of rule names whose visibility must be forced to
	// public because a generated output of theirs is referenced directly (in
	// srcs) by a consumer in a DIFFERENT package. A genrule's outputs inherit
	// the genrule's visibility, so a private producer (the converter's default
	// for standalone custom commands) makes its output unreachable from another
	// package — e.g. curl's `tool_hugehelp.c` genrule in //src consumes the
	// `docs/cmdline-opts/curl.txt` output of a genrule in the root package.
	// The generated-HEADER (.inc) path doesn't need this: it's consumed via a
	// same-package `generated_includes` wrapper (itself public), not by a direct
	// cross-package output reference.
	publicize map[string]bool
}

// deepestPkg returns the longest sub-package directory that owns path p
// (p equals it or sits under it). "" (the root package) owns everything
// not claimed by a deeper package, so it is always the fallback. Used to
// detect when a retained srcs/hdrs entry would cross into a deeper
// package — an illegal Bazel cross-boundary reference once that
// directory has its own BUILD.
func (p *splitPlan) deepestPkg(s string) string {
	for _, d := range p.pkgs {
		if _, ok := relUnder(d, s); ok {
			return d
		}
	}
	return ""
}

// targetDir returns the declaring directory of target name, "" (root)
// when unknown — install-derived / synthesized targets fall here.
func (p *splitPlan) targetDir(name string) string {
	if d, ok := p.sub[name]; ok {
		return d
	}
	return ""
}

// landingDir returns the Bazel package dir a target is placed in by the emit
// loop: its declaring dir, overridden (local regime) to the package owning its
// primary generated output so the out is package-local. Mirrors the dir
// computation in EmitSplit's partition loop; used by the cross-package
// gen-output publicize pre-pass.
func (p *splitPlan) landingDir(t ir.Target, local bool) string {
	dir := p.targetDir(t.Name)
	if local {
		if out := primaryGeneratedOutput(t); out != "" {
			if od := p.deepestPkg(out); od != "" {
				dir = od
			}
		}
	}
	return dir
}

// headerLibTarget builds the synthesized header cc_library for an
// include-root dir. The lib lives in package `inc`; a header physically
// under a DEEPER package than `inc` can't be named by a package-relative
// string — that's an invalid label ("'…/sub' is a subpackage"). Three cases
// for such a deeper-package entry (local regime only — the SourceKey regime
// prefixes hdrs to package-location-independent @src_<key>//: labels, so no
// relabel):
//
//   - a GENERATED compiled source (a write_file/genrule .c/.S/... out, e.g.
//     OpenBLAS rolls every codegenned `driver/<n>/CMakeFiles/<routine>.c`
//     into the root include-root's header surface): DROP it. It's a compiled
//     translation unit its owning package already builds in a cc_library
//     srcs — never #included as a header — so it has no place in a header
//     lib's hdrs, and keeping it would only force the producing write_file
//     rule public (it's emitted private) to satisfy cross-package visibility.
//   - any other deeper-package SOURCE header (a real .h sibling): cross-
//     package file label + an exports_files() need in the owning package,
//     exactly like rewriteTarget does for a real target's cross-package hdrs.
//   - a generated *header* (.h/.inc out): cross-package label, but NO
//     exports_files() (a generated file is already a target; exports_files()
//     over it errors "source file conflicts with existing generated file").
func (p *splitPlan) headerLibTarget(inc, name string, local bool, exportsByDir map[string]map[string]struct{}) ir.Target {
	hdrs := make([]string, 0, len(p.headersIn[inc]))
	for _, h := range p.headersIn[inc] {
		if local {
			if dh := p.deepestPkg(h); dh != inc {
				// A generated compiled source owned by a deeper package is a
				// translation unit that package compiles itself — drop it from
				// the header aggregation rather than relabel+expose it.
				if p.genOuts[h] && isCompiledSourceExt(h) {
					continue
				}
				// Header owned by a deeper package: cross-package label (+
				// exports_files() for an on-disk source). A bare packaged dir
				// (file=="") isn't a file label, so it falls through to the
				// package-relative path below.
				if file, _ := relUnder(dh, h); file != "" {
					hdrs = append(hdrs, crossPkgFileLabel(p, dh, file))
					if !p.genOuts[h] {
						if exportsByDir[dh] == nil {
							exportsByDir[dh] = map[string]struct{}{}
						}
						exportsByDir[dh][file] = struct{}{}
					}
					continue
				}
			}
		}
		rel, _ := relUnder(inc, h)
		hdrs = append(hdrs, rel)
	}
	sort.Strings(hdrs)

	// Forward to descendant include-root header libs. cmake's include
	// dirs are additive and recursive: a target with `-I<inc>` can
	// `#include` any header physically under `<inc>`, spelled relative
	// to `<inc>`. Monolithic emit captures this because discoverHeaders
	// walks `<inc>` recursively, so every such header lands in one
	// target's hdrs. Under --split-packages each header is assigned to
	// its LONGEST-matching include-root (planSplit), so when include
	// roots nest — VTK's `vtk_module_third_party` forwarders declare
	// `ThirdParty/token`, `ThirdParty/token/vtktoken`, and (via the
	// internal lib's `includes=["."]`) `ThirdParty/token/vtktoken/token`
	// — the deepest root claims every header and the ancestor header
	// libs are left empty. A consumer that depended on the ancestor lib
	// (because it had `-IThirdParty/token` on its include path) would
	// then resolve no headers. Restore the recursive reachability by
	// depending on every strict-descendant include-root's header lib:
	// Bazel stages those libs' hdrs at their package-relative paths, and
	// this lib's own `includes=["."]` supplies the search root, so the
	// ancestor-prefixed `#include <vtktoken/token/Token.h>` resolves.
	// As a bonus this turns the otherwise-empty ancestor cc_library into
	// a non-empty one, clearing the empty-cc-library idiom finding.
	var deps []string
	for r2, n2 := range p.headerLibs {
		if r2 == inc {
			continue
		}
		if _, ok := relUnder(inc, r2); ok {
			deps = append(deps, headerLibLabel(p, r2, n2))
		}
	}
	sort.Strings(deps)

	return ir.Target{
		Name:       name,
		Kind:       ir.KindCCLibrary,
		Hdrs:       hdrs,
		Includes:   []string{"."},
		Deps:       deps,
		Visibility: []string{"//visibility:public"},
	}
}

// rootHdrLibTarget builds the per-package root-walk header library for owner
// (splitPlan.rootHdrLibs). It carries owner's slice of the element-root header
// surface, re-relativized to the package and re-prefixed with
// include_prefix=<owner> so that cmake's `#include "<owner>/foo.h"` resolves
// again after --split-packages re-homed the file to a package-local path.
// owner=="" (the root package) needs no prefix — those headers already sit at
// their element-root-relative paths.
func (p *splitPlan) rootHdrLibTarget(owner, name string) ir.Target {
	hdrs := make([]string, 0, len(p.rootHdrsIn[owner]))
	for _, h := range p.rootHdrsIn[owner] {
		rel, _ := relUnder(owner, h)
		hdrs = append(hdrs, rel)
	}
	sort.Strings(hdrs)
	t := ir.Target{
		Name:       name,
		Kind:       ir.KindCCLibrary,
		Hdrs:       hdrs,
		Visibility: []string{"//visibility:public"},
	}
	if owner != "" {
		// A subpackage lib's hdrs were re-relativized to the package
		// (casts.h, not absl/base/casts.h), so restore the element-root path
		// with include_prefix=<owner> → `#include "absl/base/casts.h"` resolves.
		t.IncludePrefix = owner
	} else {
		// The root-package lib owns the headers in element-root dirs that are
		// NOT their own Bazel package (abseil's header-only absl/meta,
		// absl/utility, …). Those hdrs already sit at their element-root paths
		// (absl/meta/type_traits.h), so includes=["."] — which puts the element
		// root on the search path — is what makes `#include "absl/meta/..."`
		// resolve (include_prefix would wrongly double the absl/ prefix).
		t.Includes = []string{"."}
	}
	return t
}

// rootHdrAggTarget builds the aggregate root-walk header library: a headerless
// cc_library in the root package whose deps fan out to every per-package
// rootHdrLib, so a single dep on it re-exposes the whole element-root `#include`
// surface (each piece at its include_prefix-restored path). Every RootInclude
// target that triggered the multi-package split depends on this aggregate
// (rewriteTarget) — mirroring the `target_include_directories(${SOURCE_DIR})`
// grant, which lets such a target include any header under the root.
func (p *splitPlan) rootHdrAggTarget() ir.Target {
	deps := make([]string, 0, len(p.rootHdrLibs))
	for owner, name := range p.rootHdrLibs {
		deps = append(deps, headerLibLabel(p, owner, name))
	}
	sort.Strings(deps)
	return ir.Target{
		Name:       p.rootHdrAgg,
		Kind:       ir.KindCCLibrary,
		Deps:       deps,
		Visibility: []string{"//visibility:public"},
	}
}

// globSrcFilegroups synthesizes the build-time glob() filegroups that back
// file(GLOB)-derived genrule sources. For each GlobSrcGroup{Dir, Pattern}
// on a genrule it locates Dir's owning package and emits — deduped per
// (package, name) — a filegroup(srcs = glob(["<rel>/<pattern>"])), then
// records the filegroup's label so EmitSplit can splice it into the
// genrule's srcs. Keeping the glob in project B (rather than the frozen
// convert-time match set) is what re-evaluates the genrule's deps when a
// matching file is added post-conversion.
//
// byDir maps owning-package dir → the filegroups to inject there;
// labelsFor maps genrule name → the filegroup labels to add to its srcs.
func (p *splitPlan) globSrcFilegroups(pkg *ir.Package) (byDir map[string][]ir.Target, labelsFor map[string][]string) {
	byDir = map[string][]ir.Target{}
	labelsFor = map[string][]string{}
	seen := map[string]bool{} // owningDir + "\x00" + name → already synthesized
	for _, t := range pkg.Targets {
		if t.Kind != ir.KindGenrule || len(t.GlobSrcGroups) == 0 {
			continue
		}
		var labels []string
		seenLabel := map[string]bool{}
		for _, g := range t.GlobSrcGroups {
			owningDir := p.deepestPkg(g.Dir)
			rel, _ := relUnder(owningDir, g.Dir)
			pattern := g.Pattern
			if rel != "" {
				pattern = rel + "/" + pattern
			}
			name := globSrcName(rel, g.Pattern)
			label := headerLibLabel(p, owningDir, name)
			if !seenLabel[label] {
				seenLabel[label] = true
				labels = append(labels, label)
			}
			if key := owningDir + "\x00" + name; !seen[key] {
				seen[key] = true
				byDir[owningDir] = append(byDir[owningDir], ir.Target{
					Name:          name,
					Kind:          ir.KindFilegroup,
					FilegroupGlob: []string{pattern},
					Visibility:    []string{"//visibility:public"},
				})
			}
		}
		sort.Strings(labels)
		labelsFor[t.Name] = labels
	}
	return byDir, labelsFor
}

// generatedIncludesName is the name of the synthesized generated-header
// wrapper cc_library in each producing package. It deliberately does NOT end
// in "_headers" (nor "_srcs"): split's other synthesized rules use those
// suffixes — headerLibName(dir) is `<dir>_headers`, globSrcName is
// `..._srcs` — so a bare "generated_headers" would collide with the header
// lib of an include root literally named "generated". The "_includes" form
// sits outside every synthesized-name scheme; generatedHeaderWrappers still
// fails fast if a real project target claims it. The consumer-side per-item
// keep marker recognizes a dep on the wrapper by this label suffix.
const generatedIncludesName = "generated_includes"

// generatedIncludesTag marks the synthesized wrapper so addKeepMarkers can
// whole-rule-keep it by tag — robust against a user rule that happens to
// share generatedIncludesName in a non-producing package (which must NOT be
// kept), and against any future rename.
const generatedIncludesTag = "cmake-codegen-generated-includes"

// generatedHeaderWrappers synthesizes, per producing package, a
// generated-header wrapper cc_library (named generatedIncludesName) carrying
// that package's generated `.inc` headers in textual_hdrs (with
// includes=["."] supplying the genfiles include root), and returns the
// wrapper labels each consumer must depend on. Producer and consumer are
// usually distinct packages — a tablegen `.inc` is generated under //include
// but #included by a library in //llvm/lib/... — so the consumer can't reach
// the genrule output as a bare same-package input; it depends on the
// wrapper, whose textual_hdrs declare the output as an input and whose
// includes put the package root on the consumer's -I. byDir maps a
// producing-package dir → its wrapper; labelsFor maps a consumer name → the
// sorted wrapper labels it needs.
func (p *splitPlan) generatedHeaderWrappers(pkg *ir.Package) (byDir map[string][]ir.Target, labelsFor map[string][]string, err error) {
	byDir = map[string][]ir.Target{}
	labelsFor = map[string][]string{}
	if len(pkg.CodegenHeaderConsumers) == 0 {
		return byDir, labelsFor, nil
	}
	// Accumulate the package-local header set per producing dir across all
	// consumers: each producing package gets one wrapper that is the union
	// of every generated header any consumer needs from it.
	hdrsByDir := map[string]map[string]bool{}
	for _, outs := range pkg.CodegenHeaderConsumers {
		for _, o := range outs {
			dir := p.deepestPkg(o)
			rel, ok := relUnder(dir, o)
			if !ok {
				continue
			}
			if hdrsByDir[dir] == nil {
				hdrsByDir[dir] = map[string]bool{}
			}
			hdrsByDir[dir][rel] = true
		}
	}
	// Guard the reserved wrapper name: a real same-named target in a
	// producing package would be a duplicate Bazel target (load error), and
	// renaming isn't safe here (the consumer-side label and its per-item keep
	// marker key off this exact name). Fail fast with a clear message rather
	// than emit a BUILD that won't load.
	for _, t := range pkg.Targets {
		if t.Name != generatedIncludesName {
			continue
		}
		if d := p.targetDir(t.Name); hdrsByDir[d] != nil {
			return nil, nil, fmt.Errorf("split-packages: package %q already declares a target named %q, which the converter reserves for tablegen-consumer generated-header wiring — rename the project target", dirKey(d), generatedIncludesName)
		}
	}
	for dir, set := range hdrsByDir {
		hdrs := make([]string, 0, len(set))
		for h := range set {
			hdrs = append(hdrs, h)
		}
		sort.Strings(hdrs)
		byDir[dir] = append(byDir[dir], ir.Target{
			Name:        generatedIncludesName,
			Kind:        ir.KindCCLibrary,
			TextualHdrs: hdrs,
			// includes=["."] supplies the genfiles include root so a consumer
			// #includes the .inc by its package-relative path — matching
			// headerLibTarget, which sets it for every include root. The
			// package base makes "." the element package (e.g.
			// elements/<name>), not the workspace root, so Bazel's
			// root-include guard doesn't fire.
			Includes:   []string{"."},
			Visibility: []string{"//visibility:public"},
			// Tag so addKeepMarkers whole-rule-keeps the synthesized wrapper
			// by tag (gazelle would delete a cc_library with no on-disk srcs)
			// without mistaking a user rule that shares the name.
			Tags: []string{generatedIncludesTag},
		})
	}
	for cons, outs := range pkg.CodegenHeaderConsumers {
		seen := map[string]bool{}
		var labels []string
		for _, o := range outs {
			dir := p.deepestPkg(o)
			if _, ok := relUnder(dir, o); !ok {
				continue
			}
			label := headerLibLabel(p, dir, generatedIncludesName)
			if !seen[label] {
				seen[label] = true
				labels = append(labels, label)
			}
		}
		sort.Strings(labels)
		labelsFor[cons] = labels
	}
	return byDir, labelsFor, nil
}

// globSrcName derives a deterministic, package-unique filegroup name for a
// file(GLOB)-derived source group: "glob_<ext>_srcs" at a package root, or
// "<rel>_glob_<ext>_srcs" for a subdir glob (rel + ext sanitized to a legal
// identifier). The extension is taken from the pattern's trailing "*.<ext>";
// patterns without one fall back to "files".
func globSrcName(relInPkg, pattern string) string {
	// Map every char that isn't legal in a Bazel target name to "_", so an
	// extension carrying glob syntax ("*.[ch]", "*.?pp") can't leak '[' /
	// '?' / ']' into the name.
	san := func(s string) string {
		return strings.Map(func(r rune) rune {
			switch {
			case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_':
				return r
			default:
				return '_'
			}
		}, s)
	}
	ext := "files"
	if i := strings.LastIndex(pattern, "*."); i >= 0 {
		ext = san(pattern[i+2:])
	}
	// Disambiguate by a stable hash of the full (rel, pattern): distinct
	// globs that share rel+ext — "*.td" vs "**/*.td", "*.txt" vs
	// "foo*.txt" — must not collide into (and be wrongly deduped to) one
	// filegroup, which would point some genrules at the wrong glob.
	h := fnv.New32a()
	_, _ = h.Write([]byte(relInPkg + "\x00" + pattern))
	disc := fmt.Sprintf("%08x", h.Sum32())
	if relInPkg == "" {
		return "glob_" + ext + "_" + disc + "_srcs"
	}
	return san(relInPkg) + "_glob_" + ext + "_" + disc + "_srcs"
}

// dropGlobSrcFiles returns a copy of t with the file(GLOB)-covered sources
// (recorded on its GlobSrcGroups.Files) removed from Srcs. split serves
// those via the synthesized glob() filegroups, so the explicit entries
// would be redundant; lower keeps them in Srcs so the monolithic emitter —
// which synthesizes no filegroup — still sees the inputs.
func dropGlobSrcFiles(t ir.Target) ir.Target {
	drop := map[string]bool{}
	for _, g := range t.GlobSrcGroups {
		for _, f := range g.Files {
			drop[f] = true
		}
	}
	if len(drop) == 0 {
		return t
	}
	kept := make([]string, 0, len(t.Srcs))
	for _, s := range t.Srcs {
		if !drop[s] {
			kept = append(kept, s)
		}
	}
	t.Srcs = kept
	return t
}

// planSplit computes the split layout from a lowered package.
func planSplit(pkg *ir.Package, local bool) *splitPlan {
	p := &splitPlan{
		sub:            map[string]string{},
		headerLibs:     map[string]string{},
		headersIn:      map[string][]string{},
		genOuts:        map[string]bool{},
		genOutProducer: map[string]string{},
		publicize:      map[string]bool{},
	}
	for k, v := range pkg.SubPackages {
		p.sub[k] = v
	}
	// Index every generated output (genrule outs + write_file out) so a
	// cross-package reference to one isn't double-declared via exports_files()
	// (see splitPlan.genOuts). Paths are element-root-relative slash form, the
	// same shape as headersIn entries — compared raw like the rest of split.go.
	for _, t := range pkg.Targets {
		for _, o := range t.GenruleOuts {
			p.genOuts[o] = true
			p.genOutProducer[o] = t.Name
		}
		if t.WriteFileOut != "" {
			p.genOuts[t.WriteFileOut] = true
			p.genOutProducer[t.WriteFileOut] = t.Name
		}
	}
	// Collect every include-root dir and the union of every target's
	// headers so header-library synthesis can glob the right files.
	incRoots := map[string]struct{}{}
	allHdrs := map[string]struct{}{}
	realNames := map[string]struct{}{}
	for _, t := range pkg.Targets {
		realNames[t.Name] = struct{}{}
		for _, inc := range t.Includes {
			// An unresolved generator expression ("$<…>") is never a real
			// include root — synthesizing a header lib for it produces an
			// invalid Bazel target name ("$<…>_headers") that aborts the
			// emit. ToIR's dropGenexIncludeDirs strips these upstream; this
			// is a backstop so emit can't be aborted by one that slips past.
			if strings.Contains(inc, "$<") {
				continue
			}
			incRoots[normDir(inc)] = struct{}{}
		}
		for _, h := range t.Hdrs {
			allHdrs[h] = struct{}{}
		}
	}

	// Assign each header to the include-root it lives under (longest
	// match wins so nested include roots claim their own headers).
	roots := make([]string, 0, len(incRoots))
	for r := range incRoots {
		roots = append(roots, r)
	}
	// Longest first for longest-prefix assignment.
	sort.Slice(roots, func(i, j int) bool {
		if len(roots[i]) != len(roots[j]) {
			return len(roots[i]) > len(roots[j])
		}
		return roots[i] < roots[j]
	})
	for h := range allHdrs {
		for _, r := range roots {
			if _, ok := relUnder(r, h); ok {
				p.headersIn[r] = append(p.headersIn[r], h)
				break
			}
		}
	}

	// Synthesize a deterministic, unique header-lib name per
	// include-root.
	for _, r := range roots {
		name := headerLibName(r)
		// Ensure uniqueness vs real targets and other header libs.
		base := name
		for i := 1; ; i++ {
			if _, clash := realNames[name]; !clash && !hasValue(p.headerLibs, name) {
				break
			}
			name = fmt.Sprintf("%s_%d", base, i)
		}
		p.headerLibs[r] = name
	}

	// The full sub-package set = root ∪ target-declaring dirs ∪
	// include-root dirs, longest-first so deepestPkg does longest-prefix
	// matching.
	pkgSet := map[string]struct{}{"": {}}
	for _, d := range p.sub {
		pkgSet[normDir(d)] = struct{}{}
	}
	for r := range p.headerLibs {
		pkgSet[normDir(r)] = struct{}{}
	}
	for d := range pkgSet {
		p.pkgs = append(p.pkgs, d)
	}
	sort.Slice(p.pkgs, func(i, j int) bool {
		if len(p.pkgs[i]) != len(p.pkgs[j]) {
			return len(p.pkgs[i]) > len(p.pkgs[j])
		}
		return p.pkgs[i] < p.pkgs[j]
	})

	// Publicize any producer whose generated output is referenced directly (in
	// srcs) by a consumer landing in a DIFFERENT package — the producer's
	// private default would make that output unreachable cross-package (Bazel
	// "target X is not visible from Y", e.g. curl's `tool_hugehelp.c` genrule in
	// //src consuming the `docs/cmdline-opts/curl.txt` output of a root-package
	// genrule). Needs p.pkgs (deepestPkg / landingDir), hence after the pkgSet
	// build. Computed before the emit loop so a producer processed before its
	// consumer still gets the bump.
	for _, t := range pkg.Targets {
		consumerDir := p.landingDir(t, local)
		for _, s := range t.Srcs {
			prod, ok := p.genOutProducer[s]
			if !ok || prod == t.Name {
				continue
			}
			if p.deepestPkg(s) != consumerDir {
				p.publicize[prod] = true
			}
		}
	}

	// Root-walk header-lib synthesis (see splitPlan.rootHdrLibs). Collect the
	// union of every RootInclude target's headers and bucket them by owning
	// package (deepestPkg, which needs p.pkgs — hence after the pkgSet build).
	// Only when the surface spans MORE THAN ONE package does the
	// include_prefix-on-the-target path (rewriteTarget, the glm shape) break
	// down — a single package keeps that path. In the multi-package case
	// re-home the surface into per-package header libs behind one aggregate.
	//
	// Local regime only (opts.SourceKey==""), like the include_prefix gate this
	// replaces: the SourceKey regime keeps hdrs element-root-relative (the
	// emitter prefixes them to @src_<key>//:tree_dir/<path>), so re-relativizing
	// + include_prefix here would emit wrong labels. Gating the synthesis off
	// keeps rootHdrLibs/rootHdrAgg empty there, so emission and the
	// rewriteTarget fast-path both no-op.
	p.rootHdrLibs = map[string]string{}
	p.rootHdrsIn = map[string][]string{}
	rootWalk := map[string]struct{}{}
	for _, t := range pkg.Targets {
		if !t.RootInclude {
			continue
		}
		for _, h := range t.Hdrs {
			rootWalk[h] = struct{}{}
		}
	}
	byPkg := map[string][]string{}
	for h := range rootWalk {
		owner := p.deepestPkg(h)
		byPkg[owner] = append(byPkg[owner], h)
	}
	if local && len(byPkg) > 1 {
		uniqueName := func(name string) string {
			base := name
			for i := 1; ; i++ {
				_, clash := realNames[name]
				if !clash && !hasValue(p.headerLibs, name) && !hasValue(p.rootHdrLibs, name) && name != p.rootHdrAgg {
					return name
				}
				name = fmt.Sprintf("%s_%d", base, i)
			}
		}
		// Deterministic order: synthesize per-package libs in package order.
		owners := make([]string, 0, len(byPkg))
		for owner := range byPkg {
			owners = append(owners, owner)
		}
		sort.Strings(owners)
		for _, owner := range owners {
			hs := byPkg[owner]
			sort.Strings(hs)
			p.rootHdrsIn[owner] = hs
			p.rootHdrLibs[owner] = uniqueName(rootHdrLibName(owner))
		}
		p.rootHdrAgg = uniqueName("element_root_headers")
	}
	return p
}

// rewriteTarget produces the sub-package-local copy of a real target:
// re-relativize srcs/hdrs to its declaring dir, strip include-roots +
// their headers (now owned by the synthesized header libs), and rewrite
// intra-element deps to cross-package labels.
func rewriteTarget(t ir.Target, dir string, plan *splitPlan, local bool, exportsByDir map[string]map[string]struct{}) ir.Target {
	rt := t

	// Multi-package RootInclude fast-path (abseil's element-root grant spanning
	// many packages): planSplit re-homed the HEADER surface (every RootInclude
	// target's t.Hdrs) into per-package header libs behind plan.rootHdrAgg, so
	// this target drops its now-redundant copy of (nearly) all element headers and
	// instead depends on the aggregate (wired at the bottom). Compute it up front
	// so the hdr-rewrite loop below SKIPS this target — otherwise it'd
	// cross-package-relabel + exports_files() the whole ~397-header walked set,
	// which is pure BUILD bloat (and a collision risk) since it's dropped anyway.
	// Local-only, matching the synthesis gate (the SourceKey regime keeps hdrs
	// element-root-relative and never synthesizes the libs).
	//
	// Only Hdrs are re-homed — textual_hdrs are NOT collected into the root-walk
	// surface, so they still need their normal package-local / cross-package
	// rewrite (the textual loop below runs for fast-path targets too). Dropping
	// them here would lose textual inputs the aggregate never provides.
	rootHdrFastPath := local && t.RootInclude && plan.rootHdrAgg != ""

	// Header libs this target must depend on (one per include-root it
	// referenced), plus the residual includes (none, after the split).
	// headerDeps feed the consumer-visible deps (PUBLIC/INTERFACE
	// includes, which propagate); privHeaderDeps feed the non-propagating
	// implementation_deps (PRIVATE includes — see the copt scan below).
	var headerDeps []string
	var privHeaderDeps []string
	incRoots := map[string]struct{}{}
	for _, inc := range t.Includes {
		n := normDir(inc)
		if name, ok := plan.headerLibs[n]; ok {
			incRoots[n] = struct{}{}
			headerDeps = append(headerDeps, headerLibLabel(plan, n, name))
		}
	}
	rt.Includes = nil

	// PRIVATE target_include_directories ride in Copts as "-I<dir>" /
	// "-isystem<dir>" rather than t.Includes, so lower can honour cmake's
	// PRIVATE-doesn't-propagate semantics (Bazel's `includes` attribute is
	// consumer-visible; copts aren't). Under --split-packages that breaks
	// when <dir> is a synthesized header-lib root: the bare "-I<dir>" sets
	// the search path but leaves that package's headers undeclared as
	// inputs, so the sandbox can't find them (fmt's posix-mock-test:
	// `#include <fmt/os.h>` via a PRIVATE -Iinclude into the split-out
	// //include package — R_X86_64 "No such file or directory"). Wire the
	// header lib so its hdrs become inputs, routing it like lower routes a
	// PRIVATE link: implementation_deps on cc_library/cc_interface (the
	// non-propagating slot that preserves PRIVATE) and deps on
	// binary/test kinds (no implementation_deps bucket; no consumers to
	// leak to). The "-I<dir>" copt is then redundant with the header lib's
	// includes=["."] — drop it, mirroring the public-include path above.
	if len(rt.Copts) > 0 {
		kept := make([]string, 0, len(rt.Copts))
		for _, c := range rt.Copts {
			dir, isInc := includeDirFromCopt(c)
			n := normDir(dir)
			if isInc && n != "" {
				if _, already := incRoots[n]; already {
					continue // header dep already wired via a public include
				}
				if name, isRoot := plan.headerLibs[n]; isRoot {
					incRoots[n] = struct{}{}
					label := headerLibLabel(plan, n, name)
					if t.Kind == ir.KindCCLibrary || t.Kind == ir.KindCCInterface {
						privHeaderDeps = append(privHeaderDeps, label)
					} else {
						headerDeps = append(headerDeps, label)
					}
					continue // drop the now-redundant -I copt
				}
			}
			kept = append(kept, c)
		}
		rt.Copts = kept
	}

	// Drop headers now owned by a synthesized header lib; keep the rest
	// (physically under this target's dir) package-relative.
	var keepHdrs []string
	for _, h := range t.Hdrs {
		if rootHdrFastPath {
			// Surface dropped + served by the aggregate (set above); skip the
			// per-header cross-package relabel + exports_files() recording,
			// which over the whole walked set is pure BUILD bloat.
			break
		}
		owned := false
		for inc := range incRoots {
			if _, ok := relUnder(inc, h); ok {
				owned = true
				break
			}
		}
		if owned {
			continue
		}
		if !local {
			// SourceKey regime: hdrs become @src_<key>//: absolute labels,
			// which are package-location-independent — keep as-is.
			keepHdrs = append(keepHdrs, h)
			continue
		}
		if plan.deepestPkg(h) == dir {
			rel, _ := relUnder(dir, h)
			keepHdrs = append(keepHdrs, rel)
			continue
		}
		// Cross-package header with no owning header lib: reference it by a
		// cross-package label + raise an exports_files() need in the owning
		// package — the same treatment cross-package source FILEs get below.
		// (Dropping it, as we used to, loses headers a target genuinely
		// needs: e.g. a test that pulls a sibling .cc cross-package whose
		// quote-include resolves to that same package's sibling .h — the
		// source-dir siblings #413 stages.) A bare packaged directory
		// (file == "") isn't a file label, so that case still drops.
		dh := plan.deepestPkg(h)
		if file, _ := relUnder(dh, h); file != "" {
			keepHdrs = append(keepHdrs, crossPkgFileLabel(plan, dh, file))
			// A generated file (write_file/genrule out) is already a target in
			// its package; exports_files() over it would error ("source file
			// conflicts with existing generated file"). The label resolves to
			// the rule's output regardless. Only on-disk sources need the export.
			if !plan.genOuts[h] {
				if exportsByDir[dh] == nil {
					exportsByDir[dh] = map[string]struct{}{}
				}
				exportsByDir[dh][file] = struct{}{}
			}
		}
	}
	rt.Hdrs = keepHdrs

	// Re-relativize / relabel textual_hdrs the same way as hdrs: a
	// textually-#included file (e.g. the synthesized textual-source-include
	// lib's `src/os.cc`, or a cc_library's own textual header) that belongs to
	// a deeper/other package becomes a cross-package file label + an
	// exports_files() need; one in this package stays package-relative. (No
	// header-lib "owned" check — textual_hdrs are explicit textual includes,
	// not glob-claimed by a synthesized include-root header lib.) Runs for
	// multi-package RootInclude (rootHdrFastPath) targets too: their textual_hdrs
	// are NOT re-homed into the aggregate, so they need this rewrite to stay
	// loadable after the split.
	if len(t.TextualHdrs) > 0 {
		var keepTextual []string
		for _, h := range t.TextualHdrs {
			if !local {
				// SourceKey regime: left element-root-relative, prefixed
				// @src_<key>//: by the emitter — package-location-independent.
				keepTextual = append(keepTextual, h)
				continue
			}
			if plan.deepestPkg(h) == dir {
				rel, _ := relUnder(dir, h)
				keepTextual = append(keepTextual, rel)
				continue
			}
			dh := plan.deepestPkg(h)
			if file, _ := relUnder(dh, h); file != "" {
				keepTextual = append(keepTextual, crossPkgFileLabel(plan, dh, file))
				if !plan.genOuts[h] {
					if exportsByDir[dh] == nil {
						exportsByDir[dh] = map[string]struct{}{}
					}
					exportsByDir[dh][file] = struct{}{}
				}
			}
		}
		rt.TextualHdrs = keepTextual
	}

	// Re-relativize srcs to the declaring dir in the local regime, routing
	// any entry that belongs to a deeper/other package to a cross-package
	// label (files) or dropping it (a bare packaged directory).
	if local {
		var srcs []string
		for _, s := range t.Srcs {
			// PkgSrcsGlob srcs are glob PATTERNS (e.g.
			// "c/include/brotli/**" from install(DIRECTORY)), not file
			// paths. A glob can't be expressed as a cross-package label
			// (`glob(["//c/include:brotli/**"])` is invalid — patterns
			// must be package-relative, never absolute) and Bazel's glob
			// doesn't cross package boundaries anyway, so once the
			// globbed dir is its own package a root-level glob wouldn't
			// match it. Same situation as the bare-packaged-directory
			// case below: not expressible post-split; drop it — the
			// install is served by the layout-independent install root,
			// and EmitSplit skips the now-empty pkg_files. (The glob
			// pattern itself contains no package boundary we can
			// re-root against, so there's nothing to relativize.)
			if t.PkgSrcsGlob {
				continue
			}
			d := plan.deepestPkg(s)
			if d == dir {
				rel, _ := relUnder(dir, s)
				srcs = append(srcs, rel)
				continue
			}
			file, _ := relUnder(d, s)
			if file == "" {
				// s names a packaged directory itself (e.g. an
				// install(DIRECTORY) filegroup src pointing at a dir that
				// became its own package). Not expressible as a
				// cross-package file label; drop it — the install path is
				// served by the install root, which is layout-independent.
				continue
			}
			// Cross-package source FILE: reference by label + raise an
			// exports_files() need in the owning package (unless it's a
			// generated out — already a target, exports_files() would conflict).
			srcs = append(srcs, crossPkgFileLabel(plan, d, file))
			if !plan.genOuts[s] {
				if exportsByDir[d] == nil {
					exportsByDir[d] = map[string]struct{}{}
				}
				exportsByDir[d][file] = struct{}{}
			}
		}
		rt.Srcs = srcs
	}
	// (SourceKey regime: srcs/hdrs are left element-root-relative;
	// EmitWithOptions prefixes them with @src_<key>//: unchanged.)

	// Genrule outs are element-root-relative on the IR target; once
	// the genrule is placed in its output's package (see EmitSplit's
	// partition loop), re-relativize each out to that package dir so
	// Bazel sees a package-local output (outs = ["x.cpp"], not
	// ["doc/snippets/x.cpp"]). The genrule cmd references outputs via
	// $@ / $(location), which stay correct regardless of the literal
	// out path, so this is purely a label re-rooting. Only the local
	// regime; the SourceKey regime keeps element-root-relative paths.
	if t.Kind == ir.KindGenrule {
		if local && len(t.GenruleOuts) > 0 {
			cmd := rt.GenruleCmd
			outs := make([]string, 0, len(t.GenruleOuts))
			for _, o := range t.GenruleOuts {
				if rel, ok := relUnder(dir, o); ok {
					// The standalone-custom-command cmd references
					// each out as $(RULEDIR)/<out> (the lower-side
					// anchorGenruleOutputsToRuledir); moving the
					// genrule into its output's package shrinks the
					// out, so the cmd's $(RULEDIR)/<old> must shrink
					// in lockstep. $@ / $(location <out>) forms need
					// no rewrite.
					if rel != o {
						cmd = strings.ReplaceAll(cmd, "$(RULEDIR)/"+o, "$(RULEDIR)/"+rel)
						// Shrink the output's parent-dir tokens in lockstep
						// (lower anchors multi-component parents for a
						// make_directory/mkdir of the output's dir). Immediate
						// parent first → longest-first, so a child dir is
						// rewritten before its prefix.
						for d := pathDir(o); strings.Contains(d, "/"); d = pathDir(d) {
							if dRel, ok := relUnder(dir, d); ok && dRel != d {
								cmd = strings.ReplaceAll(cmd, "$(RULEDIR)/"+d, "$(RULEDIR)/"+dRel)
							}
						}
					}
					outs = append(outs, rel)
				} else {
					// Out sits outside this genrule's package (a
					// multi-output genrule spanning packages — rare and
					// not expressible as-is in Bazel). Leave it
					// element-root-relative so the breakage is visible
					// rather than silently mis-rooted.
					outs = append(outs, o)
				}
			}
			rt.GenruleOuts = outs
			rt.GenruleCmd = cmd
		}
		// Rewrite intra-element genrule tool labels (":x") and their
		// matching $(location :x) cmd references to cross-package form —
		// e.g. a tablegen genrule placed in //include whose generator
		// binary (llvm-min-tblgen) lives in //llvm/utils/TableGen.
		// Mirrors the deps rewrite below; runs in both regimes since a
		// tool label is package-location-dependent either way.
		if len(t.GenruleTools) > 0 {
			tools := make([]string, 0, len(t.GenruleTools))
			cmd := rt.GenruleCmd
			for _, tool := range t.GenruleTools {
				if strings.HasPrefix(tool, ":") {
					label := targetLabel(plan, strings.TrimPrefix(tool, ":"))
					tools = append(tools, label)
					cmd = strings.ReplaceAll(cmd, "$(location "+tool+")", "$(location "+label+")")
					continue
				}
				tools = append(tools, tool)
			}
			rt.GenruleTools = tools
			rt.GenruleCmd = cmd
		}
	}

	// write_file (configure_file / file(GENERATE) bake tier) and
	// cmake_configure_file (lift tier) carry a single element-root-
	// relative output path; once the rule is placed in its output's
	// package (EmitSplit's partition loop above), re-relativize the out
	// to that package dir so Bazel sees a package-local generated file
	// (out = "llvm/Config/config.h", not "include/llvm/Config/config.h").
	// CMakeConfigureFile is a pointer into the shared IR target, so copy
	// the spec before mutating to avoid corrupting the source package.
	if local && t.Kind == ir.KindWriteFile && t.WriteFileOut != "" {
		if rel, ok := relUnder(dir, t.WriteFileOut); ok {
			rt.WriteFileOut = rel
		}
	}
	if local && t.Kind == ir.KindCMakeConfigureFile &&
		t.CMakeConfigureFile != nil && t.CMakeConfigureFile.Out != "" {
		if rel, ok := relUnder(dir, t.CMakeConfigureFile.Out); ok {
			specCopy := *t.CMakeConfigureFile
			specCopy.Out = rel
			rt.CMakeConfigureFile = &specCopy
		}
	}

	// Rewrite intra-element deps (":x") to cross-package labels. PRIVATE
	// include-dir header libs (privHeaderDeps) ride implementation_deps so
	// they don't propagate to consumers; PUBLIC ones ride deps.
	rt.Deps = rewriteDeps(t.Deps, plan, headerDeps)
	rt.ImplementationDeps = rewriteDeps(t.ImplementationDeps, plan, privHeaderDeps)
	// Data carries add_dependencies-derived intra-element target edges (":x",
	// build-order only); relabel them to cross-package labels like deps, else a
	// sub-package consumer's `:LLVMAnalysis` resolves to its OWN package
	// (LLVM's lib/Frontend/Atomic referencing lib/Analysis:LLVMAnalysis →
	// "missing input file"). Same target-ref shape as deps, so rewriteDeps is
	// the right mapper.
	rt.Data = rewriteDeps(t.Data, plan, nil)

	// An alias's `actual` is an intra-element target reference too — relabel
	// it to the target's real package the same way deps are. Aliases land in
	// their declaring package (for abseil's absl::* / googletest's GTest::*
	// the helper macro puts them in the root element package), but the target
	// they point at often splits into a subpackage; the bare ":x" then no
	// longer resolves and Bazel reads it as a missing same-package input file.
	if t.Kind == ir.KindAlias && strings.HasPrefix(t.AliasActual, ":") {
		rt.AliasActual = targetLabel(plan, strings.TrimPrefix(t.AliasActual, ":"))
	}

	// Multi-package RootInclude (abseil's element-root grant spanning many
	// packages): planSplit re-homed the whole element-root header surface into
	// per-package header libs behind the aggregate, because include_prefix on the
	// target itself (the glm path below) can't carry headers that re-home into
	// OTHER packages. Drop this target's now-redundant header copy (the hdr-rewrite
	// loop above already skipped relabeling it) and depend on the aggregate — every
	// walked header is re-provided there at its include_prefix-restored `<pkg>/...`
	// path, so the target's own `#include "<pkg>/foo.h"` resolves. textual_hdrs are
	// left as the textual loop rewrote them (not re-homed into the aggregate).
	// (rewriteDeps is idempotent over the already-labeled rt.Deps; it folds in the
	// aggregate label and re-sorts. The single-package shape leaves rootHdrAgg
	// empty and falls through.)
	if rootHdrFastPath {
		rt.Hdrs = nil
		rt.Deps = rewriteDeps(rt.Deps, plan, []string{headerLibLabel(plan, "", plan.rootHdrAgg)})
		return rt
	}

	// A target whose headers are include-rooted at the element root
	// (RootInclude — cmake's target_include_directories(${CMAKE_SOURCE_DIR}),
	// which lower can't emit as includes=[""]) loses that root-relative prefix
	// when it re-homes into a subpackage: `glm/foo.hpp` becomes package-local
	// `foo.hpp`, so `#include <glm/foo.hpp>` no longer resolves. Restore it with
	// include_prefix=<package dir> — Bazel re-prepends the dir to this target's
	// header paths for both its own compilation and its consumers (the
	// self-contained shape gazelle_cc also emits; no parent-package header lib
	// or cross-package up-reference needed).
	//
	// Gated to the LOCAL regime with all-package-local headers: the SourceKey
	// regime keeps hdrs element-root-relative (they already carry the prefix —
	// include_prefix would double it to `glm/glm/foo.hpp`), and a cross-package
	// header label (`//pkg:h`) would be wrongly prefixed too (include_prefix
	// applies uniformly to every hdr). glm's single-package header set in the
	// local regime satisfies both.
	if local && t.RootInclude && dir != "" && rt.IncludePrefix == "" &&
		allPackageLocalHdrs(rt.Hdrs) && allPackageLocalHdrs(rt.TextualHdrs) {
		rt.IncludePrefix = dir
	}
	// A producer whose generated output is consumed cross-package (see
	// plan.publicize) must be public so the output is reachable from the
	// consumer's package — its outputs inherit this visibility.
	if plan.publicize[t.Name] {
		rt.Visibility = []string{"//visibility:public"}
	}
	return rt
}

// allPackageLocalHdrs reports whether every header entry is a package-local
// path (not a cross-package "//…" or external "@…" label). include_prefix
// applies uniformly to all of a target's hdrs, so it's only safe to set when
// none of them already live in (or point at) another package.
func allPackageLocalHdrs(hdrs []string) bool {
	for _, h := range hdrs {
		if strings.HasPrefix(h, "//") || strings.HasPrefix(h, "@") {
			return false
		}
	}
	return true
}

// rewriteDeps maps each intra-element ":x" dep to its cross-package
// label form using the split plan, leaving external ("@repo//…",
// "//…") labels untouched, and appends the supplied header-lib deps.
// The result is sorted + deduped.
func rewriteDeps(deps []string, plan *splitPlan, extra []string) []string {
	if len(deps) == 0 && len(extra) == 0 {
		return nil
	}
	seen := map[string]struct{}{}
	var out []string
	add := func(l string) {
		if _, ok := seen[l]; ok {
			return
		}
		seen[l] = struct{}{}
		out = append(out, l)
	}
	for _, d := range deps {
		if strings.HasPrefix(d, ":") {
			name := strings.TrimPrefix(d, ":")
			add(targetLabel(plan, name))
			continue
		}
		add(d)
	}
	for _, e := range extra {
		add(e)
	}
	sort.Strings(out)
	return out
}

// targetLabel returns the cross-package label for an intra-element
// target name. A name with no SubPackages entry (install-derived /
// synthesized) resolves to the root package.
func targetLabel(plan *splitPlan, name string) string {
	dir := plan.targetDir(name)
	if dir == "" {
		return fmt.Sprintf("//%s:%s", plan.bazelRoot(), name)
	}
	return fmt.Sprintf("//%s:%s", joinPkgPath(plan.bazelRoot(), dir), name)
}

// headerLibLabel returns the label of a synthesized header lib living in
// include-root dir inc.
func headerLibLabel(plan *splitPlan, inc, name string) string {
	if inc == "" {
		return fmt.Sprintf("//%s:%s", plan.bazelRoot(), name)
	}
	return fmt.Sprintf("//%s:%s", joinPkgPath(plan.bazelRoot(), inc), name)
}

// crossPkgFileLabel returns a label for a source file exported via
// exports_files() from the package owning its directory.
func crossPkgFileLabel(plan *splitPlan, sdir, file string) string {
	if sdir == "" {
		return fmt.Sprintf("//%s:%s", plan.bazelRoot(), file)
	}
	return fmt.Sprintf("//%s:%s", joinPkgPath(plan.bazelRoot(), sdir), file)
}

// bazelRoot is the repo-root-relative base package path. The split plan
// stamps it once from EmitSplit's opts so label formation is consistent.
func (p *splitPlan) bazelRoot() string { return p.base }

// base is the repo-root-relative element package path; set by EmitSplit.
// (Stored on the plan so label helpers don't thread it through every
// call.)
func (p *splitPlan) setBase(b string) { p.base = b }

// appendExportsFiles renders an exports_files([...]) block for the
// supplied files and appends it to an already-rendered BUILD body,
// re-canonicalizing so the result stays buildifier-clean.
func appendExportsFiles(body []byte, files map[string]struct{}) ([]byte, error) {
	names := make([]string, 0, len(files))
	for f := range files {
		names = append(names, f)
	}
	sort.Strings(names)
	var b bytes.Buffer
	b.Write(body)
	if len(body) > 0 && !bytes.HasSuffix(body, []byte("\n\n")) {
		b.WriteString("\n")
	}
	b.WriteString("exports_files([")
	for i, n := range names {
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "%q", n)
	}
	b.WriteString("])\n")
	return canonicalize(b.Bytes())
}

// --- small path / name helpers ---

// normDir normalizes a directory string to the slash form used as a
// SubPackages key: "." and "./" become "", trailing slashes dropped.
func normDir(d string) string {
	d = strings.TrimSpace(d)
	d = strings.TrimPrefix(d, "./")
	d = strings.TrimSuffix(d, "/")
	if d == "." {
		return ""
	}
	return d
}

// includeDirFromCopt extracts the directory from an include-search copt
// in the joined form lower emits for PRIVATE include dirs ("-I<dir>" or
// "-isystem<dir>"; see lower.go). It reports (dir, true) only for those
// two flags so the header-lib relabel (rewriteTarget) doesn't misfire on
// other -i* flags (-include, -iquote, -idirafter) or unrelated copts.
func includeDirFromCopt(c string) (string, bool) {
	if d := strings.TrimPrefix(c, "-isystem"); d != c {
		return d, true
	}
	if d := strings.TrimPrefix(c, "-I"); d != c {
		return d, true
	}
	return "", false
}

// relUnder returns (path-relative-to-dir, true) when p is at or below
// dir, ("", false) otherwise. dir "" means the root, where every path is
// already relative.
// pathDir returns the parent directory of a slash-separated path, or "" when
// it has no slash. String-based to match this file's path handling (normDir
// / relUnder) and to avoid path.Dir's "." result for slash-less inputs,
// which the callers' `strings.Contains(d, "/")` loop guard treats as "stop".
func pathDir(p string) string {
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[:i]
	}
	return ""
}

func relUnder(dir, p string) (string, bool) {
	dir = normDir(dir)
	p = strings.TrimPrefix(p, "./")
	if dir == "" {
		return p, true
	}
	if p == dir {
		return "", true
	}
	if strings.HasPrefix(p, dir+"/") {
		return strings.TrimPrefix(p, dir+"/"), true
	}
	return "", false
}

// joinPkgPath joins a repo-root base package path with an element-root-
// relative sub-directory, dropping empty segments.
func joinPkgPath(base, dir string) string {
	base = strings.Trim(base, "/")
	dir = normDir(dir)
	switch {
	case base == "" && dir == "":
		return ""
	case base == "":
		return dir
	case dir == "":
		return base
	default:
		return base + "/" + dir
	}
}

// compiledSourceExts are the extensions Bazel's cc rules treat as COMPILED
// translation units (a file with one in a cc_library's srcs is compiled
// standalone). A GENERATED file with one of these is never a header — it's a
// codegenned source its owning package compiles — so the header-lib synthesis
// (headerLibTarget) drops such a cross-package generated entry instead of
// listing it as a header. Mirrors lower's ccSourceExts plus the asm
// extensions cc_library compiles (.S/.s/.asm).
var compiledSourceExts = map[string]bool{
	".c": true, ".cc": true, ".cpp": true, ".cxx": true, ".c++": true,
	".cu": true, ".cl": true, ".cppm": true, ".ixx": true,
	".s": true, ".asm": true,
	// ".S" matched case-insensitively below (a capital-S asm is preprocessed
	// then assembled — OpenBLAS's kernel/CMakeFiles/<k>.S codegen).
}

// isCompiledSourceExt reports whether path p ends in a compiled-source
// extension (case-insensitive). Used to identify generated translation units
// that don't belong in a synthesized header library.
func isCompiledSourceExt(p string) bool {
	dot := strings.LastIndex(p, ".")
	if dot < 0 {
		return false
	}
	return compiledSourceExts[strings.ToLower(p[dot:])]
}

// headerLibName derives a deterministic sanitized cc_library name from an
// include-root dir: "include" → "include_headers"; "src/api" →
// "src_api_headers"; "" (root) → "root_headers".
func headerLibName(inc string) string {
	inc = normDir(inc)
	if inc == "" {
		return "root_headers"
	}
	s := strings.NewReplacer("/", "_", "-", "_", ".", "_").Replace(inc)
	return s + "_headers"
}

// rootHdrLibName derives a deterministic sanitized cc_library name for a
// per-package root-walk header lib (splitPlan.rootHdrLibs): "absl/base" →
// "absl_base_root_hdrs"; "" (root) → "root_hdrs". The "_root_hdrs" suffix is
// distinct from headerLibName's "_headers" so the two synthesis schemes never
// collide on a name.
func rootHdrLibName(dir string) string {
	dir = normDir(dir)
	if dir == "" {
		return "root_hdrs"
	}
	s := strings.NewReplacer("/", "_", "-", "_", ".", "_").Replace(dir)
	return s + "_root_hdrs"
}

// dirKey renders a sub-package dir for error messages.
func dirKey(d string) string {
	if d == "" {
		return "<root>"
	}
	return d
}

// hasValue reports whether m contains the value v.
func hasValue(m map[string]string, v string) bool {
	for _, x := range m {
		if x == v {
			return true
		}
	}
	return false
}
