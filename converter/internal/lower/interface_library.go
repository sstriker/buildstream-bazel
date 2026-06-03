package lower

import (
	"path/filepath"
	"sort"
	"strings"

	"github.com/sstriker/buildstream-bazel/converter/ir"
	"github.com/sstriker/buildstream-bazel/internal/genexeval"
	"github.com/sstriker/buildstream-bazel/internal/shadow"
)

// lowerInterfaceLibraries synthesizes cc_library IR targets for
// `add_library(<name> INTERFACE)` calls the cmake codemodel
// dropped. cmake's File API doesn't list INTERFACE_LIBRARY targets
// in its `targets[]` array (they have no link step to model), so
// the main lift's codemodel walk emits nothing for them. The
// trace records `add_library(<name> INTERFACE)` directly; this
// lift cross-references against the codemodel-known set
// (knownTargets) and emits one cc_library per INTERFACE-only
// declaration the codemodel didn't already cover.
//
// Resulting cc_library shape:
//
//	cc_library(
//	    name      = <call.Name>,
//	    hdrs      = glob([<rel-include-dir>/**/*.h, *.hpp, ...]),
//	    includes  = [<rel-include-dir>],          // from
//	                                              // target_include_directories(... INTERFACE ...)
//	    defines   = [<defs>...],                  // from
//	                                              // target_compile_definitions(... INTERFACE ...)
//	)
//
// Walks the trace-recorded INTERFACE arms of `target_include_directories`
// and `target_compile_definitions` for the same target name to
// populate the body. PUBLIC arms also propagate to consumers under
// cmake semantics so they're folded in too; PRIVATE arms are
// excluded (they're compile-only for the target itself, but an
// INTERFACE library has no compile step).
//
// hostSrc is the on-disk path to the project source tree used to
// resolve absolute include paths to workspace-relative form;
// cmakeSrc is the cmake-recorded source root, workspaceRoot the
// detected (umbrella-or-cmakeSrc) anchor the rest of the lift uses
// for path keys. Returns an empty slice when no add_library
// INTERFACE calls survive the cross-reference (typical for
// projects that have compiled libraries — every target is already
// in the codemodel).
//
// ALIAS-form declarations are skipped; they're already covered by
// the underlying target's emission, and Bazel doesn't need an
// extra rule for them.
func lowerInterfaceLibraries(
	decoded *shadow.Decoded,
	knownTargets map[string]bool,
	hostSrc, cmakeSrc, workspaceRoot string,
	genexTargets map[string]genexeval.TargetInfo,
	cc *codegenContext,
) []ir.Target {
	if decoded == nil || len(decoded.AddLibraries) == 0 {
		return nil
	}

	// Build per-target Includes (INTERFACE + PUBLIC) and Defines.
	// PUBLIC arms apply to the target itself AND propagate; for
	// an INTERFACE-only target, both arms describe what consumers
	// see, which is the entire interface surface.
	includesByTarget := map[string][]string{}
	// Targets whose INTERFACE/PUBLIC include path is the package root
	// (e.g. `target_include_directories(lib INTERFACE
	// $<BUILD_INTERFACE:${CMAKE_SOURCE_DIR}>)`). Bazel rejects
	// `includes = [""]`, so the root never becomes an include attr — but
	// the headers under it must still be discovered, so we record the
	// target here and prepend "" to its discoverHeaders walk below
	// (mirrors the codemodel path's walkPkgRootForHdrs). Without this an
	// INTERFACE lib that declares only the source root emits empty
	// (glm's glm-header-only shape).
	rootWalkByTarget := map[string]bool{}
	for _, ic := range decoded.Includes {
		for _, grp := range ic.Groups {
			if grp.Visibility != "INTERFACE" && grp.Visibility != "PUBLIC" {
				continue
			}
			for _, dir := range grp.Dirs {
				rel := dir
				if filepath.IsAbs(dir) {
					if r, ok := relativeIfInside(workspaceRoot, dir); ok {
						rel = r
					} else if r, ok := relativeIfInside(cmakeSrc, dir); ok {
						rel = r
					}
				}
				rel = strings.TrimSpace(rel)
				if rel == "" {
					rootWalkByTarget[ic.Target] = true
					continue
				}
				if strings.HasPrefix(rel, "../") || filepath.IsAbs(rel) {
					continue
				}
				includesByTarget[ic.Target] = append(includesByTarget[ic.Target], rel)
			}
		}
	}

	definesByTarget := map[string][]string{}
	// Targets where at least one INTERFACE/PUBLIC define carried a
	// generator expression the Go-side evaluator couldn't crack.
	// For these we prefer cmake's own resolved
	// INTERFACE_COMPILE_DEFINITIONS (captured by the structural
	// genex probe) over the partial trace-evaluated list — see the
	// reconciliation pass below.
	unresolvedGenexTargets := map[string]bool{}
	for _, tc := range decoded.CompileDefinitions {
		for _, grp := range tc.Groups {
			if grp.Visibility != "INTERFACE" && grp.Visibility != "PUBLIC" {
				continue
			}
			for _, def := range grp.Items {
				def = strings.TrimSpace(def)
				if def == "" {
					continue
				}
				// Evaluate cmake generator expressions like
				// `$<$<BOOL:OFF>:FOO=1>`. nlohmann-json emits ~5
				// of these as the INTERFACE define list; cmake
				// evaluates them at generate time based on the
				// option values.
				//
				// Try the (a) genexeval parser first for shapes
				// it supports; fall back to a small BOOL/NOT-
				// specific evaluator for the nested
				// `$<$<BOOL:...>:RESULT>` shape the parser
				// rejects (its op-name lexer doesn't accept
				// nested `$<`).
				if strings.Contains(def, "$<") {
					if nodes, err := genexeval.Parse([]byte(def)); err == nil {
						if eval, err := genexeval.Eval(nodes, genexeval.Context{}); err == nil {
							def = strings.TrimSpace(string(eval))
						}
					} else if evalled, ok := evalNestedBoolGenex(def); ok {
						def = strings.TrimSpace(evalled)
					}
				}
				if def == "" {
					continue
				}
				// Still unresolved: note the target so the
				// structural-probe reconciliation below can
				// substitute cmake's own resolved define list.
				// Without that, dropping silently loses a define
				// that genuinely applies under the configured
				// build (e.g. `$<$<CONFIG:Release>:NDEBUG_EXTRA>`)
				// — intent loss the probe lets us avoid.
				if strings.Contains(def, "$<") {
					unresolvedGenexTargets[tc.Target] = true
					continue
				}
				definesByTarget[tc.Target] = append(definesByTarget[tc.Target], def)
			}
		}
	}

	// Structural-probe reconciliation: for every target whose
	// INTERFACE defines had an unresolved genex, replace the
	// partial trace-evaluated list with cmake's own resolved
	// INTERFACE_COMPILE_DEFINITIONS (the structural genex probe
	// captured it at generation time, where cmake's evaluator
	// already answered every `$<…>`). This turns "drop the define
	// we couldn't evaluate" into "emit the define cmake resolved"
	// — capturing intent instead of losing it. Empty probe data
	// (no --probe-genex, cmake < 3.24) leaves behavior unchanged:
	// the partial list stands and unresolved entries stay dropped.
	for tgt := range unresolvedGenexTargets {
		ti, ok := genexTargets[tgt]
		if !ok || ti.InterfaceCompileDefinitions == "" {
			continue
		}
		resolved := splitResolvedDefines(ti.InterfaceCompileDefinitions)
		if len(resolved) == 0 {
			continue
		}
		definesByTarget[tgt] = resolved
	}

	// Build per-target Deps from INTERFACE/PUBLIC arms of
	// target_link_libraries. cmake projects with deep modular
	// structure (abseil, modular Boost) declare INTERFACE
	// libraries as deps-only wrappers — `absl_check`
	// `target_link_libraries(absl_check INTERFACE
	// absl::log_internal_check_impl)`. Without routing the deps,
	// the trace-synthesized cc_library is empty and the
	// `empty-cc-library` audit fires (abseil: 100 findings, all
	// these wrapper interfaces).
	//
	// Lib name → Bazel label resolution:
	//
	//   1. If the lib appears in `decoded.AddLibraries` as an
	//      ALIAS, resolve to the underlying target's sanitized
	//      name (`absl::log_internal_check_impl` →
	//      `:absl_log_internal_check_impl`).
	//   2. If the lib is a plain in-tree name (no `::`), emit as
	//      `:<name>` — the consumer side resolves it whether the
	//      target is codemodel-emitted or trace-synthesized.
	//   3. If the lib has `::` but no recorded ALIAS, sanitize
	//      `::` → `_` and emit (alias-target rule will resolve).
	//   4. Empty / build-genex / link-flag tokens drop silently.
	aliasMap := map[string]string{}
	for _, call := range decoded.AddLibraries {
		if call.Type == "ALIAS" && len(call.Aliases) > 0 {
			aliasMap[call.Name] = call.Aliases[0]
		}
	}
	resolveLibToLabel := func(lib string) string {
		lib = strings.TrimSpace(lib)
		if lib == "" {
			return ""
		}
		// Drop link-flag tokens; cmake's File API records flags
		// (`-Wl,...`, `-pthread`) here too — those route through
		// the link path, not deps.
		if strings.HasPrefix(lib, "-") {
			return ""
		}
		// Drop genex placeholders cmake didn't expand.
		if strings.Contains(lib, "$<") {
			return ""
		}
		if actual, ok := aliasMap[lib]; ok {
			return ":" + strings.ReplaceAll(actual, "::", "_")
		}
		return ":" + strings.ReplaceAll(lib, "::", "_")
	}
	depsByTarget := map[string][]string{}
	for _, link := range decoded.Links {
		for _, grp := range link.Groups {
			if grp.Visibility != "INTERFACE" && grp.Visibility != "PUBLIC" {
				continue
			}
			seen := map[string]bool{}
			for _, lib := range grp.Libs {
				label := resolveLibToLabel(lib)
				if label == "" || seen[label] {
					continue
				}
				seen[label] = true
				depsByTarget[link.Target] = append(depsByTarget[link.Target], label)
			}
		}
	}

	var out []ir.Target
	emitted := map[string]bool{}
	for _, call := range decoded.AddLibraries {
		if call.Type != "INTERFACE" {
			continue
		}
		// Skip when the codemodel already knew about this target —
		// no need to double-emit. (cmake 3.19+ does expose
		// INTERFACE_LIBRARYs in codemodel-v2 under some shapes;
		// the cross-reference keeps the lift's output stable
		// regardless of codemodel surface.)
		if knownTargets[call.Name] {
			continue
		}
		// Skip namespaced cmake names like `OpenGL::GL` /
		// `Foo::Bar` — these are cmake's IMPORTED-target alias
		// surface from find_package(). The underlying actual
		// target (e.g. the system OpenGL library) isn't
		// declarable as a Bazel rule from converter side; the
		// operator's manifest resolves these through their own
		// rules. Emitting them here would produce a
		// `cc_library(name = "OpenGL::GL", ...)` which Bazel
		// rejects ("not a valid Bazel identifier") — even
		// sanitizing the name doesn't help since the resulting
		// rule still wouldn't have the right hdrs/deps.
		if strings.Contains(call.Name, "::") {
			continue
		}
		if emitted[call.Name] {
			continue
		}
		emitted[call.Name] = true

		includes := dedupSlice(includesByTarget[call.Name])
		defines := dedupSlice(definesByTarget[call.Name])

		// Walk each include dir at convert time to materialise an
		// explicit hdrs list. The walk uses the existing
		// discoverHeaders helper so the missing-include-dir
		// warning path is shared with the codemodel-driven lift.
		// Bazel-idiom: a fixed list is fine here because the
		// converter re-runs on source-tree changes anyway; using
		// `glob(...)` would require an IR/emit change we can
		// queue separately.
		var hdrs []string
		if hostSrc != "" {
			cache := cc.HeaderWalkCache
			if cache == nil {
				cache = map[string][]string{}
			}
			missing := cc.MissingIncludeDirs
			if missing == nil {
				missing = map[string]bool{}
			}
			// Prepend the package root "" when the target declared a
			// root-level include (see rootWalkByTarget) so discoverHeaders
			// materialises the headers that live there — without it the
			// INTERFACE lib would emit empty.
			walkDirs := includes
			if rootWalkByTarget[call.Name] {
				walkDirs = append([]string{""}, includes...)
			}
			h, err := discoverHeaders(hostSrc, walkDirs, cache, missing)
			if err == nil {
				hdrs = h
			}
		}

		// Filter self-deps that snuck through (an INTERFACE lib
		// shouldn't list itself, but the trace can record both
		// the bare name and the `::` form of the same target).
		deps := depsByTarget[call.Name]
		filtered := deps[:0]
		selfLabel := ":" + call.Name
		for _, d := range deps {
			if d != selfLabel {
				filtered = append(filtered, d)
			}
		}
		tgt := ir.Target{
			Name:       call.Name,
			Kind:       ir.KindCCLibrary,
			Hdrs:       hdrs,
			Includes:   includes,
			Defines:    defines,
			Deps:       filtered,
			Visibility: []string{"//visibility:public"},
			Tags:       []string{"cmake-codegen-interface-library-from-trace"},
		}
		out = append(out, tgt)
	}
	// Deterministic order by name.
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// evalNestedBoolGenex evaluates the cmake genex shape
//
//	$<$<BOOL:<val>>:<result>>
//	$<$<NOT:$<BOOL:<val>>>:<result>>
//
// that the (a) genexeval parser rejects (its op-name scan doesn't
// accept nested `$<`). This covers the common header-only-library
// pattern of guarding INTERFACE defines on cmake-side options
// (e.g. nlohmann-json's
// $<$<BOOL:OFF>:JSON_DIAGNOSTICS=1>).
//
// Returns (result, true) on a recognised shape; (_, false)
// otherwise. The caller falls through to "drop the define" when
// false (literal `$<` text reaching the C compiler is worse than
// silent drop).
func evalNestedBoolGenex(s string) (string, bool) {
	// Shape 1: $<$<BOOL:<val>>:<result>>
	if rest, ok := stripPrefix(s, "$<$<BOOL:"); ok {
		// rest is "<val>>:<result>>"
		i := strings.Index(rest, ">:")
		if i < 0 {
			return "", false
		}
		val := rest[:i]
		body := rest[i+2:]
		if !strings.HasSuffix(body, ">") {
			return "", false
		}
		body = body[:len(body)-1]
		if isBoolTrue(val) {
			return body, true
		}
		return "", true
	}
	// Shape 2: $<$<NOT:$<BOOL:<val>>>:<result>>
	if rest, ok := stripPrefix(s, "$<$<NOT:$<BOOL:"); ok {
		i := strings.Index(rest, ">>>:")
		if i < 0 {
			return "", false
		}
		val := rest[:i]
		body := rest[i+4:]
		if !strings.HasSuffix(body, ">") {
			return "", false
		}
		body = body[:len(body)-1]
		if isBoolTrue(val) {
			return "", true
		}
		return body, true
	}
	return "", false
}

func stripPrefix(s, prefix string) (string, bool) {
	if strings.HasPrefix(s, prefix) {
		return s[len(prefix):], true
	}
	return "", false
}

// isBoolTrue mirrors cmake's BOOL coercion: case-insensitive
// "1", "ON", "YES", "TRUE", "Y" → true; anything else → false.
// cmake's actual rules cover more strings (any non-zero number,
// etc.) but the common cases the genex shape carries are limited.
func isBoolTrue(s string) bool {
	switch strings.ToUpper(strings.TrimSpace(s)) {
	case "1", "ON", "YES", "TRUE", "Y":
		return true
	}
	return false
}

// dedupSlice returns a copy of vs with duplicate entries removed
// while preserving first-occurrence order.
func dedupSlice(vs []string) []string {
	if len(vs) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(vs))
	out := make([]string, 0, len(vs))
	for _, v := range vs {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}

// splitResolvedDefines splits cmake's resolved
// INTERFACE_COMPILE_DEFINITIONS string (the structural probe
// captures it as a `;`-joined list, cmake's native list
// separator) into individual define entries, trimming whitespace
// and dropping empties. A define that still carries an unresolved
// `$<…>` is skipped defensively — the probe resolves everything in
// practice, but a literal genex must never reach the compiler as a
// define.
func splitResolvedDefines(joined string) []string {
	parts := strings.Split(joined, ";")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" || strings.Contains(p, "$<") {
			continue
		}
		out = append(out, p)
	}
	return out
}
