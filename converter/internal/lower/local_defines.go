package lower

import (
	"strings"

	"github.com/sstriker/buildstream-bazel/converter/ir"
	"github.com/sstriker/buildstream-bazel/internal/genexeval"
	"github.com/sstriker/buildstream-bazel/internal/shadow"
)

// normalizeDefineItem strips a compiler `-D` / `/D` prefix from a
// target_compile_definitions trace item. cmake tolerates both the bare
// macro form (`HAVE_ZLIB`) and the flag form (`-DHAVE_ZLIB`, common in
// real CMakeLists — protobuf's
// `target_compile_definitions(t PRIVATE -DHAVE_ZLIB)`) and normalizes
// them to the bare form in the codemodel's CompileGroups.Defines. The
// scope-routing passes match trace items against those normalized
// codemodel defines, so the trace side must be normalized too — without
// it `-DHAVE_ZLIB` (trace) never matches `HAVE_ZLIB` (codemodel) and a
// PRIVATE define wrongly stays in the transitive `defines`, leaking to
// consumers (protobuf's libupb → protoc-gen-upb, whose guarded
// gzip_stream.cc then references zlib it doesn't link).
func normalizeDefineItem(item string) string {
	if d := strings.TrimPrefix(item, "-D"); d != item {
		return d
	}
	if d := strings.TrimPrefix(item, "/D"); d != item {
		return d
	}
	return item
}

// applyPrivateScopeToDefines routes PRIVATE-scoped
// target_compile_definitions trace events into the IR's
// non-transitive `local_defines` attribute instead of the
// transitive `defines`. Closes the cmake-to-Bazel scope-fidelity
// gap on PRIVATE compile_definitions:
//
//	target_compile_definitions(foo PRIVATE FOO_INTERNAL=1)
//	# cmake: applies only when compiling foo's own sources.
//	# Bazel `defines = ["FOO_INTERNAL=1"]` propagates the macro
//	# to every consumer linking against :foo — wrong scope.
//	# Bazel `local_defines = ["FOO_INTERNAL=1"]` matches cmake.
//
// Conservative behaviour: only moves defines the trace explicitly
// classifies as PRIVATE-scope. Defines that arrive from
// add_definitions / directory-level / CMAKE_*_FLAGS sources stay
// in the transitive Defines slice — the trace doesn't tag those,
// so the conservative default preserves the pre-existing emit.
//
// PUBLIC and INTERFACE scopes stay in Defines: Bazel's transitive
// defines is the right shape for those (the macro reaches every
// consumer). The empty-visibility legacy positional shape
// (`target_compile_definitions(foo FOO)` without a keyword) is
// PRIVATE-equivalent per cmake's own docs — also routed to
// LocalDefines.
//
// The pass walks pkg.Targets in-place. Targets without
// trace-recorded PRIVATE defines, or whose trace records no
// PRIVATE definitions, stay byte-identical to the pre-existing
// emit.
func applyPrivateScopeToDefines(pkg *ir.Package, calls []shadow.TargetCompileCall) {
	if pkg == nil || len(calls) == 0 {
		return
	}
	// Build (target → set of PRIVATE-scoped defines) from the
	// trace. Empty-visibility "" is PRIVATE-equivalent per cmake.
	privateByTarget := map[string]map[string]bool{}
	for _, call := range calls {
		if call.Cmd != "target_compile_definitions" {
			continue
		}
		for _, grp := range call.Groups {
			switch grp.Visibility {
			case "PRIVATE", "":
				// keep
			default:
				continue
			}
			set := privateByTarget[call.Target]
			if set == nil {
				set = map[string]bool{}
				privateByTarget[call.Target] = set
			}
			for _, item := range grp.Items {
				set[normalizeDefineItem(item)] = true
			}
		}
	}
	if len(privateByTarget) == 0 {
		return
	}
	for i := range pkg.Targets {
		t := &pkg.Targets[i]
		privSet := privateByTarget[t.Name]
		if len(privSet) == 0 {
			continue
		}
		// Partition t.Defines into transitive (kept) + private (moved).
		kept := t.Defines[:0]
		for _, d := range t.Defines {
			if privSet[d] {
				if !stringSliceContains(t.LocalDefines, d) {
					t.LocalDefines = append(t.LocalDefines, d)
				}
				continue
			}
			kept = append(kept, d)
		}
		t.Defines = kept
	}
}

