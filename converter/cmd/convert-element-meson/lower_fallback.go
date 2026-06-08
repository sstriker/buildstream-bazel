package main

// kind:meson Phase B fallback emitter.
//
// When --unsupported-target-fallback is set on convert-element-meson
// and the native lowering path refuses (typed Tier-1 like
// unsupported-meson-subproject / unsupported-meson-custom-target /
// unsupported-meson-generated-sources / unsupported-meson-cross-
// compile / unresolved-meson-dependency / unsupported-meson-target-
// type), main.go calls emitFallbackPlaceholder to produce an
// install-plan-driven placeholder ir.Package instead of exiting
// Tier-1.
//
// Shape (parallels docs/design/rendezvous.md
// — the kind:cmake Phase B sibling):
//
//   - one pick_file projection per file over the install-root
//     TreeArtifact, deriving each projected path from intro-
//     install_plan.json's `targets` + `headers` sections.
//   - per-target stubs dispatched on (Tag, install-path basename):
//     * tag=devel + libfoo.a       → cc_import + static_library
//     * tag=runtime + libfoo.so*   → cc_import + shared_library
//     * tag=devel + libfoo.so*     → cc_import + shared_library
//     * tag=runtime, no lib prefix → sh_binary + srcs (executable)
//     * tag=devel + headers        → folded into the matching
//       library's hdrs when one matches by base name, otherwise
//       a header-only cc_library is emitted.
//
// The "richer signal" the design doc mentions (vs cmake's
// destination-path inference) lives here: meson's `tag` field
// distinguishes runtime artefacts from devel artefacts directly,
// so we don't have to guess from the destination directory. cmake's
// fallback infers from Target.Install.Destinations[0].Path; meson's
// fallback reads Tag straight off the install-plan row.
//
// Path resolution: meson's destinations embed `{libdir_static}`,
// `{libdir_shared}`, `{bindir}`, `{includedir}` etc. The install
// rule in project B pins `meson setup --prefix=/ --libdir=lib`,
// which makes those placeholders resolve to clean relative paths
// (`lib/libfoo.a`, `bin/foo`, `include/foo.h`). resolvePlaceholders
// reads intro-buildoptions.json's `section: directory` rows for the
// substitution map.

import (
	"path"
	"sort"
	"strings"

	"github.com/sstriker/buildstream-bazel/converter/ir"
	"github.com/sstriker/buildstream-bazel/internal/sliceutil"
)

// targetStub is one accumulated install-plan stub before it
// reaches the package's target list. installPath / baseName feed
// the extract genrule's outs and the SONAME-stripped target name;
// isLibrary gates header-fold attribution; kind retains the
// classifier verdict for the name-collision disambiguation pass
// that runs after the classification loop.
type targetStub struct {
	stub        ir.Target
	installPath string
	baseName    string
	isLibrary   bool
	kind        artefactKind
}

