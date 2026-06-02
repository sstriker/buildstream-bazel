package lower

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/sstriker/buildstream-bazel/converter/internal/ninja"
	"github.com/sstriker/buildstream-bazel/converter/ir"
	"github.com/sstriker/buildstream-bazel/internal/shadow"
)

// standaloneTraceContext bundles the optional trace-derived inputs
// `lowerStandaloneCustomCommands` consults when naming + sizing
// the visibility of the emitted genrules. Empty / nil-valued fields
// disable the corresponding cross-reference (the caller pre-trace
// uplift / TraceRaw-empty path passes a zero-valued ctx, which
// keeps the legacy `custom_command_<sanitized-output>` /
// `//visibility:private` shape intact).
//
// Phase 4 residue: PR #237's predecessor named standalone genrules
// purely from output-path hashes and hardcoded visibility. The
// trace records the source-level call site (add_custom_command's
// OUTPUT list, add_custom_target's wrapping target name, downstream
// add_dependencies links) so the rendered genrule can:
//
//   - Name itself after the add_custom_target that wraps the
//     OUTPUT (when one exists), instead of the hash.
//   - Open visibility to `:__pkg__` when another target in the
//     same package references the OUTPUT via add_dependencies
//     (or names the wrapping custom-target via the same).
//
// The cross-reference is a heuristic — `add_dependencies` doesn't
// uniquely identify cross-package consumers — but it covers the
// common "library X uses generator Y in the same CMakeLists" case
// that produces the bulk of real-world refusal noise.
type standaloneTraceContext struct {
	// CustomCommands and CustomTargets carry the user's
	// source-level call sites for OUTPUT-form
	// add_custom_command and add_custom_target events,
	// respectively. Insertion order is the trace-recorded
	// order; the lookups built from these slices are by-output
	// or by-name maps so order doesn't drive naming.
	CustomCommands []shadow.AddCustomCommandCall
	CustomTargets  []shadow.AddCustomTargetCall

	// AddDependencies carries the user's source-level
	// add_dependencies(target dep1 dep2 ...) calls. Used to
	// detect downstream consumers of an emitted genrule's
	// outputs — when target T depends on a custom-target wrapping
	// OUTPUT foo.h (or directly on foo.h), the foo.h-producing
	// genrule needs at least `:__pkg__` visibility for T to
	// reference it.
	AddDependencies []shadow.AddDependenciesCall

	// AliasToActual maps each add_library(<alias> ALIAS <actual>) alias name
	// to its actual producing target (namespaced aliases like Foo::Bar
	// included). Used by the genex audit: cmake allows $<TARGET_FILE:alias>,
	// but rewriteToolFromTarget lifts the resolved artifact path to the ACTUAL
	// target, so the genrule's tools carries `:actual`, not `:alias` — the
	// classifier normalizes a referenced genex target name through this map
	// before checking tools so an aliased reference isn't mis-tagged
	// -unresolved. Empty / nil when the project declares no aliases.
	AliasToActual map[string]string
}

