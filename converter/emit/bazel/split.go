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
	plan := planSplit(pkg)
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
		groups[inc] = append(groups[inc], plan.headerLibTarget(inc, name))
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

// headerLibTarget builds the synthesized header cc_library for an
// include-root dir.
func (p *splitPlan) headerLibTarget(inc, name string) ir.Target {
	hdrs := make([]string, 0, len(p.headersIn[inc]))
	for _, h := range p.headersIn[inc] {
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
func planSplit(pkg *ir.Package) *splitPlan {
	p := &splitPlan{
		sub:        map[string]string{},
		headerLibs: map[string]string{},
		headersIn:  map[string][]string{},
	}
	for k, v := range pkg.SubPackages {
		p.sub[k] = v
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
	return p
}

// rewriteTarget produces the sub-package-local copy of a real target:
// re-relativize srcs/hdrs to its declaring dir, strip include-roots +
// their headers (now owned by the synthesized header libs), and rewrite
// intra-element deps to cross-package labels.
func rewriteTarget(t ir.Target, dir string, plan *splitPlan, local bool, exportsByDir map[string]map[string]struct{}) ir.Target {
	rt := t

	// Header libs this target must depend on (one per include-root it
	// referenced), plus the residual includes (none, after the split).
	var headerDeps []string
	incRoots := map[string]struct{}{}
	for _, inc := range t.Includes {
		n := normDir(inc)
		if name, ok := plan.headerLibs[n]; ok {
			incRoots[n] = struct{}{}
			headerDeps = append(headerDeps, headerLibLabel(plan, n, name))
		}
	}
	rt.Includes = nil

	// Drop headers now owned by a synthesized header lib; keep the rest
	// (physically under this target's dir) package-relative.
	var keepHdrs []string
	for _, h := range t.Hdrs {
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
		// Cross-package header with no owning header lib isn't expressible
		// as a local hdr (listing it would cross a package boundary and
		// fail to load); drop it. Rare — a discoverHeaders artifact whose
		// dir became its own package without being an include root. The
		// header still reaches consumers via the owning package's own rule
		// and the layout-independent install path.
	}
	rt.Hdrs = keepHdrs

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
			// exports_files() need in the owning package.
			srcs = append(srcs, crossPkgFileLabel(plan, d, file))
			if exportsByDir[d] == nil {
				exportsByDir[d] = map[string]struct{}{}
			}
			exportsByDir[d][file] = struct{}{}
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

	// Rewrite intra-element deps (":x") to cross-package labels.
	rt.Deps = rewriteDeps(t.Deps, plan, headerDeps)
	rt.ImplementationDeps = rewriteDeps(t.ImplementationDeps, plan, nil)
	return rt
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
