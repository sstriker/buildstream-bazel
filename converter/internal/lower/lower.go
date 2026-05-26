// Package lower converts a parsed CMake File API reply into the IR consumed by
// emit/. It is the conversion brain; most semantic decisions (rule kind
// classification, header discovery, flag splitting) live here.
//
// M1 scope: single-config (Release), single-language compile groups, no
// add_custom_command (genrule recovery is M2). Anything outside this scope
// returns a Tier-1 failure via failure.Error so the caller can surface it
// without aborting the conversion run.
package lower

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/sstriker/buildstream-bazel/converter/internal/cmakerun"
	"github.com/sstriker/buildstream-bazel/converter/internal/ctest"
	"github.com/sstriker/buildstream-bazel/converter/internal/failure"
	"github.com/sstriker/buildstream-bazel/converter/internal/fileapi"
	"github.com/sstriker/buildstream-bazel/converter/internal/ninja"
	"github.com/sstriker/buildstream-bazel/converter/ir"
	"github.com/sstriker/buildstream-bazel/internal/manifest"
	"github.com/sstriker/buildstream-bazel/internal/shadow"
)

// Options controls behavior that the orchestrator (M3) overrides per-package.
// M1 callers can pass the zero value.
type Options struct {
	// HostSourceRoot is the on-disk path to the source tree, used for
	// filesystem walks (e.g. header discovery under each include directory).
	// Defaults to the source path recorded in the File API codemodel.
	HostSourceRoot string

	// HostPrefixDir, when set, is the absolute on-disk path to the
	// synthesized prefix tree cmake configured against (CMAKE_PREFIX_PATH).
	// Codemodel link.commandFragments paths anchored at this dir are
	// remapped to manifestPrefixAnchor before manifest lookup so the
	// host-independent imports manifest matches regardless of where on
	// disk the prefix tree happens to live.
	HostPrefixDir string

	// Imports resolves out-of-tree imported targets (find_package-style
	// names that aren't part of the current codebase) to Bazel labels.
	// Optional; nil disables manifest lookup, in which case unresolved
	// link deps trigger unresolved-link-dep.
	Imports *manifest.Resolver

	// CTest, when non-nil, lets ToIR classify EXECUTABLE targets that
	// were registered via add_test() as cc_test rules. The registry is
	// produced by parsing CTestTestfile.cmake out of the cmake build
	// dir; nil means no test classification (every EXECUTABLE stays
	// cc_binary, matching pre-CTest-support behavior).
	CTest *ctest.Registry

	// BuildDir is the host-real path of the cmake build directory
	// where configured outputs (configure_file's targets, version
	// headers cmake writes at configure time, etc.) live on this
	// machine. Used together with TraceRaw to read the rendered
	// bytes of configure_file outputs and embed them into Bazel
	// genrules. Empty disables configure_file recovery.
	//
	// Production: the live cmake build dir convert-element-cmake just
	// configured.
	// Offline tests: the fixture dir, which record-fileapi.sh
	// stashes configured outputs into mirroring the build-dir
	// layout.
	BuildDir string

	// TraceRaw is cmake's --trace-expand --trace-format=json-v1
	// output (one JSON event per line). When non-empty, lower
	// uses it to surface signals the codemodel doesn't expose:
	//   - target_include_directories PUBLIC/PRIVATE arms
	//     (visibility delta — the codemodel flattens both
	//     into compileGroups[].includes[]).
	//   - target_link_libraries IMPORTED-target deps for
	//     STATIC libs (find-package STATIC delta — codemodel
	//     records nothing for static-lib link inputs).
	//   - configure_file(<input> <output> ...) input/output
	//     pairings (configure_file delta — cmakeFiles records
	//     the input only).
	//
	// Empty disables the trace-driven enrichment paths;
	// codemodel-only behavior matches pre-trace lower output.
	TraceRaw []byte

	// UnsupportedExecuteProcessFallback toggles
	// recoverExecuteProcess's refusal handling. When false
	// (the default — preserves Phase A behaviour),
	// classifier refusals (stamp / probe / unknown buckets)
	// produce a typed unsupported-execute-process Tier-1
	// failure. When true, refusals don't error; ToIR returns
	// a placeholder ir.Package whose targets are **empty
	// cc_library / cc_binary / cc_library-interface stubs**
	// (one per non-UTILITY codemodel target, public
	// visibility) so downstream label references still
	// resolve at analysis time. The per-target artifact
	// wiring (cc_import / sh_binary referencing install_tree.tar
	// paths reconstructed from Target.Install.Destinations
	// + NameOnDisk) lands in Step 2.5 — until then,
	// downstream consumers' compile/link actions against the
	// stubs fail. See
	// docs/design/cmake-execute-process-round2-fallback.md
	// for the architectural shape.
	UnsupportedExecuteProcessFallback bool

	// LiftConfigureFile toggles the configure_file recovery's
	// lifted shape. When true (and a values namespace is
	// available — either via CMakeVars below or via per-template
	// Extract recovery), the emitted genrule has the .h.in
	// template as a real srcs input and invokes
	// //tools:cmake-configure-file at Bazel build time. When
	// false (the default — preserves pre-lift behaviour for
	// callers that don't yet stage the tool), the legacy
	// base64-of-rendered-bytes cmd shape is always emitted. See
	// internal/configurefile package doc for the full lift
	// rationale + cache-key analysis.
	LiftConfigureFile bool

	// CMakeVars is the full cmake variable namespace captured
	// at end of configure (cmakerun.Reply.Vars). Used by the
	// configure_file lift as the values map for the lifted
	// genrule's Substitute, replacing per-template Extract.
	// With every cmake variable in hand, .h.in edits that
	// introduce new @VAR@/${VAR} markers always resolve
	// correctly without convert-element-cmake rerunning — closes the
	// soundness gap the per-template Extract had. Empty (e.g.
	// offline --reply-dir tests, or a configure that fatal-
	// erred before the dump-vars hook fired) falls back to
	// Extract per-template; if Extract also fails the lift
	// falls back to legacy.
	CMakeVars map[string]string

	// GenexProbes carries the per-target probe data captured by
	// the probe-genex.cmake hook at cmake generation time
	// (cmakerun.ReadGenexProbe). When non-empty, buildGenexTargets
	// folds the probe-captured INTERFACE_* aggregates and
	// TARGET_OBJECTS values into the genexeval.TargetInfo entries
	// so the (a) evaluator can resolve those genex shapes
	// directly — retiring the UnsupportedError fall-throughs the
	// lifter previously had to route to (b) / legacy. Empty
	// (probe didn't run, cmake < 3.24, or the operator didn't
	// pass --probe-genex) leaves the new TargetInfo fields blank
	// and the evaluator falls back as before.
	//
	// Phase 3 of the generator-parity uplift in ROADMAP.md.
	GenexProbes []cmakerun.GenexProbe

	// EmitStandaloneCustomCommands toggles Phase 4 of the
	// generator-parity uplift: emit genrules for CUSTOM_COMMAND
	// edges in build.ninja that aren't already covered by the
	// recoverGenrule path. Off by default — opt-in is the safer
	// default because the new emission can shift BUILD shape for
	// projects relying on implicit add_custom_target bookkeeping.
	EmitStandaloneCustomCommands bool

	// ConfigureLog carries the parsed CMakeConfigureLog.yaml
	// events from configureLog-v1 (cmake 3.26+). Empty for cmake
	// < 3.26 or projects whose configure didn't fire any
	// configureLog-aware events. Phase 4 of the generator-parity
	// uplift consumes try_compile-v1 / find_package-v1 results
	// to retire probe-bucket execute_process refusals — when a
	// refused probe (e.g. `git rev-parse` for a version stamp,
	// or a `pkg-config` lookup) has a configureLog event with
	// the same outcome, the lifter can emit the resolved value
	// directly via select() / stamp / a literal embed instead
	// of Tier-1-failing.
	//
	// Loaded by the caller via fileapi.LoadConfigureLogYAML to
	// keep lower's I/O scope unchanged (lower itself stays
	// pure-function over the reply data, no FS reads beyond
	// configure_file's rendered output capture).
	ConfigureLog []fileapi.Event
}

// manifestPrefixAnchor is the canonical token the orchestrator's imports
// manifest uses to anchor cross-element link paths (see
// orchestrator.sandboxPrefix). The token is virtual — no filesystem path
// of that name exists; lower remaps real prefix paths onto it before
// LookupLinkPath.
const manifestPrefixAnchor = "/opt/prefix/"

// Header file extensions we treat as `hdrs` candidates when walking include
// directories. Lowercase comparison.
var headerExts = map[string]bool{
	".h":   true,
	".hh":  true,
	".hpp": true,
	".hxx": true,
	".inl": true,
}