// lowerStandaloneCustomCommands walks every CUSTOM_COMMAND edge in
// build.ninja and emits an ir.KindGenrule for each edge whose outputs
// aren't already covered by an existing genrule in cc.Genrules.
//
// Phase 4 of the generator-parity uplift (ROADMAP.md). The existing
// recoverGenrule path covers custom-command edges whose outputs
// other cmake targets reference as sources; this consumer picks up
// the standalone shape (add_custom_target without a downstream
// consumer, add_custom_command that drives an out-of-graph side
// effect like a code generator that produces files no cmake target
// claims).
//
// Dedup: an edge is skipped when ANY of its outputs is already
// present as an `outs` entry in cc.Genrules. First-output-wins isn't
// safe because a single CUSTOM_COMMAND can produce multiple
// outputs and only one might be referenced by another target.
//
// Naming: when traceCtx records an add_custom_target whose DEPENDS /
// BYPRODUCTS / SOURCES list intersects the edge's OUTPUTs, the
// genrule takes that target's name (e.g. `add_custom_target(gen_headers
// DEPENDS version.h)` → `gen_headers`, not `custom_command_version_h`).
// Otherwise the genrule falls back to
// `custom_command_<sanitized first output>` so the name is stable
// across rebuilds. Name collisions get a `_<index>` suffix in both
// shapes.
//
// Visibility: when traceCtx records an add_dependencies(t producer)
// call where `producer` is the name picked above (or an
// add_custom_target whose OUTPUTs intersect the edge's), or when
// any add_dependencies call lists the OUTPUT directly, visibility
// opens to `:__pkg__`. Without that signal it stays
// `//visibility:private` (the conservative default — no other
// target in the package references the OUTPUT, so no consumer
// needs to see it).
//
// cmakeSrc is the cmake source root — used by genruleSrcs to
// re-anchor source-tree-absolute input paths (`/tmp/<src>/foo.c`)
// to workspace-relative form (`foo.c`); essential for the
// --cmake-script-bake / Phase 4 standalone-edge cases where the
// edge's ninja-recorded inputs arrive as absolute paths from
// cmake's `cmake -P` build-line resolution. Without it the
// rendered genrule's srcs leak the convert-time absolute prefix,
// and Bazel sandbox-misses the input at action time.
//
// buildDir is the cmake build directory — used to convert build-
// relative output paths to package-relative paths the emitted
// genrule's outs reference.
//
// artifactToName threads the codemodel artifact-path → IR target-name
// map into the lift so rewriteToolFromTarget can lift bare
// `bin/<tool>` references in the cmd into `$(location :<name>)` +
// tools attribute entries. Empty map disables tool rewriting.
func lowerStandaloneCustomCommands(g *ninja.Graph, existing []ir.Target, cmakeSrc, buildDir, umbrellaPrefix string, artifactToName map[string]string, traceCtx standaloneTraceContext) []ir.Target {
	if g == nil {
		return nil
	}
	covered := coveredOuts(existing)
	edges := ninja.CustomCommandEdges(g)
	if len(edges) == 0 {
		return nil
	}

	// Pre-build the trace-derived lookups once per call. Each
	// returns an empty map when traceCtx carries no records, so
	// the legacy `custom_command_<output>` / private-visibility
	// path stays intact on the offline-replay-no-trace path.
	outputToTargetName := buildOutputToCustomTargetIndex(traceCtx.CustomCommands, traceCtx.CustomTargets)
	consumedOutputs := buildConsumedOutputIndex(traceCtx.CustomCommands, traceCtx.CustomTargets, traceCtx.AddDependencies)
	// Generator-expression audit index: maps each genex-bearing
	// add_custom_command's outputs to its $<...> footprint, so the
	// emitted genrule can be tagged resolved/unresolved depending on
	// whether its path-bearing genexes (TARGET_FILE / TARGET_OBJECTS)
	// were lifted to $(location) labels. nil when the trace has no
	// genex-bearing commands (the audit tag is then never added).
	genexIndex := buildOutputToCustomCommandGenex(traceCtx.CustomCommands)

	var out []ir.Target
	seenNames := map[string]int{}
	for _, b := range edges {
		if edgeCovered(b, covered) {
			continue
		}
		cmd, ok := ninja.CommandFor(g, b)
		if !ok || cmd == "" {
			// Rule without a command binding — cmake's no-op
			// stamp shape (just declares a phony output). Skip.
			continue
		}
		// All outputs reference relative to the build dir's
		// per-target convention; emit them as-is. Stripping
		// buildDir isn't safe because the outputs are already
		// build-dir-relative in cmake's Ninja generator.
		outs := append([]string(nil), b.Outputs...)
		outs = append(outs, b.ImplicitOuts...)
		// Drop outputs that contain unexpanded ninja variable
		// references (e.g. `${cmake_ninja_workdir}foo.txt`).
		// cmake's Ninja generator pairs every real custom-command
		// output with a `${cmake_ninja_workdir}<basename>`
		// implicit-output shadow so the restat=1 semantics see
		// both relative and absolute paths; a Bazel genrule can't
		// declare an outs entry whose path is a ninja-time
		// variable reference. The real (variable-free) output
		// stays in the list and drives the genrule's outs / name.
		outs = filterOutVarRefs(outs)
		// Sort for byte-stability and dedup.
		sort.Strings(outs)
		outs = dedupSorted(outs)
		if len(outs) == 0 {
			continue
		}
		// Skip cmake-internal bookkeeping edges. cmake's Ninja
		// generator adds standalone CUSTOM_COMMAND edges for its
		// own IDE / regen workflows: `CMakeFiles/edit_cache.util`,
		// `CMakeFiles/rebuild_cache.util`, and a handful more
		// (install / package / package_source / test / list_install
		// _components) under multi-target generators. These run
		// cmake itself or echo a message; lifting them to genrules
		// in the rendered BUILD.bazel adds noise without
		// operator-visible value (the cmake re-invoke shape
		// doesn't work outside cmake's own build dir, and the IDE
		// hooks are no-ops under bazel anyway). Filter every edge
		// whose first output is `CMakeFiles/<name>.util` —
		// cmake's bookkeeping outputs uniformly land under that
		// path with the .util suffix.
		if isCMakeBookkeepingOutput(outs[0]) {
			continue
		}
		// Skip cmake-internal install / regen / cpack edges
		// regardless of output path. These run cmake (or
		// cpack/rpmbuild) at action time against cmake's own
		// build-dir layout — they don't translate to the Bazel
		// sandbox AT ALL. Filtering at the cmd-shape level
		// catches every install-component variant
		// (DCMAKE_INSTALL_COMPONENT, DCMAKE_INSTALL_DO_STRIP,
		// DCMAKE_INSTALL_LOCAL_ONLY) plus cpack/rpmbuild +
		// regen-during-build edges whose output paths land
		// outside the `.util` filter above (e.g.
		// `CMakeFiles/install-<component>` shapes from
		// add_subdirectory-spawned install components).
		if isCMakeInternalCmd(cmd) {
			continue
		}
		// Use genruleSrcs so source-tree-absolute inputs
		// (e.g. `/tmp/<src>/foo.c` from a `cmake -P` build line
		// that the ninja generator resolved with the cmake build
		// dir's absolute prefix) get re-anchored to workspace-
		// relative form. The pre-genruleSrcs path appended the
		// raw ninja inputs verbatim, leaking convert-time
		// absolute paths into the rendered genrule.
		srcs := genruleSrcs(b, cmakeSrc, buildDir, umbrellaPrefix)

		// Naming: prefer the source-level add_custom_target name
		// when one wraps any of the edge's outputs. Falls back to
		// the legacy `custom_command_<sanitized first output>`
		// shape when the trace is absent or when the OUTPUT isn't
		// wrapped by a custom-target. Collision suffixing applies
		// to both shapes — multiple edges that share a base name
		// after sanitization get a `_<index>` suffix.
		baseName := pickStandaloneName(outs, outputToTargetName)
		name := baseName
		if n, used := seenNames[baseName]; used {
			seenNames[baseName] = n + 1
			name = baseName + "_" + intToStr(n+1)
		} else {
			seenNames[baseName] = 1
		}

		// Visibility: open to `:__pkg__` when any of the edge's
		// outputs (or the wrapping target-name, if any) is named
		// by an add_dependencies call in the same trace. The
		// heuristic intentionally errs on the private side —
		// projects whose cross-references aren't expressed via
		// add_dependencies keep the conservative default.
		visibility := []string{"//visibility:private"}
		if hasDownstreamConsumer(outs, baseName, consumedOutputs) {
			visibility = []string{":__pkg__"}
		}

		// Two-pass: anchor / cmake-E / host-bin normalisation
		// first, then tool-from-target lift on the workspace-
		// relative cmd. The tool lift's artifact-path keys are
		// build-dir-relative (cmake records them that way); the
		// anchor pass strips buildDir prefixes from the cmd so
		// the bare `bin/<tool>` form survives intact for the
		// lookup.
		rewrittenCmd := rewriteGenruleCmd(cmd, cmakeSrc, buildDir, umbrellaPrefix)
		rewrittenCmd, tools := rewriteToolFromTarget(rewrittenCmd, artifactToName)
		// The tool-from-target lift hoisted the generator binary into
		// `tools` and rewrote the cmd to $(location :tool); drop the
		// now-redundant build artifact (e.g. the multi-config
		// `<cfg>/bin/<tool>`) from srcs so it doesn't survive as a
		// dangling file input with no producing package.
		srcs = dropLiftedToolSrcs(srcs, tools, artifactToName)
		// Re-anchor build-dir-relative output paths in the cmd to
		// $(RULEDIR)/<out> so the genrule writes its declared outs
		// under bazel-out rather than to a bare relative path (which
		// bazel rejects as a missing output). split.go re-relativizes
		// the $(RULEDIR)-relative path if the genrule moves packages.
		rewrittenCmd = anchorGenruleOutputsToRuledir(rewrittenCmd, outs)
		// Audit: when the trace shows this command carried generator
		// expressions, tag whether its path-bearing genexes resolved to
		// $(location) labels (portable) or baked a machine-specific
		// literal (the rewriteToolFromTarget lift covers single-artifact
		// $<TARGET_FILE:t>; $<TARGET_OBJECTS:t> / cross-element refs are
		// the known residue and surface as -unresolved).
		tags := []string{"cmake-codegen-standalone-custom-command"}
		if genexTag := customCommandGenexTag(outs, genexIndex, tools, traceCtx.AliasToActual); genexTag != "" {
			tags = append(tags, genexTag)
		}
		out = append(out, ir.Target{
			Name:         name,
			Kind:         ir.KindGenrule,
			Srcs:         srcs,
			GenruleOuts:  outs,
			GenruleCmd:   rewrittenCmd,
			GenruleTools: tools,
			Visibility:   visibility,
			Tags:         tags,
		})
	}
	return out
}

