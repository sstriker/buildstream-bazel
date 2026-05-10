// Package cli holds flag parsing, validation, and CLI exit codes for
// convert-element. Kept separate from main so it's testable without a process
// boundary.
package cli

import (
	"flag"
	"fmt"
	"io"
	"os"
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
	SourceRoot string

	// ReplyDir, when non-empty, skips invocation of cmake and reads File API
	// JSON directly from this directory. Used by tests and for offline
	// dry-runs against pre-recorded fixtures.
	ReplyDir string

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

	// OutToolchainSignalDir, when non-empty, causes convert-element
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

	// OutIRJSON, when non-empty, causes convert-element to write
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
	fs := flag.NewFlagSet("convert-element", flag.ContinueOnError)
	fs.SetOutput(stderr)
	a := Args{}
	fs.StringVar(&a.SourceRoot, "source-root", "", "absolute path to the CMake project root")
	fs.StringVar(&a.ReplyDir, "reply-dir", "", "skip cmake invocation; read File API reply from this dir (testing)")
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
	fs.BoolVar(&a.UnsupportedExecuteProcessFallback, "unsupported-execute-process-fallback", false, "on classifier refusal of execute_process calls (stamp / probe / unknown buckets), emit a placeholder BUILD.bazel listing every non-UTILITY codemodel target as an empty cc_library / cc_binary / cc_library-interface stub with public visibility — instead of exiting Tier-1 with unsupported-execute-process. Step 2 (this PR) only restores label resolution at analysis time; the per-target install_tree.tar wiring (cc_import / sh_binary referencing artifact paths derived from Target.Install.Destinations) lands in Step 2.5, after which downstream consumers' compile/link actions resolve as well. Off by default to preserve the strict-fail behaviour. See docs/design/cmake-execute-process-round2-fallback.md.")
	fs.BoolVar(&a.AllowCMakeVersionMismatch, "allow-cmake-version-mismatch", false, "let convert-element run with cmake older than the codemodel-v2 floor (local-dev escape hatch)")
	fs.StringVar(&a.PrefixDir, "prefix-dir", "", "directory added to CMAKE_PREFIX_PATH (out-of-tree synth-prefix; orchestrator-driven)")
	fs.StringVar(&a.ToolchainCMakeFile, "toolchain-cmake-file", "", "CMake toolchain file (typically derive-toolchain's toolchain.cmake); skips per-conversion compiler probing")
	fs.StringVar(&a.SourceKey, "source-key", "", "when set, prefix every source path in emitted cc_library/cc_binary srcs with @src_<key>//: (the FUSE-sources Bazel-label path)")
	fs.BoolVar(&a.Verify, "verify", false, "after lowering, cross-check the IR against compile_commands.json; surface -D/-I drops and adds as stderr warnings (does not fail the run)")
	fs.StringVar(&a.VerifyReport, "verify-report", "", "write the structured verify Report (JSON) here; implies --verify")

	if err := fs.Parse(argv); err != nil {
		return a, ExitUsage
	}
	if a.VerifyReport != "" {
		a.Verify = true
	}
	if a.SourceRoot == "" && a.ReplyDir == "" {
		fmt.Fprintln(stderr, "convert-element: must set --source-root or --reply-dir")
		fs.Usage()
		return a, ExitUsage
	}
	return a, ExitSuccess
}

// LookEnv is a tiny indirection so tests can inject env without touching
// process state.
type LookEnv func(string) (string, bool)

// OSLookEnv is the production env reader.
var OSLookEnv LookEnv = func(k string) (string, bool) { return os.LookupEnv(k) }