// ToIR lowers a parsed reply into a Package. The optional ninja graph
// enables genrule recovery for targets with isGenerated sources; pass nil to
// disable (M1-style behavior — generated sources then trigger
// unsupported-custom-command).
func ToIR(r *fileapi.Reply, g *ninja.Graph, opts Options) (*ir.Package, error) {
	if got := len(r.Codemodel.Configurations); got != 1 {
		return nil, failure.New(failure.UnsupportedTargetType,
			"M1 supports exactly one configuration; got %d", got)
	}
	cfg := r.Codemodel.Configurations[0]

	// Pre-parse trace records once so lowerTarget can consult
	// per-target maps without re-walking the trace bytes per
	// target. Empty TraceRaw → empty maps, no behavior change.
	//
	// knownTargets lets the trace extractors keep calls that
	// originate in producer-element cmake macros (outside the
	// consumer source tree) but act on consumer-defined
	// targets — the macro-from-import case. Without this,
	// inSourceTree alone would drop those calls and lower
	// would lose visibility/link-libs information they
	// contribute.
	knownTargets := map[string]bool{}
	for _, tref := range cfg.Targets {
		knownTargets[tref.Name] = true
	}

	var privateIncludeDirs map[string]map[string]bool // target → set of absolute private dir paths
	var traceLinkLibs map[string][]string             // target → ordered list of cmake lib names from target_link_libraries (all visibility arms, dedup-preserved)
	// traceLinkScope maps target → libName → cmake keyword
	// ("PUBLIC", "PRIVATE", "INTERFACE", or "" for the legacy
	// keyword-less positional shape — which cmake treats as
	// PUBLIC). Populated alongside traceLinkLibs from the same
	// shadow.Decode pass; consumed by lowerTarget to route
	// PRIVATE deps to ir.Target.ImplementationDeps per Phase 4
	// (parsed-from-trace signal).
	//
	// First-write-wins on duplicate (target, lib) pairs across
	// arms: cmake itself allows multiple target_link_libraries
	// calls with conflicting keywords, but the upstream-most
	// scope governs propagation. The codemodel's
	// commandFragments resolve to the same final link line
	// regardless; the keyword recovery here is best-effort.
	var traceLinkScope map[string]map[string]string
	// platformConditionalSrcs maps target → source-path (project-
	// relative, slash form, matching codemodel TargetSource.Path)
	// → Bazel constraint label the source should be conditional
	// on. Populated by shadow.Decode from
	// `if(CMAKE_SYSTEM_NAME STREQUAL "<Name>")` blocks in the
	// trace (#217 Tier 1). When a source has an entry here, the
	// lowerTarget source walk moves it from the flat
	// `irt.Srcs` into `irt.PerPlatform["srcs"][selectKey]` so the
	// emitter renders a select() arm. Sources without an entry
	// stay in flat srcs — single-platform conversion of projects
	// without platform conditionals stays byte-identical.
	var platformConditionalSrcs map[string]map[string]string
	// traceDecoded tracks whether shadow.Decode ran; when true,
	// decodedConfigureFiles holds the configure_file extractions
	// from that single pass and the configure_file recovery
	// reuses them rather than re-parsing the trace. A
	// nil-slice sentinel wouldn't suffice — a trace with zero
	// configure_file events leaves the slice nil, which would
	// otherwise look identical to "decode never ran".
	var traceDecoded bool
	var decodedConfigureFiles []shadow.ConfigureFileCall
	var decodedFileGenerates []shadow.FileGenerateCall
	var decodedExecuteProcesses []shadow.ExecuteProcessCall
	// headerOnlySources holds slash-form source paths declared with
	// set_source_files_properties(... HEADER_FILE_ONLY TRUE). The
	// per-target source walk reclassifies these from srcs to hdrs.
	// Populated by collectHeaderOnlySources when the trace decoded
	// SourceFileProperties calls.
	var headerOnlySources map[string]bool
	// objectDependsBySrc maps source paths to the list of header
	// paths declared via set_source_files_properties(... PROPERTIES
	// OBJECT_DEPENDS "h1;h2"). The per-target post-pass adds those
	// headers to the target's hdrs so Bazel's incremental rebuild
	// trips on changes.
	var objectDependsBySrc map[string][]string

	// Phase 1 task 1 keyword recovery runs FIRST — backtrace is
	// strictly more authoritative than trace in every case where
	// they disagree:
	//
	//   - Macro-wrapped target_link_libraries: trace records the
	//     macro's inner call (the keyword the macro author chose);
	//     backtrace walks to the user's outer call site and
	//     recovers the keyword the user wrote. User intent wins.
	//
	//   - Both agree: order doesn't matter; backtrace's value is
	//     identical to what trace would write.
	//
	// The trace block below has a first-write-wins guard on its
	// per-(target, lib) population, so once backtrace pre-populates
	// a pair the trace processing leaves it alone — backtrace's
	// (outer-frame) keyword survives.
	//
	// The one case trace handles that backtrace can't: `target_link_libraries(foo
	// PUBLIC ${SOME_DEP_VAR})` — the dep name in the source argv
	// is `${SOME_DEP_VAR}`, not the expanded literal, so
	// cmakeargv's literal match against the codemodel's dep name
	// misses. The trace's post-expansion argv handles it; backtrace
	// leaves the entry unpopulated, trace fills the gap. (The
	// reverse case — trace missing a keyword backtrace recovers —
	// is what the offline-replay-no-trace path always exercises.)
	if btScope := backtraceRecoverLinkScope(r); len(btScope) > 0 {
		traceLinkScope = map[string]map[string]string{}
		for tgt, libs := range btScope {
			traceLinkScope[tgt] = map[string]string{}
			for lib, kw := range libs {
				traceLinkScope[tgt][lib] = kw
			}
		}
	}

	if len(opts.TraceRaw) > 0 {
		cmakeSrcForTrace := r.Codemodel.Paths.Source
		decoded := shadow.Decode(opts.TraceRaw, cmakeSrcForTrace, knownTargets)
		traceDecoded = true
		privateIncludeDirs = map[string]map[string]bool{}
		for _, call := range decoded.Includes {
			for _, grp := range call.Groups {
				if grp.Visibility != "PRIVATE" {
					continue
				}
				if _, ok := privateIncludeDirs[call.Target]; !ok {
					privateIncludeDirs[call.Target] = map[string]bool{}
				}
				for _, dir := range grp.Dirs {
					privateIncludeDirs[call.Target][dir] = true
				}
			}
		}
		traceLinkLibs = map[string][]string{}
		traceLinkScope = map[string]map[string]string{}
		for _, call := range decoded.Links {
			seen := map[string]bool{}
			if _, ok := traceLinkScope[call.Target]; !ok {
				traceLinkScope[call.Target] = map[string]string{}
			}
			for _, grp := range call.Groups {
				for _, lib := range grp.Libs {
					if seen[lib] {
						continue
					}
					seen[lib] = true
					traceLinkLibs[call.Target] = append(traceLinkLibs[call.Target], lib)
					// First-write-wins so an earlier
					// target_link_libraries(t PUBLIC bar) call
					// doesn't get overwritten by a later
					// target_link_libraries(t PRIVATE bar) — the
					// effective semantics in cmake itself are
					// not well-defined when the same library is
					// listed twice with different keywords, but
					// the upstream-most call governs header
					// propagation in the typical case.
					if _, ok := traceLinkScope[call.Target][lib]; !ok {
						traceLinkScope[call.Target][lib] = grp.Visibility
					}
				}
			}
		}
		decodedConfigureFiles = decoded.ConfigFiles
		decodedFileGenerates = decoded.FileGenerates
		decodedExecuteProcesses = decoded.ExecuteProcesses
		// Phase 1 task 3 extension — HEADER_FILE_ONLY routing.
		// Build the per-source path lookup once so the per-target
		// source walk can reclassify .h-only sources from srcs
		// into hdrs.
		headerOnlySources = collectHeaderOnlySources(decoded.SourceFileProperties)
		objectDependsBySrc = collectObjectDepends(decoded.SourceFileProperties)
		if len(decoded.PlatformConditionalSources) > 0 {
			platformConditionalSrcs = map[string]map[string]string{}
			for _, pcs := range decoded.PlatformConditionalSources {
				if _, ok := platformConditionalSrcs[pcs.Target]; !ok {
					platformConditionalSrcs[pcs.Target] = map[string]string{}
				}
				// First-write-wins: if the same (target, src)
				// shows up under two different conditionals
				// (rare — would mean nested elseif arms both
				// adding the same source on different
				// platforms), the first one's SelectKey
				// governs. Cheap to refine later if real
				// projects need a list-valued mapping.
				if _, ok := platformConditionalSrcs[pcs.Target][pcs.Source]; !ok {
					platformConditionalSrcs[pcs.Target][pcs.Source] = pcs.SelectKey
				}
			}
		}
	}

	cmakeSrc := r.Codemodel.Paths.Source
	cmakeBuild := r.Codemodel.Paths.Build
	hostSrc := opts.HostSourceRoot
	if hostSrc == "" {
		hostSrc = cmakeSrc
	}
	// hostSrcOnDisk gates the per-source existence check used for
	// the #209 missing-source elision. Reply-dir-only replay runs
	// (golden tests, offline fixtures) point hostSrc at a path the
	// recording machine had but this host doesn't, and the elision
	// against an absent root would drop every source. Stat once
	// here; the loop reads the bool.
	hostSrcOnDisk := false
	if info, statErr := os.Stat(hostSrc); statErr == nil && info.IsDir() {
		hostSrcOnDisk = true
	}

	pkg := &ir.Package{
		Name:           projectName(r),
		SourceRoot:     hostSrc,
		HeaderComments: findPackageHeaderComments(opts.ConfigureLog),
	}

	cc := newCodegenContext()

	// execute_process recovery. Configure-time subprocess
	// invocations are a hermeticity violation by Bazel's
	// analysis-then-action model. Some calls are liftable
	// (cmake -E builtins; file-producing tools with declared
	// OUTPUT_FILE — translated to build-time genrules) and some
	// aren't (version stamps, toolchain probes, opaque
	// pipelines).
	//
	// Disposition of the unliftable bucket depends on
	// opts.UnsupportedExecuteProcessFallback:
	//   - off (Phase A): aggregate into a single typed
	//     unsupported-execute-process Tier-1 failure so
	//     projects with several offending calls get one
	//     triage report rather than N converter runs
	//     uncovering them one at a time.
	//   - on (Phase B fallback): emitFallbackPlaceholder
	//     enumerates every non-UTILITY codemodel target as
	//     an empty cc_library / cc_binary / cc_library-
	//     interface stub so downstream label references
	//     resolve at analysis time. Step 2.5 (PR #98)
	//     extends the placeholder to wire those stubs to
	//     Target.Install.Destinations via cc_import /
	//     sh_binary; this PR (Step 2) only delivers the
	//     analysis-time label-resolution shape.
	//
	// Liftable buckets always append to cc.Genrules +
	// cc.OutToGenrule before lowerTarget runs so consumer
	// attribution can attach generated artefacts to cc
	// targets that include the build dir. The
	// []executeProcessOut return is parallel to
	// configureFileOut and feeds the same per-target
	// attribution loop in lowerTarget below.
	// Merge cmakeVars (dump-vars hook output) with configureLog-
	// derived try_compile / try_run result variables. cmakeVars
	// covers the user-defined namespace; configureLog covers
	// probe-set variables that landed in cmake's cache via
	// Check_* modules. cmakeVars wins on overlap — it's the
	// canonical end-of-configure namespace.
	rescueVars := opts.CMakeVars
	if clVars := configureLogVars(opts.ConfigureLog); len(clVars) > 0 {
		merged := make(map[string]string, len(rescueVars)+len(clVars))
		for k, v := range clVars {
			merged[k] = v
		}
		for k, v := range rescueVars {
			merged[k] = v
		}
		rescueVars = merged
	}
	executeProcesses, executeProcessRefusals := recoverExecuteProcess(decodedExecuteProcesses, hostSrc, cmakeSrc, opts.BuildDir, cmakeBuild, opts.LiftConfigureFile, rescueVars, cc)
	if len(executeProcessRefusals) > 0 {
		if !opts.UnsupportedExecuteProcessFallback {
			return nil, formatExecuteProcessFailure(executeProcessRefusals)
		}
		// Phase B fallback: emit a placeholder ir.Package
		// rather than continuing into the native lowering
		// path. The native path would either redo the
		// refusal analysis or trip on the unliftable call
		// later in lowerTarget; the placeholder is the
		// cleaner cut, and it lets downstream consumers see
		// per-target labels at analysis time even when the
		// element itself can't be fine-converted.
		return emitFallbackPlaceholder(r, hostSrc)
	}

	// Recover configure_file outputs from trace before lowering
	// targets. Each call surfaces as an ir.Target{KindGenrule}
	// on cc.Genrules; the returned slice tells lowerTarget which
	// recorded outputs (and their build-dir-relative paths) to
	// attach to consuming targets that include the cmake build
	// dir in their codemodel-recorded Includes.
	var configureFiles []configureFileOut
	if traceDecoded {
		var err error
		configureFiles, err = recoverConfigureFilesFromCalls(decodedConfigureFiles, hostSrc, cmakeSrc, opts.BuildDir, cmakeBuild, opts.LiftConfigureFile, opts.CMakeVars, cc)
		if err != nil {
			return nil, err
		}
	} else {
		var err error
		configureFiles, err = recoverConfigureFiles(opts.TraceRaw, hostSrc, opts.BuildDir, cmakeSrc, cmakeBuild, opts.LiftConfigureFile, opts.CMakeVars, cc)
		if err != nil {
			return nil, err
		}
	}

	// file(GENERATE) recovery — sister to configure_file's lift.
	// Outputs land in cc.Genrules / cc.OutToGenrule before
	// lowerTarget runs; the returned slice feeds lowerTarget's
	// consumer-attribution loop the same way configureFiles does.
	// Reuses opts.LiftConfigureFile as the lift opt-in: both
	// lifters require the same Bazel-time tool (//tools:cmake-
	// configure-file) and the same cmakeVars namespace dump, so
	// flipping them together keeps the operator's flag surface
	// minimal. Only the decoded-trace path drives this — the
	// pre-trace fallback that re-parses opts.TraceRaw doesn't
	// surface a file(GENERATE) extractor today (none of the
	// pre-Decode callers need it).
	var fileGenerates []fileGenerateOut
	if traceDecoded {
		var err error
		fileGenerates, err = recoverFileGenerate(decodedFileGenerates, hostSrc, cmakeSrc, opts.BuildDir, cmakeBuild, opts.LiftConfigureFile, opts.CMakeVars, buildGenexTargets(r, cmakeBuild, opts.GenexProbes), opts.Imports, cc)
		if err != nil {
			return nil, err
		}
	}

	// Build the in-codebase id -> Bazel-rule-name map up front so dep
	// lowering can map t.Dependencies[].Id to a label without re-walking
	// configurations. UTILITY targets (add_custom_target nodes) are
	// excluded — they have no Bazel equivalent, so depending on them is a
	// no-op; the underlying add_custom_command's outputs are referenced
	// via srcs/hdrs instead. utilityIDs records them separately so dep
	// resolution can distinguish "skip utility" from "unresolved".
	idToName := map[string]string{}
	utilityIDs := map[string]bool{}
	for _, tref := range cfg.Targets {
		if t, ok := r.Targets[tref.Id]; ok && t.Type == "UTILITY" {
			utilityIDs[tref.Id] = true
			continue
		}
		idToName[tref.Id] = tref.Name
	}

	for _, tref := range cfg.Targets {
		t, ok := r.Targets[tref.Id]
		if !ok {
			return nil, failure.New(failure.FileAPIMalformed,
				"target id %q in codemodel but not loaded", tref.Id)
		}
		irt, err := lowerTarget(&t, cmakeSrc, cmakeBuild, hostSrc, opts.HostPrefixDir, hostSrcOnDisk, g, cc, idToName, utilityIDs, opts.Imports, opts.CTest, privateIncludeDirs[tref.Name], traceLinkLibs[tref.Name], traceLinkScope[tref.Name], configureFiles, fileGenerates, executeProcesses, platformConditionalSrcs[tref.Name])
		if err != nil {
			return nil, err
		}
		if irt == nil {
			// lowerTarget returned (nil, nil) to skip — UTILITY targets
			// (add_custom_target nodes) and similar that have no Bazel
			// equivalent. Also fires when an EXECUTABLE was rewritten
			// into one or more cc_test entries on cc.Tests.
			continue
		}
		pkg.Targets = append(pkg.Targets, *irt)
	}

	// Append recovered genrules then per-language sub-libraries
	// then cc_test rules in deterministic order; each slot is
	// appended in target-walk order during lowerTarget, which is
	// itself stable.
	pkg.Targets = append(pkg.Targets, cc.Genrules...)
	pkg.Targets = append(pkg.Targets, cc.Subs...)
	pkg.Targets = append(pkg.Targets, cc.Tests...)
	// HEADER_FILE_ONLY reclassification — walk every target's
	// srcs and move entries the trace's
	// set_source_files_properties calls marked
	// HEADER_FILE_ONLY=TRUE into hdrs. Phase 1 task 3 extension
	// (per docs/design/generator-parity-gaps.md). Post-emit pass
	// keeps lowerTarget's signature stable and applies uniformly
	// to all the rule families.
	reclassifyHeaderOnlySources(pkg, headerOnlySources)
	// Probe-genex per-target Properties → Bazel attributes:
	// BUILD_RPATH / INSTALL_RPATH lift to linkopts,
	// POSITION_INDEPENDENT_CODE to features=["pic"] /
	// features=["-pic"], visibility presets to copts. Off when no
	// probe ran (opts.GenexProbes empty) so back-compat preserved
	// for callers that don't pass --probe-genex.
	applyProbeGenexProperties(pkg, opts.GenexProbes)
	// OBJECT_DEPENDS post-pass adds declared header dependencies
	// to the target's hdrs so incremental rebuilds trip on
	// changes. Uses the same per-pkg walk shape as the
	// HEADER_FILE_ONLY pass.
	addObjectDependsHeaders(pkg, objectDependsBySrc)
	// install(FILES) → filegroup() lowering (Phase 1 task 2 of the
	// generator-parity uplift). Appended last so the file-head
	// targets stay grouped by family: cc rules first, generated
	// content next, then install-side filegroups.
	pkg.Targets = append(pkg.Targets, lowerDirectoryInstallers(r)...)
	// Phase 4 standalone custom-command emission. Opt-in via
	// Options.EmitStandaloneCustomCommands; the dedup against
	// existing genrules keeps the recoverGenrule path's output
	// intact even when this fires.
	if opts.EmitStandaloneCustomCommands {
		pkg.Targets = append(pkg.Targets,
			lowerStandaloneCustomCommands(g, pkg.Targets, opts.BuildDir)...)
	}
	// Phase 5 multi-config delta fold. When the reply carries
	// per-config target data (BuildTypes-driven multi-config),
	// project the cross-config Partition into PerPlatform-shaped
	// select() arms on the existing targets. Sanitizer-shaped
	// configs are filtered out and route to --features in a
	// future slice.
	if len(r.TargetsByConfig) > 0 {
		var configs []string
		for _, cfg := range r.Codemodel.Configurations {
			configs = append(configs, cfg.Name)
		}
		lowerMultiConfigDeltas(pkg, r.TargetsByConfig, configs)
	}
	return pkg, nil
}

