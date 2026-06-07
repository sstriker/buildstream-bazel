// Package cli holds flag parsing, validation, and CLI exit codes for
// convert-element-cmake. Kept separate from main so it's testable without a process
// boundary.
package cli

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/sstriker/buildstream-bazel/internal/convmode"
)

// Exit codes documented in the README. These map onto the failure-tier model:
//
//	0   success
//	1   Tier-1 (per-codebase conversion error; failure.json written)
//	64  CLI usage error
//	65  Tier-2 (converter bug / malformed cmake output)
//	70  Tier-3 (infrastructure)
const (
	ExitSuccess = 0
	ExitTier1   = 1
	ExitUsage   = 64
	ExitTier2   = 65
	ExitTier3   = 70
)

// Args is the parsed command-line input.
type Args struct {
	// SourceRoot is the absolute path to the CMake project root.
	// When set, the converter runs cmake itself (capturing the
	// File API reply, build.ninja, --trace-expand log, and
	// cmake-variable dump in a fresh build dir).
	SourceRoot string

	// ReplyDir, when non-empty, points at an existing cmake
	// File API reply directory (typically `<build>/.cmake/api/
	// v1/reply`) and skips the cmake invocation. The converter
	// reads the codemodel from this directory and opportunisti-
	// cally picks up build.ninja, trace.jsonl, and the cmake
	// variable dump from the surrounding build dir (the
	// derivation walks four parents up from the reply dir).
	// CMakeBuildDir is the friendlier alias — most operators
	// should point at that instead.
	ReplyDir string

	// CMakeBuildDir is the friendly alias to ReplyDir for
	// operators who already ran cmake themselves and want the
	// converter to use the resulting build dir without
	// re-invoking cmake. Points at the cmake build dir directly
	// (typically the value cmake was invoked with as `-B`).
	// Parse derives ReplyDir = `<CMakeBuildDir>/.cmake/api/v1/
	// reply` automatically; downstream code uses ReplyDir
	// uniformly, so this flag is purely an entry-point
	// ergonomics fix. Exactly one of SourceRoot / ReplyDir /
	// CMakeBuildDir must be set.
	CMakeBuildDir string

	// StrictTrace, when true, refuses to run if the cmake
	// trace (trace.jsonl) is missing or empty. Without trace
	// data, several recovery paths silently skip (configure_file
	// lift, PUBLIC/PRIVATE include partition, IMPORTED-target
	// dep recovery for static libs, platform-conditional source
	// partition, etc.) — the rendered BUILD is structurally
	// valid but coverage is degraded. Off by default so existing
	// flows keep working; opt-in for production runs that
	// want to surface trace-input gaps loudly. When off, the
	// converter prints a warning to stderr on missing trace
	// instead of refusing.
	StrictTrace bool

	// ProbeDistroHardening, when true, compiles a tiny stub
	// with the convert host's cc and inspects the resulting
	// object file for hardening-symbol references
	// (__*_chk → -D_FORTIFY_SOURCE, __stack_chk_* →
	// -fstack-protector-*). Surfaces the detected flags as a
	// stderr warning so operators see the symbol-set delta
	// they should expect between the cmake-produced artifact
	// and the Bazel-rebuilt one. Diagnostic-only: no BUILD.bazel
	// emit-side change. Off by default; the probe costs one
	// cc invocation per convert run.
	ProbeDistroHardening bool

	// OutBuild is the destination path for the generated BUILD.bazel.
	OutBuild string

	// OutBundleDir, when non-empty, is the directory where the synthesized
	// cmake-config bundle is written (one .cmake file per kind).
	OutBundleDir string

	// OutExports, when non-empty, is the path where the element's
	// exports manifest (a manifest.Imports doc describing this element
	// as a producer: real namespaced cmake targets → this element's
	// Bazel labels) is written. Downstream consumers stage it via
	// --exports-in so their lower pass resolves the producer's
	// IMPORTED targets to real labels — replacing write-a's render-time
	// "<elem>::<elem>" convention guess with the producer's own
	// trace-recovered export surface. Content is deterministic
	// (sorted, source-intrinsic) so it doesn't churn consumer caches.
	OutExports string

	// ExportsIn lists producer exports-manifest files (each a
	// manifest.Imports doc emitted by a dep's --out-exports) to merge
	// into the imports resolver alongside --imports-manifest. This is
	// the action-time half of the producer→consumer export channel:
	// the dep's real export surface arrives as a build input rather
	// than being guessed at write-a render time. Repeatable
	// (--exports-in a --exports-in b).
	ExportsIn []string

	// CmakeDefines carries extra cmake cache variables for the configure as
	// KEY[=VALUE] entries WITHOUT a leading -D (cmakeDefinesToMap →
	// cmakerun.Options.ExtraCacheVars, which prepends the -D itself).
	// Repeatable (--cmake-define K1=V1 --cmake-define K2=V2). Drives a
	// project's own build options at configure time — e.g. glm's tests add a
	// `-Werror` that GCC 13 trips on (a -Wclass-memaccess in glm's own
	// headers), so the build lens passes --cmake-define CMAKE_CXX_FLAGS=-w to
	// inhibit warnings while leaving glm's C++ auto-detection (and thus its
	// std::hash specializations) intact.
	CmakeDefines []string

	// OutFailure, when non-empty, is the path to write failure.json on
	// Tier-1 errors.
	OutFailure string

	// ImportsManifest, when non-empty, is the path to a per-orchestration
	// imports manifest (see docs/codegen-tags.md sibling and
	// internal/manifest/imports.go for schema). Out-of-tree deps (CMake
	// targets the current codebase imports via find_package) are resolved
	// via this map; the orchestrator (M3) writes one before each
	// per-codebase conversion.
	ImportsManifest string

	// OutReadPaths, when non-empty and the converter ran cmake itself
	// (not via --reply-dir), writes a JSON array of source-tree paths
	// that cmake read at configure time, parsed from
	// `--trace-expand --trace-format=json-v1`. M3 merges these into
	// per-package allowlist registries.
	OutReadPaths string

	// OutTimings, when non-empty, writes a JSON document with
	// per-phase wall-clock timings: cmake configure, translation
	// (lower + emit), and total. M3 aggregates these into a final
	// summary so operators can see configure-vs-translate ratios
	// across a project.
	OutTimings string

	// OutCMakeConfigureReads, when non-empty, writes a JSON array of
	// source-relative paths drawn from build.ninja's RERUN_CMAKE
	// implicit-input list. This is the cmake-side configure-time
	// oracle: the set of files cmake itself thinks should re-trigger
	// configure when their bytes change (see
	// internal/ninja.Graph.ReconfigureInputs +
	// ProjectToSourceTree). A sibling oracle to OutReadPaths (which
	// derives from --trace-expand events); the two have overlap but
	// neither subsumes the other. Downstream audit tooling compares
	// either oracle against the per-kind narrowing patterns to flag
	// undercoverage drift.
	OutCMakeConfigureReads string

	// OutToolchainSignalDir, when non-empty, causes convert-element-cmake
	// to copy the cmake File API reply directory contents to this
	// path after a successful configure. Off by default; the
	// orchestrator opts in via CollectToolchainSignal so existing
	// flows that don't unify toolchains pay nothing for the extra
	// directory copy. Capture-only today: the on-disk format is the
	// input shape for the future unifier-side consumer (the
	// "Element-signal consumption in the unifier" follow-up under
	// ROADMAP.md Next), which will fold any per-element
	// builtin-include / sysroot fact a real element exposes that
	// the dedicated toolchain probe missed.
	OutToolchainSignalDir string

	// OutIRJSON, when non-empty, causes convert-element-cmake to write
	// the post-lower ir.Package as JSON to this path alongside
	// the rendered BUILD.bazel. The orchestrator's per-element
	// multi-platform fold consumes this JSON directly: re-parsing
	// the rendered Bazel rules to recover the IR would be brittle
	// (Bazel grammar is non-trivial and the emitter applies
	// platform-aware select() logic that's hard to reverse), so
	// shipping the IR as serialized data is the cleaner contract.
	//
	// Off by default; only the multi-platform orchestrator path
	// sets it. Single-platform conversion ignores it; the
	// rendered BUILD.bazel stays the canonical output.
	OutIRJSON string

	// LiftConfigureFile toggles the configure_file recovery's
	// lifted shape (.h.in as a real srcs +
	// //tools:cmake-configure-file invocation at Bazel build
	// time). Off by default so existing orchestrator-driven
	// flows that don't stage the tool keep working. Callers
	// who do stage //tools:cmake-configure-file (write-a's
	// project A render with --cmake-configure-file-bin set, and
	// any operator-built downstream Bazel envelope that mirrors
	// it) opt in via this flag. See internal/configurefile
	// package doc for the cache-key analysis.
	LiftConfigureFile bool

	// ToolchainFeaturesFrom points the raw-flag → feature lift at the
	// operator's REAL Bazel toolchain: a path to a cc_toolchain_config.bzl
	// (or a toolchains/ dir of *.bzl) whose declared feature() names are
	// enumerated (toolchainscan.ParseDeclared) and used as the lift's gate
	// instead of the converter's generated default. So the lift only
	// rewrites a flag onto a feature the operator's toolchain actually
	// defines. Unset (empty path) keeps the generated-toolchain default; when
	// set, the parsed vocabulary gates the lift even if it's empty (a
	// toolchain whose features the parser can't read lifts only the built-in
	// `pic`, not the generated default).
	ToolchainFeaturesFrom string

	// DumpVars toggles the post-configure variable-namespace
	// capture (cmakerun stages dump-vars.cmake into
	// CMAKE_PROJECT_TOP_LEVEL_INCLUDES; on success cmake writes
	// `<build>/cmake-to-bazel.vars.dump`). The lower's
	// configure_file recovery AND the find_package variable-form
	// attribution path both read it — the former for @VAR@ /
	// ${VAR} substitution, the latter for <Pkg>_LIBRARIES →
	// package name correlation when the configureLog
	// find_package-v1 event isn't available (cmake < 3.32).
	//
	// On by default. The hook is cmake 3.24+; on older cmakes
	// the `CMAKE_PROJECT_TOP_LEVEL_INCLUDES` injection silently
	// fails to install and the dump file never lands — the
	// downstream paths fall back to their non-dump shapes
	// (configure_file lift uses build-time-only @VAR@, attribution
	// uses configureLog events only). Pass --dump-vars=false to
	// opt out explicitly (the per-conversion cost is small but
	// non-zero on large CMakeLists trees).
	//
	// Pre-#229 this was implicitly coupled to LiftConfigureFile ||
	// ProbeGenex inside cmd/convert-element-cmake/main.go.
	// Decoupling it lets operators who run without probe-genex
	// (--probe-genex=false) still get the variable-form
	// attribution path.
	DumpVars bool

	// UnsupportedExecuteProcessFallback toggles the
	// recoverExecuteProcess refusal path's behaviour. Off
	// (default): refusals exit Tier-1 with the typed
	// `unsupported-execute-process` failure code, the same as
	// today. On: refusals don't exit Tier-1; the converter
	// instead emits a placeholder BUILD.bazel — at this stage
	// (Step 2 / PR #97), an enumeration of the codemodel's
	// non-UTILITY targets as **empty cc_library / cc_binary /
	// cc_library-interface stubs** with public visibility, so
	// downstream label references resolve at analysis time.
	// Step 2.5 (PR #98) wires those stubs to the element's
	// round-2 install root via per-target cc_import /
	// sh_binary rules reconstructed from
	// Target.Install.Destinations + NameOnDisk; until then,
	// downstream consumers' compile/link actions against the
	// stubs will fail (analysis is the only contract this PR
	// delivers). See
	// docs/design/rendezvous.md.
	UnsupportedExecuteProcessFallback bool

	// Fidelity is the operator-facing refusal-handling dial threaded
	// from write-a. "strict" (default) leaves the per-kind fallback
	// behaviors off; "best-effort" turns them on. For convert-
	// element-cmake "on" means implicitly enabling
	// UnsupportedExecuteProcessFallback (the execute_process
	// placeholder shape). Maintained as a separate flag — rather
	// than merged onto UnsupportedExecuteProcessFallback — so the
	// dial vocabulary is uniform across converters (meson/pyproject
	// honor the same flag and map it to their own internal fallback
	// switches). The low-level
	// --unsupported-execute-process-fallback flag stays as the
	// per-kind escape hatch and overrides the dial.
	Fidelity string

	// BakeIn is the operator-facing convert-time-baking dial:
	// "warn" (default, today's behaviour — every baked output shows
	// up on stderr but conversion succeeds), "allow" (silent),
	// or "reject" (any bake-shaped emission exits non-zero with the
	// inventory embedded). Orthogonal to Fidelity: it asks "HOW
	// should successful conversions emit?", not "WHAT to do on
	// refusal?". Empty resolves to "warn" via
	// convmode.ParseBakeIn so a zero-valued Args preserves today's
	// CLI semantics. Plumbed into lower.Options.BakeIn.
	BakeIn string

	// Diagnostics is the operator-facing diagnostic-mode dial:
	// "off" (default) keeps the strict first-Tier-1-refusal-aborts
	// behavior; "on" implicitly enables
	// IgnoreRejectionsForDiagnostics so every refusal is collected
	// rather than aborting the run. Equivalent to passing
	// --ignore-rejections-for-diagnostics directly. The low-level
	// flag stays as an alias.
	Diagnostics bool

	// AllowCMakeVersionMismatch lets the converter run with a cmake
	// version below the architectural floor (3.20 — codemodel-v2 minimum).
	// Local-dev only; M3 must never set this.
	AllowCMakeVersionMismatch bool

	// CMP0026Shim toggles the cmake-4.x compatibility shim that
	// overrides get_target_property to translate LOCATION queries
	// into $<TARGET_FILE:<tgt>> generator expressions. cmake 4.x
	// removed CMP0026 OLD entirely; legacy packages that read
	// `get_target_property(<v> <tgt> LOCATION)` (the pre-3.0
	// idiom yasm and other vintage codebases still rely on) now
	// fatal-error at configure time. With this flag on, the
	// converter stages a small shim cmake script into the build
	// dir and adds it to CMAKE_PROJECT_TOP_LEVEL_INCLUDES so
	// every project() call picks up the override before user
	// code runs. Opt-in because the override changes
	// get_target_property's return shape (a generator
	// expression rather than a resolved configure-time path)
	// for ALL LOCATION queries — projects that string-compose
	// the LOCATION value at configure time would see literal
	// `$<TARGET_FILE:foo>` text instead of a path. See #208.
	CMP0026Shim bool

	// ProbeGenex toggles the per-target genex-probe hook (Phase 3
	// of the generator-parity uplift in ROADMAP.md). When true,
	// convert-element-cmake stages probe-genex.cmake into the build
	// dir and layers it onto CMAKE_PROJECT_TOP_LEVEL_INCLUDES; the
	// hook walks BUILDSYSTEM_TARGETS recursively from the source
	// root and emits file(GENERATE) declarations capturing common
	// genex shapes (TARGET_FILE / TARGET_OBJECTS / INTERFACE_*
	// aggregates) into per-target probe files. The lift can then
	// consult cmakerun.ReadGenexProbe to retire UnsupportedError
	// sites in internal/genexeval where the resolution depends on
	// cmake's transitive-property walk. Default ON — the hook is
	// a net improvement for every conversion; the affirmative
	// type gate keeps it safe on UTILITY / ALIAS / INTERFACE
	// targets. Requires cmake 3.24+; pass --probe-genex=false to
	// disable when targeting older toolchains.
	ProbeGenex bool

	// TwoPassGenex toggles the generalized-genex warm second
	// configure pass. When true (the default) and the first ToIR
	// pass records arbitrary genex literals the Go-side evaluator +
	// structural probe couldn't resolve, convert-element-cmake runs
	// a second cmake configure against the SAME (warm) build dir
	// that injects a file(GENERATE) literal-probe hook, reads the
	// resolved bytes back, and re-runs ToIR with them in hand. The
	// second pass is conditional — skipped entirely when the first
	// pass leaves no unresolved literals (the common case, zero
	// overhead) — and warm (reuses the first pass's try_compile /
	// find_package cache), so it costs a small fraction of the
	// first configure. Only meaningful in --source-root mode (the
	// offline --reply-dir / --cmake-build-dir paths have no live
	// build dir to reconfigure). Pass --two-pass-genex=false to
	// disable.
	TwoPassGenex bool

	// BuildType selects the cmake configuration name passed via
	// -DCMAKE_BUILD_TYPE. Empty defaults to "Release" inside
	// cmakerun.Configure. Mutually exclusive with BuildTypes —
	// when BuildTypes is non-empty, BuildType must be empty.
	BuildType string

	// AuditBazelIdiomReport, when non-empty, writes the structured
	// audit findings as JSON to this path. The audit itself runs
	// unconditionally — bazelidiom.Audit is read-only and emits
	// to stderr only when findings exist, so there's no cost to
	// always-on; the bool toggle was retired (see PR #227 review).
	AuditBazelIdiomReport string

	// AuditCoverageReport, when non-empty, writes the lens-3
	// dependency-coverage findings as JSON to this path. Like the
	// bazelidiom audit, the check runs unconditionally (findings go
	// to stderr); the path only adds the structured report. See
	// converter/internal/coverage.
	AuditCoverageReport string

	// EmitProvenance enables Phase 1 task 1's backtrace-derived
	// per-rule annotation: emit a leading `# Source: <file>:<line>
	// (<command>)` comment above each rule whose cmake declaration
	// is recorded in the codemodel's BacktraceGraph. Default ON —
	// the comment is high-signal navigation help; pass
	// --emit-provenance=false for byte-clean output (e.g. golden
	// regression tests pre-dating the provenance comments).
	EmitProvenance bool

	// EmitSourceComments enables comment-carrying: recover the author's
	// CMakeLists comments (leading block per target + the file header) from
	// raw source and emit them onto the corresponding rules. Default ON —
	// the author's comments are high-signal navigation help that survives
	// into the converted BUILD. Unlike --emit-provenance (which reads the
	// already-loaded backtrace), this reads raw source files, adding them
	// to the action's inputs; pass --emit-source-comments=false to suppress
	// (skips the raw-source reads, e.g. for byte-clean output or
	// reply-dir-only runs where source isn't staged). Drives
	// lower.Options.RecoverSourceComments + emit.Options.EmitSourceComments
	// together.
	EmitSourceComments bool

	// EmitStandaloneCustomCommands enables Phase 4 of the
	// generator-parity uplift's standalone-genrule emission: walk
	// every CUSTOM_COMMAND edge in build.ninja and emit a genrule
	// for each whose outputs aren't already covered by an existing
	// recoverGenrule emission. On by default — Phase 4 graduated
	// after fixture coverage + render gate (see
	// scripts/meta-cmake-standalone-custom-command.sh). Pass
	// --emit-standalone-custom-commands=false to opt out for
	// operators who hit edge cases (e.g. projects with very large
	// custom-command graphs where the new genrules slow analysis
	// or surface phony bookkeeping rules the operator doesn't want
	// rendered).
	EmitStandaloneCustomCommands bool

	// OutSanitizerFeatures, when non-empty, writes a .bzl file
	// at this path carrying cc_toolchain feature definitions
	// extracted from cmake's CMAKE_<LANG>_FLAGS_<CONFIG> /
	// CMAKE_<TYPE>_LINKER_FLAGS_<CONFIG> cache entries for every
	// sanitizer-shaped configuration the operator passed via
	// --build-types. Phase 5 of the generator-parity uplift
	// (ROADMAP.md). Operators thread SANITIZER_FEATURES from the
	// generated file into their cc_toolchain config.
	//
	// Empty (zero) suppresses the emit — back-compat for callers
	// that don't want the sidecar.
	OutSanitizerFeatures string

	// OutConfigSettings, when non-empty, writes a //config package
	// BUILD file at this path: a string_flag `build_type` plus one
	// config_setting per (non-sanitizer) cmake configuration in
	// --build-types. These back the multi-config fold's
	// //config:<name> select() arms, making the converted output
	// self-contained (select with --//config:build_type=<name>).
	// Phase 5 of the generator-parity uplift (ROADMAP.md).
	//
	// Empty (zero) suppresses the emit; only meaningful with a
	// multi-config --build-types.
	OutConfigSettings string

	// BuildTypes selects the cmake "Ninja Multi-Config" generator
	// path (Phase 5 of the generator-parity uplift). When non-empty,
	// the configure pass runs once with CMAKE_CONFIGURATION_TYPES=
	// <joined ;> and the codemodel reply carries one Configuration
	// entry per name. Phase 5's downstream fold (queued) collapses
	// per-config compile/link fragments via select() arms over
	// //config:<name> config_settings; for sanitizer-shaped names
	// (ASan / TSan / MSan / UBSan / LTO) the fragments lower to
	// --features on the cc_toolchain instead of raw selects.
	//
	// Custom config names work natively — cmake treats the list
	// opaquely. The project must define CMAKE_<LANG>_FLAGS_<NAME>
	// for any non-standard entry (typically via a
	// cmake/Sanitizers.cmake module or a toolchain file).
	BuildTypes []string

	// PrefixDir, when non-empty, is added to CMAKE_PREFIX_PATH. Holds the
	// synthesized cmake-config bundles + zero-byte IMPORTED_LOCATION
	// stubs that let find_package resolve out-of-tree deps. The
	// orchestrator (M3a step 4) builds the tree per-codebase from the
	// converted-deps registry; standalone runs leave this empty.
	PrefixDir string

	// ToolchainCMakeFile, when non-empty, points at a CMake toolchain
	// file (typically derive-toolchain's toolchain.cmake) that pre-
	// populates the compiler-detection cache. cmakerun passes it via
	// -DCMAKE_TOOLCHAIN_FILE so cmake skips the compiler-detection
	// probe — a measurable per-conversion latency win at project
	// scale.
	ToolchainCMakeFile string

	// Verify, when true, cross-checks the lowered IR against the
	// compile_commands.json cmake emits at configure time. Mismatches
	// (a -D macro or -I include that's in compile_commands but not in
	// any IR target's flags, or vice versa) are surfaced as warnings
	// on stderr; conversion still succeeds. Off by default — adds a
	// JSON parse + per-source diff pass; only enable in CI or when
	// auditing a converter change.
	Verify bool

	// VerifyReport, when non-empty, writes the structured Report
	// (verify.Report — JSON) to this path in addition to (or instead
	// of) the stderr warnings. Implies Verify=true.
	VerifyReport string

	// BazelPackagePath, when non-empty, is the repo-root-relative
	// path of the Bazel package the emitted BUILD.bazel will live
	// in (e.g. "elements/hello-world"). Threaded into
	// bazel.Options.BazelPackagePath so the `# gazelle:cc_search`
	// directives are framed in the repo-root-relative form
	// gazelle_cc's resolver expects. Empty (the test default)
	// suppresses the directive entirely — wrong bytes are worse
	// than no bytes, since gazelle_cc would interpret an
	// unframed directive as pointing at the workspace root.
	BazelPackagePath string

	// CMakeScriptRunner is the operator-supplied Bazel label of a
	// target that, when invoked, behaves like cmake (specifically:
	// supports `<runner> -P <script.cmake> [-D <var>=<val> ...]`).
	// When non-empty, custom commands using `cmake -P` lift to a
	// genrule that calls the runner at Bazel build time instead of
	// refusing with UnsupportedCustomCommandScript.
	//
	// Soundness caveats: cmake -P scripts that hardcode absolute
	// paths (configure_file-derived scripts with `set(SRCDIR
	// "/abs/path")`) won't resolve under Bazel's sandbox.
	// Parameter-driven scripts (e.g. VTK's vtkHashSource shape,
	// which takes inputs via -D args) work cleanly. Off by
	// default; only operators who stage a runner tool opt in.
	CMakeScriptRunner string

	// CMakeScriptBake runs the cmake -P script at convert time
	// and bakes the resulting OUTPUT bytes into static genrules
	// that materialize them via base64-decode. Closes the
	// script-hardcoded-absolute-paths gap that the runner-only
	// lift can't (paths resolve at convert time where they
	// exist). Trade-off: outputs are convert-time-baked and
	// don't auto-refresh on input change — operator re-runs
	// convert. Same shape + warning as the legacy
	// configure_file capture. Off by default.
	CMakeScriptBake bool

	// LiftCCEmbed recognizes a known file-embedding cmake -P encoder
	// (vtkEncodeString) and lowers it to the native cc_embed rule. Off
	// by default.
	LiftCCEmbed bool

	// LiftCCHash recognizes a known file-hashing cmake -P script
	// (vtkHashSource) and lowers it to the native cc_hash rule. Off by
	// default.
	LiftCCHash bool

	// CMakeScriptTrace asks the cmake -P lift to actually run
	// the script under `cmake --trace --trace-format=json-v1
	// -P <script>` at convert time. The trace's read paths
	// drive auto-augmentation of the genrule's srcs and a
	// structured refusal diagnostic when the script touches
	// paths Bazel's sandbox can't reproduce. Off by default —
	// the trace step is convert-time execution of arbitrary
	// cmake-script-language; operators opt in by passing
	// --cmake-script-trace after acknowledging the side-effect
	// risk. Requires --cmake-script-runner (no trace without a
	// runner — the operator's already opted into the lift
	// flow).
	CMakeScriptTrace bool

	// IgnoreRejectionsForDiagnostics switches the converter from
	// "first Tier-1 refusal aborts" to "collect every refusal,
	// continue past each with a local skip, write a diagnostic
	// report at the end". The resulting BUILD.bazel is NOT
	// guaranteed to build — refused constructs (bad source paths,
	// unresolved link deps, unsupported target types, missing
	// custom commands, classifier-refused execute_process calls)
	// are silently elided so the lower can reach the end of the
	// codemodel walk. Implicitly enables
	// UnsupportedExecuteProcessFallback (any execute_process
	// refusal routes through that pre-existing fallback path).
	// Use with --rejections-report=<path> to capture the structured
	// rejection list. Off by default — production conversion paths
	// want the strict refusal so broken output never lands.
	IgnoreRejectionsForDiagnostics bool

	// RejectionsReport, when non-empty, is the path the converter
	// writes a JSON array of recorded Rejection records to. Only
	// meaningful with --ignore-rejections-for-diagnostics; ignored
	// when the strict path runs (an empty file is written instead
	// so consumers can rely on the path existing).
	RejectionsReport string

	// ConversionTodos enables the agent-prompts producer: write the
	// structured conversion-todos.json (the "no-mechanical-form" cmake
	// constructs an author or AI post-pass must re-express). Default ON.
	// Destination is ConversionTodosReport when set, else
	// "<dir(OutBuild)>/conversion-todos.json"; with no resolvable
	// destination the producer is a silent no-op. Pass --conversion-todos=false
	// to suppress entirely.
	ConversionTodos bool

	// ConversionTodosReport, when non-empty, is the explicit path the
	// converter writes the structured conversion-todos.json to: the
	// "no-mechanical-form" cmake constructs (add_test COMMAND cmake -P
	// harnesses, filtered command edges with no Bazel analogue,
	// install(SCRIPT)/install(CODE)) an author or AI post-pass must
	// re-express. Always materialized when ConversionTodos is on (an empty
	// {todos:[]} report when nothing fired), so consumers can rely on the
	// path existing. Overrides the OutBuild-derived default destination.
	// Independent of --ignore-rejections-for-diagnostics — these are clean
	// (Tier-0) drops, not refusals. See converter/internal/todos.
	ConversionTodosReport string

	// ConversionTodosPreamble, when non-empty, is the path to an
	// operator-supplied preamble that replaces the built-in default in
	// conversion-todos.json. Read verbatim as prose (not JSON). Empty
	// uses the built-in default encoding this repo's
	// transition-to-plain-Bazel intent with the brotli worked example.
	ConversionTodosPreamble string

	// SourceKey, when non-empty, names the @src_<key>// external
	// repository the FUSE-sources path declared for this element's
	// source tree. The Bazel emitter prefixes every relative source
	// path in cc_library/cc_binary `srcs = [...]` with
	// "@src_<SourceKey>//:" so project B references sources by
	// digest-stable Bazel label rather than by symlinked filesystem
	// path. Empty leaves the legacy behaviour (relative paths
	// resolved against the local package).
	//
	// The label form is what makes project B's compile actions
	// fully BwoB: the executor reads source bytes from CAS by
	// digest reference; the dev machine never materialises them
	// (no FUSE crutch downstream of the converter). FUSE remains
	// the mechanism that fed the converter itself — cmake walks
	// the filesystem so it needs the symlinked tree — but stops
	// being necessary once we have a precise file list.
	SourceKey string

	// SplitPackages, when true, switches the standalone CLI emit from
	// a single monolithic BUILD.bazel to one BUILD.bazel per directory
	// (the "gazelle model"), mirroring the CMakeLists/add_subdirectory
	// layout: targets land in the Bazel package matching their
	// declaring cmake dir, header include-roots become a synthesized
	// header cc_library, and intra-element deps are rewritten to the
	// cross-package label form. Default false; when false the output
	// is byte-identical to the pre-feature single-BUILD emit.
	//
	// Mutually exclusive with --out-ir-json (the multi-platform fold
	// path round-trips IR through JSON, and the per-directory split is
	// a v1-scope-cut single-platform-only emit transform).
	SplitPackages bool

	// EmitInstallExportConfig opts in to generating the install(EXPORT)
	// config-mode bundle — the real <Pkg>Targets.cmake + cmake_config_bundle
	// filegroup. Off by default: the orchestrated graph wires the
	// synthprefix-synthesized bundle, so the converter's standalone bundle is
	// unused and emitting it over non-existent .cmake files would break
	// `bazel build //...`. Opt in when shipping the element for EXTERNAL cmake
	// config-mode consumption.
	EmitInstallExportConfig bool

	// EmitSharedLibraries opts in to faithful SHARED conversion: a cmake
	// SHARED_LIBRARY/MODULE_LIBRARY emits its static cc_library impl PLUS a
	// sibling cc_shared_library (real .so). Off by default — the static-collapse
	// emit stays byte-identical.
	EmitSharedLibraries bool
}

