package lower

import (
	"path/filepath"
	"strings"

	"github.com/sstriker/buildstream-bazel/converter/internal/fileapi"
	"github.com/sstriker/buildstream-bazel/converter/ir"
)

// liftCompiledLibFileSetStripIncludePrefix is Phase 7 slice 2: the
// compiled-library counterpart to shapeHeaderOnlyStripIncludePrefix. A
// modern-cmake library that compiles sources AND exports public headers via
// `target_sources(<t> PUBLIC FILE_SET HEADERS BASE_DIRS <d> ...)` currently
// emits `includes = ["<d>"]` (the FILE_SET base dir reaches the codemodel
// CompileGroups), a broad `-I<d>`. This lifts that public-header export dir
// to `strip_include_prefix = "<d>"` — the precise form — while leaving any
// OTHER (genuine compile-time) includes intact.
//
// Unlike the header-only IR pass, this runs inside lowerTarget because it
// MUST key on FileSet metadata: a compiled lib's `includes` come from
// CompileGroups and can be arbitrary `-I` roots, so it only lifts the include
// dir that is demonstrably a FILE_SET HEADERS base dir (not a guess from
// includes+hdrs alone).
//
// Conservative — lifts only when:
//   - the target is a compiled cc_library (KindCCLibrary with srcs) without
//     an existing strip_include_prefix;
//   - its HEADERS FileSets resolve to EXACTLY ONE base dir (relativized,
//     slash-normalized, not the package root / outside the tree / with ".."),
//     which is ALSO present in `includes` (the broad `-I` being replaced); and
//   - EVERY header is under that dir (strip_include_prefix must cover the
//     whole hdrs set; a mixed set keeps the includes form).
//
// On success it sets StripIncludePrefix and drops that one dir from Includes,
// keeping the rest. The lib's own sources still resolve their public
// `#include <d-relative>` via the virtual include root strip_include_prefix
// establishes (validated by the meta-cmake-fileset-compiled-lib build gate).
func liftCompiledLibFileSetStripIncludePrefix(irt *ir.Target, t *fileapi.Target, cmakeSrc string) {
	if irt == nil || t == nil {
		return
	}
	if irt.Kind != ir.KindCCLibrary || len(irt.Srcs) == 0 || irt.StripIncludePrefix != "" {
		return
	}
	if len(irt.Hdrs) == 0 || len(irt.Includes) == 0 {
		return
	}

	// The single FILE_SET HEADERS base dir, relativized + slash-normalized.
	dirs := map[string]bool{}
	for _, fs := range t.FileSets {
		if fs.Type != "HEADERS" {
			continue
		}
		for _, bd := range fs.BaseDirectories {
			rel := bd
			if filepath.IsAbs(rel) {
				r, inside := relativeIfInside(cmakeSrc, rel)
				if !inside {
					return
				}
				rel = r
			}
			rel = filepath.ToSlash(filepath.Clean(strings.TrimSpace(rel)))
			if rel == "" || rel == "." || pathHasDotDotSegment(rel) {
				return
			}
			dirs[rel] = true
		}
	}
	if len(dirs) != 1 {
		return
	}
	var d string
	for k := range dirs {
		d = k
	}

	// d must be the broad include we're replacing.
	inIncludes := false
	for _, inc := range irt.Includes {
		if filepath.ToSlash(filepath.Clean(inc)) == d {
			inIncludes = true
			break
		}
	}
	if !inIncludes {
		return
	}

	// Every header must live under d for strip_include_prefix to cover it.
	prefix := d + "/"
	for _, h := range irt.Hdrs {
		if !strings.HasPrefix(filepath.ToSlash(filepath.Clean(strings.TrimSpace(h))), prefix) {
			return
		}
	}

	irt.StripIncludePrefix = d
	kept := make([]string, 0, len(irt.Includes))
	for _, inc := range irt.Includes {
		if filepath.ToSlash(filepath.Clean(inc)) != d {
			kept = append(kept, inc)
		}
	}
	if len(kept) == 0 {
		irt.Includes = nil
	} else {
		irt.Includes = kept
	}
}

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
		// Header-only contract: no compiled sources. cmake permits
		// `target_sources` on INTERFACE targets, so an interface lib could
		// surface with srcs whose compilation relies on the `includes`
		// `-I` — re-rooting those to strip_include_prefix could change
		// their include behavior. Skip such targets (true to the
		// "header-only" contract).
		if len(t.Hdrs) == 0 || len(t.Srcs) != 0 || len(t.Includes) != 1 {
			continue
		}
		// Normalize the include dir before matching: trace-derived dirs
		// can carry a leading "./", a trailing "/", or surrounding
		// whitespace that Clean folds away (Bazel treats "include" and
		// "include/" equivalently). ToSlash keeps the result
		// forward-slash on Windows (filepath.Clean would emit "\", which
		// is invalid in a Bazel strip_include_prefix and wouldn't match
		// the "/"-anchored prefix below). Compare headers in the same
		// normalized space, and store the cleaned slash form.
		d := filepath.ToSlash(filepath.Clean(strings.TrimSpace(t.Includes[0])))
		if d == "" || d == "." || pathHasDotDotSegment(d) {
			continue
		}
		prefix := d + "/"
		covered := true
		for _, h := range t.Hdrs {
			if !strings.HasPrefix(filepath.ToSlash(filepath.Clean(strings.TrimSpace(h))), prefix) {
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