// dropLiftedToolSrcs removes srcs that rewriteToolFromTarget already
// hoisted into the genrule's `tools` attribute. The lift rewrites a
// build-dir-relative artifact reference in the cmd (e.g.
// `<cfg>/bin/llvm-min-tblgen`) to `$(location :<name>)` and adds
// `:<name>` to tools; the same artifact path also lands in srcs (it's
// a ninja input of the edge). Left there it renders as a dangling file
// label (`//:<cfg>/bin/<tool>`) with no producing package. We match a
// src by the same artifactToName key the cmd lift used, so the two
// stay in lockstep.
func dropLiftedToolSrcs(srcs, tools []string, artifactToName map[string]string) []string {
	if len(tools) == 0 || len(srcs) == 0 || len(artifactToName) == 0 {
		return srcs
	}
	lifted := make(map[string]bool, len(tools))
	for _, t := range tools {
		lifted[t] = true // tools are ":<name>" labels
	}
	kept := make([]string, 0, len(srcs))
	for _, s := range srcs {
		if name, ok := artifactToName[s]; ok && lifted[":"+name] {
			continue
		}
		kept = append(kept, s)
	}
	return kept
}

// anchorGenruleOutputsToRuledir rewrites each build-dir-relative output
// path that appears literally in the cmd to $(RULEDIR)/<out>. cmake's
// Ninja generator bakes the output path as a build-dir-relative literal
// (e.g. `-o include/llvm/.../X.inc`); a Bazel genrule must instead write
// its declared outs under $(RULEDIR) (the package's bin dir). Outputs
// are rewritten longest-first so a shorter out that is a path-prefix of
// a longer one doesn't rewrite the longer one's interior, and an out
// already carrying the $(RULEDIR)/ prefix is skipped (idempotent).
// Sibling paths derived from an out by suffix — e.g. the `<out>.d`
// depfile tblgen emits — re-anchor for free since the out substring
// they contain is rewritten in place.
func anchorGenruleOutputsToRuledir(cmd string, outs []string) string {
	if cmd == "" || len(outs) == 0 {
		return cmd
	}
	sorted := append([]string(nil), outs...)
	sort.Slice(sorted, func(i, j int) bool { return len(sorted[i]) > len(sorted[j]) })
	for _, o := range sorted {
		if o == "" {
			continue
		}
		anchored := "$(RULEDIR)/" + o
		if strings.Contains(cmd, anchored) {
			continue
		}
		cmd = strings.ReplaceAll(cmd, o, anchored)
	}
	return cmd
}