// reservedCmakeDefine names cmake cache vars the converter drives itself —
// either through a dedicated flag (routed to its own cmakerun slot) or set
// unconditionally to make the configure observable. Accepting one via
// --cmake-define would conflict at configure time (last-wins, or a broken
// codemodel), so each is rejected at parse time with the per-var guidance
// shown here (a single hardcoded "use --build-type" message would be wrong for
// the non-build-type entries).
var reservedCmakeDefine = map[string]string{
	"CMAKE_BUILD_TYPE":                 "set it via --build-type / --build-types",
	"CMAKE_CONFIGURATION_TYPES":        "set it via --build-types",
	"CMAKE_TOOLCHAIN_FILE":             "set it via --toolchain-cmake-file",
	"CMAKE_EXPORT_COMPILE_COMMANDS":    "the converter sets it ON to read the compile commands",
	"CMAKE_PROJECT_TOP_LEVEL_INCLUDES": "the converter sets it to inject its variable-dump hook",
	"CMAKE_FIND_PACKAGE_PREFER_CONFIG": "the converter sets it ON to keep find_package hermetic (prefer config-mode)",
}

// Parse reads argv (without program name), populates Args, and prints usage
// to stderr if invalid. Returns ExitUsage on bad input.
func Parse(argv []string, stderr io.Writer) (Args, int) {
	fs := flag.NewFlagSet("convert-element-cmake", flag.ContinueOnError)
	fs.SetOutput(stderr)
	a := Args{}
	fs.StringVar(&a.SourceRoot, "source-root", "", "absolute path to the CMake project root; the converter runs cmake itself in a fresh build dir")
	fs.StringVar(&a.ReplyDir, "reply-dir", "", "skip cmake invocation; read File API reply from this dir (typically <build>/.cmake/api/v1/reply). --cmake-build-dir is the friendlier alias")
	fs.StringVar(&a.CMakeBuildDir, "cmake-build-dir", "", "skip cmake invocation; point at an existing cmake build dir (the value passed to cmake -B). Derives the reply dir as <cmake-build-dir>/.cmake/api/v1/reply and auto-picks up build.ninja / trace.jsonl / cmake-variable dump from the same dir")
	fs.BoolVar(&a.StrictTrace, "strict-trace", false, "refuse with a Tier-1 error when no cmake trace data is available (instead of warning and continuing with degraded recovery). Implicitly enabled by --fidelity=strict (the dial default); pass --strict-trace=false to opt out — needed for offline replay flows where trace data isn't available alongside the fileapi reply.")
	fs.BoolVar(&a.ProbeDistroHardening, "probe-distro-hardening", false, "probe the convert host's cc for distro-default hardening flags (FORTIFY_SOURCE, stack-protector) that Bazel's hermetic cc_toolchain won't reproduce; emit a stderr warning naming the detected flags + a remediation recipe. Diagnostic-only.")
	fs.StringVar(&a.OutBuild, "out-build", "BUILD.bazel", "destination path for generated BUILD.bazel")
	fs.StringVar(&a.OutBundleDir, "out-bundle-dir", "", "directory for synthesized cmake-config bundle (optional)")
	fs.StringVar(&a.OutExports, "out-exports", "", "path to write this element's exports manifest (manifest.Imports JSON: real namespaced cmake targets → this element's Bazel labels) for downstream consumers' --exports-in (optional; requires --bazel-package-path for label formation)")
	fs.Var(repeatedString{&a.ExportsIn}, "exports-in", "producer exports-manifest file to merge into the imports resolver (the action-time half of the producer→consumer export channel). Repeatable: --exports-in a --exports-in b.")
	fs.Var(repeatedString{&a.CmakeDefines}, "cmake-define", "extra cmake cache variable for the configure, as KEY[=VALUE] WITHOUT a leading -D (the converter adds the -D) — drives a project's own build options. E.g. --cmake-define CMAKE_CXX_FLAGS=-w to inhibit a project's warnings-as-errors that a newer host compiler trips on. Repeatable: --cmake-define K1=V1 --cmake-define K2=V2.")
	fs.StringVar(&a.OutFailure, "out-failure", "", "write Tier-1 failure JSON here on per-codebase errors (optional)")
	fs.StringVar(&a.ImportsManifest, "imports-manifest", "", "path to JSON imports manifest mapping out-of-tree CMake targets to Bazel labels (optional)")
	fs.StringVar(&a.OutReadPaths, "out-read-paths", "", "write JSON array of source-tree paths cmake read at configure time (requires --source-root, optional)")
	fs.StringVar(&a.OutTimings, "out-timings", "", "write JSON with per-phase wall-clock timings (cmake configure, translation, total)")
	fs.StringVar(&a.OutCMakeConfigureReads, "out-cmake-configure-reads", "", "write JSON array of source-relative paths from build.ninja's RERUN_CMAKE implicit-input list (configure-time oracle)")
	fs.StringVar(&a.OutToolchainSignalDir, "out-toolchain-signal-dir", "", "directory; on success, copy the cmake File API reply contents here so the unifier can fold per-element toolchain signal into the platform's ResolvedToolchain.Base")
	fs.StringVar(&a.OutIRJSON, "out-ir-json", "", "write the post-lower ir.Package as JSON to this path. Drives the orchestrator's per-element multi-platform fold; ignored by single-platform flows.")
	fs.BoolVar(&a.LiftConfigureFile, "lift-configure-file", false, "emit configure_file recovery in the lifted shape (.h.in as a real srcs + //tools:cmake-configure-file invocation at Bazel build time). Requires the caller to stage //tools:cmake-configure-file. Off by default to preserve compatibility with downstream Bazel envelopes that don't yet stage the tool.")
	fs.StringVar(&a.ToolchainFeaturesFrom, "toolchain-features-from", "", "path to the operator's cc_toolchain_config.bzl (or a toolchains/ dir of *.bzl); its declared feature() names gate the raw-flag → cc_toolchain feature lift instead of the converter's generated default, so the lift matches the real toolchain. Unset keeps the generated default; when set, only features the toolchain literally declares lift (a wrapper/computed-name toolchain the parser can't read → only the built-in pic lifts, with a warning).")
	fs.BoolVar(&a.DumpVars, "dump-vars", true, "stage the dump-vars.cmake hook to capture cmake's variable namespace into <build>/cmake-to-bazel.vars.dump. Read by configure_file lift (@VAR@ / ${VAR} substitution) and find_package variable-form attribution (<Pkg>_LIBRARIES correlation on cmakes below the 3.32 find_package-v1 floor). On by default; requires cmake 3.24+ (silently inactive on older cmakes — the hook's CMAKE_PROJECT_TOP_LEVEL_INCLUDES injection floor).")
	fs.BoolVar(&a.UnsupportedExecuteProcessFallback, "unsupported-execute-process-fallback", false, "on classifier refusal of execute_process calls, emit empty cc_library/cc_binary stubs so downstream consumers' label resolution still works (round-2 mode). Off by default; see docs/design/rendezvous.md. Low-level per-kind escape hatch; --fidelity=best-effort enables it implicitly, and an explicit value here overrides the dial-derived default.")
	fs.StringVar(&a.Fidelity, "fidelity", "", "operator-facing refusal-handling dial: \"strict\" (default; refusals exit non-zero) or \"best-effort\" (refusals lower to placeholder shapes — for kind:cmake, an enumeration of cc_library/cc_binary stubs over the install-root TreeArtifact (via pick_file)). Implicitly enables --unsupported-execute-process-fallback. Threaded verbatim from cmd/write-a; the same vocabulary applies to convert-element-meson / -pyproject so a higher-level operator dial reads consistently across kinds.")
	fs.StringVar(&a.BakeIn, "bake-in", "", "convert-time-baking policy: \"warn\" (default; the baking-warnings post-pass emits the per-rule inventory on stderr but conversion succeeds), \"allow\" (silent), or \"reject\" (any bake-shaped emission exits non-zero with the inventory embedded). Orthogonal to --fidelity; threaded verbatim from cmd/write-a's --bake-in dial.")
	fs.BoolVar(&a.Diagnostics, "diagnostics", false, "operator-facing diagnostic-mode dial: when set, every Tier-1 refusal is collected (write the report via --rejections-report) and the run continues past each refusal rather than aborting on the first. Implicitly enables --ignore-rejections-for-diagnostics. Threaded verbatim from cmd/write-a's --diagnostics; the same flag exists on convert-element-meson and -pyproject for CLI uniformity.")
	fs.BoolVar(&a.AllowCMakeVersionMismatch, "allow-cmake-version-mismatch", false, "let convert-element-cmake run with cmake older than the codemodel-v2 floor (local-dev escape hatch)")
	fs.BoolVar(&a.CMP0026Shim, "cmp0026-shim", false, "translate get_target_property(... LOCATION) into $<TARGET_FILE:...> at configure time (cmake 4.x escape hatch for removed CMP0026 OLD). Changes LOCATION's return shape project-wide; see #208.")
	fs.BoolVar(&a.ProbeGenex, "probe-genex", true, "stage the per-target genex-probe hook (Phase 3 of the generator-parity uplift). cmake emits file(GENERATE) for each artifact-producing target's common genex shapes (TARGET_FILE, TARGET_OBJECTS, INTERFACE_*) so the lift reads post-walk resolved bytes instead of reimplementing the cmake-side evaluator. Default ON; requires cmake 3.24+.")
	fs.BoolVar(&a.TwoPassGenex, "two-pass-genex", true, "enable the warm second cmake configure passes (source-root mode, cmake 3.24+). Two independent triggers share this flag: (1) arbitrary genex literals the structural probe + Go-side evaluator can't resolve are resolved via a file(GENERATE) reconfigure; (2) when the first pass finds VCS-stamp vars, a NON-EXPANDED-trace reconfigure recovers set(X ${Y}) copies so a configure_file referencing a copy of a stamp var lifts to stamp_values. Both are conditional (skipped when nothing is unresolved / no stamp vars — zero overhead otherwise) and warm (reuse the first pass's try_compile/find_package cache). Pass --two-pass-genex=false to disable both.")
	fs.StringVar(&a.BuildType, "build-type", "", "cmake -DCMAKE_BUILD_TYPE value (defaults to Release in cmakerun). Mutually exclusive with --build-types.")
	fs.Var(commaSlice{&a.BuildTypes}, "build-types", "comma-separated list of cmake configuration names; switches the generator to \"Ninja Multi-Config\" with -DCMAKE_CONFIGURATION_TYPES=<a;b;c>. Phase 5 of the generator-parity uplift (ROADMAP.md). Mutually exclusive with --build-type.")
	fs.BoolVar(&a.EmitProvenance, "emit-provenance", true, "above each emitted rule, write a leading `# Source: <file>:<line> (<command>)` comment derived from the cmake codemodel's BacktraceGraph. Default ON; pass --emit-provenance=false for byte-clean output.")
	fs.BoolVar(&a.EmitSourceComments, "emit-source-comments", true, "carry author comments from CMakeLists into the emitted BUILD: the leading `#` comment block above each target's declaration, plus the top-of-file header block. Default ON; pass --emit-source-comments=false to suppress (skips reading raw source — useful for byte-clean output or reply-dir-only runs where source isn't staged).")
	fs.BoolVar(&a.EmitStandaloneCustomCommands, "emit-standalone-custom-commands", true, "Phase 4 of the generator-parity uplift: walk every CUSTOM_COMMAND edge in build.ninja and emit a genrule for each whose outputs aren't already covered by an existing recoverGenrule emission. On by default; covers add_custom_target / add_custom_command edges nothing consumes. Pass --emit-standalone-custom-commands=false to opt out.")
	fs.StringVar(&a.OutSanitizerFeatures, "out-sanitizer-features", "", "write cc_toolchain sanitizer feature definitions (.bzl) extracted from cmake's CMAKE_<LANG>_FLAGS_<CONFIG> cache for sanitizer-shaped configs in --build-types. Phase 5 of the generator-parity uplift.")
	fs.StringVar(&a.OutConfigSettings, "out-config-settings", "", "write a //config package BUILD (string_flag build_type + one config_setting per non-sanitizer config in --build-types) backing the multi-config fold's //config:<name> select() arms, making the converted output self-contained. Phase 5 of the generator-parity uplift.")
	fs.StringVar(&a.AuditBazelIdiomReport, "audit-bazel-idiom-report", "", "write the structured bazelidiom audit findings (JSON) to this path. The audit pass itself runs unconditionally on every convert and surfaces findings on stderr.")
	fs.StringVar(&a.AuditCoverageReport, "audit-coverage-report", "", "write the structured lens-3 dependency-coverage findings (JSON) to this path. The check runs unconditionally on every convert (findings to stderr); it flags trace target_link_libraries arms naming an in-codebase target that didn't land in any dep bucket.")
	fs.StringVar(&a.PrefixDir, "prefix-dir", "", "directory added to CMAKE_PREFIX_PATH (out-of-tree synth-prefix; orchestrator-driven)")
	fs.StringVar(&a.ToolchainCMakeFile, "toolchain-cmake-file", "", "CMake toolchain file (typically derive-toolchain's toolchain.cmake); skips per-conversion compiler probing")
	fs.StringVar(&a.SourceKey, "source-key", "", "when set, prefix every source path in emitted cc_library/cc_binary srcs with @src_<key>//: (the FUSE-sources Bazel-label path)")
	fs.BoolVar(&a.SplitPackages, "split-packages", false, "emit one BUILD.bazel per directory (the gazelle model) mirroring the CMakeLists/add_subdirectory layout, instead of a single monolithic BUILD.bazel. Targets land in the package matching their declaring cmake dir; header include-roots become a synthesized header cc_library; intra-element deps are rewritten to cross-package labels. Off by default (single-BUILD output is byte-identical to today). Mutually exclusive with --out-ir-json.")
	fs.BoolVar(&a.EmitInstallExportConfig, "emit-install-export-config", false, "generate the install(EXPORT) config-mode bundle — the real <Pkg>Targets.cmake (imported-target defs) plus the cmake_config_bundle filegroup. Off by default: the orchestrated graph wires the synthprefix-synthesized bundle, so the converter's standalone bundle is unused and emitting it over the (install-generated, not-on-disk) .cmake files would break `bazel build //...`. Opt in when shipping the element for EXTERNAL cmake config-mode consumption.")
	fs.BoolVar(&a.EmitSharedLibraries, "emit-shared-libraries", false, "faithful SHARED conversion: a cmake SHARED_LIBRARY/MODULE_LIBRARY emits its static cc_library impl PLUS a sibling cc_shared_library (real .so). Off by default — the historical static-collapse emit stays byte-identical.")
	fs.StringVar(&a.BazelPackagePath, "bazel-package-path", "", "repo-root-relative path of the destination Bazel package (e.g. \"elements/hello-world\"). Frames the emitted `# gazelle:cc_search` directives so gazelle_cc's resolver — which interprets cc_search arguments repo-root relative — picks up the same include search paths cmake recorded. Empty suppresses the directive; safer than emitting wrong bytes.")
	fs.BoolVar(&a.Verify, "verify", false, "after lowering, cross-check the IR against compile_commands.json; surface -D/-I drops and adds as stderr warnings (does not fail the run)")
	fs.StringVar(&a.VerifyReport, "verify-report", "", "write the structured verify Report (JSON) here; implies --verify")
	fs.StringVar(&a.CMakeScriptRunner, "cmake-script-runner", "", "Bazel label of a target that behaves like cmake (supports `<runner> -P <script.cmake> [-D ...]`). When set, add_custom_command(... cmake -P <script> ...) shapes lift to a genrule invoking the runner at build time. Off by default; only operators who stage the tool opt in. Soundness caveats apply: scripts with hardcoded absolute paths (configure_file-derived) won't resolve under Bazel's sandbox; parameter-driven scripts work cleanly.")
	fs.BoolVar(&a.CMakeScriptTrace, "cmake-script-trace", false, "actually run the cmake -P script under `cmake --trace --trace-format=json-v1 -P <script>` at convert time. The trace's read paths drive auto-augmentation of the genrule's srcs and a structured refusal diagnostic when the script touches paths Bazel's sandbox can't reproduce. Off by default — convert-time execution carries side-effect risk; opt in after reading docs/design/conversion-architecture.md's convert-time platform coupling note. Requires --cmake-script-runner.")
	fs.BoolVar(&a.CMakeScriptBake, "cmake-script-bake", false, "run the cmake -P script at convert time, capture the declared output bytes, and emit genrules that materialize them via base64-decode. Closes the script-hardcoded-absolute-paths gap by resolving paths at convert time. Trade-off: outputs are convert-time-baked and don't auto-refresh on upstream input change — operator re-runs convert. Same warning shape as the legacy configure_file capture (warnConvertTimeBaking post-pass picks up the cmake-codegen-cmake-script-bake tag). Off by default.")
	fs.BoolVar(&a.LiftCCEmbed, "lift-cc-embed", false, "recognize a custom command running a known file-embedding cmake -P encoder (VTK's vtkEncodeString) and lower it to the native cc_embed rule (//tools:cc-embed) — the converted project needs no cmake at build time. Faithful (the symbol name + runtime value are preserved). Off by default; requires the consuming project to stage //tools:cc-embed (like the runner). The Bazel-native end-state for the embed-file-as-C-array codegen idiom (docs/research/codegen-idiom-coverage.md).")
	fs.BoolVar(&a.LiftCCHash, "lift-cc-hash", false, "recognize a custom command running a known file-hashing cmake -P script (VTK's vtkHashSource) and lower it to the native cc_hash rule (//tools:cc-hash) — the converted project needs no cmake at build time, and the digest recomputes on input change (unlike --cmake-script-bake). Faithful (the #define name + digest are preserved). Off by default; requires the consuming project to stage //tools:cc-hash. The Bazel-native end-state for the hash-a-file-into-a-header codegen idiom (docs/research/codegen-idiom-coverage.md).")
	fs.BoolVar(&a.IgnoreRejectionsForDiagnostics, "ignore-rejections-for-diagnostics", false, "collect every Tier-1 refusal and continue past each with a local skip rather than aborting on the first one. The resulting BUILD.bazel is NOT guaranteed to build — refused constructs are silently elided. Use with --rejections-report to capture the structured rejection list. Diagnostic surveys only; production paths want the strict refusal.")
	fs.StringVar(&a.RejectionsReport, "rejections-report", "", "write the structured rejection records (JSON array) here. Only meaningful with --ignore-rejections-for-diagnostics.")
	fs.BoolVar(&a.ConversionTodos, "conversion-todos", true, "emit conversion-todos.json — the agent-actionable prompts for no-mechanical-form cmake constructs (add_test COMMAND cmake -P harnesses, filtered command edges with no Bazel analogue, install(SCRIPT)/install(CODE)). Default ON; written to --conversion-todos-report if set, else <out-build dir>/conversion-todos.json. Pass --conversion-todos=false to suppress.")
	fs.StringVar(&a.ConversionTodosReport, "conversion-todos-report", "", "explicit destination for conversion-todos.json, overriding the <out-build dir>/conversion-todos.json default. The deterministic producer; the AI post-pass that consumes it is out of scope. See the no-mechanical-form-constructs item in ROADMAP.md.")
	fs.StringVar(&a.ConversionTodosPreamble, "conversion-todos-preamble", "", "path to an operator-supplied preamble (prose, read verbatim) that replaces the built-in default in conversion-todos.json. Empty uses the built-in default (transition-to-plain-Bazel intent + brotli worked example). Only meaningful with --conversion-todos-report.")

	if err := fs.Parse(argv); err != nil {
		return a, ExitUsage
	}
	if a.VerifyReport != "" {
		a.Verify = true
	}
	// --cmake-define entries must carry a non-empty KEY (a bare "=VALUE"
	// would emit a bogus "-D=VALUE"), and must not name a cache var the
	// converter already owns through a dedicated flag: CMAKE_BUILD_TYPE /
	// CMAKE_CONFIGURATION_TYPES are driven by --build-type / --build-types
	// and routed to their own cmakerun slots, so passing them here would
	// conflict at configure time. Reject at parse time with a clean
	// ExitUsage rather than letting cmakerun fail Tier-2 downstream.
	for _, d := range a.CmakeDefines {
		key, _, _ := strings.Cut(d, "=")
		// cmake also accepts a typed cache entry, "KEY:TYPE=VALUE" — compare the
		// bare KEY (sans :TYPE) against the reserved set so a typed
		// CMAKE_BUILD_TYPE:STRING can't slip the reservation.
		bareKey, _, _ := strings.Cut(key, ":")
		reservedWhy, reserved := reservedCmakeDefine[bareKey]
		switch {
		case key == "":
			fmt.Fprintf(stderr, "convert-element-cmake: malformed --cmake-define %q: expected KEY[=VALUE] with a non-empty KEY\n", d)
			return a, ExitUsage
		case strings.HasPrefix(key, "-"):
			// The converter prepends the -D itself; a copy-pasted cmake-CLI
			// "-DKEY=VALUE" here would emit "-D-DKEY=VALUE". Pass KEY=VALUE.
			fmt.Fprintf(stderr, "convert-element-cmake: malformed --cmake-define %q: pass KEY[=VALUE] without a leading -D (the converter adds it)\n", d)
			return a, ExitUsage
		case strings.ContainsAny(key, " \t\r\n\f\v"):
			// A space in the KEY (e.g. "CMAKE_CXX_FLAGS =-w") would set the
			// wrong cache variable name or emit an unparseable -D argument.
			fmt.Fprintf(stderr, "convert-element-cmake: malformed --cmake-define %q: KEY must not contain whitespace\n", d)
			return a, ExitUsage
		case reserved:
			fmt.Fprintf(stderr, "convert-element-cmake: --cmake-define %s is reserved — %s\n", bareKey, reservedWhy)
			return a, ExitUsage
		}
	}
	// Entry-point selection: exactly one of SourceRoot,
	// ReplyDir, or CMakeBuildDir must be set. CMakeBuildDir is
	// pure syntactic sugar — Parse derives ReplyDir from it so
	// downstream code (main.go) only ever reads ReplyDir.
	switch {
	case a.SourceRoot == "" && a.ReplyDir == "" && a.CMakeBuildDir == "":
		fmt.Fprintln(stderr, "convert-element-cmake: must set --source-root, --reply-dir, or --cmake-build-dir")
		fs.Usage()
		return a, ExitUsage
	case a.SourceRoot != "" && (a.ReplyDir != "" || a.CMakeBuildDir != ""):
		fmt.Fprintln(stderr, "convert-element-cmake: --source-root is incompatible with --reply-dir / --cmake-build-dir (the converter either runs cmake or reuses an existing build, not both)")
		return a, ExitUsage
	case a.ReplyDir != "" && a.CMakeBuildDir != "":
		fmt.Fprintln(stderr, "convert-element-cmake: --reply-dir and --cmake-build-dir are aliases; set one, not both")
		return a, ExitUsage
	case a.CMakeBuildDir != "":
		a.ReplyDir = filepath.Join(a.CMakeBuildDir, ".cmake", "api", "v1", "reply")
	}
	// Operator-facing dials: validate the enums, then map to the
	// existing per-kind switches. Explicit per-kind flags
	// (--unsupported-execute-process-fallback,
	// --ignore-rejections-for-diagnostics) stay as escape hatches —
	// when set true they override the dial-derived default; an
	// explicit "false" still wins because Go's flag.BoolVar
	// short-circuits the derivation only when an explicit value
	// matches the derived one. Pragmatic: best-effort + explicit
	// --unsupported-...=false is a contradiction the operator
	// should not write, and we don't try to detect it here.
	fidelity, err := convmode.ParseFidelity(a.Fidelity)
	if err != nil {
		fmt.Fprintln(stderr, "convert-element-cmake: "+err.Error())
		return a, ExitUsage
	}
	a.Fidelity = string(fidelity)
	if fidelity == convmode.FidelityBestEffort {
		a.UnsupportedExecuteProcessFallback = true
	}
	// --fidelity=strict implies --strict-trace: the strict-refusal
	// dial values "I want every undefined / degraded shape to
	// fail loudly", which includes missing cmake trace data
	// (Lower's PUBLIC/PRIVATE recovery, IMPORTED-target dep
	// recovery, etc. all degrade silently without trace). Operator
	// can pass --strict-trace=false explicitly to keep the
	// degrade-and-warn shape under strict fidelity (e.g. offline
	// replay flows where trace data isn't available); the explicit
	// override wins. Detection uses fs.Visit because Go's flag
	// package doesn't track explicit-vs-default for bool flags any
	// other way.
	explicit := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { explicit[f.Name] = true })
	if fidelity == convmode.FidelityStrict && !explicit["strict-trace"] {
		a.StrictTrace = true
	}
	if _, err := convmode.ParseBakeIn(a.BakeIn); err != nil {
		fmt.Fprintln(stderr, "convert-element-cmake: "+err.Error())
		return a, ExitUsage
	}
	if a.Diagnostics {
		// --diagnostics is "I'm surveying this codebase; surface
		// everything you know how to surface, and don't abort on
		// the first refusal". That collects rejections
		// (--ignore-rejections-for-diagnostics), reports distro-
		// hardening drift (--probe-distro-hardening), and cross-
		// checks the IR against compile_commands (--verify). Each
		// is independently silenceable via an explicit
		// --<flag>=false override; the dial only sets unset
		// defaults.
		a.IgnoreRejectionsForDiagnostics = true
		if !explicit["probe-distro-hardening"] {
			a.ProbeDistroHardening = true
		}
		if !explicit["verify"] {
			a.Verify = true
		}
	}
	return a, ExitSuccess
}