func projectName(r *fileapi.Reply) string {
	if e := r.Cache.Get("CMAKE_PROJECT_NAME"); e != nil {
		return e.Value
	}
	return ""
}

func lowerTarget(t *fileapi.Target, cmakeSrc, cmakeBuild, hostSrc, hostPrefix string, hostSrcOnDisk bool, g *ninja.Graph, cc *codegenContext, idToName map[string]string, utilityIDs map[string]bool, imports *manifest.Resolver, tests *ctest.Registry, privateIncludeDirs map[string]bool, traceLinkLibs []string, traceLinkScope map[string]string, configureFiles []configureFileOut, fileGenerates []fileGenerateOut, executeProcesses []executeProcessOut, platformConditionalSrcs map[string]string) (*ir.Target, error) {
	// Generator-provided targets (ZERO_CHECK, INSTALL, PACKAGE,
	// RUN_TESTS, etc.) are inserted by cmake itself for IDE
	// integration and have no Bazel equivalent. Skip them silently.
	if t.IsGeneratorProvided {
		return nil, nil
	}

	irt := &ir.Target{Name: t.Name}

	// Provenance: project the per-target Backtrace index into a
	// {File, Line, Command} triple from the BacktraceGraph the
	// target's JSON file carries. Phase 1 task 1 of the
	// generator-parity uplift (ROADMAP.md). Emit-side gating
	// renders this as a leading comment when EmitProvenance is
	// on.
	if t.Backtrace > 0 && t.Backtrace < len(t.BacktraceGraph.Nodes) {
		node := t.BacktraceGraph.Nodes[t.Backtrace]
		var file, cmd string
		if node.File >= 0 && node.File < len(t.BacktraceGraph.Files) {
			file = t.BacktraceGraph.Files[node.File]
		}
		if node.Command >= 0 && node.Command < len(t.BacktraceGraph.Commands) {
			cmd = t.BacktraceGraph.Commands[node.Command]
		}
		if file != "" {
			irt.Provenance = ir.Provenance{File: file, Line: node.Line, Command: cmd}
		}
	}

	switch t.Type {
	case "STATIC_LIBRARY":
		irt.Kind = ir.KindCCLibrary
		irt.Linkstatic = true
	case "OBJECT_LIBRARY":
		// cmake OBJECT libs compile sources to .o without
		// archiving. Consumers reference $<TARGET_OBJECTS:t>
		// to inline the objects into a downstream artifact.
		// Bazel analog: cc_library with alwayslink=True so
		// the objects always link into transitive consumers
		// (matches cmake's "every consumer drags every
		// object" semantics). linkstatic stays false — there's
		// no archive; alwayslink is what carries the inline
		// behavior.
		irt.Kind = ir.KindCCLibrary
		irt.Alwayslink = true
	case "SHARED_LIBRARY", "MODULE_LIBRARY":
		irt.Kind = ir.KindCCLibrary
	case "EXECUTABLE":
		irt.Kind = ir.KindCCBinary
	case "INTERFACE_LIBRARY":
		irt.Kind = ir.KindCCInterface
	case "UTILITY":
		// add_custom_target / add_dependencies grouping. The underlying
		// add_custom_command is recovered separately via genrule lookup;
		// the utility node itself has no Bazel equivalent.
		return nil, nil
	default:
		return nil, failure.New(failure.UnsupportedTargetType,
			"target %q has unsupported type %q", t.Name, t.Type)
	}

	// Build the source-index → in-compile-group set once per target.
	// Walking t.CompileGroups[].SourceIndexes per source visit was
	// O(N*M) where N is sources and M is compile-group entries; for
	// large targets (mesa, gcc, clang) that's the dominant cost in
	// lowerTarget.
	inCompileGroup := buildCompileGroupSet(t)

	consumesCodegen := false
	elidedBuildDirSrc := false
	elidedMissingSrc := false
	elidedCompilerArtifact := false
	for i, src := range t.Sources {
		// CMake's bookkeeping `<build>/version.h.rule` files are internal
		// re-run markers; skip them silently.
		if strings.HasSuffix(src.Path, ".rule") {
			continue
		}

		if src.IsGenerated {
			// $<TARGET_OBJECTS:other_target> shows up in
			// codemodel as "<build>/CMakeFiles/<other>.dir/<src>.o"
			// generated sources. The other target's compile is
			// already captured as its own cc_library; the
			// inlining relationship surfaces via t.Dependencies
			// + the OBJECT lib's alwayslink=True. Skip these
			// here — passing them through recoverGenrule would
			// fail because cmake's C compile rule isn't a
			// CUSTOM_COMMAND.
			if isTargetObjectsRef(src.Path, cmakeBuild, idToName) {
				continue
			}
			// Unity builds and other compile-rule-produced .o
			// files appear as generated sources but aren't
			// CUSTOM_COMMAND outputs — they're cc-compile
			// artifacts already captured by the same target's
			// compile group. Skip silently with an audit tag so
			// the recoverGenrule call below doesn't refuse them;
			// #206. Conservative gate: must look like a compile
			// artifact (.o/.obj under CMakeFiles/<x>.dir) AND
			// the producing ninja rule must be a known compiler
			// rule shape. Either signal alone is too permissive.
			if isCompilerObjectArtifact(src.Path, cmakeBuild, g) {
				elidedCompilerArtifact = true
				continue
			}
			relOut, _, err := cc.recoverGenrule(src.Path, cmakeSrc, cmakeBuild, g)
			if err != nil {
				return nil, err
			}
			consumesCodegen = true
			ext := strings.ToLower(filepath.Ext(relOut))
			switch {
			case inCompileGroup[i]:
				irt.Srcs = append(irt.Srcs, relOut)
			case headerExts[ext]:
				irt.Hdrs = append(irt.Hdrs, relOut)
			default:
				// Non-header, not compiled: still belongs in srcs so the
				// genrule's output is included in the package's input set.
				irt.Srcs = append(irt.Srcs, relOut)
			}
			continue
		}

		if !inCompileGroup[i] {
			// Not assigned to a compile group: probably a header in
			// target_sources(); we'll discover hdrs via include-dir
			// walking below. Skip here.
			continue
		}
		// Configure-time-created files living under the build dir
		// (e.g. `file(WRITE ${CMAKE_BINARY_DIR}/dummy.cpp "")` +
		// `add_library(foo ${CMAKE_BINARY_DIR}/dummy.cpp)` — a
		// common header-only-library shim) get recorded with an
		// absolute path under cmakeBuild but without the
		// IsGenerated flag (cmake only sets that for sources
		// produced by add_custom_command / configure_file etc.).
		// Their absolute paths point at this run's tmp build dir,
		// which is gone before Bazel ever executes the rule —
		// emitting them produces a cc_library whose srcs label
		// resolves to a nonexistent file. Drop them silently and
		// tag the consuming target so audit queries can find the
		// elided sources; downstream the cc_library renders with
		// the remaining (real) sources, or hdrs-only if this was
		// the only source.
		if cmakeBuild != "" && filepath.IsAbs(src.Path) {
			if _, inside := relativeIfInside(cmakeBuild, src.Path); inside {
				elidedBuildDirSrc = true
				continue
			}
		}
		// Path normalization + invalid-label refusal (#221).
		// cmake's codemodel documents TargetSource.Path as
		// "relative to the project source root" (codemodel.go),
		// but a few shapes still slip through with absolute
		// paths or syntactic noise that Bazel rejects as a
		// label. Refusing at convert-time surfaces the
		// underlying cmake issue (the project referenced a
		// source outside its hermetic boundary, or a name that
		// can't be a Bazel label) before any broken BUILD
		// lands on disk — strictly better than letting Bazel
		// surface "target names may not start with '/'" or
		// "target names may not contain '.' as a path segment"
		// downstream.
		srcPath := src.Path
		// In-tree absolute path: cmake recorded an absolute
		// path that happens to live under cmakeSrc. Normalize
		// to the documented project-relative form so the
		// emitted label is valid. cmakeSrc is "" on
		// reply-dir-only replay runs; skip in that case
		// because the relativeIfInside check can't run.
		if cmakeSrc != "" && filepath.IsAbs(srcPath) {
			if rel, inside := relativeIfInside(cmakeSrc, srcPath); inside {
				srcPath = rel
			}
		}
		// Out-of-tree absolute path: at this point we've
		// already filtered absolute-under-cmakeBuild (elided
		// above) and absolute-under-cmakeSrc (normalized just
		// above). What's left is absolute paths under neither
		// root — e.g. `add_library(foo /vendored/elsewhere/bar.c)`.
		// Refuse with a typed Tier-1 error so the operator
		// sees the broken cmake call, not a downstream Bazel
		// load error.
		if filepath.IsAbs(srcPath) {
			return nil, failure.New(failure.UnsupportedSourcePath,
				"target %q references source %q at an absolute path outside the project source tree (%s) and the build tree (%s); Bazel labels must be package-relative",
				t.Name, srcPath, cmakeSrc, cmakeBuild)
		}
		// Strip a leading "./". cmake's parser usually
		// normalizes these away but pathological inputs can
		// survive. "./foo.c" and "foo.c" name the same file;
		// the prefix is a no-op we silently absorb so the
		// label is valid.
		for strings.HasPrefix(srcPath, "./") {
			srcPath = srcPath[2:]
		}
		// Refuse paths with `..` segments: Bazel labels can't
		// escape their package. Allowing one would either
		// generate an out-of-package label (rejected by Bazel
		// load) or silently shift the file to a different
		// package (broken without warning). The clean refusal
		// surfaces the cmake misuse explicitly.
		if pathHasDotDotSegment(srcPath) {
			return nil, failure.New(failure.UnsupportedSourcePath,
				"target %q references source %q whose path escapes the project source tree via `..` segments; Bazel labels must stay within the package",
				t.Name, src.Path)
		}
		// Empty after normalization (only possible from the
		// "./" strip on a pathological input like "./" alone)
		// or single ".": refuse — there's no useful label here.
		if srcPath == "" || srcPath == "." {
			return nil, failure.New(failure.UnsupportedSourcePath,
				"target %q references source %q which normalizes to an empty Bazel label",
				t.Name, src.Path)
		}
		// Now safe to append.
		src.Path = srcPath
		// Confirm the source actually exists on disk at convert
		// time. cmake's target model lists sources as add_executable
		// / add_library / target_sources(...) receive them, without
		// checking existence; a static path can legitimately enter
		// the model and survive configure even when the file isn't
		// in the source tree the converter sees (e.g. the producer's
		// tarball pruned the tests/ subtree but kept the
		// add_executable(test_x tests/...) entry). Letting them
		// through emits a BUILD whose `srcs = ["tests/x.cpp"]`
		// label Bazel rejects at build time with "missing input
		// file". Drop the missing source here with an audit tag
		// so the surviving cc_library still builds; #209.
		//
		// The check is gated on hostSrcOnDisk because reply-dir-only
		// runs (golden tests, offline replay) point cmakeSrc at a
		// path the recording machine had but this host doesn't, and
		// elision against that absent root would drop every source.
		if hostSrcOnDisk {
			onDisk := src.Path
			if !filepath.IsAbs(onDisk) {
				onDisk = filepath.Join(hostSrc, src.Path)
			}
			if _, statErr := os.Stat(onDisk); statErr != nil && errors.Is(statErr, fs.ErrNotExist) {
				elidedMissingSrc = true
				continue
			}
		}
		irt.Srcs = append(irt.Srcs, src.Path)
	}
	if consumesCodegen {
		irt.Tags = append(irt.Tags, "has-cmake-codegen")
	}
	if elidedBuildDirSrc {
		irt.Tags = append(irt.Tags, "cmake-elided-build-dir-source")
	}
	if elidedMissingSrc {
		irt.Tags = append(irt.Tags, "cmake-elided-missing-source")
	}
	if elidedCompilerArtifact {
		irt.Tags = append(irt.Tags, "cmake-elided-compiler-artifact")
	}

	// Build-dir-rooted includes (relative to the cmake build
	// dir). Populated alongside the source-tree includes in
	// the CompileGroup walk; used afterward for configure_file
	// consumer attribution. Empty for targets that don't
	// include any build-dir path — they consume no
	// configure_file outputs.
	targetBuildIncs := map[string]bool{}

	if len(t.CompileGroups) > 0 {
		// M1 assumption: at most one language per target. Aggregate the
		// first compile group's flags/includes/defines.
		cg := t.CompileGroups[0]
		copts, defs := splitCompileFragments(cg.CompileCommandFragments)
		// LanguageStandard: cmake records the resolved
		// CMAKE_<LANG>_STANDARD value (e.g. "17" for cxx_std_17)
		// per CompileGroup. Most projects already see the standard
		// materialize as a `-std=…` fragment in
		// CompileCommandFragments (cmake's generator inlines it
		// there), so the prepend only fires when the codemodel
		// records the standard but the fragment didn't pick it
		// up — covers projects using target_compile_features
		// without an explicit -std fragment. Idempotent guard:
		// skip when copts already names a -std=… flag.
		copts = prependLanguageStandardCopt(cg.Language, cg.LanguageStandard, copts)
		// Apple framework search paths: CompileGroup.Frameworks
		// records -F directives cmake emits for `#include
		// <Foo/Bar.h>` framework header lookup. Empty on
		// non-Apple targets. Append as -F<path> copts; gcc /
		// clang accept this form for compile-time framework
		// search.
		for _, fw := range cg.Frameworks {
			if fw.Path == "" {
				continue
			}
			copts = append(copts, "-F"+fw.Path)
		}
		irt.Copts = copts

		for _, d := range cg.Defines {
			defs = append(defs, d.Define)
		}
		irt.Defines = defs

		// Phase 1 task 3 extension: tag targets that declare
		// target_precompile_headers. The codemodel records the
		// PCH set in CompileGroup.PrecompileHeaders; the PCH
		// headers themselves are typically already in t.Sources
		// (and route through the standard srcs/hdrs walk). The
		// tag surfaces the cmake-side PCH intent so operators
		// can grep `cmake-codegen-pch` and route via a
		// cc_toolchain pch feature (Bazel cc_library has no
		// native PCH attribute).
		for _, cgPCH := range t.CompileGroups {
			if len(cgPCH.PrecompileHeaders) > 0 {
				if !stringSliceContains(irt.Tags, "cmake-codegen-pch") {
					irt.Tags = append(irt.Tags, "cmake-codegen-pch")
				}
				break
			}
		}

		// Dedup includes: cmake's codemodel emits one entry per
		// PUBLIC include propagation, so a target whose own
		// target_include_directories names "include" plus a PUBLIC
		// dep that also names "include" surfaces with two identical
		// entries. The emitter sorts but doesn't dedup; bazel
		// accepts duplicates but they're cosmetic noise. Dedup-while-
		// preserving-order at IR-build time so any downstream
		// consumer of irt.Includes sees a clean list.
		//
		// PRIVATE-include partition: when a trace record (loaded
		// in ToIR) marks an absolute include path as PRIVATE for
		// this target, the dir flows into the cc_library's compile-
		// only `copts = ["-I<dir>"]` rather than the
		// consumer-visible `includes` attribute. cmake's PUBLIC
		// keyword propagates to consumers; PRIVATE doesn't —
		// Bazel's `includes` is consumer-visible by default, so
		// PRIVATE has to ride on -I in copts to preserve
		// encapsulation.
		seenInc := map[string]bool{}
		for _, inc := range cg.Includes {
			if rel, ok := relativeIfInsideRelaxed(cmakeBuild, inc.Path); ok {
				// Build-dir include — codemodel records this
				// for targets that target_include_directories'd
				// $<BUILD_INTERFACE:${CMAKE_CURRENT_BINARY_DIR}>
				// (typical configure_file consumer pattern).
				// Track for the configure_file consumer
				// attribution below; don't surface in
				// irt.Includes (source-tree-relative only).
				targetBuildIncs[rel] = true
				continue
			}
			rel, ok := relativeIfInside(cmakeSrc, inc.Path)
			if !ok {
				continue
			}
			if seenInc[rel] {
				continue
			}
			seenInc[rel] = true
			if privateIncludeDirs[inc.Path] {
				// Compile-only — don't propagate to consumers.
				irt.Copts = append(irt.Copts, "-I"+rel)
				continue
			}
			irt.Includes = append(irt.Includes, rel)
		}
	}

	// configure_file consumer attribution. Any target whose
	// codemodel-recorded includes contain the cmake build dir
	// (or a subdir thereof) is a candidate consumer of
	// configure_file outputs that landed inside that include
	// path. We add each matching output to the target's hdrs;
	// the genrule that produces it is already on cc.Genrules
	// from recoverConfigureFiles. cmake-codegen tag mirrors the
	// existing CUSTOM_COMMAND-recovered consumer's shape.
	if len(configureFiles) > 0 && len(targetBuildIncs) > 0 {
		var addedHdrs []string
		for _, cfgOut := range configureFiles {
			for inc := range targetBuildIncs {
				if isPathPrefix(inc, cfgOut.RelOutput) {
					addedHdrs = append(addedHdrs, cfgOut.RelOutput)
					break
				}
			}
		}
		if len(addedHdrs) > 0 {
			irt.Hdrs = append(irt.Hdrs, addedHdrs...)
			irt.Tags = append(irt.Tags, "has-cmake-codegen")
		}
	}

	// file(GENERATE) consumer attribution. Sister block to the
	// configure_file walk above; file(GENERATE) outputs can be
	// any extension (config-shaped .h, license blobs, generated
	// source, .json manifests, etc.) so we branch on extension
	// like the execute_process walk below — headers go to hdrs,
	// other artefacts go to srcs.
	if len(fileGenerates) > 0 && len(targetBuildIncs) > 0 {
		var addedHdrs, addedSrcs []string
		seenHdr := map[string]bool{}
		seenSrc := map[string]bool{}
		for _, fg := range fileGenerates {
			match := false
			for inc := range targetBuildIncs {
				if isPathPrefix(inc, fg.RelOutput) {
					match = true
					break
				}
			}
			if !match {
				continue
			}
			ext := strings.ToLower(filepath.Ext(fg.RelOutput))
			if headerExts[ext] {
				if !seenHdr[fg.RelOutput] {
					seenHdr[fg.RelOutput] = true
					addedHdrs = append(addedHdrs, fg.RelOutput)
				}
				continue
			}
			if !seenSrc[fg.RelOutput] {
				seenSrc[fg.RelOutput] = true
				addedSrcs = append(addedSrcs, fg.RelOutput)
			}
		}
		if len(addedHdrs) > 0 {
			irt.Hdrs = append(irt.Hdrs, addedHdrs...)
		}
		if len(addedSrcs) > 0 {
			irt.Srcs = append(irt.Srcs, addedSrcs...)
		}
		if len(addedHdrs) > 0 || len(addedSrcs) > 0 {
			irt.Tags = append(irt.Tags, "has-cmake-codegen")
		}
	}

	// execute_process consumer attribution. Sister block to the
	// configure_file walk above, applied to the recovered
	// execute_process outputs (cmake -E touch / copy /
	// copy_if_different + the file-producing hoist's
	// OUTPUT_FILE). Without this, a target that
	// target_include_directories'd ${CMAKE_CURRENT_BINARY_DIR}
	// and #includes (or #compiles) a file an
	// execute_process(... OUTPUT_FILE generated.h) call produces
	// would resolve at analysis time but fail at action time —
	// the genrule landed on cc.Genrules but no cc target's
	// hdrs/srcs referenced it, so Bazel's sandbox wouldn't stage
	// the recovered artefact for the consumer's compile. Branch
	// on extension so headers go to hdrs (publicly exposed) and
	// other generated artefacts go to srcs (private compile
	// inputs); same shape as the IsGenerated branch in the
	// CompileGroup walk above.
	if len(executeProcesses) > 0 && len(targetBuildIncs) > 0 {
		var addedHdrs, addedSrcs []string
		seenHdr := map[string]bool{}
		seenSrc := map[string]bool{}
		for _, ep := range executeProcesses {
			match := false
			for inc := range targetBuildIncs {
				if isPathPrefix(inc, ep.RelOutput) {
					match = true
					break
				}
			}
			if !match {
				continue
			}
			ext := strings.ToLower(filepath.Ext(ep.RelOutput))
			if headerExts[ext] {
				if !seenHdr[ep.RelOutput] {
					seenHdr[ep.RelOutput] = true
					addedHdrs = append(addedHdrs, ep.RelOutput)
				}
				continue
			}
			if !seenSrc[ep.RelOutput] {
				seenSrc[ep.RelOutput] = true
				addedSrcs = append(addedSrcs, ep.RelOutput)
			}
		}
		if len(addedHdrs) > 0 {
			irt.Hdrs = append(irt.Hdrs, addedHdrs...)
		}
		if len(addedSrcs) > 0 {
			irt.Srcs = append(irt.Srcs, addedSrcs...)
		}
		if len(addedHdrs) > 0 || len(addedSrcs) > 0 {
			irt.Tags = append(irt.Tags, "has-cmake-codegen")
		}
	}

	hdrs, err := discoverHeaders(hostSrc, irt.Includes, cc.HeaderWalkCache)
	if err != nil {
		return nil, err
	}
	// FileSets give cmake's authoritative list of HEADERS-typed sources
	// declared via `target_sources(... FILE_SET HEADERS ...)`. The
	// filesystem walk above catches anything under irt.Includes, but a
	// FILE_SET can name a header outside any declared include dir;
	// merging the two lists ensures those don't get dropped. Sources
	// without a FileSetIndex (the common case in M1 fixtures) leave
	// fileSetHdrs empty and behavior matches the pre-FileSets path.
	var fileSetHdrs []string
	if len(t.FileSets) > 0 {
		for _, src := range t.Sources {
			if src.FileSetIndex == nil {
				continue
			}
			idx := *src.FileSetIndex
			if idx < 0 || idx >= len(t.FileSets) {
				continue
			}
			if t.FileSets[idx].Type != "HEADERS" {
				continue
			}
			fileSetHdrs = append(fileSetHdrs, src.Path)
		}
	}
	merged := append(irt.Hdrs, hdrs...)
	merged = append(merged, fileSetHdrs...)
	sort.Strings(merged)
	irt.Hdrs = dedupeStrings(merged)

	// Lower dependencies. In-codebase target ids look like `<name>::@<hash>`
	// where <name> is the CMake target name; out-of-tree find_package-
	// imported targets carry a namespaced name like `Pkg::tgt::@<hash>`.
	// Resolution order:
	//
	//   1. In-codebase non-UTILITY target -> ":<name>"
	//   2. In-codebase UTILITY target -> skip silently (no Bazel equivalent)
	//   3. CMake target name in imports manifest -> bazel_label
	//   4. Otherwise -> Tier-1 unresolved-link-dep.
	//
	// Phase 4 enrichment: when traceLinkScope (the cmake-side
	// PUBLIC/PRIVATE/INTERFACE keyword recovered by shadow.Decode
	// from the trace) records this dep as PRIVATE, route the
	// resolved Bazel label to irt.ImplementationDeps rather than
	// irt.Deps — matching `implementation_deps = [...]` on the
	// emitted cc_library, which prevents the dep's headers from
	// propagating to consumers. Unknown scope (no trace, or the
	// dep wasn't named in any target_link_libraries call —
	// possible for cmake-generated dependency edges that aren't
	// user-authored, like cyclical-static-archive helpers) falls
	// through to the historical `irt.Deps` routing — strictly
	// safe (PUBLIC default forwards headers, which is the cmake
	// default for non-keyword target_link_libraries usage).
	//
	// Rule-kind gating: only cc_library supports
	// `implementation_deps` in stock rules_cc. cc_binary /
	// cc_test / cc_import don't accept the attribute, and bazel
	// rejects it at analysis time. For non-library kinds the
	// scope distinction is moot — a binary has no consumers
	// (it's a leaf), and a cc_import wraps a pre-built archive
	// whose link interface is fixed at build time — so fold
	// PRIVATE deps into `irt.Deps` for those kinds.
	allowsImplementationDeps := irt.Kind == ir.KindCCLibrary
	// Issue #194: cmake's codemodel can report the same
	// dependency twice — e.g. a target referenced by
	// target_link_libraries with both INTERFACE and PRIVATE
	// visibility, or referenced by separate
	// target_link_libraries invocations. Without a seen-set,
	// both entries land in irt.Deps and the rendered BUILD.bazel
	// carries a duplicate label that Bazel rejects ("Label '...'
	// is duplicated in the 'deps' attribute"). The Link.CommandFragments
	// loop below already has a seen-set spanning Deps +
	// ImplementationDeps (to avoid cross-bucket dupes); this
	// one covers within-loop duplicates that come straight from
	// t.Dependencies. Both maps fold into the same routing-
	// decision shape so a dep already in either bucket isn't
	// re-appended.
	seenDep := map[string]bool{}
	for _, dep := range t.Dependencies {
		// Resolve label; routing decision (Deps vs
		// ImplementationDeps) folds in after.
		var label string
		if name, ok := idToName[dep.Id]; ok {
			label = ":" + name
		} else if utilityIDs[dep.Id] {
			continue
		} else {
			cmakeName := stripIDHash(dep.Id)
			if export := imports.LookupCMakeTarget(cmakeName); export != nil {
				label = export.BazelLabel
			} else {
				return nil, failure.New(failure.UnresolvedLinkDep,
					"target %q depends on %q which is neither in-codebase nor in the imports manifest",
					t.Name, cmakeName)
			}
		}
		if seenDep[label] {
			continue
		}
		seenDep[label] = true
		// add_dependencies-derived edges route to data (build-
		// order only, no headers / link) rather than deps.
		// Detected via the codemodel's TargetDependency backtrace:
		// when the recorded command is "add_dependencies", the
		// edge has no compile/link impact. Conservative — only
		// fires when the codemodel records the call directly;
		// macro-wrapped add_dependencies stay on Deps until the
		// outermost-user-frame walk surfaces them (future slice).
		if isAddDependenciesEdge(dep, t.BacktraceGraph) {
			irt.Data = append(irt.Data, label)
			continue
		}
		if allowsImplementationDeps && depScopeIsPrivate(traceLinkScope, dep, idToName) {
			irt.ImplementationDeps = append(irt.ImplementationDeps, label)
		} else {
			irt.Deps = append(irt.Deps, label)
		}
	}

	// Out-of-tree link fragments. CMake records IMPORTED_LOCATION paths
	// in t.Link.CommandFragments[role="libraries"] as resolved absolute
	// paths under the synth-prefix tree. The orchestrator's imports
	// manifest carries each export's link paths so we can rewrite those
	// fragments to Bazel labels.
	//
	// The seen-set spans Deps + ImplementationDeps so a dep already
	// routed to either bucket by the t.Dependencies loop above
	// doesn't get re-appended to Deps here (which would duplicate
	// it across both buckets and produce an invalid BUILD).
	// INTERPROCEDURAL_OPTIMIZATION (cmake's per-target LTO toggle)
	// surfaces in the codemodel as TargetArchive.LTO (STATIC_LIBRARY)
	// or TargetLink.LTO (EXECUTABLE / SHARED_LIBRARY / MODULE_LIBRARY).
	// Map to Bazel's features=["lto"] — the operator's cc_toolchain
	// owns the actual -flto flag set; see Phase 5's
	// docs/design/sanitizer-as-feature.md for the feature-definition
	// convention (lto is in SANITIZER_FEATURES alongside the
	// sanitizers).
	if (t.Archive != nil && t.Archive.LTO) || (t.Link != nil && t.Link.LTO) {
		if !stringSliceContains(irt.Features, "lto") {
			irt.Features = append(irt.Features, "lto")
		}
	}
	if t.Link != nil {
		seen := map[string]bool{}
		for _, d := range irt.Deps {
			seen[d] = true
		}
		for _, d := range irt.ImplementationDeps {
			seen[d] = true
		}
		for _, frag := range t.Link.CommandFragments {
			// Non-library fragments (flags / libraryPath /
			// frameworkPath / frameworks) route directly to
			// linkopts. cmake's codemodel exposes the per-fragment
			// role so we can attribute correctly; the existing
			// "libraries"-only path below handles deps wiring.
			switch frag.Role {
			case "flags":
				if v := strings.TrimSpace(frag.Fragment); v != "" {
					irt.LinkOpts = append(irt.LinkOpts, v)
				}
				continue
			case "libraryPath":
				if v := strings.TrimSpace(frag.Fragment); v != "" {
					irt.LinkOpts = append(irt.LinkOpts, "-L"+v)
				}
				continue
			case "frameworkPath":
				if v := strings.TrimSpace(frag.Fragment); v != "" {
					irt.LinkOpts = append(irt.LinkOpts, "-F"+v)
				}
				continue
			case "frameworks":
				// cmake records the framework NAME; gcc/clang
				// expect `-framework Foo`. Emit as two separate
				// args via the canonical two-token form.
				if v := strings.TrimSpace(frag.Fragment); v != "" {
					irt.LinkOpts = append(irt.LinkOpts, "-framework", v)
				}
				continue
			}
			if frag.Role != "libraries" {
				continue
			}
			path := strings.TrimSpace(frag.Fragment)
			if path == "" || !filepath.IsAbs(path) {
				continue
			}
			if hostPrefix != "" && strings.HasPrefix(path, hostPrefix+string(filepath.Separator)) {
				path = manifestPrefixAnchor + path[len(hostPrefix)+1:]
			}
			if export := imports.LookupLinkPath(path); export != nil {
				if !seen[export.BazelLabel] {
					seen[export.BazelLabel] = true
					// Trace-scope routing: if the
					// underlying cmake lib name is recorded
					// PRIVATE in traceLinkScope AND the
					// target accepts implementation_deps
					// (cc_library only), route to
					// ImplementationDeps. Otherwise fold
					// into Deps — cc_binary / cc_test /
					// cc_import don't accept the attribute.
					if allowsImplementationDeps && traceLinkScope != nil && scopeForLabelLib(traceLinkScope, export.CMakeTarget) == "PRIVATE" {
						irt.ImplementationDeps = append(irt.ImplementationDeps, export.BazelLabel)
					} else {
						irt.Deps = append(irt.Deps, export.BazelLabel)
					}
				}
			}
		}
	}

	// STATIC IMPORTED dep recovery from trace. STATIC archives don't
	// run a link step at build time, so cmake's codemodel records
	// no Link.CommandFragments and no IMPORTED-target Dependencies
	// for them — both upstream channels are empty. Trace's
	// target_link_libraries calls are the only ground truth for a
	// static lib's IMPORTED deps. For each lib name the trace
	// records, look it up in the imports manifest; resolve hits
	// are appended (deduped). Non-IMPORTED libs (in-codebase target
	// names) already came in via t.Dependencies above and are
	// covered by the seen-map.
	//
	// The seen-set spans Deps + ImplementationDeps so a dep already
	// routed to ImplementationDeps by the t.Dependencies loop above
	// doesn't get re-appended to Deps here.
	if t.Type == "STATIC_LIBRARY" && len(traceLinkLibs) > 0 {
		seen := map[string]bool{}
		for _, d := range irt.Deps {
			seen[d] = true
		}
		for _, d := range irt.ImplementationDeps {
			seen[d] = true
		}
		for _, lib := range traceLinkLibs {
			if export := imports.LookupCMakeTarget(lib); export != nil {
				if !seen[export.BazelLabel] {
					seen[export.BazelLabel] = true
					// Trace-scope routing: route PRIVATE
					// imports to ImplementationDeps only on
					// targets that accept the attribute
					// (cc_library — the STATIC_LIBRARY guard
					// above already ensures this for the
					// outer if, but make the rule-kind check
					// explicit for parallel structure with
					// the t.Dependencies + Link.CommandFragments
					// paths). Otherwise fold into Deps.
					if allowsImplementationDeps && traceLinkScope != nil && traceLinkScope[lib] == "PRIVATE" {
						irt.ImplementationDeps = append(irt.ImplementationDeps, export.BazelLabel)
					} else {
						irt.Deps = append(irt.Deps, export.BazelLabel)
					}
				}
			}
		}
	}

	if t.Install != nil && len(t.Install.Destinations) > 0 {
		irt.Visibility = []string{"//visibility:public"}
		irt.InstallDest = t.Install.Destinations[0].Path
	}

	if len(t.Artifacts) > 0 {
		irt.ArtifactName = t.Artifacts[0].Path
	} else if t.NameOnDisk != "" {
		irt.ArtifactName = t.NameOnDisk
	}

	switch {
	case t.Link != nil && t.Link.Language != "":
		irt.LinkLanguage = t.Link.Language
	case len(t.CompileGroups) > 0:
		irt.LinkLanguage = t.CompileGroups[0].Language
	}

	// CTest classification. An EXECUTABLE registered via add_test() is
	// rewritten as one or more cc_test rules — one per registration —
	// each sharing the cc_binary's srcs/hdrs/copts/deps. The cc_binary
	// itself is dropped (return nil) since the test executable is
	// addressable as a cc_test label after rewriting.
	if irt.Kind == ir.KindCCBinary && tests != nil {
		regs := tests.Lookup(t.Name)
		if len(regs) > 0 {
			for _, reg := range regs {
				cct := *irt
				cct.Name = reg.Name
				cct.Kind = ir.KindCCTest
				cct.TestArgs = append([]string(nil), reg.Args...)
				cct.TestEnv = append([]string(nil), reg.Env...)
				cct.TestData = append([]string(nil), reg.Data...)
				cct.TestTimeout = reg.Timeout
				if len(reg.Tags) > 0 {
					seen := make(map[string]bool, len(cct.Tags)+len(reg.Tags))
					merged := append([]string(nil), cct.Tags...)
					for _, x := range cct.Tags {
						seen[x] = true
					}
					for _, x := range reg.Tags {
						if seen[x] {
							continue
						}
						seen[x] = true
						merged = append(merged, x)
					}
					cct.Tags = merged
				}
				cc.Tests = append(cc.Tests, cct)
			}
			return nil, nil
		}
	}

	// Multi-language structural split: cmake records one
	// CompileGroup per language with that language's flags. A
	// single Bazel cc_library can't carry per-source-language
	// copts, so split into a wrapper cc_library (the user-
	// visible target name, deps-only) plus one private
	// sub-library per language with that language's srcs +
	// flags. Single-language targets stay one cc_library; this
	// branch fires for either multi-language CGs OR multi-CG-
	// per-language with differing Defines / CompileCommandFragments
	// (Phase 1 task 3: per-source-defines case where cmake's
	// codemodel partitions sources via CompileGroupIndex).
	subsBefore := len(cc.Subs)
	if shouldSplitCompileGroups(t) {
		if err := splitMultiLanguage(t, irt, cc); err != nil {
			return nil, err
		}
	}

	// #217 Tier 1: partition flat Srcs by trace-recovered
	// platform conditionality. For each src whose trace
	// attribution names a Bazel constraint label, move it from
	// .Srcs to .PerPlatform["srcs"][selectKey] so the emitter
	// renders a select() arm. Sources without an attribution
	// stay in flat srcs, preserving byte-stable emission for
	// projects without platform conditionals.
	//
	// Apply to the wrapper AND to any sub-libraries that
	// splitMultiLanguage just appended to cc.Subs. The wrapper
	// case covers single-language targets (where irt.Srcs
	// carries everything); the sub-library case covers
	// multi-language targets (where splitMultiLanguage cleared
	// irt.Srcs and distributed sources across per-language sub-
	// libraries — those subs carry the conditional sources now
	// and need partitioning too).
	if len(platformConditionalSrcs) > 0 {
		partitionPlatformConditionalSrcs(irt, platformConditionalSrcs)
		for i := subsBefore; i < len(cc.Subs); i++ {
			partitionPlatformConditionalSrcs(&cc.Subs[i], platformConditionalSrcs)
		}
	}

	return irt, nil
}