// recordCodegenIncludeClosure appends the transitive `include "..."`
// closure of each include-resolving codegen genrule's primary input to
// its srcs — the tablegen shape: a tool that reads `-I <dir>` roots and
// resolves `include "x.td"` directives against them.
//
// cmake tracks that transitive closure via a per-output DEPFILE, which is
// dynamic: absent from a configure-only reply's static ninja inputs AND
// from the trace. (LLVM's tablegen() macro uses the depfile under Ninja —
// "Use depfile instead of globbing arbitrary *.td(s) for Ninja" — and only
// falls back to `file(GLOB)` for non-depfile generators, so neither the
// precise set nor a glob call ever reaches us.) So the lowered genrule
// lists only the explicit primary input (RISCV.td); without the rest of
// the `.td` closure the tool fails at action time with "could not find
// include file ...".
//
// We replicate the depfile statically: from the primary input, follow
// `include "..."` directives, resolving each against the genrule's own
// `-I` roots (in order, first existing match wins), and add every
// reachable source file to srcs. This is the precise set the depfile would
// list — minimal and faithful, not the coarse `glob(**/*.td)` over-
// approximation cmake falls back to for non-Ninja generators. split's
// cross-package src handling relabels each closure file to its owning
// package and raises the exports_files() need automatically.
//
// labelRoot is the absolute path the genrule's (umbrella-anchored) srcs
// and `-I` paths are relative to; "" disables the pass (unit tests,
// non-promoted offline replays without a source tree on disk). An include
// that doesn't resolve to a file on the source FS (e.g. a generated `.td`)
// contributes no further edges — the same blind spot cmake's glob fallback
// has.
//
// Scope guard: only genrules whose primary explicit source sits inside
// one of their own `-I` roots count as include-resolving codegen. A plain
// genrule that passes `-I` for a compiler invocation doesn't match — its
// input isn't resolved via the include path — so it's left untouched.
func recordCodegenIncludeClosure(targets []ir.Target, labelRoot string) {
	if labelRoot == "" {
		return
	}
	for i := range targets {
		t := &targets[i]
		if t.Kind != ir.KindGenrule || t.GenruleCmd == "" {
			continue
		}
		roots := genruleIncludeRoots(t.GenruleCmd)
		if len(roots) == 0 {
			continue
		}
		primary, ext := primaryCodegenInput(t.Srcs, roots)
		if ext == "" {
			continue
		}
		existing := make(map[string]bool, len(t.Srcs))
		for _, s := range t.Srcs {
			existing[s] = true
		}
		var add []string
		for _, c := range tdIncludeClosure(primary, roots, labelRoot) {
			if !existing[c] {
				existing[c] = true
				add = append(add, c)
			}
		}
		sort.Strings(add)
		t.Srcs = append(t.Srcs, add...)
	}
}

// tdIncludeClosure returns the set of labelRoot-relative source files
// reachable from primary by transitively following `include "..."`
// directives, resolving each path against roots (first existing match
// wins). primary is always the first element. A file that can't be read
// (generated / off-tree) terminates that branch; cycles are bounded by the
// visited set.
func tdIncludeClosure(primary string, roots []string, labelRoot string) []string {
	seen := map[string]bool{}
	var order []string
	var visit func(rel string)
	visit = func(rel string) {
		if seen[rel] {
			return
		}
		seen[rel] = true
		order = append(order, rel)
		data, err := os.ReadFile(filepath.Join(labelRoot, filepath.FromSlash(rel)))
		if err != nil {
			return
		}
		for _, inc := range parseTdIncludes(data) {
			if r := resolveTdInclude(inc, roots, labelRoot); r != "" {
				visit(r)
			}
		}
	}
	visit(primary)
	return order
}

