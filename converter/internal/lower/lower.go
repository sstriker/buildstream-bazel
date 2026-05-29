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
	"io"
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
	"github.com/sstriker/buildstream-bazel/converter/internal/rejection"
	"github.com/sstriker/buildstream-bazel/converter/ir"
	"github.com/sstriker/buildstream-bazel/internal/convmode"
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
	// wiring (cc_import / sh_binary referencing install-root
	// TreeArtifact paths reconstructed from Target.Install.Destinations
	// + NameOnDisk) lands in Step 2.5 — until then,
	// downstream consumers' compile/link actions against the
	// stubs fail. See
	// docs/design/rendezvous.md
	// for the architectural shape.
	UnsupportedExecuteProcessFallback bool

	// FallbackInstallTarget is the Bazel label of the
	// pipeline_install target whose install-root TreeArtifact the
	// execute-process-fallback's pick_file stubs project files out
	// of. Defaults to ":<basename(BazelPackagePath)>_trace_build"
	// (the same-package install target write-a renders for the
	// round-2 fallback). The caller (convert-element-cmake) derives
	// it from --bazel-package-path; when empty the fallback uses
	// the literal ":_trace_build" default, which a consumer build
	// surfaces loudly if the install target name diverges.
	FallbackInstallTarget string

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
	// recoverGenrule path. On by default at the CLI layer after
	// the Phase 4 graduation (see
	// scripts/meta-cmake-standalone-custom-command.sh and the
	// fixture under converter/testdata/sample-projects/standalone-
	// custom-command). Operators with edge-case projects opt out
	// via --emit-standalone-custom-commands=false. lower.Options
	// itself still defaults to the Go zero value (false) so
	// in-process callers (golden tests, the legacy embedders that
	// pass an explicit lower.Options literal) keep their existing
	// shape — flipping the lower-side default would silently
	// re-shape every unit test golden that doesn't set the field
	// explicitly. The two-tier default (CLI on, library off)
	// keeps the operator-facing surface graduated without
	// regressing in-process consumers.
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

	// Rejections, when non-nil, switches Tier-1 refusal sites
	// from "return a typed failure.Error" to "record the
	// rejection and continue with a local skip (drop the bad
	// source / dep / target / genrule)". The resulting IR is
	// NOT guaranteed to be buildable — the goal is diagnostic
	// surveys against large real-world projects so the operator
	// sees every refusal in one pass. Driven by the
	// --ignore-rejections-for-diagnostics CLI flag.
	//
	// Sites covered: UnsupportedSourcePath (drop the source),
	// UnresolvedLinkDep (drop the dep), UnsupportedTargetType
	// (skip the target), UnsupportedCustomCommand /
	// UnsupportedCustomCommandScript (skip the consuming source),
	// FileAPIMalformed dangling-target-ref (skip the target),
	// UnsupportedExecuteProcess (route through the existing
	// execute-process fallback automatically — the flag sets
	// UnsupportedExecuteProcessFallback implicitly when this
	// field is non-nil).
	Rejections *rejection.Collector

	// CMakeScriptRunner, when non-empty, is the Bazel label of a
	// target that the cmake-P lift will invoke at Bazel build
	// time in place of refusing add_custom_command(... cmake -P
	// <script> ...) shapes. Empty (the default) preserves the
	// pre-existing UnsupportedCustomCommandScript refusal — only
	// operators who stage a runner (a Bazel cc_binary / sh_binary
	// / alias that behaves like cmake) opt in via
	// --cmake-script-runner=<label>. Soundness caveats apply; see
	// liftCmakeScriptGenrule for the limitation details.
	CMakeScriptRunner string

	// CMakeScriptBake, when true (and CMakeBinary is set), runs
	// the cmake -P script at convert time, captures the declared
	// output bytes, and emits genrules that materialize them via
	// base64-decode. Closes the script-hardcoded-absolute-paths
	// gap (paths resolve at convert time where they exist).
	// Outputs are convert-time-baked — the
	// warnConvertTimeBaking post-pass picks up the
	// cmake-codegen-cmake-script-bake tag so operators see the
	// inventory. Off by default.
	CMakeScriptBake bool

	// CMakeScriptTrace, when true (and a runner is set), runs
	// the cmake -P script under `cmake --trace -P` at convert
	// time. The trace's read paths drive auto-augmentation of
	// the genrule's srcs and a structured refusal diagnostic
	// when the script touches paths Bazel's sandbox can't
	// reproduce. Off by default — see CMakeScriptRunner for the
	// operator-opt-in shape. The trace step uses the convert-host
	// `cmake` from PATH (the same one cmakerun.Configure shells
	// to); convert-time execution is gated on the
	// docs/design/conversion-architecture.md platform-coupling
	// caveat.
	CMakeScriptTrace bool

	// Warnings, when non-nil, is the sink lower writes non-fatal
	// diagnostics to. The first user is the missing-include-dir
	// notice: cmake permits target_include_directories(...) entries
	// whose path doesn't physically exist on disk (LLVM's
	// llvm-mca declares `include` without the matching
	// subdirectory), and we silently skip those rather than
	// aborting the conversion. With Warnings set, ToIR emits one
	// aggregated line listing the skipped dirs so the operator
	// sees the cmake oddity. Nil suppresses the message (matches
	// the lower-as-pure-function shape every existing test
	// depends on).
	Warnings io.Writer

	// BakeIn controls the convert-time-baked-output post-pass.
	// Zero value (empty string) resolves to convmode.BakeInWarn so
	// callers leaving the field default get today's behaviour:
	// write the inventory to Warnings, but let conversion succeed.
	// convmode.BakeInAllow silences the inventory;
	// convmode.BakeInReject turns it into a Tier-2 refusal that
	// ToIR returns as an error. See baking_warnings.go for the
	// per-tag taxonomy.
	BakeIn convmode.BakeIn
}

// manifestPrefixAnchor is the canonical token the orchestrator's imports
// manifest uses to anchor cross-element link paths (see
// orchestrator.sandboxPrefix). The token is virtual — no filesystem path
// of that name exists; lower remaps real prefix paths onto it before
// LookupLinkPath.
const manifestPrefixAnchor = ManifestPrefixAnchor

