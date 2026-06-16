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
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/sstriker/buildstream-bazel/converter/internal/cmakerun"
	"github.com/sstriker/buildstream-bazel/converter/internal/coverage"
	"github.com/sstriker/buildstream-bazel/converter/internal/ctest"
	"github.com/sstriker/buildstream-bazel/converter/internal/failure"
	"github.com/sstriker/buildstream-bazel/converter/internal/fileapi"
	"github.com/sstriker/buildstream-bazel/converter/internal/ninja"
	"github.com/sstriker/buildstream-bazel/converter/internal/rejection"
	"github.com/sstriker/buildstream-bazel/converter/internal/todos"
	"github.com/sstriker/buildstream-bazel/converter/ir"
	"github.com/sstriker/buildstream-bazel/internal/cclang"
	"github.com/sstriker/buildstream-bazel/internal/convmode"
	"github.com/sstriker/buildstream-bazel/internal/genexeval"
	"github.com/sstriker/buildstream-bazel/internal/manifest"
	"github.com/sstriker/buildstream-bazel/internal/shadow"
	"github.com/sstriker/buildstream-bazel/internal/sliceutil"
)

// Options controls behavior that the orchestrator (M3) overrides per-package.
// M1 callers can pass the zero value.
type Options struct {
	// HostSourceRoot is the on-disk path to the source tree, used for
	// filesystem walks (e.g. header discovery under each include directory).
	// Defaults to the source path recorded in the File API codemodel.
	HostSourceRoot string

	// ElementSourceRoot, when set, FORCES the label-relativization root (the
	// "workspace root" labels anchor against) to this directory — an explicit
	// override of the auto-detected/escape-gated workspace root. Use it when the
	// element is OVERLAID at a directory ABOVE the cmake source root and cmake is
	// configured at a subdir, so the per-sample sources/includes still need the
	// subdir-relative-to-overlay prefix even though no source "escapes" cmakeSrc.
	// cuda-samples is the case: each sample is its own `project()` (cmake
	// configures there, dodging the whole-tree `9_CUDA_Tile`/tileiras blocker),
	// but the build-lens overlays the whole repo (so the repo-root `Common/`
	// headers stage), so labels must anchor at the repo root. Must be an ancestor
	// of (or equal to) cmakeSrc. Empty = auto-detect (the default behavior).
	ElementSourceRoot string

	// EmitInstallExportConfig opts in to generating the install(EXPORT)
	// config-mode bundle — the real <Pkg>Targets.cmake + the cmake_config_bundle
	// filegroup (exportshape.EmitInputs.EmitConfig). OFF by default: the
	// orchestrated graph wires the synthprefix-synthesized bundle, so the
	// converter's standalone bundle is unused and emitting it would only break
	// `bazel build //...`. A project shipping the element for EXTERNAL cmake
	// config-mode consumption opts in (--emit-install-export-config).
	EmitInstallExportConfig bool

	// EmitSharedLibraries opts in to faithful SHARED conversion: a cmake
	// SHARED_LIBRARY/MODULE_LIBRARY emits its static cc_library impl PLUS a
	// sibling cc_shared_library (real .so). OFF by default — the historical
	// static-collapse emit stays byte-identical. See ROADMAP's cc_shared_library
	// item.
	EmitSharedLibraries bool

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

	// WorkspaceStatusSink, when non-nil, receives the workspace-status key
	// -> producing shell command map for every stamp key an emitted
	// configure_file / file(WRITE) actually reads at build time (status key
	// -> e.g. `git rev-parse HEAD`). The CLI serializes it to the
	// --out-workspace-status helper script so an operator gets a ready
	// --workspace_status_command emitting exactly the keys this project's
	// stamped templates need under --stamp, instead of reverse-engineering
	// them. Reset + repopulated each pass like StampVarSink. Empty when no
	// stamped template was emitted.
	WorkspaceStatusSink map[string]string

	// NestedConfigureSink, when non-nil, receives the nested cmake
	// builds pass 1 detected (outer-build-relative nested build dir →
	// trace-recorded nested source dir). The driver reads it after
	// pass 1 to stage File API queries into each nested build dir and
	// run the warm second pass; see nested_cmake.go.
	NestedConfigureSink map[string]string

	// NestedBuilds is the warm second pass's harvest: each detected
	// nested build's File API reply + ninja graph, recursively lowered
	// and merged into the outer package by lowerNestedBuilds.
	NestedBuilds []NestedBuildInput

	// CaptureRefusalSink, when non-nil, receives the variable names of
	// every capture-bearing execute_process refusal (OUTPUT_VARIABLE /
	// ERROR_VARIABLE / RESULT_VARIABLE / RESULTS_VARIABLE). The driver
	// reads it after pass 1 to run the dead-capture analysis (a warm
	// non-expanded trace pass) and re-lower with DeadCaptureVars; see
	// execute_process_dead_capture.go.
	CaptureRefusalSink map[string]bool

	// DeadCaptureVars are capture variables the dead-capture analysis
	// proved the configure never reads (captured only to silence
	// output). clearDeadCaptures treats their capture keywords as
	// absent, unlocking the lifts the keyword's presence refused.
	DeadCaptureVars map[string]bool

	// NonExpandedFileWriters are file(WRITE/APPEND) calls harvested from
	// the warm pass's NON-EXPANDED trace (where `${GIT_SHA}` survives
	// verbatim). The lower builds cc.FileWriterTemplates from them so
	// the writer index can route a stamp-bearing file(WRITE) through the
	// configure_file stamp_values machinery instead of baking the frozen
	// revision. Empty when no warm non-expanded pass ran.
	NonExpandedFileWriters []shadow.FileWriterCall

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

	// LiftDownload toggles the file(DOWNLOAD) recovery's lifted
	// shape. When true, a recovered build-dir download becomes a
	// genrule copying @<repo>//file from an http_file repository
	// rule (the producer is rewired; consumers are unaffected,
	// since the fetch genrule produces the same path the byte-bake
	// would have). When false (the default), the hermetic byte-bake
	// stands. Independent of the lockfile emission below: downloads
	// are recorded for DownloadRepos regardless. See
	// download_lift.go.
	LiftDownload bool

	// DownloadRepos, when non-nil, collects one DownloadRepoSpec per
	// recovered file(DOWNLOAD) — the lockfile the CLI serializes to
	// --out-download-repos. Populated at end of lower from the same
	// records the `download` todo draws on; mirrors the Todos /
	// Rejections / Coverage collector pattern.
	DownloadRepos *[]DownloadRepoSpec

	// RecognizeCodegen opts into the codegen-recognizer registry: a recovered
	// codegen custom-command that a recognizer claims (protoc --cpp_out today)
	// lowers to the idiomatic native rule (proto_library + cc_proto_library)
	// instead of a genrule. Off by default — additive opt-in while the
	// consumer-dep rewiring + corpus validation land; a non-standard invocation
	// (output cross-check mismatch) falls back to the genrule, never regressing.
	RecognizeCodegen bool

	// ExtraCodegenRecognizers are operator-supplied recognizers (loaded from
	// --recognizers Starlark files by the CLI via LoadStarlarkRecognizers),
	// appended to the built-in registry after the first-party ones. Only
	// consulted when RecognizeCodegen is set.
	ExtraCodegenRecognizers []CodegenRecognizer

	// LiftDerivedCodegen opts the derived-name stem-match execute_process
	// recovery into a live genrule re-run instead of the convert-time byte-bake
	// (--lift-derived-codegen). Off by default; a non-liftable shape falls back
	// to the bake, so existing converts are unchanged.
	LiftDerivedCodegen bool

	// Fidelity is the operator dial ("strict" / "best-effort", convmode
	// vocabulary) governing how an unfaithful recovery is handled. Today it
	// gates the recognizer's output cross-check: a matched-but-non-standard
	// claim (derived outputs disagree with cmake's recorded ones) REFUSES under
	// strict (a loud build-time stub) and FALLS BACK to the generic genrule
	// under best-effort. The lower-package ZERO VALUE "" is treated as
	// best-effort (a direct caller that leaves it unset gets the fall-back); the
	// PRODUCT default is strict — the CLI canonicalizes "" → strict
	// (ParseFidelity, flags.go) before threading it here. See
	// docs/design/codegen-fidelity-ladder.md.
	Fidelity convmode.Fidelity

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

	// Todos, when non-nil, collects "no-mechanical-form" constructs —
	// ones with a good Bazel form but no faithful mechanical
	// translation, which an author (or AI post-pass) must re-express:
	// add_test(COMMAND cmake -P …) script harnesses, filtered cmake
	// command edges with no Bazel analogue, and install(SCRIPT) /
	// install(CODE) directives. Each producer site Adds one grouped
	// entry alongside its (retained) stderr warning. Surfaced via
	// --conversion-todos-report; see converter/internal/todos.
	Todos *todos.Collector

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

	// RecoverSourceComments enables comment-carrying recovery: ToIR reads
	// raw cmake source at each target's declaration site (the File API
	// carries no comments) and populates ir.Target.LeadingComment plus the
	// package HeaderComments file-header block, for the emitter to render
	// under emit.Options.EmitSourceComments. Off by default — it reads
	// source files and changes IR; the CLI sets both from one
	// `--emit-source-comments` flag.
	RecoverSourceComments bool

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
// hostPrefix→anchor rewrite. The canonical home is the manifest
// CONTRACT itself (manifest.PrefixAnchor); this aliases it.
const ManifestPrefixAnchor = manifest.PrefixAnchor

// ccLinkableSrcExts are non-header extensions that are still valid `srcs`
// entries for a cc_library/cc_binary — precompiled objects/archives and
// assembly that Bazel links or assembles. A GENERATED output with one of these
// extensions stays in srcs; any other non-header generated output (VTK's
// hierarchy .args/.data, etc.) is routed to `data` since cc rules reject a
// non-source srcs entry.
var ccLinkableSrcExts = map[string]bool{
	".o":   true,
	".obj": true,
	".a":   true,
	".lo":  true,
	".s":   true,
	".S":   true,
	".asm": true,
}

// compilableSrcExts are the source extensions Bazel's cc rules COMPILE.
var compilableSrcExts = map[string]bool{
	".c": true, ".cc": true, ".cpp": true, ".cxx": true, ".c++": true,
	".cu": true, ".cl": true, ".cppm": true, ".ixx": true,
	".m": true, ".mm": true,
}

// isCcSrcEntry reports whether path p is a valid cc_library/cc_binary `srcs`
// entry — a compiled source, a header, or a linkable object/archive/assembly.
// A GENERATED target-source that is none of these (VTK lists each module's
// wrap-hierarchy artifacts — CMakeFiles/<mod>-hierarchy.*.args / .data — as the
// module target's cmake sources for build ordering) must not enter cc srcs, or
// Bazel fails analysis with "does not produce any cc_library srcs files".
func isCcSrcEntry(p string) bool {
	ext := strings.ToLower(filepath.Ext(p))
	return cclang.IsHeaderExt(ext) || compilableSrcExts[ext] || ccLinkableSrcExts[ext]
}

// classifyAndAttach routes a generated / consumer-attributed output PATH into
// the correct cc attribute on irt, records it in seen for dedup, and returns
// true iff it attached the path. It is the single source-classification
// chokepoint the consumer-attribution sites share: the file(GENERATE) and
// execute_process attribution blocks historically open-coded the identical
// "header→hdrs / non-cc→data / cc→srcs" routing, and the VTK wrap-hierarchy
// `.args/.data` fix had to be repeated across them — a "same fix in N places"
// smell. Centralizing it means a new non-cc / odd-extension shape is handled
// once. (See ROADMAP.md's source-classification consolidation item; the
// CompileGroup IsGenerated branch carries extra concerns — CcEmbed header
// pairing, compile-group priority — so it keeps its own switch but shares the
// dropNonCc disposition documented below.)
//
// Routing:
//   - a header extension → Hdrs (exposed to consumers);
//   - a non-cc input (isCcSrcEntry false: a `.args` response file, `.data` blob,
//     `.json` manifest, …): dropNonCc=false routes it to Data (preserves the
//     build-order association without the cc-srcs requirement — what the
//     consumer-attribution blocks need); dropNonCc=true drops it (the
//     cross-package-safe behavior the CompileGroup site needs, where a bare-path
//     data entry can't carry a cross-package ref);
//   - otherwise a cc compile/link input → Srcs.
//
// seen dedups within one attribution pass (a path has one extension, so it lands
// in exactly one attribute). It does NOT dedup against entries irt already
// carried before the pass — matching the open-coded blocks it replaces.
func classifyAndAttach(irt *ir.Target, path string, seen map[string]bool, dropNonCc bool) bool {
	if seen[path] {
		return false
	}
	seen[path] = true
	ext := strings.ToLower(filepath.Ext(path))
	switch {
	case cclang.IsHeaderExt(ext):
		irt.Hdrs = append(irt.Hdrs, path)
	case !isCcSrcEntry(path):
		if dropNonCc {
			return false
		}
		irt.Data = append(irt.Data, path)
	default:
		irt.Srcs = append(irt.Srcs, path)
	}
	return true
}

// isGeneratedOutputRoot reports whether dir is the leading directory component
// of any generated output (an OutToGenrule key) — i.e. genrule outputs live
// under dir. Used to surface a build-dir include (cmake's `-I<build>/<dir>`,
// e.g. grpc's protoc `gens/`) on a consumer's `includes` so a generated source's
// full-path `#include "..."` of a sibling generated header resolves.
func isGeneratedOutputRoot(dir string, outToGenrule map[string]string) bool {
	if dir == "" || len(outToGenrule) == 0 {
		return false
	}
	prefix := dir + "/"
	for out := range outToGenrule {
		if strings.HasPrefix(out, prefix) {
			return true
		}
	}
	return false
}

// attachGeneratedSource routes a GENERATED target-source PATH into the right cc
// attribute, the second source-classification chokepoint (sister to
// classifyAndAttach). Where classifyAndAttach serves the consumer-attribution
// blocks (match-an-include then route), this serves the per-source IsGenerated /
// on-disk-generated / OutToGenrule branches in the main lowerTarget walk, which
// share a compile-group-aware shape that classifyAndAttach's plain ext routing
// doesn't capture:
//
//   - inCG (the source is in one of cmake's compile groups → a COMPILED source):
//     it goes to Srcs, and a cc_embed lift's generated `.cxx` also contributes
//     its sibling header (embedHdr, from cc.CcEmbedSourceToHeader) to Hdrs so a
//     target compiling the source — and any same-package source that #includes
//     the header — resolves it as a declared input;
//   - a header extension → Hdrs;
//   - otherwise → Srcs (a compilable source not in a group, or a linkable
//     object/archive/assembly).
//
// dropNonCc=true drops a non-cc input (a `.args`/`.data` build-order artifact)
// instead of letting it fall through to Srcs — the IsGenerated branch's
// cross-package-safe behavior, where a non-cc srcs entry fails cc analysis and a
// bare-path data entry can't carry a cross-package ref. The compile-group test
// is `inCG && !header`: a compile group only ever holds compiled sources, so the
// header guard is defensive (an inCG header still routes to Hdrs). embedHdr ""
// means no cc_embed pairing.
// Returns the path appended to irt.Srcs (so the caller can record the
// codemodel-source-index → emitted-src-path mapping splitCompileGroups
// needs to partition generated sources by compile group), or "" when the
// input went to Hdrs or was dropped.
func attachGeneratedSource(irt *ir.Target, path string, inCG, dropNonCc bool, embedHdr string) string {
	ext := strings.ToLower(filepath.Ext(path))
	switch {
	case dropNonCc && !isCcSrcEntry(path):
		return ""
	case inCG && !cclang.IsHeaderExt(ext):
		irt.Srcs = append(irt.Srcs, path)
		if embedHdr != "" {
			irt.Hdrs = append(irt.Hdrs, embedHdr)
		}
		return path
	case cclang.IsHeaderExt(ext):
		irt.Hdrs = append(irt.Hdrs, path)
		return ""
	default:
		irt.Srcs = append(irt.Srcs, path)
		return path
	}
}

// stageSiblingGeneratedHeaders stages sibling generated headers (path-independent
// post-pass). A code generator routinely emits a .c PLUS a sibling .h the .c
// #includes by bare same-dir quote (libevent's regress.gen.c → regress.gen.h),
// but cmake omits the generated header from the consuming target's source list,
// so the .c's compile can't find it. This maps each genrule SOURCE output to the
// genrule's HEADER outputs, then for every cc target attaches the sibling
// headers of any genrule source the target compiles — regardless of which
// attribution path put the .c there.
//
// The sibling header is added to SRCS (not hdrs): a header in srcs is a valid
// private-header input for EVERY cc rule kind (cc_test / cc_binary have no hdrs
// attribute), and the generated header is genuinely an implementation detail of
// the generated .c. Referenced by element-relative path (resolving to the
// producing genrule's output); split's srcs relabel re-homes it as needed.
//
// Staging the header as an input is necessary but NOT sufficient: the .c
// #includes it by BARE same-dir quote (`#include "regress.gen.h"`), which cmake
// resolved because the custom command writes the header into the SOURCE dir
// next to the .c. Under Bazel the genrule output lands in genfiles, so the
// compiler's implicit same-dir search (which only walks the .c's SOURCE dir)
// misses it. So we also add the header's directory to the consuming target's
// Includes — Bazel's `includes` puts BOTH the source-tree and the genfiles
// variant of that dir on the search path, and the genfiles one is where the
// bare include now resolves. Under --split-packages this include dir becomes a
// normal include root: planSplit synthesizes its header lib (which collects the
// generated header and carries includes=["."]) and the consumer deps on it, so
// the genfiles -I propagates without any special-casing in the split transform.
func stageSiblingGeneratedHeaders(pkg *ir.Package) {
	isHdr := func(p string) bool { return cclang.IsHeaderExt(strings.ToLower(filepath.Ext(p))) }
	sib := map[string][]string{} // genrule source-output -> sibling header outputs
	for _, t := range pkg.Targets {
		if t.Kind != ir.KindGenrule {
			continue
		}
		var hdrs []string
		for _, o := range t.GenruleOuts {
			if isHdr(o) {
				hdrs = append(hdrs, o)
			}
		}
		if len(hdrs) == 0 {
			continue
		}
		for _, o := range t.GenruleOuts {
			if !isHdr(o) {
				sib[o] = hdrs
			}
		}
	}
	if len(sib) == 0 {
		return
	}
	for i := range pkg.Targets {
		t := &pkg.Targets[i]
		switch t.Kind {
		case ir.KindCCLibrary, ir.KindCCBinary, ir.KindCCInterface, ir.KindCCTest:
			stageSiblingHeadersForTarget(t, sib)
		default:
			continue
		}
	}
}

// stageSiblingHeadersForTarget attaches the sibling generated headers of every
// genrule SOURCE output this target compiles (mapped by `sib`) and surfaces
// each header's genfiles directory on the target's include path. Split out of
// stageSiblingGeneratedHeaders to keep that pass under the cognitive-complexity
// gate; see the caller for the why.
func stageSiblingHeadersForTarget(t *ir.Target, sib map[string][]string) {
	have := make(map[string]bool, len(t.Srcs))
	for _, s := range t.Srcs {
		have[s] = true
	}
	var add []string
	var surfaceDirs []string
	seenDir := map[string]bool{}
	consider := func(srcs []string) {
		for _, s := range srcs {
			for _, h := range sib[s] {
				// Record the sibling header's directory REGARDLESS of whether the
				// header is already staged in srcs: the genfiles -I is what makes
				// the .c's bare same-dir `#include "<gen>.h"` resolve, and an
				// earlier attribution path that already put the header in srcs
				// (e.g. a project that lists its generated header in the
				// add_library source list for IDE/moc visibility) need NOT have
				// surfaced its genfiles dir. Decoupling the dir from the
				// "newly-staged" gate mirrors the same-dir configure_file pass,
				// which had the same pre-emption gap (VTK's kwsysPrivate.h was
				// added to hdrs by an earlier pass, so its dir was skipped).
				dir := filepath.Dir(h)
				if dir == "" {
					dir = "."
				}
				if !seenDir[dir] {
					seenDir[dir] = true
					surfaceDirs = append(surfaceDirs, dir)
				}
				if !have[h] {
					add = append(add, h)
					have[h] = true
				}
			}
		}
	}
	consider(t.Srcs)
	// Multi-config targets carry per-config srcs in select() arms (the
	// generated .c can be config-divergent — libevent's regress.gen.c); scan
	// those too. The sibling header goes to flat srcs (a declared input is
	// config-invariant), staged for every arm.
	for _, arm := range t.PerPlatform["srcs"] {
		consider(arm)
	}
	t.Srcs = append(t.Srcs, add...)
	// Put each sibling header's directory on the target's include path so the
	// .c's bare same-dir `#include "<gen>.h"` resolves against the genfiles copy
	// (the staged srcs entry alone doesn't add the -I). surfaceDirs is built in
	// srcs/sib order for deterministic output and deduped against the includes
	// the target already carries.
	haveInc := make(map[string]bool, len(t.Includes))
	for _, inc := range t.Includes {
		haveInc[inc] = true
	}
	for _, dir := range surfaceDirs {
		if !haveInc[dir] {
			t.Includes = append(t.Includes, dir)
			haveInc[dir] = true
		}
	}
}

// wireDefineDrivenGeneratedHeaders connects a target to a generated header it
// pulls in via a compile DEFINE rather than a literal #include, which the
// converter's include scan can't see. VTK's module machinery
// (vtkModule.cmake) does exactly this: each implementing module gets
// `target_compile_definitions(<Mod>_AUTOINIT_INCLUDE="vtkModuleAutoInit_<hash>.h")`
// and its sources do `#ifdef <mod>_AUTOINIT_INCLUDE / #include
// <mod>_AUTOINIT_INCLUDE` — so the (already-generated, by a genrule) per-module
// auto-init header is never wired into the consumer and every such TU fails with
// "vtkModuleAutoInit_<hash>.h: No such file".
//
// The header is included by BASENAME and the genrule emits it under CMakeFiles/,
// so we synthesize one public wrapper cc_library carrying every such header with
// `includes=["CMakeFiles"]` (which propagates the -I to dependents so the
// basename resolves) and add it to the `deps` of each target whose defines name
// one. The wrapper lives in the root package (no SubPackages entry); split
// relabels the intra-element `:`-dep and the public wrapper is reachable
// cross-package.
func wireDefineDrivenGeneratedHeaders(pkg *ir.Package) {
	const suffix = "_AUTOINIT_INCLUDE"
	// Generated-header basename → its genrule output path (package-relative).
	outByBase := map[string]string{}
	for _, t := range pkg.Targets {
		if t.Kind != ir.KindGenrule {
			continue
		}
		for _, o := range t.GenruleOuts {
			outByBase[filepath.Base(o)] = o
		}
	}
	neededOuts := map[string]bool{}
	consumers := map[int]bool{}
	for i := range pkg.Targets {
		t := &pkg.Targets[i]
		if t.Kind != ir.KindCCLibrary && t.Kind != ir.KindCCBinary && t.Kind != ir.KindCCTest {
			continue
		}
		defs := append(append([]string{}, t.Defines...), t.LocalDefines...)
		for _, d := range defs {
			eq := strings.IndexByte(d, '=')
			if eq < 0 || !strings.HasSuffix(d[:eq], suffix) {
				continue
			}
			base := filepath.Base(strings.Trim(d[eq+1:], `"`))
			if out, ok := outByBase[base]; ok {
				neededOuts[out] = true
				consumers[i] = true
			}
		}
	}
	if len(neededOuts) == 0 {
		return
	}
	hdrs := make([]string, 0, len(neededOuts))
	incs := map[string]bool{}
	for o := range neededOuts {
		hdrs = append(hdrs, o)
		// Propagate -I<dir-of-header> so the BASENAME include resolves for
		// dependents. dir == "." (header at the package root) is the common case
		// (the auto-init basename genrule outputs there) and MUST be included as
		// "." — otherwise the root-package header is only includable at its
		// repo-root path, not by basename, and the consumer still can't find it.
		d := filepath.Dir(o)
		if d == "" {
			d = "."
		}
		incs[d] = true
	}
	sort.Strings(hdrs)
	includes := sliceutil.SortedKeys(incs)
	const wrapperName = "define_driven_generated_headers"
	pkg.Targets = append(pkg.Targets, ir.Target{
		Name:       wrapperName,
		Kind:       ir.KindCCLibrary,
		Hdrs:       hdrs,
		Includes:   includes, // propagate -I<dir> so the BASENAME include resolves
		Visibility: publicVisibility(),
		Tags:       []string{"cmake-define-driven-generated-headers"},
	})
	for i := range pkg.Targets {
		if consumers[i] {
			pkg.Targets[i].Deps = append(pkg.Targets[i].Deps, ":"+wrapperName)
		}
	}
}

// ToIR lowers a parsed reply into a Package. The optional ninja graph
// enables genrule recovery for targets with isGenerated sources; pass nil to
// disable (M1-style behavior — generated sources then trigger
// unsupported-custom-command).
// buildPrivateIncludeDirs collects the PRIVATE include directories per target
// from the decoded target_include_directories trace calls.
func buildPrivateIncludeDirs(includes []shadow.TargetIncludeCall) map[string]map[string]bool {
	out := map[string]map[string]bool{}
	for _, call := range includes {
		for _, grp := range call.Groups {
			if grp.Visibility != "PRIVATE" {
				continue
			}
			if _, ok := out[call.Target]; !ok {
				out[call.Target] = map[string]bool{}
			}
			for _, dir := range grp.Dirs {
				out[call.Target][dir] = true
			}
		}
	}
	return out
}

// buildPublicIncludeDirs collects the PUBLIC/INTERFACE include directories per
// target from the decoded target_include_directories trace calls — the dirs a
// target genuinely re-exports to consumers. Used to GUARD the directory-scoped
// include_directories() privatization (buildIncludeDirsGlobal): a dir a target
// also declares PUBLIC propagates, so it must stay on the transitive `includes`
// even if the same path appears in an include_directories() elsewhere.
func buildPublicIncludeDirs(includes []shadow.TargetIncludeCall) map[string]map[string]bool {
	out := map[string]map[string]bool{}
	for _, call := range includes {
		for _, grp := range call.Groups {
			if grp.Visibility != "PUBLIC" && grp.Visibility != "INTERFACE" {
				continue
			}
			if _, ok := out[call.Target]; !ok {
				out[call.Target] = map[string]bool{}
			}
			for _, dir := range grp.Dirs {
				out[call.Target][dir] = true
			}
		}
	}
	return out
}

// buildIncludeDirsGlobal collects the directory-scoped include_directories()
// dirs as a GLOBAL set of absolute paths, resolving a relative arg against the
// calling CMakeLists' directory (CMAKE_CURRENT_SOURCE_DIR). These dirs are
// PRIVATE (cmake never exports them via INTERFACE_INCLUDE_DIRECTORIES). The set
// is global, but it only privatizes an include on a target that ACTUALLY has
// that path in its codemodel include list (include_directories applied to it) —
// a target in an unrelated directory never carries the path, so the global set
// can't reach it. A target that re-exports the same dir PUBLIC is excluded at
// the use site via buildPublicIncludeDirs.
func buildIncludeDirsGlobal(calls []shadow.IncludeDirectoriesCall) map[string]bool {
	out := map[string]bool{}
	for _, call := range calls {
		base := filepath.Dir(call.File)
		for _, dir := range call.Dirs {
			d := dir
			if !filepath.IsAbs(d) {
				d = filepath.Join(base, d)
			}
			out[filepath.Clean(d)] = true
		}
	}
	return out
}

// parseInterfaceIncludeDirs extracts the absolute BUILD-tree include dirs a
// target EXPORTS from its (genex-intact) INTERFACE_INCLUDE_DIRECTORIES — the
// public whitelist for the interface-driven include scope (B1). cmake records
// an exported include as `$<BUILD_INTERFACE:/abs/dir>` (the build-tree form;
// `$<INSTALL_INTERFACE:relpath>` is the installed-tree form, irrelevant to the
// in-tree compile and skipped) or, rarely, a bare absolute path. Entries with
// any other unresolved genex are skipped defensively.
func parseInterfaceIncludeDirs(joined string) map[string]bool {
	out := map[string]bool{}
	for _, raw := range strings.Split(joined, ";") {
		e := strings.TrimSpace(raw)
		if e == "" {
			continue
		}
		const bi = "$<BUILD_INTERFACE:"
		if strings.HasPrefix(e, bi) && strings.HasSuffix(e, ">") {
			inner := e[len(bi) : len(e)-1]
			if !strings.Contains(inner, "$<") && filepath.IsAbs(inner) {
				out[filepath.Clean(inner)] = true
			}
			continue
		}
		if strings.Contains(e, "$<") {
			continue
		}
		if filepath.IsAbs(e) {
			out[filepath.Clean(e)] = true
		}
	}
	return out
}

// interfaceIncludeScope is the per-target interface-driven include whitelist
// (B1). public is the set of absolute BUILD-tree include dirs the target EXPORTS
// (INTERFACE_INCLUDE_DIRECTORIES); active is true only when the genex PROBE ran
// AND captured this target — only then is "in the target's resolved includes
// but NOT exported ⇒ PRIVATE" a trustworthy signal. Without the probe (active
// false) the include partition keeps its trace-only behavior, byte-identical.
type interfaceIncludeScope struct {
	public map[string]bool
	active bool
}

// interfaceIncludeDirsFor builds the per-target interface-include whitelist from
// the genex targets, gated on the probe having run.
func interfaceIncludeDirsFor(genex map[string]genexeval.TargetInfo, name string, probed bool) interfaceIncludeScope {
	if !probed {
		return interfaceIncludeScope{}
	}
	ti, ok := genex[name]
	if !ok {
		return interfaceIncludeScope{}
	}
	return interfaceIncludeScope{public: parseInterfaceIncludeDirs(ti.InterfaceIncludeDirectories), active: true}
}

// buildTraceLinkInfo collects, per target, the ordered link libraries and each
// library's link-scope keyword from the decoded target_link_libraries trace
// calls. Libraries are deduped WITHIN each call (the `seen` set is per
// TargetLinkCall); a library named in two separate target_link_libraries() calls
// for the same target is kept in both, preserving the recorded call order.
// Scope, by contrast, is first-write-wins ACROSS all of a target's calls (the
// per-target scope map persists), so an earlier PUBLIC arm isn't overwritten by
// a later PRIVATE one for the same library — cmake's own semantics for a
// doubly-listed library with differing keywords are undefined, but the
// upstream-most call governs header propagation in the typical case.
func buildTraceLinkInfo(links []shadow.TargetLinkCall) (map[string][]string, map[string]map[string]string) {
	traceLinkLibs := map[string][]string{}
	traceLinkScope := map[string]map[string]string{}
	for _, call := range links {
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
				if _, ok := traceLinkScope[call.Target][lib]; !ok {
					traceLinkScope[call.Target][lib] = grp.Visibility
				}
			}
		}
	}
	return traceLinkLibs, traceLinkScope
}