// tdIncludeRe matches a tablegen `include "path"` directive at the start
// of a line (after optional leading whitespace) — the uniform form across
// LLVM's `.td` tree.
var tdIncludeRe = regexp.MustCompile(`(?m)^[ \t]*include[ \t]+"([^"]+)"`)

// parseTdIncludes extracts the include paths from a `.td` file body.
func parseTdIncludes(data []byte) []string {
	ms := tdIncludeRe.FindAllSubmatch(data, -1)
	out := make([]string, 0, len(ms))
	for _, m := range ms {
		out = append(out, string(m[1]))
	}
	return out
}

// resolveTdInclude resolves an include path against the `-I` roots in
// order, returning the first labelRoot-relative path that names an
// existing file, or "" if none do.
func resolveTdInclude(inc string, roots []string, labelRoot string) string {
	inc = strings.TrimLeft(filepath.ToSlash(inc), "/")
	for _, r := range roots {
		cand := strings.TrimRight(r, "/") + "/" + inc
		if fi, err := os.Stat(filepath.Join(labelRoot, filepath.FromSlash(cand))); err == nil && !fi.IsDir() {
			return cand
		}
	}
	return ""
}

// genruleIncludeRoots extracts the `-I` include roots from a genrule cmd,
// handling both `-I dir` (separate token) and `-Idir` (joined) forms.
// Roots are returned slash-form with surrounding quotes and trailing
// slashes trimmed, deduped in first-seen order.
func genruleIncludeRoots(cmd string) []string {
	fields := strings.Fields(cmd)
	var roots []string
	seen := map[string]bool{}
	add := func(r string) {
		r = strings.TrimRight(filepath.ToSlash(strings.Trim(r, `"'`)), "/")
		if r == "" || seen[r] {
			return
		}
		seen[r] = true
		roots = append(roots, r)
	}
	for i := 0; i < len(fields); i++ {
		f := fields[i]
		switch {
		case f == "-I" && i+1 < len(fields):
			add(fields[i+1])
			i++
		case strings.HasPrefix(f, "-I") && len(f) > 2:
			add(f[2:])
		}
	}
	return roots
}

// primaryCodegenInput returns the first src whose directory is one of the
// genrule's own `-I` roots, plus that src's extension. That coincidence —
// the input lives under an include root the same tool searches — is the
// signal that the genrule resolves includes (tablegen-shaped) rather than
// merely passing `-I` to a compiler. srcs are still relative (umbrella-
// anchored) at this lower-time stage; cross-package labels come later.
func primaryCodegenInput(srcs, roots []string) (string, string) {
	rootSet := make(map[string]bool, len(roots))
	for _, r := range roots {
		rootSet[r] = true
	}
	for _, s := range srcs {
		if strings.HasPrefix(s, "//") || strings.HasPrefix(s, "@") || strings.HasPrefix(s, "$") {
			continue // external / label / make-var srcs aren't FS inputs
		}
		dir := strings.TrimRight(filepath.ToSlash(filepath.Dir(s)), "/")
		if rootSet[dir] {
			return s, filepath.Ext(s)
		}
	}
	return "", ""
}

// pickStandaloneName returns the source-level add_custom_target
// name that wraps any of `outs` (when one exists), or falls back
// to the legacy `custom_command_<sanitized first output>` shape.
// outs is the already-sorted, deduped list from the build edge.
func pickStandaloneName(outs []string, outputToTargetName map[string]string) string {
	for _, o := range outs {
		if name, ok := outputToTargetName[o]; ok && name != "" {
			return name
		}
	}
	return "custom_command_" + sanitizeOutputName(outs[0])
}

// hasDownstreamConsumer reports whether any of `outs` or the
// emitted `baseName` appears in the consumedOutputs index. The
// index is built from add_dependencies calls (target → declared
// deps); when a dep name matches an output path OR matches the
// wrapping target's name, that's the signal the genrule has at
// least one same-package consumer that needs to see it.
func hasDownstreamConsumer(outs []string, baseName string, consumedOutputs map[string]bool) bool {
	if len(consumedOutputs) == 0 {
		return false
	}
	if baseName != "" && consumedOutputs[baseName] {
		return true
	}
	for _, o := range outs {
		if consumedOutputs[o] {
			return true
		}
	}
	return false
}