// ManifestPrefixAnchor is the exported form of manifestPrefixAnchor. A
// producer emitting cross-element link_paths in its exports.json
// (convert-element-cmake --out-exports) anchors them with this token so
// they match a consumer's synth-prefix link fragment after lower's
// hostPrefix→anchor rewrite.
const ManifestPrefixAnchor = "/opt/prefix/"

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
		// Diagnostic mode: continue against the first
		// configuration so the survey reaches every
		// per-target refusal site. The Phase 5 multi-config
		// codemodel fold (lowerMultiConfigDeltas at the end
		// of this function) still runs when r.TargetsByConfig
		// is populated, so the per-config select() arms still
		// land on top of cfg[0]'s walk. Strict mode keeps
		// rejecting — production callers want the loud Tier-1
		// until Phase 5 ships full multi-config support.
		if opts.Rejections != nil {
			opts.Rejections.Add(failure.UnsupportedTargetType,
				fmt.Sprintf("multi-config codemodel (%d configurations) — surveying against the first one only; Phase 5 multi-config fold is the canonical path",
					got))
		} else {
			return nil, failure.New(failure.UnsupportedTargetType,
				"M1 supports exactly one configuration; got %d", got)
		}
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
	// platformConditionalSrcsToAdd carries the Tier 2 (#217
	// follow-on) recovery: sources cmake never executed (the
	// other arms of an `if(CMAKE_SYSTEM_NAME ...)` block) that
	// the parser pulled out of the CMakeLists.txt. Unlike Tier
	// 1's platformConditionalSrcs (which moves srcs already in
	// the codemodel's flat list), Tier 2 sources are by
	// construction NOT in the codemodel — they need to be
	// injected directly into the target's
	// PerPlatform["srcs"][selectKey]. Shape:
	// target → selectKey → []src.
	var platformConditionalSrcsToAdd map[string]map[string][]string
	// traceDecoded tracks whether shadow.Decode ran; when true,
	// decodedConfigureFiles holds the configure_file extractions
	// from that single pass and the configure_file recovery
	// reuses them rather than re-parsing the trace. A
	// nil-slice sentinel wouldn't suffice — a trace with zero
	// configure_file events leaves the slice nil, which would
	// otherwise look identical to "decode never ran".
	var traceDecoded bool
	var decodedTrace *shadow.Decoded
	var decodedConfigureFiles []shadow.ConfigureFileCall
	var decodedFileGenerates []shadow.FileGenerateCall
	var decodedExecuteProcesses []shadow.ExecuteProcessCall
	// decoded{AddCustomCommands,AddCustomTargets,AddDependencies}
	// carry the source-level add_custom_command / add_custom_target /
	// add_dependencies events the standalone-genrule cross-reference
	// uses to (a) name the emitted genrule after the wrapping
	// custom-target instead of the output-path hash and (b) pick
	// `:__pkg__` visibility over the default `//visibility:private`
	// when an in-trace consumer references the output. Empty on the
	// no-trace path → cross-reference disabled → legacy naming +
	// private visibility preserved.
	var decodedAddCustomCommands []shadow.AddCustomCommandCall
	var decodedAddCustomTargets []shadow.AddCustomTargetCall
	var decodedAddDependencies []shadow.AddDependenciesCall
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
	// languageOverrideBySrc maps source paths to the cmake LANGUAGE
	// property value (e.g. "CXX" when forcing a .c file to compile
	// as C++). Used by the post-pass to tag affected targets so
	// operators see the gap — Bazel cc_library can't directly
	// override per-source language; the gap needs source rename
	// or per-source library splits.
	var languageOverrideBySrc map[string]string

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
		decodedVal := shadow.Decode(opts.TraceRaw, cmakeSrcForTrace, knownTargets)
		decoded := &decodedVal
		decodedTrace = decoded
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
		decodedAddCustomCommands = decoded.AddCustomCommands
		decodedAddCustomTargets = decoded.AddCustomTargets
		decodedAddDependencies = decoded.AddDependencies
		// Phase 1 task 3 extension — HEADER_FILE_ONLY routing.
		// Build the per-source path lookup once so the per-target
		// source walk can reclassify .h-only sources from srcs
		// into hdrs.
		headerOnlySources = collectHeaderOnlySources(decoded.SourceFileProperties)
		objectDependsBySrc = collectObjectDepends(decoded.SourceFileProperties)
		languageOverrideBySrc = collectLanguageOverrides(decoded.SourceFileProperties)
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
		// Tier 2 (#217 follow-on): recover sources from the
		// SKIPPED arms of platform-conditional if-blocks. cmake
		// only traces what it actually executed, so the other
		// arms of an `if(CMAKE_SYSTEM_NAME ...) elseif(...)`
		// block never surface in the trace — but their sources
		// still need to land under the right `@platforms//os:*`
		// constraint so a bazel reconfigure for the other
		// platform finds them. Tier 2 re-reads CMakeLists.txt at
		// every recognized-predicate `if()` event the trace
		// recorded and parses the skipped arms' source-attaching
		// calls.
		//
		// Tier 2 sources are by construction NOT in the trace's
		// Sources list (cmake never executed the calls), so the
		// flat-srcs partition pass below leaves them un-moved.
		// We collect them in platformConditionalSrcsToAdd —
		// the partition pass injects them directly into
		// PerPlatform["srcs"][selectKey] after handling Tier 1.
		//
		// Pass HostSourceRoot so offline-replay fixtures (where
		// the trace's `file` paths are recording-host absolute
		// but the actual on-disk CMakeLists.txt lives elsewhere)
		// still find their files.
		if tier2 := shadow.ExtractPlatformConditionalSourcesTier2(
			opts.TraceRaw,
			cmakeSrcForTrace,
			opts.HostSourceRoot,
			knownTargets,
			decoded.PlatformConditionalSources,
		); len(tier2) > 0 {
			platformConditionalSrcsToAdd = map[string]map[string][]string{}
			for _, pcs := range tier2 {
				if _, ok := platformConditionalSrcsToAdd[pcs.Target]; !ok {
					platformConditionalSrcsToAdd[pcs.Target] = map[string][]string{}
				}
				platformConditionalSrcsToAdd[pcs.Target][pcs.SelectKey] = append(
					platformConditionalSrcsToAdd[pcs.Target][pcs.SelectKey],
					pcs.Source,
				)
			}
		}
	}

	cmakeSrc := r.Codemodel.Paths.Source
	cmakeBuild := r.Codemodel.Paths.Build
	// Workspace-root auto-detection. Projects whose CMakeLists.txt
	// sits below the repo root (zstd's `build/cmake/CMakeLists.txt`
	// is the canonical example) reference sources via paths that
	// resolve to absolute locations outside cmakeSrc — e.g.
	// `/repo/lib/common/debug.c` referenced from a CMakeLists at
	// `/repo/build/cmake/`. Without detection, the per-source
	// normalizer below refuses those paths as outside both
	// cmakeSrc and cmakeBuild. With detection (.git, MODULE.bazel,
	// WORKSPACE markers walked up from the on-disk source tree),
	// we use the workspace root as the label-relativization base
	// so the emitted BUILD.bazel can sit at the workspace root and
	// reference workspace-relative source paths cleanly.
	//
	// Detection anchor: prefer opts.HostSourceRoot when set
	// (offline replay points at the current-machine fixture path;
	// the recorded cmakeSrc is the recording-machine path and may
	// not exist locally). Fall back to cmakeSrc for the live
	// production path where the two coincide. Common shadow-staged
	// orchestrator paths don't include the workspace markers, so
	// detection returns "" there and the existing cmakeSrc-relative
	// behavior holds.
	detectAnchor := opts.HostSourceRoot
	if detectAnchor == "" {
		detectAnchor = cmakeSrc
	}
	workspaceRoot := detectWorkspaceRoot(detectAnchor)
	hostSrc := opts.HostSourceRoot
	if hostSrc == "" {
		hostSrc = cmakeSrc
	}
	// When workspaceRoot fires AND it sits above hostSrc (the
	// zstd-shape case: HostSourceRoot is the cmake source dir
	// `<repo>/build/cmake`, workspace is `<repo>`), bump hostSrc
	// to workspaceRoot. Otherwise the per-source existence check
	// (filepath.Join(hostSrc, src.Path)) lands at
	// `<repo>/build/cmake/lib/common/debug.c` and elides the
	// source as missing — exactly defeating the workspace-root
	// fix. The check stays a strict-ancestor relation so
	// shadow-stage paths (where workspaceRoot equals hostSrc or
	// is unrelated) don't move under our feet.
	if workspaceRoot != "" && workspaceRoot != hostSrc {
		if _, inside := relativeIfInside(workspaceRoot, hostSrc); inside {
			hostSrc = workspaceRoot
		}
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
		Name:       projectName(r),
		SourceRoot: hostSrc,
		HeaderComments: append(append(
			findPackageHeaderComments(opts.ConfigureLog),
			optionsHeaderComments(r.Cache)...),
			deprecationHeaderComments(opts.ConfigureLog)...,
		),
	}

	cc := newCodegenContext()
	cc.CMakeScriptRunner = opts.CMakeScriptRunner
	cc.CMakeScriptTrace = opts.CMakeScriptTrace
	cc.CMakeScriptBake = opts.CMakeScriptBake
	cc.CMakeBinary = lookupCmakeBinary()
	cc.Warnings = opts.Warnings

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
	// find_package(X) variable-form attribution. cmake's older
	// idiom `target_link_libraries(foo ${ZLIB_LIBRARIES})` resolves
	// to absolute paths the codemodel records verbatim; without
	// attribution back to ZLIB the lower's manifest lookup misses
	// and the dep drops silently. buildFindPackageAttrib correlates
	// configureLog find_package-v1 events with cmakeVars's
	// `<Pkg>_LIBRARIES` lists so the Link.CommandFragments loop
	// can route the path → package → manifest label (when an
	// imports manifest entry exists) or surface a
	// cmake-codegen-find-package-fallback tag (when it doesn't).
	findPkgAttrib := buildFindPackageAttrib(opts.ConfigureLog, opts.CMakeVars)

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
		if opts.Rejections != nil {
			// Diagnostic mode: record the refusal and fall
			// through to the rich lift below. The rich lift
			// produces every cc_library / cc_binary the
			// codemodel exposes plus the genrules every
			// other lift path can recover — strictly more
			// information for the survey than the
			// install_tree_extract placeholder
			// emitFallbackPlaceholder used to return here.
			// Operator sees the per-call refusal record AND
			// the rest of the project's targets in one pass.
			var tier1 *failure.Error
			if errors.As(formatExecuteProcessFailure(executeProcessRefusals), &tier1) {
				opts.Rejections.AddError(tier1)
			}
			// Empty executeProcesses (refusals didn't produce
			// liftable replacements) means the rich lift won't
			// have any genrule entries to emit FROM those
			// execute_process calls — that's the intended
			// behaviour; the rejection record carries the
			// per-call diagnosis.
		} else if !opts.UnsupportedExecuteProcessFallback {
			return nil, formatExecuteProcessFailure(executeProcessRefusals)
		} else {
			// Phase B fallback (strict mode, fallback opt-in):
			// emit a placeholder ir.Package rather than
			// continuing into the native lowering path. The
			// native path would either redo the refusal
			// analysis or trip on the unliftable call later
			// in lowerTarget; the placeholder is the cleaner
			// cut, and it lets downstream consumers see
			// per-target labels at analysis time even when
			// the element itself can't be fine-converted.
			return emitFallbackPlaceholder(r, hostSrc, opts.FallbackInstallTarget)
		}
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
		fileGenerates, err = recoverFileGenerate(decodedFileGenerates, hostSrc, cmakeSrc, opts.BuildDir, cmakeBuild, opts.LiftConfigureFile, opts.CMakeVars, buildGenexTargets(r, cmakeBuild, opts.GenexProbes, decodedTrace, opts.Imports), opts.Imports, cc)
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
	// artifactToName maps each codemodel target's artifact paths
	// (build-dir-relative, e.g. `bin/llvm-min-tblgen`) to the
	// target's name. Used by rewriteToolFromTarget to lift bare
	// artifact-path tool references in genrule cmds into
	// `$(location :<name>)` form plus a tools attr entry.
	artifactToName := map[string]string{}
	for _, tref := range cfg.Targets {
		if t, ok := r.Targets[tref.Id]; ok && t.Type == "UTILITY" {
			utilityIDs[tref.Id] = true
			continue
		}
		idToName[tref.Id] = tref.Name
		if t, ok := r.Targets[tref.Id]; ok {
			for _, art := range t.Artifacts {
				if art.Path != "" {
					artifactToName[art.Path] = tref.Name
				}
			}
		}
	}
	// Thread the artifact-path map into the codegenContext so
	// recoverGenrule's tool-from-target rewrite can lift bare
	// `bin/<tool>` references in per-target generated-source
	// genrule cmds — same shape lowerStandaloneCustomCommands
	// receives the map as a parameter.
	cc.ArtifactToName = artifactToName

	for _, tref := range cfg.Targets {
		t, ok := r.Targets[tref.Id]
		if !ok {
			if opts.Rejections != nil {
				opts.Rejections.AddWithContext(failure.FileAPIMalformed,
					fmt.Sprintf("target id %q in codemodel but not loaded", tref.Id),
					tref.Name, "")
				continue
			}
			return nil, failure.New(failure.FileAPIMalformed,
				"target id %q in codemodel but not loaded", tref.Id)
		}
		irt, err := lowerTarget(&t, cmakeSrc, cmakeBuild, hostSrc, opts.HostPrefixDir, hostSrcOnDisk, g, cc, idToName, utilityIDs, opts.Imports, opts.CTest, privateIncludeDirs[tref.Name], traceLinkLibs[tref.Name], traceLinkScope[tref.Name], configureFiles, fileGenerates, executeProcesses, platformConditionalSrcs[tref.Name], platformConditionalSrcsToAdd[tref.Name], findPkgAttrib, workspaceRoot, opts.Rejections)
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

		// Record the element-root-relative declaring directory for the
		// --split-packages emit transform (out-of-band; never
		// serialized, see ir.Package.SubPackages). The codemodel's
		// ConfigDirectory.Source is cmakeSrc-relative; reconcile it
		// with the same labelRoot base srcs are relativized against so
		// dirs and srcs agree (see the labelRoot pick in lowerTarget,
		// ~1328). When workspaceRoot is a strict ancestor of cmakeSrc
		// (the umbrella / zstd shape), prepend the cmakeSrc-under-
		// workspaceRoot prefix so the recorded dir is workspace-root-
		// relative like the srcs.
		if pkg.SubPackages == nil {
			pkg.SubPackages = map[string]string{}
		}
		pkg.SubPackages[irt.Name] = subPackageDir(cfg, tref.DirectoryIndex, cmakeSrc, workspaceRoot)
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
	// (per ROADMAP.md). Post-emit pass
	// keeps lowerTarget's signature stable and applies uniformly
	// to all the rule families.
	reclassifyHeaderOnlySources(pkg, headerOnlySources)
	// Route PRIVATE-scoped target_compile_definitions trace
	// events into Bazel's non-transitive `local_defines`
	// attribute instead of the transitive `defines`. Closes the
	// scope-fidelity gap: cmake's PRIVATE means "only for this
	// target's own compile", which is local_defines in Bazel;
	// the codemodel folds everything into CompileGroups.Defines
	// without a scope tag, so the trace is the only source of
	// truth here. No-op when trace was absent (decodedTrace nil)
	// or when no PRIVATE-scoped defines appear.
	if decodedTrace != nil {
		applyPrivateScopeToDefines(pkg, decodedTrace.CompileDefinitions)
	}
	// Probe-genex per-target Properties → Bazel attributes:
	// BUILD_RPATH / INSTALL_RPATH lift to linkopts,
	// POSITION_INDEPENDENT_CODE to features=["pic"] /
	// features=["-pic"], visibility presets to copts. Off when no
	// probe ran (opts.GenexProbes empty) so back-compat preserved
	// for callers that don't pass --probe-genex.
	applyProbeGenexProperties(pkg, opts.GenexProbes)
	// Lift raw toolchain-feature flags out of copts/linkopts into
	// the Features attribute so the cc_toolchain owns the flag
	// set instead of every cc_library carrying the same per-rule
	// emission. Runs AFTER applyProbeGenexProperties so the
	// visibility presets that pass routes to copts (today)
	// immediately move to features here. Closes the
	// `raw-toolchain-feature-flag` audit gap (~785 findings
	// across the 9-project survey on PR #247).
	liftRawFeatureFlags(pkg)
	// Strip cross-target hdrs duplication: when target C declares
	// a header H also owned by sibling S that's already in C's
	// deps, drop H from C.hdrs — Bazel propagates hdrs through
	// deps, so re-listing is redundant. At LLVM/VTK/Boost scale
	// this collapses per-target hdrs counts by 10-100x. The
	// dep-aware guard preserves compilability for consumers that
	// would otherwise lose access to a transitively-owned header.
	stripDepOwnedHdrs(pkg)
	// Convert-time baked outputs (configure_file legacy capture,
	// file(GENERATE) (b) base64 shape, execute_process value
	// hoists, cmake -P lift, etc.) carry tags that ToIR scans
	// after every emit-time tagging is done. The post-pass writes
	// a single aggregated warning to opts.Warnings (and / or
	// refuses with the inventory embedded, depending on
	// opts.BakeIn) so operators see at convert time which rules
	// carry bytes that won't auto-refresh when upstream inputs
	// change. Empty BakeIn resolves to convmode.BakeInWarn so
	// existing callers see today's behaviour without setting the
	// field. Per-tag taxonomy in
	// converter/internal/lower/baking_warnings.go.
	if err := applyBakeInPolicy(pkg, opts.Warnings, opts.BakeIn); err != nil {
		return nil, err
	}
	// OBJECT_DEPENDS post-pass adds declared header dependencies
	// to the target's hdrs so incremental rebuilds trip on
	// changes. Uses the same per-pkg walk shape as the
	// HEADER_FILE_ONLY pass.
	addObjectDependsHeaders(pkg, objectDependsBySrc)
	// LANGUAGE override post-pass tags targets whose sources
	// were forced to a non-default compile language via
	// set_source_files_properties(... LANGUAGE ...). Bazel
	// cc_library can't directly override per-source language;
	// tag signals the gap.
	tagLanguageOverrides(pkg, languageOverrideBySrc)
	// install(FILES) → filegroup() lowering (Phase 1 task 2 of the
	// generator-parity uplift). Appended last so the file-head
	// targets stay grouped by family: cc rules first, generated
	// content next, then install-side filegroups.
	pkg.Targets = append(pkg.Targets, lowerDirectoryInstallers(r)...)
	// INTERFACE-only library lift. cmake's File API codemodel
	// omits INTERFACE_LIBRARY targets from its targets[] array —
	// they're header-only declarations with no link step to
	// model. The trace records them via add_library(<name>
	// INTERFACE); cross-referencing against the codemodel-known
	// set gives us the INTERFACE-only residue the main lift
	// missed (nlohmann-json's nlohmann_json, boost-core's
	// boost_core, etc.). For each, synthesize a cc_library
	// carrying the trace-recorded INTERFACE_INCLUDE_DIRECTORIES
	// + INTERFACE_COMPILE_DEFINITIONS as hdrs / defines / includes.
	if decodedTrace != nil {
		pkg.Targets = append(pkg.Targets,
			lowerInterfaceLibraries(decodedTrace, knownTargets, hostSrc, cmakeSrc, workspaceRoot, cc)...)
	}
	// Alias-target lift from trace: `add_library(<alias> ALIAS
	// <target>)` shapes don't appear in codemodel.targets[]
	// (cmake resolves aliases at configure time so codemodel
	// only records the underlying target). The trace captures
	// the source-level alias declaration; emit Bazel-native
	// alias() rules so operator-written cross-package consumers
	// resolve the alias name correctly.
	//
	// Runs AFTER lowerInterfaceLibraries so trace-synthesized
	// INTERFACE_LIBRARY targets (boost_core, nlohmann_json) are
	// in the resolvable set. Boost.Core's
	// `add_library(Boost::core ALIAS boost_core)` previously
	// dropped because `boost_core` lives only in trace-synthesized
	// IR, not in the codemodel's knownTargets.
	if decodedTrace != nil {
		resolvable := map[string]bool{}
		for k, v := range knownTargets {
			resolvable[k] = v
		}
		for _, t := range pkg.Targets {
			resolvable[t.Name] = true
		}
		pkg.Targets = append(pkg.Targets,
			lowerAliasTargets(decodedTrace, resolvable, cmakeSrc)...)
	}
	// Phase 4 standalone custom-command emission. Opt-in via
	// Options.EmitStandaloneCustomCommands; the dedup against
	// existing genrules keeps the recoverGenrule path's output
	// intact even when this fires.
	if opts.EmitStandaloneCustomCommands {
		traceCtx := standaloneTraceContext{
			CustomCommands:  decodedAddCustomCommands,
			CustomTargets:   decodedAddCustomTargets,
			AddDependencies: decodedAddDependencies,
		}
		pkg.Targets = append(pkg.Targets,
			lowerStandaloneCustomCommands(g, pkg.Targets, cmakeSrc, cmakeBuild, artifactToName, traceCtx)...)
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
		lowerMultiConfigDeltas(pkg, r.TargetsByConfig, configs, cmakeSrc, cmakeBuild, idToName)
	}
	// Surface missing-include-dir skips so the operator sees the
	// cmake oddity instead of silently losing the dir. Per-dir
	// dedup happens via the map; we render a deterministic
	// alphabetical list. When the diagnostic-mode rejection
	// collector is active, also record a synthetic rejection per
	// dir so the rejections.json sidecar captures the survey
	// signal alongside the other refusal records.
	if len(cc.MissingIncludeDirs) > 0 {
		dirs := make([]string, 0, len(cc.MissingIncludeDirs))
		for d := range cc.MissingIncludeDirs {
			dirs = append(dirs, d)
		}
		sort.Strings(dirs)
		if opts.Warnings != nil {
			fmt.Fprintf(opts.Warnings,
				"lower: %d include dir(s) declared by the codemodel are missing on disk; treated as empty:\n",
				len(dirs))
			for _, d := range dirs {
				fmt.Fprintf(opts.Warnings, "  - %s\n", d)
			}
		}
		if opts.Rejections != nil {
			for _, d := range dirs {
				opts.Rejections.AddWithContext(
					failure.UnsupportedSourcePath,
					fmt.Sprintf("include dir %q referenced by codemodel doesn't exist on disk; treated as empty (cmake permits forward-declared include paths — LLVM's llvm-mca shape)", d),
					"", d)
			}
		}
	}
	// Surface install(SCRIPT) / install(CODE) directives. These run
	// cmake script code at install time and have no Bazel
	// analogue — the converter drops them silently. The warning
	// makes the omission auditable so operators who care about
	// install-time logic see what was lost.
	surfaceInstallScriptInstallers(r, opts.Warnings)
	return pkg, nil
}