// LookEnv is a tiny indirection so tests can inject env without touching
// process state.
type LookEnv func(string) (string, bool)

// OSLookEnv is the production env reader.
var OSLookEnv LookEnv = func(k string) (string, bool) { return os.LookupEnv(k) }

// repeatedString adapts a *[]string to flag.Value, appending one
// entry per flag occurrence (`--foo a --foo b` → []string{"a","b"}).
// Used for path-valued flags where comma-splitting would be wrong.
type repeatedString struct{ p *[]string }

func (r repeatedString) String() string {
	if r.p == nil {
		return ""
	}
	return strings.Join(*r.p, " ")
}

func (r repeatedString) Set(v string) error {
	if v != "" {
		*r.p = append(*r.p, v)
	}
	return nil
}

// commaSlice adapts a *[]string to flag.Value so the CLI can take
// `--foo=a,b,c` (single repeat) and surface as `[]string{"a","b","c"}`.
// Empty strings between commas are dropped — `--foo=,a,,b,` → `[a, b]`.
type commaSlice struct{ p *[]string }

func (c commaSlice) String() string {
	if c.p == nil {
		return ""
	}
	return strings.Join(*c.p, ",")
}

func (c commaSlice) Set(v string) error {
	if v == "" {
		*c.p = nil
		return nil
	}
	var out []string
	for _, part := range strings.Split(v, ",") {
		if part != "" {
			out = append(out, part)
		}
	}
	*c.p = out
	return nil
}
