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
	"github.com/sstriker/buildstream-bazel/converter/internal/coverage"
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

	// EmitInstallExportConfig opts in to generating the install(EXPORT)
	// config-mode bundle — the real <Pkg>Targets.cmake + the cmake_config_bundle
	// filegroup (exportshape.EmitInputs.EmitConfig). OFF by default: the
	// orchestrated graph wires the synthprefix-synthesized bundle, so the
	// converter's standalone bundle is unused and emitting it would only break
	// `bazel build //...`. A project shipping the element for EXTERNAL cmake
	// config-mode consumption opts in (--emit-install-export-config).
	EmitInstallExportConfig bool

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

	// SetAssignments carries verbatim `set(X ${Y})` variable copies
	// recovered from a NON-EXPANDED trace (shadow.ExtractSetAssignments),
	// supplied by the driver's warm second-configure pass. ToIR walks
	// them after recoverExecuteProcess so a variable copied from a
	// VCS-stamp var inherits its workspace-status key — letting a
	// configure_file referencing the copy (`set(VERSION ${GIT_SHA})`;
	// `@VERSION@`) lift to stamp_values. Empty (the single-pass default)
	// leaves only the direct stamp vars.
	SetAssignments []shadow.SetAssignment

	// ParentScopeForwards carries function-parameter-forwarded stamp writes
	// recovered from the same NON-EXPANDED trace
	// (shadow.ExtractParentScopeForwards): a helper's
	// `set(${_var} "${out}" PARENT_SCOPE)` resolved to the caller argument the
	// parameter was bound to (`get_git_sha(GIT_SHA)` -> GIT_SHA). ToIR marks
	// the resolved consumer as a stamp var (re-keyed to its own name) before
	// propagation, so a configure_file referencing it (`@GIT_SHA@`) lifts to
	// stamp_values instead of baking the convert-time revision. Empty (the
	// single-pass default) leaves a function-forwarded stamp baked.
	ParentScopeForwards []shadow.ParentScopeForward

	// StampVarSink, when non-nil, receives a copy of the recovered
	// stamp-variable set (var -> workspace-status key) after
	// recoverExecuteProcess + propagation. The driver reads it after
	// pass 1 to decide whether a stamp set()-indirection second pass is
	// worth running (non-empty => the project has VCS-stamp vars).
	StampVarSink map[string]string

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

	// BackedFeatures is the operator's real cc_toolchain feature vocabulary
	// (feature names enumerated from their toolchain Starlark by
	// toolchainscan.ParseDeclared). The nil-vs-empty distinction is load-
	// bearing: NON-NIL (operator supplied --toolchain-features-from) gates the
	// raw-flag → feature lift on exactly this vocabulary — even when empty (a
	// toolchain whose features the parser couldn't read lifts only the
	// built-in `pic`, never falling back to the generated default and dropping
	// flags onto features the real toolchain lacks). NIL (no operator
	// toolchain) keeps the generated-toolchain default vocabulary.
	BackedFeatures []string

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

	// Coverage, when non-nil, collects lens-3 ("did we lose intent
	// vs the CMakeLists?") findings — losses the converter would
	// otherwise not self-report. v1 records dependency-coverage gaps
	// (a trace target_link_libraries arm naming an in-codebase target
	// that didn't land in any dep bucket). Surfaced via
	// --audit-coverage-report; see converter/internal/coverage.
	Coverage *coverage.Collector

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

	// LiftCCEmbed recognizes a known file-embedding cmake -P encoder
	// (vtkEncodeString) and lowers it to the native cc_embed rule. Off by
	// default; see codegenContext.LiftCCEmbed.
	LiftCCEmbed bool

	// LiftCCHash recognizes a known file-hashing cmake -P script
	// (vtkHashSource) and lowers it to the native cc_hash rule. Off by
	// default; see codegenContext.LiftCCHash.
	LiftCCHash bool

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

	// BazelPackagePath is the repo-root-relative path of the destination
	// Bazel package (e.g. "elements/hello-world"), mirroring the
	// convert-element-cmake flag of the same name. Empty means the element
	// converts AT the workspace root ("//"). It gates the package-root
	// (`includes = ["."]`) include the configure_file consumer adds: Bazel
	// rejects a root `includes` entry ("'.' resolves to the workspace
	// root"), so that include is only valid — and only emitted — when the
	// element lands in a sub-package (BazelPackagePath != "").
	BazelPackagePath string

	// BakeIn controls the convert-time-baked-output post-pass.
	// Zero value (empty string) resolves to convmode.BakeInWarn so
	// callers leaving the field default get today's behaviour:
	// write the inventory to Warnings, but let conversion succeed.
	// convmode.BakeInAllow silences the inventory;
	// convmode.BakeInReject turns it into a Tier-2 refusal that
	// ToIR returns as an error. See baking_warnings.go for the
	// per-tag taxonomy.
	BakeIn convmode.BakeIn

	// LiteralProbeSink, when non-nil, collects arbitrary genex
	// literals the lifter could not resolve via the Go-side
	// evaluator + structural probe (GenexProbes). It drives the
	// warm second configure pass: pass 1 records unresolved
	// literals here, the orchestrator feeds Requests() to
	// cmakerun.Configure as Options.LiteralProbes, and pass 2 reads
	// the resolutions back into LiteralResolutions below. Nil
	// disables collection (the two-pass loop is opt-in), so unset
	// callers keep their existing single-pass behavior.
	//
	// Generalized-genex two-pass; see probe_literals.go.
	LiteralProbeSink *LiteralProbeSink

	// LiteralResolutions carries the second-pass results, keyed by
	// LiteralProbeRequest.Hash() (the same key LiteralProbeSink.Want
	// returns). On the second ToIR pass the lift sites that recorded
	// a request in pass 1 find the resolved value here and emit it
	// (flat, or a select() over //config:<name> when the literal
	// diverged per config) instead of recording or refusing. Empty
	// on the first pass and for single-pass callers.
	LiteralResolutions map[string]cmakerun.LiteralResolution
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
//
// `.def` and `.inc` cover the x-macro / textual-include idioms (a file of
// `HANDLE_FOO(...)` macro calls #included multiple times with different macro
// definitions, or a checked-in code fragment pulled in via quote-include)
// that LLVM and many C/C++ projects use pervasively — e.g. Demangle's
// `#include "ItaniumNodes.def"` and Support's regex engine
// `#include "regengine.inc"`. These are never compiled directly (so they
// belong in hdrs, not srcs) but must be staged as inputs or the quote-include
// misses in Bazel's sandbox. discoverHeaders only walks source-tree include
// roots, so the *generated* `.def`/`.inc` files (tablegen / write_file output
// into the build dir, e.g. Config/config.def or IR/Attributes.inc) aren't
// double-claimed here — their producing genrule/write_file owns them, and
// checked-in vs generated paths are disjoint in practice.
var headerExts = map[string]bool{
	".h":   true,
	".hh":  true,
	".hpp": true,
	".hxx": true,
	".inl": true,
	".def": true,
	".inc": true,
}