// buildPlatformConditionalSrcs maps each (target, source) recovered from
// platform-conditional if-blocks to its select key. First-write-wins: if the
// same (target, src) shows up under two conditionals (rare — nested elseif arms
// adding the same source on different platforms), the first SelectKey governs.
func buildPlatformConditionalSrcs(pcsList []shadow.PlatformConditionalSource) map[string]map[string]string {
	out := map[string]map[string]string{}
	for _, pcs := range pcsList {
		if _, ok := out[pcs.Target]; !ok {
			out[pcs.Target] = map[string]string{}
		}
		if _, ok := out[pcs.Target][pcs.Source]; !ok {
			out[pcs.Target][pcs.Source] = pcs.SelectKey
		}
	}
	return out
}

// traceFacts bundles the per-target maps the single trace pre-parse
// produces for the target-lowering loop and the post-passes. Fields keep
// ToIR's original local names; ToIR destructures them back into
// same-named locals so the consuming code reads exactly as before.
type traceFacts struct {
	knownTargets                 map[string]bool
	privateIncludeDirs           map[string]map[string]bool
	publicIncludeDirs            map[string]map[string]bool
	includeDirsGlobal            map[string]bool
	defineSymbols                map[string]string
	traceLinkLibs                map[string][]string
	traceLinkScope               map[string]map[string]string
	platformConditionalSrcs      map[string]map[string]string
	platformConditionalSrcsToAdd map[string]map[string][]string
	traceDecoded                 bool
	decodedTrace                 *shadow.Decoded
	decodedConfigureFiles        []shadow.ConfigureFileCall
	decodedFileGenerates         []shadow.FileGenerateCall
	decodedExecuteProcesses      []shadow.ExecuteProcessCall
	decodedOutOfTreeExecProcs    []shadow.ExecuteProcessCall
	decodedAddCustomCommands     []shadow.AddCustomCommandCall
	decodedAddCustomTargets      []shadow.AddCustomTargetCall
	decodedAddDependencies       []shadow.AddDependenciesCall
	decodedFileWriters           []shadow.FileWriterCall
	headerOnlySources            map[string]bool
	objectDependsBySrc           map[string][]string
	languageOverrideBySrc        map[string]string
	generatedSources             map[string]bool
	perSourceDefinesBySrc        map[string][]string
}

// parseTraceFacts runs the one-shot trace pre-parse (plus the
// backtrace-first link-keyword recovery) for ToIR. Body and rationale
// comments are verbatim from ToIR's original inline section.
func parseTraceFacts(r *fileapi.Reply, cfg fileapi.Configuration, opts Options) traceFacts {
	knownTargets := map[string]bool{}
	for _, tref := range cfg.Targets {
		knownTargets[tref.Name] = true
	}

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
	var privateIncludeDirs map[string]map[string]bool // target → set of absolute private dir paths
	var publicIncludeDirs map[string]map[string]bool  // target → PUBLIC/INTERFACE target_include dirs (guard for the include_directories privatization)
	var includeDirsGlobal map[string]bool             // directory-scoped include_directories() dirs (absolute) — PRIVATE
	var defineSymbols map[string]string               // target → cmake DEFINE_SYMBOL export macro (trace set_target_properties)
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
	var decodedOutOfTreeExecProcs []shadow.ExecuteProcessCall
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
	var decodedFileWriters []shadow.FileWriterCall
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
		privateIncludeDirs = buildPrivateIncludeDirs(decoded.Includes)
		publicIncludeDirs = buildPublicIncludeDirs(decoded.Includes)
		includeDirsGlobal = buildIncludeDirsGlobal(decoded.IncludeDirectories)
		traceLinkLibs, traceLinkScope = buildTraceLinkInfo(decoded.Links)
		defineSymbols = decoded.DefineSymbols
		decodedConfigureFiles = decoded.ConfigFiles
		decodedFileGenerates = decoded.FileGenerates
		decodedExecuteProcesses = decoded.ExecuteProcesses
		decodedOutOfTreeExecProcs = decoded.OutOfTreeExecuteProcesses
		decodedAddCustomCommands = decoded.AddCustomCommands
		decodedAddCustomTargets = decoded.AddCustomTargets
		decodedAddDependencies = decoded.AddDependencies
		decodedFileWriters = shadow.ExtractFileWriterCalls(opts.TraceRaw, cmakeSrcForTrace)
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
			platformConditionalSrcs = buildPlatformConditionalSrcs(decoded.PlatformConditionalSources)
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

	return traceFacts{
		knownTargets:                 knownTargets,
		privateIncludeDirs:           privateIncludeDirs,
		publicIncludeDirs:            publicIncludeDirs,
		includeDirsGlobal:            includeDirsGlobal,
		defineSymbols:                defineSymbols,
		traceLinkLibs:                traceLinkLibs,
		traceLinkScope:               traceLinkScope,
		platformConditionalSrcs:      platformConditionalSrcs,
		platformConditionalSrcsToAdd: platformConditionalSrcsToAdd,
		traceDecoded:                 traceDecoded,
		decodedTrace:                 decodedTrace,
		decodedConfigureFiles:        decodedConfigureFiles,
		decodedFileGenerates:         decodedFileGenerates,
		decodedExecuteProcesses:      decodedExecuteProcesses,
		decodedOutOfTreeExecProcs:    decodedOutOfTreeExecProcs,
		decodedAddCustomCommands:     decodedAddCustomCommands,
		decodedAddCustomTargets:      decodedAddCustomTargets,
		decodedAddDependencies:       decodedAddDependencies,
		decodedFileWriters:           decodedFileWriters,
		headerOnlySources:            headerOnlySources,
		objectDependsBySrc:           objectDependsBySrc,
		languageOverrideBySrc:        languageOverrideBySrc,
		generatedSources:             generatedSources,
		perSourceDefinesBySrc:        perSourceDefinesBySrc,
	}
}

// resolveSourceRoots performs the workspace-root auto-detection and the
// hostSrc promotion/existence gating. Body and rationale comments are
// verbatim from ToIR's original inline section.
func resolveSourceRoots(r *fileapi.Reply, opts Options, cmakeSrc string) (workspaceRoot, hostSrc string, hostSrcOnDisk bool, err error) {
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
	workspaceRoot = detectWorkspaceRoot(detectAnchor)
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
	// Explicit override: --element-source-root forces the label root to the
	// overlay root regardless of the escape heuristic. The element is overlaid
	// there (the build-lens ELEMENT_SOURCE_ROOT) while cmake configured at a
	// subdir, so labels MUST anchor at the overlay root even though this sample's
	// own sources don't escape cmakeSrc (cuda-samples: per-sample configure +
	// whole-repo overlay so the repo-root Common/ headers resolve).
	if opts.ElementSourceRoot != "" {
		root, rootErr := resolveElementSourceRoot(opts.ElementSourceRoot, cmakeSrc)
		if rootErr != nil {
			return "", "", false, rootErr
		}
		workspaceRoot = root
	}
	hostSrc = opts.HostSourceRoot
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
	hostSrcOnDisk = false
	if info, statErr := os.Stat(hostSrc); statErr == nil && info.IsDir() {
		hostSrcOnDisk = true
	}

	return workspaceRoot, hostSrc, hostSrcOnDisk, nil
}

// targetIndexes bundles the codemodel index maps ToIR builds before the
// target loop. Fields keep ToIR's original local names; ToIR destructures
// them back into same-named locals.
type targetIndexes struct {
	idToName            map[string]string
	utilityIDs          map[string]bool
	utilityIDToName     map[string]string
	artifactToName      map[string]string
	isTargetName        map[string]bool
	codegenConsumerDeps map[string][]fileapi.TargetDependency
	pchResolve          func(targetName, language string) []fileapi.CompilePCH
}

// buildTargetIndexes builds the id→name / artifact / PCH index maps the
// target loop and post-passes consult. Body and rationale comments are
// verbatim from ToIR's original inline section.
// resolveWrappedTestCommands re-points launcher-wrapped add_test registrations
// at their real driver executable (see ctest.Registry.ResolveWrappedCommands).
// The executable predicate is the codemodel's EXECUTABLE target NAME set — the
// same key the direct add_test classification matches against — so a wrapped
// driver lowers to cc_test exactly as a bare-executable registration would.
func resolveWrappedTestCommands(r *fileapi.Reply, cfg fileapi.Configuration, tests *ctest.Registry) {
	if tests == nil {
		return
	}
	execNames := map[string]bool{}
	for _, tref := range cfg.Targets {
		if et, ok := r.Targets[tref.Id]; ok && et.Type == "EXECUTABLE" {
			execNames[tref.Name] = true
		}
	}
	tests.ResolveWrappedCommands(func(name string) bool { return execNames[name] })
}

func buildTargetIndexes(r *fileapi.Reply, cfg fileapi.Configuration, cmakeBuild string, cc *codegenContext) targetIndexes {
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
	execArtifacts := map[string]bool{}
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
					if t.Type == "EXECUTABLE" {
						execArtifacts[art.Path] = true
					}
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
	cc.ExecArtifacts = execArtifacts

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

	// PCH owner index for the forced-include lift: target name + language →
	// that target's ordered PrecompileHeaders. A REUSE_FROM consumer's
	// codemodel carries a null PrecompileHeaders — only the `-include
	// <owner>.dir/cmake_pch.*` fragment names the owner — so lowerTarget
	// resolves the owner's declared list through this. Keyed on the primary
	// configuration's view (the same view the rest of the lower walks); the
	// name-only fallback covers a language-mismatch edge (e.g. a C consumer
	// reusing a CXX owner's PCH) better than dropping the include outright.
	pchByNameLang := map[string][]fileapi.CompilePCH{}
	pchByName := map[string][]fileapi.CompilePCH{}
	for _, tref := range cfg.Targets {
		t, ok := r.Targets[tref.Id]
		if !ok {
			continue
		}
		for _, cg := range t.CompileGroups {
			if len(cg.PrecompileHeaders) == 0 {
				continue
			}
			k := tref.Name + "\x00" + cg.Language
			if _, dup := pchByNameLang[k]; !dup {
				pchByNameLang[k] = cg.PrecompileHeaders
			}
			if _, dup := pchByName[tref.Name]; !dup {
				pchByName[tref.Name] = cg.PrecompileHeaders
			}
		}
	}
	pchResolve := func(targetName, language string) []fileapi.CompilePCH {
		if entries := pchByNameLang[targetName+"\x00"+language]; len(entries) > 0 {
			return entries
		}
		return pchByName[targetName]
	}

	return targetIndexes{
		idToName:            idToName,
		utilityIDs:          utilityIDs,
		utilityIDToName:     utilityIDToName,
		artifactToName:      artifactToName,
		isTargetName:        isTargetName,
		codegenConsumerDeps: codegenConsumerDeps,
		pchResolve:          pchResolve,
	}
}

// recoveredArtifacts bundles the configure-time recovery results
// (execute_process / configure_file / file(GENERATE) lifts, the genex
// target facts, and the find_package attribution) ToIR threads into the
// target loop. Fields keep ToIR's original local names.
type recoveredArtifacts struct {
	executeProcesses []executeProcessOut
	configureFiles   []configureFileOut
	fileGenerates    []fileGenerateOut
	genexTargets     map[string]genexeval.TargetInfo
	findPkgAttrib    *findPackageAttrib
}

// recoverConfigureTimeArtifacts runs the configure-time recovery family —
// execute_process (with stamp propagation + structured refusals),
// configure_file, file(GENERATE) — and builds the genex target facts and
// find_package attribution. Body and rationale comments are verbatim from
// ToIR's original inline section.
// The second return is the Phase-B fallback-placeholder package: non-nil
// when execute_process refusals routed to emitFallbackPlaceholder, in
// which case ToIR returns it directly (the original inline early-exit).
func recoverConfigureTimeArtifacts(r *fileapi.Reply, g *ninja.Graph, opts Options, cfg fileapi.Configuration, tf traceFacts, cmakeSrc, cmakeBuild, hostSrc string, cc *codegenContext) (*recoveredArtifacts, *ir.Package, error) {
	cc.FileWriterIndex = buildFileWriterIndex(tf.decodedFileWriters, cmakeBuild)
	cc.FileWriterTemplates = buildFileWriterTemplates(tf.decodedFileWriters, opts.NonExpandedFileWriters, cmakeBuild)
	cc.CMakeVars = opts.CMakeVars
	cc.LiftConfigureFile = opts.LiftConfigureFile
	cc.LiftDownload = opts.LiftDownload
	cc.RecognizeCodegen = opts.RecognizeCodegen
	cc.ExtraRecognizers = opts.ExtraCodegenRecognizers
	cc.LiftDerivedCodegen = opts.LiftDerivedCodegen
	cc.Fidelity = opts.Fidelity
	traceDecoded, decodedTrace := tf.traceDecoded, tf.decodedTrace
	decodedConfigureFiles, decodedFileGenerates, decodedExecuteProcesses := tf.decodedConfigureFiles, tf.decodedFileGenerates, tf.decodedExecuteProcesses
	decodedOutOfTreeExecProcs := tf.decodedOutOfTreeExecProcs
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
	// Demand/exclusion sets for the unspecified-output execute_process
	// lift (execute_process_unspecified.go): consumed build-dir sources
	// from the codemodel, minus anything a ninja edge produces, are the
	// orphans only configure-time codegen can explain.
	cc.NinjaOuts = ninjaOutputSet(g)
	cc.ConsumedBuildRel = consumedBuildDirSources(r, cmakeBuild)
	// Out-of-tree execute_process calls the lift's inSourceTree filter
	// dropped: route the codemodel-source-backed build-dir subprojects into
	// the SAME lift machinery as in-tree calls (so they get a genrule/probe
	// lift, or a loud per-call refusal — not a vague note), set the uncertain
	// ones aside for warnOutOfTreeExecuteProcess, and drop cmake's own probe
	// noise silently. See out_of_tree_execute_process.go.
	liftOOT, noteOOT := partitionOutOfTreeExec(decodedOutOfTreeExecProcs, cmakeSrc, cmakeBuild, opts.HostPrefixDir, cc.ConsumedBuildRel, cc)
	cc.OutOfTreeExecNotes = noteOOT
	if len(liftOOT) > 0 {
		decodedExecuteProcesses = append(append([]shadow.ExecuteProcessCall(nil), decodedExecuteProcesses...), liftOOT...)
	}
	executeProcesses, executeProcessRefusals := recoverExecuteProcess(decodedExecuteProcesses, hostSrc, cmakeSrc, opts.BuildDir, cmakeBuild, opts.LiftConfigureFile, rescueVars, forwardedStampVars, opts.SetAssignments, opts.ParentScopeForwards, cc)
	// Expand the stamp-var set through verbatim set(X ${Y}) copies the
	// driver recovered from a non-expanded trace (empty in the single-pass
	// default), so a configure_file referencing a copy of a VCS-stamp var
	// lifts to stamp_values. Then surface the (direct + propagated) set to
	// the optional sink for the driver's second-pass gate.
	// Resolve function-parameter-forwarded stamps (git_describe()'s
	// `set(${_var} "${out}" PARENT_SCOPE)`) to the caller-scope variable
	// before propagating verbatim copies, so the marked consumer (GIT_SHA)
	// also seeds any further `set(VERSION ${GIT_SHA})` copy of it.
	applyParentScopeForwards(cc.StampVars, cc.StampCommands, opts.ParentScopeForwards)
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
		// Structured per-call mirror into conversion-todos.json (the
		// agent-actionable channel) — fires in every disposition mode below;
		// the coarse rejection:unsupported-execute-process mirror is skipped
		// in emitRejectionTodos in favor of this producer.
		emitExecuteProcessRefusalTodos(opts.Todos, executeProcessRefusals, hostSrc, opts.BuildDir)
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
			return nil, nil, formatExecuteProcessFailure(executeProcessRefusals)
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
			fallbackPkg, fbErr := emitFallbackPlaceholder(r, hostSrc, opts.FallbackInstallTarget)
			if fbErr != nil {
				return nil, nil, fbErr
			}
			return nil, fallbackPkg, nil
		}
	}

	// Recover configure_file outputs from trace before lowering
	// targets. Each call surfaces as an ir.Target{KindGenrule}
	// on cc.Genrules; the returned slice tells lowerTarget which
	// recorded outputs (and their build-dir-relative paths) to
	// attach to consuming targets that include the cmake build
	// dir in their codemodel-recorded Includes.
	// Codemodel directory scopes (source-relative, "" = root) — the
	// CMAKE_CURRENT_BINARY_DIR levels a relative configure_file output anchors
	// against. Lets the recovery resolve a call made inside an include()d
	// module to the includer's scope rather than the module's own dir.
	configureDirScopes := make([]dirScope, 0, len(cfg.Directories))
	for _, d := range cfg.Directories {
		// ToSlash first, THEN trim the trailing separator (matching
		// subPackageDir) — a Windows-separator Source must be slash-normalized
		// before the trim, else a trailing "/" survives and breaks dirScopeRel's
		// prefix match. Build rides along so relative-output anchoring lands
		// where cmake actually writes when add_subdirectory used a custom
		// binary dir (Source != Build; the FetchContent <name>-src/<name>-build
		// shape).
		norm := func(p string) string {
			p = strings.TrimSuffix(filepath.ToSlash(p), "/")
			if p == "." {
				return ""
			}
			return p
		}
		configureDirScopes = append(configureDirScopes, dirScope{Source: norm(d.Source), Build: norm(d.Build)})
	}
	var configureFiles []configureFileOut
	if traceDecoded {
		var err error
		configureFiles, err = recoverConfigureFilesFromCalls(decodedConfigureFiles, hostSrc, cmakeSrc, opts.BuildDir, cmakeBuild, configureDirScopes, opts.LiftConfigureFile, opts.CMakeVars, cc)
		if err != nil {
			return nil, nil, err
		}
	} else {
		var err error
		configureFiles, err = recoverConfigureFiles(opts.TraceRaw, hostSrc, opts.BuildDir, cmakeSrc, cmakeBuild, configureDirScopes, opts.LiftConfigureFile, opts.CMakeVars, cc)
		if err != nil {
			return nil, nil, err
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
	genexTargets := buildGenexTargets(r, cmakeBuild, opts.GenexProbes, decodedTrace, opts.Imports, opts.HostPrefixDir)

	var fileGenerates []fileGenerateOut
	if traceDecoded {
		var err error
		fileGenerates, err = recoverFileGenerate(decodedFileGenerates, hostSrc, cmakeSrc, opts.BuildDir, cmakeBuild, opts.BazelPackagePath, configureDirScopes, opts.LiftConfigureFile, opts.CMakeVars, genexTargets, opts.Imports, cc)
		if err != nil {
			return nil, nil, err
		}
	}

	return &recoveredArtifacts{
		executeProcesses: executeProcesses,
		configureFiles:   configureFiles,
		fileGenerates:    fileGenerates,
		genexTargets:     genexTargets,
		findPkgAttrib:    findPkgAttrib,
	}, nil, nil
}

// assembleLoweredPackage appends the recovered/synthesized target families
// to the lowered package and runs the structural post-passes (retags,
// reclassifications, lifts, interface/alias synthesis, standalone
// custom-command emission, and the multi-config fold). Bodies and
// rationale comments are verbatim from ToIR's original inline tail.
// emitStandaloneCustomCommands runs the Phase 4 standalone
// custom-command emission family for assembleLoweredPackage. Body and
// rationale comments are verbatim from the original inline block.
func emitStandaloneCustomCommands(pkg *ir.Package, g *ninja.Graph, opts Options, tf traceFacts, ti targetIndexes, cc *codegenContext, cmakeSrc, cmakeBuild, hostSrc string) {
	decodedTrace := tf.decodedTrace
	decodedAddCustomCommands, decodedAddCustomTargets, decodedAddDependencies := tf.decodedAddCustomCommands, tf.decodedAddCustomTargets, tf.decodedAddDependencies
	artifactToName, codegenConsumerDeps := ti.artifactToName, ti.codegenConsumerDeps
	utilityIDs, utilityIDToName, isTargetName := ti.utilityIDs, ti.utilityIDToName, ti.isTargetName
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
		// The main cc.Genrules fold already ran (assembleLoweredPackage's
		// top); anything THIS pass synthesizes onto cc.Genrules (the
		// response-file header filegroups, a leftover script bake) must
		// fold separately below or it silently vanishes.
		preGenrules := len(cc.Genrules)
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
		if len(cc.Genrules) > preGenrules {
			pkg.Targets = append(pkg.Targets, cc.Genrules[preGenrules:]...)
		}

		// Stage sibling generated headers (path-independent post-pass). A genrule
		// that emits a .c + .h pair (rpcgen: regress.gen.c + regress.gen.h) — any
		// cc target listing only the .c must still see the .h, which the .c
		// #includes by bare same-dir quote (cmake omits the generated header from
		// the target's source list). Regardless of HOW the .c reached a target's
		// srcs (per-source recovery, build-include consumer attribution, …),
		// attach the producing genrule's sibling header outputs to that target's
		// hdrs. Element-relative; split's hdrs relabel re-homes them cross-package.
		stageSiblingGeneratedHeaders(pkg)

		// Resolve each cc_library consumer's UTILITY (tablegen) dependencies
		// to the generated `.inc` headers it #includes, now that the
		// standalone genrules producing them have been recovered. The
		// combined output set — per-target codegen outputs (cc.OutToGenrule)
		// plus this standalone recovery — is what the ninja walk filters on;
		// matches land on pkg.CodegenHeaderConsumers for the split transform
		// to synthesize the wrapper library and wire the consumer's dep.
		if len(codegenConsumerDeps) > 0 {
			resolveCodegenHeaderConsumers(pkg, g, stand, cc, codegenConsumerDeps, utilityIDs, utilityIDToName, isTargetName)
		}
		// A cc target that LISTS a recognized codegen output as a source (the
		// execute_process shape: the generated foo.pb.cc compiled into a lib) has
		// it stripped + a direct deps edge to the native rule added — the native
		// rule compiles the generated source itself. Runs regardless of the
		// utility-edge walk above (it keys on OutToNativeConsumerDep).
		rewriteNativeRuleConsumers(pkg, cc)
		// Place recognized native rules in the package owning their codegen
		// output (recorded by the dispatch), so the recognizer's basename srcs
		// resolve and cross-package proto imports line up (//pkg/a:a_proto).
		placeNativeRuleSubPackages(pkg, cc)
	}
}