// partitionPlatformConditionalSrcs moves any src in t.Srcs
// whose path appears in srcToSelectKey into
// t.PerPlatform["srcs"][selectKey], then sorts each affected
// arm so emit's verbatim arm rendering is byte-stable.
//
// Used by lowerTarget to apply the #217 Tier 1 partition both
// to the wrapper target and to splitMultiLanguage's per-
// language sub-libraries (which inherit the wrapper's
// trace-recovered conditionality map — the trace records
// (target, src, selectKey) without sub-library scope).
func partitionPlatformConditionalSrcs(t *ir.Target, srcToSelectKey map[string]string) {
	if len(srcToSelectKey) == 0 || len(t.Srcs) == 0 {
		return
	}
	touchedArms := map[string]bool{}
	var kept []string
	for _, src := range t.Srcs {
		if key, ok := srcToSelectKey[src]; ok {
			if t.PerPlatform == nil {
				t.PerPlatform = map[string]map[string][]string{}
			}
			if t.PerPlatform["srcs"] == nil {
				t.PerPlatform["srcs"] = map[string][]string{}
			}
			t.PerPlatform["srcs"][key] = append(t.PerPlatform["srcs"][key], src)
			touchedArms[key] = true
			continue
		}
		kept = append(kept, src)
	}
	t.Srcs = kept
	// Sort each touched arm for byte-stable BUILD output. emit
	// renders `select({key: [...]})` arms verbatim;
	// elementfold sorts at fold time but on this trace-only
	// partition path there's no fold. Sort once per arm we
	// actually populated rather than walking every PerPlatform
	// entry the target might already have from other passes.
	for key := range touchedArms {
		sort.Strings(t.PerPlatform["srcs"][key])
	}
}