// emitFallbackPlaceholder builds the placeholder ir.Package
// returned by Lower (via main.go's handleError-style intercept)
// when --unsupported-target-fallback is on and native lowering
// would refuse. The shape is described in this file's header.
//
// Returns a package with nil Targets when the install plan is
// empty or every plan row filters out (all subprojects, all
// destinations with unresolved placeholders, all tag/basename
// combinations land in artefactUnknown). main.go's caller
// inspects len(pkg.Targets) post-call and propagates the
// original Tier-1 in that case — an empty BUILD on disk would
// be more confusing operator-side than a typed refusal.
func emitFallbackPlaceholder(intro *Introspect, opts LowerOptions) (*ir.Package, error) {
	pkg := &ir.Package{
		Name:       intro.ProjectInfo.Name,
		SourceRoot: opts.SourceRoot,
	}

	dirs := dirValuesFromOptions(intro.BuildOptions)

	var stubs []targetStub
	var headerOuts []string

	// Walk install-plan targets in sorted source-path order so
	// rendering is deterministic. The map iteration order would
	// otherwise shuffle stubs across runs.
	//
	// Two passes through the same loop, by name-collision arbitration:
	// first build the raw stubs, then disambiguate any (name, kind)
	// collisions before they reach the package's target list. Meson's
	// `both_libraries('foo', ...)` is the canonical collision source —
	// it installs both libfoo.a (artefactStaticLib → name "foo") and
	// libfoo.so (artefactSharedLib → name "foo"), and emitting both
	// stubs as-is would yield two `cc_import(name = "foo", ...)`
	// declarations in the same BUILD, which Bazel rejects at load
	// time with "Target 'foo' already declared".
	for _, srcPath := range sortedKeys(intro.InstallPlan.Targets) {
		entry := intro.InstallPlan.Targets[srcPath]
		if entry.Subproject != nil && *entry.Subproject != "" {
			// Subproject artefacts aren't part of the consumer-
			// visible install contract; emitting stubs for them
			// would surface labels that resolve to bytes coming
			// from an upstream project. Native lowering refuses
			// subprojects entirely (unsupported-meson-subproject);
			// the fallback keeps that filter.
			continue
		}
		dest := resolvePlaceholders(entry.Destination, dirs)
		if dest == "" {
			continue
		}
		// Tree-relative path inside the install-root TreeArtifact
		// (e.g. "lib/libfoo.a"). pick_file projects it out in place;
		// no "install_tree/" prefix — that named the old extract
		// genrule's untar subdirectory, which the TreeArtifact shape
		// removes.
		installPath := path.Clean(dest)
		baseName := path.Base(installPath)
		stub := ir.Target{
			Name:       targetNameFromInstallPath(baseName),
			Tags:       []string{"meson-codegen-target-fallback"},
			Visibility: []string{"//visibility:public"},
		}
		if stub.Name == "" {
			continue
		}
		kind := classifyArtefact(entry.Tag, baseName)
		switch kind {
		case artefactStaticLib:
			stub.Kind = ir.KindCCImport
			stub.StaticLibrary = installPath
			stubs = append(stubs, targetStub{stub: stub, installPath: installPath, baseName: baseName, isLibrary: true, kind: kind})
		case artefactSharedLib:
			stub.Kind = ir.KindCCImport
			stub.SharedLibrary = installPath
			stubs = append(stubs, targetStub{stub: stub, installPath: installPath, baseName: baseName, isLibrary: true, kind: kind})
		case artefactExecutable:
			stub.Kind = ir.KindShBinary
			stub.Srcs = []string{installPath}
			stubs = append(stubs, targetStub{stub: stub, installPath: installPath, baseName: baseName, kind: kind})
		default:
			// Unknown artefact shape (e.g. tag="man" or a custom
			// tag a meson module emits) — fall back to a private
			// filegroup so a pick_file projection still claims the
			// path. cc_binary/cc_import would mis-classify; a
			// filegroup keeps the install root honest without
			// claiming a typed shape we can't validate.
			//
			// v1 drops the entry entirely: the install-root
			// TreeArtifact still carries the bytes (the install
			// rule installs them in place), but no per-target Bazel
			// label surfaces. A real fixture forcing the divergence
			// drives a typed-filegroup follow-up.
			continue
		}
	}

	// Disambiguate same-name stubs. `both_libraries('foo')` produces
	// `foo` for libfoo.a and `foo` for libfoo.so; suffix the static
	// one with `_static` and the shared one with `_shared` so each
	// gets a unique Bazel label. Idempotent: stubs whose name only
	// appears once are left untouched. Done after the classification
	// loop (rather than during) so the suffix decision sees the full
	// collision picture — emitting `_static` proactively when only
	// the static lib exists would break the common single-library
	// case.
	disambiguateLibraryCollisions(stubs)

	// Headers: derive install-tree paths and fold into the matching
	// library's `hdrs` when basename-resolved against a library's
	// SONAME-style prefix. Most projects ship `libfoo.a` +
	// `<foo.h, foo_extras.h>` under {includedir}; tying the
	// headers to `libfoo`'s cc_import lets downstream consumers
	// `#include "foo.h"` against the placeholder.
	for _, srcPath := range sortedKeys(intro.InstallPlan.Headers) {
		entry := intro.InstallPlan.Headers[srcPath]
		if entry.Subproject != nil && *entry.Subproject != "" {
			continue
		}
		dest := resolvePlaceholders(entry.Destination, dirs)
		if dest == "" {
			continue
		}
		// Tree-relative header path inside the install-root
		// TreeArtifact (e.g. "include/foo.h"). pick_file projects it
		// out in place; no "install_tree/" prefix.
		installPath := path.Clean(dest)
		headerOuts = append(headerOuts, installPath)
		// Fold the header into a library stub if we can find one
		// whose name plausibly matches the project. v1 attaches
		// every header to every library — a coarse but safe choice
		// that mirrors cmake's behaviour: downstream `#include
		// <foo.h>` resolves against any library that ships it.
		// More precise per-library folding requires meson to
		// expose which target a header belongs to, which the
		// introspection schema doesn't carry today.
		for i := range stubs {
			if stubs[i].isLibrary {
				stubs[i].stub.Hdrs = append(stubs[i].stub.Hdrs, installPath)
			}
		}
	}

	// Emit one pick_file per unique install path the stubs
	// reference (artefacts + headers), projecting it out of the
	// install-root TreeArtifact (installTarget) in place. Replaces
	// the old _install_tree_extract tar-untar genrule: no
	// per-consumer re-materialization of the whole tree, file-
	// granular CAS dedup. installTarget defaults to the same-package
	// pipeline_install target write-a renders for the round-2
	// fallback.
	installTarget := opts.FallbackInstallTarget
	if installTarget == "" {
		installTarget = ":_trace_build"
	}
	var pickPaths []string
	for _, s := range stubs {
		pickPaths = append(pickPaths, s.installPath)
	}
	pickPaths = append(pickPaths, headerOuts...)
	pickPaths = dedupeSorted(pickPaths)
	pickLabel := map[string]string{}
	for _, p := range pickPaths {
		name := mesonPickFileName(p)
		pickLabel[p] = ":" + name
		pkg.Targets = append(pkg.Targets, ir.Target{
			Name:     name,
			Kind:     ir.KindPickFile,
			PickSrc:  installTarget,
			PickPath: p,
			Tags: []string{
				"meson-codegen-target-fallback",
				"meson-codegen-target-fallback-extract",
			},
			Visibility: []string{"//visibility:private"},
		})
	}
	resolve := func(p string) string {
		if l, ok := pickLabel[p]; ok {
			return l
		}
		return p
	}

	for _, s := range stubs {
		t := s.stub
		switch t.Kind {
		case ir.KindCCImport:
			if t.StaticLibrary != "" {
				t.StaticLibrary = resolve(t.StaticLibrary)
			}
			if t.SharedLibrary != "" {
				t.SharedLibrary = resolve(t.SharedLibrary)
			}
		case ir.KindShBinary:
			for i, src := range t.Srcs {
				t.Srcs[i] = resolve(src)
			}
		}
		// Sort hdrs deterministically — they accumulate via the
		// header fold loop which preserves install-plan iteration
		// order, but we always emit sorted lists for golden
		// stability. Rewrite each to its pick_file label.
		sort.Strings(t.Hdrs)
		if len(t.Hdrs) > 0 {
			hdrs := make([]string, len(t.Hdrs))
			for i, h := range t.Hdrs {
				hdrs[i] = resolve(h)
			}
			t.Hdrs = hdrs
		}
		pkg.Targets = append(pkg.Targets, t)
	}
	return pkg, nil
}