func assembleLoweredPackage(pkg *ir.Package, r *fileapi.Reply, g *ninja.Graph, opts Options, tf traceFacts, ti targetIndexes, cc *codegenContext, ra *recoveredArtifacts, cmakeSrc, cmakeBuild, hostSrc, workspaceRoot string) error {
	knownTargets, decodedTrace := tf.knownTargets, tf.decodedTrace
	headerOnlySources, objectDependsBySrc, languageOverrideBySrc := tf.headerOnlySources, tf.objectDependsBySrc, tf.languageOverrideBySrc
	perSourceDefinesBySrc := tf.perSourceDefinesBySrc
	idToName := ti.idToName
	genexTargets := ra.genexTargets
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
	wireDefineDrivenGeneratedHeaders(pkg)
	// Faithful-SHARED Phase 2: wire consumers' dynamic_deps. A target that
	// depends on a SHARED/MODULE library (which now also emits a sibling
	// cc_shared_library) keeps the impl in deps (for headers) and lists the
	// `<dep>_shared` target in dynamic_deps, so Bazel links the .so instead of
	// static-linking the impl — and the "a cc_library is owned by at most one
	// shared lib" rule is satisfied. Intra-package label match by bare name;
	// the split emit relabels DynamicDeps cross-package like it does deps.
	if opts.EmitSharedLibraries {
		wireDynamicDeps(pkg)
	}
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
	// Fortran retag — lower a cmake Fortran target (.f / .f90 / ... sources a
	// cc_* rule can't compile) to a buildable fortran_library that drives the
	// cc toolchain's own gfortran driver and exposes CcInfo. Fortran-only
	// targets retag in place; mixed C+Fortran targets gain a private sibling
	// fortran_library the cc_* target deps on. See retagFortranTargets.
	retagFortranTargets(pkg, cmakeSrc)
	// CUDA retag — a cc_* target whose compiled srcs are entirely `.cu`
	// CUDA device code renders as the matching rules_cuda rule (cuda_library
	// / cuda_binary / cuda_test) so nvcc compiles it; a plain cc_library
	// can't (no `.cu` compile action). Runs after the per-language split so
	// it only catches WHOLE-target CUDA cases (the split already retagged
	// mixed-language CUDA sub-libs). See retagCudaTargets.
	retagCudaTargets(pkg, cudaRdcTargetNames(g))
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
		// Directory-scoped add_definitions() is PRIVATE too (never
		// exported via INTERFACE_COMPILE_DEFINITIONS) — route it to
		// local_defines so it doesn't leak to consumers (e.g. curl's
		// BUILDING_LIBCURL on libcurl leaking to the curl tool).
		applyAddDefinitionsScope(pkg, decodedTrace.AddDefinitions, decodedTrace.CompileDefinitions)
		// Principled define-scope pass: keep a define transitive only when the
		// owning cmake target EXPORTS it via INTERFACE_COMPILE_DEFINITIONS, else
		// route it to local_defines. Generalizes the two trace passes above —
		// which only classify target_compile_definitions PRIVATE + add_definitions
		// — to every private mechanism (set_property COMPILE_DEFINITIONS, the auto
		// <target>_EXPORTS macro, CMAKE_<LANG>_FLAGS globals). genexTargets carries
		// cmake's resolved INTERFACE_COMPILE_DEFINITIONS; cc.SubParent maps split
		// sub-libraries back to their owning cmake target. Gated with the trace
		// (decodedTrace != nil) so the interface whitelist is populated; a no-op
		// when genexTargets is empty.
		applyInterfaceScopeToDefines(pkg, genexTargets, cc.SubParent)
	}
	// PRIVATE link-dependency scope: route deps cmake marks `$<LINK_ONLY:Dep>`
	// in INTERFACE_LINK_LIBRARIES (the probe captures it verbatim) from the
	// transitive `deps` to `implementation_deps`, so a privately-linked dep's
	// PUBLIC defines/includes don't over-propagate to consumers. Independent of
	// the trace (driven by the probe's LINK_ONLY marker); a no-op when no marker
	// is present (non-probe converts, or targets with no private links).
	applyInterfaceLinkScopeToDeps(pkg, genexTargets, cc.SubParent)
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
		return err
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
	// Build-relative outputs the convert actually PRODUCES (genrule / write_file
	// / configure_file). install(FILES) of a generated build-dir file packages
	// only when its output has a producer — otherwise the pkg_files would
	// reference a missing input (e.g. fmt's configure_package_config_file
	// fmt-config.cmake, which the converter doesn't lift, must NOT be packaged
	// until a producer exists; see the Config.cmake-generation follow-up).
	produced := producedOutputs(pkg.Targets)
	pkg.Targets = append(pkg.Targets, lowerDirectoryInstallers(r, opts.EmitInstallExportConfig, produced)...)
	// install(TARGETS) → pkg_files: package each built library / binary under
	// its install destination (the per-target Install slot otherwise only feeds
	// the cc_import facade + round-2 tree, leaving the artifact in no install
	// package). Faithful, same as install(FILES)/install(DIRECTORY) above; runs
	// over the lowered codemodel targets (the appended pkg_files carry no
	// InstallDest so they're skipped).
	pkg.Targets = append(pkg.Targets, synthesizeTargetInstallPkgFiles(pkg.Targets)...)
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
		ifaceLibs := lowerInterfaceLibraries(decodedTrace, knownTargets, hostSrc, cmakeSrc, workspaceRoot, genexTargets, opts.Imports, cc)
		// Place each trace-synth interface lib in its DECLARING sub-package so
		// --split-packages materializes a BUILD.bazel there (else they fall to
		// the root package). The declaring scope is recovered via the trace
		// frame stack (AddLibraryCall.DeclFile) — abseil wraps add_library in
		// the absl_cc_library FUNCTION, so the call's own File is the helper
		// module, not absl/<m>/CMakeLists.txt. Mirrors the codemodel path's
		// per-target pkg.SubPackages assignment (subPackageDir above).
		declByName := make(map[string]string, len(decodedTrace.AddLibraries))
		for _, c := range decodedTrace.AddLibraries {
			if c.DeclFile != "" {
				declByName[c.Name] = c.DeclFile
			}
		}
		// A project with ONLY trace-synth interface libs (no codemodel targets)
		// never had pkg.SubPackages initialized by the codemodel loop above.
		if len(ifaceLibs) > 0 && pkg.SubPackages == nil {
			pkg.SubPackages = map[string]string{}
		}
		for i := range ifaceLibs {
			name := ifaceLibs[i].Name
			if _, set := pkg.SubPackages[name]; set {
				continue
			}
			if dir, ok := subPackageDirFromFile(declByName[name], cmakeSrc, workspaceRoot); ok {
				pkg.SubPackages[name] = dir
			}
		}
		pkg.Targets = append(pkg.Targets, ifaceLibs...)
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
	emitStandaloneCustomCommands(pkg, g, opts, tf, ti, cc, cmakeSrc, cmakeBuild, hostSrc)
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
		// The per-config PCH divergence pass needs the same mirror
		// machinery lowerTarget's lift uses; reconstruct the context
		// from assemble-level state.
		mcPCH := multiConfigPCHCtx(ti, opts, cc, cmakeSrc, cmakeBuild, hostSrc)
		lowerMultiConfigDeltas(pkg, r.TargetsByConfig, configs, cmakeSrc, cmakeBuild, idToName, mcPCH)
		// The multi-config fold populates the per-config `defines` select() arms
		// AFTER the flat-define scoping above, so a config-divergent PRIVATE
		// define (VTK's KWSYS_SYSTEMINFORMATION_HAS_DEBUG_BUILD, set only under
		// Debug/RelWithDebInfo via set_property(SOURCE … COMPILE_DEFINITIONS_DEBUG))
		// lands transitive and bypasses that scoping. Re-run the interface-scope
		// pass here to route the freshly-folded per-config arms (idempotent on the
		// already-scoped flat defines). Gated with the trace like the flat pass.
		if decodedTrace != nil {
			applyInterfaceScopeToDefines(pkg, genexTargets, cc.SubParent)
		}
	}
	return nil
}

// emitToIRDiagnostics runs the reporting tail: operator breadcrumbs,
// structured conversion-todos, the coverage audit, and the final
// shaping/sanitization passes that precede emission. Bodies and rationale
// comments are verbatim from ToIR's original inline tail.
// warnUnconvertedTests surfaces add_test registrations that produced no
// cc_test, grouped by COMMAND runner. Body and rationale comments are
// verbatim from emitToIRDiagnostics' original inline block.
func warnUnconvertedTests(opts Options, cc *codegenContext) {
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
}

func emitToIRDiagnostics(pkg *ir.Package, r *fileapi.Reply, g *ninja.Graph, opts Options, tf traceFacts, ti targetIndexes, cc *codegenContext, cmakeSrc, cmakeBuild, hostSrc string, hostSrcOnDisk bool) error {
	decodedTrace := tf.decodedTrace
	decodedExecuteProcesses := tf.decodedExecuteProcesses
	decodedAddCustomCommands, decodedAddCustomTargets := tf.decodedAddCustomCommands, tf.decodedAddCustomTargets
	knownTargets := tf.knownTargets
	traceLinkLibs, traceLinkScope := tf.traceLinkLibs, tf.traceLinkScope
	isTargetName := ti.isTargetName
	_, _, _, _ = decodedTrace, decodedAddCustomTargets, knownTargets, isTargetName
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
		kinds := sliceutil.SortedKeys(byKind)
		fmt.Fprintf(opts.Warnings,
			"lower: filtered %d cmake command edge(s) with no Bazel analogue (dropped, not converted):\n",
			len(cc.FilteredInternalCmds))
		for _, k := range kinds {
			outs := byKind[k]
			sort.Strings(outs)
			fmt.Fprintf(opts.Warnings, "  %s (%d): %s\n", k, len(outs), strings.Join(outs, ", "))
		}
	}
	// Same filtered-command drops, as structured conversion-todos (one
	// per drop kind). No-op on a nil collector; independent of the
	// stderr breadcrumb above so the JSON is produced even when
	// Warnings is nil.
	emitInternalDropTodos(opts.Todos, cc.FilteredInternalCmds)
	emitUnreadableConfigureOutputTodos(opts.Todos, cc.UnreadableConfigureOutputs)
	// Configure-time recovery inputs that couldn't be resolved (an
	// unanchorable configure_file output, an unreadable declared .proto or
	// nested header): uncertain drops, surfaced as todos + a breadcrumb rather
	// than the historical silent continue. See unresolved_recovery_inputs.go.
	warnUnresolvedRecoveryInputs(opts, cc)
	warnUnconvertedTests(opts, cc)
	// Nested cmake builds detected but not lifted (offline run, warm
	// pass disabled, or a failed nested lowering): loud degradation —
	// stderr warning + structured todo — replacing the historical
	// Tier-1 refusal. See nested_cmake.go.
	warnUnliftedNestedBuilds(opts, cc)
	// Out-of-tree execute_process calls that were uncertain (no codemodel
	// sources to anchor a lift): the codemodel-source-backed ones were already
	// spliced into the lift in recoverConfigureTimeArtifacts; this surfaces the
	// rest as conversion-todos. cmake's own probe noise stayed silent. See
	// out_of_tree_execute_process.go.
	warnOutOfTreeExecuteProcess(opts, cc)
	// Codemodel sources dropped without recovery (no silent drops):
	// aggregate stderr warning + structured source-elided todos. See
	// build_dir_source_bake.go.
	warnElidedSources(opts, cc)
	emitDownloadLiftTodos(opts.Todos, opts.BazelPackagePath, cc.DownloadLifts)
	if opts.DownloadRepos != nil {
		*opts.DownloadRepos = append(*opts.DownloadRepos, downloadRepoSpecs(opts.BazelPackagePath, cc.DownloadLifts)...)
	}
	populateWorkspaceStatusSink(opts.WorkspaceStatusSink, pkg, cc.StampCommands)
	// Same unconverted add_test registrations, as structured
	// conversion-todos (one per COMMAND runner). No-op on a nil
	// collector; independent of the stderr breadcrumb above.
	emitCMakePTestTodos(opts.Todos, opts.CTest, cc.Tests, opts.HostSourceRoot, opts.BuildDir)
	// Surface install(SCRIPT) / install(CODE) directives. These run
	// cmake script code at install time and have no Bazel
	// analogue — the converter drops them silently. The warning
	// makes the omission auditable so operators who care about
	// install-time logic see what was lost.
	surfaceInstallScriptInstallers(r, opts.Warnings)
	// Same install(SCRIPT)/install(CODE) directives, as structured
	// conversion-todos (one per (site, scriptFile)). No-op on a nil
	// collector.
	emitInstallScriptTodos(opts.Todos, r)

	// Full-coverage generic producers: mirror every Tier-1 refusal (diagnostic
	// mode only), every convert-time bake, and every unresolved-genex audit tag
	// into the report, each carrying a best-guess disposition.
	emitRejectionTodos(opts.Todos, opts.Rejections, cmakeBuild)
	emitBakeTodos(opts.Todos, pkg, cc.bakeTodoDisposition)
	emitUnresolvedGenexTodos(opts.Todos, pkg)
	emitHostCodegenToolTodos(opts.Todos, cc.HostCodegenTools)

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

	// Comment-carrying (opt-in): recover author comments from raw cmake
	// source onto the lowered targets + the package header. The File API
	// carries no comments, so this reads source at each declaration site.
	if opts.RecoverSourceComments {
		recoverSourceComments(pkg, hostSrc, cmakeSrc, cmakeBuild,
			decodedExecuteProcesses, decodedAddCustomCommands, decodedAddCustomTargets)
	}

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

	// Dedup/sort the declared source-byte reads the two passes above
	// published — the few SOURCE files whose bytes shaped the BUILD (fused-
	// source includers + generated-source-include closure). Published via
	// --out-source-reads for the source-narrowing lens. See
	// ir.Package.SourceByteReads.
	if len(pkg.SourceByteReads) > 0 {
		pkg.SourceByteReads = sliceutil.SortedUnique(pkg.SourceByteReads)
	}

	return nil
}

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

	tf := parseTraceFacts(r, cfg, opts)
	privateIncludeDirs, publicIncludeDirs, includeDirsGlobal := tf.privateIncludeDirs, tf.publicIncludeDirs, tf.includeDirsGlobal
	defineSymbols, traceLinkLibs, traceLinkScope := tf.defineSymbols, tf.traceLinkLibs, tf.traceLinkScope
	platformConditionalSrcs, platformConditionalSrcsToAdd := tf.platformConditionalSrcs, tf.platformConditionalSrcsToAdd
	generatedSources := tf.generatedSources

	cmakeSrc := r.Codemodel.Paths.Source
	cmakeBuild := r.Codemodel.Paths.Build
	workspaceRoot, hostSrc, hostSrcOnDisk, err := resolveSourceRoots(r, opts, cmakeSrc)
	if err != nil {
		return nil, err
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

	cc := newCodegenContextFor(opts)
	cc.BazelPackagePath = opts.BazelPackagePath
	cc.CMakeScriptRunner = opts.CMakeScriptRunner
	cc.CMakeScriptTrace = opts.CMakeScriptTrace
	cc.CMakeScriptBake = opts.CMakeScriptBake
	cc.LiftCCEmbed = opts.LiftCCEmbed
	cc.LiftCCHash = opts.LiftCCHash
	cc.CMakeBinary, cc.Warnings = lookupCmakeBinary(), opts.Warnings
	cc.OutputToCustomCommand = buildOutputToCustomCommand(tf.decodedAddCustomCommands, opts.BuildDir)
	cc.LiteralProbeSink = opts.LiteralProbeSink
	cc.LiteralResolutions = opts.LiteralResolutions
	// Parallel pre-warm of the cmake -P script bakes: with the bake opted
	// in, run every bakeable script up front in dependency waves with a
	// bounded pool (see cmake_script_prewarm.go) — the serial recovery path
	// then consumes the cached results instead of paying one subprocess
	// wait at a time (the dominant translation cost on script-heavy
	// projects; VTK: 238 runs ≈ 95s serial).
	if cc.CMakeScriptBake {
		prewarmScriptBakes(cc, g, opts.BuildDir)
	}

	// Nested-cmake lift (see nested_cmake.go): route the driver's
	// detection sink into the recovery context, and merge the warm
	// second pass's harvested nested builds BEFORE the recovery +
	// target loop so their artifacts and baked headers are registered
	// for consumer attribution.
	if opts.NestedConfigureSink != nil {
		cc.NestedConfigureSink = opts.NestedConfigureSink
	}
	cc.CaptureRefusalSink, cc.DeadCaptureVars = opts.CaptureRefusalSink, opts.DeadCaptureVars
	nestedOuts, err := lowerNestedBuilds(pkg, opts, cc, hostSrc)
	if err != nil {
		return nil, err
	}

	ra, fallbackPkg, err := recoverConfigureTimeArtifacts(r, g, opts, cfg, tf, cmakeSrc, cmakeBuild, hostSrc, cc)
	if err != nil {
		return nil, err
	}
	if fallbackPkg != nil {
		return fallbackPkg, nil
	}
	ra.executeProcesses = append(ra.executeProcesses, nestedOuts...)
	executeProcesses, configureFiles, fileGenerates, genexTargets, findPkgAttrib := ra.executeProcesses, ra.configureFiles, ra.fileGenerates, ra.genexTargets, ra.findPkgAttrib

	// Build the in-codebase id -> Bazel-rule-name map up front so dep
	// lowering can map t.Dependencies[].Id to a label without re-walking
	// configurations. UTILITY targets (add_custom_target nodes) are
	// excluded — they have no Bazel equivalent, so depending on them is a
	// no-op; the underlying add_custom_command's outputs are referenced
	// via srcs/hdrs instead. utilityIDs records them separately so dep
	// resolution can distinguish "skip utility" from "unresolved".
	ti := buildTargetIndexes(r, cfg, cmakeBuild, cc)
	idToName, utilityIDs, codegenConsumerDeps, pchResolve := ti.idToName, ti.utilityIDs, ti.codegenConsumerDeps, ti.pchResolve

	// See through launcher-wrapped add_test COMMANDs to the real driver exe
	// before the target loop classifies it (see the helper).
	resolveWrappedTestCommands(r, cfg, opts.CTest)

	lc := targetLowerCtx{
		cmakeSrc:            cmakeSrc,
		cmakeBuild:          cmakeBuild,
		hostSrc:             hostSrc,
		hostBuild:           opts.BuildDir,
		hostPrefix:          opts.HostPrefixDir,
		hostSrcOnDisk:       hostSrcOnDisk,
		g:                   g,
		cc:                  cc,
		idToName:            idToName,
		utilityIDs:          utilityIDs,
		imports:             opts.Imports,
		tests:               opts.CTest,
		configureFiles:      configureFiles,
		fileGenerates:       fileGenerates,
		executeProcesses:    executeProcesses,
		findPkgAttrib:       findPkgAttrib,
		workspaceRoot:       workspaceRoot,
		bazelPackagePath:    opts.BazelPackagePath,
		generatedSources:    generatedSources,
		rejections:          opts.Rejections,
		emitSharedLibraries: opts.EmitSharedLibraries,
		pchResolve:          pchResolve,
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
			publicIncludeDirs:            publicIncludeDirs[tref.Name],
			includeDirsGlobal:            includeDirsGlobal,
			interfaceIncludeDirs:         interfaceIncludeDirsFor(genexTargets, tref.Name, len(opts.GenexProbes) > 0),
			traceLinkLibs:                traceLinkLibs[tref.Name],
			traceLinkScope:               traceLinkScope[tref.Name],
			platformConditionalSrcs:      platformConditionalSrcs[tref.Name],
			platformConditionalSrcsToAdd: platformConditionalSrcsToAdd[tref.Name],
			defineSymbol:                 defineSymbols[tref.Name],
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

		// Record this target's codemodel UTILITY dependencies so a pass
		// AFTER standalone-genrule recovery (which is what fills the genrule
		// output set the walk filters on) can resolve them to the generated
		// `.inc` headers it consumes. cc_binary/cc_test are included alongside
		// cc_library: LLVM's tools are cc_binary that `#include "Opts.inc"`
		// (the `-gen-opt-parser-defs` tablegen output from each tool's Opts.td,
		// wired via add_public_tablegen_target), so they need the same
		// generated-header wrapper dep + genfiles include the libraries get.
		// The split transform's wrapper synthesis keys on consumer NAME, not
		// kind, so a cc_binary consumer flows through unchanged.
		if (irt.Kind == ir.KindCCLibrary || irt.Kind == ir.KindCCBinary || irt.Kind == ir.KindCCTest) && len(t.Dependencies) > 0 {
			codegenConsumerDeps[irt.Name] = t.Dependencies
		}
	}

	if err := assembleLoweredPackage(pkg, r, g, opts, tf, ti, cc, ra, cmakeSrc, cmakeBuild, hostSrc, workspaceRoot); err != nil {
		return nil, err
	}
	if err := emitToIRDiagnostics(pkg, r, g, opts, tf, ti, cc, cmakeSrc, cmakeBuild, hostSrc, hostSrcOnDisk); err != nil {
		return nil, err
	}
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
	hostBuild        string
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
	// emitSharedLibraries: when true, a SHARED_LIBRARY/MODULE_LIBRARY target's
	// IR carries SharedLibName so emit renders a real cc_shared_library wrapper
	// (faithful-SHARED). Off by default — the static-collapse emit is unchanged.
	emitSharedLibraries bool
	// pchResolve resolves a target name + language to its declared
	// target_precompile_headers list — the REUSE_FROM half of the PCH
	// forced-include lift (see pch.go).
	pchResolve pchResolver
}

