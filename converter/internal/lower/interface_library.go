package lower

import (
	"path/filepath"
	"sort"
	"strings"

	"github.com/sstriker/buildstream-bazel/converter/ir"
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
				if rel == "" || strings.HasPrefix(rel, "../") || filepath.IsAbs(rel) {
					continue
				}
				includesByTarget[ic.Target] = append(includesByTarget[ic.Target], rel)
			}
		}
	}

	definesByTarget := map[string][]string{}
	for _, tc := range decoded.CompileDefinitions {
		for _, grp := range tc.Groups {
			if grp.Visibility != "INTERFACE" && grp.Visibility != "PUBLIC" {
				continue
			}
			for _, def := range grp.Items {
				if def = strings.TrimSpace(def); def != "" {
					definesByTarget[tc.Target] = append(definesByTarget[tc.Target], def)
				}
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
			h, err := discoverHeaders(hostSrc, includes, cache, missing)
			if err == nil {
				hdrs = h
			}
		}

		tgt := ir.Target{
			Name:       call.Name,
			Kind:       ir.KindCCLibrary,
			Hdrs:       hdrs,
			Includes:   includes,
			Defines:    defines,
			Visibility: []string{"//visibility:public"},
			Tags:       []string{"cmake-codegen-interface-library-from-trace"},
		}
		out = append(out, tgt)
	}
	// Deterministic order by name.
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
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