// shouldSplitCompileGroups reports whether the target's sources
// fall into ≥ 2 CompileGroups that need separate per-CG attribution
// — either multi-language (the historical trigger) or
// same-language with differing Defines / CompileCommandFragments
// (the per-source-defines case Phase 1 task 3 covers: cmake's
// codemodel partitions sources via CompileGroupIndex when
// set_source_files_properties or target_sources(PRIVATE FILE_SET)
// gives sources differing compile contexts).
//
// Same-language CGs with identical Defines + CompileCommandFragments
// don't trigger the split — they're a degenerate codemodel shape
// (cmake occasionally emits multiple identical CGs as a
// generator-side artifact) and merging them into one set keeps the
// output compact.
func shouldSplitCompileGroups(t *fileapi.Target) bool {
	if len(t.CompileGroups) < 2 {
		return false
	}
	// Multi-language: existing case.
	langs := map[string]bool{}
	for _, cg := range t.CompileGroups {
		if cg.Language == "" {
			continue
		}
		langs[cg.Language] = true
	}
	if len(langs) >= 2 {
		return true
	}
	// Single-language, multi-CG. Split when any pair differs in
	// the attribution-affecting attrs.
	type sig struct{ defs, cmdFrags string }
	seen := map[string]sig{}
	for _, cg := range t.CompileGroups {
		if cg.Language == "" {
			continue
		}
		s := sig{
			defs:     joinDefines(cg.Defines),
			cmdFrags: joinFragments(cg.CompileCommandFragments),
		}
		if prev, ok := seen[cg.Language]; ok {
			if prev != s {
				return true
			}
		} else {
			seen[cg.Language] = s
		}
	}
	return false
}