// targetTrace bundles the per-target trace-derived inputs to
// lowerTarget, looked up by target name at the call site.
type targetTrace struct {
	privateIncludeDirs           map[string]bool
	publicIncludeDirs            map[string]bool
	includeDirsGlobal            map[string]bool
	interfaceIncludeDirs         interfaceIncludeScope
	traceLinkLibs                []string
	traceLinkScope               map[string]string
	platformConditionalSrcs      map[string]string
	platformConditionalSrcsToAdd map[string][]string
	// defineSymbol is the target's cmake DEFINE_SYMBOL export macro (from the
	// trace's set_target_properties), or "" if unset. lowerTarget routes a
	// matching define to local_defines so a SHARED/MODULE lib's export macro
	// doesn't propagate to consumers when SHARED collapses to cc_library.
	defineSymbol string
}

// linkDepRouter centralizes the dep-append motif of the link attribution:
// dedup against the seen-set (which spans Deps + ImplementationDeps so a
// dep already routed to either bucket by the t.Dependencies loop doesn't
// get re-appended — that would duplicate it across both buckets and
// produce an invalid BUILD), then trace-scope routing — a lib recorded
// PRIVATE routes to ImplementationDeps only on targets that accept the
// attribute (cc_library; cc_binary / cc_test / cc_import don't), else
// folds into Deps.
type linkDepRouter struct {
	irt                      *ir.Target
	seen                     map[string]bool
	allowsImplementationDeps bool
}

func newLinkDepRouter(irt *ir.Target) *linkDepRouter {
	seen := map[string]bool{}
	for _, d := range irt.Deps {
		seen[d] = true
	}
	for _, d := range irt.ImplementationDeps {
		seen[d] = true
	}
	return &linkDepRouter{irt: irt, seen: seen, allowsImplementationDeps: kindAllowsImplementationDeps(irt.Kind)}
}

// addExport wires a manifest export: its BazelLabel plus every
// Export.Deps label, all with the same scope. The deps ride along
// because Bazel transitivity can't recover them for prebuilt/
// cc_import-backed labels (see manifest.Export.Deps).
func (rt *linkDepRouter) addExport(ex *manifest.Export, private bool) {
	rt.add(ex.BazelLabel, private)
	for _, d := range ex.Deps {
		rt.add(d, private)
	}
}

func (rt *linkDepRouter) add(label string, private bool) {
	if rt.seen[label] {
		return
	}
	rt.seen[label] = true
	if rt.allowsImplementationDeps && private {
		rt.irt.ImplementationDeps = append(rt.irt.ImplementationDeps, label)
	} else {
		rt.irt.Deps = append(rt.irt.Deps, label)
	}
}

// scopeIsPrivate is the router call sites' shared scope predicate over the
// trace-recorded target_link_libraries keyword for a manifest export.
func scopeIsPrivate(traceLinkScope map[string]string, cmakeTarget string) bool {
	return traceLinkScope != nil && scopeForLabelLib(traceLinkScope, cmakeTarget) == "PRIVATE"
}

// appendLinkFlagTokens lowers one "flags"-role link fragment onto
// irt.LinkOpts. cmake's File API serialises link flags as one
// whitespace-joined string per fragment (e.g. "-Wl,--gc-sections
// -Wl,-z,now -O3 -DNDEBUG"); tokenise so each flag lands as its own
// linkopts entry — Bazel passes each list entry as a separate argv to the
// linker driver; without this split the linker receives the entire string
// as a single (invalid) flag. Per-token drops and rationale are verbatim
// from the original inline case body.
func appendLinkFlagTokens(irt *ir.Target, t *fileapi.Target, fragment, cmakeSrc, cmakeBuild string) {
	for _, tok := range strings.Fields(fragment) {
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
		// nvcc GPU-arch flags (--generate-code / -gencode): cmake
		// puts the CMAKE_CUDA_ARCHITECTURES list on the device-LINK
		// line too (CUDA_SEPARABLE_COMPILATION). rules_cuda forbids
		// them in linkopts (arch is a toolchain/flag concern), and
		// cmake's quoted form would reach the linker driver with
		// literal quotes. Drop them — mirror the copts-side drop.
		if isNvccArchFlag(rewritten) {
			continue
		}
		// Shared-only link flags (version script / soname /
		// retain-symbols) apply to a .so link. SHARED_LIBRARY /
		// MODULE_LIBRARY collapse to a static cc_library (no .so),
		// where a propagating version-script linkopt fails on every
		// consumer link missing the script's symbols (zlib's
		// zlib.map). Drop them (and their additional_linker_input,
		// since this skips before the append below).
		if (t.Type == "SHARED_LIBRARY" || t.Type == "MODULE_LIBRARY") &&
			isSharedOnlyLinkFlag(rewritten) {
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
}

// routeNonAbsLibraryFragment handles a non-absolute "libraries"-role
// fragment. In-codebase target output names (e.g. `libfoo.a` for a
// sibling cc_library) are already routed to irt.Deps by the
// t.Dependencies walk — re-emitting them here would create
// false-positive audit noise, so they stay dropped. But cmake also lands
// bare SYSTEM-library links here as link FLAGS (anything cmake emits with
// a leading `-`; an in-codebase target ref never starts with `-`):
// target_link_libraries(foo m) → `-lm`, Threads::Threads →
// `-lpthread`/`-pthread`, ${CMAKE_DL_LIBS} → `-ldl`. Those have no dep to
// carry them, so dropping them silently loses the link (the brotli -lm,
// googletest -lpthread, libxml2/llvm -ldl survey gaps). Route the flag
// shapes to linkopts; a `-l<name>` first goes through the same
// producer-element precedence as the absolute system-lib lift (a producer
// claiming the name wins over the host -l<name>).
func routeNonAbsLibraryFragment(irt *ir.Target, rt *linkDepRouter, path string, imports *manifest.Resolver, traceLinkScope map[string]string) {
	if !strings.HasPrefix(path, "-") {
		return
	}
	if name, ok := linkLibFlagName(path); ok {
		if export := imports.LookupLinkLibrary(name); export != nil {
			rt.addExport(export, scopeIsPrivate(traceLinkScope, export.CMakeTarget))
			return
		}
	}
	// `-l<name>` (no producer claim) or another link flag
	// (`-pthread`). Defensive isCompileOnlyLinkFlag guard
	// keeps a compile-only flag cmake mis-attached to the
	// link line off it; dedup mirrors the flags-role path.
	if !isCompileOnlyLinkFlag(path) && !stringSliceContains(irt.LinkOpts, path) {
		irt.LinkOpts = append(irt.LinkOpts, path)
	}
}

// attributeUnresolvedLibPath handles an absolute "libraries"-role
// fragment the imports manifest didn't claim: find_package variable-form
// attribution (with the system-lib -l<name> lift and the
// find-package-fallback tag), then the #220 manifest-and-attribution-miss
// path (system-lib lift, elided-fragment tag, attribution-missed tag).
// Bodies and rationale comments are verbatim from the original inline
// region.
func attributeUnresolvedLibPath(irt *ir.Target, rt *linkDepRouter, t *fileapi.Target, path string, imports *manifest.Resolver, findPkgAttrib *findPackageAttrib, traceLinkScope map[string]string) {
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
			rt.add(export.BazelLabel, scopeIsPrivate(traceLinkScope, export.CMakeTarget))
			return
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
				rt.add(export.BazelLabel, scopeIsPrivate(traceLinkScope, export.CMakeTarget))
				return
			}
			flag := "-l" + name
			if !stringSliceContains(irt.LinkOpts, flag) {
				irt.LinkOpts = append(irt.LinkOpts, flag)
			}
			return
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
		return
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
			rt.addExport(export, scopeIsPrivate(traceLinkScope, export.CMakeTarget))
			return
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

// lowerLinkFragments walks t.Link.CommandFragments, routing non-library
// roles to linkopts and attributing "libraries"-role fragments through
// the imports manifest / find_package channels via the linkDepRouter.
// Rationale comments are verbatim from the original inline block.
func lowerLinkFragments(irt *ir.Target, t *fileapi.Target, tt targetTrace, lc targetLowerCtx) {
	if t.Link == nil {
		return
	}
	cmakeSrc, cmakeBuild, hostPrefix := lc.cmakeSrc, lc.cmakeBuild, lc.hostPrefix
	imports, findPkgAttrib := lc.imports, lc.findPkgAttrib
	traceLinkLibs, traceLinkScope := tt.traceLinkLibs, tt.traceLinkScope
	rt := newLinkDepRouter(irt)
	// Direct find_package deps the trace recorded for THIS target
	// (target_link_libraries lib names). When present, they gate the
	// link-fragment LookupLinkPath attribution below: an EXECUTABLE /
	// SHARED lib static-links its WHOLE transitive .a closure, so
	// cmake's Link.CommandFragments lists every archive — including
	// internal ones a consumer never names directly (abseil's
	// raw_logging_internal / spinlock_wait / strerror, pulled in by
	// the public absl targets). Attributing each flattened archive as
	// a DIRECT Bazel dep both over-specifies the graph and breaks on
	// the internal targets' restricted visibility
	// (`//absl:__subpackages__`). Bazel computes transitivity itself,
	// so we only want the libs the target links DIRECTLY; the rest
	// flow through the directly-named public deps. Empty (no trace for
	// this target) → the gate is disabled and every matched fragment
	// is attributed, preserving the offline-replay behavior.
	directTraceLibs := map[string]bool{}
	for _, lib := range traceLinkLibs {
		directTraceLibs[lib] = true
	}
	for _, frag := range t.Link.CommandFragments {
		// Non-library fragments (flags / libraryPath /
		// frameworkPath / frameworks) route directly to
		// linkopts. cmake's codemodel exposes the per-fragment
		// role so we can attribute correctly; the existing
		// "libraries"-only path below handles deps wiring.
		switch frag.Role {
		case "flags":
			appendLinkFlagTokens(irt, t, frag.Fragment, cmakeSrc, cmakeBuild)
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
		if path == "" {
			continue
		}
		if !filepath.IsAbs(path) {
			// Nested-cmake artifact (relative form): cmake's ninja
			// generator records the archive build-relative
			// (`subbuild/libsublib.a`); route it to the merged nested
			// target's label. See nested_cmake.go.
			if label, hit := lc.cc.NestedArtifactDeps[path]; hit {
				rt.add(label, false)
				continue
			}
			routeNonAbsLibraryFragment(irt, rt, path, imports, traceLinkScope)
			continue
		}
		// Nested-cmake artifact wiring: a fragment naming an archive the
		// merged nested build produces (`<build>/subbuild/libX.a`) routes
		// to the nested target's label — the codemodel records it as a
		// raw path because the outer project links the FILE, not a
		// target. Checked before the imports manifest (a nested artifact
		// never matches one). See nested_cmake.go.
		if len(lc.cc.NestedArtifactDeps) > 0 {
			if rel, inside := relativeIfInside(lc.cmakeBuild, path); inside {
				if label, hit := lc.cc.NestedArtifactDeps[rel]; hit {
					rt.add(label, false)
					continue
				}
			}
		}
		if hostPrefix != "" && strings.HasPrefix(path, hostPrefix+string(filepath.Separator)) {
			path = manifestPrefixAnchor + path[len(hostPrefix)+1:]
		}
		if export := imports.LookupLinkPath(path); export != nil {
			// Gate on the direct-trace set: when the trace
			// recorded this target's direct link libs, only
			// attribute a flattened archive fragment if the
			// target links it DIRECTLY. A transitive-only
			// archive (in the static closure but not named in
			// target_link_libraries) is dropped — it reaches
			// the link through a directly-named public dep's
			// own Bazel deps, with correct visibility. Skip
			// the gate entirely when no trace covers this
			// target (directTraceLibs empty).
			if len(directTraceLibs) > 0 && !directTraceLibs[export.CMakeTarget] {
				continue
			}
			// addExport (not bare add): the export's manifest-declared
			// Deps ride along, which is what makes the transitive-only
			// drop above SOUND for prebuilt-backed labels — the dropped
			// flattened archives re-enter through the directly-named
			// export's declared closure instead of its (nonexistent)
			// Bazel-side deps.
			rt.addExport(export, scopeIsPrivate(traceLinkScope, export.CMakeTarget))
			continue
		}
		attributeUnresolvedLibPath(irt, rt, t, path, imports, findPkgAttrib, traceLinkScope)
	}

}

// lowerLinkAttribution lowers the link side of one target onto irt: the
// out-of-tree link-fragment attribution (imports-manifest rewrites,
// find_package variable-form routing, linkopt re-anchoring, the LTO
// feature) and the trace-driven IMPORTED dep recovery. Rationale comments
// are verbatim from lowerTarget's original inline region; the lc/tt
// destructure mirrors lowerTarget's header so the code reads identically.
func lowerLinkAttribution(irt *ir.Target, t *fileapi.Target, tt targetTrace, lc targetLowerCtx) {
	imports := lc.imports
	traceLinkLibs, traceLinkScope := tt.traceLinkLibs, tt.traceLinkScope
	// Out-of-tree link fragments. CMake records IMPORTED_LOCATION paths
	// in t.Link.CommandFragments[role="libraries"] as resolved absolute
	// paths under the synth-prefix tree. The orchestrator's imports
	// manifest carries each export's link paths so we can rewrite those
	// fragments to Bazel labels.
	//
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
	lowerLinkFragments(irt, t, tt, lc)

	// IMPORTED dep recovery from trace. Two channels miss find_package
	// imports that the trace records as direct target_link_libraries:
	//   - A STATIC archive runs no link step, so cmake's codemodel
	//     records no Link.CommandFragments and no IMPORTED-target
	//     Dependencies — both upstream channels are empty.
	//   - A header-only IMPORTED target (no .a / .so on disk, e.g.
	//     absl::flat_hash_map / absl::span) never appears as a link
	//     fragment on ANY consumer, EXECUTABLE or SHARED included, so
	//     the Link.CommandFragments attribution above can't reach it
	//     even though the executable's sources #include its headers.
	// The trace's target_link_libraries calls are the ground truth for
	// both. For each direct lib name the trace records, look it up in
	// the imports manifest; resolve hits are appended (deduped). Runs
	// for every cc rule kind (the router's seen-map keeps the
	// link-fragment path from double-adding the compiled libs it already
	// wired on executables/shared libs); in-codebase target names already
	// came in via t.Dependencies above and are covered by the seen-map
	// too. PRIVATE routing keys on the trace keyword for the bare lib
	// name (the call form the trace recorded), not the manifest's
	// CMakeTarget.
	if (t.Type == "STATIC_LIBRARY" || t.Type == "SHARED_LIBRARY" ||
		t.Type == "MODULE_LIBRARY" || t.Type == "EXECUTABLE") && len(traceLinkLibs) > 0 {
		rt := newLinkDepRouter(irt)
		for _, lib := range traceLinkLibs {
			if export := imports.LookupCMakeTarget(lib); export != nil {
				rt.addExport(export, traceLinkScope != nil && traceLinkScope[lib] == "PRIVATE")
			}
		}
	}
}

// umbrellaReanchor derives the workspace-promotion umbrella prefix
// (cmakeSrc-relative-to-labelRoot when the workspace root promoted
// labelRoot above cmakeSrc; LLVM: labelRoot=llvm-project/,
// cmakeSrc=llvm-project/llvm/ → "llvm") and the matching re-anchor
// closure. The single home for the promotion logic — lowerTarget's
// header and the source walk both consume it, so the two can't drift.
func (lc targetLowerCtx) umbrellaReanchor() (string, func(string) string) {
	umbrellaPrefix := ""
	if lc.hostSrc != "" && lc.hostSrc != lc.cmakeSrc {
		if rel, inside := relativeIfInside(lc.hostSrc, lc.cmakeSrc); inside && rel != "" && rel != "." {
			umbrellaPrefix = rel
		}
	}
	reanchor := func(rel string) string {
		if umbrellaPrefix != "" && rel != "" {
			return filepath.Join(umbrellaPrefix, rel)
		}
		return rel
	}
	return umbrellaPrefix, reanchor
}

// lowerTargetSources is lowerTarget's per-source walk: it classifies and
// re-anchors every codemodel source onto irt (srcs / hdrs / per-platform
// select() arms), recovers generated sources through the codegen
// machinery, applies the missing-source elision and the non-cc-extension
// catch-all, and stamps the per-class audit tags. Returns the codemodel
// source-index → emitted-srcs-path map (splitCompileGroups partitions the
// wrapper's srcs back into per-compile-group sub-libraries with it) and
// whether any compiled source was ELIDED (the srcs-less binary/test
// refusal gate). Bodies and rationale comments are verbatim from the
// original inline region; the lc/tt destructure and the umbrella/reanchor
// derivation mirror lowerTarget's header so the code reads identically.
//
// The walk is itself still over the lens thresholds (deep per-source
// classification nesting) — it carries the lowerTarget burndown's
// directive and shrinks in subsequent slices.
// lowerGeneratedSource handles one IsGenerated codemodel source in the
// walk: TARGET_OBJECTS refs and compiler artifacts skip, committed
// "generated" sources route through the regular source handling, and
// genuine generated sources recover their producing genrule and attach.
// Bodies and rationale comments are verbatim from the original inline
// branch; every original `continue` is a return here.
func lowerGeneratedSource(irt *ir.Target, t *fileapi.Target, src fileapi.TargetSource, i int, st *sourceWalkState, inCG bool, lc targetLowerCtx) error {
	cmakeSrc, cmakeBuild, hostSrc := lc.cmakeSrc, lc.cmakeBuild, lc.hostSrc
	g, cc := lc.g, lc.cc
	idToName := lc.idToName
	rejections := lc.rejections
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
		return nil
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
		st.elidedCompilerArtifact = true
		return nil
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
				if sp := attachGeneratedSource(irt, rel, inCG, false, ""); sp != "" {
					st.srcEmitPath[i] = sp
				}
				return nil
			}
		}
		// Purely generated IN-SOURCE output not on disk: cmake's
		// add_custom_command writes it into the SOURCE tree (libevent's
		// test/regress.gen.c via event_rpcgen.py with a source-dir
		// WORKING_DIRECTORY). recoverGenrule can't anchor a source-tree
		// output (it's not under the build dir), but the standalone-
		// custom-command walk emits a genrule producing it at this
		// element-relative path (see buildInSourceWorkdirGenrule); refer
		// to that output here instead of refusing. Gated on the producing
		// ninja edge being a CUSTOM_COMMAND so a genuinely-missing source
		// still refuses.
		if rel != "" && g != nil {
			// ninja keys an in-source output by its ABSOLUTE path; src.Path
			// here is cmakeSrc-relative, so look up the absolute form.
			abs := src.Path
			if !filepath.IsAbs(abs) {
				abs = filepath.Join(cmakeSrc, rel)
			}
			if b := g.BuildFor(abs); b != nil && b.Rule == "CUSTOM_COMMAND" {
				if sp := attachGeneratedSource(irt, rel, inCG, true, cc.CcEmbedSourceToHeader[rel]); sp != "" {
					st.srcEmitPath[i] = sp
				}
				// Sibling generated headers (the .c's foo.gen.h pair) are
				// staged by the stageSiblingGeneratedHeaders post-pass, which
				// is path-independent (covers consumers that get the .c via
				// other attribution paths + multi-config select arms).
				st.consumesCodegen = true
				return nil
			}
		}
	}
	// Authoritative wiring signal: if another lift already emitted a producer
	// for this exact build-dir output, attach to it instead of rejecting —
	// regardless of cmake's IsGenerated flag or whether a ninja CUSTOM_COMMAND
	// edge exists. configure_file / file(GENERATE) / execute_process record
	// their outputs in cc.OutToGenrule with NO CUSTOM_COMMAND edge for
	// recoverGenrule to find, so without this an IsGenerated source we DID
	// recover (e.g. a file(GENERATE) output, or an execute_process output the
	// project marked GENERATED via set_source_files_properties) would be
	// falsely rejected here. Mirrors the ordinary-source build-dir path
	// (recoverOrElideBuildDirSource); OutToGenrule keys are build-dir-relative.
	if brel, inside := relativeIfInside(cmakeBuild, src.Path); inside {
		if cc.outputClaimed(brel) {
			st.consumesCodegen = true
			if sp := attachGeneratedSource(irt, brel, inCG, false, cc.CcEmbedSourceToHeader[brel]); sp != "" {
				st.srcEmitPath[i] = sp
			}
			return nil
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
			return nil
		}
		return err
	}
	st.consumesCodegen = true
	// Generated output: drop a non-cc build artifact (VTK wrap-hierarchy
	// .args/.data), route a compile-group source to srcs (+ its cc_embed
	// sibling header), a header to hdrs, else to srcs. See
	// attachGeneratedSource.
	if sp := attachGeneratedSource(irt, relOut, inCG, true, cc.CcEmbedSourceToHeader[relOut]); sp != "" {
		st.srcEmitPath[i] = sp
	}
	return nil
}

// sourceWalkState carries the per-target flags and the source-index →
// emitted-path map across the walk's branches.
type sourceWalkState struct {
	consumesCodegen        bool
	elidedBuildDirSrc      bool
	elidedMissingSrc       bool
	elidedCompilerArtifact bool
	declaredGeneratedSrc   bool
	droppedNonCcSrc        bool
	srcEmitPath            map[int]string
}

// lowerOrdinarySource handles one non-generated codemodel source in the
// walk: header-extension routing for compile-group-less entries, path
// normalization against the label root, on-disk existence elision,
// HEADER_FILE_ONLY-style reclassification, and the srcs append. Bodies
// and rationale comments are verbatim from the original inline tail of
// the loop; every original `continue` is a return here.
// routeCompileGrouplessSource handles a source outside every compile
// group (typically a target_sources() header): explicit header
// extensions route to hdrs so they aren't silently dropped when the
// target declares no include dirs; non-header / out-of-tree /
// dot-dot-segment entries drop (the include-dir walk owns them). The
// entry never falls through to the compiled-source path. Bodies and
// rationale comments are verbatim from the original inline branch.
func routeCompileGrouplessSource(irt *ir.Target, src fileapi.TargetSource, lc targetLowerCtx) {
	cmakeSrc := lc.cmakeSrc
	_, reanchor := lc.umbrellaReanchor()
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
		rel, inside := relativeIfInside(cmakeSrc, srcPath)
		if !inside {
			routeCompileGrouplessAbsSource(irt, src, lc)
			return
		}
		srcPath = rel
	}
	if pathHasDotDotSegment(srcPath) {
		recordElidedSource(lc.cc, irt.Name, src.Path, "non-compile-group-source")
		return
	}
	ext := strings.ToLower(filepath.Ext(srcPath))
	if cclang.IsHeaderExt(ext) {
		irt.Hdrs = append(irt.Hdrs, reanchor(srcPath))
	}
}

// routeCompileGrouplessAbsSource handles the absolute-path entries
// routeCompileGrouplessSource can't relativize against the source tree.
// A build-dir target_sources() entry (typically a generated header): a
// recovered producer covers it — the consumer attribution attaches it —
// so nothing drops; cmake bookkeeping stays elided by design; otherwise
// the on-disk bake recovers it like the compiled-source path, attaching
// as a header. Only a byteless orphan records. An out-of-tree absolute
// entry has nothing staging it — account the drop.
func routeCompileGrouplessAbsSource(irt *ir.Target, src fileapi.TargetSource, lc targetLowerCtx) {
	rel, insideBuild := relativeIfInside(lc.cmakeBuild, src.Path)
	if !insideBuild {
		recordElidedSource(lc.cc, irt.Name, src.Path, "non-compile-group-source")
		return
	}
	if _, produced := lc.cc.OutToGenrule[rel]; produced {
		return
	}
	if strings.HasPrefix(rel, "CMakeFiles/") {
		return
	}
	if _, baked := bakeBuildDirFile(rel, lc); baked {
		irt.Hdrs = append(irt.Hdrs, rel)
		if !stringSliceContains(irt.Tags, "has-cmake-codegen") {
			irt.Tags = append(irt.Tags, "has-cmake-codegen")
		}
		return
	}
	recordElidedSource(lc.cc, irt.Name, src.Path, "non-compile-group-source")
}