// buildOutputToCustomTargetIndex maps an OUTPUT path → the name of
// the add_custom_target that wraps it via DEPENDS / BYPRODUCTS /
// SOURCES. The wrap relationship comes from either:
//
//  1. add_custom_target(name DEPENDS out.h ...) directly listing
//     the output path in DEPENDS.
//  2. add_custom_target(name DEPENDS producer-name ...) listing
//     a producer-name where producer-name corresponds to no
//     other defined target — in which case it's treated as a
//     pure custom-target wrapping the producer's OUTPUTs
//     (the common idiom: a custom-target's only job is to
//     trigger a custom-command).
//  3. add_custom_target(name BYPRODUCTS out.h ...) listing the
//     output as a byproduct.
//  4. add_custom_target(name COMMAND ... output.h) — when the
//     target's COMMAND produces a known add_custom_command
//     OUTPUT, that's a wrap too. (Less common; the DEPENDS
//     shape covers most real cases.)
//
// Returns nil when traceCtx carries no records; callers handle
// nil-map reads gracefully.
//
// Tie-breaking: when two add_custom_target calls would name the
// same output, first-write-wins. The trace's recording order
// matches the CMakeLists.txt evaluation order; the first target
// to "claim" an output is the user's primary intent.
func buildOutputToCustomTargetIndex(commands []shadow.AddCustomCommandCall, targets []shadow.AddCustomTargetCall) map[string]string {
	if len(commands) == 0 && len(targets) == 0 {
		return nil
	}
	// First: map OUTPUT → the add_custom_command that produced
	// it. Used to resolve add_custom_target(... DEPENDS
	// producer-name) where producer-name is the OUTPUT of a
	// distinct add_custom_command (the case where the trace's
	// add_custom_command and add_custom_target are coupled but
	// the command's OUTPUT isn't directly listed in the target's
	// DEPENDS).
	outputCommandOwners := map[string]bool{}
	for _, c := range commands {
		for _, o := range c.Outputs {
			outputCommandOwners[o] = true
		}
		for _, o := range c.ByProducts {
			outputCommandOwners[o] = true
		}
	}

	idx := map[string]string{}
	claim := func(out, name string) {
		if out == "" || name == "" {
			return
		}
		if _, used := idx[out]; used {
			return // first-write-wins
		}
		idx[out] = name
	}

	for _, t := range targets {
		// Outputs listed in DEPENDS (direct path) or BYPRODUCTS
		// or SOURCES — claim them all under the target's name.
		for _, d := range t.Depends {
			if outputCommandOwners[d] {
				claim(d, t.Name)
			}
		}
		for _, b := range t.ByProducts {
			claim(b, t.Name)
		}
	}
	return idx
}

// buildConsumedOutputIndex returns the set of OUTPUT paths AND
// custom-target names that another in-trace add_dependencies call
// references. A non-empty entry signals "this output has a
// downstream consumer in the same package" → the emitted genrule
// needs visibility ≥ `:__pkg__`.
//
// The set is keyed by string (output path OR target name) so the
// caller can look up either shape. Returns nil for empty trace
// records.
func buildConsumedOutputIndex(commands []shadow.AddCustomCommandCall, targets []shadow.AddCustomTargetCall, deps []shadow.AddDependenciesCall) map[string]bool {
	if len(deps) == 0 {
		return nil
	}
	// Pre-build the OUTPUT → target-name map so an
	// add_dependencies(consumer foo) can resolve `foo` to the
	// target-name that owns OUTPUT foo — but more commonly,
	// `foo` IS the target name and the resolution is identity.
	out := map[string]bool{}
	for _, d := range deps {
		for _, name := range d.Depends {
			out[name] = true
		}
	}
	// add_dependencies dep entries name targets (typically
	// custom-target names), not output paths. The cross-
	// reference works on either: if the standalone genrule's
	// chosen name lands in the consumed-set, that's a hit; if
	// any OUTPUT path itself appears in an add_dependencies dep
	// list, that's also a hit (uncommon but legal — cmake
	// accepts add_dependencies(t out.h) when out.h is a
	// custom-target's output).
	//
	// Mention `commands` and `targets` to keep the contract
	// explicit: the consumer set is keyed only by name strings
	// drawn from add_dependencies; the commands/targets slices
	// don't expand the set. If a future heuristic needs to
	// expand consumers via OUTPUT→producer chains, the
	// expansion lives here.
	_ = commands
	_ = targets
	return out
}

// coveredOuts collects every output path that an existing IR
// output-producing target already declares. Used to dedup
// standalone-edge emission against the recoverGenrule / bake paths.
// Must cover every rule kind that produces a file at a known path:
// genrules (GenruleOuts), write_file bakes (WriteFileOut), AND
// cmake_configure_file lifts (CMakeConfigureFile.Out) — the bakes are
// what the file(GENERATE) / configure_file / cmake-script paths emit for
// \n-text bodies and the lifts are their re-rendering counterpart, and a
// produced output (e.g. libpng's pnglibconf.c) is frequently also a ninja
// CUSTOM_COMMAND edge, so missing any of them here would re-emit a second
// producer and Bazel rejects the duplicate generated file.
func coveredOuts(existing []ir.Target) map[string]bool {
	covered := map[string]bool{}
	for _, t := range existing {
		switch t.Kind {
		case ir.KindGenrule:
			for _, o := range t.GenruleOuts {
				covered[o] = true
			}
		case ir.KindWriteFile:
			if t.WriteFileOut != "" {
				covered[t.WriteFileOut] = true
			}
		case ir.KindCMakeConfigureFile:
			// The lift tier (configure_file / file(GENERATE) /
			// cmake -E configure_file re-render) produces its output via
			// CMakeConfigureFile.Out, not GenruleOuts / WriteFileOut. A
			// lifted output can also be a ninja CUSTOM_COMMAND edge, so —
			// exactly as for the write_file bake — missing it here would
			// re-emit a second producer and Bazel rejects the duplicate.
			if t.CMakeConfigureFile != nil && t.CMakeConfigureFile.Out != "" {
				covered[t.CMakeConfigureFile.Out] = true
			}
		}
	}
	return covered
}