// ToIR lowers a parsed reply into a Package. The optional ninja graph
// enables genrule recovery for targets with isGenerated sources; pass nil to
// disable (M1-style behavior — generated sources then trigger
// unsupported-custom-command).
func ToIR(r *fileapi.Reply, g *ninja.Graph, opts Options) (*ir.Package, error) {
	if got := len(r.Codemodel.Configurations); got != 1 {
		// The multi-config fold (lowerMultiConfigDeltas, at the
		// end of this function) projects every non-primary
		// configuration's flag / src / dep deltas onto cfg[0]'s
		// targets as //config:<name> select() arms when
		// r.TargetsByConfig is populated — so multi-config
		// *intent* is captured, not surveyed-first-only. The one
		// thing that fold can't recover is a target built only in
		// a non-primary configuration: cfg[0]'s walk never sees
		// it, and lowerMultiConfigDeltas only augments targets
		// cfg[0] emitted, so it's silently dropped.
		dropped := configOnlyTargetNames(r.Codemodel.Configurations)
		if opts.Rejections != nil {
			// Diagnostic mode: flag exactly the config-only
			// targets (the genuine residual); stay silent when
			// cfg[0] covers the whole target set (the common
			// "same targets, differing per-config flags" case the
			// fold handles end-to-end).
			for _, name := range dropped {
				opts.Rejections.AddWithContext(failure.UnsupportedTargetType,
					"target built only in a non-primary configuration; the multi-config fold projects per-config deltas onto the primary configuration's targets, so a config-only target is dropped",
					name, "")
			}
		} else if len(dropped) > 0 {
			// Strict mode: the fold captures every config's
			// flag/src/dep intent as //config:<name> select() arms
			// (and convert-element-cmake --out-config-settings
			// emits the backing config_settings), so multi-config
			// is a supported path now. The lone thing the
			// first-config-primary fold can't recover is a target
			// built only in a non-primary configuration — that's
			// genuine intent loss, so strict mode still refuses it.
			return nil, failure.New(failure.UnsupportedTargetType,
				"target %q is built only in a non-primary configuration; the multi-config fold projects per-config deltas onto the primary configuration's targets, so a config-only target would be dropped (pass --diagnostics to convert the rest anyway)", dropped[0])
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
	// generatedSources holds slash-form source paths the trace
	// marked set_source_files_properties(... GENERATED TRUE). The
	// codemodel already flags add_custom_command / configure_file
	// outputs as IsGenerated, but a project can also mark a source
	// GENERATED manually — for those the lowerTarget missing-source
	// elision must NOT drop the file as "missing" (it's expected to
	// be produced by a generator, not present in the source tree).
	// Phase 1 slice 1c.
	var generatedSources map[string]bool
	// perSourceDefinesBySrc maps source paths to the list of
	// COMPILE_DEFINITIONS the trace recorded via
	// set_source_files_properties(... COMPILE_DEFINITIONS ...). The
	// post-pass folds these into the consuming target's `defines`
	// when they're uniform across the target's sources (or there's
	// a single source); genuinely per-file divergent defines aren't
	// expressible in a single cc_library and get a diagnostic tag
	// instead. Phase 1 slice 1c.
	var perSourceDefinesBySrc map[string][]string

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
		generatedSources = collectGeneratedSources(decoded.SourceFileProperties)
		perSourceDefinesBySrc = collectPerSourceCompileDefinitions(decoded.SourceFileProperties)
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
	// Only PROMOTE to the detected workspace root if the project actually
	// references sources OUTSIDE cmakeSrc (zstd's sibling-dir file(GLOB), which
	// the File API records as absolute paths). A self-contained subproject of a
	// larger git repo — LLVM's llvm-project/llvm — trips detectWorkspaceRoot on
	// the monorepo's .git but keeps every source under cmakeSrc; promoting there
	// injects a spurious `llvm/` umbrella prefix the converter applies
	// inconsistently across emitters (genrule srcs vs install(FILES)/root refs),
	// yielding a self-inconsistent single/double package tree no overlay can
	// satisfy. Gate on real escape so zstd still promotes and LLVM doesn't.
	if workspaceRoot != "" && workspaceRoot != cmakeSrc && !sourcesEscapeCmakeSrc(r, cmakeSrc, workspaceRoot) {
		workspaceRoot = ""
	}
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
	cc.LiftCCEmbed = opts.LiftCCEmbed
	cc.LiftCCHash = opts.LiftCCHash
	cc.CMakeBinary = lookupCmakeBinary()
	cc.Warnings = opts.Warnings
	cc.LiteralProbeSink = opts.LiteralProbeSink
	cc.LiteralResolutions = opts.LiteralResolutions

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
	// A stamp value forwarded onward by a recovered set() copy (the SrcVar of
	// a SetAssignment) — including a helper function's PARENT_SCOPE return —
	// reaches a consuming configure_file even when the execute_process's own
	// OUTPUT_VARIABLE is a function-local the dump-vars top-level snapshot
	// can't see, so recoverExecuteProcess rescues it rather than refusing
	// (git_describe()'s shape). Empty in the single-pass default (no recovered
	// copies), which preserves the uncaptured-stamp → round-2 refusal.
	forwardedStampVars := map[string]bool{}
	for _, a := range opts.SetAssignments {
		forwardedStampVars[a.SrcVar] = true
	}
	executeProcesses, executeProcessRefusals := recoverExecuteProcess(decodedExecuteProcesses, hostSrc, cmakeSrc, opts.BuildDir, cmakeBuild, opts.LiftConfigureFile, rescueVars, forwardedStampVars, cc)
	// Expand the stamp-var set through verbatim set(X ${Y}) copies the
	// driver recovered from a non-expanded trace (empty in the single-pass
	// default), so a configure_file referencing a copy of a VCS-stamp var
	// lifts to stamp_values. Then surface the (direct + propagated) set to
	// the optional sink for the driver's second-pass gate.
	// Resolve function-parameter-forwarded stamps (git_describe()'s
	// `set(${_var} "${out}" PARENT_SCOPE)`) to the caller-scope variable
	// before propagating verbatim copies, so the marked consumer (GIT_SHA)
	// also seeds any further `set(VERSION ${GIT_SHA})` copy of it.
	applyParentScopeForwards(cc.StampVars, opts.ParentScopeForwards)
	propagateStampVars(cc.StampVars, opts.SetAssignments)
	if opts.StampVarSink != nil {
		// Reset first: the driver reuses one sink across passes, and a
		// stale key from an earlier pass would misreport the set (and
		// could mis-gate the stamp second pass).
		for k := range opts.StampVarSink {
			delete(opts.StampVarSink, k)
		}
		for k, v := range cc.StampVars {
			opts.StampVarSink[k] = v
		}
	}
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
	// Structural-probe-resolved per-target genex facts (TARGET_FILE
	// family, TARGET_OBJECTS, INTERFACE_* aggregates), keyed by
	// cmake target name. Computed once here and shared by every
	// consumer that resolves genex against cmake's own
	// generation-time evaluator output: recoverFileGenerate's
	// template/OUTPUT resolution and lowerInterfaceLibraries'
	// INTERFACE_COMPILE_DEFINITIONS reconciliation. Empty when no
	// probe ran (no --probe-genex, cmake < 3.24); each consumer
	// degrades to its pre-probe behavior in that case.
	genexTargets := buildGenexTargets(r, cmakeBuild, opts.GenexProbes, decodedTrace, opts.Imports)

	var fileGenerates []fileGenerateOut
	if traceDecoded {
		var err error
		fileGenerates, err = recoverFileGenerate(decodedFileGenerates, hostSrc, cmakeSrc, opts.BuildDir, cmakeBuild, opts.LiftConfigureFile, opts.CMakeVars, genexTargets, opts.Imports, cc)
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
	// utilityIDToName records the name of each UTILITY (add_custom_target)
	// target — excluded from idToName because depending on one is a Bazel
	// no-op, but the consumer-side codegen-header wiring needs the name to
	// resolve the target's ninja phony to the generated `.inc` outputs it
	// wraps (a tablegen `*TableGen` target).
	utilityIDToName := map[string]string{}
	// artifactToName maps each codemodel target's artifact paths
	// (build-dir-relative, e.g. `bin/llvm-min-tblgen`) to the
	// target's name. Used by rewriteToolFromTarget to lift bare
	// artifact-path tool references in genrule cmds into
	// `$(location :<name>)` form plus a tools attr entry.
	artifactToName := map[string]string{}
	for _, tref := range cfg.Targets {
		if t, ok := r.Targets[tref.Id]; ok && t.Type == "UTILITY" {
			utilityIDs[tref.Id] = true
			utilityIDToName[tref.Id] = tref.Name
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

	// isTargetName is the set of every codemodel target name (real +
	// UTILITY). The codegen-header walk uses it to stop at sibling-target
	// boundaries when tracing a UTILITY tablegen target's ninja phony to
	// the `.inc` outputs it wraps — so it collects that target's own
	// generated headers without pulling a depended-on tool's or library's
	// outputs.
	isTargetName := make(map[string]bool, len(idToName)+len(utilityIDToName))
	for _, n := range idToName {
		isTargetName[n] = true
	}
	for _, n := range utilityIDToName {
		isTargetName[n] = true
	}
	// codegenConsumerDeps maps a cc_library target Name → its codemodel
	// dependencies, captured in the target loop and resolved to consumed
	// generated `.inc` headers after standalone-genrule recovery completes.
	codegenConsumerDeps := map[string][]fileapi.TargetDependency{}

	lc := targetLowerCtx{
		cmakeSrc:         cmakeSrc,
		cmakeBuild:       cmakeBuild,
		hostSrc:          hostSrc,
		hostPrefix:       opts.HostPrefixDir,
		hostSrcOnDisk:    hostSrcOnDisk,
		g:                g,
		cc:               cc,
		idToName:         idToName,
		utilityIDs:       utilityIDs,
		imports:          opts.Imports,
		tests:            opts.CTest,
		configureFiles:   configureFiles,
		fileGenerates:    fileGenerates,
		executeProcesses: executeProcesses,
		findPkgAttrib:    findPkgAttrib,
		workspaceRoot:    workspaceRoot,
		bazelPackagePath: opts.BazelPackagePath,
		generatedSources: generatedSources,
		rejections:       opts.Rejections,
	}

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
		irt, err := lowerTarget(&t, targetTrace{
			privateIncludeDirs:           privateIncludeDirs[tref.Name],
			traceLinkLibs:                traceLinkLibs[tref.Name],
			traceLinkScope:               traceLinkScope[tref.Name],
			platformConditionalSrcs:      platformConditionalSrcs[tref.Name],
			platformConditionalSrcsToAdd: platformConditionalSrcsToAdd[tref.Name],
		}, lc)
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

		// Record this cc_library's codemodel UTILITY dependencies so a pass
		// AFTER standalone-genrule recovery (which is what fills the genrule
		// output set the walk filters on) can resolve them to the generated
		// `.inc` headers it consumes. Only cc_library carries the wrapper
		// dep cleanly via the consumer's deps; cc_binary/cc_test are skipped.
		if irt.Kind == ir.KindCCLibrary && len(t.Dependencies) > 0 {
			codegenConsumerDeps[irt.Name] = t.Dependencies
		}
	}

	// Append recovered genrules then per-language sub-libraries
	// then cc_test rules in deterministic order; each slot is
	// appended in target-walk order during lowerTarget, which is
	// itself stable.
	pkg.Targets = append(pkg.Targets, cc.Genrules...)
	// Co-locate each per-language sub-library in its parent wrapper's
	// sub-package (set above at pkg.SubPackages[irt.Name]). The sub's srcs and
	// the wrapper that deps on it both live there; leaving the sub in the root
	// package makes the wrapper→sub edge cross-package against a private target
	// (LLVM's BLAKE3 _asm/_c splits: "not visible from").
	if pkg.SubPackages != nil {
		for _, sub := range cc.Subs {
			if parent, ok := cc.SubParent[sub.Name]; ok {
				if dir, ok := pkg.SubPackages[parent]; ok {
					pkg.SubPackages[sub.Name] = dir
				}
			}
		}
	}
	pkg.Targets = append(pkg.Targets, cc.Subs...)
	pkg.Targets = append(pkg.Targets, cc.Tests...)
	// Make every cc_test name a valid Bazel identifier before anything
	// else looks at it: CTest registers tests with hierarchical names
	// like `Suite::Case::Sub` (the Catch2 / GoogleTest convention) and the
	// add_test() → cc_test synthesis copies the TEST name verbatim, so the
	// `:` separators would otherwise hard-fail the whole convert in the
	// validate pass (issue #368). Runs before the collision pass so any
	// names the rewrite folds together get disambiguated.
	sanitizeTestNames(pkg)
	// Disambiguate cc_test names that collide with another emitted
	// target. An EXECUTABLE registered via add_test() becomes a
	// cc_test named after the TEST (reg.Name), which is usually the
	// binary's own name but need not be: a malformed (or copy-pasted)
	// add_test can register a test whose NAME equals a *different*
	// target's name. OpenBLAS does exactly this —
	// `add_test(openblas_utest_ext <openblas_utest binary>)` (utest/
	// CMakeLists.txt: the _ext test points at the wrong binary), so the
	// openblas_utest binary yields a cc_test named openblas_utest_ext
	// that collides with the real add_executable(openblas_utest_ext).
	// Bazel rejects duplicate names, so without this the whole convert
	// hard-fails on an upstream cmake quirk. Rename the colliding
	// cc_test (suffix `_test`, then numeric) — nothing references a
	// test target, so the rename is safe; the authoritative
	// codemodel-derived target keeps its name.
	disambiguateTestNameCollisions(pkg)
	// Fortran partition — move Fortran (.f / .f90 / ...) sources out of
	// cc_* targets (which can't compile them) into a sibling filegroup,
	// keeping the cc_* target buildable and the Fortran intent labeled +
	// operator-routable. See partitionFortranSources.
	partitionFortranSources(pkg)
	// CUDA retag — a cc_* target whose compiled srcs are entirely `.cu`
	// CUDA device code renders as the matching rules_cuda rule (cuda_library
	// / cuda_binary / cuda_test) so nvcc compiles it; a plain cc_library
	// can't (no `.cu` compile action). Runs after the per-language split so
	// it only catches WHOLE-target CUDA cases (the split already retagged
	// mixed-language CUDA sub-libs). See retagCudaTargets.
	retagCudaTargets(pkg)
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
	liftRawFeatureFlags(pkg, opts.BackedFeatures)
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
	// Per-source COMPILE_DEFINITIONS post-pass (Phase 1 slice 1c):
	// fold set_source_files_properties(... COMPILE_DEFINITIONS ...)
	// into the consuming target's defines when uniform across its
	// sources; tag (don't silently fold) when they diverge per-file,
	// since a single cc_library can't carry per-source defines.
	applyPerSourceCompileDefinitions(pkg, perSourceDefinesBySrc)
	// install(FILES)/install(DIRECTORY) → pkg_files() lowering
	// (Phase 1 slice 1b of the generator-parity uplift). Appended
	// last so the file-head targets stay grouped by family: cc rules
	// first, generated content next, then install-side packaging
	// rules (pkg_files for FILES/DIRECTORY, cmake_config_bundle
	// filegroup for declarative install(EXPORT)).
	pkg.Targets = append(pkg.Targets, lowerDirectoryInstallers(r, opts.EmitInstallExportConfig)...)
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
			lowerInterfaceLibraries(decodedTrace, knownTargets, hostSrc, cmakeSrc, workspaceRoot, genexTargets, opts.Imports, cc)...)
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
	// Prune dangling `:`-local deps from trace-synth INTERFACE libraries —
	// edges whose label points at no emitted target or alias. Runs AFTER
	// both lowerInterfaceLibraries and lowerAliasTargets so the
	// emitted-target/alias set is final (no forward-ref false positives).
	// See pruneDanglingTraceInterfaceDeps for the abseil case it fixes.
	pruneDanglingTraceInterfaceDeps(pkg)
	// Phase 4 standalone custom-command emission. Opt-in via
	// Options.EmitStandaloneCustomCommands; the dedup against
	// existing genrules keeps the recoverGenrule path's output
	// intact even when this fires.
	if opts.EmitStandaloneCustomCommands {
		var aliasLibs []shadow.AddLibraryCall
		if decodedTrace != nil {
			aliasLibs = decodedTrace.AddLibraries
		}
		var fileGlobs []shadow.FileGlobCall
		if decodedTrace != nil {
			fileGlobs = decodedTrace.FileGlobs
		}
		traceCtx := standaloneTraceContext{
			CustomCommands:  decodedAddCustomCommands,
			CustomTargets:   decodedAddCustomTargets,
			AddDependencies: decodedAddDependencies,
			AliasToActual:   buildAliasToActual(aliasLibs),
			FileGlobs:       fileGlobs,
		}
		// umbrellaPrefix is cmakeSrc-relative-to-labelRoot when the
		// workspace-root umbrella promoted labelRoot above cmakeSrc
		// (LLVM: hostSrc=llvm-project/, cmakeSrc=llvm-project/llvm/ →
		// "llvm"). Standalone genrules anchor their source-tree srcs
		// and cmd paths with it, consistent with the cc_library
		// re-anchor; empty in the common non-promoted case.
		umbrellaPrefix := ""
		if hostSrc != "" && hostSrc != cmakeSrc {
			if rel, inside := relativeIfInside(hostSrc, cmakeSrc); inside && rel != "" && rel != "." {
				umbrellaPrefix = rel
			}
		}
		stand := lowerStandaloneCustomCommands(g, pkg.Targets, cmakeSrc, cmakeBuild, umbrellaPrefix, opts.BazelPackagePath, artifactToName, traceCtx, cc.FilteredInternalCmds, cc)
		// Add the transitive `include "..."` closure of tablegen-shaped
		// codegen genrules to their srcs (their `.td` deps live only in
		// cmake's dynamic DEPFILE, not the static reply). hostSrc is the
		// labelRoot the genrules' anchored `-I` paths resolve against.
		recordCodegenIncludeClosure(stand, hostSrc, opts.BazelPackagePath)
		// Fold cmake file(GLOB)/file(GLOB_RECURSE)-sourced genrule inputs
		// back into build-time glob() filegroups (split-synthesized), so a
		// globbing genrule's deps re-evaluate in project B. No-op when the
		// project uses no file(GLOB) (e.g. tablegen under Ninja).
		threadFileGlobs(stand, traceCtx.FileGlobs, hostSrc)
		pkg.Targets = append(pkg.Targets, stand...)

		// Resolve each cc_library consumer's UTILITY (tablegen) dependencies
		// to the generated `.inc` headers it #includes, now that the
		// standalone genrules producing them have been recovered. The
		// combined output set — per-target codegen outputs (cc.OutToGenrule)
		// plus this standalone recovery — is what the ninja walk filters on;
		// matches land on pkg.CodegenHeaderConsumers for the split transform
		// to synthesize the wrapper library and wire the consumer's dep.
		if len(codegenConsumerDeps) > 0 {
			genOut := make(map[string]string, len(cc.OutToGenrule))
			for o, n := range cc.OutToGenrule {
				genOut[o] = n
			}
			for _, gt := range stand {
				for _, o := range gt.GenruleOuts {
					genOut[o] = gt.Name
				}
			}
			// Index ninja outputs by final path component so the codegen
			// walk can seed from a sub-directory custom target's prefixed
			// phony (cmake names it `<dir>/<target>`).
			phonyByName := map[string][]string{}
			if g != nil {
				if g.OutputIndex == nil {
					g.Index()
				}
				for o := range g.OutputIndex {
					base := o
					if i := strings.LastIndex(o, "/"); i >= 0 {
						base = o[i+1:]
					}
					phonyByName[base] = append(phonyByName[base], o)
				}
			}
			for name, deps := range codegenConsumerDeps {
				hdrs := collectCodegenHeaders(g, deps, utilityIDs, utilityIDToName, genOut, isTargetName, phonyByName)
				if len(hdrs) == 0 {
					continue
				}
				if pkg.CodegenHeaderConsumers == nil {
					pkg.CodegenHeaderConsumers = map[string][]string{}
				}
				pkg.CodegenHeaderConsumers[name] = hdrs
			}
		}
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
	// Breadcrumb for the cmake command edges the standalone-genrule pass
	// dropped (install / uninstall / regen / cpack / clean / dashboard /
	// ide-stub, create_symlink tool/SONAME/manpage aliases, and source-less
	// cmake -E copy edges whose source is outside the element). They have no
	// Bazel analogue
	// so dropping is correct, but an operator auditing a conversion should see
	// WHAT was filtered rather than the drop being silent — one aggregated
	// notice grouped by category (mirrors the MissingIncludeDirs breadcrumb
	// above).
	if len(cc.FilteredInternalCmds) > 0 && opts.Warnings != nil {
		byKind := map[string][]string{}
		for out, kind := range cc.FilteredInternalCmds {
			byKind[kind] = append(byKind[kind], out)
		}
		kinds := make([]string, 0, len(byKind))
		for k := range byKind {
			kinds = append(kinds, k)
		}
		sort.Strings(kinds)
		fmt.Fprintf(opts.Warnings,
			"lower: filtered %d cmake command edge(s) with no Bazel analogue (dropped, not converted):\n",
			len(cc.FilteredInternalCmds))
		for _, k := range kinds {
			outs := byKind[k]
			sort.Strings(outs)
			fmt.Fprintf(opts.Warnings, "  %s (%d): %s\n", k, len(outs), strings.Join(outs, ", "))
		}
	}
	// add_test registrations for which no cc_test was emitted — breadcrumb
	// so the drop isn't silent (mirrors the cmake-internal filter above). An
	// add_test becomes a cc_test only when its COMMAND is a converted
	// executable; entries whose COMMAND is a script runner (e.g. brotli's
	// `cmake -P` roundtrip/compatibility harness) — or whose executable the
	// converter didn't emit — have no Bazel test target, so they're left
	// unconverted. Surfacing them keeps "not all of this project's tests are
	// buildable" auditable instead of silent.
	//
	// The "emitted" set comes from the synthesized cc.Tests, which preserve
	// the original add_test NAME — NOT pkg.Targets, whose cc_test names have
	// by now been through sanitizeTestNames / disambiguateTestNameCollisions
	// and so would no longer match the registry's original names (false-
	// positiving renamed-but-converted tests as dropped).
	if opts.CTest != nil && opts.Warnings != nil {
		emitted := map[string]bool{}
		for _, t := range cc.Tests {
			emitted[t.Name] = true
		}
		byCmd := map[string][]string{}
		var cmds []string
		for _, tst := range opts.CTest.All() {
			if emitted[tst.Name] {
				continue
			}
			if _, seen := byCmd[tst.Target]; !seen {
				cmds = append(cmds, tst.Target)
			}
			byCmd[tst.Target] = append(byCmd[tst.Target], tst.Name)
		}
		if len(cmds) > 0 {
			total := 0
			for _, n := range byCmd {
				total += len(n)
			}
			sort.Strings(cmds)
			fmt.Fprintf(opts.Warnings,
				"lower: %d add_test registration(s) not converted to cc_test — no Bazel test target emitted (COMMAND is a script runner like cmake -P, or its executable wasn't converted):\n",
				total)
			for _, c := range cmds {
				names := byCmd[c]
				sort.Strings(names)
				shown, more := names, ""
				if len(shown) > 12 {
					more = fmt.Sprintf(", … +%d more", len(shown)-12)
					shown = shown[:12]
				}
				label := c
				if label == "" {
					label = "(unknown)"
				}
				fmt.Fprintf(opts.Warnings, "  COMMAND %s (%d): %s%s\n", label, len(names), strings.Join(shown, ", "), more)
			}
		}
	}
	// Surface install(SCRIPT) / install(CODE) directives. These run
	// cmake script code at install time and have no Bazel
	// analogue — the converter drops them silently. The warning
	// makes the omission auditable so operators who care about
	// install-time logic see what was lost.
	surfaceInstallScriptInstallers(r, opts.Warnings)

	// Surface target launchers (CROSSCOMPILING_EMULATOR /
	// TEST_LAUNCHER). Bazel has no per-target run-launcher; these
	// aren't routed automatically, so name them rather than drop
	// them silently. Empty across the survey corpus — fires only on
	// cross builds.
	surfaceLauncherTargets(r, opts.Warnings)

	// Route trace target_link_libraries arms to in-codebase trace-synth
	// INTERFACE libraries into deps. cmake bakes an INTERFACE lib's usage
	// requirements (include dirs, defines) into the consumer's compile
	// flags, so a STATIC/SHARED target that links one carries no codemodel
	// dependency edge to it — the edge survives only in the trace. Routing
	// it makes the consumer re-export the INTERFACE lib's headers/defines
	// to ITS consumers (the cmake intent). Runs after every target is in
	// pkg.Targets and before the lens-3 audit so routed edges aren't
	// reported as dropped.
	routeTraceInterfaceLibDeps(pkg, traceLinkLibs, traceLinkScope)

	// Lens-3 coverage audit: dependency-coverage over the final
	// package. Runs after every target (codemodel-derived + trace-
	// synthesized interface libs + aliases) is in pkg.Targets so each
	// target's dep buckets are final, and uses traceLinkLibs (the
	// recorded target_link_libraries arms) as the intent oracle. No-op
	// when no trace was decoded (traceLinkLibs empty) or no collector
	// was supplied.
	if opts.Coverage != nil {
		for _, f := range coverage.AuditLinkDeps(pkg, traceLinkLibs) {
			opts.Coverage.Add(f)
		}
	}

	// Drop include dirs that are unresolved generator expressions (any entry
	// containing "$<"): the genex evaluator can't reduce a shape like
	// `target_include_directories(t $<TARGET_PROPERTY:other,INCLUDE_DIRECTORIES>)`
	// to a path, so it survives as a literal include dir — useless as a
	// monolithic `includes = ["$<…>"]` and, in --split-packages, a header-lib
	// root whose synthesized name is an invalid Bazel identifier that aborts
	// the whole convert (glog's glog_test INTERFACE-library shape). Runs over
	// the fully-lowered package — after lowerInterfaceLibraries, which is
	// where the trace-synth interface libs (and their genex includes) land —
	// and before idiom-shaping/emit. The dirs are already surfaced via the
	// missing-include-dir notice; this just stops a genex reaching emit.
	dropGenexIncludeDirs(pkg)

	// Phase 7 idiom-shaping: lift header-only libraries' single include
	// directory from the broad `includes = ["<d>"]` form to the precise
	// `strip_include_prefix = "<d>"` form. Runs over the fully-lowered
	// package so it catches header-only libs from BOTH the codemodel path
	// (lowerTarget) and the trace-synth path (lowerInterfaceLibraries).
	shapeHeaderOnlyStripIncludePrefix(pkg)

	// cc_binary / cc_test have no `hdrs` (nor `textual_hdrs`) attribute, so
	// the emitter folds their Hdrs into `srcs`. A header whose extension
	// Bazel rejects in `srcs` (e.g. `.def`, `.gen` — not in rules_cc's
	// CC_HEADER set) then trips the rule's srcs-extension check at analysis
	// ("source file '…' is misplaced here"). Drop such headers from
	// executable targets so the fold is legal; they reach the target via a
	// dep's hdrs/textual_hdrs (the only place such a header can live), and
	// the drop is breadcrumbed so it isn't silent. Found by the survey build
	// lens on libxml2 (codegen/ranges.def in testrecurse's srcs).
	dropNonSrcsHeadersFromCcExecutables(pkg, opts.Warnings)

	// The "textually include a .cc to intercept its internals" idiom (fmt's
	// posix-mock-test: `#include "../src/os.cc"`): a cc_binary/cc_test source
	// quote-includes a .cc the target doesn't compile. Those rules have no
	// textual_hdrs slot, so synthesize a textual_hdrs cc_library carrying the
	// file and dep the target on it — declaring the input without compiling it
	// standalone. Runs last so the synthesized libs aren't reprocessed above.
	synthesizeTextualSourceIncludeLibs(pkg, hostSrc, hostSrcOnDisk, opts.Warnings)

	// The same idiom in GENERATED form: a convert-time-baked wrapper source
	// (configure_file / file(WRITE) → write_file) textually `#include`s a real
	// in-tree source by its source-root-ABSOLUTE path — OpenBLAS's
	// GenerateNamedObjects writes ~1951 such per-routine wrappers. Rewrite the
	// baked absolute include to a workspace-relative path and stage the kernel
	// as a textual_hdr on the compiling target. Runs after the on-disk pass
	// above (it can also feed textual_hdrs onto the same cc_library).
	stageGeneratedSourceRootIncludes(pkg, hostSrc, opts.BazelPackagePath, hostSrcOnDisk, opts.Warnings)

	return pkg, nil
}

func projectName(r *fileapi.Reply) string {
	if e := r.Cache.Get("CMAKE_PROJECT_NAME"); e != nil {
		return e.Value
	}
	return ""
}

// targetLowerCtx bundles the inputs to lowerTarget that don't vary
// across ToIR's per-target loop — the source/build roots, codemodel
// id maps, shared recovery results, and sinks. Built once before the
// loop so each call carries only its target plus that target's trace.
type targetLowerCtx struct {
	cmakeSrc         string
	cmakeBuild       string
	hostSrc          string
	hostPrefix       string
	hostSrcOnDisk    bool
	g                *ninja.Graph
	cc               *codegenContext
	idToName         map[string]string
	utilityIDs       map[string]bool
	imports          *manifest.Resolver
	tests            *ctest.Registry
	configureFiles   []configureFileOut
	fileGenerates    []fileGenerateOut
	executeProcesses []executeProcessOut
	findPkgAttrib    *findPackageAttrib
	workspaceRoot    string
	bazelPackagePath string
	generatedSources map[string]bool
	rejections       *rejection.Collector
}

// targetTrace bundles the per-target trace-derived inputs to
// lowerTarget, looked up by target name at the call site.
type targetTrace struct {
	privateIncludeDirs           map[string]bool
	traceLinkLibs                []string
	traceLinkScope               map[string]string
	platformConditionalSrcs      map[string]string
	platformConditionalSrcsToAdd map[string][]string
}

func lowerTarget(t *fileapi.Target, tt targetTrace, lc targetLowerCtx) (*ir.Target, error) {
	// Unpack the bundled inputs into the locals the body uses: lc is
	// invariant across the per-target loop, tt is this target's trace.
	cmakeSrc, cmakeBuild, hostSrc, hostPrefix := lc.cmakeSrc, lc.cmakeBuild, lc.hostSrc, lc.hostPrefix
	hostSrcOnDisk := lc.hostSrcOnDisk
	// umbrellaPrefix is the cmakeSrc-relative-to-labelRoot segment when the
	// workspace-root umbrella promoted labelRoot above cmakeSrc (LLVM:
	// labelRoot=llvm-project/, cmakeSrc=llvm-project/llvm/ → "llvm"). srcs
	// are already re-anchored to labelRoot in the source loop; hdrs and
	// source-tree includes must get the same prefix so a BUILD at labelRoot
	// resolves them consistently. Empty in the common (non-promoted) case —
	// where hostSrc == cmakeSrc — so behavior there is unchanged.
	umbrellaPrefix := ""
	if hostSrc != "" && hostSrc != cmakeSrc {
		if rel, inside := relativeIfInside(hostSrc, cmakeSrc); inside && rel != "" && rel != "." {
			umbrellaPrefix = rel
		}
	}
	reanchor := func(rel string) string {
		if umbrellaPrefix != "" && rel != "" {
			return filepath.Join(umbrellaPrefix, rel)
		}
		return rel
	}
	g, cc := lc.g, lc.cc
	idToName, utilityIDs := lc.idToName, lc.utilityIDs
	imports, tests := lc.imports, lc.tests
	configureFiles, fileGenerates, executeProcesses := lc.configureFiles, lc.fileGenerates, lc.executeProcesses
	findPkgAttrib, workspaceRoot := lc.findPkgAttrib, lc.workspaceRoot
	bazelPackagePath := lc.bazelPackagePath
	generatedSources, rejections := lc.generatedSources, lc.rejections
	privateIncludeDirs, traceLinkLibs, traceLinkScope := tt.privateIncludeDirs, tt.traceLinkLibs, tt.traceLinkScope
	platformConditionalSrcs, platformConditionalSrcsToAdd := tt.platformConditionalSrcs, tt.platformConditionalSrcsToAdd

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
	declaredGeneratedSrc := false
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
				// A cc_embed lift's generated .cxx #includes its sibling .h;
				// a target compiling the source needs the header as a
				// declared hdr (an -I path isn't enough — Bazel needs the
				// file as a declared input), which also lets any same-package
				// source that #includes it resolve.
				if hdr := cc.CcEmbedSourceToHeader[relOut]; hdr != "" {
					irt.Hdrs = append(irt.Hdrs, hdr)
				}
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
				irt.Hdrs = append(irt.Hdrs, reanchor(srcPath))
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
			if rel, inside := relativeIfInside(cmakeBuild, src.Path); inside {
				// Before eliding: a build-dir source that matches a
				// recovered generator output (configure_file /
				// file(GENERATE) / execute_process / custom-command)
				// is produced by a genrule the converter already
				// emitted onto cc.Genrules — wire that output edge
				// into srcs (or hdrs) instead of dropping it. This
				// captures the project's generated-compile-source
				// intent natively rather than emitting a broken
				// empty-srcs target. The canonical case is eigen's
				// doc-snippet `compile_<snippet>` cc_binaries:
				// configure_file splices a snippet's .cpp into a
				// template and the generated .cpp is the binary's
				// only source, so eliding it leaves srcs empty. The
				// build-dir-relative `rel` matches OutToGenrule's
				// keys (both relativize against the codemodel build
				// dir; see recoverConfigureFiles' recordedBuildDir).
				if _, produced := cc.OutToGenrule[rel]; produced {
					consumesCodegen = true
					ext := strings.ToLower(filepath.Ext(rel))
					if inCompileGroup[i] && !headerExts[ext] {
						irt.Srcs = append(irt.Srcs, rel)
						// A cc_embed lift's generated .cxx #includes its
						// sibling .h; a target compiling the source needs the
						// header as a declared hdr (an -I path isn't enough —
						// Bazel needs the file as a declared input). Add it so
						// this library — and any same-package source that
						// #includes the header — resolves it.
						if hdr := cc.CcEmbedSourceToHeader[rel]; hdr != "" {
							irt.Hdrs = append(irt.Hdrs, hdr)
						}
					} else if headerExts[ext] {
						irt.Hdrs = append(irt.Hdrs, rel)
					} else {
						irt.Srcs = append(irt.Srcs, rel)
					}
					continue
				}
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
				// GENERATED-marked sources (via
				// set_source_files_properties(... GENERATED TRUE))
				// are expected to be produced by a generator, not
				// present in the source tree — don't elide them as
				// "missing". Keep the source in srcs (it resolves to
				// the generator's output edge in Bazel) and tag the
				// target so the convert-time generated-input handling
				// is auditable. Phase 1 slice 1c. The codemodel's own
				// IsGenerated outputs are handled in the
				// src.IsGenerated branch above; this catches sources
				// the project marked GENERATED manually without the
				// codemodel flagging them.
				if generatedSources[src.Path] {
					declaredGeneratedSrc = true
					irt.Srcs = append(irt.Srcs, src.Path)
					continue
				}
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
	if declaredGeneratedSrc {
		// A source marked GENERATED via set_source_files_properties
		// that isn't on disk was kept (not elided) because a
		// generator is expected to produce it. Tag so operators can
		// audit which targets consume a manually-declared generated
		// input — and so they can wire the producing genrule /
		// configure_file edge if the converter didn't recover one.
		irt.Tags = append(irt.Tags, "cmake-declared-generated-source")
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
		// skip when copts already names a -std=… flag. Also skip
		// when this CG folds C and C++ sources together (issue
		// #315): a single language's -std would leak onto the
		// other's sources, which gcc/clang reject.
		if !compileGroupMixesCAndCXX(cg, t.Sources) {
			copts = prependLanguageStandardCopt(cg.Language, cg.LanguageStandard, copts)
		}
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
				// Under the umbrella promotion the source-tree includes are
				// re-anchored (e.g. "llvm/include"), so they no longer cover
				// the generated headers, which land at the build-dir-relative
				// path ("include/..."). Surface that build-dir include so
				// `-Iinclude` finds the generated headers' bazel-out tree.
				if umbrellaPrefix != "" && !seenInc[rel] {
					seenInc[rel] = true
					irt.Includes = append(irt.Includes, rel)
				}
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
			// Re-anchor source-tree includes to labelRoot under the
			// umbrella promotion (consistent with srcs/hdrs); no-op
			// otherwise. Dedup on the emitted value so a build-dir include
			// surfaced above (e.g. "include") and a re-anchored source
			// include (e.g. "llvm/include") don't collide.
			emit := reanchor(rel)
			if seenInc[emit] {
				continue
			}
			seenInc[emit] = true
			// target_include_directories(${CMAKE_CURRENT_SOURCE_DIR})
			// resolves to rel == "". Handle it BEFORE the private/system
			// split below. Bazel rejects `includes = [""]` ("resolves to
			// the workspace root, which would allow this rule and all of
			// its transitive dependents to include any file in your
			// workspace"); same-package consumers already see this target's
			// headers via hdrs+deps, so dropping the entry from `includes =`
			// is the idiomatic shape. The package root is still the
			// authoritative source for hdrs discovery — walkPkgRootForHdrs
			// makes discoverHeaders walk hostSrc (otherwise zlib-shape
			// projects that declare ONLY target_include_directories(.) end
			// up with empty hdrs) — and it sets RootInclude, which the split
			// emitter turns into include_prefix=<package dir> so the
			// target's own element-root-relative includes resolve.
			//
			// This MUST run before the privateIncludeDirs branch: a PRIVATE
			// root include otherwise fell into the copt branch and emitted a
			// bogus bare `-I` (reanchor("") == "") while leaving RootInclude
			// false. abseil targets whose root include is PRIVATE
			// (spinlock_wait, cctz — `#include
			// "absl/base/internal/spinlock_wait.h"`) then lost the
			// include_prefix that re-homes their headers, and their own
			// compile couldn't find them. The PRIVATE-ness (don't propagate
			// the -I to consumers) is moot once the headers carry the prefix.
			if rel == "" {
				walkPkgRootForHdrs = true
				continue
			}
			if privateIncludeDirs[inc.Path] {
				// Compile-only — don't propagate to consumers.
				// target_include_directories(... SYSTEM PRIVATE ...)
				// keeps its system flavour as -isystem so header
				// warnings stay suppressed the way cmake suppresses
				// them; plain PRIVATE stays -I. (PUBLIC includes ride
				// irt.Includes / cc_library.includes, which Bazel
				// already emits as -isystem + transitive, so the SYSTEM
				// keyword is faithful there without extra handling.)
				flag := "-I"
				if inc.IsSystem {
					flag = "-isystem"
				}
				irt.Copts = append(irt.Copts, flag+emit)
				continue
			}
			irt.Includes = append(irt.Includes, emit)
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
		hostingIncs := map[string]bool{}
		needsPkgRoot := false
		for _, cfgOut := range configureFiles {
			for inc := range targetBuildIncs {
				if isPathPrefix(inc, cfgOut.RelOutput) {
					addedHdrs = append(addedHdrs, cfgOut.RelOutput)
					hostingIncs[inc] = true
					// A configure_file output that lands in a SUBDIR under the
					// ROOT build-dir include ("") is consumed via that subdir
					// path, so the package-root genfiles dir must be searched.
					// libxml2: `configure_file(include/libxml/xmlversion.h.in
					// libxml/xmlversion.h)` → `<build>/libxml/xmlversion.h`,
					// #included as `<libxml/xmlversion.h>`. addBuildDirIncludes
					// skips the root ""/"." (Bazel rejects `includes=[""]`),
					// but the valid `includes=["."]` expresses exactly the
					// needed path — and ONLY at a non-root package: Bazel also
					// rejects `includes=["."]` at the workspace root ("'.'
					// resolves to the workspace root"), so it's added only when
					// the package path is NOT root (`!pkgPathIsRoot`).
					// Converting AT the
					// workspace root (no package path, or "."/"./" — e.g. the
					// fidelity harness) must NOT add it: the include can't be
					// expressed there and adding it hard-fails analysis (the
					// libpng regression). (A root-LEVEL output like `config.h`
					// is consumed via a relative `#include "config.h"` and
					// needs no path — gated out by the subdir check too.)
					if !pkgPathIsRoot(bazelPackagePath) && needsPkgRootInclude(inc, cfgOut.RelOutput) {
						needsPkgRoot = true
					}
					// A generate_export_header output is #included by BARE
					// name, so its OWN directory (cmake's
					// CMAKE_CURRENT_BINARY_DIR) must be on the include path —
					// the prefix match above can settle on a shallower parent
					// dir (the package), which leaves `#include
					// "<name>_export.h"` unresolved. Surface the output's dir
					// directly. Harmless for subdir-qualified consumers (an
					// extra search path), required for the bare ones.
					if cfgOut.ExportHeader {
						if d := filepath.ToSlash(filepath.Dir(cfgOut.RelOutput)); d != "." && d != "" {
							hostingIncs[d] = true
						}
					}
					break
				}
			}
		}
		if len(addedHdrs) > 0 {
			irt.Hdrs = append(irt.Hdrs, addedHdrs...)
			irt.Tags = append(irt.Tags, "has-cmake-codegen")
		}
		// The build-dir include that hosts a lifted configure_file
		// output is a real include path in Bazel — the genrule writes
		// the header under it (e.g. Catch2's genrule emits
		// generated-includes/catch2/catch_user_config.hpp, consumed as
		// `#include <catch2/catch_user_config.hpp>`). Surface it in
		// `includes` so the angle-bracket include resolves; without it
		// the header lands in hdrs but the compile can't find it. (Plain
		// build-dir includes that DON'T host a lifted output stay elided
		// — they'd point at the absent cmake build dir.)
		addBuildDirIncludes(irt, hostingIncs)
		// Package root (".") for a subdir output under the root build-dir
		// (see needsPkgRoot above); addBuildDirIncludes deliberately skips
		// "."/"" so it's added here, deduped.
		if needsPkgRoot {
			hasDot := false
			for _, e := range irt.Includes {
				if e == "." {
					hasDot = true
					break
				}
			}
			if !hasDot {
				irt.Includes = append(irt.Includes, ".")
			}
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
	// Carry the dropped root-include signal to the IR so the split emitter can
	// restore the prefix via include_prefix when this target re-homes into a
	// subpackage (the includes=[""] entry can't survive directly).
	irt.RootInclude = walkPkgRootForHdrs
	// Stage private "sibling" headers that live in the target's own source
	// directories. cmake implicitly searches a source file's own directory for
	// quote-includes (`#include "sibling.h"`), so a private header beside the
	// sources — brotli's c/common/platform.h, libxml2's libxml.h, fmt's
	// test/gtest-extra.h — is found at cmake build time with no -I. Under Bazel
	// the header must be a declared input or the quote-include misses in the
	// sandbox. These dirs feed the hdrs walk ONLY, never irt.Includes (the
	// emit-side include path): a sibling quote-include resolves relative to the
	// including file, so no -I is needed — just the staged hdr.
	seenWalk := map[string]bool{}
	for _, inc := range includesForWalk {
		seenWalk[inc] = true
	}
	// Gated on hostSrcOnDisk: without a real on-disk source root the per-dir
	// os.Stat below would probe the current working directory (filepath.Join
	// with an empty hostSrc collapses to the bare dir), so a reply-dir-only
	// offline run could accidentally stage headers from an unrelated CWD.
	if hostSrcOnDisk {
		for _, s := range irt.Srcs {
			dir := filepath.ToSlash(filepath.Dir(s))
			if dir == "." {
				dir = ""
			}
			if seenWalk[dir] {
				continue
			}
			// Skip dirs that aren't real source-tree directories. A GENERATED
			// source (CUSTOM_COMMAND output like "gen/foo.cc") carries a
			// build-dir path that doesn't exist under hostSrc at convert time;
			// feeding it to discoverHeaders would os.Stat(hostSrc/"gen") → miss
			// → record it in cc.MissingIncludeDirs and surface a misleading
			// "missing include dir" warning/rejection. Only real in-tree dirs
			// hold sibling headers worth staging.
			if st, statErr := os.Stat(filepath.Join(hostSrc, dir)); statErr != nil || !st.IsDir() {
				continue
			}
			seenWalk[dir] = true
			includesForWalk = append(includesForWalk, dir)
		}
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

	// Phase 7 idiom-shaping (slice 2): a compiled library that exports
	// public headers via FILE_SET HEADERS lifts that base dir from the
	// broad `includes = ["<d>"]` to the precise `strip_include_prefix`.
	// Keyed on FileSet metadata (compiled-lib includes are arbitrary -I
	// roots, so includes+hdrs alone can't tell a header-export dir from a
	// compile -I). Runs after Hdrs/Includes are finalized; header-only
	// libs are handled separately by shapeHeaderOnlyStripIncludePrefix.
	liftCompiledLibFileSetStripIncludePrefix(irt, t, cmakeSrc)

	// Refuse a srcs-less binary/test whose sources were all elided.
	// When every compiled source of a cc_binary / cc_test got
	// dropped by the elision branches above — a build-dir-only
	// generated file with no recovered generator edge
	// (cmake-elided-build-dir-source), a source missing on disk
	// (cmake-elided-missing-source), or a compiler artifact
	// (cmake-elided-compiler-artifact) — emitting the target anyway
	// produces a `cc_binary`/`cc_test` with `srcs = []` that Bazel
	// rejects at build time ("missing srcs"). That's a silently
	// broken BUILD, the worst outcome. The re-wire above already
	// captures the intent when a generator edge was recovered; this
	// is the honest fallback for when it wasn't (e.g. trace data
	// absent). Refuse cleanly so the lost intent is surfaced as a
	// typed rejection rather than shipped as an unbuildable rule.
	// Gated on an elision flag so the rare legitimate srcs-less
	// binary (one that links a dep providing main, with nothing
	// elided) is left untouched.
	if (irt.Kind == ir.KindCCBinary || irt.Kind == ir.KindCCTest) &&
		len(irt.Srcs) == 0 &&
		(elidedBuildDirSrc || elidedMissingSrc || elidedCompilerArtifact) {
		msg := fmt.Sprintf("target %q (%s) has no srcs after lowering — every compiled source was elided (build-dir-only generated file with no recovered generator edge, missing-on-disk source, or compiler artifact); emitting it would produce a Bazel-invalid srcs-less rule", t.Name, t.Type)
		if rejections != nil {
			rejections.AddWithContext(failure.AllSourcesElided, msg, t.Name, "")
			return nil, nil
		}
		return nil, failure.New(failure.AllSourcesElided, "%s", msg)
	}

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
	// Rule-kind gating: only kinds that emit as cc_library accept
	// `implementation_deps` in stock rules_cc — see
	// kindAllowsImplementationDeps. cc_binary / cc_test / cc_import
	// don't accept the attribute (bazel rejects it at analysis), and
	// for those leaf/prebuilt kinds the scope distinction is moot
	// (a binary has no consumers; a cc_import's link interface is
	// fixed at build time) — so fold PRIVATE deps into `irt.Deps`.
	allowsImplementationDeps := kindAllowsImplementationDeps(irt.Kind)
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
				// No manifest hit. Before falling back to a
				// tag-only elision, try the same lift the
				// attribution-MISSED path uses below: if
				// find_package resolved to a real SYSTEM library
				// (e.g. find_package(ZLIB) → /usr/lib/.../libz.so),
				// link it as `-l<name>` so the rule actually links
				// against it. cmake found the lib on the host; the
				// toolchain's library search path covers the standard
				// system locations, so `-lz` resolves the same way at
				// Bazel build time. Without this, a static-archive
				// build looks fine (undefined symbols are legal in a
				// .a) but every EXECUTABLE that pulls the compression
				// code (LLVM's opt/llc → zlib's compress2/crc32/…)
				// fails the final link. A producer element claiming
				// the lib name (exports.json) still wins over the host
				// -l<name>.
				if name := systemLibName(path); name != "" {
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
					continue
				}
				// Not a system lib (vendored / custom prefix); emit a
				// fallback tag so operators see which package's link is
				// unresolved. One tag per (pkg, path) pair — same
				// package can show up across multiple paths (release +
				// debug, main + dep libs).
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

	// OpenMP (issue #313): `-fopenmp` is both a compile AND a link flag —
	// gcc/clang need it at link time to pull in the OpenMP runtime
	// (libgomp / libomp), or consumers hit undefined references
	// (GOMP_parallel, __kmpc_fork_call, ...). cmake records it in the
	// compile group's flags (→ copts) but threads the link side through the
	// OpenMP::OpenMP_CXX IMPORTED target; when that import isn't resolved
	// (no manifest entry) the link flag is lost. Mirror it onto linkopts.
	propagateOpenMPLinkFlag(irt)

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

// propagateOpenMPLinkFlag mirrors a `-fopenmp` (or `-fopenmp=<rt>`) compile
// flag onto linkopts when it isn't already present. gcc/clang require it on
// the link line to pull in the OpenMP runtime (libgomp / libomp); cmake
// records it compile-side and threads the link side through the
// OpenMP::OpenMP_* IMPORTED target, so when that import isn't resolved the
// link flag would otherwise be dropped and consumers hit undefined
// references (GOMP_parallel, __kmpc_fork_call, ...). The exact flag is
// preserved so a clang `-fopenmp=libomp` links against the same runtime it
// compiled with. (Issue #313.) Called on both the wrapper target and each
// split sub-library (a split clears the wrapper's copts onto the subs).
func propagateOpenMPLinkFlag(t *ir.Target) {
	var flag string
	for _, c := range t.Copts {
		if c == "-fopenmp" || strings.HasPrefix(c, "-fopenmp=") {
			flag = c
			break
		}
	}
	if flag == "" {
		return
	}
	for _, l := range t.LinkOpts {
		if l == "-fopenmp" || strings.HasPrefix(l, "-fopenmp=") {
			return // already linked with OpenMP
		}
	}
	t.LinkOpts = append(t.LinkOpts, flag)
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
	// Track each emitted sub's language so a cross-language linkage pass below
	// can wire C/C++ → asm/fortran deps (the subStart marks where this call's
	// subs begin in cc.Subs).
	subStart := len(cc.Subs)
	langByName := map[string]string{}
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
		// already inlined a -std flag; skipped for a C+C++ mixed
		// CG so the flag doesn't leak across languages — #315).
		if !compileGroupMixesCAndCXX(cg, t.Sources) {
			copts = prependLanguageStandardCopt(cg.Language, cg.LanguageStandard, copts)
		}
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

		// A CUDA compile group's `.cu` sources can't compile in a cc_*
		// sub-library (no nvcc action in Bazel's cc rules), so the sub
		// renders as rules_cuda's cuda_library. Other languages keep the
		// wrapper's kind. The wrapper stays a deps-only cc_library/cc_binary
		// that depends on this sub, so the link is a normal cc→cuda dep
		// (rules_cuda's cuda_library provides CcInfo).
		subKind := irt.Kind
		if cg.Language == "CUDA" {
			subKind = ir.KindCudaLibrary
		}
		sub := ir.Target{
			Name:     subName,
			Kind:     subKind,
			Srcs:     subSrcs,
			Hdrs:     subHdrs,
			Includes: sharedIncludes,
			Copts:    copts,
			Defines:  defs,
			Tags:     subTags,
			// Split sub-libraries are INTERNAL object-libraries (alwayslink)
			// that exist only to be statically absorbed into the deps-only
			// wrapper — never linked standalone. Force linkstatic so Bazel
			// doesn't build a standalone .so for each: a C/C++ sub's .so can't
			// resolve the hidden-visibility symbols of a sibling asm/fortran sub
			// across a .so boundary (LLVM's BLAKE3 _c.so → the _asm subs'
			// llvm_blake3_hash_many_*). Static absorption keeps all the
			// objects in one linkage unit so those symbols resolve.
			Linkstatic: true,
			Alwayslink: irt.Alwayslink,
			Visibility: []string{"//visibility:private"},
		}
		// OpenMP (issue #313): a split sub-library carries the per-CG copts,
		// so `-fopenmp` lives here — not on the now-cleared wrapper. Mirror
		// it onto this sub's linkopts (the wrapper-level propagation in
		// lowerTarget is a no-op once the split clears irt.Copts).
		propagateOpenMPLinkFlag(&sub)
		cc.Subs = append(cc.Subs, sub)
		// Record the sub→parent link so the package-assignment pass co-locates
		// this sub in the parent's sub-package (its srcs + the wrapper that
		// deps on it live there). Without it the sub defaults to the root
		// package — a cross-package + private-visibility analysis error.
		if cc.SubParent != nil {
			cc.SubParent[sub.Name] = irt.Name
		}
		langByName[sub.Name] = cg.Language
		subDeps = append(subDeps, ":"+sub.Name)
	}

	// Cross-language linkage: a C/C++ sub typically CALLS into the same target's
	// asm/fortran subs (BLAKE3's blake3_dispatch.c → the per-arch
	// llvm_blake3_hash_many_* asm functions, compiled hidden-visibility). The
	// split makes them siblings under the wrapper, so when a C/C++ sub is linked
	// as a standalone .so (a dynamic consumer of the wrapper triggers it) its
	// hidden cross-language symbols are undefined. Make each C/C++ sub dep on
	// the same target's asm/fortran subs (one direction → no cycle): those subs
	// are alwayslink, so their objects fold into the C/C++ sub's linkage and the
	// symbols resolve. Asm/fortran calling back into C is rare and not wired.
	var otherLangSubs []string
	for i := subStart; i < len(cc.Subs); i++ {
		switch langByName[cc.Subs[i].Name] {
		case "ASM", "ASM_NASM", "Fortran":
			otherLangSubs = append(otherLangSubs, ":"+cc.Subs[i].Name)
		}
	}
	if len(otherLangSubs) > 0 {
		for i := subStart; i < len(cc.Subs); i++ {
			switch langByName[cc.Subs[i].Name] {
			case "C", "CXX", "OBJC", "OBJCXX":
				cc.Subs[i].Deps = appendUnique(cc.Subs[i].Deps, otherLangSubs...)
			}
		}
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
		// POSITION_INDEPENDENT_CODE = TRUE → features=["pic"].
		//
		// FALSE/unset emits NOTHING — deliberately. In cmake this property
		// only ever ADDS -fPIC (libs) / -fPIE (executables) when TRUE; it
		// never adds -fno-PIC/-fno-PIE for FALSE, so a FALSE/unset target
		// just inherits the compiler's default (PIE on modern toolchains).
		// Emitting features=["-pic"] there actively DISABLES pic, producing
		// non-PIC objects that then can't link into Bazel's PIE binaries /
		// shared libs (ld: "relocation R_X86_64_PC32 ... recompile with
		// -fPIC"). Letting Bazel's toolchain default govern matches cmake's
		// actual behavior and links cleanly. (Found via the brotli build
		// lens: its `brotli` tool leaves PIC unset, only the libs set it.)
		if v := strings.TrimSpace(p.Properties["POSITION_INDEPENDENT_CODE"]); v != "" && cmakeTruthy(v) {
			if !stringSliceContains(tgt.Features, "pic") {
				tgt.Features = append(tgt.Features, "pic")
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
		// ENABLE_EXPORTS (executables / shared libs that export
		// their symbols so dynamically-loaded plugins can resolve
		// against them). cmake implements this by adding the
		// platform's export-dynamic linker flag — `-rdynamic`
		// (a.k.a. `-Wl,--export-dynamic`) on GNU/Clang ld. That IS
		// a native Bazel concept: a linkopts entry. Emit it so the
		// converted binary actually exports its dynamic symbol
		// table, instead of only tagging the gap for the operator
		// to wire by hand. The tag is kept alongside for
		// auditability (so the bazel-idiom pass and operators can
		// still see the cmake-side intent), but the flag now
		// carries the real effect.
		//
		// Scope: GNU/Clang-style flag. The structural probe doesn't
		// record the target platform's linker family here, so we
		// emit the GNU/Clang spelling — correct for the Linux/macOS
		// (ld/lld) toolchains this converter targets; an MSVC-link
		// toolchain ignores `-rdynamic` (it's not the right spelling
		// there, but cmake's ENABLE_EXPORTS is also a no-op for the
		// MSVC import-lib model, so emitting nothing harmful).
		if cmakeTruthy(p.Properties["ENABLE_EXPORTS"]) {
			const exportDynamic = "-rdynamic"
			if !stringSliceContains(tgt.LinkOpts, exportDynamic) {
				tgt.LinkOpts = append(tgt.LinkOpts, exportDynamic)
			}
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

// sanitizeTestNames rewrites cc_test names that aren't valid Bazel
// identifiers into ones that are. CTest registers tests with
// hierarchical names like `Suite::Case::Sub` (the Catch2 / GoogleTest
// convention), and the add_test() → cc_test synthesis copies the TEST
// name verbatim; the `:` separators make it an invalid Bazel target
// name, which would hard-fail the whole convert in the bazelconstraints
// validate pass — even though the library targets the operator actually
// wants are fine (issue #368, surfaced by Catch2 v3.7.1's 142 add_test
// registrations). Renaming a cc_test is safe — nothing references a test
// target — so we sanitize in place and tag the rule. Only cc_tests are
// touched: a codemodel library/binary name can't contain `:` (cmake
// rejects it at declaration), and namespaced aliases are sanitized at
// alias synthesis. Any collisions the rewrite folds together are
// resolved by disambiguateTestNameCollisions, which runs next.
func sanitizeTestNames(pkg *ir.Package) {
	if pkg == nil {
		return
	}
	for i := range pkg.Targets {
		t := &pkg.Targets[i]
		if t.Kind != ir.KindCCTest {
			continue
		}
		san := sanitizeTestName(t.Name)
		if san == t.Name {
			continue
		}
		t.Name = san
		if !stringSliceContains(t.Tags, "cmake-test-name-sanitized") {
			t.Tags = append(t.Tags, "cmake-test-name-sanitized")
		}
	}
}

// sanitizeTestName maps an arbitrary CTest test name to the conservative
// Bazel identifier subset bazelconstraints.validNameRe enforces
// (`[a-zA-Z0-9_][a-zA-Z0-9_.+-]*`): every character that regex disallows
// becomes `_` (so `:` does; letters, digits, `_`, `.`, `+`, and `-` are
// kept), and a leading `.`/`+`/`-` — legal only after the first char — is
// also mapped to `_`. `Suite::Case::Sub` → `Suite__Case__Sub`.
// The empty string maps to a placeholder — validNameRe also rejects ""
// and ctest.Parse doesn't guard a malformed add_test() with an empty
// NAME — so the helper always returns a valid identifier; any resulting
// collisions are split by disambiguateTestNameCollisions.
func sanitizeTestName(name string) string {
	if name == "" {
		return "unnamed_test"
	}
	var b strings.Builder
	b.Grow(len(name))
	for i, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_':
			b.WriteRune(r)
		case (r == '.' || r == '+' || r == '-') && i > 0:
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	return b.String()
}

// kindAllowsImplementationDeps reports whether a target of this kind emits as
// a cc_library that accepts the implementation_deps attribute. Both
// ir.KindCCLibrary and ir.KindCCInterface render as cc_library (see
// emit.go's ccRuleLoads), so a PRIVATE link dep on either must route to
// implementation_deps to avoid re-exporting it. cc_binary / cc_test /
// cc_import don't accept the attribute (bazel rejects it at analysis), and for
// those leaf/prebuilt kinds the scope distinction is moot.
func kindAllowsImplementationDeps(k ir.Kind) bool {
	return k == ir.KindCCLibrary || k == ir.KindCCInterface
}

// routeTraceInterfaceLibDeps adds dependency edges that cmake records via
// target_link_libraries but the codemodel omits. cmake bakes an INTERFACE
// library's usage requirements (include dirs, defines) straight into a
// consuming STATIC/SHARED target's compile flags, so the codemodel carries
// no dependency edge to the INTERFACE lib (glm's
// `target_link_libraries(glm PUBLIC glm-header-only)`, glog's unit tests
// linking glog_test). The trace still records the arm. Without the edge the
// emitted consumer doesn't re-export the INTERFACE lib's headers/defines to
// ITS consumers — a lens-3 intent loss, and an external consumer of the
// INTERFACE lib gets nothing.
//
// Scope: only arms naming an in-codebase trace-synthesised INTERFACE library
// (the `cmake-codegen-interface-library-from-trace` targets) are routed —
// those are exactly the libs the codemodel legitimately omits, so we never
// fabricate a link edge that was dropped for some other reason. The arm's
// recorded visibility is honoured: a PRIVATE arm on a cc_library goes to
// implementation_deps (no re-export), while every other arm — PUBLIC/INTERFACE,
// or any arm on a binary/test that has no implementation_deps bucket — goes to
// deps. This mirrors the codemodel dep-routing convention elsewhere in lowering
// (allowsImplementationDeps + the traceLinkScope PRIVATE check).
func routeTraceInterfaceLibDeps(pkg *ir.Package, traceLinkLibs map[string][]string, traceLinkScope map[string]map[string]string) {
	if pkg == nil || len(traceLinkLibs) == 0 {
		return
	}
	ifaceLibs := map[string]bool{}
	for _, t := range pkg.Targets {
		for _, tag := range t.Tags {
			if tag == "cmake-codegen-interface-library-from-trace" {
				ifaceLibs[t.Name] = true
				break
			}
		}
	}
	if len(ifaceLibs) == 0 {
		return
	}
	for i := range pkg.Targets {
		t := &pkg.Targets[i]
		arms := traceLinkLibs[t.Name]
		if len(arms) == 0 {
			continue
		}
		have := map[string]bool{}
		for _, d := range t.Deps {
			have[d] = true
		}
		for _, d := range t.ImplementationDeps {
			have[d] = true
		}
		// Kinds that emit as cc_library (cc_library, cc_interface) have an
		// implementation_deps bucket; a PRIVATE arm there must not re-export.
		// Binaries/tests have no such bucket, so everything goes to deps.
		allowsImplementationDeps := kindAllowsImplementationDeps(t.Kind)
		scopes := traceLinkScope[t.Name]
		for _, lib := range arms {
			if lib == t.Name || !ifaceLibs[lib] {
				continue
			}
			label := ":" + lib
			if have[label] {
				continue
			}
			if allowsImplementationDeps && scopes != nil && scopeForLabelLib(scopes, lib) == "PRIVATE" {
				t.ImplementationDeps = append(t.ImplementationDeps, label)
			} else {
				t.Deps = append(t.Deps, label)
			}
			have[label] = true
		}
	}
}

// pruneDanglingTraceInterfaceDeps drops dependency edges on trace-synthesized
// INTERFACE libraries (`cmake-codegen-interface-library-from-trace`) that name
// a same-package target which was never emitted — a `:<name>` label with no
// matching rule or alias in the package. Such a label is guaranteed to fail
// Bazel analysis ("target '//pkg:<name>' does not exist").
//
// Why these arise: the trace records an INTERFACE library's
// target_link_libraries arms verbatim, but the codemodel can legitimately
// omit a *referenced* target. abseil's GTest-less default build is the canonical
// case: `absl_heterogeneous_lookup_testing` (a header-only INTERFACE lib that
// abseil declares WITHOUT the TESTONLY keyword, so it is always emitted) links
// `absl::test_instance_tracker`, which IS TESTONLY — so when testing is off the
// macro early-returns and never creates that target. The codemodel never sees
// it, lowerInterfaceLibraries sanitizes the dep to a local
// `:absl_test_instance_tracker`, and `bazel build //...` then dies on the
// dangling label even though every other one of abseil's 600+ targets builds.
// (The sibling find_package edge, GTest::gmock, is handled separately by
// lowerInterfaceLibraries' imports-manifest routing.)
//
// Pruning — rather than erroring — is the right call here: the INTERFACE lib is
// header-only, so without the unbuildable edge it still carries its headers and
// every real consumer builds; the dropped edge points at a target that, by
// construction, does not exist in this configuration. This is a graceful,
// intentional repair (a sibling of routeTraceInterfaceLibDeps and
// dropGenexIncludeDirs), NOT a Tier-1 refusal — the codemodel dep path records
// an UnresolvedLinkDep because it has no fallback and must abort, whereas this
// IS the fallback. So the drop is silent: recording a rejection would both
// misreport a handled case as a refusal and trip the survey's skip(rej)
// short-circuit, suppressing the very build-lens pass that proves the repair.
// Runs LAST (after both lowerInterfaceLibraries and lowerAliasTargets) so the
// emitted-name set is complete and no forward reference is misread as dangling.
// Only `:`-local labels are considered: external (`@repo//:t`) and absolute
// in-repo (`//pkg:t`) labels resolve elsewhere and are left untouched. Returns
// the number of edges dropped.
func pruneDanglingTraceInterfaceDeps(pkg *ir.Package) int {
	if pkg == nil {
		return 0
	}
	// Set of every emitted target/alias name in the package — the universe a
	// `:<name>` local label can legitimately resolve to.
	emitted := make(map[string]bool, len(pkg.Targets))
	for i := range pkg.Targets {
		emitted[pkg.Targets[i].Name] = true
	}
	// danglingLocal reports whether a dep label is a `:`-local reference to a
	// name with no emitted target/alias. A bare ":" (defensive) is not dangling.
	danglingLocal := func(label string) bool {
		name, ok := strings.CutPrefix(label, ":")
		if !ok || name == "" {
			return false
		}
		return !emitted[name]
	}
	dropped := 0
	for i := range pkg.Targets {
		t := &pkg.Targets[i]
		if !stringSliceContains(t.Tags, "cmake-codegen-interface-library-from-trace") {
			continue
		}
		pruneBucket := func(deps []string) []string {
			kept := deps[:0]
			for _, d := range deps {
				if danglingLocal(d) {
					dropped++
					continue
				}
				kept = append(kept, d)
			}
			return kept
		}
		t.Deps = pruneBucket(t.Deps)
		t.ImplementationDeps = pruneBucket(t.ImplementationDeps)
	}
	return dropped
}

// dropGenexIncludeDirs removes include directories that are unresolved
// generator expressions (any entry containing "$<") from every target's
// Includes. cmake records shapes like
// `target_include_directories(glog_test INTERFACE $<TARGET_PROPERTY:glog,INCLUDE_DIRECTORIES>)`,
// and the converter's genex evaluator can't reduce a TARGET_PROPERTY genex
// to a concrete path, so it survives as a literal include dir (here on the
// trace-synthesized glog_test INTERFACE library). Such an entry is never a
// real directory: in the monolithic emit it renders as a useless
// `includes = ["$<…>"]`, and in --split-packages it becomes a header-library
// root whose synthesized name ("$<…>_headers") fails Bazel's identifier
// rule, aborting the whole convert. The dirs are already surfaced via the
// missing-include-dir notice (they don't exist on disk); dropping them here
// keeps a genex from reaching emit. Forward-declared *real* include paths
// (no "$<") are left untouched — cmake legitimately permits those (the LLVM
// llvm-mca shape). Returns the number dropped.
func dropGenexIncludeDirs(pkg *ir.Package) int {
	if pkg == nil {
		return 0
	}
	dropped := 0
	for i := range pkg.Targets {
		t := &pkg.Targets[i]
		if len(t.Includes) == 0 {
			continue
		}
		kept := make([]string, 0, len(t.Includes))
		for _, inc := range t.Includes {
			if strings.Contains(inc, "$<") {
				dropped++
				continue
			}
			kept = append(kept, inc)
		}
		if len(kept) == len(t.Includes) {
			continue
		}
		if len(kept) == 0 {
			t.Includes = nil
		} else {
			t.Includes = kept
		}
	}
	return dropped
}

// ccSrcsAllowedExts is the lower-cased extension set rules_cc accepts in a
// cc rule's `srcs` — its ALLOWED_SRC_FILES (CC_SOURCE ∪ C_SOURCE ∪ CC_HEADER
// ∪ ASSEMBLER{,_WITH_C_PREPROCESSOR} ∪ ARCHIVE ∪ PIC_ARCHIVE). Lower-cased
// because filepath.Ext is case-sensitive but `.C`→`.c` / `.H`→`.h` are both
// valid; `.pic.a`'s Ext is `.a`, already covered. NOT the broader cc_library
// `hdrs` set — `hdrs` additionally accepts header-like artifacts srcs
// rejects (e.g. `.def` for LLVM's x-macro idiom), which is exactly why the
// fold below is gated to executable kinds.
var ccSrcsAllowedExts = map[string]bool{
	// sources
	".cc": true, ".cpp": true, ".cxx": true, ".c++": true, ".c": true,
	".cu": true, ".cl": true,
	// headers
	".h": true, ".hh": true, ".hpp": true, ".ipp": true, ".hxx": true,
	".h++": true, ".inc": true, ".inl": true, ".tlh": true, ".tli": true,
	".tcc": true,
	// assembler + archives
	".s": true, ".asm": true, ".a": true, ".lib": true,
}

// dropNonSrcsHeadersFromCcExecutables removes from each cc_binary / cc_test
// any Hdrs entry whose extension Bazel rejects in `srcs`. Those rules have
// no `hdrs` (nor `textual_hdrs`) attribute, so the emitter folds their Hdrs
// into `srcs`; an entry whose extension isn't in rules_cc's ALLOWED_SRC_FILES
// then hard-fails the rule's srcs-extension check at analysis — "source file
// '…' is misplaced here" (a header ext srcs rejects, e.g. `.def`/`.gen`) or
// "'…' does not produce any cc_binary srcs files" (a non-code artifact, e.g.
// a generated `.pc` pkg-config file or an extension-less config script).
// Such a file can only reach an executable target via a dep's hdrs/
// textual_hdrs (a cc_library can hold it), so dropping it here is the only
// legal shape — cc_library / cc_interface keep theirs (their `hdrs` accepts
// the wider set). The drop is breadcrumbed (no silent drops) so an operator
// knows to ensure a providing dep where one isn't already present.
func dropNonSrcsHeadersFromCcExecutables(pkg *ir.Package, warn io.Writer) {
	if pkg == nil {
		return
	}
	type dropRec struct {
		target string
		hdrs   []string
	}
	var recs []dropRec
	total := 0
	for i := range pkg.Targets {
		t := &pkg.Targets[i]
		if t.Kind != ir.KindCCBinary && t.Kind != ir.KindCCTest {
			continue
		}
		if len(t.Hdrs) == 0 {
			continue
		}
		kept := make([]string, 0, len(t.Hdrs))
		var dropped []string
		for _, h := range t.Hdrs {
			if ccSrcsAllowedExts[strings.ToLower(filepath.Ext(h))] {
				kept = append(kept, h)
				continue
			}
			dropped = append(dropped, h)
		}
		if len(dropped) == 0 {
			continue
		}
		if len(kept) == 0 {
			t.Hdrs = nil
		} else {
			t.Hdrs = kept
		}
		sort.Strings(dropped)
		recs = append(recs, dropRec{target: t.Name, hdrs: dropped})
		total += len(dropped)
	}
	if total == 0 || warn == nil {
		return
	}
	sort.Slice(recs, func(i, j int) bool { return recs[i].target < recs[j].target })
	fmt.Fprintf(warn,
		"lower: dropped %d non-srcs file(s) from %d cc_binary/cc_test target(s) — those rules have no hdrs/textual_hdrs slot, so a file whose extension Bazel rejects in srcs (e.g. .def/.gen headers, .pc pkg-config, config scripts) must reach the target via a dep's hdrs:\n",
		total, len(recs))
	for _, r := range recs {
		fmt.Fprintf(warn, "  %s (%d): %s\n", r.target, len(r.hdrs), strings.Join(r.hdrs, ", "))
	}
}

// disambiguateTestNameCollisions renames any cc_test whose name
// collides with an earlier-emitted target. cc_tests synthesized from
// add_test() take the TEST name, which a malformed add_test can set
// to a different target's name (see the call site for OpenBLAS's
// openblas_utest_ext case). Bazel rejects duplicate target names, so
// the collision would hard-fail the convert. Renaming the cc_test is
// safe — no rule references a test target — so the authoritative
// (codemodel-derived, first-seen) target keeps its name and the
// cc_test gets a deterministic unique suffix. No-op when there are no
// collisions.
func disambiguateTestNameCollisions(pkg *ir.Package) {
	if pkg == nil {
		return
	}
	seen := make(map[string]bool, len(pkg.Targets))
	for i := range pkg.Targets {
		t := &pkg.Targets[i]
		name := t.Name
		if !seen[name] {
			seen[name] = true
			continue
		}
		// Collision. Only rename cc_tests — renaming a library/binary
		// could break a label reference, and a non-test duplicate is a
		// real bug we still want to surface via the validate pass.
		if t.Kind != ir.KindCCTest {
			seen[name] = true
			continue
		}
		candidate := name + "_test"
		for n := 2; seen[candidate]; n++ {
			candidate = fmt.Sprintf("%s_test%d", name, n)
		}
		if !stringSliceContains(t.Tags, "cmake-test-name-disambiguated") {
			t.Tags = append(t.Tags, "cmake-test-name-disambiguated")
		}
		t.Name = candidate
		seen[candidate] = true
	}
}

// fortranSrcExts are the source extensions Bazel's cc_* rules can't
// compile (cmake drives a per-source Fortran compiler that Bazel cc
// rules have no equivalent for). Mirrors the bazelidiom audit's list;
// kept local so lower has no dependency on the audit package.
var fortranSrcExts = map[string]bool{
	".f": true, ".f90": true, ".f95": true, ".f03": true, ".f08": true,
	".for": true, ".ftn": true, ".fpp": true,
}

func isFortranSrc(p string) bool {
	if dot := strings.LastIndex(p, "."); dot >= 0 {
		return fortranSrcExts[strings.ToLower(p[dot:])]
	}
	return false
}

// partitionFortranSources moves Fortran sources OUT of each cc_*
// target's srcs into a sibling `filegroup` named `<target>_fortran_srcs`.
//
// Why: cmake routes a target's Fortran (.f / .f90 / ...) sources into
// the same add_library/add_executable as its C/C++ sources, but Bazel's
// cc rules dispatch by file extension and have NO Fortran compile action
// — so a cc_library carrying `.f` srcs is unbuildable as emitted (the
// non-cc-language-source idiom). There's no canonical Bazel Fortran
// ruleset (rules_fortran is experimental + not in the BCR) and no
// gazelle Fortran extension to hand off to, so the converter can't emit
// a buildable Fortran rule. The honest, plain-Bazel increment:
//
//   - pull the Fortran sources out so the cc_* target keeps only its
//     C/C++/ASM sources and BUILDS;
//   - park them in a `filegroup` (global Bazel namespace, no MODULE
//     deps) that's clearly labeled, so the intent is preserved and an
//     operator can point a Fortran ruleset (rules_fortran / foreign_cc /
//     hand-rolled) at `:<target>_fortran_srcs` when they wire one;
//   - tag both the cc_* target and the filegroup
//     `cmake-codegen-fortran-target` so the gap is grep-able and the
//     bazelidiom audit's non-cc-language-source finding has a concrete
//     home (the filegroup), not a broken cc_library.
//
// A cc_* target whose srcs become EMPTY after partitioning (a
// Fortran-only library, e.g. OpenBLAS's reference-LAPACK targets) is
// dropped — an srcs-less cc_library/cc_binary is Bazel-invalid, and the
// filegroup carries the real content. The target's deps still resolve
// via the filegroup label if needed; the all-sources-elided refusal
// path (lowerTarget) doesn't fire here because partitioning runs as a
// post-pass after emit-time target assembly.
func partitionFortranSources(pkg *ir.Package) {
	if pkg == nil {
		return
	}
	var added []ir.Target
	kept := pkg.Targets[:0]
	for i := range pkg.Targets {
		t := pkg.Targets[i]
		isCC := t.Kind == ir.KindCCLibrary || t.Kind == ir.KindCCBinary ||
			t.Kind == ir.KindCCInterface || t.Kind == ir.KindCCTest
		if !isCC || len(t.Srcs) == 0 {
			kept = append(kept, t)
			continue
		}
		var ftn, rest []string
		for _, s := range t.Srcs {
			if isFortranSrc(s) {
				ftn = append(ftn, s)
			} else {
				rest = append(rest, s)
			}
		}
		if len(ftn) == 0 {
			kept = append(kept, t)
			continue
		}
		// Build the sibling filegroup carrying the Fortran sources.
		fg := ir.Target{
			Name:       t.Name + "_fortran_srcs",
			Kind:       ir.KindFilegroup,
			Srcs:       ftn,
			Visibility: []string{"//visibility:public"},
			Tags:       []string{"cmake-codegen-fortran-target"},
		}
		added = append(added, fg)

		if len(rest) == 0 {
			// Fortran-only target: the cc_* rule would be srcs-less
			// (Bazel-invalid). Drop it; the filegroup holds the content.
			continue
		}
		t.Srcs = rest
		if !stringSliceContains(t.Tags, "cmake-codegen-fortran-target") {
			t.Tags = append(t.Tags, "cmake-codegen-fortran-target")
		}
		kept = append(kept, t)
	}
	pkg.Targets = append(kept, added...)
}

// isCudaSrc reports whether p is a CUDA device-code source (`.cu`). `.cuh`
// is a CUDA header, not a compiled TU, so it is NOT treated as a source here
// (it rides in hdrs like any header).
func isCudaSrc(p string) bool {
	if dot := strings.LastIndex(p, "."); dot >= 0 {
		return strings.ToLower(p[dot:]) == ".cu"
	}
	return false
}

// retagCudaTargets retags a cc_* target whose COMPILED sources are entirely
// `.cu` CUDA device code as the matching rules_cuda rule (cuda_library /
// cuda_binary / cuda_test), so it renders with nvcc-driving rules instead of
// a cc_library that Bazel can't compile (`.cu` has no cc compile action — the
// same gap partitionFortranSources handles for Fortran, but unlike Fortran,
// CUDA HAS a canonical Bazel ruleset (rules_cuda in the BCR), so the honest
// lowering is a real buildable rule, not a parked filegroup).
//
// Runs AFTER splitCompileGroups' per-language split (which already tags a CUDA
// sub-library KindCudaLibrary and leaves the wrapper deps-only): this pass
// catches the WHOLE-target case — a library/binary/test whose only compile
// group is CUDA, so it never split (cuda-samples' `.cu` executables, cutlass's
// `.cu`-only object libs). A target that MIXES `.cu` with C/C++ compiled srcs
// is left as cc_* (it should already have been language-split; retagging a
// genuinely-mixed leftover to cuda_* would drop its C/C++ TUs from the cc
// compile path — safer to leave it and let the bazelidiom audit flag it).
// Headers (.h/.hpp/.cuh/…) don't count as compiled srcs for this test, so a
// cuda_library keeps its non-.cu headers in srcs/hdrs as usual.
func retagCudaTargets(pkg *ir.Package) {
	if pkg == nil {
		return
	}
	for i := range pkg.Targets {
		t := &pkg.Targets[i]
		var cudaKind ir.Kind
		switch t.Kind {
		case ir.KindCCLibrary, ir.KindCCInterface:
			cudaKind = ir.KindCudaLibrary
		case ir.KindCCBinary:
			cudaKind = ir.KindCudaBinary
		case ir.KindCCTest:
			cudaKind = ir.KindCudaTest
		default:
			continue
		}
		// Inspect compiled sources only (skip headers): the target is a CUDA
		// target iff it has at least one `.cu` and every compiled src is `.cu`.
		sawCuda := false
		sawNonCudaCompiled := false
		for _, s := range t.Srcs {
			ext := ""
			if dot := strings.LastIndex(s, "."); dot >= 0 {
				ext = strings.ToLower(s[dot:])
			}
			if headerExts[ext] || ext == ".cuh" {
				continue // a header in srcs (incl. CUDA `.cuh`) — not a compiled TU
			}
			if isCudaSrc(s) {
				sawCuda = true
			} else {
				sawNonCudaCompiled = true
			}
		}
		if sawCuda && !sawNonCudaCompiled {
			t.Kind = cudaKind
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

// collectGeneratedSources walks the decoded
// set_source_files_properties calls and returns the set of source
// paths declared with GENERATED=TRUE (cmake's truthy convention).
// The codemodel already flags add_custom_command / configure_file
// outputs as IsGenerated; this captures sources a project marked
// GENERATED manually so the missing-source elision in lowerTarget
// doesn't drop them as absent — they resolve to a generator's output
// edge in Bazel rather than to a file in the source tree.
//
// Phase 1 slice 1c. Returns nil when no GENERATED property surfaces.
func collectGeneratedSources(calls []shadow.SourceFilePropertiesCall) map[string]bool {
	var out map[string]bool
	for _, call := range calls {
		for _, prop := range call.Properties {
			if !strings.EqualFold(prop.Name, "GENERATED") {
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

// collectPerSourceCompileDefinitions walks the decoded
// set_source_files_properties calls and returns a map from source
// path → list of COMPILE_DEFINITIONS declared for that source. cmake
// stores the values as a semicolon-separated list (e.g.
// "FOO=1;BAR"); we split on ";" and drop empties, preserving the
// declared order within a single property and across multiple
// declarations for the same source (additive, last-listed last).
//
// Phase 1 slice 1c. Returns nil when no COMPILE_DEFINITIONS surface.
func collectPerSourceCompileDefinitions(calls []shadow.SourceFilePropertiesCall) map[string][]string {
	var out map[string][]string
	for _, call := range calls {
		for _, prop := range call.Properties {
			if !strings.EqualFold(prop.Name, "COMPILE_DEFINITIONS") {
				continue
			}
			if prop.Value == "" {
				continue
			}
			defs := strings.Split(prop.Value, ";")
			for _, f := range call.Files {
				for _, d := range defs {
					if d == "" {
						continue
					}
					if out == nil {
						out = map[string][]string{}
					}
					out[f] = append(out[f], d)
				}
			}
		}
	}
	return out
}

// applyPerSourceCompileDefinitions folds per-source COMPILE_DEFINITIONS
// (from set_source_files_properties(... COMPILE_DEFINITIONS ...)) into
// the consuming target's defines where Bazel can express it. Bazel's
// cc_library defines/copts are per-TARGET, not per-source; cmake's
// per-source COMPILE_DEFINITIONS only maps cleanly when every
// source-define in a target agrees:
//
//   - If the set of defines is identical across every one of the
//     target's sources that carries the property (and any source
//     that doesn't carry it is treated as carrying the empty set),
//     fold the (uniform) defines into the target's Defines. The
//     common single-source case (one .c with COMPILE_DEFINITIONS)
//     falls here trivially.
//   - If the defines genuinely DIFFER between sources within one
//     target, a single cc_library can't represent that — Bazel would
//     apply every define to every source. We do NOT silently fold
//     (that would over-define the sources that didn't ask for it);
//     instead we tag the target `cmake-per-source-compile-definitions-divergent`
//     so the gap is auditable, and leave Defines untouched. The
//     operator's fix is splitting the divergent sources into separate
//     cc_library targets (the same remedy the per-source LANGUAGE and
//     per-source-defines compile-group split already use). Documented
//     in ROADMAP.md.
//
// No-op when byPath is empty.
func applyPerSourceCompileDefinitions(pkg *ir.Package, byPath map[string][]string) {
	if len(byPath) == 0 || pkg == nil {
		return
	}
	for i := range pkg.Targets {
		tgt := &pkg.Targets[i]
		if tgt.Kind != ir.KindCCLibrary && tgt.Kind != ir.KindCCBinary &&
			tgt.Kind != ir.KindCCInterface && tgt.Kind != ir.KindCCTest {
			continue
		}
		// Gather the per-source define sets for sources this target
		// actually compiles. A source with no entry contributes the
		// empty set — which still participates in the uniformity test
		// (a target with one defined source and one undefined source
		// is divergent: the define would leak to the undefined one).
		var defined [][]string // define-sets for sources that carry the property
		anyUndefined := false
		for _, src := range tgt.Srcs {
			if defs, ok := byPath[src]; ok {
				defined = append(defined, defs)
			} else {
				anyUndefined = true
			}
		}
		if len(defined) == 0 {
			continue
		}
		// Uniformity check: every defined source must carry the SAME
		// set, and no source may be undefined (an undefined source is
		// a different — empty — set).
		uniform := !anyUndefined
		if uniform {
			first := sortedDedupStrings(defined[0])
			for _, d := range defined[1:] {
				if !sameDefineSet(first, sortedDedupStrings(d)) {
					uniform = false
					break
				}
			}
		}
		if !uniform {
			tag := "cmake-per-source-compile-definitions-divergent"
			if !stringSliceContains(tgt.Tags, tag) {
				tgt.Tags = append(tgt.Tags, tag)
			}
			continue
		}
		// Uniform: fold the defines into the target's Defines,
		// deduping against any already present (codemodel-derived).
		for _, d := range defined[0] {
			if !stringSliceContains(tgt.Defines, d) {
				tgt.Defines = append(tgt.Defines, d)
			}
		}
	}
}

// sortedDedupStrings returns a sorted, deduped copy of in. Small
// helper for the COMPILE_DEFINITIONS uniformity comparison.
func sortedDedupStrings(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	cp := append([]string(nil), in...)
	sort.Strings(cp)
	out := cp[:1]
	for _, x := range cp[1:] {
		if x != out[len(out)-1] {
			out = append(out, x)
		}
	}
	return out
}

// sameDefineSet reports whether the two already-sorted-and-deduped
// define lists are identical — the uniformity test for per-source
// COMPILE_DEFINITIONS folding.
func sameDefineSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
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

// compileGroupMixesCAndCXX reports whether a single CompileGroup's compiled
// sources include BOTH a C source and a C++ source (by file extension) —
// the case cmake sometimes produces by folding a mixed-language target into
// one CompileGroup with a single Language. Prepending that group's
// language-standard `-std=` flag then leaks it onto the other language's
// sources (gcc rejects `-std=c11` on a .cpp and `-std=c++17` on a .c), so
// prependLanguageStandardCopt is skipped when this is true. (Issue #315.)
// Headers and non-C/C++ extensions are language-neutral and don't count.
func compileGroupMixesCAndCXX(cg fileapi.CompileGroup, srcs []fileapi.TargetSource) bool {
	hasC, hasCXX := false, false
	for _, idx := range cg.SourceIndexes {
		if idx < 0 || idx >= len(srcs) {
			continue
		}
		switch sourceCLanguage(srcs[idx].Path) {
		case "C":
			hasC = true
		case "CXX":
			hasCXX = true
		}
	}
	return hasC && hasCXX
}

// sourceCLanguage classifies a source path as "C", "CXX", or "" (neutral —
// headers and non-C/C++ languages) by extension, for the mixed-language
// `-std` guard. Extension case matters for one pair: a lowercase `.c` is C,
// but an UPPERCASE `.C` is C++ (gcc/cmake convention) — so the `.c` check is
// case-SENSITIVE and `.C` falls through to the C++ set. The remaining C++
// extensions are matched case-insensitively.
func sourceCLanguage(path string) string {
	ext := filepath.Ext(path)
	if ext == ".c" {
		return "C" // lowercase .c only
	}
	switch strings.ToLower(ext) {
	case ".c": // reached only by uppercase ".C" → C++ by convention
		return "CXX"
	case ".cc", ".cpp", ".cxx", ".c++", ".cp", ".cppm", ".ixx":
		return "CXX"
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

// needsPkgRootInclude reports whether a configure_file output at relOutput,
// hosted by the build-dir include `inc`, requires the package root (".") on
// the consuming target's include path. True only when the output lives in a
// SUBDIR under the ROOT ("") build-dir include — it's then consumed via that
// subdir path (e.g. libxml2's `<build>/libxml/xmlversion.h`, #included as
// `<libxml/xmlversion.h>`), so the package-root genfiles dir must be on -I.
// addBuildDirIncludes skips the root ""/"." (Bazel rejects `includes=[""]`),
// but the valid `includes=["."]` expresses exactly this — and it resolves to
// a real sub-dir ONLY under a non-root package (project-B's
// `elements/<name>/`); at the workspace root Bazel rejects it, so the caller
// also gates on pkgPathIsRoot. A root-LEVEL output (no subdir) is consumed
// via a relative `#include "x.h"` and needs no -I, so it returns false.
func needsPkgRootInclude(inc, relOutput string) bool {
	return (inc == "" || inc == ".") && strings.Contains(relOutput, "/")
}

// pkgPathIsRoot reports whether a --bazel-package-path value denotes the
// workspace root ("", ".", "./", " . "), where Bazel rejects an
// `includes = ["."]` entry ("'.' resolves to the workspace root"). The
// configure_file package-root include (needsPkgRootInclude) is suppressed
// for such root conversions — it can't be expressed there anyway.
func pkgPathIsRoot(p string) bool {
	p = strings.Trim(strings.TrimSpace(p), "/")
	return p == "" || p == "."
}

// addBuildDirIncludes appends build-dir-relative include dirs to
// irt.Includes (sorted, deduped against existing entries). Used by the
// codegen-consumer attribution blocks when a lifted output
// (configure_file / file(GENERATE) / execute_process) lands a header under
// a build-dir include the codemodel recorded but lowerTarget otherwise
// elides: once the genrule writes the header there, the dir is a real
// Bazel include path the angle-bracket `#include <…>` needs.
func addBuildDirIncludes(irt *ir.Target, dirs map[string]bool) {
	if len(dirs) == 0 {
		return
	}
	existing := map[string]bool{}
	for _, inc := range irt.Includes {
		existing[inc] = true
	}
	var add []string
	for d := range dirs {
		// Skip the package/workspace root ("" or "."): Bazel rejects a
		// root `includes` entry ("resolves to the workspace root, which
		// would allow this rule … to include any file in your workspace"
		// — the #253 fix), and a configured header that lands at the
		// package root is consumed via a relative `#include "x.h"` that
		// needs no include path anyway. Only real subdirs (e.g. Catch2's
		// `generated-includes`) need surfacing.
		if d == "" || d == "." || existing[d] {
			continue
		}
		add = append(add, d)
	}
	sort.Strings(add)
	irt.Includes = append(irt.Includes, add...)
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
//
// bazelPackagePath is the repo-root-relative landing package of the
// element (e.g. "elements/llvm"); when non-empty, source-tree paths
// re-anchor to their EXEC-ROOT form (`<bazelPackagePath>/<umbrella>/<rel>`)
// rather than the bare labelRoot-relative form. A genrule cmd runs at the
// Bazel exec root, so a source input or `-I` root referenced by a bare
// relative path (`include/...`) would resolve against the exec root, not
// the element's package — wrong for any element that lands under a
// sub-package. The exec-root anchor is package-location-independent: it
// stays correct no matter which sub-package split moves the genrule into
// (split only re-relativizes the `$(RULEDIR)/<out>` build-output tokens,
// never these source paths). Empty bazelPackagePath (the fidelity harness
// converting AT the workspace root) preserves the prior labelRoot-relative
// behavior exactly.
func rewriteGenruleCmd(cmd, cmakeSrc, buildDir, umbrellaPrefix, bazelPackagePath string) string {
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
	//   - bare <anchor> at an argv boundary → its re-anchor repl
	//     (buildDir → "."; cmakeSrc → the umbrella prefix, or "."
	//     when not promoted) — so -DLLVM_SOURCE_DIR=<src> becomes
	//     =llvm, not =. under umbrella promotion.
	//     The boundary requirement (whitespace / quote / argv
	//     separator on the right side) avoids mangling argv
	//     values that happen to start with the anchor prefix
	//     but continue with letters or digits (e.g. <buildDir>_other
	//     stays intact; <buildDir> followed by space or quote
	//     gets re-anchored).
	// cmakeSrc-prefixed paths re-anchor to the umbrella prefix
	// (cmakeSrc-relative-to-labelRoot, e.g. "llvm" for LLVM); buildDir
	// paths strip to "". Applying the umbrella here — during the strip,
	// before the prefix is gone — is what keeps `<cmakeSrc>/include`
	// (source, → llvm/include) distinct from `<buildDir>/include`
	// (build, → include); a post-pass couldn't tell them apart. Empty
	// umbrella (the non-promoted case) preserves the prior strip-to-""
	// behavior exactly.
	// Source-tree paths re-anchor to their exec-root form: the element's
	// landing package (bazelPackagePath) joined with the cmakeSrc-relative-
	// to-labelRoot umbrella segment. With an empty bazelPackagePath this
	// collapses to the umbrella-only form (the fidelity harness, converting
	// at the workspace root); with an empty umbrella it's just the package
	// (the common element-under-elements/<name> case, e.g. LLVM).
	srcBase := umbrellaPrefix
	if bazelPackagePath != "" {
		srcBase = filepath.ToSlash(filepath.Join(bazelPackagePath, umbrellaPrefix))
	}
	srcRepl, srcBareRepl := "", "."
	if srcBase != "" {
		srcRepl = srcBase + "/"
		srcBareRepl = srcBase
	}
	for _, a := range []struct{ anchor, repl, bareRepl string }{
		{cmakeSrc, srcRepl, srcBareRepl},
		{buildDir, "", "."},
	} {
		if a.anchor == "" {
			continue
		}
		cmd = strings.ReplaceAll(cmd, a.anchor+"/", a.repl)
		cmd = replaceBareAnchorAtBoundary(cmd, a.anchor, a.bareRepl)
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
// with `repl` whenever it sits at an argv-token boundary in `cmd`.
// "Boundary" = the character immediately after `anchor` is one of:
// whitespace, double-quote, single-quote, `=` (DKEY=VALUE shape),
// shell command-separator (`&`, `|`, `;`), or end-of-string.
//
// `repl` mirrors the `<anchor>/<rel>` strip's replacement so a bare
// anchor re-anchors consistently: buildDir → "." (Bazel's genrule cwd),
// cmakeSrc → the umbrella prefix in the promoted case (e.g. "llvm", so
// -DLLVM_SOURCE_DIR=<abs-src> becomes =llvm, not =.) or "." when not
// promoted.
//
// The argv-boundary requirement avoids mangling argv values that
// happen to start with the anchor prefix but continue with letters
// or digits (e.g. `<buildDir>_other` stays intact). Conservative on
// purpose — the cmake-emitted shapes that surface this (LLVM's
// -DLLVM_SOURCE_DIR=<abs-src>, VTK's -DCMAKE_BINARY_DIR=<abs-build>)
// all hit a clean argv boundary.
func replaceBareAnchorAtBoundary(cmd, anchor, repl string) string {
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
				b.WriteString(repl)
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
	cmd = strings.TrimPrefix(cmd, prefix)
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

// stripBalancedQuotes removes ONE balanced pair of surrounding double-quotes
// from s (`"foo"` → `foo`), leaving an unbalanced or quote-less token
// untouched (`"foo` and `foo` stay as-is). Used to de-shell-quote
// CompileCommandFragments tokens for Bazel's no-shell argv (see the call site
// in splitCompileFragments). Length guard (>=2) avoids treating a lone `"` as
// a pair.
func stripBalancedQuotes(s string) string {
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		return s[1 : len(s)-1]
	}
	return s
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
			// Strip a balanced pair of surrounding double-quotes cmake added
			// when it shell-quoted a fragment whose value carries shell-special
			// chars (CUDA's `"--generate-code=arch=compute_80,code=[sm_80]"` —
			// quoted for the `[`/`,`). Bazel passes copts as argv with NO shell,
			// so a literal surrounding quote becomes part of the argument and
			// breaks the tool (nvcc: "a single input file is required ..."). The
			// File API fragment is meant to be shell-tokenized; for a no-shell
			// argv the de-quoted token is the faithful flag. Only a whole-token
			// "..." pair is stripped (a flag never legitimately needs surrounding
			// shell quotes in argv form); embedded quotes are left untouched.
			p = stripBalancedQuotes(p)
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
// isGeneratedHeaderPath reports whether p has an extension that marks it a
// generated header a consumer #includes — the tablegen / x-macro `.inc` /
// `.def` idioms plus ordinary header extensions. Generated compiled
// sources (.cpp/.c/.S) are deliberately excluded: those flow through the
// direct-source genrule-output path in lowerTarget, not the consumer
// textual_hdrs wiring.
func isGeneratedHeaderPath(p string) bool {
	switch strings.ToLower(filepath.Ext(p)) {
	case ".inc", ".def", ".h", ".hh", ".hpp", ".hxx", ".ipp", ".gen":
		return true
	}
	return false
}

// collectCodegenHeaders returns the element-root-relative generated-header
// output paths a consumer needs on its include path. For each codemodel
// UTILITY (add_custom_target) dependency of the consumer it walks that
// target's ninja phony — bounded by a visited set, stopping at any sibling
// target's node so it never pulls a depended-on tool's or library's
// outputs — and keeps every reached node that is a recovered genrule
// output (cc.OutToGenrule) with a header extension. The returned paths key
// the same way as the genrule outs the split transform places, so the
// caller can group them by producing package. Returns nil when the
// consumer has no UTILITY deps or no ninja graph is available.
func collectCodegenHeaders(g *ninja.Graph, deps []fileapi.TargetDependency, utilityIDs map[string]bool, utilityIDToName map[string]string, outToGenrule map[string]string, isTargetName map[string]bool, phonyByName map[string][]string) []string {
	if g == nil || len(deps) == 0 {
		return nil
	}
	if g.OutputIndex == nil {
		g.Index()
	}
	var headers []string
	seen := map[string]bool{}
	for _, dep := range deps {
		if !utilityIDs[dep.Id] {
			continue
		}
		name := utilityIDToName[dep.Id]
		if name == "" {
			continue
		}
		// cmake's Ninja generator names a sub-directory custom target's
		// phony with its build-relative dir prefix (`gen/gen_inc`), while
		// the codemodel records the bare unique name (`gen_inc`). Seed the
		// walk from every ninja output whose final path component is the
		// target name — covering the prefixed phony and its CMakeFiles
		// intermediate. Falls back to the bare name (root-dir target).
		seeds := phonyByName[name]
		if len(seeds) == 0 {
			seeds = []string{name}
		}
		visited := map[string]bool{}
		stack := append([]string(nil), seeds...)
		for len(stack) > 0 {
			cur := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if visited[cur] {
				continue
			}
			visited[cur] = true
			if _, ok := outToGenrule[cur]; ok && isGeneratedHeaderPath(cur) {
				if !seen[cur] {
					seen[cur] = true
					headers = append(headers, cur)
				}
				continue
			}
			b := g.BuildFor(cur)
			if b == nil {
				continue
			}
			for _, ins := range [][]string{b.Inputs, b.ImplicitInputs, b.OrderOnly} {
				for _, in := range ins {
					// Don't cross into another named target's node: that
					// would pull a sibling's outputs (the tablegen tool, a
					// dependent lib). The target's own `.inc` outputs are
					// not target names, so they're still reached.
					if isTargetName[in] {
						continue
					}
					stack = append(stack, in)
				}
			}
		}
	}
	sort.Strings(headers)
	return headers
}

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