// shouldSplitMultiLanguage is the legacy name for the gate;
// preserved as a thin alias for any external test consumer that
// still references it.
func shouldSplitMultiLanguage(t *fileapi.Target) bool {
	return shouldSplitCompileGroups(t)
}

// joinDefines / joinFragments produce a stable string signature
// for a CompileGroup's Defines / CompileCommandFragments lists.
// Used by the gate to compare two same-language CGs for
// attribution-affecting differences.
func joinDefines(defs []fileapi.CompileDefine) string {
	parts := make([]string, len(defs))
	for i, d := range defs {
		parts[i] = d.Define
	}
	return strings.Join(parts, "\x00")
}

func joinFragments(frags []fileapi.CommandFragment) string {
	parts := make([]string, len(frags))
	for i, f := range frags {
		parts[i] = f.Role + "\x01" + f.Fragment
	}
	return strings.Join(parts, "\x00")
}

// intSuffix is itoa for the splitMultiLanguage sub-name
// disambiguator. The expected range is small (handful of CGs
// per language); avoiding strconv keeps the per-target loop's
// allocation profile predictable.
func intSuffix(n int) string {
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

// splitMultiLanguage rewrites irt as a deps-only wrapper and
// appends one per-language ir.Target to cc.Subs. Each sub-
// library carries:
//
//   - Srcs filtered to the sources cmake assigned to that
//     compile group.
//   - Copts + Defines extracted from that compile group's
//     command fragments (each cmake CG carries its own full
//     flag set including general flags like -O3, so per-
//     language flag isolation is automatic).
//   - The same Hdrs / Includes as the wrapper would have had
//     (cmake doesn't language-tag headers; the public include
//     surface is shared).
//   - Private visibility: only the wrapper consumes them.
//
// The wrapper drops Srcs / Copts / Defines, retains the
// public surface (hdrs, includes, visibility, install
// metadata), and adds a Deps edge to each sub-library.
func splitMultiLanguage(t *fileapi.Target, irt *ir.Target, cc *codegenContext) error {
	// Sort CompileGroups by language for deterministic sub-
	// library ordering across runs (the codemodel records them
	// in source-declaration order, which is stable but harder
	// to reason about).
	groups := append([]fileapi.CompileGroup(nil), t.CompileGroups...)
	sort.Slice(groups, func(i, j int) bool { return groups[i].Language < groups[j].Language })

	// Count CompileGroups per language so the naming loop knows
	// whether to disambiguate (multiple CGs sharing a language
	// — the per-source-defines case Phase 1 task 3 surfaces).
	// Single-CG-per-language keeps the legacy `<target>_C` /
	// `<target>_CXX` naming for byte-stable goldens; multi-CG-
	// per-language adds a stable index suffix.
	langCount := map[string]int{}
	for _, cg := range groups {
		if cg.Language != "" {
			langCount[cg.Language]++
		}
	}
	langSeen := map[string]int{} // per-language emit counter

	sharedHdrs := append([]string(nil), irt.Hdrs...)
	sharedIncludes := append([]string(nil), irt.Includes...)
	wrapperSrcs := irt.Srcs

	var subDeps []string
	for _, cg := range groups {
		if cg.Language == "" {
			continue
		}
		// Source partition: keep the entries the wrapper
		// already accepted (those that survived
		// shouldIncludeSource etc.) AND that the codemodel
		// assigned to this CG.
		srcIndex := map[int]bool{}
		for _, idx := range cg.SourceIndexes {
			srcIndex[idx] = true
		}
		var subSrcs []string
		for i, s := range t.Sources {
			if !srcIndex[i] {
				continue
			}
			// Only keep srcs that landed on the wrapper's Srcs
			// slice — the existing source-walk filtering
			// (.rule files, generated sources we couldn't
			// recover, etc.) already trimmed them.
			rel := relForSource(s.Path, t)
			if !stringSliceContains(wrapperSrcs, rel) {
				continue
			}
			subSrcs = append(subSrcs, rel)
		}
		if len(subSrcs) == 0 {
			continue
		}
		copts, defs := splitCompileFragments(cg.CompileCommandFragments)
		for _, d := range cg.Defines {
			defs = append(defs, d.Define)
		}

		subName := irt.Name + "_" + langSuffix(cg.Language)
		if langCount[cg.Language] > 1 {
			// Multi-CG-per-language: append a stable index
			// suffix per emit. The codemodel's CompileGroup order
			// is stable; sort by Language above keeps the
			// per-language sub-order stable too. The index reflects
			// emit order within the language, not the codemodel's
			// raw index — easier for operators to grep.
			subName = subName + "_" + intSuffix(langSeen[cg.Language])
		}
		langSeen[cg.Language]++

		sub := ir.Target{
			Name:       subName,
			Kind:       irt.Kind,
			Srcs:       subSrcs,
			Hdrs:       sharedHdrs,
			Includes:   sharedIncludes,
			Copts:      copts,
			Defines:    defs,
			Linkstatic: irt.Linkstatic,
			Alwayslink: irt.Alwayslink,
			Visibility: []string{"//visibility:private"},
		}
		cc.Subs = append(cc.Subs, sub)
		subDeps = append(subDeps, ":"+sub.Name)
	}

	if len(subDeps) == 0 {
		// Nothing actually split (e.g. all CGs ended up
		// empty); leave the wrapper alone so the existing
		// output stays valid.
		return nil
	}

	// Wrapper: deps-only. Strip srcs/copts/defines (now on
	// sub-libs); keep hdrs/includes/visibility/install/etc.
	irt.Srcs = nil
	irt.Copts = nil
	irt.Defines = nil
	irt.Deps = append(irt.Deps, subDeps...)
	return nil
}

// langSuffix maps a cmake compile-group language string to the
// short suffix used in the synthesized sub-library name.
func langSuffix(lang string) string {
	switch lang {
	case "C":
		return "c"
	case "CXX":
		return "cxx"
	case "OBJC":
		return "objc"
	case "OBJCXX":
		return "objcxx"
	case "Fortran":
		return "fortran"
	case "ASM":
		return "asm"
	}
	return strings.ToLower(lang)
}

// findPackageHeaderComments projects configureLog find_package-v1
// events into a list of attribution lines the emitter renders as
// `# ` comments at the file head. Operators see the external-dep
// inventory at a glance without re-running the converter or
// re-reading the cmake source.
//
// Output shape: one comment per resolved package with package +
// version + config-file path. Unresolved find_package events
// (Found.IsFound==false) get a less-detailed line — they're
// usually projects building against a system-fallback path.
// Stable order (alphabetical by package name).
func findPackageHeaderComments(events []fileapi.Event) []string {
	if len(events) == 0 {
		return nil
	}
	type entry struct {
		pkg     string
		version string
		cfg     string
		found   bool
	}
	seen := map[string]bool{}
	var entries []entry
	for _, e := range events {
		if e.Kind != "find_package-v1" {
			continue
		}
		var pkg string
		if e.Found != nil && e.Found.Package != "" {
			pkg = e.Found.Package
		}
		if pkg == "" {
			continue
		}
		if seen[pkg] {
			continue
		}
		seen[pkg] = true
		ent := entry{pkg: pkg}
		if e.Found != nil {
			ent.found = e.Found.IsFound
			ent.version = e.Found.Version
			ent.cfg = e.Found.ConfigFile
		}
		entries = append(entries, ent)
	}
	if len(entries) == 0 {
		return nil
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].pkg < entries[j].pkg })
	var out []string
	out = append(out, "find_package resolutions (from cmake's configureLog):")
	for _, e := range entries {
		if e.found {
			line := "  - " + e.pkg
			if e.version != "" {
				line += " " + e.version
			}
			if e.cfg != "" {
				line += " (via " + e.cfg + ")"
			}
			out = append(out, line)
		} else {
			out = append(out, "  - "+e.pkg+" (NOT FOUND)")
		}
	}
	return out
}