// edgeCovered reports whether ANY of the build edge's outputs are
// already covered by an existing genrule. Conservative dedup:
// even one overlap is enough to skip, on the theory that the
// existing genrule was emitted for a good reason and emitting a
// second one for the same producer would double-build.
func edgeCovered(b *ninja.Build, covered map[string]bool) bool {
	for _, o := range b.Outputs {
		if covered[o] {
			return true
		}
	}
	for _, o := range b.ImplicitOuts {
		if covered[o] {
			return true
		}
	}
	return false
}

// filterOutVarRefs returns xs with any entry containing an
// unexpanded ninja variable reference (substring `${`) removed.
// cmake's Ninja generator pairs every real CUSTOM_COMMAND output
// with a `${cmake_ninja_workdir}<basename>` implicit-output
// shadow used for restat tracking — those shadow paths are
// ninja-internal and can't be emitted as Bazel genrule outs.
//
// Note: also drops outputs that use any other unresolved
// ninja-var reference — none of those have a defined meaning at
// Bazel emission time either.
func filterOutVarRefs(xs []string) []string {
	out := xs[:0]
	for _, x := range xs {
		if strings.Contains(x, "${") {
			continue
		}
		out = append(out, x)
	}
	return out
}

// isCMakeBookkeepingOutput reports whether a build-edge output
// path is one of cmake's internal IDE / regen / packaging /
// install utility outputs. cmake's Ninja generator emits a
// standalone CUSTOM_COMMAND for each of edit_cache /
// rebuild_cache (always present), plus a handful more under
// multi-target generators (install / package / package_source /
// test / list_install_components).
//
// Shapes observed:
//
//   - Single-config (Ninja): `CMakeFiles/<name>.util`.
//   - Multi-config (Ninja Multi-Config): `<subdir>/CMakeFiles/
//     <Config>/<name>.util` per CMAKE_CONFIGURATION_TYPES entry
//     and per subdirectory cmake recursed through.
//
// Both shapes share `CMakeFiles/` as a path component and end
// in `.util`; checking that pair is both necessary and sufficient
// because cmake reserves the `.util` extension for these
// bookkeeping edges (no user-declared add_custom_command lands
// an output with that extension).
func isCMakeBookkeepingOutput(p string) bool {
	// `.util`-extension shape (single-config + multi-config Ninja
	// generators).
	if strings.HasSuffix(p, ".util") {
		if strings.HasPrefix(p, "CMakeFiles/") || strings.Contains(p, "/CMakeFiles/") {
			return true
		}
	}
	// `CMakeFiles/check-<name>` shape — cmake's add_custom_target
	// for project test runners (LLVM's `check-all`, `check-llvm`,
	// `check-mlgo-utils`, etc., plus a per-leaf-test variant under
	// each subdirectory). These emit
	// `add_custom_target(check-<name> COMMAND llvm-lit -sv test/<name>)`,
	// which Bazel users replace with `cc_test` rules; converting them
	// to genrules emits broken cmds (the test runner is
	// configure_file-generated, not a build target). Same
	// `CMakeFiles/` path-component check as the .util shape keeps
	// the filter from misfiring on user directories.
	if strings.HasPrefix(p, "CMakeFiles/check-") {
		return true
	}
	if i := strings.Index(p, "/CMakeFiles/check-"); i >= 0 {
		return true
	}
	return false
}