// elideBuildDirOnlySource handles a configure-time-created file living
// under the build dir with no recovered generator edge: elide it (tagged)
// rather than emit an unbuildable reference. Returns true when the entry
// was handled (elided or attached); false falls through to the ordinary
// path handling. Bodies and rationale comments are verbatim from the
// original inline branch.
func elideBuildDirOnlySource(irt *ir.Target, src fileapi.TargetSource, i int, st *sourceWalkState, inCG bool, lc targetLowerCtx) bool {
	cmakeBuild := lc.cmakeBuild
	if cmakeBuild == "" || !filepath.IsAbs(src.Path) {
		return false
	}
	rel, inside := relativeIfInside(cmakeBuild, src.Path)
	if !inside {
		return false
	}
	recoverOrElideBuildDirSource(irt, src, rel, i, st, inCG, lc)
	return true
}

// recoverOrElideBuildDirSource runs the recovery ladder for one
// build-dir source: wire a recovered producer, keep cmake bookkeeping
// elided by design, bake on-disk bytes, and only then elide — loudly.
// Bodies and rationale comments are verbatim from the original inline
// branch of elideBuildDirOnlySource.
func recoverOrElideBuildDirSource(irt *ir.Target, src fileapi.TargetSource, rel string, i int, st *sourceWalkState, inCG bool, lc targetLowerCtx) {
	cc := lc.cc
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
	if cc.outputClaimed(rel) {
		st.consumesCodegen = true
		if sp := attachGeneratedSource(irt, rel, inCG, false, cc.CcEmbedSourceToHeader[rel]); sp != "" {
			st.srcEmitPath[i] = sp
		}
		return
	}
	// cmake bookkeeping under CMakeFiles/ (PCH compile stubs
	// like cmake_pch.hxx.cxx, re-run markers) is cmake-internal
	// — its semantics are handled by dedicated lifts (the PCH
	// forced-include lift), so the by-design elide stands, tag
	// only (the meta-cmake-pch gate pins that these never leak
	// into the BUILD).
	if strings.HasPrefix(rel, "CMakeFiles/") {
		st.elidedBuildDirSrc = true
		return
	}
	// On-disk bake fallback (no silent drops of anything in the
	// codemodel): the configure already CREATED this file — by a
	// writer the trace classifiers don't model (file(WRITE) /
	// APPEND / COPY / TOUCH / DOWNLOAD, custom macros) — so the
	// bytes are sitting in the build dir. Bake them rather than
	// emitting a rule that's missing an input cmake compiled.
	// See build_dir_source_bake.go.
	if _, baked := bakeBuildDirFile(rel, lc); baked {
		st.consumesCodegen = true
		if sp := attachGeneratedSource(irt, rel, inCG, false, cc.CcEmbedSourceToHeader[rel]); sp != "" {
			st.srcEmitPath[i] = sp
		}
		return
	}
	// Bytes unavailable (offline replay without a recorded
	// mirror): the elide stands, but LOUDLY — recorded for the
	// end-of-lower aggregate warning + source-elided todo, with
	// the per-target tag kept for BUILD-side audits.
	recordElidedSource(cc, irt.Name, src.Path, "build-dir-source")
	st.elidedBuildDirSrc = true
}

func lowerOrdinarySource(irt *ir.Target, t *fileapi.Target, src fileapi.TargetSource, i int, st *sourceWalkState, inCG bool, lc targetLowerCtx) error {
	cmakeSrc, cmakeBuild, hostSrc := lc.cmakeSrc, lc.cmakeBuild, lc.hostSrc
	hostSrcOnDisk := lc.hostSrcOnDisk
	generatedSources, rejections := lc.generatedSources, lc.rejections
	workspaceRoot := lc.workspaceRoot
	if !inCG {
		routeCompileGrouplessSource(irt, src, lc)
		return nil
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
	if elideBuildDirOnlySource(irt, src, i, st, inCG, lc) {
		return nil
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
			return nil
		}
		return failure.New(failure.UnsupportedSourcePath,
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
			return nil
		}
		return failure.New(failure.UnsupportedSourcePath,
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
			return nil
		}
		return failure.New(failure.UnsupportedSourcePath,
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
				st.declaredGeneratedSrc = true
				// A GENERATED target-source that isn't a cc compile/link/header
				// input is a build-ordering artifact, not a source — VTK lists
				// each module's wrap-hierarchy files (CMakeFiles/<mod>-hierarchy.
				// *.args/.data, incl. dependency modules' across packages) as the
				// module target's GENERATED sources. Keeping it in cc srcs fails
				// analysis; it has no cc-rule role (the producing genrule builds
				// independently), so drop it from the target.
				if !isCcSrcEntry(src.Path) {
					return nil
				}
				irt.Srcs = append(irt.Srcs, src.Path)
				st.srcEmitPath[i] = src.Path
				return nil
			}
			recordElidedSource(lc.cc, irt.Name, src.Path, "missing-source")
			st.elidedMissingSrc = true
			return nil
		}
	}
	irt.Srcs = append(irt.Srcs, src.Path)
	st.srcEmitPath[i] = src.Path
	return nil
}

func lowerTargetSources(irt *ir.Target, t *fileapi.Target, tt targetTrace, lc targetLowerCtx) (map[int]string, bool, error) {
	// Build the source-index → in-compile-group set once per target.
	// Walking t.CompileGroups[].SourceIndexes per source visit was
	// O(N*M) where N is sources and M is compile-group entries; for
	// large targets (mesa, gcc, clang) that's the dominant cost in
	// lowerTarget.
	inCompileGroup := buildCompileGroupSet(t)

	st := &sourceWalkState{}
	// codemodel source-index → the path actually stored in irt.Srcs. The
	// codemodel records generated sources with absolute build-dir paths
	// while the walk below relativizes them (CMakeFiles/foo.S), so the two
	// can't be matched by string equality. splitCompileGroups uses this
	// map to partition the wrapper's srcs back into per-compile-group
	// sub-libraries by the codemodel's SourceIndexes (OpenBLAS's kernel:
	// 65 generated .S in an ASM group, the rest .c in a C group).
	st.srcEmitPath = make(map[int]string, len(t.Sources))
	for i, src := range t.Sources {
		// CMake's bookkeeping `<build>/version.h.rule` files are internal
		// re-run markers; skip them silently. Same for the PCH machinery
		// sources (the generated cmake_pch creator TU + header) cmake
		// lists on a declaring target: the mirror lift owns the
		// semantics, and eliding them as "missing build-dir sources"
		// would stamp a misleading audit tag on every PCH target.
		if strings.HasSuffix(src.Path, ".rule") || isCMakePCHMachinerySource(src.Path) {
			continue
		}

		if src.IsGenerated {
			if err := lowerGeneratedSource(irt, t, src, i, st, inCompileGroup[i], lc); err != nil {
				return nil, false, err
			}
			continue
		}

		if err := lowerOrdinarySource(irt, t, src, i, st, inCompileGroup[i], lc); err != nil {
			return nil, false, err
		}
	}

	// Catch-all over every src-append path above: a cc/cuda rule rejects a srcs
	// entry that isn't a compile/link/header input. Some GENERATED build-order
	// artifacts reach srcs through cmake's compile-group filing (VTK files each
	// module's wrap-hierarchy .args/.data under a compile group), past the
	// per-branch handling. Drop any entry whose extension is clearly non-cc; the
	// producing genrule still builds via //... and the artifact has no cc-rule
	// role. Only entries WITH a non-cc extension are dropped, so bare-name /
	// extensionless labels (`:foo`, `//pkg:foo`) are never touched.
	//
	// Fortran sources (.f / .f90 / ...) are explicitly KEPT here even though a
	// cc rule can't compile them: the per-language compile-group split plus the
	// pkg-level retagFortranTargets pass lower them to a fortran_library (the cc
	// toolchain's gfortran driver). Dropping them here would silently lose a
	// cmake Fortran target (OpenBLAS's reference LAPACK) before the retag ever
	// sees it.
	switch irt.Kind {
	case ir.KindCCLibrary, ir.KindCCBinary, ir.KindCCTest,
		ir.KindCudaLibrary, ir.KindCudaBinary, ir.KindCudaTest:
		kept := make([]string, 0, len(irt.Srcs))
		for _, s := range irt.Srcs {
			if filepath.Ext(s) != "" && !isCcSrcEntry(s) && !isFortranSrc(s) {
				st.droppedNonCcSrc = true
				continue
			}
			kept = append(kept, s)
		}
		irt.Srcs = kept
	}

	if st.consumesCodegen {
		irt.Tags = append(irt.Tags, "has-cmake-codegen")
	}
	if st.elidedBuildDirSrc {
		irt.Tags = append(irt.Tags, "cmake-elided-build-dir-source")
	}
	if st.elidedMissingSrc {
		irt.Tags = append(irt.Tags, "cmake-elided-missing-source")
	}
	if st.elidedCompilerArtifact {
		irt.Tags = append(irt.Tags, "cmake-elided-compiler-artifact")
	}
	if st.declaredGeneratedSrc {
		// A source marked GENERATED via set_source_files_properties
		// that isn't on disk was kept (not elided) because a
		// generator is expected to produce it. Tag so operators can
		// audit which targets consume a manually-declared generated
		// input — and so they can wire the producing genrule /
		// configure_file edge if the converter didn't recover one.
		irt.Tags = append(irt.Tags, "cmake-declared-generated-source")
	}
	if st.droppedNonCcSrc {
		// A non-cc-input source (e.g. a VTK wrap-hierarchy .args/.data build-order
		// artifact cmake filed under the module target) was dropped from cc srcs.
		irt.Tags = append(irt.Tags, "cmake-dropped-non-cc-src")
	}

	return st.srcEmitPath, st.elidedBuildDirSrc || st.elidedMissingSrc || st.elidedCompilerArtifact, nil
}

// finalizeLoweredTarget is lowerTarget's tail: the CTest classification
// (an EXECUTABLE registered via add_test() rewrites into cc_test rules
// and drops the binary), the multi-language compile-group split, the
// platform-conditional source partition (Tier 1 move + Tier 2
// injection), and the OpenMP link-flag mirror. Bodies and rationale
// comments are verbatim from the original inline region.
func finalizeLoweredTarget(irt *ir.Target, t *fileapi.Target, tt targetTrace, lc targetLowerCtx, srcEmitPath map[int]string, pchCtx pchLiftCtx) (*ir.Target, error) {
	cmakeSrc, cmakeBuild := lc.cmakeSrc, lc.cmakeBuild
	cc, tests := lc.cc, lc.tests
	platformConditionalSrcs, platformConditionalSrcsToAdd := tt.platformConditionalSrcs, tt.platformConditionalSrcsToAdd
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
		if err := splitCompileGroups(t, irt, cc, cmakeSrc, cmakeBuild, srcEmitPath, pchCtx); err != nil {
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

// lowerCompileGroups is lowerTarget's compile-group region: it lowers
// the first compile group's flags / defines / language standard onto
// irt, partitions PUBLIC vs PRIVATE include dirs (PRIVATE rides -I
// copts), collects the build-dir-rooted includes the consumer
// attribution below keys on, and mirrors the extraction for
// INTERFACE_LIBRARY targets from their HEADERS FileSets. Returns the
// build-dir include set, the walk-package-root signal, and the PRIVATE
// include dirs the discoverHeaders walk must also cover. Bodies and
// rationale comments are verbatim from the original inline region.
// lowerInterfaceFileSetIncludes mirrors the compile-group include
// extraction for INTERFACE_LIBRARY targets from their HEADERS FileSets'
// BaseDirectories (they have no CompileGroups). Returns the updated
// walk-package-root signal. Bodies and rationale comments are verbatim
// from the original inline region.
func lowerInterfaceFileSetIncludes(irt *ir.Target, t *fileapi.Target, cmakeSrc string, walkPkgRootForHdrs bool) bool {
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

	return walkPkgRootForHdrs
}

// partitionTargetIncludeDirs walks one compile group's include dirs:
// build-dir includes record into targetBuildIncs (consumer attribution),
// source-tree includes dedup into irt.Includes or — when the trace marks
// them PRIVATE — into -I copts, with the package-root case routed to the
// discoverHeaders walk. Returns the updated walk-package-root signal and
// the PRIVATE include dirs collected for that walk. Bodies and rationale
// comments are verbatim from the original inline region.
// recordBuildDirInclude handles one BUILD-dir include of the partition
// loop: track it for consumer attribution, and surface it on
// irt.Includes when the umbrella promotion or a generated-output root
// requires the -I at Bazel time. Bodies and rationale comments are
// verbatim from the original inline branch.
func recordBuildDirInclude(irt *ir.Target, rel, umbrellaPrefix string, seenInc, targetBuildIncs map[string]bool, cc *codegenContext) {
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
	// A build-dir include that is the ROOT of generated outputs
	// (cmake's `-I<build>/gens` for grpc's protoc gens dir) must be
	// surfaced on `includes` too: the generated `.pb.cc` does a
	// full-path `#include "src/proto/.../x.pb.h"`, which only
	// resolves with the gens root on the include path. The headers
	// are declared in hdrs (staged), but Bazel still needs the
	// `-I<root>` to find them. Gated on the dir actually holding a
	// genrule output, so non-generated build-dir includes (tracked
	// for configure_file consumer attribution above) aren't surfaced.
	if !seenInc[rel] && cc != nil && isGeneratedOutputRoot(rel, cc.OutToGenrule) {
		seenInc[rel] = true
		irt.Includes = append(irt.Includes, rel)
	}
}

func partitionTargetIncludeDirs(irt *ir.Target, t *fileapi.Target, cg fileapi.CompileGroup, tt targetTrace, lc targetLowerCtx, targetBuildIncs map[string]bool, walkPkgRootForHdrs bool, privateIncDirs []string) (bool, []string) {
	cmakeSrc, cmakeBuild, hostPrefix := lc.cmakeSrc, lc.cmakeBuild, lc.hostPrefix
	workspaceRoot := lc.workspaceRoot
	cc, imports := lc.cc, lc.imports
	privateIncludeDirs, publicIncludeDirs, includeDirsGlobal := tt.privateIncludeDirs, tt.publicIncludeDirs, tt.includeDirsGlobal
	interfaceIncludeDirs := tt.interfaceIncludeDirs
	umbrellaPrefix, reanchor := lc.umbrellaReanchor()
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
			recordBuildDirInclude(irt, rel, umbrellaPrefix, seenInc, targetBuildIncs, cc)
			continue
		}
		rel, ok := relativeIfInside(cmakeSrc, inc.Path)
		if !ok && workspaceRoot != "" {
			// In-element include that sits OUTSIDE the cmake source root but
			// INSIDE the (promoted) label root — a sample under a wider
			// overlay reaching a sibling subtree: cuda-samples'
			// `include_directories(../../../Common)` from
			// cpp/<group>/<sample>/ resolves to the repo-root `Common/`,
			// which the overlay staged under the element. Anchor it
			// labelRoot-relative (`Common`) and surface it as an include
			// root so split synthesizes its header lib and this target deps
			// on it. (find_package include trees live under hostPrefix, not
			// the label root, so they fall through to the umbrella handling
			// below.)
			if wrel, inside := relativeIfInside(workspaceRoot, inc.Path); inside && wrel != "" && wrel != "." {
				if !seenInc[wrel] {
					seenInc[wrel] = true
					irt.Includes = append(irt.Includes, wrel)
				}
				continue
			}
		}
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
			// find_package whole-include-tree umbrella: a
			// find_package(<Pkg>) puts Pkg's entire include
			// dir on this target's compile line (the dir
			// resolves outside the source/build tree, so we'd
			// otherwise drop it here). If the imports manifest
			// declared this dir as one of an element's
			// UmbrellaIncludeRoots, add the element's umbrella
			// label to deps instead — a cc_library re-exporting
			// the package's full public header surface — so
			// headers the consumer #includes but never linked a
			// specific target for (absl/functional/overload.h
			// from protobuf, never in protobuf_ABSL_USED_TARGETS)
			// resolve under Bazel's strict per-target headers.
			if umbrella := imports.UmbrellaForIncludeDir(inc.Path); umbrella != "" {
				if !stringSliceContains(irt.Deps, umbrella) {
					irt.Deps = append(irt.Deps, umbrella)
				}
			}
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
		// A directory-scoped include_directories() dir is PRIVATE (cmake
		// never exports it via INTERFACE_INCLUDE_DIRECTORIES), so route it to a
		// `-I` copt like a PRIVATE target_include_directories — UNLESS this
		// target re-exports the same dir PUBLIC, which genuinely propagates.
		// Without this, OpenBLAS's `include_directories(include …)` LAPACKE dir
		// defaults to the transitive `includes` and over-propagates to ~1700
		// consumer TUs cmake never gave it to.
		dirScopedPrivate := includeDirsGlobal[inc.Path] && !publicIncludeDirs[inc.Path]
		// B1: interface-driven scope. When the probe captured this target, an
		// include in its resolved set but NOT in its
		// INTERFACE_INCLUDE_DIRECTORIES is PRIVATE — route it to a `-I` copt
		// (split → implementation_deps) so it doesn't propagate. Only reaches
		// here for source-tree includes (build-dir / generated-header dirs are
		// handled by earlier branches that `continue`), and a dir the target
		// re-exports stays in `public`. Generalizes the trace-only
		// dirScopedPrivate to every private-include mechanism.
		ifacePrivate := interfaceIncludeDirs.active && !interfaceIncludeDirs.public[inc.Path]
		if privateIncludeDirs[inc.Path] || dirScopedPrivate || ifacePrivate {
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
			// Emit the PRIVATE include dir in element-root-relative form
			// (`-Iinclude`, not the exec-root `-Ielements/<pkg>/include`):
			// split's rewriteTarget copt scan keys on this form to wire the
			// synthesized header lib for the dir (staging its headers + the
			// correct exec-root search path via the lib's `includes`) and drop
			// the bare -I. Anchoring to exec-root here breaks that match and
			// leaves the dir's headers unstaged (fmt's posix-mock-test:
			// `#include <fmt/os.h>` "No such file"). NOTE: this only stages
			// headers when the dir is ALSO a public include root somewhere (so
			// a header lib exists for it); a PRIVATE-only dir whose headers
			// aren't otherwise declared still needs the header-staging work
			// tracked in ROADMAP (mbedtls / sdl).
			irt.Copts = append(irt.Copts, flag+emit)
			privateIncDirs = append(privateIncDirs, emit)
			continue
		}
		irt.Includes = append(irt.Includes, emit)
	}
	return walkPkgRootForHdrs, privateIncDirs
}

func lowerCompileGroups(irt *ir.Target, t *fileapi.Target, tt targetTrace, lc targetLowerCtx, pchCtx pchLiftCtx) (map[string]bool, bool, []string) {
	cmakeSrc, cmakeBuild := lc.cmakeSrc, lc.cmakeBuild
	cc := lc.cc
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

	// PRIVATE include dirs (emitted as -I copts below) collected so the
	// discoverHeaders walk further down also walks them — otherwise a
	// PRIVATE-only dir's headers are never declared as inputs and a bare
	// `#include` into it fails in the sandbox (the dir's -I sets the search
	// path but stages no files). See ROADMAP "Stage headers from a PRIVATE
	// include dir with no public header lib".
	var privateIncDirs []string

	//nolint:nestif // inside the grandfathered lowerTarget giant; folds into its ROADMAP burndown pass.
	if cg, ok := firstRealCompileGroup(t); ok {
		// M1 assumption: at most one language per target. Aggregate the
		// first REAL compile group's flags/includes/defines — skipping
		// cmake's PCH-creator group, whose `-x c++-header` fragments
		// would otherwise compile every project TU as a header (the
		// shape executables hit: they don't split groups, so the
		// creator group used to be picked verbatim).
		copts, defs, pchArtifacts := splitCompileFragments(cg.CompileCommandFragments)
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
		// PCH forced-include lift: expand the cmake_pch artifacts the
		// fragment split withheld into the declared headers' ordered
		// `-include` copts, staging + tagging as needed (see pch.go).
		copts, irt.Srcs = pchCtx.apply(pchArtifacts, cg, copts, irt.Srcs, irt.Hdrs, irt)
		// Source-tree -idirafter dirs: exec-root re-anchor + header-walk
		// staging (see idirafter.go). The walk dirs ride the same
		// privateIncDirs channel PRIVATE -I dirs use.
		var idirWalk []string
		copts, idirWalk = reanchorIDirAfterCopts(copts, pchCtx.cmakeSrc, pchCtx.reanchor, pchCtx.pkgPath)
		privateIncDirs = append(privateIncDirs, idirWalk...)
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

		// DEFINE_SYMBOL export-macro routing — see applyExportMacro.
		applyExportMacro(irt, t.Type, t.Name, tt.defineSymbol)

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

		// Tag targets that declare target_precompile_headers even when no
		// cmake_pch fragment reached the compile line (the artifact-driven
		// shapes — incl. REUSE_FROM consumers — are tagged inside
		// applyPCHForcedIncludes). The forced-include semantics are
		// preserved by the lift above; the tag keeps the remaining
		// residual — the compile-SPEED effect of actual precompilation,
		// which Bazel cc_library can't express — greppable for operators
		// (see docs/operator-toolchain-features.md).
		for _, cgPCH := range t.CompileGroups {
			if len(cgPCH.PrecompileHeaders) > 0 {
				if !stringSliceContains(irt.Tags, "cmake-codegen-pch") {
					irt.Tags = append(irt.Tags, "cmake-codegen-pch")
				}
				break
			}
		}

		walkPkgRootForHdrs, privateIncDirs = partitionTargetIncludeDirs(irt, t, cg, tt, lc, targetBuildIncs, walkPkgRootForHdrs, privateIncDirs)
	}

	walkPkgRootForHdrs = lowerInterfaceFileSetIncludes(irt, t, cmakeSrc, walkPkgRootForHdrs)

	return targetBuildIncs, walkPkgRootForHdrs, privateIncDirs
}

// lowerTarget is a tracked complexity giant (cognitive ~213, down from 754 —
// the highest in the tree). Breaking it down into focused, behavior-preserving
// sub-pass extractions (link-fragment attribution, compile-group lowering,
// generated-source handling) is its own ROADMAP "complexity lens" burndown
// pass; grandfathered here so the lens can gate as blocking on every other
// function. Remove the directive below as the function comes back under
// threshold. See ROADMAP.md.
//
// attributeGeneratedConsumers is lowerTarget's consumer-attribution
// family: a target whose codemodel includes contain the build dir (or a
// subdir) is a candidate consumer of configure_file / file(GENERATE) /
// execute_process outputs that landed inside that include path — each
// matching output attaches to hdrs (with the same-directory CONFIG-header
// staging in between). Bodies and rationale comments are verbatim from
// the original inline region.
// attributeGeneratedConsumers is lowerTarget's consumer-attribution
// family, one sub-pass per recovered-output channel; each block's
// rationale lives on its helper.
func attributeGeneratedConsumers(irt *ir.Target, t *fileapi.Target, lc targetLowerCtx, targetBuildIncs map[string]bool) {
	attributeConfigureFileConsumers(irt, lc, targetBuildIncs)
	stageSameDirConfigHeaders(irt, t, lc)
	attributeFileGenerateConsumers(irt, lc, targetBuildIncs)
	attributeExecuteProcessConsumers(irt, lc, targetBuildIncs)
}

// attributeConfigureFileConsumers attaches recovered configure_file
// outputs to a consumer whose includes contain the build dir, with the
// export-header own-dir and package-root genfiles handling. Bodies and
// rationale comments are verbatim from the original inline block.
func attributeConfigureFileConsumers(irt *ir.Target, lc targetLowerCtx, targetBuildIncs map[string]bool) {
	bazelPackagePath := lc.bazelPackagePath
	configureFiles := lc.configureFiles
	// configure_file consumer attribution. Any target whose
	// codemodel-recorded includes contain the cmake build dir
	// (or a subdir thereof) is a candidate consumer of
	// configure_file outputs that landed inside that include
	// path. We add each matching output to the target's hdrs;
	// the genrule that produces it is already on cc.Genrules
	// from recoverConfigureFiles. cmake-codegen tag mirrors the
	// existing CUSTOM_COMMAND-recovered consumer's shape.
	//nolint:nestif // inside the grandfathered lowerTarget giant; folds into its ROADMAP burndown pass.
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

}

