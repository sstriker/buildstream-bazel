package lower

import (
	"path/filepath"
	"strings"

	"github.com/sstriker/buildstream-bazel/converter/ir"
)

// shapeHeaderOnlyStripIncludePrefix is a final-emission idiom-shaping pass
// (Phase 7 — Bazel-idiom shaping audit) that lifts a header-only library's
// single include directory from the broad `includes = ["<d>"]` form to the
// precise `strip_include_prefix = "<d>"` form.
//
// A header-only cmake library (`add_library(<t> INTERFACE)` with
// `target_sources(... FILE_SET HEADERS BASE_DIRS <d> ...)`) exposes its
// declared headers to consumers at <d>-relative include paths
// (`#include <foo/bar.h>`). The lowering routes <d> to `includes = ["<d>"]`
// — a transitive `-I<d>` that ALSO exposes every other file under <d>, not
// just the declared headers. `strip_include_prefix = "<d>"` re-roots the
// declared `hdrs` into a virtual include root instead: only the declared
// headers are visible (matching the FILE_SET HEADERS contract) and
// `#include <foo/bar.h>` resolution is unchanged for them.
//
// Operating on the fully-lowered package (rather than inside lowerTarget)
// catches header-only libs from BOTH lowering paths — the codemodel path
// (lowerTarget's KindCCInterface FileSets branch) and the trace-synth path
// (lowerInterfaceLibraries), which is where a consumed INTERFACE_LIBRARY
// actually lands in practice. The shaping needs no FileSet metadata: a
// header-only target's `includes` already IS the FILE_SET base dir.
//
// Conservative — a target qualifies only when:
//   - it's a genuine header-only interface library without an existing
//     strip_include_prefix: either KindCCInterface (the codemodel
//     INTERFACE_LIBRARY path) or a KindCCLibrary tagged
//     `cmake-codegen-interface-library-from-trace` (the trace-synth path,
//     lowerInterfaceLibraries, emits cc_library + hdrs-only with that tag).
//     A plain KindCCLibrary is excluded even when it happens to have no
//     srcs — e.g. a compiled library whose generated sources were elided
//     (cmake-elided-build-dir-source) carries copts/linkstatic and its
//     `includes` are compile-time `-I` roots, not a header-export prefix,
//     so re-rooting them would be wrong;
//   - it has at least one header and EXACTLY ONE include directory (a single
//     strip_include_prefix can't cover multiple include roots, so multi-dir
//     libs keep the includes form); and
//   - that directory is a usable relative prefix (not empty, not ".", no
//     ".." segment) under which EVERY header lives — otherwise the prefix
//     wouldn't cover the header set.
//
// On success it sets StripIncludePrefix and clears Includes (the broad `-I`
// is subsumed by the virtual include root).
func shapeHeaderOnlyStripIncludePrefix(pkg *ir.Package) {
	if pkg == nil {
		return
	}
	for i := range pkg.Targets {
		t := &pkg.Targets[i]
		if !isHeaderOnlyInterfaceLib(t) || t.StripIncludePrefix != "" {
			continue
		}
		if len(t.Hdrs) == 0 || len(t.Includes) != 1 {
			continue
		}
		// Normalize the include dir before matching: trace-derived dirs
		// can carry a leading "./", a trailing "/", or surrounding
		// whitespace that filepath.Clean folds away (Bazel treats
		// "include" and "include/" equivalently). Compare headers in the
		// same normalized space so a harmless variant doesn't block the
		// lift, and store the cleaned form on the attribute.
		d := filepath.Clean(strings.TrimSpace(t.Includes[0]))
		if d == "" || d == "." || pathHasDotDotSegment(d) {
			continue
		}
		prefix := d + "/"
		covered := true
		for _, h := range t.Hdrs {
			if !strings.HasPrefix(filepath.Clean(strings.TrimSpace(h)), prefix) {
				covered = false
				break
			}
		}
		if !covered {
			continue
		}
		t.StripIncludePrefix = d
		t.Includes = nil
	}
}

// isHeaderOnlyInterfaceLib reports whether t is a genuine header-only
// interface library — the codemodel INTERFACE_LIBRARY shape (KindCCInterface)
// or the trace-synthesized one (KindCCLibrary tagged
// cmake-codegen-interface-library-from-trace, emitted by
// lowerInterfaceLibraries). It deliberately excludes a plain KindCCLibrary
// even when srcs are empty (e.g. a compiled lib whose generated sources were
// elided), since those carry compile-time includes that must not be re-rooted.
func isHeaderOnlyInterfaceLib(t *ir.Target) bool {
	if t.Kind == ir.KindCCInterface {
		return true
	}
	if t.Kind == ir.KindCCLibrary {
		for _, tag := range t.Tags {
			if tag == "cmake-codegen-interface-library-from-trace" {
				return true
			}
		}
	}
	return false
}
