package lower

import (
	"io/fs"
	"os"
	"path"
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

	// FileGlobs carries the user's source-level file(GLOB)/file(GLOB_RECURSE)
	// calls. threadFileGlobs matches each glob's result set against the
	// standalone genrules' srcs: when a genrule depends on exactly a glob's
	// output, those srcs are folded into a build-time glob() filegroup
	// (split-synthesized) so the glob is preserved in project B. Empty when
	// the project uses no file(GLOB).
	FileGlobs []shadow.FileGlobCall

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
// filteredInternal (when non-nil) collects the cmake-internal command edges
// this pass drops — keyed by the edge's first output (or category when it has
// none), valued by category (install / regen / cpack / dashboard / ide-stub)
// — so the caller can surface an audit breadcrumb instead of dropping silently.
// customTargetStampIsNonAll reports whether any of a genrule's outputs is
// the stamp file of an add_custom_target that was declared WITHOUT `ALL`.
// cmake places a custom target's stamp at `<dir>/CMakeFiles/<name>` (Ninja
// Multi-Config appends a `-<config>` suffix). The check is gated on the
// derived name being a KNOWN custom target in allByName, so an ordinary
// genrule that merely happens to write under a CMakeFiles/ path is never
// mis-tagged. Returns false on an empty map (no trace).
func customTargetStampIsNonAll(outs []string, allByName map[string]bool) bool {
	if len(allByName) == 0 {
		return false
	}
	for _, o := range outs {
		idx := strings.LastIndex(o, "CMakeFiles/")
		if idx < 0 {
			continue
		}
		base := o[idx+len("CMakeFiles/"):]
		if strings.Contains(base, "/") {
			continue // a deeper file under CMakeFiles/, not a target stamp
		}
		if all, ok := allByName[base]; ok {
			if !all {
				return true
			}
			continue
		}
		for _, cfg := range []string{"Debug", "Release", "RelWithDebInfo", "MinSizeRel"} {
			if n, found := strings.CutSuffix(base, "-"+cfg); found {
				if all, ok := allByName[n]; ok && !all {
					return true
				}
			}
		}
	}
	return false
}

func lowerStandaloneCustomCommands(g *ninja.Graph, existing []ir.Target, cmakeSrc, buildDir, umbrellaPrefix, bazelPackagePath string, artifactToName map[string]string, traceCtx standaloneTraceContext, filteredInternal map[string]string, cc *codegenContext) []ir.Target {
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
	// add_custom_target name → ALL flag. A custom target declared
	// WITHOUT `ALL` is not part of cmake's default build (you invoke it
	// explicitly, `make <name>`); the faithful Bazel analog is a
	// `manual`-tagged rule, excluded from `bazel build //...` wildcard
	// expansion. This keeps phony run-targets — curl's `test-ci` /
	// `test-am` runtests.pl harness wrappers, whose cmds carry shell
	// `$TFLAGS` and produce no real file — out of the default build
	// instead of breaking its analysis. Empty when the trace has no
	// add_custom_target records (offline-replay-no-trace path).
	customTargetAll := map[string]bool{}
	for _, ct := range traceCtx.CustomTargets {
		customTargetAll[ct.Name] = ct.All
	}

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
		// Recover the genrule srcs up front (re-anchors source-tree-absolute
		// inputs to workspace-relative form), then drop the edges with no
		// faithful Bazel build-graph form — cmake bookkeeping, install/regen/
		// cpack, create_symlink, a source-less copy, make_directory — recording
		// a breadcrumb for each categorized drop. See standaloneEdgeFiltered.
		srcs := genruleSrcs(b, cmakeSrc, buildDir, umbrellaPrefix)
		if standaloneEdgeFiltered(cmd, outs, srcs, filteredInternal) {
			continue
		}

		// IN-SOURCE generation: the declared OUTPUT lands in the SOURCE tree via a
		// source-dir `cd <srcdir> && <tool>` WORKING_DIRECTORY shape (libevent's
		// event_rpcgen.py → test/regress.gen.{c,h}). Handled by reconstructing the
		// working dir in a mktemp scratch dir — see tryInSourceWorkdirGenrule.
		if t, ok := tryInSourceWorkdirGenrule(b, cmd, srcs, outs, cmakeSrc, buildDir, umbrellaPrefix, bazelPackagePath, cc); ok {
			out = append(out, t)
			continue
		}

		// A standalone `cmake -P <script>` custom command can't run under Bazel
		// (no cmake on the executor), so emitting it as a raw genrule produces
		// an unrunnable rule (LLVM's VCSRevision.h: `cmake -P
		// GenerateVersionFromVCS.cmake`). The ninja-genrule path
		// (recoverGenrule) already routes cmake -P through the bake/lift logic;
		// mirror that here so the standalone path bakes too. Gated on
		// cc.CMakeScriptBake (opt-in --cmake-script-bake) — off by default, so
		// every project that doesn't opt in keeps the exact prior behavior.
		// bakeCmakeScriptGenrule runs the script at convert time and appends the
		// captured-bytes write_file(s) to cc.Genrules + cc.OutToGenrule; move
		// those into this pass's return slice (cc.Genrules was already merged
		// into pkg.Targets before this pass runs) and skip the raw emit.
		if cc != nil && cc.CMakeScriptBake && usesCmakeScriptMode(cmd) {
			n := len(cc.Genrules)
			if _, _, _, ok := bakeCmakeScriptGenrule(cc, b, cmd, extractCmakeScriptPath(cmd), buildDir, g); ok {
				out = append(out, cc.Genrules[n:]...)
				cc.Genrules = cc.Genrules[:n]
				continue
			}
			// Bake declined (e.g. no cmake on PATH, script produced no output):
			// fall through to the existing raw-genrule emit / refusal path.
		}

		// In-place rewrite remediation: a custom command that reads a
		// source-tree file and writes the SAME relative path into the
		// build tree (LLVM's Remarks.exports shape — `> ${target}.exports`
		// where the source symbol file is also named `${target}.exports`)
		// produces a genrule with that path as BOTH an srcs entry and an
		// outs entry, which Bazel rejects ("file X as both an input and an
		// output"). Detect the collision (an output whose source-tree form
		// is also a src) and rename the colliding OUTPUT to a non-shadowing
		// `.gen` sibling. The rename is two-stage:
		//
		//  1. RAW cmd, while input and output are still distinguishable:
		//     the source operand carries the absolute SOURCE-dir prefix,
		//     the output is `<buildDir>/<o>` or a cwd-relative `<o>` (cmake
		//     runs the recipe cd'd into the binary dir). Renaming here
		//     (renameInPlaceOutputsRawCmd) is what keeps the SOURCE-input
		//     occurrence safe when both collapse to the same workspace-
		//     relative token after rewriteGenruleCmd — without it,
		//     anchorGenruleOutputsToRuledir anchors the input too and the
		//     cmd degenerates into a self-copy of the renamed output
		//     (`cp $(RULEDIR)/x.gen $(RULEDIR)/x.gen`).
		//  2. Post-anchor (renameAnchoredGenruleOutputs below): catches
		//     output forms the raw pass can't key on — e.g. a bare BASENAME
		//     redirect of a subdir output, which only converges with the
		//     declared out at its `$(RULEDIR)/<out>` anchored form. A
		//     no-op for occurrences stage 1 already renamed.
		//
		// outs are renamed HERE (before naming/anchoring) so the rule name
		// derives from the renamed output and the anchor pass targets the
		// renamed token only; the trace-driven name/consumer indexes are
		// aliased to the renamed path so custom-target wrapping and
		// add_dependencies visibility still resolve.
		inPlaceRenames := detectInPlaceOutputRenames(outs, srcs, umbrellaPrefix)
		cmd, outs = applyInPlaceRenames(cmd, outs, inPlaceRenames, buildDir, outputToTargetName, consumedOutputs)

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
		rewrittenCmd := rewriteGenruleCmd(cmd, cmakeSrc, buildDir, umbrellaPrefix, bazelPackagePath)
		var execArtifacts map[string]bool
		if cc != nil {
			execArtifacts = cc.ExecArtifacts
		}
		rewrittenCmd, tools := rewriteToolFromTarget(rewrittenCmd, artifactToName, execArtifacts)
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
		// A non-ALL add_custom_target's stamp → `manual` (not in the
		// default build; see customTargetAll above).
		if customTargetStampIsNonAll(outs, customTargetAll) {
			tags = append(tags, "manual")
		}
		if len(inPlaceRenames) > 0 {
			// Stage 2 of the in-place-output rename (stage 1 ran on the raw
			// cmd above, where input and output were still distinguishable):
			// catch any colliding-output occurrence the raw pass couldn't
			// key on — those converge with the declared out only at their
			// anchored $(RULEDIR)/<out> form. The source-tree input
			// occurrence (not $(RULEDIR)-prefixed) is left alone, so the
			// genrule reads the source and writes the renamed,
			// non-shadowing output. outs were renamed at detection time.
			rewrittenCmd = renameAnchoredGenruleOutputs(rewrittenCmd, inPlaceRenames)
			tags = append(tags, "cmake-codegen-genrule-inplace-rewrite")
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

// standaloneEdgeFiltered reports whether a custom-command edge should be
// DROPPED rather than lifted to a genrule, recording a breadcrumb in
// filteredInternal for each categorized drop (the cmake-bookkeeping `.util`
// skip is silent — it carries no operator-visible intent). The drops, each with
// no faithful Bazel build-graph form (see the inline rationale at the former
// call sites): cmake-internal bookkeeping outputs; install / regen / cpack
// edges; `cmake -E create_symlink`; a source-less `cmake -E copy`; and a
// make_directory-only command.
func standaloneEdgeFiltered(cmd string, outs, srcs []string, filteredInternal map[string]string) bool {
	record := func(kind string) {
		if filteredInternal == nil {
			return
		}
		key := kind
		if len(outs) > 0 {
			key = outs[0]
		}
		filteredInternal[key] = kind
	}
	if isCMakeBookkeepingOutput(outs[0]) {
		return true // cmake IDE/regen bookkeeping (.util) — silent drop
	}
	// cmakeInternalCmdKind does non-trivial string normalization; compute once.
	if kind := cmakeInternalCmdKind(cmd); kind != "" {
		record(kind)
		return true
	}
	switch {
	case isCreateSymlinkCmd(cmd):
		record("symlink")
		return true
	case len(srcs) == 0 && isCopyCmd(cmd):
		record("copy")
		return true
	case isMakeDirOnlyCmd(cmd):
		record("make_directory")
		return true
	}
	return false
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

// inSourceOutputs reports whether EVERY output is an absolute path under
// cmakeSrc (i.e. the custom command writes into the SOURCE tree — in-source
// generation), returning the element-root-relative forms. A mixed edge (some
// outputs in the build dir, some in the source tree) or any non-source output
// returns ok=false so the caller keeps the normal build-dir-output path.
func inSourceOutputs(outs []string, cmakeSrc string) (rel []string, ok bool) {
	if cmakeSrc == "" || len(outs) == 0 {
		return nil, false
	}
	for _, o := range outs {
		if !filepath.IsAbs(o) {
			return nil, false
		}
		r, inside := relativeIfInside(cmakeSrc, o)
		if !inside {
			return nil, false
		}
		rel = append(rel, filepath.ToSlash(r))
	}
	return rel, true
}

// tryInSourceWorkdirGenrule emits the genrule for an in-source WORKING_DIRECTORY
// custom command when the shape applies: every output is in the source tree
// (inSourceOutputs) and the command has a recoverable `cd <srcdir>` working dir.
// It registers the outputs in cc.OutToGenrule and returns the genrule; ok=false
// leaves the caller's normal build-dir-output path. See buildInSourceWorkdirGenrule
// for the scratch-dir mechanism. Works under both the monolithic and split emits
// — the cmd's $(execpath <src>) / $(RULEDIR)/<out> refs are re-relativized by
// split's relocateGenruleSrcs / relocateGenruleOuts on a package re-home.
func tryInSourceWorkdirGenrule(b *ninja.Build, cmd string, srcs, outs []string, cmakeSrc, buildDir, umbrellaPrefix, bazelPackagePath string, cc *codegenContext) (ir.Target, bool) {
	if cc == nil {
		return ir.Target{}, false
	}
	relOuts, inSrc := inSourceOutputs(outs, cmakeSrc)
	if !inSrc {
		return ir.Target{}, false
	}
	wdAbs := extractCdDir(cmd)
	if wdAbs == "" {
		return ir.Target{}, false
	}
	wd, inside := relativeIfInside(cmakeSrc, wdAbs)
	if !inside {
		return ir.Target{}, false
	}
	body := rewriteGenruleCmd(cmd, cmakeSrc, buildDir, umbrellaPrefix, bazelPackagePath)
	// Name from the SOURCE-relative output, not genruleNameFor(b, buildDir):
	// genruleNameFor relativizes against the BUILD dir, but an in-source output
	// lives under cmakeSrc, so it would fall through verbatim and bake the
	// absolute SOURCE path into the rule name (`gen__tmp_libevent_test_…`) —
	// non-deterministic across checkout locations (caught by the source-narrowing
	// lens: the name differed between the real tree and a relocated copy). relOuts
	// is already source-root-relative and deterministically ordered (outs is
	// dedup-sorted upstream), so it yields a stable `gen_test_regress_gen_c`.
	name := "gen_" + sanitizePathToNameStem(relOuts[0])
	for _, o := range relOuts {
		cc.OutToGenrule[o] = name
	}
	return ir.Target{
		Name:        name,
		Kind:        ir.KindGenrule,
		Srcs:        srcs,
		GenruleOuts: relOuts,
		GenruleCmd:  buildInSourceWorkdirGenrule(body, filepath.ToSlash(wd), srcs, relOuts),
		Tags:        []string{"cmake-codegen-standalone-custom-command", "cmake-codegen-in-source-workdir"},
		Visibility:  []string{"//visibility:private"},
	}, true
}

// buildInSourceWorkdirGenrule builds the genrule cmd for an in-source
// WORKING_DIRECTORY custom command (see the call site). It reconstructs the
// command's working directory in a mktemp scratch dir — free of Bazel's
// rule-writes-only-within-its-package constraint — by materializing every
// declared input (srcs, referenced by $(execpath) so the labels survive a
// split package move) at its element-root-relative position, running the
// already-stripped command body verbatim from the scratch working dir (wd,
// element-root-relative), then copying each output to its $(RULEDIR)/<out>
// anchored path. The $(RULEDIR)/<out> form is exactly what split's
// relocateGenruleOuts re-relativizes when the genrule re-homes into its
// output's package, so the same cmd is correct under both the monolithic and
// --split-packages emits. outs are element-root-relative; an output's path
// under the scratch wd is its own element-relative path (the tool writes it at
// the wd-relative position, which equals <out> since <out> is under wd).
func buildInSourceWorkdirGenrule(body, wd string, srcs, outs []string) string {
	var b strings.Builder
	b.WriteString(`tmp="$$(mktemp -d)"`)
	for _, s := range srcs {
		if d := path.Dir(s); d != "." {
			b.WriteString(` && mkdir -p "$$tmp/` + d + `"`)
		}
		b.WriteString(` && cp "$(execpath ` + s + `)" "$$tmp/` + s + `"`)
	}
	b.WriteString(` && ( cd "$$tmp/` + wd + `" && ` + body + ` )`)
	for _, o := range outs {
		if d := path.Dir(o); d != "." {
			b.WriteString(` && mkdir -p "$(RULEDIR)/` + d + `"`)
		}
		b.WriteString(` && cp "$$tmp/` + o + `" "$(RULEDIR)/` + o + `"`)
	}
	return b.String()
}

// anchorGenruleOutputsToRuledir rewrites each build-dir-relative output
// path (and each output's multi-component parent dir) that appears
// literally in the cmd to $(RULEDIR)/<path>. cmake's Ninja generator bakes
// the output path as a build-dir-relative literal (e.g. `-o
// include/llvm/.../X.inc`, and a `make_directory include/llvm/...` for its
// parent); a Bazel genrule must instead write its declared outs — and mkdir
// their parents — under $(RULEDIR) (the package's bin dir). Tokens are
// rewritten longest-first so a shorter path that is a prefix of a longer
// one doesn't rewrite the longer one's interior, and a token already
// carrying the $(RULEDIR)/ prefix is skipped (idempotent).
// Sibling paths derived from an out by suffix — e.g. the `<out>.d`
// depfile tblgen emits — re-anchor for free since the out substring
// they contain is rewritten in place.
// detectInPlaceOutputRenames returns a map of build-dir-relative output
// path → renamed output path for each output that collides with a source
// (the in-place-rewrite shape: a custom command whose output path equals
// one of its source-tree inputs). outs are build-dir-relative; srcs are
// workspace-relative (umbrella-anchored), so an output `o` collides when
// its source-tree form — `o` itself, or `<umbrellaPrefix>/o` under
// umbrella promotion — appears in srcs. Returns nil when there's no
// collision (the overwhelming common case), so non-in-place genrules are
// untouched and byte-identical.
func detectInPlaceOutputRenames(outs, srcs []string, umbrellaPrefix string) map[string]string {
	if len(outs) == 0 || len(srcs) == 0 {
		return nil
	}
	srcSet := make(map[string]bool, len(srcs))
	for _, s := range srcs {
		srcSet[s] = true
	}
	var renames map[string]string
	for _, o := range outs {
		if o == "" {
			continue
		}
		srcForm := o
		if umbrellaPrefix != "" {
			srcForm = umbrellaPrefix + "/" + o
		}
		if srcSet[srcForm] {
			if renames == nil {
				renames = map[string]string{}
			}
			renames[o] = inPlaceRenamedOutput(o)
		}
	}
	return renames
}

// inPlaceRenamedOutput returns a non-shadowing sibling name for an
// in-place-rewrite output: the original path with a stable `.gen` suffix
// (`Remarks.exports` → `Remarks.exports.gen`). The output no longer
// collides with the same-named source, while the suffix keeps the name
// recognizable and deterministic across rebuilds.
func inPlaceRenamedOutput(o string) string {
	return o + ".gen"
}

// applyInPlaceRenames performs stage 1 of the in-place-output rename for
// one edge: the raw-cmd rewrite (renameInPlaceOutputsRawCmd), the outs
// rename, and the aliasing of the trace-driven name/consumer indexes to
// the renamed paths (so custom-target wrapping and add_dependencies
// visibility still resolve). No-op passthrough when there are no renames.
func applyInPlaceRenames(cmd string, outs []string, renames map[string]string, buildDir string, outputToTargetName map[string]string, consumedOutputs map[string]bool) (string, []string) {
	if len(renames) == 0 {
		return cmd, outs
	}
	cmd = renameInPlaceOutputsRawCmd(cmd, renames, buildDir)
	for i, o := range outs {
		r, hit := renames[o]
		if !hit {
			continue
		}
		outs[i] = r
		if tn, ok := outputToTargetName[o]; ok {
			outputToTargetName[r] = tn
		}
		if consumedOutputs[o] {
			consumedOutputs[r] = true
		}
	}
	return cmd, outs
}

// renameInPlaceOutputsRawCmd is stage 1 of the in-place-output rename: it
// runs on the RAW ninja cmd, where the colliding input and output are
// still distinguishable by FORM — the source operand carries the absolute
// SOURCE-dir prefix, while the output appears as `<buildDir>/<o>`
// (explicit-path shape: `cmake -E copy <srcdir>/x <buildDir>/x`) or as a
// cwd-relative `<o>` (cmake cd's the recipe into the binary dir, so a
// bare-token occurrence names the build file). Renaming the output here —
// before rewriteGenruleCmd collapses both operands to the same
// workspace-relative token — is what lets anchorGenruleOutputsToRuledir
// anchor ONLY the renamed output and leave the source input reading the
// source. Matches are guarded on BOTH token edges (renameTokenBoundary…)
// so the source operand's `<srcdir>/<o>` occurrence, prefix-sharing
// names (`my<o>`), and extended names (`<o>.bak`) are never touched —
// a mid-word rewrite here would be a silently corrupted cmd, not a loud
// failure.
func renameInPlaceOutputsRawCmd(cmd string, renames map[string]string, buildDir string) string {
	if cmd == "" || len(renames) == 0 {
		return cmd
	}
	type repl struct{ search, with string }
	var pairs []repl
	for o, renamed := range renames {
		if o == "" {
			continue
		}
		if buildDir != "" {
			pairs = append(pairs, repl{buildDir + "/" + o, buildDir + "/" + renamed})
		}
		pairs = append(pairs, repl{o, renamed})
	}
	sort.Slice(pairs, func(i, j int) bool { return len(pairs[i].search) > len(pairs[j].search) })
	var b strings.Builder
	for i := 0; i < len(cmd); {
		matched := false
		for _, p := range pairs {
			if !strings.HasPrefix(cmd[i:], p.search) {
				continue
			}
			if !renameTokenBoundaryLeft(cmd, i) || !renameTokenBoundaryRight(cmd, i+len(p.search)) {
				continue
			}
			b.WriteString(p.with)
			i += len(p.search)
			matched = true
			break
		}
		if !matched {
			b.WriteByte(cmd[i])
			i++
		}
	}
	return b.String()
}

// renameTokenBoundaryLeft reports whether position i opens a shell token:
// the string start or a separator byte. A preceding '/' (mid-path — the
// source operand), word byte (`my<o>`), or any other glue byte means the
// match sits inside a longer token and must not be rewritten.
func renameTokenBoundaryLeft(cmd string, i int) bool {
	if i == 0 {
		return true
	}
	switch cmd[i-1] {
	case ' ', '\t', '\'', '"', '=', '(', '<', '>', ';', '&', '|':
		return true
	}
	return false
}

// renameTokenBoundaryRight reports whether position end closes a shell
// token: the string end or a separator byte. A following extension byte
// (`<o>.bak`) or path byte (`<o>/…`) means the match is a prefix of a
// longer name and must not be rewritten.
func renameTokenBoundaryRight(cmd string, end int) bool {
	if end >= len(cmd) {
		return true
	}
	switch cmd[end] {
	case ' ', '\t', '\'', '"', ')', ';', '&', '|', '<', '>':
		return true
	}
	return false
}

// renameAnchoredGenruleOutputs rewrites each `$(RULEDIR)/<o>` occurrence in
// the ALREADY-ANCHORED cmd to `$(RULEDIR)/<renamed>`, renaming an in-place-
// rewrite output to its non-shadowing `.gen` sibling in lockstep with the
// outs declaration. It runs after anchorGenruleOutputsToRuledir so it keys on
// the single canonical `$(RULEDIR)/<out>` form every output shape converges
// on — the cmake bare-basename redirect (`> ${native_export_file}`, cd'd into
// the binary dir) and the buildDir-prefixed form alike. The source-tree input
// occurrence is NOT `$(RULEDIR)`-prefixed (it carries the element package
// path, e.g. `elements/llvm/tools/remarks-shlib/Remarks.exports`), so it is
// left untouched — the genrule reads the source and writes the renamed output.
//
// Pairs are applied longest-search-first in a single left-to-right pass so
// that overlapping outputs (`$(RULEDIR)/x` vs `$(RULEDIR)/x.inc`) and the
// search-is-a-prefix-of-its-replacement case (`$(RULEDIR)/x` →
// `$(RULEDIR)/x.gen`) can't re-match or mangle one another, independent of
// Go's randomized map iteration order. Each match must CLOSE a token
// (renameTokenBoundaryRight): an anchored token that merely starts with
// the output — `$(RULEDIR)/<o>.bak`, a depfile `$(RULEDIR)/<o>.d` — is a
// different file and renaming its prefix would silently corrupt the cmd.
func renameAnchoredGenruleOutputs(cmd string, renames map[string]string) string {
	if cmd == "" || len(renames) == 0 {
		return cmd
	}
	const rp = "$(RULEDIR)/"
	type repl struct{ search, with string }
	pairs := make([]repl, 0, 2*len(renames))
	for o, renamed := range renames {
		// Identity pair first (longer, so sorted ahead of the rename
		// pair): an occurrence stage 1 already renamed must not be
		// re-suffixed (`x.gen` → `x.gen.gen`) — this pass is the
		// second stage of the two-stage rename and must be idempotent.
		pairs = append(pairs, repl{rp + renamed, rp + renamed})
		pairs = append(pairs, repl{rp + o, rp + renamed})
	}
	sort.Slice(pairs, func(i, j int) bool { return len(pairs[i].search) > len(pairs[j].search) })
	var b strings.Builder
	for i := 0; i < len(cmd); {
		matched := false
		for _, p := range pairs {
			if strings.HasPrefix(cmd[i:], p.search) && renameTokenBoundaryRight(cmd, i+len(p.search)) {
				b.WriteString(p.with)
				i += len(p.search)
				matched = true
				break
			}
		}
		if !matched {
			b.WriteByte(cmd[i])
			i++
		}
	}
	return b.String()
}

// containsBoundaryToken reports whether tok occurs in cmd at a path-token
// boundary — its left edge is the string start or a non-'/' byte (so a
// substring sitting in the middle of a longer path, including an already
// `$(RULEDIR)/`-anchored occurrence, doesn't count). Mirrors the left-boundary
// guard the anchoring loop uses, so the suffix-fallback only fires when the
// token is a real standalone reference.
func containsBoundaryToken(cmd, tok string) bool {
	if tok == "" {
		return false
	}
	for i := 0; i+len(tok) <= len(cmd); i++ {
		if cmd[i:i+len(tok)] == tok && (i == 0 || cmd[i-1] != '/') {
			return true
		}
	}
	return false
}

func anchorGenruleOutputsToRuledir(cmd string, outs []string) string {
	if cmd == "" || len(outs) == 0 {
		return cmd
	}
	const rp = "$(RULEDIR)/"
	// Anchor the declared outputs AND each output's multi-component parent
	// directories. A `cmake -E make_directory <outdir>` / `mkdir -p <outdir>`
	// that creates an output's parent in cmake's build tree must target
	// $(RULEDIR)/<outdir> too — otherwise the genrule mkdir's a stray dir in
	// the sandbox cwd while the write to $(RULEDIR)/<outdir>/<file> fails on
	// the missing parent. Only parents containing a "/" are anchored: a
	// single-component parent ("gen") maps to the package's $(RULEDIR) root
	// post-split (no subdir, no mkdir needed) and is too short to substring-
	// match safely (it would corrupt a sibling like "gen.inc.in").
	tokenSet := map[string]bool{}
	for _, o := range outs {
		// Skip empty and ABSOLUTE outputs. A genrule output is $(RULEDIR)-
		// relative, so an absolute path (some of libevent's regress
		// add_custom_command OUTPUTs are absolute) is not anchorable here.
		// Processing it both hangs the parent walk (path.Dir("/") == "/" never
		// loses its slash, so it loops forever) AND would seed "/" + the absolute
		// path into tokenSet, corrupting the command (injecting "$(RULEDIR)/" at
		// every separator / producing "$(RULEDIR)//abs/..."). Excluding absolute
		// outputs makes the relative-only walk terminate naturally (a relative
		// path's parent reduces to "." / a slash-less component).
		if o == "" || path.IsAbs(o) {
			continue
		}
		tokenSet[o] = true
		for d := path.Dir(o); strings.Contains(d, "/"); d = path.Dir(d) {
			tokenSet[d] = true
		}
	}
	// Fallback for cd-stripped WORKING_DIRECTORY-relative outputs: cmake
	// records an add_custom_command OUTPUT relative to CMAKE_CURRENT_BINARY_DIR
	// and wraps the recipe in `cd <subdir> && …`; rewriteGenruleCmd strips that
	// cd, leaving a positional output arg in its bare workdir-relative form
	// (curl's `perl mk-lib1521.pl < curl.h lib1521.c`, where the declared out is
	// the build-dir-relative `tests/libtest/lib1521.c`). When an output's full
	// form isn't in the cmd, anchor the LONGEST path-suffix of it that IS — the
	// workdir-relative form — so it still resolves; split re-relativizes the
	// $(RULEDIR)-relative result on the genrule's package move. No-op (no churn)
	// when the full form is present, which is the standalone path's usual shape.
	for _, o := range outs {
		if o == "" || path.IsAbs(o) || containsBoundaryToken(cmd, o) {
			continue
		}
		for s := o; strings.Contains(s, "/"); {
			s = s[strings.Index(s, "/")+1:]
			if containsBoundaryToken(cmd, s) {
				tokenSet[s] = true
				break
			}
		}
	}
	sorted := make([]string, 0, len(tokenSet))
	for tkn := range tokenSet {
		sorted = append(sorted, tkn)
	}
	// Longest first so a path that is a prefix of another ("foo/bar" vs
	// "foo/bar.d", or "foo/bar" vs "foo/bar/x") is anchored before its
	// prefix; the rp-guard below then skips the prefix's now-anchored
	// occurrences.
	sort.Slice(sorted, func(i, j int) bool { return len(sorted[i]) > len(sorted[j]) })
	for _, o := range sorted {
		if o == "" {
			continue
		}
		// Anchor each occurrence of o that begins a path token. The guard
		// is "the preceding byte is not '/'": an occurrence preceded by '/'
		// sits in the MIDDLE of a longer path and must not be anchored —
		// either it's already `$(RULEDIR)/<o>` (rp ends in '/', so this
		// subsumes the old double-anchor guard), or it's a source input the
		// strip already re-anchored to its exec-root form
		// (`<bazelPackagePath>/…/<outdir>/<file>` — e.g. a tablegen `.td`
		// living in the same subdir as a generated `.inc` output, where the
		// output's parent-dir token is a substring of the source path).
		// Injecting `$(RULEDIR)/` there would corrupt the source reference.
		// Occurrences at a real token boundary stay anchored, including the
		// `<o>.d` depfile (a right-extension of o, whose left edge is still a
		// boundary) and joined-flag forms like `-I<outdir>` (preceded by 'I',
		// not '/').
		var b strings.Builder
		for i := 0; i < len(cmd); {
			if strings.HasPrefix(cmd[i:], o) && (i == 0 || cmd[i-1] != '/') {
				b.WriteString(rp)
				b.WriteString(o)
				i += len(o)
				continue
			}
			b.WriteByte(cmd[i])
			i++
		}
		cmd = b.String()
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
//
// bazelPackagePath is the element's exec-root landing package: source `-I`
// roots in the rewritten cmd carry it as a prefix (rewriteGenruleCmd anchors
// them to exec-root form), but the genrule's srcs and the on-disk source tree
// are labelRoot-relative — so the prefix is stripped before resolving each
// include against labelRoot. Empty bazelPackagePath leaves the roots as-is.
func recordCodegenIncludeClosure(targets []ir.Target, labelRoot, bazelPackagePath string) {
	if labelRoot == "" {
		return
	}
	for i := range targets {
		t := &targets[i]
		if t.Kind != ir.KindGenrule || t.GenruleCmd == "" {
			continue
		}
		roots := stripPackagePrefixFromRoots(genruleIncludeRoots(t.GenruleCmd), bazelPackagePath)
		if len(roots) == 0 {
			continue
		}
		primary, ext := primaryCodegenInput(t.Srcs, roots)
		// Gate to tablegen .td: the include scanner recognizes tablegen's
		// `include "..."` syntax, so restrict to .td primaries rather than
		// reading unrelated codegen inputs. (Another include-syntax codegen
		// tool would be a one-line extension to this check.)
		if ext != ".td" {
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

// stripPackagePrefixFromRoots returns roots with a leading
// "<bazelPackagePath>/" removed (and the bare package itself mapped to
// "."), so exec-root-anchored source `-I` roots resolve against the
// labelRoot-relative source tree on disk. Build-dir roots ($(RULEDIR)/…)
// and roots that don't carry the prefix pass through unchanged; the result
// is de-duplicated so a source root that collapses onto an existing
// build-dir root doesn't double the work. Empty bazelPackagePath is a
// pass-through.
func stripPackagePrefixFromRoots(roots []string, bazelPackagePath string) []string {
	if bazelPackagePath == "" || len(roots) == 0 {
		return roots
	}
	pref := bazelPackagePath + "/"
	seen := make(map[string]bool, len(roots))
	out := make([]string, 0, len(roots))
	for _, r := range roots {
		switch {
		case r == bazelPackagePath:
			r = "."
		case strings.HasPrefix(r, pref):
			r = r[len(pref):]
		}
		if seen[r] {
			continue
		}
		seen[r] = true
		out = append(out, r)
	}
	return out
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
		cand := path.Clean(strings.TrimRight(r, "/") + "/" + inc)
		// Reject includes that escape labelRoot (e.g. "../../etc/passwd"):
		// path.Clean would resolve the "..", letting os.Stat read host
		// files outside the workspace and yielding a label containing ".."
		// that Bazel can't represent. Only files within labelRoot fold in.
		if cand == ".." || strings.HasPrefix(cand, "../") || path.IsAbs(cand) {
			continue
		}
		if fi, err := os.Stat(filepath.Join(labelRoot, filepath.FromSlash(cand))); err == nil && !fi.IsDir() {
			return cand
		}
	}
	return ""
}

// threadFileGlobs folds cmake file(GLOB)/file(GLOB_RECURSE) results back
// into build-time glob() filegroups. cmake --trace-expand expands ${glob}
// to its matched files wherever it's used (e.g. a genrule's DEPENDS), so a
// lowered genrule sourced from a glob lists the frozen match set rather
// than the glob. We recover the glob: for each file(GLOB) call we compute
// its match set on the source FS, and when that whole set is a subset of a
// genrule's srcs (the genrule depends on exactly the glob's output) we
// record a GlobSrcGroup — keeping the explicit srcs in place. split then
// drops the covered files, emits one filegroup(srcs = glob([<pattern>]))
// per group, and splices its label into the genrule (so a file added post-
// conversion is picked up); the monolithic emitter keeps the explicit srcs,
// so neither path loses inputs.
//
// The subset test avoids false positives: a genrule with an explicit dep
// that merely overlaps a glob (without covering the whole match set) is
// left untouched. RELATIVE globs are skipped — their results are relative
// to a base dir, not matchable against the absolute-anchored srcs.
//
// labelRoot is the absolute path srcs resolve against; "" disables the
// pass (offline replay without a source tree).
func threadFileGlobs(targets []ir.Target, globs []shadow.FileGlobCall, labelRoot string) {
	if labelRoot == "" || len(globs) == 0 {
		return
	}
	type group struct {
		dir, pattern string
		files        []string
	}
	var groups []group
	for _, gc := range globs {
		if gc.Relative {
			continue
		}
		for _, pat := range gc.Patterns {
			if files, dir, bpat, ok := fileGlobMatchSet(pat, gc.File, gc.Recurse, labelRoot); ok {
				groups = append(groups, group{dir, bpat, files})
			}
		}
	}
	for i := range targets {
		t := &targets[i]
		if t.Kind != ir.KindGenrule || len(t.Srcs) == 0 {
			continue
		}
		srcSet := make(map[string]bool, len(t.Srcs))
		for _, s := range t.Srcs {
			srcSet[s] = true
		}
		var specs []ir.GlobSrcGroup
		seenSpec := map[string]bool{}
		for _, g := range groups {
			if !allIn(g.files, srcSet) {
				continue
			}
			if key := g.dir + "\x00" + g.pattern; !seenSpec[key] {
				seenSpec[key] = true
				specs = append(specs, ir.GlobSrcGroup{
					Dir:     g.dir,
					Pattern: g.pattern,
					Files:   append([]string(nil), g.files...),
				})
			}
		}
		if len(specs) == 0 {
			continue
		}
		// Keep the explicit srcs in place: split drops them in favor of the
		// synthesized glob() filegroup, but the monolithic emitter doesn't
		// synthesize one, so dropping here would silently lose the inputs.
		t.GlobSrcGroups = append(t.GlobSrcGroups, specs...)
	}
}

// fileGlobMatchSet evaluates one file(GLOB) pattern on the source FS,
// returning the matched files (labelRoot-relative, sorted), the labelRoot-
// relative directory the glob is anchored at, and the Bazel glob pattern
// relative to that dir ("*.x" for GLOB, "**/*.x" for GLOB_RECURSE). ok is
// false when the pattern's directory falls outside labelRoot (not
// expressible as a project-local glob) or nothing matches.
func fileGlobMatchSet(pattern, callFile string, recurse bool, labelRoot string) (files []string, anchorDir, bazelPat string, ok bool) {
	pattern = filepath.FromSlash(pattern)
	if !filepath.IsAbs(pattern) {
		// cmake evaluates a relative globbing expression against the calling
		// list file's directory (CMAKE_CURRENT_SOURCE_DIR) — so "src/*.cpp"
		// anchors at dir(callFile). For a glob written inside a macro,
		// callFile is the macro itself, whose dir typically holds no matches;
		// that yields an empty match set and the call is safely skipped
		// rather than mis-anchored.
		pattern = filepath.Join(filepath.Dir(callFile), pattern)
	}
	dir, base := filepath.Dir(pattern), filepath.Base(pattern)
	if strings.ContainsAny(dir, "*?[") {
		// A wildcard in the directory portion (e.g. "data/*/*.txt") means
		// the anchor dir isn't a real package, so we can't root a valid
		// glob() filegroup there. Skip folding — the genrule keeps its
		// explicit srcs — until directory-wildcard anchoring is supported.
		return nil, "", "", false
	}
	relDir, err := filepath.Rel(labelRoot, dir)
	if err != nil || relDir == ".." || strings.HasPrefix(relDir, ".."+string(filepath.Separator)) {
		return nil, "", "", false
	}
	anchorDir = filepath.ToSlash(relDir)
	if anchorDir == "." {
		anchorDir = ""
	}
	bazelPat = base
	if recurse {
		bazelPat = "**/" + base
	}
	var matches []string
	if recurse {
		_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, walkErr error) error {
			if walkErr != nil || d.IsDir() {
				return nil
			}
			if m, _ := filepath.Match(base, filepath.Base(p)); m {
				matches = append(matches, p)
			}
			return nil
		})
	} else {
		// filepath.Glob can match directories (e.g. "data/*"); Bazel glob()
		// excludes dirs and genrule srcs expect files, so drop them — same
		// as the recurse path's d.IsDir() skip.
		all, _ := filepath.Glob(pattern)
		for _, m := range all {
			if fi, err := os.Stat(m); err == nil && !fi.IsDir() {
				matches = append(matches, m)
			}
		}
	}
	for _, m := range matches {
		if rel, err := filepath.Rel(labelRoot, m); err == nil {
			files = append(files, filepath.ToSlash(rel))
		}
	}
	sort.Strings(files)
	return files, anchorDir, bazelPat, len(files) > 0
}

// allIn reports whether every file is present in set (and the list is
// non-empty) — the "genrule depends on the whole glob" subset test.
func allIn(files []string, set map[string]bool) bool {
	for _, f := range files {
		if !set[f] {
			return false
		}
	}
	return len(files) > 0
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

// primaryCodegenInput returns the first src that sits at or under one of the
// genrule's own `-I` roots, plus that src's extension. That coincidence —
// the input lives under an include root the same tool searches — is the
// signal that the genrule resolves includes (tablegen-shaped) rather than
// merely passing `-I` to a compiler. The root may be the src's immediate
// directory or an ancestor of it (e.g. `-I llvm/lib/Target` with a primary
// in `llvm/lib/Target/RISCV/`). srcs are still relative (umbrella-anchored)
// at this lower-time stage; cross-package labels come later.
func primaryCodegenInput(srcs, roots []string) (string, string) {
	clean := make([]string, 0, len(roots))
	for _, r := range roots {
		clean = append(clean, strings.TrimRight(filepath.ToSlash(r), "/"))
	}
	for _, s := range srcs {
		if strings.HasPrefix(s, "//") || strings.HasPrefix(s, "@") || strings.HasPrefix(s, "$") {
			continue // external / label / make-var srcs aren't FS inputs
		}
		dir := strings.TrimRight(filepath.ToSlash(filepath.Dir(s)), "/")
		for _, r := range clean {
			if r == "." || dir == r || (r != "" && strings.HasPrefix(dir, r+"/")) {
				return s, filepath.Ext(s)
			}
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
		case ir.KindCCEmbed:
			// The cc_embed lift produces its .h + .cxx via CCEmbed.OutHeader
			// / OutSource (not GenruleOuts). The recognized vtkEncodeString
			// edge is a ninja CUSTOM_COMMAND, so — as for write_file /
			// cmake_configure_file — missing it here would re-emit a second
			// producer for the same outputs and Bazel rejects the duplicate.
			if t.CCEmbed != nil {
				if t.CCEmbed.OutHeader != "" {
					covered[t.CCEmbed.OutHeader] = true
				}
				if t.CCEmbed.OutSource != "" {
					covered[t.CCEmbed.OutSource] = true
				}
			}
		case ir.KindCCHash:
			// The cc_hash lift produces its header via CCHash.OutHeader (not
			// GenruleOuts). The recognized vtkHashSource edge is a ninja
			// CUSTOM_COMMAND, so — as for cc_embed — missing it here would
			// re-emit a second producer for the same output and Bazel rejects
			// the duplicate generated file.
			if t.CCHash != nil && t.CCHash.OutHeader != "" {
				covered[t.CCHash.OutHeader] = true
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
	return cmakeInternalCmdKind(cmd) != ""
}

// isCreateSymlinkCmd reports whether a recovered ninja CUSTOM_COMMAND is a
// `cmake -E create_symlink` invocation — cmake's portable symlink primitive.
// It creates a symlink side-effect (a tool alias like zstd→zstdcat, a library
// SONAME link, or a manpage alias), not a content output, so it has no Bazel
// build-graph analogue and is dropped like the cmake-internal edges. Matched on
// the RAW ninja cmd (before rewriteGenruleCmd normalizes it to `ln -sfn`),
// where the `create_symlink` -E subcommand token is stable across cmake paths.
func isCreateSymlinkCmd(cmd string) bool {
	return strings.Contains(cmd, "create_symlink")
}

// isCopyCmd reports whether a recovered ninja CUSTOM_COMMAND is a
// `cmake -E copy` (incl. copy_if_different / copy_directory) — cmake's portable
// file-copy primitive. Matched on the raw ninja cmd before rewriteGenruleCmd
// normalizes it to `cp`. Used only in tandem with an empty srcs list, where the
// copy source was never staged and the genrule can't run.
func isCopyCmd(cmd string) bool {
	return strings.Contains(cmd, "-E copy")
}

// isMakeDirOnlyCmd reports whether a recovered ninja CUSTOM_COMMAND is purely a
// directory creation — `cmake -E make_directory <dir>` or a raw `mkdir [-p]
// <dir>` — with no other command chained. Such a command has no Bazel
// build-graph analogue (a Bazel action creates its output's parents implicitly
// under $(RULEDIR); files written into the dir are recovered by their own
// genrules), and cmake's make_directory custom command declares a stamp output
// the mkdir never writes, so emitting a genrule fails "declared output was not
// created". Matched on the raw ninja cmd (the `-E make_directory` token is
// stable before rewriteGenruleCmd normalizes it to `mkdir -p`). A chained
// command (`&&` / `;` / pipe) does real work beyond the mkdir and is NOT
// matched, so only a pure dir-create is dropped.
func isMakeDirOnlyCmd(cmd string) bool {
	c := strings.TrimSpace(cmd)
	// Strip a leading `cd <abs> && ` preamble (cmake-Ninja's per-target
	// build-subdir cd, present on the raw cmd before rewriteGenruleCmd drops
	// it). Without this the && guard below would reject every cd-prefixed
	// make_directory (LLVM's ocaml_make_directory).
	if strings.HasPrefix(c, "cd ") {
		if i := strings.Index(c, " && "); i > 0 {
			c = strings.TrimSpace(c[i+4:])
		}
	}
	if c == "" || strings.ContainsAny(c, ";|") {
		return false
	}
	// Every &&-joined segment must be a pure make_directory / mkdir — a custom
	// command that ONLY creates directories (cmake sometimes chains several).
	// Any other segment means real work, so don't drop.
	for _, seg := range strings.Split(c, " && ") {
		seg = strings.TrimSpace(seg)
		if strings.Contains(seg, "-E make_directory") {
			continue
		}
		if fields := strings.Fields(seg); len(fields) > 0 && filepath.Base(fields[0]) == "mkdir" {
			continue
		}
		return false
	}
	return true
}

// cmakeInternalCmdKind reports the CATEGORY of cmake-internal command a
// recovered ninja CUSTOM_COMMAND edge is, or "" if it isn't one. It's the
// body of isCMakeInternalCmd; the category lets the drop site
// (lowerStandaloneCustomCommands) record an audit breadcrumb instead of
// dropping silently — these edges have no Bazel analogue, but an operator
// auditing a conversion should still see WHAT was filtered. Categories:
// "install" / "uninstall" / "regen" / "cpack" / "clean" / "dashboard" /
// "ide-stub". (The "symlink" (create_symlink) and "copy" (cmake -E copy with no
// stageable source) categories are filtered separately, not via this function —
// they're user commands, not cmake-internal bookkeeping.)
func cmakeInternalCmdKind(cmd string) string {
	c := strings.TrimSpace(cmd)
	// Strip a leading `cd <abs> && ` preamble (cmake-Ninja's
	// per-target build-subdir cd, present on the raw cmd).
	if strings.HasPrefix(c, "cd ") {
		if i := strings.Index(c, " && "); i > 0 {
			c = strings.TrimSpace(c[i+4:])
		}
	}
	// Normalize the leading command token to its basename so any absolute or
	// versioned install path resolves to the bare tool name. A fixed-prefix
	// strip (/usr/bin, /usr/local/bin, …) missed non-standard locations — most
	// visibly the web-session cmake pin (/usr/local/opt/cmake-4.3.3/bin/cmake),
	// which left every `cmake ...`-prefixed match below (install / uninstall /
	// regen) silently unmatched. Keying on the basename is path-independent.
	if sp := strings.IndexByte(c, ' '); sp > 0 {
		c = filepath.Base(c[:sp]) + c[sp:]
	} else if c != "" {
		c = filepath.Base(c)
	}
	// cmake_install.cmake invocations — `cmake ... cmake_install.cmake`
	// (the -P arg may carry a relative or absolute path; check any
	// occurrence of the script-name token).
	if strings.HasPrefix(c, "cmake ") &&
		strings.Contains(c, "cmake_install.cmake") {
		return "install"
	}
	// Uninstall maintenance target — `add_custom_target(uninstall COMMAND cmake
	// -P <uninstall-script>.cmake)`. The CMake-FAQ recipe glm and many projects
	// copy verbatim names the script `cmake_uninstall.cmake`, but the script name
	// is project-chosen: eigen ships `EigenUninstall.cmake`, others use
	// `<Proj>Uninstall.cmake` / `uninstall.cmake`. Match any `-P` script whose
	// basename contains "uninstall" (case-insensitive) and ends in `.cmake`, so
	// the variants are all caught while a user's own `cmake -P myscript.cmake`
	// custom command (no "uninstall" in the name) still isn't swept up. The
	// script deletes install-manifest files at action time: install's mirror, and
	// just as much build-dir bookkeeping with no Bazel analogue.
	if strings.HasPrefix(c, "cmake ") && cmdRunsUninstallScript(c) {
		return "uninstall"
	}
	// `--regenerate-during-build` — cmake's CMakeFiles regen hook.
	if strings.HasPrefix(c, "cmake ") &&
		strings.Contains(c, "--regenerate-during-build") {
		return "regen"
	}
	// cpack + cpack-with-rpmbuild — package distribution.
	if strings.HasPrefix(c, "cpack ") || strings.HasPrefix(c, "cpack-") {
		return "cpack"
	}
	// `ninja clean` — the Ninja (Multi-Config) clean / clean_all target. Runs
	// the generator's clean against cmake's build dir; no Bazel analogue
	// (`bazel clean` is separate). Multi-config emits a clean_all custom target
	// whose output is a `CMakeFiles/clean_all-<config>` stamp — not `.util`, so
	// it leaks past the bookkeeping-output filter.
	if strings.HasPrefix(c, "ninja clean") {
		return "clean"
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
		return "dashboard"
	}
	// Scripted-dashboard form (newer cmake / brotli): instead of
	// `ctest -D <Dashboard>`, the auto-generated target runs
	// `ctest -DMODEL=<Dashboard> [-DACTIONS=...] -S CMakeFiles/CTestScript.cmake`.
	// The `-DMODEL=` here is a ctest cache var, not the `-D <Dashboard>`
	// arg, so the classic match above misses it. The `-DMODEL=Experimental`
	// / `Nightly` / `Continuous` substring is the stable marker (it prefixes
	// the *MemoryCheck / *Coverage variants too) and only appears in cmake's
	// CTestTargets-generated dashboard cmds.
	if strings.Contains(c, "-DMODEL=Experimental") ||
		strings.Contains(c, "-DMODEL=Nightly") ||
		strings.Contains(c, "-DMODEL=Continuous") {
		return "dashboard"
	}
	// `cmake -E echo No interactive CMake dialog available.` IDE
	// stubs that the `.util`-suffix filter misses when the output
	// lands under a nested CMakeFiles/ subdir.
	if strings.HasPrefix(c, "echo No interactive CMake dialog") ||
		strings.HasPrefix(c, "echo No\\ interactive\\ CMake\\ dialog") {
		return "ide-stub"
	}
	return ""
}

// cmdRunsUninstallScript reports whether a `cmake ...` cmd invokes a `-P`
// script whose basename names an uninstall maintenance script — basename
// contains "uninstall" (case-insensitive) and ends in `.cmake`. This is the
// project-independent uninstall signal: the script name is project-chosen
// (`cmake_uninstall.cmake`, eigen's `EigenUninstall.cmake`, a
// `<Proj>Uninstall.cmake`, …) but always carries the word "uninstall". Keying
// on the script basename (not a bare `-P`) means a user's own
// `cmake -P myscript.cmake` custom command isn't mis-classified. The cmd is
// already cd-stripped + basename-normalized by cmakeInternalCmdKind, so tokens
// are space-separated; a `-P` script arg may carry a relative or absolute path,
// so we test the basename of the token following `-P`.
func cmdRunsUninstallScript(cmd string) bool {
	fields := strings.Fields(cmd)
	for i := 0; i+1 < len(fields); i++ {
		if fields[i] != "-P" {
			continue
		}
		base := strings.ToLower(filepath.Base(strings.Trim(fields[i+1], `"'`)))
		if strings.HasSuffix(base, ".cmake") && strings.Contains(base, "uninstall") {
			return true
		}
	}
	return false
}

// sanitizeOutputName converts a path like `gen/version.h` into a
// Bazel target-name-safe stem like `gen_version_h`. Mirrors the
// directory-installer sanitizer but tuned for path-with-extension
// shapes (preserves the `.h` / `.cc` suffix as `_h` / `_cc`).
func sanitizeOutputName(p string) string {
	return sanitizeCollapsingRuns(p)
}

// sanitizeCollapsingRuns turns a path into a Bazel-name-safe token by Clean+
// ToSlash-ing it, then mapping each maximal run of non-alphanumeric runes to a
// single '_' and trimming leading/trailing '_'. Distinct from
// sanitizePathToNameStem (which maps each non-[A-Za-z0-9_] byte 1:1 to '_' and
// keeps '_'): the run-collapse here yields shorter, cleaner names and the two
// are NOT interchangeable (they produce different tokens, so the rule/filegroup
// name spaces they feed must each keep their own).
func sanitizeCollapsingRuns(p string) string {
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