// stageSameDirConfigHeaders stages same-directory configure_file CONFIG
// headers (kwsysPrivate.h-shape). Bodies and rationale comments are
// verbatim from the original inline block.
func stageSameDirConfigHeaders(irt *ir.Target, t *fileapi.Target, lc targetLowerCtx) {
	configureFiles := lc.configureFiles
	// Same-directory configure_file CONFIG HEADERS. A header like kwsysPrivate.h
	// (configure_file ... COPYONLY) or proj_config.h is #included by BARE quote
	// name (`#include "kwsysPrivate.h"`) from a source in the SAME directory, so
	// cmake needs no `-I` for it (a quote-include resolves same-dir) — which means
	// targetBuildIncs never records the dir and the prefix-match block above
	// misses it entirely (it's gated on a build-include match). In Bazel a
	// quote-include still needs the header DECLARED as an input of the compiling
	// target. Attribute any header configure_file output whose directory matches a
	// directory this target compiles sources in (compared in the element-root-
	// relative space both irt.Srcs and the recovered output already use). These
	// ride into the per-language sub-libraries via splitCompileGroups' sharedHdrs.
	if len(configureFiles) > 0 && len(irt.Srcs) > 0 {
		srcDirs := map[string]bool{}
		for _, s := range irt.Srcs {
			srcDirs[filepath.ToSlash(filepath.Dir(s))] = true
		}
		have := map[string]bool{}
		for _, h := range irt.Hdrs {
			have[h] = true
		}
		var extra []string
		extraDirs := map[string]bool{}
		for _, cfgOut := range configureFiles {
			if !cclang.IsHeader(cfgOut.RelOutput) {
				continue
			}
			d := filepath.ToSlash(filepath.Dir(cfgOut.RelOutput))
			if !srcDirs[d] {
				continue
			}
			// Surface the OWN directory of every same-dir configure_file header
			// this target compiles against, REGARDLESS of whether the header is
			// already in hdrs. The prefix-match pass above (gated on a build-dir
			// `-I` match) may have already added the header to hdrs and settled
			// its hosting include on a shallower PARENT dir (VTK kwsys: the
			// vtksys sources route to //…/Utilities/KWSys:…_headers with
			// includes=["."] at the PARENT). That parent include doesn't put the
			// vtksys genfiles subdir — where kwsysPrivate.h is generated — on the
			// search path, so the bare `#include "kwsysPrivate.h"` stays
			// unresolved. Recording the dir here (decoupled from the have[] gate)
			// lets the includes addition below put exactly that genfiles subdir on
			// the path.
			extraDirs[d] = true
			if have[cfgOut.RelOutput] {
				continue
			}
			extra = append(extra, cfgOut.RelOutput)
			have[cfgOut.RelOutput] = true
		}
		if len(extra) > 0 {
			irt.Hdrs = append(irt.Hdrs, extra...)
			irt.Tags = append(irt.Tags, "has-cmake-codegen")
		}
		// Staging the header as an input is necessary but NOT sufficient: it's a
		// GENERATED file (lands in genfiles, not the source tree), and the
		// consuming .c #includes it by BARE same-dir quote. cmake resolves that
		// against the source dir (where a same-named source copy sits) or its own
		// CMAKE_CURRENT_BINARY_DIR; under Bazel the compiler's same-dir search
		// walks only the .c's SOURCE dir and misses the genfiles copy. Add the
		// header's OWN directory to Includes so the genfiles variant of exactly
		// that dir is on the search path (split synthesizes its header lib / the
		// literal include as needed), mirroring stageSiblingGeneratedHeaders for
		// genrule siblings. Runs whenever any such dir was recorded — even if the
		// header was already staged in hdrs by the prefix-match pass above.
		if len(extraDirs) > 0 {
			haveInc := make(map[string]bool, len(irt.Includes))
			for _, inc := range irt.Includes {
				haveInc[inc] = true
			}
			for d := range extraDirs {
				if d == "" || d == "." {
					continue
				}
				if !haveInc[d] {
					irt.Includes = append(irt.Includes, d)
					haveInc[d] = true
				}
			}
		}
	}

}

// attributeFileGenerateConsumers is the file(GENERATE) sister of the
// configure_file attribution. Bodies and rationale comments are verbatim
// from the original inline block.
func attributeFileGenerateConsumers(irt *ir.Target, lc targetLowerCtx, targetBuildIncs map[string]bool) {
	fileGenerates := lc.fileGenerates
	// file(GENERATE) consumer attribution. Sister block to the
	// configure_file walk above; file(GENERATE) outputs can be
	// any extension (config-shaped .h, license blobs, generated
	// source, .json manifests, etc.) so we branch on extension
	// like the execute_process walk below — headers go to hdrs,
	// other artefacts go to srcs.
	if len(fileGenerates) > 0 && len(targetBuildIncs) > 0 {
		seen := map[string]bool{}
		attached := false
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
			// header→hdrs, non-cc build artifact (e.g. VTK's per-config
			// wrap-hierarchy `.args`)→data, cc input→srcs. dropNonCc=false:
			// non-cc outputs are same-package here, so data is safe. See
			// classifyAndAttach.
			if classifyAndAttach(irt, fg.RelOutput, seen, false) {
				attached = true
			}
		}
		if attached {
			irt.Tags = append(irt.Tags, "has-cmake-codegen")
		}
	}

}

// attributeExecuteProcessConsumers is the execute_process sister of the
// configure_file attribution (headers to hdrs, other artefacts to srcs).
// Bodies and rationale comments are verbatim from the original inline
// block.
func attributeExecuteProcessConsumers(irt *ir.Target, lc targetLowerCtx, targetBuildIncs map[string]bool) {
	executeProcesses := lc.executeProcesses
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
		seen := map[string]bool{}
		// Seed with sources/headers/data already on the target so an
		// execute_process output that is ALSO a codemodel source isn't attached a
		// second time (classifyAndAttach's own seen dedups only within this pass,
		// not against pre-existing entries) — Bazel rejects the duplicate
		// ("attribute srcs has duplicate entries"). Note the in==out
		// link_to_source shape (mbedtls's committed error.c symlinked onto its
		// own build-dir path) no longer reaches here at all: emitCopyGenrule's
		// identity drop surfaces NO output for it, since a per-target seen-dedup
		// can't help the OTHER targets whose includes also cover the path — they
		// don't carry it in srcs and would each attach it (the mbedtls
		// three-way source duplication the symbol-fidelity lens caught).
		for _, s := range irt.Srcs {
			seen[s] = true
		}
		for _, h := range irt.Hdrs {
			seen[h] = true
		}
		for _, d := range irt.Data {
			seen[d] = true
		}
		attached := false
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
			// Same routing as the file(GENERATE) block above (header→hdrs,
			// non-cc→data, cc→srcs); the shared classifyAndAttach is why the
			// VTK `.args/.data` fix now lives in exactly one place.
			if classifyAndAttach(irt, ep.RelOutput, seen, false) {
				attached = true
			}
		}
		if attached {
			irt.Tags = append(irt.Tags, "has-cmake-codegen")
		}
	}

}