// applyProbeGenexProperties walks pkg.Targets and applies
// per-target properties the probe-genex hook captured beyond the
// INTERFACE_* aggregates: BUILD_RPATH / INSTALL_RPATH → linkopts,
// POSITION_INDEPENDENT_CODE → features=["pic"] / ["-pic"],
// {CXX,C}_VISIBILITY_PRESET → copts -fvisibility=…
//
// No-op when probes is empty (probe-genex hook didn't run).
// Matching is by target name; targets not in the probe set are
// left unchanged.
//
// Phase 3 / generator-parity-gaps follow-up.
func applyProbeGenexProperties(pkg *ir.Package, probes []cmakerun.GenexProbe) {
	if len(probes) == 0 || pkg == nil {
		return
	}
	byName := map[string]cmakerun.GenexProbe{}
	for _, p := range probes {
		byName[p.Name] = p
	}
	for i := range pkg.Targets {
		tgt := &pkg.Targets[i]
		p, ok := byName[tgt.Name]
		if !ok {
			continue
		}
		// RPATH lifts to linkopts. BUILD_RPATH covers the
		// build/test-time consumer (relevant under Bazel);
		// INSTALL_RPATH covers cmake-install consumers (only
		// affects downstream cmake users). Bazel build typically
		// wants the BUILD_RPATH semantics.
		if r := strings.TrimSpace(p.Properties["BUILD_RPATH"]); r != "" {
			for _, entry := range strings.Split(r, ";") {
				if entry == "" {
					continue
				}
				tgt.LinkOpts = append(tgt.LinkOpts, "-Wl,-rpath,"+entry)
			}
		}
		// POSITION_INDEPENDENT_CODE = TRUE → features=["pic"]
		// (or "-pic" when explicitly OFF).
		if v := strings.TrimSpace(p.Properties["POSITION_INDEPENDENT_CODE"]); v != "" {
			if cmakeTruthy(v) {
				if !stringSliceContains(tgt.Features, "pic") {
					tgt.Features = append(tgt.Features, "pic")
				}
			} else if !stringSliceContains(tgt.Features, "-pic") {
				tgt.Features = append(tgt.Features, "-pic")
			}
		}
		// Visibility presets — gcc/clang -fvisibility=<value>.
		// CXX and C variants typically agree; emit each
		// separately if cmake records different values.
		for _, key := range []string{"CXX_VISIBILITY_PRESET", "C_VISIBILITY_PRESET"} {
			if v := strings.TrimSpace(p.Properties[key]); v != "" {
				flag := "-fvisibility=" + v
				if !stringSliceContains(tgt.Copts, flag) {
					tgt.Copts = append(tgt.Copts, flag)
				}
			}
		}
		// VISIBILITY_INLINES_HIDDEN: common modern-cmake idiom
		// (set in projects that follow the GenerateExportHeader
		// recipe). Maps to -fvisibility-inlines-hidden copt.
		if cmakeTruthy(p.Properties["VISIBILITY_INLINES_HIDDEN"]) {
			flag := "-fvisibility-inlines-hidden"
			if !stringSliceContains(tgt.Copts, flag) {
				tgt.Copts = append(tgt.Copts, flag)
			}
		}
		// ENABLE_EXPORTS (executables) tags so operators can
		// route the export-symbols-from-executable shape via
		// the operator's cc_toolchain feature. Bazel cc_binary
		// has no native attribute for this.
		if cmakeTruthy(p.Properties["ENABLE_EXPORTS"]) {
			tag := "cmake-codegen-enable-exports"
			if !stringSliceContains(tgt.Tags, tag) {
				tgt.Tags = append(tgt.Tags, tag)
			}
		}
		// SOVERSION / VERSION (shared library naming). Bazel
		// cc_library has no version-suffix attribute; surface
		// as tags so operators see the cmake-side intent.
		if v := strings.TrimSpace(p.Properties["SOVERSION"]); v != "" {
			tag := "cmake-codegen-soversion=" + v
			if !stringSliceContains(tgt.Tags, tag) {
				tgt.Tags = append(tgt.Tags, tag)
			}
		}
		if v := strings.TrimSpace(p.Properties["VERSION"]); v != "" {
			tag := "cmake-codegen-version=" + v
			if !stringSliceContains(tgt.Tags, tag) {
				tgt.Tags = append(tgt.Tags, tag)
			}
		}
	}
}

// reclassifyHeaderOnlySources walks every target in pkg.Targets
// and moves srcs entries the headerOnlySources set marks into
// hdrs. Iterates pkg.Targets by index so the mutation is in
// place; preserves the rest of the slice byte-for-byte when no
// reclassification happens. No-op when headerOnlySources is
// empty.
//
// Idempotent on repeat calls: an entry already in hdrs (via the
// codemodel's hdrs walk) gets deduped silently.
func reclassifyHeaderOnlySources(pkg *ir.Package, headerOnlySources map[string]bool) {
	if len(headerOnlySources) == 0 || pkg == nil {
		return
	}
	for i := range pkg.Targets {
		tgt := &pkg.Targets[i]
		if tgt.Kind != ir.KindCCLibrary && tgt.Kind != ir.KindCCBinary &&
			tgt.Kind != ir.KindCCInterface && tgt.Kind != ir.KindCCTest {
			continue
		}
		var keptSrcs []string
		for _, src := range tgt.Srcs {
			if headerOnlySources[src] {
				// Skip duplicate add: matches the codemodel hdrs
				// walk's first-write-wins.
				if !stringSliceContains(tgt.Hdrs, src) {
					tgt.Hdrs = append(tgt.Hdrs, src)
				}
				continue
			}
			keptSrcs = append(keptSrcs, src)
		}
		tgt.Srcs = keptSrcs
	}
}

// collectObjectDepends walks the decoded
// set_source_files_properties calls and returns a map from
// source path → list of header dependency paths declared via
// the OBJECT_DEPENDS property. cmake's documented contract for
// OBJECT_DEPENDS is "additional inputs that should trigger a
// rebuild" — the Bazel-idiomatic mapping is adding the entries
// to the consuming target's hdrs.
//
// Multiple OBJECT_DEPENDS declarations for the same source merge
// (additive); duplicate header entries get deduped at the
// reclassify-pass.
//
// Returns nil when no OBJECT_DEPENDS surfaces.
func collectObjectDepends(calls []shadow.SourceFilePropertiesCall) map[string][]string {
	var out map[string][]string
	for _, call := range calls {
		for _, prop := range call.Properties {
			if !strings.EqualFold(prop.Name, "OBJECT_DEPENDS") {
				continue
			}
			if prop.Value == "" {
				continue
			}
			// cmake list semantics: semicolon separator.
			headers := strings.Split(prop.Value, ";")
			for _, f := range call.Files {
				if out == nil {
					out = map[string][]string{}
				}
				for _, h := range headers {
					if h == "" {
						continue
					}
					out[f] = append(out[f], h)
				}
			}
		}
	}
	return out
}

// addObjectDependsHeaders walks every target in pkg.Targets and
// appends each source's OBJECT_DEPENDS-declared headers to the
// target's hdrs. Idempotent on repeat headers (skips entries
// already present in hdrs); preserves the rest of the target
// unchanged.
//
// Runs after the HEADER_FILE_ONLY reclassify so any source that
// was both HEADER_FILE_ONLY AND had OBJECT_DEPENDS-style header
// deps still surfaces its declared deps in hdrs.
func addObjectDependsHeaders(pkg *ir.Package, byPath map[string][]string) {
	if len(byPath) == 0 || pkg == nil {
		return
	}
	for i := range pkg.Targets {
		tgt := &pkg.Targets[i]
		if tgt.Kind != ir.KindCCLibrary && tgt.Kind != ir.KindCCBinary &&
			tgt.Kind != ir.KindCCInterface && tgt.Kind != ir.KindCCTest {
			continue
		}
		// Walk both srcs and hdrs to find sources that declare
		// OBJECT_DEPENDS — the headers might be on a HEADER_FILE_ONLY
		// entry that the reclassify pass moved to hdrs already.
		seen := map[string]bool{}
		for _, h := range tgt.Hdrs {
			seen[h] = true
		}
		for _, src := range append(append([]string(nil), tgt.Srcs...), tgt.Hdrs...) {
			for _, h := range byPath[src] {
				if seen[h] {
					continue
				}
				seen[h] = true
				tgt.Hdrs = append(tgt.Hdrs, h)
			}
		}
	}
}

// collectHeaderOnlySources walks the decoded
// set_source_files_properties calls and returns the slash-form
// paths declared with HEADER_FILE_ONLY=TRUE. Truthy values per
// cmake's boolean convention: TRUE / ON / YES / 1 (case-insensitive).
//
// The returned map's keys are source paths as the trace recorded
// them — typically relative to the call's source dir. Source
// walks compare against TargetSource.Path (also relative); when
// the project uses absolute paths, the lookup falls through and
// the source stays in srcs (best-effort gap fill, not a strict
// guarantee).
//
// Returns nil when no HEADER_FILE_ONLY property surfaces; callers
// treat nil as the empty set without a separate check.
func collectHeaderOnlySources(calls []shadow.SourceFilePropertiesCall) map[string]bool {
	var out map[string]bool
	for _, call := range calls {
		for _, prop := range call.Properties {
			if !strings.EqualFold(prop.Name, "HEADER_FILE_ONLY") {
				continue
			}
			if !cmakeTruthy(prop.Value) {
				continue
			}
			if out == nil {
				out = map[string]bool{}
			}
			for _, f := range call.Files {
				out[f] = true
			}
		}
	}
	return out
}

// cmakeTruthy mirrors cmake's documented truthy interpretation
// for boolean cache values: TRUE / ON / YES / 1 / Y are true
// (case-insensitive); everything else is false.
//
// Stricter than cmake's full if() type-coerced evaluator —
// HEADER_FILE_ONLY's documented contract is a plain boolean,
// not a generic expression, so the narrow form is correct here.
func cmakeTruthy(v string) bool {
	switch strings.ToUpper(strings.TrimSpace(v)) {
	case "TRUE", "ON", "YES", "1", "Y":
		return true
	}
	return false
}

// isAddDependenciesEdge reports whether a TargetDependency came
// from an `add_dependencies(target dep)` call rather than a
// target_link_libraries (or similar) — the former carries no
// compile/link facts; only build order matters. Bazel maps the
// former to `data = [...]`.
//
// Detection: the dep's Backtrace points at a BacktraceGraph node;
// if its command is "add_dependencies", route to data. Conservative
// — macro-wrapped add_dependencies (where the leaf backtrace
// points inside a macro definition) stay on the link path until
// the outermost-user-frame walk surfaces them. The trade-off:
// over-emit link edges for the macro case (safe but redundant)
// vs miss build-order semantics for the direct case (currently
// silent). The direct case is the common one.
func isAddDependenciesEdge(dep fileapi.TargetDependency, g fileapi.BacktraceGraph) bool {
	if dep.Backtrace <= 0 || dep.Backtrace >= len(g.Nodes) {
		return false
	}
	node := g.Nodes[dep.Backtrace]
	if node.Command < 0 || node.Command >= len(g.Commands) {
		return false
	}
	return strings.EqualFold(g.Commands[node.Command], "add_dependencies")
}

// prependLanguageStandardCopt augments copts with a `-std=…`
// flag derived from cmake's CompileGroup.LanguageStandard when
// the existing copts don't already name a -std flag. The
// LanguageStandard.Standard value is the bare number cmake
// records ("17" for c++17, "11" for c11, etc.); the formatter
// emits gcc/clang-style `-std=cXX` / `-std=c++XX`.
//
// Idempotency: many cmake projects already see the standard
// inlined into CompileCommandFragments by cmake's generator
// (e.g. `-std=gnu++17` appears in copts directly). The
// `-std=`-prefix check skips the prepend in that case so the
// emitted copts stay byte-stable.
//
// Gated on a recognized language — Bazel cc rules don't apply
// this to non-cc languages (Fortran, ASM) where the
// `LanguageStandard` field has different semantics.
//
// Phase 1 task 3 successor (per
// docs/design/generator-parity-gaps.md "Easy" section).
func prependLanguageStandardCopt(lang string, std *fileapi.LanguageStandard, copts []string) []string {
	if std == nil || std.Standard == "" {
		return copts
	}
	// Skip when copts already names a -std flag.
	for _, c := range copts {
		if strings.HasPrefix(c, "-std=") {
			return copts
		}
	}
	flag := stdFlagFor(lang, std.Standard)
	if flag == "" {
		return copts
	}
	// Prepend so the standard wins over any later -std= an
	// operator-defined override might inject through copts.
	return append([]string{flag}, copts...)
}

// stdFlagFor formats the gcc/clang `-std=…` flag for one
// (language, version) pair. Returns "" for unrecognized
// languages or empty versions.
func stdFlagFor(lang, version string) string {
	if version == "" {
		return ""
	}
	switch strings.ToUpper(lang) {
	case "C":
		return "-std=c" + version
	case "CXX":
		return "-std=c++" + version
	case "OBJC":
		return "-std=c" + version
	case "OBJCXX":
		return "-std=c++" + version
	}
	return ""
}

// relForSource returns the package-relative path the wrapper
// stored for this codemodel source. Sources are mostly
// relative paths the codemodel records verbatim; absolute
// paths are uncommon but possible. Mirrors how the source
// walk in lowerTarget computes the path used in irt.Srcs.
func relForSource(p string, t *fileapi.Target) string {
	_ = t
	return p
}

// stringSliceContains is a tiny linear-search helper. The
// source slices are short enough (typically <50 entries) that
// a map+rebuild is overkill.
func stringSliceContains(s []string, v string) bool {
	for _, e := range s {
		if e == v {
			return true
		}
	}
	return false
}

// scopeForLabelLib looks up a cmake lib name in the
// trace-recovered link-scope map and returns the keyword
// recorded for it (PUBLIC / PRIVATE / INTERFACE), or "" when
// the lib isn't recorded.
//
// Distinct from depScopeIsPrivate's idToName-aware lookup
// because callers here already have the cmake name from
// `manifest.Export.CMakeTarget`, not a codemodel
// `TargetDependency` id.
func scopeForLabelLib(traceLinkScope map[string]string, cmakeName string) string {
	if cmakeName == "" {
		return ""
	}
	return traceLinkScope[cmakeName]
}

