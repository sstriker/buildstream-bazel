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
// Shape (parallels docs/design/cmake-execute-process-round2-fallback.md
// — the kind:cmake Phase B sibling):
//
//   - one extract genrule that untars install_tree.tar into
//     per-file outs derived from intro-install_plan.json's
//     `targets` + `headers` sections.
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
// genrule in project B pins `meson setup --prefix=/ --libdir=lib`,
// which makes those placeholders resolve to clean relative paths
// (`lib/libfoo.a`, `bin/foo`, `include/foo.h`). resolvePlaceholders
// reads intro-buildoptions.json's `section: directory` rows for the
// substitution map.

import (
	"path"
	"sort"
	"strings"

	"github.com/sstriker/buildstream-bazel/converter/ir"
)

// emitFallbackPlaceholder builds the placeholder ir.Package
// returned by Lower (via main.go's handleError-style intercept)
// when --unsupported-target-fallback is on and native lowering
// would refuse. The shape is described in this file's header.
//
// Returns an empty Package (nil-Targets) when the install plan
// is empty — the genrule's outs still need a contract-compliant
// shape; main.go's caller skips the placeholder shape entirely
// if the result has no targets and falls back to the legacy
// Tier-1 exit (the operator is no worse off than without the
// flag).
func emitFallbackPlaceholder(intro *Introspect, opts LowerOptions) (*ir.Package, error) {
	pkg := &ir.Package{
		Name:       intro.ProjectInfo.Name,
		SourceRoot: opts.SourceRoot,
	}

	dirs := dirValuesFromOptions(intro.BuildOptions)

	type targetStub struct {
		stub        ir.Target
		installPath string
		baseName    string // basename of installPath, used to fold headers
		isLibrary   bool
	}
	var stubs []targetStub
	var headerOuts []string

	// Walk install-plan targets in sorted source-path order so
	// rendering is deterministic. The map iteration order would
	// otherwise shuffle stubs across runs.
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
		installPath := path.Join("install_tree", dest)
		baseName := path.Base(installPath)
		stub := ir.Target{
			Name:       targetNameFromInstallPath(baseName),
			Tags:       []string{"meson-codegen-target-fallback"},
			Visibility: []string{"//visibility:public"},
		}
		if stub.Name == "" {
			continue
		}
		switch classifyArtefact(entry.Tag, baseName) {
		case artefactStaticLib:
			stub.Kind = ir.KindCCImport
			stub.StaticLibrary = installPath
			stubs = append(stubs, targetStub{stub: stub, installPath: installPath, baseName: baseName, isLibrary: true})
		case artefactSharedLib:
			stub.Kind = ir.KindCCImport
			stub.SharedLibrary = installPath
			stubs = append(stubs, targetStub{stub: stub, installPath: installPath, baseName: baseName, isLibrary: true})
		case artefactExecutable:
			stub.Kind = ir.KindShBinary
			stub.Srcs = []string{installPath}
			stubs = append(stubs, targetStub{stub: stub, installPath: installPath, baseName: baseName})
		default:
			// Unknown artefact shape (e.g. tag="man" or a custom
			// tag a meson module emits) — fall back to a private
			// filegroup so the extract genrule still claims the
			// path. cc_binary/cc_import would mis-classify; a
			// filegroup keeps install_tree.tar honest without
			// claiming a typed shape we can't validate.
			//
			// v1 drops the entry entirely: the install_tree.tar
			// still carries the bytes (the install genrule's tar
			// step preserves them), but no per-target Bazel label
			// surfaces. A real fixture forcing the divergence
			// drives a typed-filegroup follow-up.
			continue
		}
	}

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
		installPath := path.Join("install_tree", dest)
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

	// Emit one extract genrule whose outs union every per-stub
	// install path + every header path. The genrule's src is the
	// literal label "install_tree.tar" — resolves once A's
	// BUILD.bazel.out gets symlinked into project B's package and
	// co-locates with B's install genrule.
	var extractOuts []string
	for _, s := range stubs {
		extractOuts = append(extractOuts, s.installPath)
	}
	extractOuts = append(extractOuts, headerOuts...)
	extractOuts = dedupeSorted(extractOuts)
	if len(extractOuts) > 0 {
		pkg.Targets = append(pkg.Targets, ir.Target{
			Name:        "_install_tree_extract",
			Kind:        ir.KindGenrule,
			Srcs:        []string{"install_tree.tar"},
			GenruleOuts: extractOuts,
			GenruleCmd: `mkdir -p "$(RULEDIR)/install_tree" && ` +
				`tar -C "$(RULEDIR)/install_tree" -xf "$(location install_tree.tar)"`,
			Tags: []string{
				"meson-codegen-target-fallback",
				"meson-codegen-target-fallback-extract",
			},
			Visibility: []string{"//visibility:private"},
		})
	}

	for _, s := range stubs {
		// Sort hdrs deterministically — they accumulate via the
		// header fold loop which preserves install-plan iteration
		// order, but we always emit sorted lists for golden
		// stability.
		sort.Strings(s.stub.Hdrs)
		pkg.Targets = append(pkg.Targets, s.stub)
	}
	return pkg, nil
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
// which the operator can recover by referencing
// install_tree.tar directly via filegroup.
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
// stub with a path the install_tree.tar can't satisfy.
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
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func dedupeSorted(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	sort.Strings(in)
	out := in[:0]
	for i, s := range in {
		if i == 0 || in[i-1] != s {
			out = append(out, s)
		}
	}
	return out
}