// applyInterfaceScopeToDefines is the PRINCIPLED define-scope pass: it
// keeps a target's define in the transitive `defines` ONLY when the owning
// cmake target actually exports it via INTERFACE_COMPILE_DEFINITIONS, and
// routes every other compile-group define to the non-transitive
// `local_defines`.
//
// Why this is the faithful model. The codemodel's CompileGroups.Defines is
// the FULLY-RESOLVED per-TU define set — a target's own private/public
// definitions PLUS everything inherited from its deps' interfaces — with no
// scope tag. cmake's scope rule is simple: only INTERFACE_COMPILE_DEFINITIONS
// propagates to consumers; the per-target COMPILE_DEFINITIONS property (set
// via target_compile_definitions PRIVATE, set_property(SOURCE|TARGET …
// COMPILE_DEFINITIONS …), set_target_properties, add_definitions, the auto
// <target>_EXPORTS macro, or CMAKE_<LANG>_FLAGS_<CONFIG> globals like NDEBUG)
// is private to that target's own compilation. The trace-driven passes
// (applyPrivateScopeToDefines / applyAddDefinitionsScope) only catch the
// subset they can classify (target_compile_definitions PRIVATE +
// add_definitions); anything set by another mechanism falls through to the
// transitive `defines` and LEAKS to every consumer — VTK's KWSys feature
// macros (set via set_property(SOURCE … COMPILE_DEFINITIONS …)) and the
// vtksys_EXPORTS macro leaked onto ~2.6k consumer TUs that way.
//
// Using INTERFACE_COMPILE_DEFINITIONS as the propagation whitelist inverts
// the default to the cmake-correct one: a define propagates iff cmake says it
// does. Leaving non-exported defines in `local_defines` keeps them on the
// owning target's OWN compile (matching cmake exactly) without re-exporting
// them; defines genuinely inherited from a dep still reach the target via that
// dep's own (correctly-kept) transitive `defines`, so nothing is lost.
//
// Split sub-libraries (splitCompileGroups' per-compile-group <name>_CXX_N
// objects) carry the real defines but are keyed under a synthesized name;
// subParent maps each back to its owning cmake target so the right interface
// whitelist applies.
//
// Guarded on having an interface signal at all (genexTargets non-empty, which
// the caller pairs with decodedTrace != nil): a target ABSENT from
// genexTargets (synthesized header libs, genrules, imported-only entries) is
// left untouched, and an empty INTERFACE_COMPILE_DEFINITIONS on a PRESENT
// target legitimately means "exports nothing" → all its defines move local.
func applyInterfaceScopeToDefines(pkg *ir.Package, genexTargets map[string]genexeval.TargetInfo, subParent map[string]string) {
	if pkg == nil || len(genexTargets) == 0 {
		return
	}
	for i := range pkg.Targets {
		t := &pkg.Targets[i]
		if len(t.Defines) == 0 {
			continue
		}
		// Split subs are keyed under their synthesized name; the exported
		// set belongs to the owning cmake target.
		owner := t.Name
		if p, ok := subParent[t.Name]; ok {
			owner = p
		}
		ti, ok := genexTargets[owner]
		if !ok {
			// No codemodel/probe signal for this target — leave the emit
			// unchanged (the conservative trace passes already ran).
			continue
		}
		exported := map[string]bool{}
		for _, d := range splitResolvedDefines(ti.InterfaceCompileDefinitions) {
			exported[normalizeDefineItem(d)] = true
		}
		kept := t.Defines[:0]
		for _, d := range t.Defines {
			if exported[normalizeDefineItem(d)] {
				kept = append(kept, d)
				continue
			}
			if !stringSliceContains(t.LocalDefines, d) {
				t.LocalDefines = append(t.LocalDefines, d)
			}
		}
		t.Defines = kept
	}
}

// applyAddDefinitionsScope routes defines that originate from a
// directory-scoped add_definitions() into the non-transitive
// local_defines, matching cmake's scope rules. add_definitions is
// PRIVATE (never exported via INTERFACE_COMPILE_DEFINITIONS), but the
// codemodel folds it into every in-directory target's effective
// Defines with no origin tag — so a project like curl, whose
// `add_definitions(-DBUILDING_LIBCURL)` in lib/ would otherwise be
// emitted as a transitive Bazel `defines` on libcurl, leaks the macro
// to the curl tool that links libcurl (the tool compiles the SAME
// sources WITHOUT BUILDING_LIBCURL to alias `Curl_*`→`curlx_*`, so
// inheriting it breaks the build).
//
// No directory bookkeeping is needed: the codemodel already scoped the
// macro to exactly the targets cmake applied it to (only those carry
// it in CompileGroups.Defines), so matching by define string against
// the add_definitions set moves it on precisely those targets. The one
// guard is a define a target ALSO declares via a PUBLIC/INTERFACE
// target_compile_definitions — that arm genuinely propagates, so it
// stays in the transitive Defines even if the same string appears in
// an add_definitions elsewhere.
//
// No-op when the trace recorded no add_definitions (addDefs empty) —
// codemodel-only paths leave the emit byte-identical.
func applyAddDefinitionsScope(pkg *ir.Package, addDefs []shadow.AddDefinitionsCall, tcdCalls []shadow.TargetCompileCall) {
	if pkg == nil || len(addDefs) == 0 {
		return
	}
	addDefSet := map[string]bool{}
	for _, c := range addDefs {
		for _, it := range c.Items {
			addDefSet[normalizeDefineItem(it)] = true
		}
	}
	if len(addDefSet) == 0 {
		return
	}
	// Per-target PUBLIC/INTERFACE defines that genuinely propagate —
	// excluded from the move so a public target_compile_definitions wins
	// over a coincidentally identical add_definitions string.
	propagatingByTarget := map[string]map[string]bool{}
	for _, call := range tcdCalls {
		if call.Cmd != "target_compile_definitions" {
			continue
		}
		for _, grp := range call.Groups {
			if grp.Visibility != "PUBLIC" && grp.Visibility != "INTERFACE" {
				continue
			}
			set := propagatingByTarget[call.Target]
			if set == nil {
				set = map[string]bool{}
				propagatingByTarget[call.Target] = set
			}
			for _, item := range grp.Items {
				set[normalizeDefineItem(item)] = true
			}
		}
	}
	for i := range pkg.Targets {
		t := &pkg.Targets[i]
		if len(t.Defines) == 0 {
			continue
		}
		pubSet := propagatingByTarget[t.Name]
		kept := t.Defines[:0]
		for _, d := range t.Defines {
			if addDefSet[d] && !pubSet[d] {
				if !stringSliceContains(t.LocalDefines, d) {
					t.LocalDefines = append(t.LocalDefines, d)
				}
				continue
			}
			kept = append(kept, d)
		}
		t.Defines = kept
	}
}