// mesonPickFileName derives a deterministic, Bazel-legal target
// name for the pick_file that projects a tree-relative install
// path. Mirrors the cmake fallback's pickFileName: the "_pick_"
// prefix namespaces these private stub targets and non-identifier
// bytes collapse to '_' (e.g. "lib/libfoo.a" -> "_pick_lib_libfoo_a").
func mesonPickFileName(p string) string {
	var b strings.Builder
	b.WriteString("_pick_")
	for i := 0; i < len(p); i++ {
		c := p[i]
		switch {
		case (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_':
			b.WriteByte(c)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

// disambiguateLibraryCollisions renames stubs whose `stub.Name`
// would collide post-classification — typically a meson
// `both_libraries('foo', ...)` declaration that installs libfoo.a
// (artefactStaticLib) and libfoo.so (artefactSharedLib) under the
// same SONAME-stripped base name "foo". Without disambiguation,
// the emitted package contains two `cc_import(name = "foo", ...)`
// declarations, which Bazel rejects at load time.
//
// Strategy: on a (static, shared) collision, suffix the static
// stub with "_static" and the shared stub with "_shared". Mirrors
// the operator-paperwork advice ("use distinct target names in
// meson.build") but applies it deterministically in the emitter
// since the fallback exists for elements the operator can't
// modify upstream. Single-library stubs are left untouched —
// projects that ship just `libfoo.a` still get `:foo` rather
// than `:foo_static`, preserving the common-case shape.
//
// Other collisions (two static libs with the same name, an
// executable colliding with a library) are theoretically possible
// but signal an upstream-meson bug or an esoteric configuration
// that v1 doesn't model. The disambiguator leaves those alone;
// the emit-time duplicate-name will surface as a clear Bazel
// loading error pointing at the offending labels.
//
// Mutates the stubs slice in place via index iteration.
func disambiguateLibraryCollisions(stubs []targetStub) {
	// Map each name → indices of stubs claiming it. Pairs of
	// (static, shared) at the same name are the disambiguation
	// case; other multiplicities fall through.
	indicesByName := map[string][]int{}
	for i := range stubs {
		indicesByName[stubs[i].stub.Name] = append(indicesByName[stubs[i].stub.Name], i)
	}
	for _, idxs := range indicesByName {
		if len(idxs) != 2 {
			continue
		}
		a, b := &stubs[idxs[0]], &stubs[idxs[1]]
		switch {
		case a.kind == artefactStaticLib && b.kind == artefactSharedLib:
			a.stub.Name += "_static"
			b.stub.Name += "_shared"
		case a.kind == artefactSharedLib && b.kind == artefactStaticLib:
			a.stub.Name += "_shared"
			b.stub.Name += "_static"
		}
	}
}

type artefactKind int

const (
	artefactUnknown artefactKind = iota
	artefactStaticLib
	artefactSharedLib
	artefactExecutable
)

// classifyArtefact maps (tag, basename) onto one of the four
// artefact buckets the placeholder shape models. Dispatch order:
//
//  1. Static archive: basename matches `lib<name>.a` (independent
//     of tag — both `devel` and `runtime` static libs ship under
//     {libdir_static}).
//  2. Shared library: basename matches `lib<name>.so` or
//     SONAME-versioned forms (`lib<name>.so.<ver>`,
//     `lib<name>.<ver>.dylib`, `lib<name>-<ver>.so`). Both tag
//     values land here — meson tags `shared_library()` as
//     `runtime` for the unversioned symlink but `devel` for the
//     versioned soname, and we want to accept both.
//  3. Executable: tag=runtime, basename has no `lib` prefix and
//     no library extension.
//
// Anything else (man pages, data files, locales) falls into
// artefactUnknown, which the caller skips. The bias is toward
// "emit a stub when we're confident" — false positives produce
// resolution-time errors at consumer build time (clearer than
// silently dropping); false negatives just drop the label,
// which the operator can recover by referencing the
// install root directly via filegroup.
func classifyArtefact(tag, basename string) artefactKind {
	if hasLibPrefix(basename) {
		if strings.HasSuffix(basename, ".a") {
			return artefactStaticLib
		}
		if isSharedLibBasename(basename) {
			return artefactSharedLib
		}
	}
	if tag == "runtime" && !hasLibPrefix(basename) {
		return artefactExecutable
	}
	return artefactUnknown
}

func hasLibPrefix(name string) bool {
	return strings.HasPrefix(name, "lib")
}

// isSharedLibBasename matches the common shared-library basename
// shapes meson emits across Linux / macOS / mingw:
//   - libfoo.so, libfoo.so.1, libfoo.so.1.2.3 (Linux ELF)
//   - libfoo.dylib, libfoo.1.dylib (macOS)
//   - libfoo.dll (mingw — rare in FDSDK; cheap to include)
//
// The check looks for either a `.so` token followed by either
// end-of-string or a `.` (catching `.so` + `.so.1` + `.so.1.2`),
// or `.dylib` / `.dll` as a strict suffix. Static archives
// (`.a`) are excluded — the static-archive branch fires first
// in classifyArtefact.
func isSharedLibBasename(name string) bool {
	if strings.HasSuffix(name, ".dylib") || strings.HasSuffix(name, ".dll") {
		return true
	}
	if strings.HasSuffix(name, ".so") {
		return true
	}
	if idx := strings.Index(name, ".so."); idx > 0 {
		return true
	}
	return false
}

// targetNameFromInstallPath derives the Bazel target name from
// the install-tree basename. Strips the `lib` prefix and any
// known library suffix so `libfoo.a` → `foo`, `libfoo.so.1` →
// `foo`, `libfoo.dylib` → `foo`. Executables pass through
// unchanged (`greet-bin` → `greet-bin`). Returns empty when the
// basename has no usable name component (path ended with a
// separator) — caller skips emitting a stub.
func targetNameFromInstallPath(basename string) string {
	if basename == "" {
		return ""
	}
	name := basename
	if hasLibPrefix(name) {
		// Try the static-archive form first since `.a` is
		// unambiguous; then SONAME-versioned `.so.*` (the
		// strip-from-first-`.so` approach handles `libfoo.so`,
		// `libfoo.so.1`, `libfoo.so.1.2.3` uniformly); then
		// macOS `.dylib`; then mingw `.dll`.
		stripped := strings.TrimPrefix(name, "lib")
		switch {
		case strings.HasSuffix(stripped, ".a"):
			return strings.TrimSuffix(stripped, ".a")
		case strings.HasSuffix(stripped, ".dylib"):
			return strings.TrimSuffix(stripped, ".dylib")
		case strings.HasSuffix(stripped, ".dll"):
			return strings.TrimSuffix(stripped, ".dll")
		case strings.HasSuffix(stripped, ".so"):
			return strings.TrimSuffix(stripped, ".so")
		}
		if idx := strings.Index(stripped, ".so."); idx > 0 {
			return stripped[:idx]
		}
		// Fall through: lib-prefixed but no recognized library
		// suffix (e.g. libtool wrapper script). Strip just the
		// prefix and return the rest.
		return stripped
	}
	return name
}

// dirValuesFromOptions extracts the install-directory variables
// from intro-buildoptions.json's flat row list. Only `section:
// directory` rows participate; the resulting map keys are the
// option names (bindir / libdir / includedir / ...) and values
// are the option values (typically empty for prefix-relative
// dirs when the operator pins --prefix=/). The fallback emitter
// reads this map via resolvePlaceholders to substitute meson's
// install-plan placeholders ({bindir}, {libdir_static}, ...).
//
// `libdir_static` and `libdir_shared` aren't first-class options
// in intro-buildoptions; they default to whatever `libdir` is
// set to. v1 maps both placeholders to `libdir` directly.
func dirValuesFromOptions(opts []BuildOption) map[string]string {
	m := map[string]string{}
	for _, o := range opts {
		if o.Section != "directory" {
			continue
		}
		s, ok := o.Value.(string)
		if !ok {
			continue
		}
		m[o.Name] = s
	}
	// Meson's intro-install_plan emits {libdir_static} and
	// {libdir_shared} as distinct placeholders even though both
	// default to the libdir option. Mirror that here so the
	// placeholder substitution lookup hits.
	if lib, ok := m["libdir"]; ok {
		if _, present := m["libdir_static"]; !present {
			m["libdir_static"] = lib
		}
		if _, present := m["libdir_shared"]; !present {
			m["libdir_shared"] = lib
		}
	}
	return m
}

// resolvePlaceholders substitutes meson's `{name}` placeholders
// in an install-plan destination with the values from `dirs`.
// Unknown placeholders are left literal (the caller surfaces
// this as an unresolvable destination). The substitution loop
// passes once over `s` so a value that itself happens to
// contain `{...}` won't trigger a second pass — meson's
// directory options never do today, so the simpler loop is
// fine.
//
// Returns "" when the resolved string is empty or still
// contains `{` (some placeholder didn't resolve). An empty /
// unresolved destination prevents the caller from emitting a
// stub with a path the install root can't satisfy.
func resolvePlaceholders(s string, dirs map[string]string) string {
	if s == "" {
		return ""
	}
	var b strings.Builder
	i := 0
	for i < len(s) {
		j := strings.IndexByte(s[i:], '{')
		if j < 0 {
			b.WriteString(s[i:])
			break
		}
		b.WriteString(s[i : i+j])
		end := strings.IndexByte(s[i+j+1:], '}')
		if end < 0 {
			// Unbalanced brace — treat as literal.
			b.WriteString(s[i+j:])
			break
		}
		name := s[i+j+1 : i+j+1+end]
		val, ok := dirs[name]
		if !ok {
			// Unknown placeholder; surface as ""-via-the-{-check
			// below so the caller skips this entry rather than
			// emitting a stub with an unresolved literal.
			b.WriteString("{" + name + "}")
		} else {
			b.WriteString(val)
		}
		i = i + j + 1 + end + 1
	}
	out := b.String()
	if strings.Contains(out, "{") {
		return ""
	}
	// Strip a leading "/" so the resulting destination is
	// install_tree-relative. Meson's prefix=/ run yields
	// destinations whose first byte is "/" only on placeholders
	// that resolved to an absolute path (an unusual case); we
	// keep the result root-relative so install_tree/<dest>
	// joins cleanly.
	return strings.TrimPrefix(out, "/")
}

func sortedKeys(m map[string]InstallPlanEntry) []string {
	out := sliceutil.SortedKeys(m)
	return out
}

func dedupeSorted(in []string) []string {
	return sliceutil.SortedUnique(in)
}