// stageTargetHeaders is lowerTarget's header-staging family: the
// discoverHeaders walk over the declared include roots (plus the
// package-root and PRIVATE-dir extensions), the own-source-dir sibling
// staging, the FileSets-declared headers, the Phase 7 header-only
// include_prefix idiom shaping, and the srcs-less binary/test refusal.
// The (nil, nil) return drops a UTILITY-shaped rule; errors are typed
// refusals. Bodies and rationale comments are verbatim from the original
// inline region.
func stageTargetHeaders(irt *ir.Target, t *fileapi.Target, lc targetLowerCtx, targetBuildIncs map[string]bool, walkPkgRootForHdrs bool, privateIncDirs []string, srcElided bool) (*ir.Target, error) {
	cmakeSrc, cmakeBuild, hostSrc, hostSrcOnDisk := lc.cmakeSrc, lc.cmakeBuild, lc.hostSrc, lc.hostSrcOnDisk
	cc, rejections := lc.cc, lc.rejections
	_, reanchor := lc.umbrellaReanchor()
	// Include the package root in the header-discovery walk when
	// the target's target_include_directories surfaced the empty-
	// relative entry (the rel=="" drop above sets the flag). The
	// emit-side `includes = [...]` slot stays untouched (Bazel
	// rejects `[""]`); hdrs discovery picks up the same files.
	includesForWalk := irt.Includes
	if walkPkgRootForHdrs {
		includesForWalk = append([]string{""}, irt.Includes...)
	}
	// Also walk PRIVATE include dirs so their headers are discovered + declared
	// (the -I copt alone stages no files). Feeds hdrs discovery only — these
	// rode into copts, not irt.Includes, preserving cmake's PRIVATE scope.
	if len(privateIncDirs) > 0 {
		includesForWalk = append(append([]string{}, includesForWalk...), privateIncDirs...)
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
	// A wired include whose headers are RECOVERED (a build-dir include,
	// or any dir that is a generated-output root now that the build-dir
	// bake at lowerTarget has fully populated cc.OutToGenrule) must not
	// be flagged "missing on disk" when the hostSrc walk can't find it —
	// its content lives in the build tree, staged by the bake +
	// consumer attribution, not here. See discoverHeaders' recordMissing.
	covered := func(inc string) bool {
		return targetBuildIncs[inc] || isGeneratedOutputRoot(inc, cc.OutToGenrule)
	}
	hdrs, err := discoverHeaders(hostSrc, includesForWalk, cc.HeaderWalkCache, cc.MissingIncludeDirs, covered)
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
			// Relativize like every other header path — a FILE_SET HEADERS
			// entry can be an absolute path (cmake records generated headers,
			// e.g. VTK's per-module `vtk<Module>Module.h` export header, at
			// their CMAKE_CURRENT_BINARY_DIR-absolute path). Appending it raw
			// made emit render an invalid `//pkg:/abs/path` label ("target
			// names may not start with /"). A source-tree path relativizes to
			// (and reanchors under) the package; a build-dir path becomes
			// build-dir-relative — matching the recovered genrule's output
			// (the module header has a gen_*_Module_h genrule) — and is NOT
			// reanchored, like other generated outputs. An out-of-tree
			// absolute drops (not expressible as a label).
			p := src.Path
			if filepath.IsAbs(p) {
				if rel, inside := relativeIfInside(cmakeSrc, p); inside {
					fileSetHdrs = append(fileSetHdrs, reanchor(rel))
				} else if rel, inside := relativeIfInside(cmakeBuild, p); inside {
					fileSetHdrs = append(fileSetHdrs, rel)
				}
				continue
			}
			if pathHasDotDotSegment(p) {
				continue
			}
			fileSetHdrs = append(fileSetHdrs, reanchor(p))
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
	// Libraries refuse too when EVERYTHING (srcs AND hdrs) elided: an
	// empty cc_library links nothing, so every consumer fails at final
	// link with no signal pointing back here.
	if elidedKindsRefuseWhenEmpty(irt) && srcElided {
		msg := fmt.Sprintf("target %q (%s) has no srcs after lowering — every compiled source was elided (build-dir-only generated file with no recovered generator edge, missing-on-disk source, or compiler artifact); emitting it would produce a Bazel-invalid or empty rule", t.Name, t.Type)
		if rejections != nil {
			rejections.AddWithContext(failure.AllSourcesElided, msg, t.Name, "")
			return nil, nil
		}
		return nil, failure.New(failure.AllSourcesElided, "%s", msg)
	}

	return irt, nil
}

// lowerTargetDeps is lowerTarget's t.Dependencies loop: in-codebase
// non-UTILITY targets resolve to ":<name>" (PRIVATE trace scope routing
// to implementation_deps on kinds that accept it), UTILITY deps skip,
// manifest hits resolve to their Bazel labels, and anything else is the
// typed unresolved-link-dep refusal. Bodies and rationale comments are
// verbatim from the original inline region.
func lowerTargetDeps(irt *ir.Target, t *fileapi.Target, tt targetTrace, lc targetLowerCtx) error {
	imports := lc.imports
	idToName, utilityIDs := lc.idToName, lc.utilityIDs
	rejections := lc.rejections
	traceLinkScope := tt.traceLinkScope
	reuseOwners := pchReuseFromOwners(t, tt, lc.cmakeBuild)
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
		// ImplementationDeps) folds in after. exportDeps carries a
		// manifest export's declared closure (Export.Deps) when the
		// label resolved through the manifest.
		var label string
		var exportDeps []string
		//nolint:nestif // inside the grandfathered lowerTarget giant; folds into its ROADMAP burndown pass.
		if name, ok := idToName[dep.Id]; ok {
			label = ":" + name
		} else if utilityIDs[dep.Id] {
			continue
		} else {
			cmakeName := stripIDHash(dep.Id)
			if export := imports.LookupCMakeTarget(cmakeName); export != nil {
				label = export.BazelLabel
				exportDeps = export.Deps
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
				return failure.New(failure.UnresolvedLinkDep,
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
		// outermost-user-frame walk surfaces them.
		if isBuildOrderOnlyEdge(dep, t.BacktraceGraph) {
			irt.Data = append(irt.Data, label)
			continue
		}
		// REUSE_FROM owner edges DROP entirely: under the mirror lift
		// the consumer's real input is the owner's mirror FILE (staged
		// via srcs — that edge already orders the build), the owner
		// TARGET contributes no headers or link inputs, deps would be
		// illegal for an executable-kind owner (no linkable CcInfo),
		// and even data poisons: a cc_test owner is implicitly
		// testonly, which a non-test consumer can't reference. A
		// consumer that genuinely LINKS its owner still wires the dep
		// through the link-fragment attribution channel.
		if reuseOwners[stripIDHash(dep.Id)] {
			continue
		}
		private := allowsImplementationDeps && depScopeIsPrivate(traceLinkScope, dep, idToName)
		// The export's manifest-declared Deps ride with its label, same
		// scope, deduped through the same seenDep set (see
		// manifest.Export.Deps for why Bazel transitivity can't recover
		// them for prebuilt-backed labels).
		for _, l := range append([]string{label}, exportDeps...) {
			if l != label && seenDep[l] {
				continue
			}
			seenDep[l] = true
			if private {
				irt.ImplementationDeps = append(irt.ImplementationDeps, l)
			} else {
				irt.Deps = append(irt.Deps, l)
			}
		}
	}

	return nil
}

// lowerTarget lowers one codemodel target to its IR form: provenance,
// the per-source walk, compile groups/includes, generated-output
// consumer attribution, header staging, dependency and link-fragment
// lowering, then the CTest/multi-language finalization. Each sub-pass's
// rationale lives on its helper. (Formerly THE tracked complexity giant
// at cognitive 754; fully decomposed across burndown slices #558, #562,
// #564, and this one.)
func lowerTarget(t *fileapi.Target, tt targetTrace, lc targetLowerCtx) (*ir.Target, error) {
	// Unpack the bundled inputs into the locals the body uses: lc is
	// invariant across the per-target loop, tt is this target's trace.
	cmakeSrc, cmakeBuild := lc.cmakeSrc, lc.cmakeBuild
	// umbrellaPrefix is the cmakeSrc-relative-to-labelRoot segment when the
	// workspace-root umbrella promoted labelRoot above cmakeSrc (LLVM:
	// labelRoot=llvm-project/, cmakeSrc=llvm-project/llvm/ → "llvm"). srcs
	// are already re-anchored to labelRoot in the source loop; hdrs and
	// source-tree includes must get the same prefix so a BUILD at labelRoot
	// resolves them consistently. Empty in the common (non-promoted) case —
	// where hostSrc == cmakeSrc — so behavior there is unchanged.
	_, reanchor := lc.umbrellaReanchor()
	// pchCtx drives the target_precompile_headers forced-include lift
	// (pch.go) for this target's compile groups — both the main path and
	// the per-language splitCompileGroups subs.
	pchCtx := pchLiftCtx{
		resolve:    lc.pchResolve,
		cmakeSrc:   cmakeSrc,
		cmakeBuild: cmakeBuild,
		reanchor:   reanchor,
		pkgPath:    lc.bazelPackagePath,
		cc:         lc.cc,
	}
	rejections := lc.rejections

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
	// on. CallSite additionally records the user-level macro/
	// function invocation when the declaring command ran inside
	// one (comment recovery prefers it).
	irt.Provenance, irt.CallSite = targetProvenance(t, cmakeSrc, cmakeBuild)

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
		// Faithful-SHARED (opt-in): mark the impl so emit renders a sibling
		// cc_shared_library producing the real .so. Use the FULL cmake artifact
		// name (NameOnDisk, e.g. libbrotlidec.so.1.2.0) — both because that's
		// what cmake actually produces (versioned soname) and because it must
		// NOT collide with the impl cc_library's auto-generated lib<target>.so
		// dynamic output (a cc_shared_library and a cc_library can't both emit
		// the same path — the brotli collision). When the unversioned NameOnDisk
		// WOULD equal lib<target>.so, suffix it so the two outputs stay distinct.
		if lc.emitSharedLibraries {
			so := t.NameOnDisk
			if so == "" {
				so = "lib" + t.Name + ".so"
			}
			if so == "lib"+t.Name+".so" {
				// The unversioned name collides with the impl cc_library's auto
				// lib<target>.so output. Append a soversion so the two outputs
				// stay distinct AND the name stays a valid .so.<N> filetype
				// (cc_shared_library rejects e.g. ".so.shared"). Prefer the
				// cmake-recorded soversion tag; fall back to ".1".
				so += "." + soversionFromTags(irt.Tags, "1")
			}
			irt.SharedLibName = so
		}
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

	srcEmitPath, srcElided, err := lowerTargetSources(irt, t, tt, lc)
	if err != nil {
		return nil, err
	}

	targetBuildIncs, walkPkgRootForHdrs, privateIncDirs := lowerCompileGroups(irt, t, tt, lc, pchCtx)

	// Bake configure-written build-dir headers this target's includes
	// consume but no recovered producer covers (the untraced-writer
	// family) — before the attribution loops, so recovered outputs keep
	// their richer attribution and only true orphans bake. See
	// build_dir_source_bake.go.
	bakeConsumedBuildDirHeaders(irt, lc, targetBuildIncs)

	attributeGeneratedConsumers(irt, t, lc, targetBuildIncs)

	if staged, err := stageTargetHeaders(irt, t, lc, targetBuildIncs, walkPkgRootForHdrs, privateIncDirs, srcElided); err != nil || staged == nil {
		return staged, err
	}

	if err := lowerTargetDeps(irt, t, tt, lc); err != nil {
		return nil, err
	}

	lowerLinkAttribution(irt, t, tt, lc)

	if t.Install != nil && len(t.Install.Destinations) > 0 {
		irt.Visibility = publicVisibility()
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

	return finalizeLoweredTarget(irt, t, tt, lc, srcEmitPath, pchCtx)
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
	// Multi-language: existing case. PCH-creator groups are cmake
	// machinery, not project compiles — they must not force a split
	// (nor count as a language).
	langs := map[string]bool{}
	for _, cg := range t.CompileGroups {
		if cg.Language == "" || isPCHCreatorCompileGroup(t, &cg) {
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
		if cg.Language == "" || isPCHCreatorCompileGroup(t, &cg) {
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
func splitCompileGroups(t *fileapi.Target, irt *ir.Target, cc *codegenContext, cmakeSrc, cmakeBuild string, srcEmitPath map[int]string, pchCtx pchLiftCtx) error {
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

	// Snapshot the wrapper's deps BEFORE the loop (which appends the
	// sub-targets themselves to subDeps). These are the real compile/link
	// deps the dep-wiring passes resolved for the target — codemodel
	// Dependencies, find_package imports (absl, libprotobuf, …), the
	// trace-recovered static-lib deps. The sub-libraries are what actually
	// COMPILE the sources, so they need these deps to find the deps'
	// headers (protobuf's protoc-gen-upb_cxx compiles wire_format_lite.cc,
	// which #includes absl/base/casts.h from @abseil-cpp//absl/base:base).
	// The wrapper keeps them too (line below appends subDeps), so its own
	// consumers still get the PUBLIC deps transitively.
	sharedDeps := append([]string(nil), irt.Deps...)
	sharedImplDeps := append([]string(nil), irt.ImplementationDeps...)

	// PRIVATE include dirs (target_include_directories(... PRIVATE ...))
	// ride the wrapper's irt.Copts as `-I<dir>` / `-isystem<dir>` (added by
	// the include loop in lowerTarget); the per-CG CompileCommandFragments
	// the subs draw from don't carry them. Since the subs are what compile
	// the sources, extract those include copts and replay them onto every
	// sub so a source in a PRIVATE include tree resolves its siblings
	// (protobuf's protoc-gen-upb_c compiles
	// upb_generator/cmake/.../plugin.upb_minitable.c, which #includes the
	// sibling plugin.upb_minitable.h via the PRIVATE -Iupb_generator/cmake).
	// Harmless on a sub that doesn't compile anything under the dir — an
	// unused -I is a no-op.
	var sharedIncludeCopts []string
	for _, c := range irt.Copts {
		if strings.HasPrefix(c, "-I") || strings.HasPrefix(c, "-isystem") {
			sharedIncludeCopts = append(sharedIncludeCopts, c)
		}
	}

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
		for i := range t.Sources {
			if !srcIndex[i] {
				continue
			}
			// Use the exact path the source walk stored in irt.Srcs
			// for this codemodel source index. The walk relativizes
			// generated sources (absolute build-dir path →
			// CMakeFiles/foo.S) and re-anchors umbrella paths, so the
			// raw codemodel path can't be matched by string equality;
			// srcEmitPath records the post-relativization form.
			rel, ok := srcEmitPath[i]
			if !ok {
				continue
			}
			// Only keep srcs that landed on the wrapper's Srcs
			// slice — the catch-all non-cc drop after the walk can
			// trim an entry srcEmitPath still records.
			if !stringSliceContains(wrapperSrcs, rel) {
				continue
			}
			subSrcs = append(subSrcs, rel)
		}
		if len(subSrcs) == 0 {
			continue
		}
		copts, defs, pchArtifacts := splitCompileFragments(cg.CompileCommandFragments)
		// Replay the wrapper's PRIVATE include-dir copts (see
		// sharedIncludeCopts above) so this sub's sources resolve headers
		// reached via target_include_directories(... PRIVATE ...).
		copts = append(copts, sharedIncludeCopts...)
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
		// PCH forced-include lift — the per-language sibling of the main
		// compile-group path in lowerTarget (see pch.go): this sub's
		// language picks up ITS cmake_pch.h/.hxx expansion, the staged
		// source-tree headers land on the sub where its compile runs, and
		// the user-visible wrapper gets the tag.
		copts, subSrcs = pchCtx.apply(pchArtifacts, cg, copts, subSrcs, subHdrs, irt)
		// Source-tree -idirafter: same exec-root re-anchor as the main
		// path; the walk dirs are dropped here — the wrapper's pass
		// already collected them, and the staged headers partition into
		// the subs via sharedHdrs.
		copts, _ = reanchorIDirAfterCopts(copts, pchCtx.cmakeSrc, pchCtx.reanchor, pchCtx.pkgPath)

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

		// A split sub is always an object LIBRARY absorbed into the
		// deps-only wrapper — never the wrapper's own kind. A cc_binary /
		// cc_test wrapper whose subs were themselves cc_binary/cc_test
		// can't link: the wrapper (srcs cleared) has no `main`, and a
		// cc_binary in another cc_binary's deps doesn't contribute its
		// objects, so the link fails "undefined symbol: main" (protobuf's
		// mixed C/C++ protoc-gen-upb). Emit cc_library subs (CUDA → rules_
		// cuda's cuda_library, which also provides CcInfo); the wrapper
		// deps on them as a normal cc→cc(/cuda) link.
		//
		// For an executable wrapper force the subs alwayslink so EVERY
		// translation unit folds into the binary — cmake compiles all of
		// the executable's sources in, but a plain cc_library archive only
		// contributes the objects the linker references, which would drop
		// `main` and registration-only TUs.
		subKind := ir.KindCCLibrary
		subAlwayslink := irt.Alwayslink
		switch {
		case cg.Language == "CUDA":
			subKind = ir.KindCudaLibrary
		case irt.Kind == ir.KindCCBinary || irt.Kind == ir.KindCCTest:
			subAlwayslink = true
		}
		// Propagate the wrapper's deps to the sub so its sources compile
		// (and link) against the deps' headers/symbols. A sub that accepts
		// implementation_deps (cc_library) keeps the wrapper's PRIVATE /
		// PUBLIC split; one that doesn't (cc_binary sub of a mixed-language
		// cc_binary wrapper) folds both into deps. The sub is
		// //visibility:private and only the wrapper consumes it, so an
		// over-broad sub `deps` never leaks to an external consumer — the
		// wrapper carries the faithful split for ITS consumers.
		subDepsForSub := append([]string(nil), sharedDeps...)
		var subImplDeps []string
		if kindAllowsImplementationDeps(subKind) {
			subImplDeps = append([]string(nil), sharedImplDeps...)
		} else {
			subDepsForSub = append(subDepsForSub, sharedImplDeps...)
		}
		sub := ir.Target{
			Name:               subName,
			Kind:               subKind,
			Srcs:               subSrcs,
			Hdrs:               subHdrs,
			Includes:           sharedIncludes,
			Copts:              copts,
			Defines:            defs,
			Tags:               subTags,
			Deps:               subDepsForSub,
			ImplementationDeps: subImplDeps,
			// Split sub-libraries are INTERNAL object-libraries (alwayslink)
			// that exist only to be statically absorbed into the deps-only
			// wrapper — never linked standalone. Force linkstatic so Bazel
			// doesn't build a standalone .so for each: a C/C++ sub's .so can't
			// resolve the hidden-visibility symbols of a sibling asm/fortran sub
			// across a .so boundary (LLVM's BLAKE3 _c.so → the _asm subs'
			// llvm_blake3_hash_many_*). Static absorption keeps all the
			// objects in one linkage unit so those symbols resolve.
			Linkstatic: true,
			Alwayslink: subAlwayslink,
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

	// Cross-language linkage: see wireCrossLanguageSubDeps.
	wireCrossLanguageSubDeps(cc, subStart, langByName)

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

// wireCrossLanguageSubDeps makes each C/C++ split sub (those added from
// subStart on in cc.Subs) dep on the same target's asm/fortran subs. A C/C++
// sub typically CALLS into the asm/fortran subs (BLAKE3's blake3_dispatch.c →
// the per-arch llvm_blake3_hash_many_* asm functions, compiled
// hidden-visibility); the split makes them siblings under the wrapper, so when
// a C/C++ sub is linked as a standalone .so its hidden cross-language symbols
// would be undefined. The asm/fortran subs are alwayslink, so depending on them
// folds their objects into the C/C++ sub's linkage and the symbols resolve. One
// direction only (no cycle); asm/fortran calling back into C is rare and not wired.
func wireCrossLanguageSubDeps(cc *codegenContext, subStart int, langByName map[string]string) {
	var otherLangSubs []string
	for i := subStart; i < len(cc.Subs); i++ {
		switch langByName[cc.Subs[i].Name] {
		case "ASM", "ASM_NASM", "Fortran":
			otherLangSubs = append(otherLangSubs, ":"+cc.Subs[i].Name)
		}
	}
	if len(otherLangSubs) == 0 {
		return
	}
	for i := subStart; i < len(cc.Subs); i++ {
		switch langByName[cc.Subs[i].Name] {
		case "C", "CXX", "OBJC", "OBJCXX":
			cc.Subs[i].Deps = appendUnique(cc.Subs[i].Deps, otherLangSubs...)
		}
	}
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
		// Probe-recovered target properties, split by effect: build-flag props
		// (linkopts/copts/features) and surface-as-tag props. The CXX/C
		// extension rewrites stay inline below (they mutate the already-prepended
		// -std copt rather than appending).
		applyProbeBuildProps(tgt, p)
		applyProbeTagProps(tgt, p)
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

// applyProbeBuildProps maps the probe-recovered target properties that carry a
// real Bazel build-flag effect (linkopts / copts / features) onto tgt:
// BUILD_RPATH, POSITION_INDEPENDENT_CODE, the C/CXX visibility presets,
// VISIBILITY_INLINES_HIDDEN, and ENABLE_EXPORTS.
func applyProbeBuildProps(tgt *ir.Target, p cmakerun.GenexProbe) {
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
}

// applyProbeTagProps maps the probe-recovered target properties that have no
// native Bazel build-flag equivalent onto tgt as audit tags (so the bazel-idiom
// pass and operators see the cmake-side intent): SOVERSION/VERSION, the Qt
// AUTO* toggles, EXCLUDE_FROM_ALL (also → tags=["manual"]), MSVC_RUNTIME_LIBRARY,
// and the JOB_POOL_* routing.
func applyProbeTagProps(tgt *ir.Target, p cmakerun.GenexProbe) {
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

// retagFortranTargets lowers each cmake Fortran target to a buildable
// fortran_library (//rules:fortran.bzl) instead of an unbuildable cc_* rule.
//
// cmake routes a target's Fortran (.f / .f90 / ...) sources into the same
// add_library/add_executable as its C/C++ sources, but Bazel's cc rules
// dispatch by file extension and have NO Fortran compile action. fortran_library
// compiles them via the cc toolchain's OWN driver (gfortran) and exposes CcInfo,
// so a consuming cc_* target links them transparently. Two shapes:
//
//   - a Fortran-only target is retagged IN PLACE to KindFortranLibrary, keeping
//     its name (so existing deps edges still resolve) and usage requirements
//     (deps / copts / includes), with cc-only attributes normalized onto the
//     fortran_library surface (defines / local_defines → -D copts,
//     implementation_deps → deps; see normalizeFortranTarget);
//
//   - a MIXED target (C/C++/ASM + Fortran) keeps its cc_* rule for the
//     non-Fortran srcs and gains a private sibling `<name>_fortran`
//     fortran_library carrying the Fortran srcs; the cc_* target deps on it, so
//     its own objects and its consumers link the Fortran symbols (cmake compiled
//     them into one archive — the dep edge reunites them).
//
// The Fortran analogue of retagCudaTargets, and like it a post-emit pass after
// the per-language compile-group split, so it sees WHOLE-target language
// composition. Targets are tagged cmake-codegen-fortran-target so the gap's
// provenance stays grep-able. A Fortran-only cc_binary / cc_test degrades to a
// (non-runnable) fortran_library — the executable wrapper is lost, but that's
// strictly better than the prior behavior (which dropped Fortran-only targets
// entirely) and OpenBLAS's reference LAPACK is libraries.
func retagFortranTargets(pkg *ir.Package, srcRoot string) {
	if pkg == nil {
		return
	}
	var added []ir.Target
	for i := range pkg.Targets {
		t := &pkg.Targets[i]
		isCC := t.Kind == ir.KindCCLibrary || t.Kind == ir.KindCCBinary ||
			t.Kind == ir.KindCCInterface || t.Kind == ir.KindCCTest
		if !isCC || len(t.Srcs) == 0 {
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
			continue
		}
		if len(rest) == 0 {
			// Fortran-only: retag in place, keep the name + dep edges.
			t.Kind = ir.KindFortranLibrary
			t.ModuleSrcs, t.Srcs = splitFortranModuleSrcs(ftn, srcRoot)
			// The module-DEFINING sources' bytes shaped the BUILD (their
			// `module`/`use` content decided the module_srcs partition + order);
			// publish them as declared source-byte reads so the narrowing lens
			// keeps them real. See ir.Package.SourceByteReads.
			pkg.SourceByteReads = append(pkg.SourceByteReads, t.ModuleSrcs...)
			normalizeFortranTarget(t)
			if !stringSliceContains(t.Tags, "cmake-codegen-fortran-target") {
				t.Tags = append(t.Tags, "cmake-codegen-fortran-target")
			}
			continue
		}
		// Mixed: keep the cc_* target with the non-Fortran srcs; split the
		// Fortran srcs into a private sibling fortran_library and dep on it.
		modSrcs, plainSrcs := splitFortranModuleSrcs(ftn, srcRoot)
		// Publish the module-definers' content reads (see the Fortran-only
		// branch above and ir.Package.SourceByteReads).
		pkg.SourceByteReads = append(pkg.SourceByteReads, modSrcs...)
		// The sibling does NOT copy t.LinkOpts: the cc_* wrapper retains them and
		// the wrapper deps on the sibling, so the wrapper already threads its
		// linkopts to consumers once. Copying them here too would duplicate them
		// on every consumer link line (after the sibling's -lgfortran -lm),
		// which can misbehave for position-sensitive flags (`-Wl,--whole-archive`
		// pairs). The sibling only needs the gfortran runtime, which the rule
		// adds unconditionally.
		sub := ir.Target{
			Name:               t.Name + "_fortran",
			Kind:               ir.KindFortranLibrary,
			ModuleSrcs:         modSrcs,
			Srcs:               plainSrcs,
			Deps:               append([]string(nil), t.Deps...),
			ImplementationDeps: append([]string(nil), t.ImplementationDeps...),
			Copts:              append([]string(nil), t.Copts...),
			Includes:           append([]string(nil), t.Includes...),
			Defines:            append([]string(nil), t.Defines...),
			LocalDefines:       append([]string(nil), t.LocalDefines...),
			Visibility:         []string{"//visibility:private"},
			Tags:               []string{"cmake-codegen-fortran-target"},
		}
		normalizeFortranTarget(&sub)
		added = append(added, sub)
		t.Srcs = rest
		t.Deps = appendUnique(t.Deps, ":"+sub.Name)
		if !stringSliceContains(t.Tags, "cmake-codegen-fortran-target") {
			t.Tags = append(t.Tags, "cmake-codegen-fortran-target")
		}
	}
	pkg.Targets = append(pkg.Targets, added...)
}

// fortranModuleDefRe matches a Fortran free/fixed-form MODULE definition
// (`module la_constants`), excluding `module procedure` (a submodule-interface
// construct, not a module definition) and `module function`/`module subroutine`.
var fortranModuleDefRe = regexp.MustCompile(`(?im)^\s*module\s+([a-z][a-z0-9_]*)\s*(?:!.*)?$`)

// fortranUseRe matches a `use <module>` statement in all its forms — the bare
// `use la_constants`, the F2003 `use :: la_constants`, and the
// module-nature `use, non_intrinsic :: la_constants` (the leading `use\b`
// boundary keeps it from matching an identifier like `used_var`). A
// `use, intrinsic :: iso_fortran_env` also matches but is harmless: the
// intrinsic module isn't a definer in this set, so the topo sort finds no
// provider edge for it.
var fortranUseRe = regexp.MustCompile(`(?im)^\s*use\b\s*(?:,\s*\w+\s*)?(?:::\s*)?([a-z][a-z0-9_]*)`)

// splitFortranModuleSrcs partitions Fortran sources into the ones that DEFINE a
// module (gfortran emits a `.mod` for each) and the rest. The module-definers
// are returned topologically ordered — a definer that `use`s another definer's
// module comes after it — so fortran_library can compile them in order into a
// shared module directory before compiling the parallel bulk. cmake computes
// this dependency at build time via its Fortran scanner (the Fortran.dd dyndep);
// we replicate the scan here (only a handful of files in a project like LAPACK
// define modules — la_constants.f90, la_xisnan.F90). srcRoot is the on-disk
// directory the element-relative source paths resolve against (cmakeSrc);
// unreadable files are treated as non-definers (safe: a missing definer just
// means no module to order, the same as today's module-free path).
func splitFortranModuleSrcs(srcs []string, srcRoot string) (moduleSrcs, rest []string) {
	type def struct {
		src      string
		provides []string
		uses     []string
	}
	var defs []def
	provider := map[string]int{} // module name -> index into defs
	for _, s := range srcs {
		body, err := os.ReadFile(filepath.Join(srcRoot, s))
		if err != nil {
			rest = append(rest, s)
			continue
		}
		var provides []string
		for _, m := range fortranModuleDefRe.FindAllSubmatch(body, -1) {
			name := strings.ToLower(string(m[1]))
			// `module procedure` is excluded by the regex (it requires the
			// module name to be the whole token); guard the reserved word
			// anyway for fixed-form oddities.
			if name == "procedure" || name == "function" || name == "subroutine" {
				continue
			}
			provides = append(provides, name)
		}
		if len(provides) == 0 {
			rest = append(rest, s)
			continue
		}
		var uses []string
		for _, m := range fortranUseRe.FindAllSubmatch(body, -1) {
			uses = append(uses, strings.ToLower(string(m[1])))
		}
		for _, p := range provides {
			provider[p] = len(defs)
		}
		defs = append(defs, def{src: s, provides: provides, uses: uses})
	}
	if len(defs) == 0 {
		return nil, rest
	}
	// Topological sort over the definers: an edge def→dep when def `use`s a
	// module another definer provides. Kahn's algorithm; ties broken by the
	// input order for determinism. A cycle (pathological) falls back to input
	// order for the remaining nodes.
	indeg := make([]int, len(defs))
	adj := make([][]int, len(defs)) // provider index -> dependent definer indices
	for di, d := range defs {
		seen := map[int]bool{}
		for _, u := range d.uses {
			pi, ok := provider[u]
			if !ok || pi == di || seen[pi] {
				continue
			}
			seen[pi] = true
			adj[pi] = append(adj[pi], di)
			indeg[di]++
		}
	}
	var queue []int
	for i := range defs {
		if indeg[i] == 0 {
			queue = append(queue, i)
		}
	}
	emitted := make([]bool, len(defs))
	for len(queue) > 0 {
		n := queue[0]
		queue = queue[1:]
		moduleSrcs = append(moduleSrcs, defs[n].src)
		emitted[n] = true
		for _, m := range adj[n] {
			indeg[m]--
			if indeg[m] == 0 {
				queue = append(queue, m)
			}
		}
	}
	// Any node left (cycle) — append in input order.
	for i := range defs {
		if !emitted[i] {
			moduleSrcs = append(moduleSrcs, defs[i].src)
		}
	}
	return moduleSrcs, rest
}

// normalizeFortranTarget folds a (former) cc target's attributes onto the
// narrow fortran_library surface: defines / local_defines become -D copts (the
// GNU driver passes -D through to the Fortran frontend; ignored on fixed-form
// .f, honored on preprocessed .F), implementation_deps merge into deps
// (fortran_library exposes only deps), and the cc-only header / link-shape
// fields are cleared so emit's adaptFortranView never has to.
func normalizeFortranTarget(t *ir.Target) {
	for _, d := range t.Defines {
		t.Copts = append(t.Copts, "-D"+d)
	}
	for _, d := range t.LocalDefines {
		t.Copts = append(t.Copts, "-D"+d)
	}
	t.Defines = nil
	t.LocalDefines = nil
	t.Deps = appendUnique(t.Deps, t.ImplementationDeps...)
	t.ImplementationDeps = nil
	t.Hdrs = nil
	t.TextualHdrs = nil
	t.Alwayslink = false
	t.Linkstatic = false
	t.Features = nil
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
// same gap retagFortranTargets handles for Fortran, now also with a real
// buildable rule rather than a parked filegroup, but unlike Fortran,
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
func retagCudaTargets(pkg *ir.Package, rdcTargets map[string]bool) {
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
		case ir.KindCudaLibrary, ir.KindCudaBinary, ir.KindCudaTest:
			// Already a CUDA rule — the per-language split's sub-library
			// (named <cmake-target>_cuda). Separable compilation
			// (CUDA_SEPARABLE_COMPILATION → rules_cuda `rdc = True`)
			// lands HERE for a mixed-language target: the cc wrapper
			// keeps the host link and must NOT carry the attr
			// (cc_binary has no `rdc`).
			if rdcTargets[t.Name] || rdcTargets[strings.TrimSuffix(t.Name, "_cuda")] {
				t.CudaRdc = true
			}
			continue
		default:
			continue
		}
		// Inspect compiled sources only (skip headers): the target is a CUDA
		// target iff it has at least one `.cu` and every compiled src is `.cu`.
		// PerPlatform srcs arms count too — a platform-guarded `.cu` executable
		// (cuda-samples' systemWideAtomics: `if(Linux) add_executable(x x.cu)`)
		// folds its ONLY compiled src into the `@platforms//os:linux` select
		// arm, leaving flat Srcs empty; skipping the arms left it a cc_binary
		// whose nvcc-flag copts then failed under gcc.
		sawCuda := false
		sawNonCudaCompiled := false
		scan := func(srcs []string) {
			for _, s := range srcs {
				ext := ""
				if dot := strings.LastIndex(s, "."); dot >= 0 {
					ext = strings.ToLower(s[dot:])
				}
				if cclang.IsHeaderExt(ext) || ext == ".cuh" {
					continue // a header in srcs (incl. CUDA `.cuh`) — not a compiled TU
				}
				if isCudaSrc(s) {
					sawCuda = true
				} else {
					sawNonCudaCompiled = true
				}
			}
		}
		scan(t.Srcs)
		for _, arm := range t.PerPlatform["srcs"] {
			scan(arm)
		}
		if sawCuda && !sawNonCudaCompiled {
			t.Kind = cudaKind
			partitionCudaLinkopts(t)
			// Whole-target separable compilation: the retagged rule is
			// the one place the attr can live.
			if rdcTargets[t.Name] {
				t.CudaRdc = true
			}
		}
	}
}

// partitionCudaLinkopts rewrites a freshly-retagged CUDA target's link flags
// for rules_cuda's split link model: plain `linkopts` there means the DEVICE
// link command, and the cuda_binary/cuda_test macros silently DROP a plain
// `linkopts` from the outer cc_binary (only `host_linkopts` survives their
// rename pass) — so cmake's host link line emitted as `linkopts` never
// reaches the final link (cudaOpenMP's -lgomp went missing exactly this way).
//
// cmake links CUDA targets through nvcc, so its link line MIXES host flags
// with nvcc driver leftovers. Host-shaped tokens (-l/-L/-Wl,/-pthread/
// -fopenmp/-shared/-rdynamic/-static*) move to HostLinkOpts; the CUDA
// runtime libs (-lcudart*/-lcudadevrt/-lculibos) are dropped — rules_cuda's
// toolchain owns runtime linking (same doctrine as the arch-flag drop), and
// re-adding the static runtime beside the toolchain's shared one double-links
// it. Everything else (nvcc link-driver flags like -lineinfo /
// --extended-lambda, echoes of the compile copts) drops rather than riding
// the device link. PerPlatform["linkopts"] arms (config/platform selects)
// partition the same way into PerPlatform["host_linkopts"].
func partitionCudaLinkopts(t *ir.Target) {
	part := func(opts []string) (host []string) {
		for _, o := range opts {
			switch {
			case o == "-lineinfo": // nvcc's debug-lineinfo flag, NOT -l ineinfo
			case strings.HasPrefix(o, "-lcudart"), strings.HasPrefix(o, "-lcudadevrt"),
				strings.HasPrefix(o, "-lculibos"):
			case strings.HasPrefix(o, "-l"), strings.HasPrefix(o, "-L"),
				strings.HasPrefix(o, "-Wl,"), o == "-pthread", o == "-fopenmp",
				o == "-shared", o == "-rdynamic", strings.HasPrefix(o, "-static"):
				host = append(host, o)
			}
		}
		return host
	}
	t.HostLinkOpts = append(t.HostLinkOpts, part(t.LinkOpts)...)
	t.LinkOpts = nil
	if arms := t.PerPlatform["linkopts"]; len(arms) > 0 {
		hostArms := map[string][]string{}
		for cond, opts := range arms {
			if host := part(opts); len(host) > 0 {
				hostArms[cond] = host
			}
		}
		delete(t.PerPlatform, "linkopts")
		if len(hostArms) > 0 {
			t.PerPlatform["host_linkopts"] = hostArms
		}
	}
}

// cudaRdcTargetNames scans the ninja graph for cmake's device-link edges and
// returns the cmake target names built with CUDA_SEPARABLE_COMPILATION. The
// File API exposes NO signal for the property (no compile fragment, no link
// fragment — verified against cmake 4.x's codemodel for the cuda-samples cdp*
// shapes); the only artifact is the `CUDA_…_DEVICE_LINKER__` build edge whose
// output is `CMakeFiles/<target>.dir[/<Config>]/cmake_device_link.o`. Nil
// graph → empty set (offline-replay paths).
func cudaRdcTargetNames(g *ninja.Graph) map[string]bool {
	if g == nil {
		return nil
	}
	names := map[string]bool{}
	collect := func(out string) {
		if filepath.Base(out) != "cmake_device_link.o" {
			return
		}
		for _, seg := range strings.Split(filepath.ToSlash(out), "/") {
			if name, ok := strings.CutSuffix(seg, ".dir"); ok && name != "" {
				names[name] = true
				return
			}
		}
	}
	for _, b := range g.Builds {
		for _, o := range b.Outputs {
			collect(o)
		}
		for _, o := range b.ImplicitOuts {
			collect(o)
		}
	}
	return names
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
	return sliceutil.SortedUnique(in)
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

// publicVisibility returns the visibility list for a target that must be
// reachable cross-package (synthesized install / import / interface targets,
// install-derived targets). Centralizes the `//visibility:public` convention;
// returns a fresh slice so callers can't alias a shared one.
func publicVisibility() []string { return []string{"//visibility:public"} }

// pchReuseFromOwners names the OTHER targets whose cmake_pch artifacts
// this target's compile fragments force-include AND that nothing
// evidences a genuine link of — the drop-safe REUSE_FROM owner set.
// cmake records the REUSE_FROM owner as a plain TargetDependency with
// NO backtrace (probed: cmake 4.x writes no backtrace EVEN when the
// same dep is also genuinely linked — target_link_libraries + REUSE_FROM
// dedup into one entry), so the artifact in the consumer's own fragments
// is the only reuse signal, and link evidence is the discriminator:
// the trace's target_link_libraries ground truth (tt.traceLinkScope /
// traceLinkLibs — also covers STATIC consumers, which have no link
// step for fragments to witness) or the owner's artifact in the
// consumer's link.commandFragments. An owner with link evidence keeps
// its dep edge (dropping it would sever the only in-tree link channel:
// lowerLinkAttribution's contract is that in-codebase names arrive via
// t.Dependencies). An owner with NO link evidence drops at the call
// site: the consumer's real input is the owner's mirror FILE (staged
// via srcs), deps would be illegal for an executable-kind owner, and
// even data poisons (a cc_test owner is implicitly testonly).
func pchReuseFromOwners(t *fileapi.Target, tt targetTrace, cmakeBuild string) map[string]bool {
	var owners map[string]bool
	for _, cg := range t.CompileGroups {
		_, _, arts := splitCompileFragments(cg.CompileCommandFragments)
		for _, art := range arts {
			owner := pchArtifactOwner(art, cmakeBuild)
			if owner == "" || owner == t.Name || pchOwnerLinkEvidence(t, tt, owner) {
				continue
			}
			if owners == nil {
				owners = map[string]bool{}
			}
			owners[owner] = true
		}
	}
	return owners
}

// pchOwnerLinkEvidence reports whether anything beyond the PCH reuse
// evidences a genuine link of owner by this target: a recorded
// target_link_libraries call naming it (the trace ground truth), or the
// owner's conventional artifact name (lib<owner>.* / <owner>.*) in the
// link command fragments.
func pchOwnerLinkEvidence(t *fileapi.Target, tt targetTrace, owner string) bool {
	if _, ok := tt.traceLinkScope[owner]; ok {
		return true
	}
	for _, lib := range tt.traceLinkLibs {
		if lib == owner {
			return true
		}
	}
	if t.Link == nil {
		return false
	}
	for _, frag := range t.Link.CommandFragments {
		for _, tok := range strings.Fields(frag.Fragment) {
			base := filepath.Base(tok)
			if strings.HasPrefix(base, "lib"+owner+".") || strings.HasPrefix(base, owner+".") {
				return true
			}
		}
	}
	return false
}

// isBuildOrderOnlyEdge reports whether a TargetDependency came
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
func isBuildOrderOnlyEdge(dep fileapi.TargetDependency, g fileapi.BacktraceGraph) bool {
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

// stringSliceContains is a tiny linear-search helper. The
// source slices are short enough (typically <50 entries) that
// a map+rebuild is overkill.
func stringSliceContains(s []string, v string) bool {
	return slices.Contains(s, v)
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
// stripLeadingCd removes a leading `cd <abs-dir> && ` prefix from cmd when the
// directory is inside buildDir or cmakeSrc, returning the trimmed cmd and the
// build/source-relative subdir that was stripped (empty when nothing matched).
// The subdir lets a later pass (qualifyRedirectBasenames) re-qualify
// bare-basename redirect targets the cd would otherwise have rooted.
func stripLeadingCd(cmd, buildDir, cmakeSrc string) (string, string) {
	if !strings.HasPrefix(cmd, "cd ") {
		return cmd, ""
	}
	end := strings.Index(cmd, " && ")
	if end <= 0 {
		return cmd, ""
	}
	target := strings.TrimSpace(strings.TrimPrefix(cmd[:end], "cd "))
	if !filepath.IsAbs(target) {
		return cmd, ""
	}
	if buildDir != "" {
		if rel, ok := relativeIfInside(buildDir, target); ok {
			return cmd[end+4:], rel
		}
	}
	if cmakeSrc != "" {
		if rel, ok := relativeIfInside(cmakeSrc, target); ok {
			return cmd[end+4:], rel
		}
	}
	return cmd, ""
}

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
	cmd, strippedCdSubdir := stripLeadingCd(cmd, buildDir, cmakeSrc)
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
	// nvcc GPU-arch flags in a recovered custom command (the driver-API
	// fatbin shape: `nvcc -gencode=arch=compute_75,... -fatbin kernel.cu`)
	// get the same treatment the rule-side copts/linkopts already do
	// (isNvccArchFlag): the list is CMAKE_CUDA_ARCHITECTURES baked at
	// configure time and routinely names arches the provisioned nvcc
	// REJECTS outright (cuda-samples hard-sets Blackwell compute_100/110/120;
	// CUDA 12.0 fails "Unsupported gpu architecture") — arch selection is a
	// toolchain concern, not the graph's. Gated on the command actually
	// invoking nvcc so non-CUDA genrules are untouched.
	if strings.Contains(cmd, "nvcc") {
		cmd = dropNvccArchFlagsFromCmd(cmd)
	}
	return cmd
}

// dropNvccArchFlagsFromCmd removes nvcc GPU-arch selection tokens from a
// genrule cmd: the one-token `-gencode=arch=…` / `--generate-code=…` forms
// (isNvccArchFlag) and the two-token `-gencode arch=…` spelling. Other
// tokens — including quoted multi-space args — pass through byte-identically
// (split/join on single spaces preserves the original spacing).
func dropNvccArchFlagsFromCmd(cmd string) string {
	toks := strings.Split(cmd, " ")
	out := toks[:0]
	skipNext := false
	for _, t := range toks {
		switch {
		case skipNext:
			skipNext = false
		case t == "-gencode" || t == "--generate-code":
			skipNext = true
		case isNvccArchFlag(t):
		default:
			out = append(out, t)
		}
	}
	return strings.Join(out, " ")
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

// isNvccArchFlag reports whether s is an nvcc GPU-architecture selection flag
// (`--generate-code=arch=...` / `-gencode...`), the form cmake bakes a
// CMAKE_CUDA_ARCHITECTURES list into on both the compile and link lines.
// rules_cuda forbids these in a rule's copts/linkopts ("not allowed to be
// specified directly via copts of rules_cuda related rules") — arch is a
// TOOLCHAIN concern set via the `@rules_cuda//cuda:archs` build flag. They never
// appear on a non-CUDA compile/link line, so dropping them is safe across kinds.
// Strips a balanced surrounding quote pair first: cmake shell-quotes the
// fragment for the `[`/`,` it contains, and the link-flag path carries that
// quoted form verbatim.
func isNvccArchFlag(s string) bool {
	s = stripBalancedQuotes(s)
	return strings.HasPrefix(s, "--generate-code") || strings.HasPrefix(s, "-gencode")
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
//
// pchArtifacts returns the cmake_pch.h[xx] paths seen as `-include`
// arguments (order-preserving, deduped). Their pairs stay IN copts at
// their original positions; the caller routes the artifacts through
// pchLiftCtx.apply (pch.go), which rewrites each artifact argument to
// the synthesized mirror header's exec-root path — preserving cmake's
// forced-include order and warning-suppression semantics.
func splitCompileFragments(frags []fileapi.CommandFragment) (copts, defines, pchArtifacts []string) {
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
	// heldInclude defers a `-include` / `-include-pch` flag until we see its
	// argument, then emits the PAIR atomically and in place — positional
	// fidelity matters because forced includes are processed left-to-right
	// (one forced header may depend on another's macros). Pairs dedup at
	// PAIR granularity (flag+arg), never through the generic coptsSeen:
	// per-flag dedup would swallow the second `-include` of two distinct
	// forced headers and leave its argument as an orphan bare token. A
	// cmake_pch artifact pair (cmake's target_precompile_headers adds
	// `-include <build>/CMakeFiles/<t>.dir/<cfg>/cmake_pch.h`, an absolute
	// build-dir path that doesn't exist under Bazel's hermetic compile)
	// stays in place too and is ALSO surfaced via pchArtifacts — the caller
	// (pchLiftCtx.apply) rewrites the artifact argument to the mirror
	// header's exec-root path, preserving cmake's "PCH header is included
	// first in every TU" semantics at the original argv position.
	heldInclude := ""
	pchSeen := map[string]bool{}
	pairSeen := map[string]bool{}
	emitHeld := func() {
		if heldInclude != "" && !coptsSeen[heldInclude] {
			coptsSeen[heldInclude] = true
			copts = append(copts, heldInclude)
		}
		heldInclude = ""
	}
	// heldArch swallows the VALUE token of a space-separated nvcc arch flag
	// (`-gencode arch=...` / `--generate-code arch=...`). See the drop case below.
	heldArch := false
	for _, f := range frags {
		if f.Role != "" {
			// Reserved for link fragments; ignore on the compile side.
			continue
		}
		// Shell-tokenize the fragment (honoring quotes + \-escapes) rather than a
		// bare whitespace split: the File API fragment is meant to be
		// shell-tokenized, and a naive split mishandles BOTH a surrounding shell
		// quote cmake added for shell-special chars (CUDA's
		// `"--generate-code=arch=compute_80,code=[sm_80]"`) and an embedded
		// ESCAPED quote in a string-valued define (OpenBLAS's CMAKE_C_FLAGS carries
		// `-DVERSION="\"0.3.28\""`, the macro value being the C string "0.3.28").
		// Bazel passes copts/defines as argv with NO shell, so the faithful token
		// is the shell-PROCESSED one — `-DVERSION="0.3.28"`, not the raw
		// backslash-laden bytes that make gcc choke on a "missing terminating \"".
		// splitShellTokens collapses the surrounding-quote and escaped-quote cases
		// alike, so the old whole-token stripBalancedQuotes step is subsumed.
		for _, p := range splitShellTokens(f.Fragment) {
			// Resolve a pending `-include` / `-include-pch` against this
			// token (its argument): emit the pair atomically, in place,
			// deduped at pair granularity. A cmake PCH artifact argument
			// (cmake_pch.h / cmake_pch.hxx) is additionally surfaced via
			// pchArtifacts so the forced-include mirror lift rewrites it.
			if heldInclude != "" {
				pair := heldInclude + "\x00" + p
				if !pairSeen[pair] {
					pairSeen[pair] = true
					copts = append(copts, heldInclude, p)
					if isCMakePCHPath(p) && !pchSeen[p] {
						pchSeen[p] = true
						pchArtifacts = append(pchArtifacts, p)
					}
				}
				heldInclude = ""
				continue
			}
			if p == "-include" || p == "-include-pch" {
				heldInclude = p
				continue
			}
			// nvcc GPU-arch flags: rules_cuda forbids per-target arch flags in
			// copts — arch selection is a TOOLCHAIN concern, set via the
			// `@rules_cuda//cuda:archs` build flag, not the rule's copts
			// (`--generate-code=... is not allowed to be specified directly via
			// copts of rules_cuda related rules`). cmake bakes
			// CMAKE_CUDA_ARCHITECTURES into `--generate-code=arch=...` compile
			// flags, so drop them here; the build lens sets the arch via
			// rules_cuda's flag. These nvcc flags never appear on a non-CUDA
			// compile line, so the drop is safe for cc targets too. Handle both
			// the `=`-joined token form (cmake's shape) and a defensive
			// space-separated `-gencode <arg>` form (swallow the value).
			if heldArch {
				heldArch = false
				continue
			}
			if p == "-gencode" || p == "--generate-code" {
				heldArch = true
				continue
			}
			switch {
			case isNvccArchFlag(p):
				// dropped: see the nvcc GPU-arch comment above.
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
	// A `-include` with no following argument in the fragment stream (cmake
	// shouldn't emit this, but be defensive) is preserved rather than lost.
	emitHeld()
	return copts, defines, pchArtifacts
}

// wireDynamicDeps adds, to every consumer of a shared (SharedLibName-bearing)
// library, the sibling `<lib>_shared` cc_shared_library in DynamicDeps — so the
// consumer dynamically links the .so rather than static-linking the impl. Match
// is by bare target name against the package's shared libs; the shared lib
// itself is skipped (it doesn't dynamic-dep on itself).
func wireDynamicDeps(pkg *ir.Package) {
	shared := map[string]bool{}
	for _, t := range pkg.Targets {
		if t.SharedLibName != "" {
			shared[t.Name] = true
		}
	}
	if len(shared) == 0 {
		return
	}
	for i := range pkg.Targets {
		t := &pkg.Targets[i]
		if t.Kind != ir.KindCCLibrary && t.Kind != ir.KindCCBinary && t.Kind != ir.KindCCTest {
			continue
		}
		seen := map[string]bool{}
		var sharedDeps []string
		all := append(append([]string{}, t.Deps...), t.ImplementationDeps...)
		for _, d := range all {
			name := bareDepName(d)
			if name == t.Name { // a shared lib's wrapper deps on its own impl — skip
				continue
			}
			if shared[name] && !seen[name] {
				seen[name] = true
				sharedDeps = append(sharedDeps, ":"+name+"_shared")
			}
		}
		if t.SharedLibName != "" {
			// This target is a shared lib: its cc_shared_library WRAPPER must
			// dynamic-dep on sibling shared libs so it doesn't statically
			// re-link a cc_library another shared lib already owns.
			t.SharedLibDynamicDeps = sharedDeps
		} else if t.Kind == ir.KindCCBinary || t.Kind == ir.KindCCTest {
			// A plain LINKING consumer: link sibling shared libs dynamically.
			// Only cc_binary/cc_test accept `dynamic_deps` — a plain
			// cc_library consumer (libevent's event_static, a static
			// variant whose cmake link interface names a shared sibling)
			// has no slot for it; it keeps the ordinary deps edge and the
			// eventual linking rule decides the link mode. Emitting the
			// attr on cc_library is an analysis-time "no such attribute"
			// error that takes the whole package down.
			t.DynamicDeps = sharedDeps
		}
	}
}

// bareDepName extracts the bare target name from a Bazel dep label:
// ":foo"→"foo", "//a/b:foo"→"foo", "//a/b/foo"→"foo".
func bareDepName(label string) string {
	if i := strings.LastIndex(label, ":"); i >= 0 {
		return label[i+1:]
	}
	if i := strings.LastIndex(label, "/"); i >= 0 {
		return label[i+1:]
	}
	return label
}

// soversionFromTags returns the SOVERSION recorded in a target's
// `cmake-codegen-soversion=<N>` tag, or def when absent. Used to disambiguate a
// shared lib's output name from the impl cc_library's auto lib<name>.so.
func soversionFromTags(tags []string, def string) string {
	const pfx = "cmake-codegen-soversion="
	for _, t := range tags {
		if strings.HasPrefix(t, pfx) {
			if v := strings.TrimPrefix(t, pfx); v != "" {
				return v
			}
		}
	}
	return def
}

// makeCIdentifier mirrors cmake's string(MAKE_C_IDENTIFIER): every character
// that isn't [A-Za-z0-9_] becomes `_`, and a leading digit is prefixed with
// `_`. cmake derives the default DEFINE_SYMBOL as `<MAKE_C_IDENTIFIER(target)>_EXPORTS`.
func makeCIdentifier(s string) string {
	if s == "" {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		isAlnum := (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '_'
		if isAlnum {
			b.WriteByte(c)
		} else {
			b.WriteByte('_')
		}
	}
	out := b.String()
	if out[0] >= '0' && out[0] <= '9' {
		out = "_" + out
	}
	return out
}

// moveDefineToLocal moves a define (matched on its NAME, before any `=value`)
// from irt.Defines (transitive) to irt.LocalDefines (non-propagating), deduped.
// No-op when the define isn't present.
func moveDefineToLocal(irt *ir.Target, macro string) {
	if macro == "" {
		return
	}
	kept := irt.Defines[:0]
	moved := false
	for _, d := range irt.Defines {
		name := d
		if i := strings.IndexByte(d, '='); i >= 0 {
			name = d[:i]
		}
		if name == macro {
			moved = true
			if !stringSliceContains(irt.LocalDefines, d) {
				irt.LocalDefines = append(irt.LocalDefines, d)
			}
			continue
		}
		kept = append(kept, d)
	}
	if moved {
		irt.Defines = kept
	}
}

// applyExportMacro routes a shared/module library's export macro to
// local_defines. cmake defines the export macro (`-D<DEFINE_SYMBOL>`, default
// `<target>_EXPORTS`) ONLY when compiling the library's OWN sources — it's the
// __declspec(dllexport) / visibility trigger and is PRIVATE (never propagated
// to consumers). Two sources for the macro, both handled:
//
//   - a CUSTOM DEFINE_SYMBOL (zlib's ZLIB_DLL) IS surfaced in the codemodel
//     defines — the SHARED->cc_library collapse would otherwise emit it as a
//     transitive `defines`, leaking it to every consumer; moveDefineToLocal
//     relocates it to local_defines.
//   - the DEFAULT `<target>_EXPORTS` is generator-implicit and usually ABSENT
//     from the codemodel defines (libevent's event_shared_EXPORTS etc. — caught
//     missing by the compile-commands fidelity lens), so nothing is moved; ADD
//     it so the converted compile matches what cmake actually compiles with.
//
// No-op for non-shared/module targets (the macro gate is cmake's own).
func applyExportMacro(irt *ir.Target, targetType, targetName, defineSymbol string) {
	if targetType != "SHARED_LIBRARY" && targetType != "MODULE_LIBRARY" {
		return
	}
	macro := defineSymbol
	if macro == "" {
		macro = makeCIdentifier(targetName) + "_EXPORTS"
	}
	moveDefineToLocal(irt, macro)
	if !localDefineHasName(irt, macro) {
		irt.LocalDefines = append(irt.LocalDefines, macro)
	}
}

// localDefineHasName reports whether irt.LocalDefines already carries a define
// with the given NAME (matched before any `=value`), so callers don't append a
// duplicate of a macro that moveDefineToLocal may have just relocated.
func localDefineHasName(irt *ir.Target, macro string) bool {
	for _, d := range irt.LocalDefines {
		name := d
		if i := strings.IndexByte(d, '='); i >= 0 {
			name = d[:i]
		}
		if name == macro {
			return true
		}
	}
	return false
}

// isCMakePCHPath reports whether p is a cmake target_precompile_headers
// artifact — the generated PCH header cmake names cmake_pch.h / cmake_pch.hxx
// under CMakeFiles/<target>.dir/<config>/. These are absolute build-dir paths
// that don't exist under Bazel's hermetic compile; splitCompileFragments
// surfaces them so the PCH forced-include lift (pch.go) can expand the
// declared header list in their place. Match on basename so it's robust to
// the host build-dir prefix and the per-config segment.
func isCMakePCHPath(p string) bool {
	base := filepath.Base(p)
	return base == "cmake_pch.h" || base == "cmake_pch.hxx" ||
		base == "cmake_pch.h.gch" || base == "cmake_pch.hxx.pch"
}

// isCMakePCHMachinerySource reports whether a target source is part of
// cmake's PCH machinery: the generated creator TU
// (CMakeFiles/<t>.dir/cmake_pch.hxx.cxx / cmake_pch.h.c — the compile
// that produces the .gch) or the generated cmake_pch header itself,
// both listed in the declaring target's Sources. They're cmake-internal
// bookkeeping, not project sources — the mirror lift (pch.go) preserves
// the forced-include semantics, and the .gch precompilation has no
// Bazel-side equivalent to feed. Anchored on the CMakeFiles/ layout the
// machinery always lives under, not the basename alone: a PROJECT
// source legitimately named cmake_pch.* must not be silently skipped.
func isCMakePCHMachinerySource(p string) bool {
	return strings.HasPrefix(filepath.Base(p), "cmake_pch.") &&
		strings.Contains(filepath.ToSlash(p), "CMakeFiles/")
}

// isPCHCreatorCompileGroup reports whether a compile group exists only
// to BUILD the PCH artifact: every source it compiles is cmake PCH
// machinery. cmake's codemodel lists this group ALONGSIDE the real TU
// groups on a PCH-declaring target, with creator-only fragments
// (`-x c++-header`) that must never leak onto project TUs — selecting
// it as "the" compile group would compile every source as a header.
func isPCHCreatorCompileGroup(t *fileapi.Target, cg *fileapi.CompileGroup) bool {
	if len(cg.SourceIndexes) == 0 {
		return false
	}
	for _, si := range cg.SourceIndexes {
		if si < 0 || si >= len(t.Sources) || !isCMakePCHMachinerySource(t.Sources[si].Path) {
			return false
		}
	}
	return true
}

// firstRealCompileGroup returns the first compile group that compiles
// project sources (skipping PCH-creator groups), preserving the M1
// "primary compile group" contract for PCH-declaring targets whose
// FIRST group is the creator (executables that don't split groups).
func firstRealCompileGroup(t *fileapi.Target) (fileapi.CompileGroup, bool) {
	for _, cg := range t.CompileGroups {
		if !isPCHCreatorCompileGroup(t, &cg) {
			return cg, true
		}
	}
	return fileapi.CompileGroup{}, false
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
// resolveCodegenHeaderConsumers resolves each cc_library consumer's UTILITY
// (tablegen) dependencies to the generated `.inc` headers it #includes, now that
// the standalone genrules producing them have been recovered. The combined
// output set — per-target codegen outputs (cc.OutToGenrule) plus the standalone
// recovery (stand) — is what the ninja walk filters on; matches land on
// pkg.CodegenHeaderConsumers for the split transform to synthesize the wrapper
// library and wire the consumer's dep.
func resolveCodegenHeaderConsumers(pkg *ir.Package, g *ninja.Graph, stand []ir.Target, cc *codegenContext, codegenConsumerDeps map[string][]fileapi.TargetDependency, utilityIDs map[string]bool, utilityIDToName map[string]string, isTargetName map[string]bool) {
	genOut := make(map[string]string, len(cc.OutToGenrule))
	for o, n := range cc.OutToGenrule {
		genOut[o] = n
	}
	for _, gt := range stand {
		for _, o := range gt.GenruleOuts {
			genOut[o] = gt.Name
		}
	}
	// A recognized native rule (cc_proto_library) produces its headers INSIDE
	// the rule, so they're neither an OutToGenrule entry nor a standalone
	// genrule out — seed them here so the consumer walk still detects a
	// `#include "foo.pb.h"`. The match wires a DIRECT deps edge to the native
	// rule (via NativeRuleConsumerLabels below + split), not the file wrapper.
	for o, name := range cc.OutToNativeConsumerDep {
		genOut[o] = name
		if pkg.NativeRuleConsumerLabels == nil {
			pkg.NativeRuleConsumerLabels = map[string]string{}
		}
		pkg.NativeRuleConsumerLabels[o] = name
	}
	// Index ninja outputs by final path component so the codegen walk can seed
	// from a sub-directory custom target's prefixed phony (cmake names it
	// `<dir>/<target>`).
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

// subPackageDirFromFile is the file-path sibling of subPackageDir: it computes
// the workspace-root-relative sub-package directory for a target declared in
// declFile (a CMakeLists.txt path). Used for trace-synth interface libs, whose
// declaring scope is recovered from the trace frame stack rather than a
// codemodel directory index. Returns ("", false) when declFile is empty or
// outside cmakeSrc (the caller then leaves the lib in the root package).
func subPackageDirFromFile(declFile, cmakeSrc, workspaceRoot string) (string, bool) {
	if declFile == "" || cmakeSrc == "" {
		return "", false
	}
	rel, inside := relativeIfInside(cmakeSrc, filepath.Dir(declFile))
	if !inside {
		return "", false
	}
	rel = filepath.ToSlash(rel)
	if rel == "." {
		rel = ""
	}
	rel = strings.TrimSuffix(rel, "/")
	// Re-anchor to workspaceRoot when it sits strictly above cmakeSrc, matching
	// subPackageDir / lowerTarget's labelRoot pick.
	if workspaceRoot != "" && workspaceRoot != cmakeSrc {
		if prefix, ok := relativeIfInside(workspaceRoot, cmakeSrc); ok && prefix != "" {
			if rel == "" {
				return prefix, true
			}
			return prefix + "/" + rel, true
		}
	}
	return rel, true
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
// looksLikeCxxHeader sniffs an extensionless file to decide whether it's a C/C++
// header (eigen's `Dense`/`Core`/… module headers) vs. a stray non-source file
// (LICENSE, README, a data file) that also sits in an include dir. Conservative:
// requires an early preprocessor directive or an obvious C++ header construct, so
// plain-text files aren't mis-collected. Reads only the first 4 KiB.
func looksLikeCxxHeader(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	buf := make([]byte, 4096)
	n, _ := f.Read(buf)
	if n == 0 {
		return false
	}
	for _, line := range strings.Split(string(buf[:n]), "\n") {
		s := strings.TrimSpace(line)
		if s == "" {
			continue
		}
		switch {
		case strings.HasPrefix(s, "#include"), strings.HasPrefix(s, "#ifndef"),
			strings.HasPrefix(s, "#define"), strings.HasPrefix(s, "#pragma"),
			strings.HasPrefix(s, "#if "), strings.HasPrefix(s, "namespace "),
			strings.HasPrefix(s, "template"):
			return true
		}
		// Allow leading comment/blank lines (license headers) before the first
		// directive; bail once real non-comment, non-directive text appears.
		if !strings.HasPrefix(s, "//") && !strings.HasPrefix(s, "/*") &&
			!strings.HasPrefix(s, "*") {
			return false
		}
	}
	return false
}

func discoverHeaders(sourceRoot string, includeDirs []string, cache map[string][]string, missing map[string]bool, covered func(inc string) bool) ([]string, error) {
	seen := map[string]struct{}{}
	// recordMissing flags an absent include dir for the end-of-lower
	// "missing on disk" warning + unsupported-source-path rejection —
	// UNLESS a recovered producer covers it. A build-dir include
	// (target_include_directories'd ${CMAKE_BINARY_DIR}/...) or a
	// generated-output root is emitted on irt.Includes for the -I that
	// resolves its GENERATED headers at Bazel time, and those headers
	// are staged by the build-dir bake + consumer attribution — not by
	// this hostSrc walk. Such a dir is absent UNDER hostSrc by
	// construction (its content lives in the build tree), so recording
	// it would reject an include that is in fact wired AND recovered.
	// `covered` is the only place the build-dir-vs-source distinction
	// still exists; once `inc` joins hostSrc below it's erased. A
	// genuine forward-declared empty SOURCE include (LLVM's llvm-mca
	// shape) has no producer, so it still surfaces — the intended
	// survey signal.
	recordMissing := func(absDir, inc string) {
		if cache != nil {
			cache[absDir] = nil
		}
		if missing != nil && (covered == nil || !covered(inc)) {
			missing[absDir] = true
		}
	}
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
		// oddity even though it isn't fatal) — but not when a
		// recovered producer covers it (see recordMissing).
		if st, statErr := os.Stat(absDir); statErr != nil {
			if errors.Is(statErr, fs.ErrNotExist) {
				recordMissing(absDir, inc)
				continue
			}
			return nil, fmt.Errorf("stat include dir %q: %w", absDir, statErr)
		} else if !st.IsDir() {
			recordMissing(absDir, inc)
			continue
		}
		perDir, err := collectDirHeaders(sourceRoot, absDir)
		if err != nil {
			return nil, err
		}
		for _, h := range perDir {
			seen[h] = struct{}{}
		}
		if cache != nil {
			cache[absDir] = perDir
		}
	}
	out := sliceutil.SortedKeys(seen)
	return out, nil
}

// collectDirHeaders walks absDir and returns every header file under it as a
// sourceRoot-relative, forward-slashed path. Post-stat walk errors (permission
// denied mid-walk, I/O failures) surface — discoverHeaders handles the
// dir-absent case before calling. Extensionless C++ headers (eigen ships
// `Dense`, `Core`, … with no extension; VTK's bundled vtkeigen the same) are
// real headers consumers `#include <vtkeigen/eigen/Dense>` — collected when a
// content sniff confirms a header so they're not silently dropped; any other
// non-header file is skipped.
func collectDirHeaders(sourceRoot, absDir string) ([]string, error) {
	var perDir []string
	walkErr := filepath.WalkDir(absDir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		ext := strings.ToLower(filepath.Ext(p))
		// `.cuh` joins the walk here (not in cclang's universal header set —
		// it's CUDA-contextual): a kernel header beside its `.cu`
		// (cuda-samples' `<sample>_kernel.cuh` quote-includes) must stage
		// like any sibling header or the include misses in Bazel's sandbox.
		if !cclang.IsHeaderExt(ext) && ext != ".cuh" {
			if ext != "" || !looksLikeCxxHeader(p) {
				return nil
			}
		}
		rel, err := filepath.Rel(sourceRoot, p)
		if err != nil {
			return err
		}
		perDir = append(perDir, filepath.ToSlash(rel))
		return nil
	})
	if walkErr != nil {
		return nil, fmt.Errorf("walk include dir %q: %w", absDir, walkErr)
	}
	return perDir, nil
}
