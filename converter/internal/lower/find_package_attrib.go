package lower

import (
	"path/filepath"
	"sort"
	"strings"

	"github.com/sstriker/buildstream-bazel/converter/internal/fileapi"
)

// findPackageAttrib maps absolute link-fragment paths (the bytes
// cmake's codemodel records in Target.Link.CommandFragments under
// role="libraries") back to the find_package(<Pkg>) call that
// resolved them. Used by the lower to attribute system-library
// link paths (`/usr/lib/x86_64-linux-gnu/libz.so` etc.) to a
// cmake package name even when the project uses the older
// variable-form idiom — `target_link_libraries(foo PRIVATE
// ${ZLIB_LIBRARIES})` — that doesn't surface the namespaced
// target name (`ZLIB::ZLIB`) the codemodel exposes for the
// modern form.
//
// Without this attribution, variable-form deps drop silently:
// the codemodel records only the resolved abs path, the imports
// manifest typically has no entry for the host's `/usr/lib`
// path, and the lower's Link.CommandFragments loop's
// `imports.LookupLinkPath(path)` returns nil — so the dep is
// quietly omitted from the emitted cc_library's `deps`. boost
// 1.86.0's boost_iostreams shows this exactly: ZLIB / BZip2 /
// LibLZMA all dropped → link-fails as soon as Bazel actually
// links the lib.
//
// Sources, layered most-specific first:
//
//  1. cmakeVars `<Pkg>_LIBRARIES` / `<Pkg>_LIBRARY` (and the
//     all-uppercase variants) — semicolon-separated list of
//     absolute paths cmake's find module bound to the variable.
//     Authoritative when present; DumpVars-gated so only fires
//     when --lift-configure-file or --probe-genex is on.
//  2. Library-basename heuristic — when the cmakeVars path
//     isn't available (DumpVars off), match a path whose
//     basename starts with `lib<pkg>.` or `lib<pkg>-` to a
//     found package, case-insensitively. Covers the common
//     `lib<lowercase pkg>.so` cmake-bundles-the-find-module
//     case (zlib → libz isn't covered; the heuristic prefers
//     false-negatives over false-positives, so packages with
//     non-obvious library basenames need the cmakeVars path).
type findPackageAttrib struct {
	// byPath maps an absolute path → find_package name. Built
	// from cmakeVars `<Pkg>_LIBRARIES` lookups; deterministic
	// across runs.
	byPath map[string]string

	// foundPackages is the ordered (insertion-order) list of
	// find_package names where IsFound=true. Used by the
	// basename heuristic; ordered so two packages whose
	// basenames could both match a given path resolve to the
	// first declared one (rare, but possible if two find
	// modules wrap libraries with overlapping name prefixes).
	foundPackages []string
}

// buildFindPackageAttrib walks the configureLog events for
// find_package-v1 with IsFound=true, then for each one:
//   - records the package name into foundPackages
//   - inspects cmakeVars for `<Pkg>_LIBRARIES` / `<Pkg>_LIBRARY`
//     and the all-uppercase variants, splitting on `;` into
//     individual abs paths; populates byPath.
//
// Returns nil when no find_package was found, so callers can
// short-circuit downstream walks.
func buildFindPackageAttrib(events []fileapi.Event, cmakeVars map[string]string) *findPackageAttrib {
	fa := &findPackageAttrib{byPath: map[string]string{}}
	for _, e := range events {
		if e.Kind != "find_package-v1" {
			continue
		}
		if e.Found == nil || !e.Found.IsFound {
			continue
		}
		pkg := e.Found.Package
		if pkg == "" {
			continue
		}
		fa.foundPackages = append(fa.foundPackages, pkg)
		if len(cmakeVars) == 0 {
			continue
		}
		for _, key := range packageVarKeys(pkg) {
			value, ok := cmakeVars[key]
			if !ok || value == "" {
				continue
			}
			for _, raw := range strings.Split(value, ";") {
				path := strings.TrimSpace(raw)
				if path == "" || !filepath.IsAbs(path) {
					continue
				}
				if _, dup := fa.byPath[path]; dup {
					continue
				}
				fa.byPath[path] = pkg
			}
		}
	}
	if len(fa.foundPackages) == 0 && len(fa.byPath) == 0 {
		return nil
	}
	return fa
}

// packageVarKeys returns the cmake variable names the find
// module conventionally binds the library list under. cmake's
// guidance varies by find module age:
//   - modern (cmake >= 3): `<Pkg>_LIBRARIES` with the Pkg
//     name in its canonical casing (`ZLIB`, `BZip2`).
//   - older: `<PKG>_LIBRARIES` with all-uppercase Pkg.
//   - singular: `<Pkg>_LIBRARY` for find modules that
//     resolve to one path.
//
// We try all four shapes; first hit wins downstream.
func packageVarKeys(pkg string) []string {
	upper := strings.ToUpper(pkg)
	keys := []string{
		pkg + "_LIBRARIES",
		pkg + "_LIBRARY",
	}
	if upper != pkg {
		keys = append(keys, upper+"_LIBRARIES", upper+"_LIBRARY")
	}
	return keys
}

// Lookup returns the find_package name that resolved path, or
// "" when none matches. Path comparison is exact for the
// cmakeVars-derived map and basename-prefix for the heuristic.
//
// Returns the cmakeVars-derived attribution in preference to
// the heuristic — the variable namespace is authoritative
// when DumpVars captured it.
func (fa *findPackageAttrib) Lookup(path string) string {
	if fa == nil || path == "" {
		return ""
	}
	if pkg, ok := fa.byPath[path]; ok {
		return pkg
	}
	base := strings.ToLower(filepath.Base(path))
	// The heuristic considers ONLY the lib<pkg>.{so,a,...}
	// shape — `lib` prefix + literal package name as the
	// basename root. cmake's "Modules" come with cases where
	// pkg name and lib basename don't match (BZip2 → libbz2,
	// LibLZMA → liblzma, FreeType → libfreetype). Those need
	// the cmakeVars path; the heuristic is for the common
	// case where the find module bundles a same-named library.
	for _, pkg := range fa.foundPackages {
		pkgL := strings.ToLower(pkg)
		if pkgL == "" {
			continue
		}
		if strings.HasPrefix(base, "lib"+pkgL+".") ||
			strings.HasPrefix(base, "lib"+pkgL+"-") {
			return pkg
		}
	}
	return ""
}

// SortedFoundPackages returns the found-package names in a
// deterministic order. Used by callers that emit
// header-comment summaries; the lower's per-target attribution
// uses Lookup directly.
func (fa *findPackageAttrib) SortedFoundPackages() []string {
	if fa == nil {
		return nil
	}
	out := append([]string(nil), fa.foundPackages...)
	sort.Strings(out)
	return out
}