// depScopeIsPrivate reports whether a dep entry was recorded
// as PRIVATE in the trace-recovered target_link_libraries
// call. Returns false when:
//   - traceLinkScope is nil (no trace was decoded — codemodel-
//     only path),
//   - the dep's name wasn't found in any keyword-scoped trace
//     entry for this target (the call used the legacy
//     keyword-less positional shape, OR the dep was a cmake-
//     synthesized edge that doesn't surface in trace),
//   - the recorded keyword is PUBLIC, INTERFACE, or empty.
//
// The cmake-name match strips the codemodel id hash; for
// in-codebase deps it also tries the literal target name
// (which is what cmake records in the trace). For external
// imports the namespaced cmake name (`Foo::bar`) is what
// appears in trace and what stripIDHash produces — direct
// match.
func depScopeIsPrivate(traceLinkScope map[string]string, dep fileapi.TargetDependency, idToName map[string]string) bool {
	if len(traceLinkScope) == 0 {
		return false
	}
	cmakeName := stripIDHash(dep.Id)
	// Try the literal cmake name first (covers
	// imports-manifest deps that carry their namespace).
	if scope, ok := traceLinkScope[cmakeName]; ok {
		return scope == "PRIVATE"
	}
	// Also try the per-codemodel target-name registry: cmake's
	// trace records in-codebase deps under the bare target name
	// (e.g. "uses_hello"), not the codemodel id form. idToName
	// gives us that bare name; look it up.
	if name, ok := idToName[dep.Id]; ok {
		if scope, ok2 := traceLinkScope[name]; ok2 {
			return scope == "PRIVATE"
		}
	}
	return false
}

// stripIDHash returns the CMake target name from a File-API target id of the
// form `<name>::@<hash>`. If the id has no hash suffix it is returned
// unchanged; namespaced names (Foo::bar::@<hash>) collapse to "Foo::bar".
func stripIDHash(id string) string {
	if i := strings.Index(id, "::@"); i >= 0 {
		return id[:i]
	}
	return id
}

// isTargetObjectsRef reports whether srcPath is a
// $<TARGET_OBJECTS:t> reference — cmake records these in a
// consumer's sources[] as
// "<buildDir>/[<subdir>/]CMakeFiles/<t>.dir/.../*.o". We
// recognize that shape and check whether <t> is a known
// in-codebase target. When it is, the consumer's deps already
// carry an edge to <t>; lower's OBJECT_LIBRARY emit gives that
// target alwayslink=True, so the objects flow into the consumer
// archive transitively. The .o path itself shouldn't go through
// recoverGenrule (cmake's compile rule isn't a CUSTOM_COMMAND).
//
// The optional `[<subdir>/]` prefix matters for multi-directory
// cmake projects: when an OBJECT library is defined in a
// subdirectory's CMakeLists.txt, cmake's ninja generator writes
// its .o files under `<subdir>/CMakeFiles/<t>.dir/...` (not the
// build-root `CMakeFiles/`). Per #212 (aws-lc surfaced this:
// `crypto/CMakeFiles/crypto_objects.dir/asn1/a_bitstr.c.o`).
func isTargetObjectsRef(srcPath, buildDir string, idToName map[string]string) bool {
	rel, ok := relativeIfInsideRelaxed(buildDir, srcPath)
	if !ok {
		return false
	}
	target, _, ok := findCMakeFilesDir(rel)
	if !ok {
		return false
	}
	for _, name := range idToName {
		if name == target {
			return true
		}
	}
	return false
}

// findCMakeFilesDir locates the segment-aligned
// `CMakeFiles/<target>.dir/` substring in rel (a build-dir-
// relative path) and returns (<target>, <tail-after-".dir/">,
// true) when found. Returns ("", "", false) otherwise.
//
// Both build-root (`CMakeFiles/<t>.dir/...`) and subdirectory
// (`<subdir>/CMakeFiles/<t>.dir/...`) layouts match — cmake's
// ninja generator writes the per-target CMakeFiles dir adjacent
// to the CMakeLists.txt that declared the target, so any
// CMakeLists nested under the source root surfaces nested
// CMakeFiles paths here. The segment-alignment guard (the
// CMakeFiles must follow a `/` or sit at the very start)
// prevents accidental matches against unrelated paths whose
// names happen to end in `CMakeFiles`.
func findCMakeFilesDir(rel string) (target, tail string, ok bool) {
	const marker = "CMakeFiles/"
	idx := strings.Index(rel, marker)
	if idx < 0 {
		return "", "", false
	}
	if idx > 0 && rel[idx-1] != '/' {
		return "", "", false
	}
	after := rel[idx+len(marker):]
	dirEnd := strings.Index(after, ".dir/")
	if dirEnd < 0 {
		return "", "", false
	}
	return after[:dirEnd], after[dirEnd+len(".dir/"):], true
}

// isCompilerObjectArtifact reports whether srcPath is a cc-compile
// output (typically from a unity build or other shape that surfaces
// the .o as a "generated source"), as opposed to a real
// CUSTOM_COMMAND-produced file. Two signals must both fire:
//
//  1. The path resolves under
//     buildDir/[<subdir>/]CMakeFiles/<x>.dir/... and ends in a
//     compile-output extension (.o, .obj, .lo).
//  2. The producing ninja Build's rule has a known compiler-rule
//     prefix (CXX_COMPILER__, C_COMPILER__, etc.; the cmake-side
//     ninja generator decorates compile rules with this shape).
//
// Either signal alone is too permissive: a real CUSTOM_COMMAND could
// happen to write a .o (signal 1 alone) and a compiler rule could
// emit a header in a code-gen toolchain (signal 2 alone). #206.
// The signal-1 check goes through findCMakeFilesDir so the
// subdirectory CMakeFiles shape from #212 matches alongside the
// build-root form.
func isCompilerObjectArtifact(srcPath, buildDir string, g *ninja.Graph) bool {
	rel, ok := relativeIfInsideRelaxed(buildDir, srcPath)
	if !ok {
		return false
	}
	if _, _, ok := findCMakeFilesDir(rel); !ok {
		return false
	}
	ext := strings.ToLower(filepath.Ext(rel))
	if ext != ".o" && ext != ".obj" && ext != ".lo" {
		return false
	}
	if g == nil {
		return false
	}
	b := g.BuildFor(rel)
	if b == nil {
		b = g.BuildFor(srcPath)
	}
	if b == nil {
		return false
	}
	// cmake's modern ninja generator names compile rules
	// `<LANG>_COMPILER__<target>[<suffix>]_<config>` (e.g.
	// `CXX_COMPILER__foo_unscanned_Release`); the `_COMPILER__`
	// infix is the stable signal across every fixture in tree
	// (see converter/testdata/fileapi/*/CMakeFiles/rules.ninja).
	//
	// The bare `<LANG>_COMPILER` suffix branch covers the
	// non-per-target-decorated rule shape — appears in
	// hand-rolled / older-cmake ninja graphs that don't fan out
	// the `__<target>_<config>` tail (the form
	// `converter/internal/ninja/parsefile_test.go` exercises).
	// Kept as a defensive second match: ignoring it would let a
	// non-decorated graph's compile artifact slip past the gate
	// and back into recoverGenrule's CUSTOM_COMMAND refusal,
	// re-introducing the #206 failure shape we just fixed.
	return strings.Contains(b.Rule, "_COMPILER__") || strings.HasSuffix(b.Rule, "_COMPILER")
}

// isPathPrefix reports whether prefix is an ancestor of (or
// equal to the parent dir of) path, where both are
// slash-separated path strings. Empty prefix matches anything
// (means "the whole containing dir"). Used by the configure_file
// consumer attribution to test whether a target's build-dir
// include covers a configured-file output. Pure path semantics —
// no filesystem access.
func isPathPrefix(prefix, path string) bool {
	if prefix == "" || prefix == "." {
		return true
	}
	prefix = strings.TrimSuffix(prefix, "/")
	if path == prefix {
		return true
	}
	return strings.HasPrefix(path, prefix+"/")
}

// dedupeStrings returns a copy of in with consecutive duplicates removed. The
// caller is expected to have sorted in.
func dedupeStrings(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, 0, len(in))
	out = append(out, in[0])
	for _, s := range in[1:] {
		if s != out[len(out)-1] {
			out = append(out, s)
		}
	}
	return out
}

// buildCompileGroupSet returns the set of source indexes that any of
// the target's compileGroups references. The CompileGroupIndex field on
// TargetSource can't be trusted on its own — it's an int with default 0,
// indistinguishable from "in group 0" vs "absent". The returned set is
// queried once per source in the per-target source loop; building it
// once flattens what was an O(sources * compileGroupEntries) inner loop.
func buildCompileGroupSet(t *fileapi.Target) map[int]bool {
	if len(t.CompileGroups) == 0 {
		return nil
	}
	out := make(map[int]bool)
	for _, cg := range t.CompileGroups {
		for _, idx := range cg.SourceIndexes {
			out[idx] = true
		}
	}
	return out
}

// splitCompileFragments parses each whitespace-delimited fragment piece. -D
// pieces are returned as defines (with the leading -D stripped); everything
// else is a copt. -I and -isystem are dropped — those come through
// compileGroup.includes structurally.
func splitCompileFragments(frags []fileapi.CommandFragment) (copts, defines []string) {
	for _, f := range frags {
		if f.Role != "" {
			// Reserved for link fragments; ignore on the compile side.
			continue
		}
		for _, p := range strings.Fields(f.Fragment) {
			switch {
			case strings.HasPrefix(p, "-D"):
				defines = append(defines, strings.TrimPrefix(p, "-D"))
			case strings.HasPrefix(p, "-I"), strings.HasPrefix(p, "-isystem"):
				// dropped: see compileGroup.includes
			default:
				copts = append(copts, p)
			}
		}
	}
	return copts, defines
}

// relativeIfInside returns (rel-path, true) if abs is at or below root, else
// ("", false). Returned slash style: filepath.ToSlash for portability of
// emitted BUILD files (Bazel labels use forward slashes always).
func relativeIfInside(root, abs string) (string, bool) {
	rel, err := filepath.Rel(root, abs)
	if err != nil {
		return "", false
	}
	rel = filepath.ToSlash(rel)
	if rel == "." {
		return "", true
	}
	if strings.HasPrefix(rel, "../") || rel == ".." {
		return "", false
	}
	return rel, true
}

// pathHasDotDotSegment reports whether p contains a `..` path
// segment that would escape its package. A pure `.` segment is
// fine (no-op), a trailing or embedded `..` is not. Matches on
// path-segment boundaries (slash-separated) so a filename
// containing `..` literally (e.g. `foo..bar.c`) doesn't trip.
//
// Used by lower's source walk (#221) to refuse cmake source
// entries whose relative path escapes the project source tree.
func pathHasDotDotSegment(p string) bool {
	for _, seg := range strings.Split(filepath.ToSlash(p), "/") {
		if seg == ".." {
			return true
		}
	}
	return false
}

// discoverHeaders walks each include dir under sourceRoot and returns a sorted
// deduplicated list of header files (package-relative). M1 walks recursively;
// per-file granularity (excluding subdirs) can come later.
//
// Walks are memoized through cache (keyed on absolute include-dir path)
// so that multiple targets sharing an include root don't re-walk the
// same filesystem subtree. Pass nil for cache to disable memoization.
func discoverHeaders(sourceRoot string, includeDirs []string, cache map[string][]string) ([]string, error) {
	seen := map[string]struct{}{}
	for _, inc := range includeDirs {
		absDir := filepath.Join(sourceRoot, inc)
		if cache != nil {
			if hdrs, ok := cache[absDir]; ok {
				for _, h := range hdrs {
					seen[h] = struct{}{}
				}
				continue
			}
		}
		var perDir []string
		walkErr := filepath.WalkDir(absDir, func(p string, d os.DirEntry, err error) error {
			if err != nil {
				// An include dir that doesn't exist on disk is an error
				// worth surfacing; this is rare (CMake validates include
				// dirs on PUBLIC), but possible if the shadow tree is
				// out of sync.
				return err
			}
			if d.IsDir() {
				return nil
			}
			ext := strings.ToLower(filepath.Ext(p))
			if !headerExts[ext] {
				return nil
			}
			rel, err := filepath.Rel(sourceRoot, p)
			if err != nil {
				return err
			}
			slash := filepath.ToSlash(rel)
			seen[slash] = struct{}{}
			perDir = append(perDir, slash)
			return nil
		})
		if walkErr != nil {
			return nil, fmt.Errorf("walk include dir %q: %w", absDir, walkErr)
		}
		if cache != nil {
			cache[absDir] = perDir
		}
	}
	out := make([]string, 0, len(seen))
	for h := range seen {
		out = append(out, h)
	}
	sort.Strings(out)
	return out, nil
}
