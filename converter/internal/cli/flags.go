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

	// OutBuild is the destination path for the generated BUILD.bazel.
	OutBuild string

	// OutBundleDir, when non-empty, is the directory where the synthesized
	// cmake-config bundle is written (one .cmake file per kind).
	OutBundleDir string

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
	// round-2 install_tree.tar via per-target cc_import /
	// sh_binary rules reconstructed from
	// Target.Install.Destinations + NameOnDisk; until then,
	// downstream consumers' compile/link actions against the
	// stubs will fail (analysis is the only contract this PR
	// delivers). See
	// docs/design/cmake-execute-process-round2-fallback.md.
	UnsupportedExecuteProcessFallback bool

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

	// EmitProvenance enables Phase 1 task 1's backtrace-derived
	// per-rule annotation: emit a leading `# Source: <file>:<line>
	// (<command>)` comment above each rule whose cmake declaration
	// is recorded in the codemodel's BacktraceGraph. Default ON —
	// the comment is high-signal navigation help; pass
	// --emit-provenance=false for byte-clean output (e.g. golden
	// regression tests pre-dating the provenance comments).
	EmitProvenance bool

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
	fs.BoolVar(&a.StrictTrace, "strict-trace", false, "refuse with a Tier-1 error when no cmake trace data is available (instead of warning and continuing with degraded recovery). Recommended for production runs; off by default to preserve existing behaviour")
	fs.StringVar(&a.OutBuild, "out-build", "BUILD.bazel", "destination path for generated BUILD.bazel")
	fs.StringVar(&a.OutBundleDir, "out-bundle-dir", "", "directory for synthesized cmake-config bundle (optional)")
	fs.StringVar(&a.OutFailure, "out-failure", "", "write Tier-1 failure JSON here on per-codebase errors (optional)")
	fs.StringVar(&a.ImportsManifest, "imports-manifest", "", "path to JSON imports manifest mapping out-of-tree CMake targets to Bazel labels (optional)")
	fs.StringVar(&a.OutReadPaths, "out-read-paths", "", "write JSON array of source-tree paths cmake read at configure time (requires --source-root, optional)")
	fs.StringVar(&a.OutTimings, "out-timings", "", "write JSON with per-phase wall-clock timings (cmake configure, translation, total)")
	fs.StringVar(&a.OutCMakeConfigureReads, "out-cmake-configure-reads", "", "write JSON array of source-relative paths from build.ninja's RERUN_CMAKE implicit-input list (configure-time oracle)")
	fs.StringVar(&a.OutToolchainSignalDir, "out-toolchain-signal-dir", "", "directory; on success, copy the cmake File API reply contents here so the unifier can fold per-element toolchain signal into the platform's ResolvedToolchain.Base")
	fs.StringVar(&a.OutIRJSON, "out-ir-json", "", "write the post-lower ir.Package as JSON to this path. Drives the orchestrator's per-element multi-platform fold; ignored by single-platform flows.")
	fs.BoolVar(&a.LiftConfigureFile, "lift-configure-file", false, "emit configure_file recovery in the lifted shape (.h.in as a real srcs + //tools:cmake-configure-file invocation at Bazel build time). Requires the caller to stage //tools:cmake-configure-file. Off by default to preserve compatibility with downstream Bazel envelopes that don't yet stage the tool.")
	fs.BoolVar(&a.DumpVars, "dump-vars", true, "stage the dump-vars.cmake hook to capture cmake's variable namespace into <build>/cmake-to-bazel.vars.dump. Read by configure_file lift (@VAR@ / ${VAR} substitution) and find_package variable-form attribution (<Pkg>_LIBRARIES correlation on cmakes below the 3.32 find_package-v1 floor). On by default; requires cmake 3.24+ (silently inactive on older cmakes — the hook's CMAKE_PROJECT_TOP_LEVEL_INCLUDES injection floor).")
	fs.BoolVar(&a.UnsupportedExecuteProcessFallback, "unsupported-execute-process-fallback", false, "on classifier refusal of execute_process calls, emit empty cc_library/cc_binary stubs so downstream consumers' label resolution still works (round-2 mode). Off by default; see docs/design/cmake-execute-process-round2-fallback.md.")
	fs.BoolVar(&a.AllowCMakeVersionMismatch, "allow-cmake-version-mismatch", false, "let convert-element-cmake run with cmake older than the codemodel-v2 floor (local-dev escape hatch)")
	fs.BoolVar(&a.CMP0026Shim, "cmp0026-shim", false, "translate get_target_property(... LOCATION) into $<TARGET_FILE:...> at configure time (cmake 4.x escape hatch for removed CMP0026 OLD). Changes LOCATION's return shape project-wide; see #208.")
	fs.BoolVar(&a.ProbeGenex, "probe-genex", true, "stage the per-target genex-probe hook (Phase 3 of the generator-parity uplift). cmake emits file(GENERATE) for each artifact-producing target's common genex shapes (TARGET_FILE, TARGET_OBJECTS, INTERFACE_*) so the lift reads post-walk resolved bytes instead of reimplementing the cmake-side evaluator. Default ON; requires cmake 3.24+.")
	fs.StringVar(&a.BuildType, "build-type", "", "cmake -DCMAKE_BUILD_TYPE value (defaults to Release in cmakerun). Mutually exclusive with --build-types.")
	fs.Var(commaSlice{&a.BuildTypes}, "build-types", "comma-separated list of cmake configuration names; switches the generator to \"Ninja Multi-Config\" with -DCMAKE_CONFIGURATION_TYPES=<a;b;c>. Phase 5 of the generator-parity uplift (ROADMAP.md). Mutually exclusive with --build-type.")
	fs.BoolVar(&a.EmitProvenance, "emit-provenance", true, "above each emitted rule, write a leading `# Source: <file>:<line> (<command>)` comment derived from the cmake codemodel's BacktraceGraph. Default ON; pass --emit-provenance=false for byte-clean output.")
	fs.BoolVar(&a.EmitStandaloneCustomCommands, "emit-standalone-custom-commands", true, "Phase 4 of the generator-parity uplift: walk every CUSTOM_COMMAND edge in build.ninja and emit a genrule for each whose outputs aren't already covered by an existing recoverGenrule emission. On by default; covers add_custom_target / add_custom_command edges nothing consumes. Pass --emit-standalone-custom-commands=false to opt out.")
	fs.StringVar(&a.OutSanitizerFeatures, "out-sanitizer-features", "", "write cc_toolchain sanitizer feature definitions (.bzl) extracted from cmake's CMAKE_<LANG>_FLAGS_<CONFIG> cache for sanitizer-shaped configs in --build-types. Phase 5 of the generator-parity uplift.")
	fs.StringVar(&a.AuditBazelIdiomReport, "audit-bazel-idiom-report", "", "write the structured bazelidiom audit findings (JSON) to this path. The audit pass itself runs unconditionally on every convert and surfaces findings on stderr.")
	fs.StringVar(&a.PrefixDir, "prefix-dir", "", "directory added to CMAKE_PREFIX_PATH (out-of-tree synth-prefix; orchestrator-driven)")
	fs.StringVar(&a.ToolchainCMakeFile, "toolchain-cmake-file", "", "CMake toolchain file (typically derive-toolchain's toolchain.cmake); skips per-conversion compiler probing")
	fs.StringVar(&a.SourceKey, "source-key", "", "when set, prefix every source path in emitted cc_library/cc_binary srcs with @src_<key>//: (the FUSE-sources Bazel-label path)")
	fs.StringVar(&a.BazelPackagePath, "bazel-package-path", "", "repo-root-relative path of the destination Bazel package (e.g. \"elements/hello-world\"). Frames the emitted `# gazelle:cc_search` directives so gazelle_cc's resolver — which interprets cc_search arguments repo-root relative — picks up the same include search paths cmake recorded. Empty suppresses the directive; safer than emitting wrong bytes.")
	fs.BoolVar(&a.Verify, "verify", false, "after lowering, cross-check the IR against compile_commands.json; surface -D/-I drops and adds as stderr warnings (does not fail the run)")
	fs.StringVar(&a.VerifyReport, "verify-report", "", "write the structured verify Report (JSON) here; implies --verify")

	if err := fs.Parse(argv); err != nil {
		return a, ExitUsage
	}
	if a.VerifyReport != "" {
		a.Verify = true
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
	return a, ExitSuccess
}

// LookEnv is a tiny indirection so tests can inject env without touching
// process state.
type LookEnv func(string) (string, bool)

// OSLookEnv is the production env reader.
var OSLookEnv LookEnv = func(k string) (string, bool) { return os.LookupEnv(k) }

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