func projectName(r *fileapi.Reply) string {
	if e := r.Cache.Get("CMAKE_PROJECT_NAME"); e != nil {
		return e.Value
	}
	return ""
}

func lowerTarget(t *fileapi.Target, cmakeSrc, cmakeBuild, hostSrc, hostPrefix string, hostSrcOnDisk bool, g *ninja.Graph, cc *codegenContext, idToName map[string]string, utilityIDs map[string]bool, imports *manifest.Resolver, tests *ctest.Registry, privateIncludeDirs map[string]bool, traceLinkLibs []string, traceLinkScope map[string]string, configureFiles []configureFileOut, fileGenerates []fileGenerateOut, executeProcesses []executeProcessOut, platformConditionalSrcs map[string]string, platformConditionalSrcsToAdd map[string][]string, findPkgAttrib *findPackageAttrib, workspaceRoot string, rejections *rejection.Collector) (*ir.Target, error) {
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
			irt.Provenance = ir.Provenance{
				File:    reanchorProvenanceFile(file, cmakeSrc, cmakeBuild),
				Line:    node.Line,
				Command: cmd,
			}
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
		if rejections != nil {
			rejections.AddWithContext(failure.UnsupportedTargetType,
				fmt.Sprintf("target %q has unsupported type %q", t.Name, t.Type),
				t.Name, "")
			return nil, nil
		}
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
			// Pre-existing checked-in "generated" source: libevent's
			// test/regress.gen.{c,h} are the canonical case — they're
			// produced by an event_rpcgen.py code generator BUT the
			// generated files are committed to the repo so the build
			// works without re-running the generator. cmake records
			// IsGenerated=true unconditionally; the converter previously
			// passed them to recoverGenrule, which refused with
			// "generated source outside the build dir" (they live under
			// cmakeSrc, not cmakeBuild). When the source exists on
			// disk at its expected package-relative location, route
			// it through the regular source-path handling — the
			// cc_library entry picks up the already-committed file.
			// The genrule that would re-produce the file is preserved
			// via the standalone-custom-command edge so operators can
			// still wire the generator if they want runtime
			// regeneration.
			//
			// cmake records IsGenerated source paths as either
			// absolute or cmakeSrc-relative; resolve both forms to
			// a package-relative path before the existence check.
			{
				rel := src.Path
				if filepath.IsAbs(rel) {
					if r, inside := relativeIfInside(cmakeSrc, rel); inside {
						rel = r
					} else {
						rel = "" // outside cmakeSrc; let recoverGenrule handle it
					}
				}
				if rel != "" && hostSrc != "" {
					if _, err := os.Stat(filepath.Join(hostSrc, rel)); err == nil {
						ext := strings.ToLower(filepath.Ext(rel))
						switch {
						case inCompileGroup[i]:
							irt.Srcs = append(irt.Srcs, rel)
						case headerExts[ext]:
							irt.Hdrs = append(irt.Hdrs, rel)
						default:
							irt.Srcs = append(irt.Srcs, rel)
						}
						continue
					}
				}
			}
			relOut, _, err := cc.recoverGenrule(src.Path, cmakeSrc, cmakeBuild, g)
			if err != nil {
				if rejections != nil {
					// Diagnostic mode: drop the generated source
					// and continue. The consuming target keeps its
					// other srcs; the missing generated input is
					// recorded as a rejection for the operator to
					// triage. A typed *failure.Error carries the
					// stable Code; bare errors (returned by
					// recoverGenrule for non-typed shapes — none
					// today, but defensive) record under a generic
					// custom-command code so the report is total.
					var tier1 *failure.Error
					if errors.As(err, &tier1) {
						rejections.AddWithContext(tier1.Code, tier1.Message, t.Name, src.Path)
					} else {
						rejections.AddWithContext(failure.UnsupportedCustomCommand, err.Error(), t.Name, src.Path)
					}
					continue
				}
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
			// Not assigned to a compile group: typically a header
			// in target_sources(). The include-dir walking below
			// usually picks them up via the target's declared
			// includes — but when the target declares no
			// target_include_directories (small projects:
			// `add_library(foo bar.cu bar.h)` with the headers
			// resolved from cwd via #include "bar.h"), the walk
			// produces nothing and the explicit header gets
			// silently dropped. Surface explicit header-extension
			// sources directly into irt.Hdrs so they land in the
			// emitted cc_library regardless of whether the include
			// walk fires.
			//
			// Path resolution mirrors the compile-group branch:
			// absolute paths under cmakeSrc relativize to package-
			// relative; cmakeSrc-relative paths pass through.
			srcPath := src.Path
			if filepath.IsAbs(srcPath) {
				if rel, inside := relativeIfInside(cmakeSrc, srcPath); inside {
					srcPath = rel
				} else {
					continue
				}
			}
			if pathHasDotDotSegment(srcPath) {
				continue
			}
			ext := strings.ToLower(filepath.Ext(srcPath))
			if headerExts[ext] {
				irt.Hdrs = append(irt.Hdrs, srcPath)
			}
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
		// Pick the label-relativization base. workspaceRoot is
		// auto-detected from cmakeSrc walking up for .git /
		// MODULE.bazel / WORKSPACE markers (see
		// detectWorkspaceRoot); when found and a strict ancestor
		// of cmakeSrc, it anchors labels to the wider workspace
		// so projects with a `build/cmake/CMakeLists.txt` layout
		// (zstd, lz4, brotli) can reference sources scattered
		// across sibling subtrees like `lib/common/debug.c`. When
		// no marker fires (the shadow-stage path), labelRoot
		// falls back to cmakeSrc and the existing behavior holds.
		labelRoot := cmakeSrc
		if workspaceRoot != "" {
			labelRoot = workspaceRoot
		}
		// cmake's codemodel-v2 spec: source paths are
		// cmakeSrc-relative when the file is inside the cmake
		// source root, absolute otherwise. The two cases need
		// different normalization to land at a valid labelRoot-
		// relative Bazel label.
		origAbs := filepath.IsAbs(srcPath)
		// In-tree absolute path: cmake recorded an absolute
		// path that happens to live under labelRoot. Normalize
		// to the documented label-relative form so the
		// emitted label is valid. labelRoot is "" on
		// reply-dir-only replay runs; skip in that case
		// because the relativeIfInside check can't run.
		if labelRoot != "" && origAbs {
			if rel, inside := relativeIfInside(labelRoot, srcPath); inside {
				srcPath = rel
			}
		}
		// Re-anchor cmakeSrc-relative paths to labelRoot-relative
		// when labelRoot is a parent of cmakeSrc (umbrella
		// detection — LLVM's llvm-project/ promoted above
		// cmakeSrc=llvm-project/llvm/). cmake records these
		// sources as cmakeSrc-relative per codemodel-v2 spec;
		// after the umbrella promotion, both:
		//   - on-disk existence checks (which join against the
		//     promoted hostSrc=labelRoot below)
		//   - emitted Bazel labels (BUILD.bazel at labelRoot
		//     expecting labelRoot-relative srcs)
		// need the path re-anchored. Example: LLVM's
		// `unittests/ADT/AnyTest.cpp` (cmakeSrc-relative)
		// becomes `llvm/unittests/ADT/AnyTest.cpp` (labelRoot-
		// relative). Gated on origAbs=false to skip absolute
		// paths that were stripped above — those were anchored
		// to labelRoot directly, not cmakeSrc, and re-anchoring
		// them would double-prefix.
		if !origAbs && labelRoot != "" && labelRoot != cmakeSrc {
			if cmakeRel, ok := relativeIfInside(labelRoot, cmakeSrc); ok && cmakeRel != "" && cmakeRel != "." {
				srcPath = filepath.Join(cmakeRel, srcPath)
			}
		}
		// Out-of-tree absolute path: at this point we've
		// already filtered absolute-under-cmakeBuild (elided
		// above) and absolute-under-labelRoot (normalized just
		// above). What's left is absolute paths under neither
		// root — e.g. `add_library(foo /vendored/elsewhere/bar.c)`.
		// Refuse with a typed Tier-1 error so the operator
		// sees the broken cmake call, not a downstream Bazel
		// load error.
		if filepath.IsAbs(srcPath) {
			if rejections != nil {
				rejections.AddWithContext(failure.UnsupportedSourcePath,
					fmt.Sprintf("target %q references source %q at an absolute path outside the project source tree (%s) and the build tree (%s); Bazel labels must be package-relative",
						t.Name, srcPath, labelRoot, cmakeBuild),
					t.Name, srcPath)
				continue
			}
			return nil, failure.New(failure.UnsupportedSourcePath,
				"target %q references source %q at an absolute path outside the project source tree (%s) and the build tree (%s); Bazel labels must be package-relative",
				t.Name, srcPath, labelRoot, cmakeBuild)
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
			if rejections != nil {
				rejections.AddWithContext(failure.UnsupportedSourcePath,
					fmt.Sprintf("target %q references source %q whose path escapes the project source tree via `..` segments; Bazel labels must stay within the package",
						t.Name, src.Path),
					t.Name, src.Path)
				continue
			}
			return nil, failure.New(failure.UnsupportedSourcePath,
				"target %q references source %q whose path escapes the project source tree via `..` segments; Bazel labels must stay within the package",
				t.Name, src.Path)
		}
		// Empty after normalization (only possible from the
		// "./" strip on a pathological input like "./" alone)
		// or single ".": refuse — there's no useful label here.
		if srcPath == "" || srcPath == "." {
			if rejections != nil {
				rejections.AddWithContext(failure.UnsupportedSourcePath,
					fmt.Sprintf("target %q references source %q which normalizes to an empty Bazel label",
						t.Name, src.Path),
					t.Name, src.Path)
				continue
			}
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
	// walkPkgRootForHdrs is set when a target_include_directories
	// entry resolves to the package root (rel==""). We drop that
	// from emit-side Includes (Bazel rejects `includes=[""]`) but
	// still need discoverHeaders to walk hostSrc so headers under
	// the package root land in irt.Hdrs and consumers can find
	// them. Declared at function scope (above the CompileGroups
	// guard) so the flag survives to the discoverHeaders call
	// further down.
	walkPkgRootForHdrs := false

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
			if baked, ok := bakeAutoinitIncludeDefine(d.Define, cmakeBuild, cc, irt); ok {
				defs = append(defs, baked)
				continue
			}
			if reanchored, keep := reanchorDefineValue(d.Define, cmakeSrc, cmakeBuild); keep {
				defs = append(defs, reanchored)
			}
		}
		irt.Defines = defs

		// Sysroot: tag the target with the cmake-recorded sysroot
		// path. Operators see cross-compile context via grep;
		// per-target sysroot lift to copts/linkopts would conflict
		// with the operator's cc_toolchain (sysroot canonically
		// lives there).
		if cg.Sysroot != nil && cg.Sysroot.Path != "" {
			tag := "cmake-codegen-sysroot=" + cg.Sysroot.Path
			if !stringSliceContains(irt.Tags, tag) {
				irt.Tags = append(irt.Tags, tag)
			}
		}

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
				// #219: include dir resolved outside both
				// cmakeSrc and cmakeBuild. The path is one of
				// three shapes:
				//   - A producer-element's export tree under
				//     hostPrefix (cross-element find_package
				//     include). We can't directly translate
				//     to a Bazel `includes` entry — the
				//     producing element provides headers
				//     through a cc_library dep rather than an
				//     include path — but operators auditing
				//     for unresolved cross-element imports
				//     want to see this. Emit a payload-bearing
				//     audit tag identifying the dropped path
				//     (hostPrefix-relative so two synth-prefix
				//     subdirs with the same trailing basename
				//     don't collide on the dedup).
				//   - A system include path
				//     (/usr/include, etc.). Filtering these
				//     silently is correct — the toolchain
				//     supplies the same headers via its
				//     default search path, and emitting them
				//     as Bazel `includes` would leak host
				//     state into the BUILD.
				//   - A user-specified out-of-tree absolute
				//     path with no hostPrefix relationship
				//     (e.g. `/opt/vendor/include` hardcoded in
				//     a CMakeLists). These currently fall
				//     through silently — the audit tag is
				//     scoped to the hostPrefix case where the
				//     producer-element framing is well-defined.
				//     Tagging the bare-hardcode case would
				//     create noise on every project that
				//     references /usr/include via find_package
				//     propagation. A separate audit tag for
				//     the hardcode case can land if a real
				//     downstream surfaces the need.
				if hostPrefix != "" {
					if relUnder, inside := relativeIfInside(hostPrefix, inc.Path); inside {
						tag := "cmake-elided-prefix-include=" + relUnder
						if !stringSliceContains(irt.Tags, tag) {
							irt.Tags = append(irt.Tags, tag)
						}
					}
				}
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
			// target_include_directories(${CMAKE_CURRENT_SOURCE_DIR})
			// resolves to rel == "". Bazel rejects
			// `includes = [""]` ("resolves to the workspace root,
			// which would allow this rule and all of its transitive
			// dependents to include any file in your workspace");
			// same-package consumers already see this target's
			// headers via hdrs+deps without an explicit include
			// dir, so dropping the entry from `includes =` is the
			// idiomatic shape. But the package root is still the
			// authoritative source for hdrs discovery — record the
			// signal so the discoverHeaders call below knows to
			// walk hostSrc itself (otherwise zlib-shape projects
			// that declare ONLY target_include_directories(.) end
			// up with empty hdrs and consumers can't find any
			// header).
			if rel == "" {
				walkPkgRootForHdrs = true
				continue
			}
			irt.Includes = append(irt.Includes, rel)
		}
	}

	// INTERFACE_LIBRARY include extraction (#308). Starting with
	// cmake 3.19 INTERFACE_LIBRARY targets surface in the codemodel
	// targets[] array, so they reach this codemodel path instead of
	// the trace-based lowerInterfaceLibraries fallback. They have no
	// CompileGroups (cmake never compiles them), so the include loop
	// above — gated on len(t.CompileGroups) > 0 — produces nothing
	// and the emitted cc_library lacks an `includes =` attribute.
	// Consumers that `#include <foo.h>` then hit Bazel "undeclared
	// inclusion" errors. The include dirs for these targets live in
	// the HEADERS-typed FileSets' BaseDirectories (codemodel-v2
	// minor 5, cmake 3.25+, `target_sources(... FILE_SET HEADERS
	// BASE_DIRS ...)`). Mirror the CompileGroups extraction above but
	// source the directory list from FileSets metadata: relativize
	// each base dir against cmakeSrc, route the package-root case to
	// the discoverHeaders walk via walkPkgRootForHdrs, and append the
	// rest to irt.Includes.
	if irt.Kind == ir.KindCCInterface && len(t.CompileGroups) == 0 {
		seenIfaceInc := map[string]bool{}
		addInclude := func(dir string) {
			rel := dir
			if filepath.IsAbs(rel) {
				r, inside := relativeIfInside(cmakeSrc, rel)
				if !inside {
					return
				}
				rel = r
			} else {
				rel = filepath.Clean(rel)
			}
			if rel == "" || rel == "." {
				// target_include_directories(${CMAKE_CURRENT_SOURCE_DIR}):
				// Bazel rejects `includes = [""]`; record the
				// signal so discoverHeaders walks the package root
				// (same shape as the CompileGroups branch above).
				walkPkgRootForHdrs = true
				return
			}
			if pathHasDotDotSegment(rel) || seenIfaceInc[rel] {
				return
			}
			seenIfaceInc[rel] = true
			irt.Includes = append(irt.Includes, rel)
		}
		for _, fs := range t.FileSets {
			if fs.Type != "HEADERS" {
				continue
			}
			for _, bd := range fs.BaseDirectories {
				addInclude(bd)
			}
		}
		// Fallback: when the target declares a distinct source dir
		// (a subdirectory target) that resolves inside cmakeSrc and
		// FileSets surfaced no usable include, treat the target's own
		// source dir as an include directory so package-root-relative
		// #includes still resolve.
		if len(irt.Includes) == 0 && !walkPkgRootForHdrs &&
			t.Paths.Source != "" && t.Paths.Source != cmakeSrc {
			addInclude(t.Paths.Source)
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

	// Include the package root in the header-discovery walk when
	// the target's target_include_directories surfaced the empty-
	// relative entry (the rel=="" drop above sets the flag). The
	// emit-side `includes = [...]` slot stays untouched (Bazel
	// rejects `[""]`); hdrs discovery picks up the same files.
	includesForWalk := irt.Includes
	if walkPkgRootForHdrs {
		includesForWalk = append([]string{""}, irt.Includes...)
	}
	hdrs, err := discoverHeaders(hostSrc, includesForWalk, cc.HeaderWalkCache, cc.MissingIncludeDirs)
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
				// find_package attribution: when the cmake
				// target name is in the `<Package>::<Component>`
				// shape, tag the consuming target with the
				// package name so operators can grep for
				// cmake-find-package=Boost etc.
				if pkg := packagePrefix(cmakeName); pkg != "" {
					tag := "cmake-find-package=" + pkg
					if !stringSliceContains(irt.Tags, tag) {
						irt.Tags = append(irt.Tags, tag)
					}
				}
			} else {
				if rejections != nil {
					rejections.AddWithContext(failure.UnresolvedLinkDep,
						fmt.Sprintf("target %q depends on %q which is neither in-codebase nor in the imports manifest",
							t.Name, cmakeName),
						t.Name, cmakeName)
					continue
				}
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
	// examples/sanitizer-features/README.md for the feature-definition
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
				// cmake's File API serialises link flags as one
				// whitespace-joined string per fragment (e.g.
				// "-Wl,--gc-sections -Wl,-z,now -O3 -DNDEBUG").
				// Tokenise so each flag lands as its own linkopts
				// entry — Bazel passes each list entry as a
				// separate argv to the linker driver; without
				// this split the linker receives the entire
				// string as a single (invalid) flag.
				for _, tok := range strings.Fields(frag.Fragment) {
					rewritten, keep, addlInput := reanchorLinkOptTokenWithInput(tok, cmakeSrc, cmakeBuild)
					if !keep {
						continue
					}
					// Drop compile-only flags that cmake folded into
					// the link line via CMAKE_*_FLAGS — warnings,
					// preprocessor defines, include dirs, language
					// standard. Bazel separates compile/link and
					// rejects these on the link line at best as
					// dead bytes, at worst as warnings.
					if isCompileOnlyLinkFlag(rewritten) {
						continue
					}
					// Dedup against earlier appends — cmake's
					// commandFragments occasionally lists the same
					// flag multiple times (transitive PUBLIC
					// propagation, hand-duplicated CMakeLists
					// entries). Mirrors the copts/defines dedup in
					// splitCompileFragments. First-occurrence-wins
					// matches the linker's argv-order semantics for
					// flags whose duplicates are noise (`-Wl,--gc-sections`,
					// `-O3`).
					if stringSliceContains(irt.LinkOpts, rewritten) {
						continue
					}
					irt.LinkOpts = append(irt.LinkOpts, rewritten)
					if addlInput != "" && !stringSliceContains(irt.AdditionalLinkerInputs, addlInput) {
						irt.AdditionalLinkerInputs = append(irt.AdditionalLinkerInputs, addlInput)
					}
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
				// Non-abs `libraries`-role fragments are
				// typically in-codebase target output names
				// (e.g. `libfoo.a` for a sibling cc_library)
				// that the t.Dependencies walk above has
				// already routed to irt.Deps; tagging them
				// here would create false-positive audit
				// noise. Pure link flags
				// (`-lpthread` / `-pthread`) usually surface
				// as `flags`-role fragments routed to
				// LinkOpts above, not here.
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
				continue
			}
			// find_package variable-form attribution. The
			// path didn't match a manifest entry directly;
			// see whether configureLog + cmakeVars attribute
			// it to a `find_package(X)` call. When attributed,
			// try the manifest under the package's namespaced
			// primary target (`<Pkg>::<Pkg>`) — that's the
			// modern cmake export shape and is what the
			// manifest typically registers. Falls back to a
			// tag-only emission so operators see the missing
			// dep even when the manifest has no matching
			// entry.
			if pkg := findPkgAttrib.Lookup(path); pkg != "" {
				if export := imports.LookupCMakeTarget(pkg + "::" + pkg); export != nil {
					if !seen[export.BazelLabel] {
						seen[export.BazelLabel] = true
						if allowsImplementationDeps && traceLinkScope != nil && scopeForLabelLib(traceLinkScope, export.CMakeTarget) == "PRIVATE" {
							irt.ImplementationDeps = append(irt.ImplementationDeps, export.BazelLabel)
						} else {
							irt.Deps = append(irt.Deps, export.BazelLabel)
						}
					}
					continue
				}
				// No manifest hit; emit a fallback tag so
				// operators see which package's link is
				// unresolved. One tag per (pkg, path)
				// pair — same package can show up across
				// multiple paths (release + debug, main +
				// dep libs).
				tag := "cmake-codegen-find-package-fallback=" + pkg + "=" + filepath.Base(path)
				if !stringSliceContains(irt.Tags, tag) {
					irt.Tags = append(irt.Tags, tag)
				}
				continue
			}
			// #220: abs-path link fragment that escapes both
			// the imports manifest AND the find_package
			// attribution. Either cmake hardcoded an absolute
			// path that didn't flow through find_package
			// (rare), or the imports manifest hasn't learned
			// about this dep yet.
			//
			// For libraries under standard system locations
			// (/usr/lib*, /lib*, /usr/local/lib*) the
			// Bazel-idiomatic shape is `linkopts = ["-l<name>"]`
			// — the toolchain's library search path covers
			// these paths universally, and -l<name> lets the
			// linker resolve via the same mechanism it'd use
			// for any other system dep. Lift the path's
			// basename → -l<name> so the rule actually links
			// against the lib at Bazel build time instead of
			// failing with undefined references. For non-
			// standard paths (vendored installs at
			// /opt/<vendor>/lib/..., custom prefixes) we keep
			// the tag-only elision — those need an explicit
			// -L<dir> the operator's imports manifest is the
			// right home for.
			if name := systemLibName(path); name != "" {
				// B: variable-only Find modules (no <Pkg>::<Pkg>
				// target) resolve via ${<Pkg>_LIBRARIES}, so the
				// host-resolved fragment lands here. If a producer
				// element claims this lib name (exports.json
				// link_libraries), redirect to it instead of linking
				// the host -l<name>.
				if export := imports.LookupLinkLibrary(name); export != nil {
					if !seen[export.BazelLabel] {
						seen[export.BazelLabel] = true
						if allowsImplementationDeps && traceLinkScope != nil && scopeForLabelLib(traceLinkScope, export.CMakeTarget) == "PRIVATE" {
							irt.ImplementationDeps = append(irt.ImplementationDeps, export.BazelLabel)
						} else {
							irt.Deps = append(irt.Deps, export.BazelLabel)
						}
					}
					continue
				}
				flag := "-l" + name
				if !stringSliceContains(irt.LinkOpts, flag) {
					irt.LinkOpts = append(irt.LinkOpts, flag)
				}
			} else {
				// Emit the full path (post-manifestPrefixAnchor
				// rewrite when the fragment was under hostPrefix)
				// rather than the basename so multi-arch layouts
				// (/usr/lib/x86_64-linux-gnu/libz.so vs
				// /usr/lib/i386-linux-gnu/libz.so → both libz.so)
				// don't collide on the dedup.
				tag := "cmake-elided-link-fragment=" + path
				if !stringSliceContains(irt.Tags, tag) {
					irt.Tags = append(irt.Tags, tag)
				}
			}
			// Dual to the cmake-codegen-find-package-fallback
			// tag above: that one fires when find_package
			// attribution SUCCEEDED but the imports manifest
			// has no `<Pkg>::<Pkg>` entry. This sibling tag
			// fires when find_package attribution itself
			// MISSED — either the configureLog carried no
			// find_package-v1 event (cmake < 3.32 OR cmake >=
			// 3.32 with the event suppressed) AND cmakeVars
			// didn't surface a `<Pkg>_FOUND` either (the
			// --dump-vars=false path, or an out-of-fileapi
			// cmake namespace). Gated on imports != Empty()
			// so the tag only fires when the operator
			// explicitly opted into find_package attribution
			// (a manifest was provided). Without that gate
			// the tag would fire on every cmake project that
			// hard-codes an absolute link path with no
			// manifest, drowning the audit signal.
			//
			// Parameterized on basename only (not full path)
			// so operators can grep against the package's
			// library-shape (libz.so) regardless of multi-
			// arch host paths. The full-path anchor lives on
			// the cmake-elided-link-fragment tag above.
			if !imports.Empty() {
				baseTag := "cmake-codegen-find-package-attribution-missed=" + filepath.Base(path)
				if !stringSliceContains(irt.Tags, baseTag) {
					irt.Tags = append(irt.Tags, baseTag)
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
		if err := splitCompileGroups(t, irt, cc, cmakeSrc, cmakeBuild); err != nil {
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
	// splitCompileGroups just appended to cc.Subs. The wrapper
	// case covers single-language targets (where irt.Srcs
	// carries everything); the sub-library case covers
	// multi-language targets (where splitCompileGroups cleared
	// irt.Srcs and distributed sources across per-language sub-
	// libraries — those subs carry the conditional sources now
	// and need partitioning too).
	if len(platformConditionalSrcs) > 0 {
		partitionPlatformConditionalSrcs(irt, platformConditionalSrcs)
		for i := subsBefore; i < len(cc.Subs); i++ {
			partitionPlatformConditionalSrcs(&cc.Subs[i], platformConditionalSrcs)
		}
	}
	// Tier 2 injection: append sources Tier 2 recovered from
	// the skipped arms of platform-conditional if-blocks. These
	// don't live in irt.Srcs (cmake never traced them), so the
	// partition pass above leaves them un-handled — addPlatform
	// ConditionalSrcsToAdd writes them straight into
	// PerPlatform["srcs"][selectKey] for each affected target.
	//
	// We add only to the wrapper target — multi-language splits
	// distribute Tier-1 sources across per-language sub-libs by
	// CompileGroupIndex (which cmake's codemodel populates from
	// the executed configure). Tier 2 sources have no compile-
	// group attribution by definition, so the wrapper is the
	// only consistent home for them. If a downstream needs
	// per-language Tier 2 attribution, a Tier 3 pass would have
	// to re-derive the language from source extension.
	if len(platformConditionalSrcsToAdd) > 0 {
		addPlatformConditionalSrcs(irt, platformConditionalSrcsToAdd)
	}

	return irt, nil
}

// addPlatformConditionalSrcs appends sources Tier 2 recovered
// (from skipped if-arms cmake never executed) directly into
// the target's PerPlatform["srcs"][selectKey] map. Sources are
// deduplicated against the existing PerPlatform arms — a
// Tier-2 recovery should never overlap a Tier-1 attribution
// (the trace-entered arm vs. the skipped arm), but the dedup
// keeps the invariant honest if a future shape (e.g. an
// elseif arm that the inner Tier-2 walker re-emits) tries to
// double-add.
//
// Each touched arm gets sorted post-add to keep emit's
// verbatim arm rendering byte-stable, matching what
// partitionPlatformConditionalSrcs does for Tier 1.
func addPlatformConditionalSrcs(t *ir.Target, srcsByKey map[string][]string) {
	if len(srcsByKey) == 0 {
		return
	}
	if t.PerPlatform == nil {
		t.PerPlatform = map[string]map[string][]string{}
	}
	if t.PerPlatform["srcs"] == nil {
		t.PerPlatform["srcs"] = map[string][]string{}
	}
	for key, srcs := range srcsByKey {
		existing := map[string]bool{}
		for _, s := range t.PerPlatform["srcs"][key] {
			existing[s] = true
		}
		for _, s := range srcs {
			if existing[s] {
				continue
			}
			existing[s] = true
			t.PerPlatform["srcs"][key] = append(t.PerPlatform["srcs"][key], s)
		}
		sort.Strings(t.PerPlatform["srcs"][key])
	}
}

// partitionPlatformConditionalSrcs moves any src in t.Srcs
// whose path appears in srcToSelectKey into
// t.PerPlatform["srcs"][selectKey], then sorts each affected
// arm so emit's verbatim arm rendering is byte-stable.
//
// Used by lowerTarget to apply the #217 Tier 1 partition both
// to the wrapper target and to splitCompileGroups's per-
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

// intSuffix is itoa for the splitCompileGroups sub-name
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

// splitCompileGroups rewrites irt as a deps-only wrapper and
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
func splitCompileGroups(t *fileapi.Target, irt *ir.Target, cc *codegenContext, cmakeSrc, cmakeBuild string) error {
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
		// Pick up per-CG LanguageStandard so sub-libraries
		// inherit the cmake-recorded -std=c++17 / -std=c11
		// (idempotent if the CG's CompileCommandFragments
		// already inlined a -std flag).
		copts = prependLanguageStandardCopt(cg.Language, cg.LanguageStandard, copts)
		// Sub-library defines: same bake-first / reanchor-fallback
		// dispatch as the main target loop. Bake mutates a local
		// subHdrs / subTags pair (not sub yet — sub is built
		// further down) so the AUTOINIT header lands in the
		// sub-lib's Hdrs closure where its compile action runs.
		subHdrs := append([]string{}, sharedHdrs...)
		var subTags []string
		subBakeView := ir.Target{Hdrs: subHdrs, Tags: subTags}
		for _, d := range cg.Defines {
			if baked, ok := bakeAutoinitIncludeDefine(d.Define, cmakeBuild, cc, &subBakeView); ok {
				defs = append(defs, baked)
				continue
			}
			if reanchored, keep := reanchorDefineValue(d.Define, cmakeSrc, cmakeBuild); keep {
				defs = append(defs, reanchored)
			}
		}
		subHdrs = subBakeView.Hdrs
		subTags = subBakeView.Tags

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
			Hdrs:       subHdrs,
			Includes:   sharedIncludes,
			Copts:      copts,
			Defines:    defs,
			Tags:       subTags,
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

// optionsHeaderComments projects cmake option()-style cache entries
// into a documentation block at the BUILD head. Detection: cache
// entry Type=="BOOL" with the property HELPSTRING that came from
// an option() declaration (cmake records the helpstring as the
// option's description).
//
// Output shape:
//
//	# cmake options resolved at convert time (values baked in;
//	# re-convert to change):
//	#   - FOO_ENABLE_TESTS = ON (Enable tests)
//	#   - FOO_USE_GPU = OFF (Build with GPU acceleration)
//
// Operators see the toggle inventory and remember to re-convert
// (or rewrite the BUILD) if they want a different value. cmake's
// option() is configure-time-resolved; the Bazel equivalent
// (bool_flag / config_setting select()s) requires re-emitting,
// which is out of scope for the converter's one-shot lift.
//
// Deterministic ordering: alphabetical by option name.
func optionsHeaderComments(cache fileapi.Cache) []string {
	type entry struct{ name, value, doc string }
	var entries []entry
	for _, e := range cache.Entries {
		if e.Type != "BOOL" {
			continue
		}
		// Filter out cmake-internal BOOL entries; operator-defined
		// option() declarations carry HELPSTRING. cmake builtins
		// (CMAKE_VERBOSE_MAKEFILE etc.) also have HELPSTRING but
		// start with `CMAKE_` — skip those to keep the list to
		// project options.
		if strings.HasPrefix(e.Name, "CMAKE_") {
			continue
		}
		var doc string
		for _, p := range e.Properties {
			if p.Name == "HELPSTRING" {
				doc = p.Value
				break
			}
		}
		entries = append(entries, entry{name: e.Name, value: e.Value, doc: doc})
	}
	if len(entries) == 0 {
		return nil
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].name < entries[j].name })
	out := []string{
		"",
		"cmake options resolved at convert time (values baked in; re-convert to change):",
	}
	for _, e := range entries {
		line := "  - " + e.name + " = " + e.value
		if e.doc != "" {
			line += " (" + e.doc + ")"
		}
		out = append(out, line)
	}
	return out
}

// packagePrefix returns the cmake package name when cmakeName is
// in the `<Package>::<Component>` find_package convention shape
// (e.g. "Boost::system" → "Boost"). Returns "" for plain target
// names that don't follow the namespaced shape — those are
// either in-codebase targets or unscoped IMPORTED libraries
// where we can't reliably attribute the package.
func packagePrefix(cmakeName string) string {
	if i := strings.Index(cmakeName, "::"); i > 0 {
		return cmakeName[:i]
	}
	return ""
}

// deprecationHeaderComments projects cmake message(DEPRECATION ...)
// events into a header-comment block. Operators see the cmake-side
// deprecation surface at convert time without scrolling through
// configure output.
//
// Limited to DEPRECATION mode (skips STATUS / WARNING / FATAL_ERROR
// — those land in cmake's stderr). Deduplicated by message body so
// the same deprecation called from N sites only surfaces once.
func deprecationHeaderComments(events []fileapi.Event) []string {
	if len(events) == 0 {
		return nil
	}
	seen := map[string]bool{}
	var msgs []string
	for _, e := range events {
		if e.Kind != "message-v1" {
			continue
		}
		if !strings.EqualFold(e.Mode, "DEPRECATION") {
			continue
		}
		body := strings.TrimSpace(e.Message)
		if body == "" || seen[body] {
			continue
		}
		seen[body] = true
		msgs = append(msgs, body)
	}
	if len(msgs) == 0 {
		return nil
	}
	sort.Strings(msgs)
	out := []string{"", "cmake deprecation warnings:"}
	for _, m := range msgs {
		// Wrap multi-line messages with explicit indent.
		for i, line := range strings.Split(m, "\n") {
			if i == 0 {
				out = append(out, "  ! "+line)
			} else {
				out = append(out, "    "+line)
			}
		}
	}
	return out
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
		// Qt's auto-source-generation toggles (AUTOMOC / AUTOUIC /
		// AUTORCC). cmake's generator runs moc / uic / rcc as
		// part of `cmake --build`; outside cmake (i.e. under
		// Bazel) those don't fire, so any target with these
		// enabled MISSES the Qt-generated sources at compile
		// time. Surface as tags so operators see the gap and
		// route via a kind:bazel override that wraps moc / uic /
		// rcc as host-tool genrules. Bazel cc_library has no
		// native AUTOMOC equivalent.
		for _, qt := range []string{"AUTOMOC", "AUTOUIC", "AUTORCC"} {
			if cmakeTruthy(p.Properties[qt]) {
				tag := "cmake-codegen-qt-" + strings.ToLower(qt)
				if !stringSliceContains(tgt.Tags, tag) {
					tgt.Tags = append(tgt.Tags, tag)
				}
			}
		}
		// EXCLUDE_FROM_ALL — cmake skips this target when
		// building the default ALL target. Bazel's closest
		// match is `tags = ["manual"]`, which excludes the
		// target from `bazel build //...` wildcard expansion.
		if cmakeTruthy(p.Properties["EXCLUDE_FROM_ALL"]) {
			if !stringSliceContains(tgt.Tags, "manual") {
				tgt.Tags = append(tgt.Tags, "manual")
			}
			if !stringSliceContains(tgt.Tags, "cmake-codegen-exclude-from-all") {
				tgt.Tags = append(tgt.Tags, "cmake-codegen-exclude-from-all")
			}
		}
		// MSVC_RUNTIME_LIBRARY — Windows-only runtime selection
		// (MultiThreaded vs MultiThreadedDLL, with/without
		// Debug). Bazel cc_library has no direct attribute;
		// the operator's cc_toolchain feature owns the actual
		// /MT vs /MD flag. Surface as tag.
		if v := strings.TrimSpace(p.Properties["MSVC_RUNTIME_LIBRARY"]); v != "" {
			tag := "cmake-codegen-msvc-runtime=" + v
			if !stringSliceContains(tgt.Tags, tag) {
				tgt.Tags = append(tgt.Tags, tag)
			}
		}
		// JOB_POOL_COMPILE / JOB_POOL_LINK — ninja-specific
		// job-pool routing for compile/link actions. Bazel's
		// closest analog is `exec_properties = {"pool": "..."}`
		// under remote execution. Surface as tag so operators
		// see the cmake-side intent.
		for _, jp := range []string{"JOB_POOL_COMPILE", "JOB_POOL_LINK"} {
			if v := strings.TrimSpace(p.Properties[jp]); v != "" {
				tag := "cmake-codegen-" + strings.ReplaceAll(strings.ToLower(jp), "_", "-") + "=" + v
				if !stringSliceContains(tgt.Tags, tag) {
					tgt.Tags = append(tgt.Tags, tag)
				}
			}
		}
		// CXX_EXTENSIONS / C_EXTENSIONS — toggle gnu extensions on
		// the language standard flag. cmake's default is ON
		// (gnu++NN / gnuNN); our prepend hardcodes the strict
		// form (c++NN / cNN). Rewrite the prepended copts to
		// match the cmake-recorded extension state.
		//
		// CXX_EXTENSIONS empty (unset) means cmake defaults to
		// ON — but since we can't tell from the probe alone
		// whether the property was unset vs explicitly empty,
		// only rewrite when the property is explicitly truthy.
		// Operators relying on the gnu default can either
		// `set(CMAKE_CXX_EXTENSIONS ON)` (no-op but makes intent
		// explicit) or hand-edit the resulting BUILD copts.
		rewriteStdForExtensions(tgt, "CXX_EXTENSIONS", p.Properties["CXX_EXTENSIONS"], "c++", "gnu++")
		rewriteStdForExtensions(tgt, "C_EXTENSIONS", p.Properties["C_EXTENSIONS"], "c", "gnu")
	}
}

// rewriteStdForExtensions walks tgt.Copts looking for our
// prepended language-standard flag (e.g. -std=c++17 or -std=c11)
// and rewrites it to the gnu-extension form (-std=gnu++17 /
// -std=gnu11) when the cmake-recorded LANGUAGE_EXTENSIONS
// property is truthy. When the property is explicitly OFF /
// FALSE / 0, no rewrite — strict form already matches cmake.
//
// strictPrefix is the bare-standard prefix (e.g. "c++", "c");
// gnuPrefix is the extensions form (e.g. "gnu++", "gnu"). The
// rewrite preserves the version suffix verbatim.
//
// Safety: only rewrites the exact `-std=<strictPrefix>NN` shape;
// leaves unrelated copts alone. Idempotent (rewriting -std=gnu++17
// to gnu++17 is a no-op).
func rewriteStdForExtensions(tgt *ir.Target, propName, propVal, strictPrefix, gnuPrefix string) {
	if !cmakeTruthy(propVal) {
		return
	}
	prefix := "-std=" + strictPrefix
	for i, c := range tgt.Copts {
		if !strings.HasPrefix(c, prefix) {
			continue
		}
		version := strings.TrimPrefix(c, prefix)
		if version == "" {
			continue
		}
		// Guard against `c` prefix matching `c++` flags: the
		// trimmed remainder must start with a digit (the
		// standard version number), not another letter.
		if version[0] < '0' || version[0] > '9' {
			continue
		}
		tgt.Copts[i] = "-std=" + gnuPrefix + version
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

// collectLanguageOverrides walks the decoded
// set_source_files_properties calls and returns a map from source
// path → LANGUAGE property value. cmake records the override
// verbatim ("C", "CXX", etc.); we preserve case so the tag
// surfaces what the project actually declared.
//
// Multiple LANGUAGE declarations for the same source — last-write-
// wins on conflict (cmake's documented behavior).
//
// Returns nil when no LANGUAGE property surfaces.
func collectLanguageOverrides(calls []shadow.SourceFilePropertiesCall) map[string]string {
	var out map[string]string
	for _, call := range calls {
		for _, prop := range call.Properties {
			if !strings.EqualFold(prop.Name, "LANGUAGE") {
				continue
			}
			if prop.Value == "" {
				continue
			}
			for _, f := range call.Files {
				if out == nil {
					out = map[string]string{}
				}
				out[f] = prop.Value
			}
		}
	}
	return out
}

// tagLanguageOverrides walks every target in pkg.Targets and tags
// each one whose Srcs include a path with a LANGUAGE override.
// Tag shape: cmake-codegen-language-override=<lang> so operators
// can grep by target language ('CXX' = forced C++, 'C' = forced
// C, etc.).
//
// One tag per distinct override-language a target uses — a
// library with multiple .c files all forced to CXX gets one tag,
// not N.
//
// No-op when byPath is empty.
func tagLanguageOverrides(pkg *ir.Package, byPath map[string]string) {
	if len(byPath) == 0 || pkg == nil {
		return
	}
	for i := range pkg.Targets {
		tgt := &pkg.Targets[i]
		if tgt.Kind != ir.KindCCLibrary && tgt.Kind != ir.KindCCBinary &&
			tgt.Kind != ir.KindCCInterface && tgt.Kind != ir.KindCCTest {
			continue
		}
		seen := map[string]bool{}
		for _, src := range tgt.Srcs {
			lang, ok := byPath[src]
			if !ok || seen[lang] {
				continue
			}
			seen[lang] = true
			tag := "cmake-codegen-language-override=" + lang
			if !stringSliceContains(tgt.Tags, tag) {
				tgt.Tags = append(tgt.Tags, tag)
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
// ROADMAP.md "Easy" section).
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

// rewriteGenruleCmd strips convert-time-only constructs from a
// custom-command's verbatim shell command before it lands as the
// genrule's cmd attribute. Specifically:
//
//   - `cd <abs-under-buildDir> && ` prefix: cmake's Ninja generator
//     prepends this so the command runs in the per-target build
//     subdir during the convert-time configure. Bazel genrules run
//     in $(GENDIR) which is Bazel-managed; the cd path doesn't
//     exist at action time. Drop the prefix entirely.
//
//   - Verbatim `<cmakeSrc>/`-rooted path references: re-anchor to
//     workspace-relative by stripping the convert-time prefix.
//
//   - Verbatim `<buildDir>/`-rooted path references: re-anchor to
//     genrule-output-relative by stripping the convert-time prefix.
//     For files the genrule itself produces this is correct (Bazel
//     puts genrule outputs under $(GENDIR) at the matching relative
//     path); for files the genrule consumes from the convert-time
//     build dir that aren't reproduced at Bazel build time, the
//     stripped reference will still fail — but the diagnostic at
//     action time names the workspace-relative path the operator
//     can investigate, not the convert-time absolute prefix.
//
// All rewrites are conservative — paths only re-anchor when they
// start with the canonical anchor prefix + "/", so partial-match
// hazards (e.g. `<buildDir>` vs `<buildDir>_other`) are avoided.
func rewriteGenruleCmd(cmd, cmakeSrc, buildDir string) string {
	if cmd == "" {
		return cmd
	}
	// Strip `cd <abs> && ` prefix when <abs> is under buildDir or
	// cmakeSrc. Bazel runs the genrule in its sandbox-rooted
	// $(GENDIR); the cd is cmake-internal. Capture the stripped
	// subdir's build-dir-relative form so the later redirect-
	// qualification pass can prepend it to bare-basename redirect
	// targets (cmake records `> LLVMHello.exports` as relative
	// to the cd dir; after the cd-strip the basename is in the
	// wrong cwd unless qualified).
	var strippedCdSubdir string
	if strings.HasPrefix(cmd, "cd ") {
		if end := strings.Index(cmd, " && "); end > 0 {
			target := strings.TrimSpace(strings.TrimPrefix(cmd[:end], "cd "))
			if filepath.IsAbs(target) {
				drop := false
				if buildDir != "" {
					if rel, ok := relativeIfInside(buildDir, target); ok {
						drop = true
						strippedCdSubdir = rel
					}
				}
				if !drop && cmakeSrc != "" {
					if rel, ok := relativeIfInside(cmakeSrc, target); ok {
						drop = true
						strippedCdSubdir = rel
					}
				}
				if drop {
					cmd = cmd[end+4:]
				}
			}
		}
	}
	// Strip cmakeSrc and buildDir prefixes from the cmd body.
	// Two variants per anchor:
	//
	//   - <anchor>/<rel> → <rel>      (the typical embedded-path
	//     case; trailing slash ensures partial-match safety
	//     against e.g. <buildDir>_other).
	//   - bare <anchor> at an argv boundary → "." (Bazel's
	//     genrule cwd / workspace root, depending on direction).
	//     The boundary requirement (whitespace / quote / argv
	//     separator on the right side) avoids mangling argv
	//     values that happen to start with the anchor prefix
	//     but continue with letters or digits (e.g. <buildDir>_other
	//     stays intact; <buildDir> followed by space or quote
	//     gets re-anchored).
	for _, anchor := range []string{cmakeSrc, buildDir} {
		if anchor == "" {
			continue
		}
		cmd = strings.ReplaceAll(cmd, anchor+"/", "")
		cmd = replaceBareAnchorAtBoundary(cmd, anchor)
	}
	// Strip well-known host-bin tool prefixes so the command relies
	// on PATH (the operator's responsibility) instead of baking the
	// convert-host's filesystem layout. Bazel's sandbox typically
	// provides /usr/bin on PATH but the cross-distro picture is
	// noisy (Alpine, NixOS, custom images) — `/usr/local/bin/python3`
	// definitely doesn't exist on Debian/Ubuntu where Bazel images
	// are commonly based. The bare-name form (`cmake`, `python3`)
	// resolves via PATH on every host that has the tool installed.
	for _, prefix := range []string{
		"/usr/bin/",
		"/usr/local/bin/",
	} {
		// Replace only when the prefix sits at a word boundary
		// (start of cmd, or preceded by whitespace / `&&` / `||`
		// / `;` / `|` / `(` ) so we don't accidentally maul an
		// `<absbuild>/.../usr/bin/...` payload. Conservative
		// regex would handle this; for a one-off rewrite, a
		// HasPrefix check at cmd start + a `&& <prefix>` /
		// `|| <prefix>` / `; <prefix>` / `| <prefix>` substring
		// scan covers the common cases.
		cmd = stripToolPrefixAtBoundaries(cmd, prefix)
	}
	// Rewrite `cmake -E <op> ...` to POSIX equivalents — keeps
	// the rendered genrule cmd portable in Bazel's bash sandbox
	// without needing cmake at action time. Runs after the
	// host-bin strip so the `cmake` token is already bare.
	cmd = rewriteCMakeEInvocations(cmd)
	// Qualify bare-basename redirect targets with the stripped
	// cd-subdir prefix. cmake emits `cd <subdir> && ... >
	// <basename>` for per-target output paths; after the cd-strip
	// the basename is in the wrong cwd unless prepended with the
	// subdir. Runs last so all upstream rewrites have settled
	// their tokens (e.g. cmake -E echo's args don't get rewritten
	// twice).
	if strippedCdSubdir != "" {
		cmd = qualifyRedirectBasenames(cmd, strippedCdSubdir)
	}
	return cmd
}

// replaceBareAnchorAtBoundary replaces `anchor` (no trailing slash)
// with `.` whenever it sits at an argv-token boundary in `cmd`.
// "Boundary" = the character immediately after `anchor` is one of:
// whitespace, double-quote, single-quote, `=` (DKEY=VALUE shape),
// shell command-separator (`&`, `|`, `;`), or end-of-string.
//
// The argv-boundary requirement avoids mangling argv values that
// happen to start with the anchor prefix but continue with letters
// or digits (e.g. `<buildDir>_other` stays intact). Conservative on
// purpose — the cmake-emitted shapes that surface this (LLVM's
// -DLLVM_SOURCE_DIR=<abs-src>, VTK's -DCMAKE_BINARY_DIR=<abs-build>)
// all hit a clean argv boundary.
func replaceBareAnchorAtBoundary(cmd, anchor string) string {
	if anchor == "" {
		return cmd
	}
	var b strings.Builder
	i := 0
	for i < len(cmd) {
		if i+len(anchor) <= len(cmd) && cmd[i:i+len(anchor)] == anchor {
			endByte := byte(0)
			if i+len(anchor) < len(cmd) {
				endByte = cmd[i+len(anchor)]
			}
			isBoundary := endByte == 0 ||
				endByte == ' ' || endByte == '\t' || endByte == '\n' ||
				endByte == '"' || endByte == '\'' ||
				endByte == '=' || endByte == '&' || endByte == '|' || endByte == ';'
			if isBoundary {
				b.WriteByte('.')
				i += len(anchor)
				continue
			}
		}
		b.WriteByte(cmd[i])
		i++
	}
	return b.String()
}

// stripToolPrefixAtBoundaries removes `prefix` from `cmd` wherever
// it sits at the start of a command word — start of cmd, or right
// after a shell command-separator. Conservative: misses prefix
// occurrences inside argv args (e.g. `--option=/usr/bin/...`) on
// purpose; those are typically genuine path values where the host
// layout is operator-significant.
func stripToolPrefixAtBoundaries(cmd, prefix string) string {
	// Cmd-start prefix.
	if strings.HasPrefix(cmd, prefix) {
		cmd = cmd[len(prefix):]
	}
	// Common shell separators followed by the prefix. Each separator
	// is space-padded by cmake's emit.
	for _, sep := range []string{
		" && ",
		" || ",
		" ; ",
		" | ",
	} {
		needle := sep + prefix
		cmd = strings.ReplaceAll(cmd, needle, sep)
	}
	return cmd
}

// reanchorDefineValue rewrites convert-time absolute paths
// embedded in a preprocessor define's value to a form that
// survives into Bazel's hermetic build. Returns (define, keep):
// keep=false signals "drop this define entirely" — used when
// the value points at a convert-time-generated file in the
// cmake build dir that won't survive into Bazel's input closure.
//
// The shape this targets: `KEY="<absolute-path>"`. VTK's
// `vtkRenderingCore_AUTOINIT_INCLUDE="/tmp/<build>/CMakeFiles/
// vtkModuleAutoInit_<hash>.h"` is the canonical case; cmake
// generates these auto-init headers per-module at configure
// time and embeds the absolute path as a preprocessor define.
// Bazel sandbox-misses the file at action time.
//
// Behaviour:
//
//   - Define value with no embedded absolute path → unchanged.
//   - Path under cmakeSrc → re-anchor + requote.
//   - Path under buildDir → drop (cmake-internal; convert-time-
//     generated file isn't reachable at Bazel build time).
//   - Other absolute path → leave alone (operator's responsibility).
func reanchorDefineValue(def, cmakeSrc, buildDir string) (string, bool) {
	eq := strings.IndexByte(def, '=')
	if eq < 0 {
		return def, true
	}
	key, raw := def[:eq], def[eq+1:]
	stripped := strings.Trim(raw, `"`)
	if !filepath.IsAbs(stripped) {
		return def, true
	}
	if buildDir != "" {
		if _, ok := relativeIfInside(buildDir, stripped); ok {
			return "", false
		}
	}
	if cmakeSrc != "" {
		if rel, ok := relativeIfInside(cmakeSrc, stripped); ok {
			return key + `="` + rel + `"`, true
		}
	}
	return def, true
}

// reanchorLinkOptToken rewrites convert-time absolute paths embedded
// in a tokenised linker flag to a form that survives into Bazel's
// hermetic build. Returns (token, keep): keep=false signals "drop
// this token entirely" — used for cmake-internal flags whose value
// is the convert-time build dir (Bazel has no equivalent and the
// flag would refer to a path that doesn't exist at action time).
//
// Rewrites:
//
//   - `-Wl,-rpath-link,<absbuild>...` / `-Wl,-rpath,<absbuild>...`:
//     cmake's Ninja generator emits these so the convert-time
//     build can find sibling libraries during link without
//     re-running the configure cache. Bazel's hermetic action
//     model resolves deps through cc_library labels and doesn't
//     need rpath-link at link time. Drop. (Source-relative
//     -rpath / -rpath-link is legitimate runtime metadata for
//     downstream consumers and stays.)
//
//   - `-Wl,--version-script,<srcabs>` / `-Wl,--retain-symbols-file,
//     <srcabs>` and the comma-quoted variants: re-anchor the
//     embedded path to source-relative slash form when it sits
//     under cmakeSrc. The operator's BUILD.bazel ends up with
//     e.g. `-Wl,--version-script,"zlib.map"`. (A workspace-relative
//     reference still leaks the convert-time current-working-dir
//     assumption; queued is a follow-up that swaps this for
//     `additional_linker_inputs = [...]` + `$(location ...)`. For
//     now, removing the absolute prefix is the table-stakes fix.)
//
// Tokens without an embedded absolute path pass through unchanged.
func reanchorLinkOptToken(tok, cmakeSrc, buildDir string) (string, bool) {
	rewritten, keep, _ := reanchorLinkOptTokenWithInput(tok, cmakeSrc, buildDir)
	return rewritten, keep
}

// reanchorLinkOptTokenWithInput is the staging-aware variant that
// also returns a workspace-relative file path the caller should
// add to the target's additional_linker_inputs. Empty
// additionalInput means no file needs staging (or the source-side
// reanchor already produced a self-contained linkopts token).
//
// When the token references a source-tree file (e.g. zlib's
// `-Wl,--version-script,"/tmp/zlib/zlib.map"`), the rewritten
// token uses Bazel's `$(location ...)` substitution to resolve
// the file at link action time, AND the workspace-relative path
// (`zlib.map`) is returned so the caller can pin the file into
// the rule's additional_linker_inputs. Closes the gap left by
// the prior reanchor that rewrote the path's prefix but didn't
// stage the file.
func reanchorLinkOptTokenWithInput(tok, cmakeSrc, buildDir string) (string, bool, string) {
	if tok == "" {
		return tok, true, ""
	}
	// cmake-internal rpath-link to the build dir's per-config lib
	// dir. cmake's generator emits one per target; Bazel's link
	// action doesn't need rpath-link because the action's input
	// closure pins every library participating in the link. The
	// reference also baked the convert-time absolute path AND
	// often an unresolved ${CONFIGURATION} placeholder, neither
	// of which would resolve at Bazel build time.
	for _, prefix := range []string{
		"-Wl,-rpath-link,",
		"-Wl,-rpath,",
	} {
		if strings.HasPrefix(tok, prefix) {
			payload := tok[len(prefix):]
			if buildDir != "" && filepath.IsAbs(payload) {
				if _, ok := relativeIfInside(buildDir, payload); ok {
					return "", false, ""
				}
			}
			return tok, true, ""
		}
	}
	// version-script / retain-symbols-file embed a single path in
	// the linker wire shape `-Wl,--<name>,<path>` (with comma) or
	// `-Wl,--<name>=<path>` (with `=`), often with `<path>` quoted.
	// Both forms are accepted by ld/gold/lld.
	//
	// Source-tree-rooted paths: rewrite to use Bazel's `$(location
	// <rel>)` substitution and return the workspace-relative path
	// for staging via additional_linker_inputs. The emitted
	// linkopts entry then resolves the path at link-action time
	// to whatever Bazel staged the source under in the sandbox.
	//
	// Build-dir-rooted paths: drop the token. The convert-time
	// generated .exports / .map file isn't reachable through any
	// Bazel input closure; an operator who needs the linker
	// directive must wire a producer genrule themselves.
	for _, prefix := range []string{
		`-Wl,--version-script,`,
		`-Wl,--version-script=`,
		`-Wl,--retain-symbols-file,`,
		`-Wl,--retain-symbols-file=`,
		`-Wl,--dynamic-list,`,
		`-Wl,--dynamic-list=`,
	} {
		if !strings.HasPrefix(tok, prefix) {
			continue
		}
		raw := tok[len(prefix):]
		// Strip wrapping double- or single-quotes. cmake serialises
		// the path either way depending on the originating
		// CMakeLists shape; libpng uses single quotes, zlib uses
		// double quotes for the same `--version-script` flag.
		stripped := raw
		if (strings.HasPrefix(raw, `"`) && strings.HasSuffix(raw, `"`)) ||
			(strings.HasPrefix(raw, `'`) && strings.HasSuffix(raw, `'`)) {
			stripped = raw[1 : len(raw)-1]
		}
		if !filepath.IsAbs(stripped) {
			return tok, true, ""
		}
		if buildDir != "" {
			if _, ok := relativeIfInside(buildDir, stripped); ok {
				return "", false, ""
			}
		}
		if cmakeSrc != "" {
			if rel, ok := relativeIfInside(cmakeSrc, stripped); ok {
				// Use $(location <rel>) so Bazel resolves the
				// path at link time. additional_linker_inputs
				// (returned via addlInput) pins the file into
				// the action's input closure so the location
				// substitution succeeds.
				return prefix + `"$(location ` + rel + `)"`, true, rel
			}
		}
		return tok, true, ""
	}
	return tok, true, ""
}

// splitCompileFragments parses each whitespace-delimited fragment piece. -D
// pieces are returned as defines (with the leading -D stripped); everything
// else is a copt. -I and -isystem are dropped — those come through
// compileGroup.includes structurally. -ffile-prefix-map / -fmacro-prefix-map
// / -fdebug-prefix-map are dropped too — they're reproducible-build
// directives that rewrite the compile-host's source path layout into debug
// info; under Bazel's hermetic sandbox the compile-time paths are already
// workspace-relative, so the directive's <from> path (typically a convert-
// time absolute like /tmp/proj/) never matches anything in the compile
// invocation and the flag becomes either a no-op or a leak of convert-time
// state into the audit surface.
func splitCompileFragments(frags []fileapi.CommandFragment) (copts, defines []string) {
	// Order-preserving dedup against cmake's own duplication —
	// projects with `add_compile_options` chains, transitive
	// PUBLIC propagation through multiple deps, or just hand-
	// duplicated CMakeLists.txt entries surface the same flag
	// in CompileCommandFragments multiple times. Keep the first
	// occurrence (cmake's argv-order semantics for warning /
	// language flags care about first-vs-last only when the
	// flags conflict; identical-flag dedup is unambiguously
	// safe). Same dedup applies to defines.
	coptsSeen := map[string]bool{}
	defSeen := map[string]bool{}
	for _, f := range frags {
		if f.Role != "" {
			// Reserved for link fragments; ignore on the compile side.
			continue
		}
		for _, p := range strings.Fields(f.Fragment) {
			switch {
			case strings.HasPrefix(p, "-D"):
				val := strings.TrimPrefix(p, "-D")
				if defSeen[val] {
					continue
				}
				defSeen[val] = true
				defines = append(defines, val)
			case strings.HasPrefix(p, "-I"), strings.HasPrefix(p, "-isystem"):
				// dropped: see compileGroup.includes
			case strings.HasPrefix(p, "-ffile-prefix-map="),
				strings.HasPrefix(p, "-fmacro-prefix-map="),
				strings.HasPrefix(p, "-fdebug-prefix-map="):
				// dropped: convert-time host-path-rewrite
				// directives have no meaning under Bazel's
				// hermetic compile (the <from> never matches
				// anything the compiler sees).
			default:
				if coptsSeen[p] {
					continue
				}
				coptsSeen[p] = true
				copts = append(copts, p)
			}
		}
	}
	return copts, defines
}

// subPackageDir returns the element-root-relative directory the target at
// dirIndex was declared in, expressed against the same base srcs are
// relativized against (workspaceRoot when set and a strict ancestor of
// cmakeSrc, else cmakeSrc). "" means the root package.
//
// The codemodel's ConfigDirectory.Source is cmakeSrc-relative ("." for the
// top-level CMakeLists, "src/util" for an add_subdirectory child). When the
// label base is the workspace root above cmakeSrc, prepend the cmakeSrc-
// under-workspaceRoot prefix so the recorded dir lines up with the
// re-anchored source labels.
func subPackageDir(cfg fileapi.Configuration, dirIndex int, cmakeSrc, workspaceRoot string) string {
	if dirIndex < 0 || dirIndex >= len(cfg.Directories) {
		return ""
	}
	src := cfg.Directories[dirIndex].Source
	src = filepath.ToSlash(src)
	if src == "." {
		src = ""
	}
	src = strings.TrimSuffix(src, "/")
	// Re-anchor to workspaceRoot when it sits strictly above cmakeSrc,
	// matching lowerTarget's labelRoot pick.
	if workspaceRoot != "" && workspaceRoot != cmakeSrc {
		if prefix, inside := relativeIfInside(workspaceRoot, cmakeSrc); inside && prefix != "" {
			if src == "" {
				return prefix
			}
			return prefix + "/" + src
		}
	}
	return src
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
func discoverHeaders(sourceRoot string, includeDirs []string, cache map[string][]string, missing map[string]bool) ([]string, error) {
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
		// cmake's target_include_directories accepts paths that
		// don't physically exist on disk — LLVM's llvm-mca declares
		// `target_include_directories(... include)` with no
		// include/ subdir present, presumably for future headers.
		// Stat-check the dir before WalkDir so an honest "no such
		// directory" case yields zero headers instead of aborting
		// the whole conversion. Other walk errors (permission
		// denied mid-walk, etc.) still propagate. Record the dir
		// in `missing` so ToIR can stderr-warn once at the end
		// (silent skip is wrong — operator should see the cmake
		// oddity even though it isn't fatal).
		if st, statErr := os.Stat(absDir); statErr != nil {
			if errors.Is(statErr, fs.ErrNotExist) {
				if cache != nil {
					cache[absDir] = nil
				}
				if missing != nil {
					missing[absDir] = true
				}
				continue
			}
			return nil, fmt.Errorf("stat include dir %q: %w", absDir, statErr)
		} else if !st.IsDir() {
			if cache != nil {
				cache[absDir] = nil
			}
			if missing != nil {
				missing[absDir] = true
			}
			continue
		}
		var perDir []string
		walkErr := filepath.WalkDir(absDir, func(p string, d os.DirEntry, err error) error {
			if err != nil {
				// Post-stat walk errors (permission denied mid-walk,
				// I/O failures, etc.) still surface — the absence
				// case was handled above.
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