// isCMakeInternalCmd reports whether a recovered ninja
// CUSTOM_COMMAND cmd belongs to cmake's own install / regen / cpack
// bookkeeping infrastructure and should be filtered from the
// rendered BUILD. These edges invoke cmake / cpack / rpmbuild
// against cmake's build-dir state at action time, which doesn't
// translate to Bazel's hermetic sandbox — they exist purely so
// `ninja install` / `ninja package` work in the cmake build dir
// and serve no purpose in the converted Bazel graph.
//
// Matched cmd shapes (after the rewriteGenruleCmd normalisation
// has stripped the `cd <abs> && ` preamble + `/usr/bin/` prefix):
//
//   - `cmake ... cmake_install.cmake` (any DCMAKE_INSTALL_*
//     env-shape variant — component / strip / local-only).
//   - `cmake --regenerate-during-build ...` (CMakeFiles regen
//     hook).
//   - `cpack ...` / `cpack ... && rpmbuild ...` (package
//     distribution edges).
//   - `echo No interactive CMake dialog available.` (IDE stub
//     edges whose CMakeFiles/ output path the .util filter misses
//     when they appear under a nested subdir).
//
// Predicate does its own normalisation pass on the raw ninja cmd
// (cd-strip + host-bin strip) so it matches BEFORE the caller
// runs rewriteGenruleCmd — the caller checks the predicate
// before the rewrite for efficiency.
//
// The cmake-cmd check requires the install-script path
// (`cmake_install.cmake`) rather than just `-P` so user-written
// `add_custom_command(... cmake -P myscript.cmake)` shapes aren't
// caught — those route through the operator-staged runner path
// (CMakeScriptRunner) or refuse with UnsupportedCustomCommandScript.
func isCMakeInternalCmd(cmd string) bool {
	c := strings.TrimSpace(cmd)
	// Strip a leading `cd <abs> && ` preamble (cmake-Ninja's
	// per-target build-subdir cd, present on the raw cmd).
	if strings.HasPrefix(c, "cd ") {
		if i := strings.Index(c, " && "); i > 0 {
			c = strings.TrimSpace(c[i+4:])
		}
	}
	// Strip host-tool prefix on the leading command token.
	for _, p := range []string{"/usr/bin/", "/usr/local/bin/", "/usr/sbin/"} {
		if strings.HasPrefix(c, p) {
			c = c[len(p):]
			break
		}
	}
	// cmake_install.cmake invocations — `cmake ... cmake_install.cmake`
	// (the -P arg may carry a relative or absolute path; check any
	// occurrence of the script-name token).
	if strings.HasPrefix(c, "cmake ") &&
		strings.Contains(c, "cmake_install.cmake") {
		return true
	}
	// `--regenerate-during-build` — cmake's CMakeFiles regen hook.
	if strings.HasPrefix(c, "cmake ") &&
		strings.Contains(c, "--regenerate-during-build") {
		return true
	}
	// cpack + cpack-with-rpmbuild — package distribution.
	if strings.HasPrefix(c, "cpack ") || strings.HasPrefix(c, "cpack-") {
		return true
	}
	// `ctest -D <Dashboard>` — CDash dashboard submission edges
	// (Experimental / Nightly / Continuous + Memory/Coverage
	// variants). cmake's CTest module emits an
	// add_custom_target(<Dashboard> COMMAND ctest -D <Dashboard>)
	// per dashboard mode when enable_testing() is called; these
	// invoke ctest's dashboard-submission mode against an external
	// CDash server, which has no Bazel sandbox analogue. The
	// in-tree test running covered by add_test() already routes
	// through CTest classification (lower/ctest) into cc_test
	// rules; the dashboard targets are orthogonal infrastructure
	// for `ninja Experimental` etc.
	//
	// Match the dashboard arg anywhere in the cmd, not just at a
	// `ctest ` prefix — mbedtls wraps the call in a
	// sed/ctest/tail/rm pipeline (the memcheck target). The
	// `-D Experimental` / `Nightly` / `Continuous` substring is
	// stable across all wrapper shapes and only appears in
	// cmake's auto-generated dashboard cmds (user-written ctest
	// invocations don't use the `-D <Dashboard>` arg form).
	if strings.Contains(c, "-D Experimental") ||
		strings.Contains(c, "-D Nightly") ||
		strings.Contains(c, "-D Continuous") {
		return true
	}
	// `cmake -E echo No interactive CMake dialog available.` IDE
	// stubs that the `.util`-suffix filter misses when the output
	// lands under a nested CMakeFiles/ subdir.
	if strings.HasPrefix(c, "echo No interactive CMake dialog") ||
		strings.HasPrefix(c, "echo No\\ interactive\\ CMake\\ dialog") {
		return true
	}
	return false
}

// sanitizeOutputName converts a path like `gen/version.h` into a
// Bazel target-name-safe stem like `gen_version_h`. Mirrors the
// directory-installer sanitizer but tuned for path-with-extension
// shapes (preserves the `.h` / `.cc` suffix as `_h` / `_cc`).
func sanitizeOutputName(p string) string {
	clean := filepath.ToSlash(filepath.Clean(p))
	var sb strings.Builder
	sb.Grow(len(clean))
	lastWasUnderscore := false
	for _, r := range clean {
		isAlnum := (r >= 'a' && r <= 'z') ||
			(r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9')
		if isAlnum {
			sb.WriteRune(r)
			lastWasUnderscore = false
			continue
		}
		if !lastWasUnderscore {
			sb.WriteRune('_')
			lastWasUnderscore = true
		}
	}
	return strings.Trim(sb.String(), "_")
}

// dedupSorted removes consecutive duplicates from a sorted slice.
// More efficient than a map-based dedup for small slices (typical
// build edge has 1-2 outputs).
func dedupSorted(xs []string) []string {
	if len(xs) <= 1 {
		return xs
	}
	out := xs[:1]
	for _, x := range xs[1:] {
		if x != out[len(out)-1] {
			out = append(out, x)
		}
	}
	return out
}

// intToStr is a tiny non-fmt itoa for the name-collision suffix.
// Avoids pulling fmt into the per-edge loop's hot path.
func intToStr(n int) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}
