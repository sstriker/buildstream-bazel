// convert-element-cmake converts one CMake source tree into a fully-declared
// BUILD.bazel plus a synthetic <Pkg>Config.cmake bundle. Each invocation
// handles exactly one codebase; the M3 orchestrator drives many such
// invocations across a project (one REAPI action per codebase) and also
// runnable standalone for development.
//
// M1 surface: --source-root for the in-development real-cmake path (NYI in
// step 4) and --reply-dir for offline runs against pre-recorded File API
// fixtures (used by step 3 / golden tests).
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/sstriker/buildstream-bazel/converter/emit/bazel"
	"github.com/sstriker/buildstream-bazel/converter/emit/configsettings"
	"github.com/sstriker/buildstream-bazel/converter/internal/bazelidiom"
	"github.com/sstriker/buildstream-bazel/converter/internal/cli"
	"github.com/sstriker/buildstream-bazel/converter/internal/cmakerun"
	"github.com/sstriker/buildstream-bazel/converter/internal/configfold"
	"github.com/sstriker/buildstream-bazel/converter/internal/coverage"
	"github.com/sstriker/buildstream-bazel/converter/internal/ctest"
	"github.com/sstriker/buildstream-bazel/converter/internal/emit/cmakecfg"
	"github.com/sstriker/buildstream-bazel/converter/internal/emit/sanitizerfeatures"
	"github.com/sstriker/buildstream-bazel/converter/internal/failure"
	"github.com/sstriker/buildstream-bazel/converter/internal/fileapi"
	"github.com/sstriker/buildstream-bazel/converter/internal/hardeningprobe"
	"github.com/sstriker/buildstream-bazel/converter/internal/lower"
	"github.com/sstriker/buildstream-bazel/converter/internal/ninja"
	"github.com/sstriker/buildstream-bazel/converter/internal/rejection"
	"github.com/sstriker/buildstream-bazel/converter/internal/todos"
	"github.com/sstriker/buildstream-bazel/converter/internal/toolchainscan"
	"github.com/sstriker/buildstream-bazel/converter/internal/verify"
	"github.com/sstriker/buildstream-bazel/converter/ir"
	"github.com/sstriker/buildstream-bazel/internal/convmode"
	"github.com/sstriker/buildstream-bazel/internal/manifest"
	"github.com/sstriker/buildstream-bazel/internal/shadow"
	"github.com/sstriker/buildstream-bazel/internal/synthprefix"
)

// timings is the on-disk schema for --out-timings. Captured per-phase
// wall-clock seconds let operators see configure-vs-translation ratios
// across a project. version=1 fences future readers.
type timings struct {
	Version            int     `json:"version"`
	CMakeConfigureSecs float64 `json:"cmake_configure_seconds"`
	TranslationSecs    float64 `json:"translation_seconds"`
	TotalSecs          float64 `json:"total_seconds"`
}

// fallbackInstallTarget derives the same-package pipeline_install
// target label the round-2 execute-process fallback's pick_file
// stubs project files out of. write-a names that target
// "<elem>_trace_build", and the bazel package path's basename is
// the element name (e.g. "elements/foo" -> ":foo_trace_build").
// Empty package path yields ":_trace_build" (the lower-side
// default), which a consumer build surfaces loudly if it diverges.
func fallbackInstallTarget(bazelPackagePath string) string {
	if bazelPackagePath == "" {
		return ""
	}
	return ":" + filepath.Base(bazelPackagePath) + "_trace_build"
}

func main() {
	args, code := cli.Parse(os.Args[1:], os.Stderr)
	if code != cli.ExitSuccess {
		os.Exit(code)
	}
	if err := run(args); err != nil {
		os.Exit(handleError(args, err))
	}
}

// runVerifyPass cross-checks the lowered IR against compile_commands.json (when
// --verify is set and a compile DB is available), printing -D/-I mismatches to
// stderr and writing the structured Report to --verify-report when set. A
// missing compile DB is a silent no-op — verify is best-effort diagnostics.
func runVerifyPass(a cli.Args, hostBuildDir string, pkg *ir.Package) error {
	ccPath := compileCommandsPath(hostBuildDir, a.ReplyDir)
	if ccPath == "" {
		return nil
	}
	rep, verr := verify.Verify(ccPath, pkg, a.SourceRoot)
	if verr != nil {
		return failure.New(failure.FileAPIMalformed, "verify: %v", verr)
	}
	if msg := verify.FormatMismatches(rep); msg != "" {
		fmt.Fprint(os.Stderr, msg)
	}
	if a.VerifyReport != "" {
		body, _ := json.MarshalIndent(rep, "", "  ")
		if err := os.MkdirAll(filepath.Dir(a.VerifyReport), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(a.VerifyReport, append(body, '\n'), 0o644); err != nil {
			return err
		}
	}
	return nil
}

// recoverPass1StampAbort attempts to rescue a pass-1 ToIR abort caused by a
// forwarded VCS stamp (git_describe() helper return, SDL): a warm
// non-expanded-trace reconfigure against the live build dir recovers the
// set()-copies + function-parameter-forwards, and re-lowering with those
// threaded in (via runToIR) lifts the call instead of aborting. Returns ok=false
// (nil pkg) when the reconfigure fails, the trace yields no set-copies/forwards,
// or the re-lower still errors — leaving the caller to surface the original
// error.
func recoverPass1StampAbort(ctx context.Context, a cli.Args, hostBuildDir string, runToIR func(*lower.LiteralProbeSink, map[string]cmakerun.LiteralResolution, []shadow.SetAssignment, []shadow.ParentScopeForward) (*ir.Package, error)) (pkg *ir.Package, sets []shadow.SetAssignment, forwards []shadow.ParentScopeForward, sink *lower.LiteralProbeSink, ok bool) {
	plainTrace := filepath.Join(hostBuildDir, "trace-plain.jsonl")
	if _, cfgErr := cmakerun.Configure(ctx, cmakerun.Options{
		SourceRoot:         a.SourceRoot,
		BuildDir:           hostBuildDir,
		PrefixDir:          a.PrefixDir,
		ToolchainCMakeFile: a.ToolchainCMakeFile,
		BuildType:          a.BuildType,
		BuildTypes:         a.BuildTypes,
		TracePath:          plainTrace,
		TraceNonExpanded:   true,
		Stdout:             os.Stderr,
		Stderr:             os.Stderr,
	}); cfgErr != nil {
		return nil, nil, nil, nil, false
	}
	raw, rerr := os.ReadFile(plainTrace)
	if rerr != nil {
		return nil, nil, nil, nil, false
	}
	sets = shadow.ExtractSetAssignments(raw, a.SourceRoot)
	forwards = shadow.ExtractParentScopeForwards(raw, a.SourceRoot)
	if len(sets) == 0 && len(forwards) == 0 {
		return nil, nil, nil, nil, false
	}
	sink = &lower.LiteralProbeSink{}
	pkg, err := runToIR(sink, nil, sets, forwards)
	if err != nil {
		return nil, nil, nil, nil, false
	}
	return pkg, sets, forwards, sink, true
}

// configureCmakeFresh runs the real-cmake path: assert the cmake version floor
// (cmake >= 3.20, the codemodel-v2 minimum; the orchestrator always runs a
// pinned cmake, so the --allow-cmake-version-mismatch escape hatch is local-dev
// only), spin a temp build dir, configure cmake against it under source-root
// mode, then load the reply. Trace is always captured (it feeds the lower's
// keyword-recovery + execute_process rescue passes, independent of
// --out-read-paths). The temp build dir is returned for the CALLER to clean up
// — returned even on Configure error so the deferred RemoveAll still fires.
func configureCmakeFresh(ctx context.Context, a cli.Args) (buildDir, replyDir string, cmakeVars map[string]string, ninjaPath string, elapsed time.Duration, err error) {
	if !a.AllowCMakeVersionMismatch {
		if _, _, _, verr := cmakerun.AssertVersion(ctx); verr != nil {
			return "", "", nil, "", 0, failure.New(failure.ConfigureFailed, "%v", verr)
		}
	}
	buildDir, err = os.MkdirTemp("", "convert-element-build-*")
	if err != nil {
		return "", "", nil, "", 0, err
	}
	opts := cmakerun.Options{
		SourceRoot:         a.SourceRoot,
		BuildDir:           buildDir,
		PrefixDir:          a.PrefixDir,
		ToolchainCMakeFile: a.ToolchainCMakeFile,
		BuildType:          a.BuildType,
		BuildTypes:         a.BuildTypes,
		ExtraCacheVars:     cmakeDefinesToMap(a.CmakeDefines),
		// DumpVars defaults true (see flags.go) so the variable-form
		// find_package attribution path fires on cmakes below the 3.32
		// find_package-v1 floor without the operator also opting into
		// configure_file lift or probe-genex. cmake 3.24+ is the
		// dump-vars-hook floor; on older cmakes the staged file is never
		// sourced and downstream paths fall back cleanly.
		DumpVars:    a.DumpVars,
		CMP0026Shim: a.CMP0026Shim,
		ProbeGenex:  a.ProbeGenex,
		Stdout:      os.Stderr, // route cmake noise to our stderr
		Stderr:      os.Stderr,
	}
	opts.TracePath = filepath.Join(buildDir, "trace.jsonl")
	configureStart := time.Now()
	reply, cerr := cmakerun.Configure(ctx, opts)
	elapsed = time.Since(configureStart)
	if cerr != nil {
		return buildDir, "", nil, "", elapsed, failure.New(failure.ConfigureFailed, "%v", cerr)
	}
	return buildDir, reply.Path, reply.Vars, filepath.Join(buildDir, "build.ninja"), elapsed, nil
}

// loadOfflineReplyArtifacts handles the offline --reply-dir path: opportunistically
// locate a checked-in build.ninja (recording scripts capture it alongside the
// reply — four parents up at <build>/build.ninja per the fileapi layout, or
// stashed directly in the reply dir by test fixtures) and pick up the cmake
// variable namespace from cmake-to-bazel.vars.dump. A missing vars dump leaves
// cmakeVars nil — silently disabling the (a) genex evaluator (an empty Context
// → every genex.UnsupportedError → fall back to (b)/legacy) — and warns so the
// degradation isn't invisible; a read error is fatal.
func loadOfflineReplyArtifacts(replyDir string) (ninjaPath string, cmakeVars map[string]string, err error) {
	candidate := filepath.Join(filepath.Dir(replyDir), "..", "..", "..", "build.ninja")
	candidate, _ = filepath.Abs(candidate)
	if _, serr := os.Stat(candidate); serr == nil {
		ninjaPath = candidate
	}
	if direct := filepath.Join(replyDir, "build.ninja"); ninjaPath == "" {
		if _, serr := os.Stat(direct); serr == nil {
			ninjaPath = direct
		}
	}
	if vars, verr := cmakerun.ReadVarsDumpFromReplyDir(replyDir); verr != nil {
		return "", nil, failure.New(failure.FileAPIMissing, "read vars dump: %v", verr)
	} else if len(vars) > 0 {
		cmakeVars = vars
	} else {
		fmt.Fprintln(os.Stderr, "convert-element-cmake: warning: no cmake-to-bazel.vars.dump found in the reply dir; the (a) genex evaluator will fall back to legacy mode (file(GENERATE) shapes that depend on cmake variables may be less precisely lifted).")
	}
	return ninjaPath, cmakeVars, nil
}

// (cognitive 247 / cyclomatic 152). The convert-element-cmake pipeline driver;
// breaking it down into focused sub-pass extractions is its own ROADMAP
// "complexity lens" burndown pass — grandfathered so the lens gates as blocking
// on all other code. Remove this directive as it comes back under threshold.
//
//nolint:gocognit,gocyclo,cyclop,maintidx,funlen // Tracked complexity giant
func run(a cli.Args) error {
	t0 := time.Now()
	var configureElapsed time.Duration

	// --split-packages is mutually exclusive with --out-ir-json: the
	// latter round-trips IR through JSON for the multi-platform fold,
	// and the per-directory split is a v1 single-platform-only emit
	// transform. Refuse loudly rather than silently splitting an IR the
	// fold path will re-merge from JSON that omits SubPackages.
	if a.SplitPackages && a.OutIRJSON != "" {
		return failure.New(failure.UnsupportedTargetType,
			"--split-packages is mutually exclusive with --out-ir-json (the multi-platform fold path); pick one")
	}

	if a.ProbeDistroHardening {
		// Probe the convert host's cc for distro-default
		// hardening flags. Diagnostic-only — we don't change
		// any BUILD.bazel emit decisions; the goal is to
		// surface the expected symbol-set delta between cmake
		// and Bazel rebuilds so operators don't chase ghost
		// regressions.
		r := hardeningprobe.Probe("")
		if r.Err != nil {
			fmt.Fprintf(os.Stderr, "convert-element-cmake: --probe-distro-hardening skipped: %v\n", r.Err)
		} else if !r.Empty() {
			fmt.Fprint(os.Stderr, r.FormatForOperator())
		}
	}

	replyDir := a.ReplyDir
	var ninjaPath string
	var hostBuildDir string
	var cmakeVars map[string]string
	ctx := context.Background()
	// When the operator passes --cmake-build-dir, the CLI Parse
	// step derives a.ReplyDir but leaves hostBuildDir empty —
	// which silently disables CTest classification, the direct
	// trace-path lookup (`<build>/trace.jsonl`), and the
	// compile_commands.json lookup downstream. All three care
	// about the build dir, not the reply dir. Populate
	// hostBuildDir from a.CMakeBuildDir so the --cmake-build-dir
	// path gets the same treatment as the real-cmake-run path.
	//
	// Pure offline --reply-dir runs (no --cmake-build-dir, no
	// --source-root) intentionally leave hostBuildDir empty —
	// fixture replay paths typically don't have a live build dir
	// the converter can poke at, and historical behavior is to
	// skip the build-dir-dependent passes silently.
	if a.CMakeBuildDir != "" {
		hostBuildDir = a.CMakeBuildDir
	}
	if replyDir == "" {
		bd, rd, vars, np, elapsed, cerr := configureCmakeFresh(ctx, a)
		// buildDir is returned (and cleaned up) even on Configure error,
		// matching the original defer-right-after-MkdirTemp placement.
		if bd != "" {
			defer os.RemoveAll(bd)
		}
		if cerr != nil {
			return cerr
		}
		hostBuildDir = bd
		replyDir = rd
		cmakeVars = vars
		ninjaPath = np
		configureElapsed = elapsed
	} else {
		np, vars, oerr := loadOfflineReplyArtifacts(replyDir)
		if oerr != nil {
			return oerr
		}
		ninjaPath = np
		cmakeVars = vars
	}

	r, err := fileapi.Load(replyDir)
	if err != nil {
		return failure.New(failure.FileAPIMissing, "load reply: %v", err)
	}

	// Phase 5 sanitizer-as-feature emit: when the operator
	// requested a .bzl sidecar AND --build-types includes one or
	// more sanitizer-shaped configs, extract cmake's per-config
	// CMAKE_<LANG>_FLAGS_<CONFIG> and render them as
	// cc_toolchain feature definitions the operator drops into
	// their toolchain. Pure read on r.Cache; no effect on
	// pkg.Targets emission.
	if a.OutSanitizerFeatures != "" && len(a.BuildTypes) > 0 {
		sets := configfold.ExtractSanitizerFlags(r.Cache, a.BuildTypes)
		body := sanitizerfeatures.Emit(sets)
		if err := os.MkdirAll(filepath.Dir(a.OutSanitizerFeatures), 0o755); err != nil {
			return fmt.Errorf("stage sanitizer-features dir: %w", err)
		}
		if err := os.WriteFile(a.OutSanitizerFeatures, body, 0o644); err != nil {
			return fmt.Errorf("write sanitizer-features: %w", err)
		}
	}

	// Phase 5 //config package emit: when the operator requested it
	// AND the codemodel is multi-config, render the config_settings
	// that back the fold's //config:<name> select() arms. Sanitizer-
	// shaped configs are excluded — they route through --features
	// (--out-sanitizer-features above), not a per-config select(),
	// matching lower's nonFeatureConfigNames split. The primary
	// non-sanitizer configuration is the string_flag default so an
	// unset flag reproduces lower's flattened baseline view.
	if a.OutConfigSettings != "" && len(r.Codemodel.Configurations) > 1 {
		var names []string
		for _, c := range r.Codemodel.Configurations {
			if _, isSanitizer := configfold.SanitizerFeature(c.Name); isSanitizer {
				continue
			}
			names = append(names, c.Name)
		}
		var primary string
		if len(names) > 0 {
			primary = names[0]
		}
		if body := configsettings.Emit(names, primary); body != nil {
			if err := os.MkdirAll(filepath.Dir(a.OutConfigSettings), 0o755); err != nil {
				return fmt.Errorf("stage config-settings dir: %w", err)
			}
			if err := os.WriteFile(a.OutConfigSettings, body, 0o644); err != nil {
				return fmt.Errorf("write config-settings: %w", err)
			}
		}
	}

	// Stage 6: per-element toolchain signal capture. The unifier
	// (Stage 5's cmd/unify-toolchains) optionally folds these
	// into the platform's ResolvedToolchain.Base, picking up any
	// builtin-include / sysroot fact a real element exposes that
	// the dedicated toolchain probe missed. Off unless the caller
	// (typically the orchestrator with --collect-toolchain-signal)
	// opts in.
	if a.OutToolchainSignalDir != "" {
		if err := copyDirContents(replyDir, a.OutToolchainSignalDir); err != nil {
			return fmt.Errorf("copy toolchain signal: %w", err)
		}
	}

	var g *ninja.Graph
	if ninjaPath != "" {
		g, err = ninja.ParseFile(ninjaPath)
		if err != nil {
			return failure.New(failure.NinjaParseFailed, "parse %s: %v", ninjaPath, err)
		}
	}

	var imports *manifest.Resolver
	switch {
	case len(a.ExportsIn) > 0:
		// Merge the (optional) render-time convention base with the
		// producer-emitted --exports-in docs, base first so the
		// producer's real export surface wins on any shared key.
		imports, err = manifest.LoadMerged(append([]string{a.ImportsManifest}, a.ExportsIn...)...)
	case a.ImportsManifest != "":
		imports, err = manifest.Load(a.ImportsManifest)
	}
	if err != nil {
		return err
	}

	prefixAbs := ""
	if a.PrefixDir != "" {
		prefixAbs, err = filepath.Abs(a.PrefixDir)
		if err != nil {
			return err
		}
	}

	// CTest classification: parse CTestTestfile.cmake out of the
	// build dir cmake just configured. The --reply-dir offline path
	// has no live build dir, so we skip — fixture-based runs stay
	// pre-CTest behavior (every EXECUTABLE → cc_binary).
	var testRegistry *ctest.Registry
	if hostBuildDir != "" {
		testRegistry, err = ctest.Parse(hostBuildDir)
		if err != nil {
			return failure.New(failure.CTestParseFailed, "%v", err)
		}
	}

	// Trace bytes drive lower's PUBLIC/PRIVATE-aware include
	// partition, IMPORTED-target dep recovery for static libs,
	// and configure_file genrule emission. Read from the
	// build dir (where cmake just wrote it) when running cmake
	// ourselves, or from the reply dir's sibling location for
	// the offline --reply-dir fixture path.
	var traceRaw []byte
	tracePath := ""
	if hostBuildDir != "" {
		tracePath = filepath.Join(hostBuildDir, "trace.jsonl")
	} else if a.ReplyDir != "" {
		// Offline mode: trace.jsonl typically sits next to the
		// reply dir (build/trace.jsonl). The reply dir is
		// build/.cmake/api/v1/reply, so walk four parents up.
		tracePath = filepath.Join(filepath.Dir(a.ReplyDir), "..", "..", "..", "trace.jsonl")
		if abs, absErr := filepath.Abs(tracePath); absErr == nil {
			tracePath = abs
		}
		if _, statErr := os.Stat(tracePath); statErr != nil {
			// Test fixtures sometimes stash the trace directly
			// inside the reply dir for convenience.
			alt := filepath.Join(a.ReplyDir, "trace.jsonl")
			if _, altErr := os.Stat(alt); altErr == nil {
				tracePath = alt
			}
		}
	}
	if tracePath != "" {
		if body, readErr := os.ReadFile(tracePath); readErr == nil {
			traceRaw = body
		}
	}
	// Loud degradation on missing trace. Without trace data,
	// several lower passes silently skip — the rendered
	// BUILD.bazel is still structurally valid but coverage
	// is degraded (no PRIVATE/PUBLIC include partition, no
	// configure_file lift, no IMPORTED-target dep recovery
	// for static libs, no platform-conditional source partition,
	// etc.). Surface the gap on stderr so operators see it at
	// the converter rather than three layers downstream when
	// something doesn't build; with --strict-trace, refuse
	// instead.
	if len(traceRaw) == 0 {
		const msg = "no cmake trace data available; recovery paths that depend on trace events will be skipped (PUBLIC/PRIVATE include partition, configure_file lift, IMPORTED-target dep recovery, platform-conditional source partition, etc.).\n" +
			"  To capture trace data, run cmake with:\n" +
			"    --trace-expand --trace-format=json-v1 --trace-redirect=<build>/trace.jsonl\n" +
			"  --source-root mode does this automatically; --reply-dir / --cmake-build-dir mode requires the trace file to exist on disk."
		if a.StrictTrace {
			return failure.New(failure.MissingTraceData, "%s", msg)
		}
		fmt.Fprintf(os.Stderr, "convert-element-cmake: warning: %s\n", msg)
	}

	// BuildDir is where lower's configure_file recovery reads
	// rendered output bytes. Live cmake build dir in production;
	// the fixture reply dir mirrors the build-dir layout (the
	// recording script stashes configure_file outputs at their
	// build-relative paths) for offline test runs.
	hostBuildOrReply := hostBuildDir
	if hostBuildOrReply == "" {
		hostBuildOrReply = a.ReplyDir
	}

	// Probe-genex hook output (Phase 3 of the generator-parity
	// uplift). ReadGenexProbe returns nil silently when the hook
	// didn't run — single-call site for both opt-in and opt-out
	// paths.
	genexProbes, err := cmakerun.ReadGenexProbe(hostBuildOrReply)
	if err != nil {
		return failure.New(failure.FileAPIMalformed, "read probe-genex output: %v", err)
	}

	// configureLog-v1 events (Phase 2 of the generator-parity
	// uplift). When the reply carries a sidecar pointer to
	// CMakeConfigureLog.yaml, load the YAML events for downstream
	// consumers. Both the sidecar absence (cmake < 3.26) and the
	// YAML file absence (configure fired no log-aware events) are
	// silent — LoadConfigureLogYAML returns nil + nil for both
	// shapes.
	var configureLogEvents []fileapi.Event
	if r.ConfigureLog != nil {
		configureLogEvents, err = fileapi.LoadConfigureLogYAML(r.ConfigureLog.Path)
		if err != nil {
			return failure.New(failure.FileAPIMalformed, "read configure log: %v", err)
		}
	}

	// Diagnostic mode: --ignore-rejections-for-diagnostics implies
	// the execute-process fallback (any execute_process refusal
	// routes through the existing placeholder emit path) and gives
	// every other refusal site somewhere to record before falling
	// through with a local skip.
	var rejections *rejection.Collector
	execFallback := a.UnsupportedExecuteProcessFallback
	if a.IgnoreRejectionsForDiagnostics {
		rejections = rejection.New()
		execFallback = true
	}
	// Lens-3 dependency-coverage collector. Always created so the
	// audit runs on every convert (findings go to stderr; the JSON
	// report is written only when --audit-coverage-report is set),
	// mirroring the bazelidiom audit's always-on behaviour.
	coverageCollector := coverage.New()
	// Conversion-todos collector. Always created so the producer sites
	// gather the no-mechanical-form constructs on every convert; the
	// JSON report is written only when --conversion-todos-report is set,
	// mirroring the coverage / bazelidiom always-on-collect shape.
	todosCollector := todos.New()
	// runToIR lowers the reply with a given literal-probe sink and
	// resolution map. Hoisted to a closure so the generalized-genex
	// two-pass can run it twice: pass 1 with a collecting sink (no
	// resolutions), pass 2 with the warm-reconfigure resolutions in
	// hand. Every other field is identical between passes.
	// stampSink gathers the recovered VCS-stamp variables from pass 1 so
	// the orchestration can decide whether a non-expanded-trace second
	// pass (to recover set()-copy stamp indirection) is worth running.
	stampSink := map[string]string{}
	// Operator's real toolchain vocabulary for the feature lift, if pointed
	// at one (--toolchain-features-from). Enumerated once from their Starlark.
	var backedFeatures []string
	if a.ToolchainFeaturesFrom != "" {
		backedFeatures, err = toolchainscan.ParseDeclared(a.ToolchainFeaturesFrom)
		if err != nil {
			return fmt.Errorf("--toolchain-features-from %s: %w", a.ToolchainFeaturesFrom, err)
		}
		// Non-nil (operator opted in) gates the lift even when empty — but an
		// empty scan usually means the parser couldn't read the toolchain's
		// features (wrapper/computed names), so only `pic` will lift. Surface
		// that rather than letting it look like the toolchain backs nothing.
		if len(backedFeatures) == 0 {
			fmt.Fprintf(os.Stderr, "convert-element-cmake: warning: --toolchain-features-from %s declared no literal feature() names; only the built-in `pic` will lift (the toolchain may build features via wrappers/computed names the parser can't read — see docs/operator-toolchain-features.md)\n", a.ToolchainFeaturesFrom)
		}
	}
	runToIR := func(sink *lower.LiteralProbeSink, resolutions map[string]cmakerun.LiteralResolution, setAssignments []shadow.SetAssignment, parentScopeForwards []shadow.ParentScopeForward) (*ir.Package, error) {
		// Reset the todos collector each pass: ToIR can run more than once
		// (two-pass genex / stamp recovery) against the same collector, and
		// the producers Add on every pass. Resetting first means the report
		// reflects only the final pass's result rather than accumulating
		// duplicate entries across passes.
		todosCollector.Reset()
		return lower.ToIR(r, g, lower.Options{
			HostSourceRoot:                    a.SourceRoot,
			ElementSourceRoot:                 a.ElementSourceRoot,
			RecoverSourceComments:             a.EmitSourceComments,
			EmitInstallExportConfig:           a.EmitInstallExportConfig,
			EmitSharedLibraries:               a.EmitSharedLibraries,
			BackedFeatures:                    backedFeatures,
			HostPrefixDir:                     prefixAbs,
			BuildDir:                          hostBuildOrReply,
			Imports:                           imports,
			CTest:                             testRegistry,
			TraceRaw:                          traceRaw,
			LiftConfigureFile:                 a.LiftConfigureFile,
			CMakeVars:                         cmakeVars,
			GenexProbes:                       genexProbes,
			ConfigureLog:                      configureLogEvents,
			EmitStandaloneCustomCommands:      a.EmitStandaloneCustomCommands,
			UnsupportedExecuteProcessFallback: execFallback,
			FallbackInstallTarget:             fallbackInstallTarget(a.BazelPackagePath),
			BazelPackagePath:                  a.BazelPackagePath,
			CMakeScriptRunner:                 a.CMakeScriptRunner,
			CMakeScriptTrace:                  a.CMakeScriptTrace,
			CMakeScriptBake:                   a.CMakeScriptBake,
			LiftCCEmbed:                       a.LiftCCEmbed,
			LiftCCHash:                        a.LiftCCHash,
			BakeIn:                            convmode.BakeIn(a.BakeIn),
			Rejections:                        rejections,
			Coverage:                          coverageCollector,
			Todos:                             todosCollector,
			Warnings:                          os.Stderr,
			LiteralProbeSink:                  sink,
			LiteralResolutions:                resolutions,
			SetAssignments:                    setAssignments,
			ParentScopeForwards:               parentScopeForwards,
			StampVarSink:                      stampSink,
		})
	}

	// Pass 1: lower with a collecting sink. Arbitrary genex literals
	// the Go-side evaluator + structural probe can't resolve land in
	// the sink instead of being dropped.
	literalSink := &lower.LiteralProbeSink{}
	// recoveredStampSets holds the set()-copies recovered to lift a
	// git_describe()-style forwarded stamp that aborted pass 1; threaded into
	// the genex/stamp re-lifts below so they don't re-abort on the same call.
	var recoveredStampSets []shadow.SetAssignment
	// recoveredStampForwards holds the function-parameter-forwarded stamps
	// (git_describe()'s PARENT_SCOPE return) recovered alongside the set-copies,
	// threaded into the re-lifts so the forwarded value lifts to stamp_values
	// rather than baking.
	var recoveredStampForwards []shadow.ParentScopeForward
	pkg, err := runToIR(literalSink, nil, nil, nil)
	if err != nil {
		// A pass-1 abort on a forwarded stamp (git_describe() helper return,
		// SDL) is recoverable: lower.go fills stampSink before returning the
		// refusal, so when stamps were detected we recover the set()-copies
		// via a warm non-expanded-trace configure and re-lower — the
		// forwarded-stamp rescue then lifts the call. If recovery doesn't
		// apply or still fails, surface the original error.
		if a.TwoPassGenex && hostBuildDir != "" && len(stampSink) > 0 {
			if pkg2, sets, forwards, sink, ok := recoverPass1StampAbort(ctx, a, hostBuildDir, runToIR); ok {
				pkg, err = pkg2, nil
				recoveredStampSets = sets
				recoveredStampForwards = forwards
				literalSink = sink
				fmt.Fprintf(os.Stderr, "convert-element-cmake: recovered pass-1 stamp abort via %d non-expanded-trace set()-copy/-ies + %d function-forward(s).\n", len(sets), len(forwards))
			}
		}
		if err != nil {
			return err
		}
	}
	// genexResolutions carries the genex two-pass result forward to the
	// stamp set()-indirection pass below, so a project that needs BOTH
	// re-lifts keeps the genex resolutions in the final result.
	var genexResolutions map[string]cmakerun.LiteralResolution

	// Pass 2 (conditional, warm): if the first pass left unresolved
	// literals AND we have a live build dir to reconfigure, run a
	// second cmake configure against the SAME build dir injecting a
	// file(GENERATE) literal-probe hook, read the resolved bytes
	// back, and re-lift with them. Skipped when nothing was
	// unresolved (the common case → zero overhead), when the
	// operator opted out (--two-pass-genex=false), or in the offline
	// --reply-dir / --cmake-build-dir paths (no live build dir).
	if a.TwoPassGenex && hostBuildDir != "" && literalSink.Len() > 0 {
		reqs := literalSink.Requests()
		fmt.Fprintf(os.Stderr, "convert-element-cmake: %d unresolved genex literal(s) after pass 1; running warm second configure pass to resolve them.\n", len(reqs))
		if _, cfgErr := cmakerun.Configure(ctx, cmakerun.Options{
			SourceRoot:         a.SourceRoot,
			BuildDir:           hostBuildDir,
			PrefixDir:          a.PrefixDir,
			ToolchainCMakeFile: a.ToolchainCMakeFile,
			BuildType:          a.BuildType,
			BuildTypes:         a.BuildTypes,
			// Inject ONLY the literal-probe hook. No trace, no
			// structural probe, no dump-vars — those already ran in
			// pass 1, and adding them (or any cache var) would
			// disturb the warm cache. Reusing hostBuildDir keeps the
			// try_compile / find_package results cached, so this pass
			// costs a small fraction of the first configure.
			LiteralProbes: reqs,
			Stdout:        os.Stderr,
			Stderr:        os.Stderr,
		}); cfgErr != nil {
			// A failed second pass is non-fatal: fall back to pass 1's
			// result (the unresolved literals stay dropped, exactly as
			// they would without the two-pass feature). Surface the
			// degradation so it isn't invisible.
			fmt.Fprintf(os.Stderr, "convert-element-cmake: warning: warm second configure pass failed (%v); keeping pass-1 result with %d literal(s) unresolved.\n", cfgErr, len(reqs))
		} else if resolutions, readErr := cmakerun.ReadLiteralProbe(hostBuildDir); readErr != nil {
			fmt.Fprintf(os.Stderr, "convert-element-cmake: warning: reading literal-probe output failed (%v); keeping pass-1 result.\n", readErr)
		} else if len(resolutions) > 0 {
			genexResolutions = resolutions
			pkg2, err2 := runToIR(nil, resolutions, recoveredStampSets, recoveredStampForwards)
			if err2 != nil {
				return err2
			}
			pkg = pkg2
		}
	}

	// Stamp set()-indirection second pass (conditional, warm): if pass 1
	// found VCS-stamp variables and we have a live build dir, run a second
	// cmake configure capturing the NON-EXPANDED trace, recover its
	// `set(X ${Y})` verbatim copies, and re-lift. That lets a
	// configure_file referencing a COPY of a stamp var — `set(VERSION
	// ${GIT_SHA})` then `@VERSION@`, the Google-Benchmark shape — re-read
	// the live revision instead of baking it; the direct-reference case
	// already lifts in pass 1 without this. Skipped with no stamp vars
	// (the common case → zero overhead), when opted out, or offline (no
	// live build dir). A failed pass is non-fatal: keep the pass-1 result
	// (direct stamp vars only).
	if a.TwoPassGenex && hostBuildDir != "" && len(stampSink) > 0 && len(recoveredStampSets) == 0 {
		plainTrace := filepath.Join(hostBuildDir, "trace-plain.jsonl")
		fmt.Fprintf(os.Stderr, "convert-element-cmake: %d VCS-stamp var(s) after pass 1; running warm second configure (non-expanded trace) to recover set()-copy indirection.\n", len(stampSink))
		if _, cfgErr := cmakerun.Configure(ctx, cmakerun.Options{
			SourceRoot:         a.SourceRoot,
			BuildDir:           hostBuildDir,
			PrefixDir:          a.PrefixDir,
			ToolchainCMakeFile: a.ToolchainCMakeFile,
			BuildType:          a.BuildType,
			BuildTypes:         a.BuildTypes,
			// We add NO new hooks or cache vars here — only the
			// non-expanded trace. (The dump-vars / probe hooks staged in
			// pass 1 may still re-run via the cached
			// CMAKE_PROJECT_TOP_LEVEL_INCLUDES, but their build-dir output
			// paths are filtered out of ExtractSetAssignments by the
			// sourceRoot gate, so they don't perturb the recovered copies.)
			// Reusing hostBuildDir keeps try_compile / find_package cached,
			// so this costs a fraction of the first configure.
			TracePath:        plainTrace,
			TraceNonExpanded: true,
			Stdout:           os.Stderr,
			Stderr:           os.Stderr,
		}); cfgErr != nil {
			fmt.Fprintf(os.Stderr, "convert-element-cmake: warning: warm second configure (stamp set-trace) failed (%v); keeping pass-1 result with direct stamp vars only.\n", cfgErr)
		} else if raw, readErr := os.ReadFile(plainTrace); readErr != nil {
			fmt.Fprintf(os.Stderr, "convert-element-cmake: warning: reading non-expanded trace failed (%v); keeping pass-1 result.\n", readErr)
		} else {
			sets := shadow.ExtractSetAssignments(raw, a.SourceRoot)
			forwards := shadow.ExtractParentScopeForwards(raw, a.SourceRoot)
			if len(sets) > 0 || len(forwards) > 0 {
				pkg3, err3 := runToIR(nil, genexResolutions, sets, forwards)
				if err3 != nil {
					return err3
				}
				pkg = pkg3
			}
		}
	}
	// Always materialize the rejections report when its path is
	// set so consumers (CI gates, downstream scripts) can rely on
	// the file existing. Empty array when no rejections fired or
	// the diagnostic flag wasn't on.
	if a.RejectionsReport != "" {
		items := []rejection.Rejection{}
		if rejections != nil {
			items = rejections.Items()
			if items == nil {
				items = []rejection.Rejection{}
			}
		}
		body, _ := json.MarshalIndent(items, "", "  ")
		if err := os.MkdirAll(filepath.Dir(a.RejectionsReport), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(a.RejectionsReport, append(body, '\n'), 0o644); err != nil {
			return err
		}
	}
	if rejections != nil && rejections.Len() > 0 {
		fmt.Fprintf(os.Stderr, "convert-element-cmake: --ignore-rejections-for-diagnostics collected %d rejection(s); output BUILD.bazel is best-effort and not guaranteed to build\n", rejections.Len())
	}

	if a.Verify {
		if err := runVerifyPass(a, hostBuildDir, pkg); err != nil {
			return err
		}
	}

	// auditBlobs collects every emitted BUILD.bazel blob so the
	// Phase 7 idiom audit can run over each (single-BUILD: one blob;
	// --split-packages: one per emitted package). Keeps the audit
	// running over the full output regardless of layout.
	var auditBlobs [][]byte

	if a.SplitPackages {
		// --split-packages: one BUILD.bazel per directory, mirroring
		// the CMakeLists/add_subdirectory layout. Root → a.OutBuild;
		// subdir "src/util" → <dir(a.OutBuild)>/src/util/BUILD.bazel.
		tree, err := bazel.EmitSplit(pkg, bazel.Options{
			SourceKey:          a.SourceKey,
			BazelPackagePath:   a.BazelPackagePath,
			EmitProvenance:     a.EmitProvenance,
			EmitSourceComments: a.EmitSourceComments,
		})
		if err != nil {
			return err
		}
		rootDir := filepath.Dir(a.OutBuild)
		// Deterministic write order (sorted dirs) for stable logs.
		dirs := make([]string, 0, len(tree))
		for d := range tree {
			dirs = append(dirs, d)
		}
		sort.Strings(dirs)
		for _, d := range dirs {
			var dst string
			if d == "" {
				dst = a.OutBuild
			} else {
				dst = filepath.Join(rootDir, filepath.FromSlash(d), "BUILD.bazel")
			}
			if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
				return err
			}
			if err := os.WriteFile(dst, tree[d], 0o644); err != nil {
				return err
			}
			auditBlobs = append(auditBlobs, tree[d])
		}
	} else {
		out, err := bazel.EmitWithOptions(pkg, bazel.Options{
			SourceKey:          a.SourceKey,
			BazelPackagePath:   a.BazelPackagePath,
			EmitProvenance:     a.EmitProvenance,
			EmitSourceComments: a.EmitSourceComments,
		})
		if err != nil {
			// canonicalize failures arrive pre-typed as
			// *failure.Error{Code: BazelCanonicalizeFailed}; #210.
			// Constraint-pass violations stay untyped — they're
			// converter-side data-integrity bugs, which the schema
			// classes as Tier-2 (the orchestrator collects them by
			// exit code, not by stable dedup key). handleError
			// routes both correctly.
			return err
		}
		if err := os.MkdirAll(filepath.Dir(a.OutBuild), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(a.OutBuild, out, 0o644); err != nil {
			return err
		}
		auditBlobs = append(auditBlobs, out)
	}

	// Phase 7: post-emission Bazel-idiom audit. Runs
	// unconditionally — the audit is read-only and FormatFindings
	// returns "" when there are no findings, so silent on clean
	// conversions. --audit-bazel-idiom-report writes the
	// structured findings as JSON in addition. With --split-packages,
	// every emitted BUILD is audited and findings are aggregated.
	{
		var findings []bazelidiom.Finding
		for _, blob := range auditBlobs {
			f, ferr := bazelidiom.Audit(blob)
			if ferr != nil {
				return failure.New(failure.BazelCanonicalizeFailed, "audit-bazel-idiom: %v", ferr)
			}
			findings = append(findings, f...)
		}
		if msg := bazelidiom.FormatFindings(findings); msg != "" {
			fmt.Fprint(os.Stderr, msg)
		}
		if a.AuditBazelIdiomReport != "" {
			// Coerce a nil findings slice to an empty slice so
			// json.MarshalIndent emits `[]` rather than `null` —
			// JSON consumers iterating the report expect an
			// array shape unconditionally.
			if findings == nil {
				findings = []bazelidiom.Finding{}
			}
			body, _ := json.MarshalIndent(findings, "", "  ")
			if err := os.MkdirAll(filepath.Dir(a.AuditBazelIdiomReport), 0o755); err != nil {
				return err
			}
			if err := os.WriteFile(a.AuditBazelIdiomReport, body, 0o644); err != nil {
				return err
			}
		}
	}

	// Lens-3 dependency-coverage findings (collected during ToIR).
	// Surface on stderr always; write the JSON report when
	// --audit-coverage-report is set. Same always-on/optional-report
	// shape as the bazelidiom audit above.
	{
		findings := coverageCollector.Items()
		for _, f := range findings {
			fmt.Fprintf(os.Stderr, "coverage(%s): %s: %s\n", f.Target, f.Code, f.Message)
		}
		if a.AuditCoverageReport != "" {
			if findings == nil {
				findings = []coverage.Finding{}
			}
			body, _ := json.MarshalIndent(findings, "", "  ")
			if err := os.MkdirAll(filepath.Dir(a.AuditCoverageReport), 0o755); err != nil {
				return err
			}
			if err := os.WriteFile(a.AuditCoverageReport, body, 0o644); err != nil {
				return err
			}
		}
	}

	// Conversion-todos report (collected during ToIR). The stderr
	// breadcrumbs are emitted at their producer sites and retained; this
	// writes the deterministic conversion-todos.json for the AI post-pass.
	// On by default (--conversion-todos); destination is the explicit
	// --conversion-todos-report, else <dir(out-build)>/conversion-todos.json.
	// With neither resolvable (no out-build), it's a silent no-op. Always
	// materialized when written (empty todos list when nothing fired) so
	// consumers can rely on the path.
	todosDest := a.ConversionTodosReport
	if todosDest == "" && a.OutBuild != "" {
		todosDest = filepath.Join(filepath.Dir(a.OutBuild), "conversion-todos.json")
	}
	if a.ConversionTodos && todosDest != "" {
		pre, perr := todos.LoadPreamble(a.ConversionTodosPreamble)
		if perr != nil {
			return fmt.Errorf("--conversion-todos-preamble %s: %w", a.ConversionTodosPreamble, perr)
		}
		report := todosCollector.Report(pre, "")
		body, merr := json.MarshalIndent(report, "", "  ")
		if merr != nil {
			// Evidence is map[string]any; a producer adding a
			// non-marshalable value should fail loudly, not write a
			// truncated/invalid report.
			return fmt.Errorf("marshal conversion-todos report: %w", merr)
		}
		if err := os.MkdirAll(filepath.Dir(todosDest), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(todosDest, append(body, '\n'), 0o644); err != nil {
			return err
		}
	}

	// Stage 6 of the per-element multi-platform plan: ship the
	// lowered ir.Package as JSON alongside the rendered
	// BUILD.bazel so the orchestrator's fold can compose
	// per-platform IRs without re-parsing Bazel rules. Only the
	// orchestrator's multi-platform path sets this; single-
	// platform conversions ignore it.
	if a.OutIRJSON != "" {
		body, err := json.MarshalIndent(pkg, "", "  ")
		if err != nil {
			return fmt.Errorf("marshal ir.Package: %w", err)
		}
		if err := os.MkdirAll(filepath.Dir(a.OutIRJSON), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(a.OutIRJSON, body, 0o644); err != nil {
			return err
		}
	}

	// Producer self-description channels (bundle + exports.json) share
	// the export namespace recovered from the trace — the codemodel
	// drops the install(EXPORT ... NAMESPACE ...) prefix, so without
	// this both would fall back to the project name (right only when
	// project name == export namespace). The namespace stem keys the
	// bundle (lib/cmake/<stem>/<stem>Config.cmake) so a consumer's
	// find_package(<stem>) (with CMAKE_FIND_PACKAGE_PREFER_CONFIG)
	// resolves against this bundle instead of the host's Find<stem>
	// module — e.g. project("zlib") + NAMESPACE ZLIB:: ⇒
	// lib/cmake/ZLIB/ZLIBConfig.cmake exporting ZLIB::ZLIB.
	var exportNS, bundlePkgName, nsPrefix string
	var aliases []cmakecfg.Alias
	if a.OutBundleDir != "" || a.OutExports != "" {
		exportNS = exportNamespaceForPackage(traceRaw, pkg.Name)
		bundlePkgName = pkg.Name
		if exportNS != "" {
			bundlePkgName = strings.TrimSuffix(exportNS, "::")
		}
		nsPrefix = exportNS
		if nsPrefix == "" {
			nsPrefix = bundlePkgName + "::"
		}
		// Recover add_library(<alias> ALIAS <target>) redirects so a
		// consumer linking the alias name (e.g. ZLIB::ZLIB aliasing
		// the real target zlibstatic) resolves both at cmake-configure
		// time (bundle) and at lower time (exports.json). The codemodel
		// omits ALIAS targets; the trace records them.
		importable := map[string]bool{}
		for _, t := range cmakecfg.ImportableTargets(pkg) {
			importable[t.Name] = true
		}
		aliases = recoverAliases(traceRaw, a.SourceRoot, importable)
	}

	if a.OutBundleDir != "" {
		bundle, err := cmakecfg.Emit(pkg, cmakecfg.Options{
			Namespace:   exportNS,
			PackageName: bundlePkgName,
			Aliases:     aliases,
		})
		if err != nil {
			return err
		}
		// Stage cmakecfg's flat <Pkg>*.cmake files into a temp
		// dir, then run synthprefix.Build to lay them out in
		// the cross-element synth-prefix shape — bundle .cmake
		// files at lib/cmake/<Pkg>/, plus zero-byte stubs at
		// every IMPORTED_LOCATION_<CONFIG> path the bundle
		// references and mkdir'd INTERFACE_INCLUDE_DIRECTORIES.
		// Downstream consumers can then `tar -xf <bundle> -C
		// $PREFIX` to materialize the slice; cmake's
		// find_package(<Pkg> CONFIG) resolves and the
		// imported-target EXISTS checks pass against the
		// stubs.
		flatDir, err := os.MkdirTemp("", "convert-element-bundle-flat-*")
		if err != nil {
			return err
		}
		defer os.RemoveAll(flatDir)
		for name, body := range bundle.Files {
			if err := os.WriteFile(filepath.Join(flatDir, name), body, 0o644); err != nil {
				return err
			}
		}
		// Capture producer-shipped cmake macros: the codemodel's
		// per-directory installer list carries every install(FILES
		// *.cmake DESTINATION lib/cmake/<Pkg>) the producer wrote.
		// Drop those into flatDir alongside the synthesized
		// <Pkg>*.cmake; synthprefix.Build's copy loop then sweeps
		// them through into lib/cmake/<Pkg>/ in the bundle. Real-
		// world helpers (KDE's ECM, GoogleTest's GoogleTest module,
		// etc.) flow without a separate plumbing path.
		if err := stageInstalledCmakeFiles(r, pkg.Name, flatDir); err != nil {
			return err
		}
		// synthprefix.Build refuses to write into an existing
		// dir; it owns its dst. The CLI lets callers point
		// --out-bundle-dir at a fresh path (Bazel genrules
		// hand us one), so removing the empty dir Bazel may
		// have created is safe and keeps the contract.
		if err := os.RemoveAll(a.OutBundleDir); err != nil {
			return err
		}
		if err := synthprefix.BuildSlice(a.OutBundleDir, []synthprefix.DepBundle{{
			Pkg:       bundlePkgName,
			SourceDir: flatDir,
		}}); err != nil {
			return err
		}
	}

	if a.OutExports != "" {
		doc := buildExportsDoc(pkg, bundlePkgName, nsPrefix, a.BazelPackagePath, aliases, a.SplitPackages)
		body, err := json.MarshalIndent(doc, "", "  ")
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(a.OutExports), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(a.OutExports, append(body, '\n'), 0o644); err != nil {
			return err
		}
	}

	if a.OutTimings != "" {
		total := time.Since(t0)
		// translation = total - configure (configureElapsed is 0 in
		// the --reply-dir offline path, so translation == total there).
		translation := total - configureElapsed
		if translation < 0 {
			translation = 0
		}
		body, _ := json.MarshalIndent(timings{
			Version:            1,
			CMakeConfigureSecs: configureElapsed.Seconds(),
			TranslationSecs:    translation.Seconds(),
			TotalSecs:          total.Seconds(),
		}, "", "  ")
		if err := os.MkdirAll(filepath.Dir(a.OutTimings), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(a.OutTimings, append(body, '\n'), 0o644); err != nil {
			return err
		}
	}

	if a.OutCMakeConfigureReads != "" {
		// The build.ninja oracle: cmake's own list of files whose bytes
		// should re-trigger configure. Project against the source root
		// to drop cmake-stdlib modules and build-tree configure outputs;
		// callers compare the result against per-kind narrowing
		// patterns to flag undercoverage drift.
		//
		// Source-root choice: the live-cmake path uses --source-root
		// directly; the offline --reply-dir path falls back to whatever
		// the recording captured (the build.ninja's absolute paths
		// remain the recording-time root, so projection works only when
		// SourceRoot matches). Empty SourceRoot → projector returns nil
		// and we write an empty array, which is unambiguous in the
		// downstream consumer.
		//
		// Build-dir choice: live-cmake uses the host tmpdir we
		// configured against; offline --reply-dir uses the build
		// path the codemodel recorded (Codemodel.Paths.Build), NOT
		// ReplyDir itself — ReplyDir is the
		// `<build>/.cmake/api/v1/reply` subdir, four levels too
		// deep. Using ReplyDir would break the in-source-buildDir
		// exclude in ProjectToSourceTree (build-tree artifacts
		// like `<build>/CMakeCache.txt` wouldn't be recognized
		// as "inside buildDir" and would leak into the oracle).
		//
		// When build.ninja wasn't parseable (g == nil — older cmake or
		// non-ninja generator), we still write the file but as an
		// empty array, so scripts that always expect the artifact to
		// exist when the flag is set don't fail with ENOENT. Audit
		// consumers see "no oracle data" via the empty array, which
		// is the right semantic.
		var reads []string
		if g != nil {
			buildDirForProj := hostBuildDir
			if buildDirForProj == "" {
				buildDirForProj = r.Codemodel.Paths.Build
			}
			inputs := g.ReconfigureInputs()
			// Fold in file(GLOB ... CONFIGURE_DEPENDS) matches. cmake
			// re-globs these at build time and reconfigures when the
			// match set changes, but the ninja RERUN_CMAKE edge depends
			// on the glob *stamp*, not the matched files, so
			// ReconfigureInputs() alone misses them. cmakeFiles-v1.1
			// records the resolved set authoritatively; without this a
			// new file matching a CONFIGURE_DEPENDS glob wouldn't
			// invalidate the converter's cache the way it re-triggers
			// cmake. ProjectToSourceTree dedups, so the union is safe.
			for _, gl := range r.CMakeFiles.GlobsDependent {
				inputs = append(inputs, gl.Paths...)
			}
			reads = ninja.ProjectToSourceTree(inputs, a.SourceRoot, buildDirForProj)
		}
		if reads == nil {
			reads = []string{}
		}
		body, err := json.MarshalIndent(reads, "", "  ")
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(a.OutCMakeConfigureReads), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(a.OutCMakeConfigureReads, append(body, '\n'), 0o644); err != nil {
			return err
		}
	}

	if a.OutReadPaths != "" && hostBuildDir != "" {
		traceHost := filepath.Join(hostBuildDir, "trace.jsonl")
		raw, err := os.ReadFile(traceHost)
		if err != nil {
			return fmt.Errorf("read trace: %w", err)
		}
		reads := shadow.ExtractReadPaths(raw, a.SourceRoot)
		body, err := json.MarshalIndent(reads, "", "  ")
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(a.OutReadPaths), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(a.OutReadPaths, append(body, '\n'), 0o644); err != nil {
			return err
		}
	}
	return nil
}

// compileCommandsPath returns the path to the compile_commands.json
// cmake emitted, or "" if neither the live build dir nor the offline
// fixture has one. Live runs always have it (we pass
// -DCMAKE_EXPORT_COMPILE_COMMANDS=ON); offline runs see it only if a
// recording script captured it alongside the reply.
func compileCommandsPath(hostBuildDir, replyDir string) string {
	if hostBuildDir != "" {
		p := filepath.Join(hostBuildDir, "compile_commands.json")
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	if replyDir != "" {
		p := filepath.Join(replyDir, "compile_commands.json")
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

// copyDirContents recursively copies srcDir's contents into dstDir,
// creating dstDir if absent. Used by the Stage 6 toolchain-signal
// capture: cmake's File API reply directory is small (a few JSON
// files), so a recursive copy is cheap and a regular file/dir
// shape is what the unifier's --element-signal consumer expects.
//
// Symlinks are skipped explicitly. filepath.Walk uses Lstat, so a
// symlinked directory wouldn't be traversed and a file symlink
// would be dereferenced by the os.ReadFile below (potentially
// pulling data from outside srcDir). Cmake's fileapi never
// produces symlinks, so the only way one would appear here is via
// a hostile build dir; rejecting them keeps the captured tree
// honest.
func copyDirContents(srcDir, dstDir string) error {
	// Lstat srcDir up front: filepath.Walk uses Lstat too but its
	// rel == "." early-return would silently mask a symlinked
	// srcDir as "no entries to copy" — the resulting empty
	// dstDir would mislead downstream consumers. Reject the
	// symlinked-root and the not-a-directory cases here so the
	// error names the actual problem.
	rootInfo, err := os.Lstat(srcDir)
	if err != nil {
		return fmt.Errorf("copyDirContents: stat srcDir: %w", err)
	}
	if rootInfo.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("copyDirContents: refusing to copy symlinked srcDir %s", srcDir)
	}
	if !rootInfo.IsDir() {
		return fmt.Errorf("copyDirContents: srcDir %s is not a directory (mode %s)", srcDir, rootInfo.Mode())
	}
	// dstDir comes from --out-toolchain-signal-dir. Reject empty
	// or obviously-broad paths up front: an unguarded
	// os.RemoveAll on "/", ".", or ".." would nuke anything
	// reachable from the converter's cwd. Both relative and
	// absolute paths are accepted (REAPI passes the relative
	// "toolchain-signal" inside the action working dir); guardDstDir
	// rejects only the dangerous shapes — see its docstring for
	// the exact rules.
	if err := guardDstDir(dstDir); err != nil {
		return err
	}
	// Reset dstDir's CONTENTS (not the directory itself) so the
	// result exactly mirrors srcDir without leaving stale JSONs.
	// Removing the directory and recreating it would also work
	// but interacts badly when dstDir is, say, a bind mount or
	// a path the parent process expects to keep open.
	if err := clearDirContents(dstDir); err != nil {
		return err
	}
	if err := os.MkdirAll(dstDir, 0o755); err != nil {
		return err
	}
	return filepath.Walk(srcDir, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(srcDir, p)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		// Reject anything that isn't a regular file or directory.
		// fileapi only writes those; surfacing the unexpected
		// type as an error catches a hostile build dir before
		// it leaks data into the unifier's input.
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("copyDirContents: refusing to copy symlink at %s", rel)
		}
		dst := filepath.Join(dstDir, rel)
		if info.IsDir() {
			return os.MkdirAll(dst, 0o755)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("copyDirContents: refusing to copy non-regular file %s (mode %s)", rel, info.Mode())
		}
		body, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		return os.WriteFile(dst, body, info.Mode().Perm())
	})
}

// guardDstDir refuses paths whose accidental misuse as a
// "wipe everything under here" target would be catastrophic.
// The function is the cheap first line before clearDirContents —
// it doesn't try to be exhaustive, just to catch the obvious
// foot-guns. Both relative and absolute paths are permitted:
// REAPI-driven conversions pass a relative path
// ("toolchain-signal") inside the action's working directory,
// so an absolute-only check would break that flow.
//
// Rejected:
//
//   - empty path
//   - "/", ".", ".." (and any path that filepath.Clean reduces to one)
//   - relative paths whose Clean form starts with ".." (would
//     escape the cwd)
//   - absolute paths that match a forbidden system root
//     (/home, /root, /tmp, /var, /etc, /usr — top-level dirs
//     the operator should never aim at as a wipe target).
func guardDstDir(dstDir string) error {
	if dstDir == "" {
		return fmt.Errorf("copyDirContents: dstDir is empty")
	}
	clean := filepath.Clean(dstDir)
	switch clean {
	case "/", ".", "..":
		return fmt.Errorf("copyDirContents: refusing to operate on dstDir %q (resolves to %q)", dstDir, clean)
	}
	// Relative path that escapes cwd? Reject — clearDirContents
	// would happily blow away the parent.
	if !filepath.IsAbs(clean) {
		if clean == ".." || strings.HasPrefix(clean, "../") {
			return fmt.Errorf("copyDirContents: refusing to operate on dstDir %q (escapes cwd)", dstDir)
		}
		// Non-escaping relative path: REAPI-style
		// "toolchain-signal" lands here. Allow.
		return nil
	}
	// Absolute path: reject the obvious system-root foot-guns.
	for _, forbid := range []string{"/", "/home", "/root", "/tmp", "/var", "/etc", "/usr"} {
		if clean == forbid {
			return fmt.Errorf("copyDirContents: refusing to operate on dstDir %q (matches forbidden root %q)", dstDir, forbid)
		}
	}
	return nil
}

// clearDirContents removes the entries inside dir without
// removing dir itself. Skips silently when dir doesn't exist
// (the subsequent os.MkdirAll handles the create case).
//
// Rejects a symlinked dir: guardDstDir's string-only checks
// don't help if the operator points the symlink at /, /etc,
// etc. Lstat'ing here closes that hole — the symlink target's
// contents are never wiped because we error out before reading
// the directory.
func clearDirContents(dir string) error {
	info, err := os.Lstat(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("clearDirContents: refusing to clear symlinked dstDir %s", dir)
	}
	if !info.IsDir() {
		return fmt.Errorf("clearDirContents: %s is not a directory (mode %s)", dir, info.Mode())
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
			return err
		}
	}
	return nil
}

// handleError marshals a typed Tier-1 failure to OutFailure (if requested) and
// returns the appropriate exit code.
// stageInstalledCmakeFiles copies every install(FILES *.cmake
// DESTINATION lib/cmake/<pkgName>[/<sub>]) target file into
// flatDir. cmakecfg's synthesized bundle lands flat in flatDir
// already; layering producer-shipped helpers in the same dir
// lets synthprefix.Build pick them up via its existing
// `*.cmake → lib/cmake/<Pkg>/` copy loop.
//
// Conservative scope:
//   - only `type=="file"` installers (install(DIRECTORY) /
//     install(EXPORT) handled elsewhere or implicitly).
//   - only destinations that match `lib/cmake/<pkgName>` exactly,
//     or any prefix-tree-shaped destination starting with
//     `lib/cmake/`. Helpers cmake configure-finds via
//     find_package(<Pkg>) live there.
//   - only files with `.cmake` extension; other shipped data
//     belongs in different filegroups (Phase 4 typed slices).
//
// Subdirectory destinations (e.g. lib/cmake/<Pkg>/modules) lose
// their nested layout when flattened into flatDir. v1 only
// surfaces the top level; nested layouts are a follow-up if a
// FDSDK-shape fixture surfaces them.
func stageInstalledCmakeFiles(r *fileapi.Reply, pkgName, flatDir string) error {
	cmakeSrc := r.Codemodel.Paths.Source
	for _, dir := range r.Directories {
		dirSrc := dir.Paths.Source
		if dirSrc == "" {
			dirSrc = cmakeSrc
		} else if !filepath.IsAbs(dirSrc) {
			dirSrc = filepath.Join(cmakeSrc, dirSrc)
		}
		for _, inst := range dir.Installers {
			if inst.Type != "file" {
				continue
			}
			if !cmakeConfigDestination(inst.Destination, pkgName) {
				continue
			}
			for _, raw := range inst.Paths {
				var p string
				if err := json.Unmarshal(raw, &p); err != nil {
					// install(FILES) records plain strings; an
					// {"from":..,"to":..} object is the
					// install(DIRECTORY) shape and shouldn't appear
					// here, but skip rather than fail.
					continue
				}
				if filepath.Ext(p) != ".cmake" {
					continue
				}
				abs := p
				if !filepath.IsAbs(abs) {
					abs = filepath.Join(dirSrc, p)
				}
				body, err := os.ReadFile(abs)
				if err != nil {
					// Producer-shipped file referenced by the
					// installer but missing on disk is unusual but
					// not a hard error — skip silently so the
					// bundle still synthesizes.
					continue
				}
				dst := filepath.Join(flatDir, filepath.Base(p))
				if err := os.WriteFile(dst, body, 0o644); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// cmakeConfigDestination reports whether dest is the canonical
// shape cmake's find_package(CONFIG) probes. Accepts:
//   - lib/cmake/<pkgName>          (canonical)
//   - lib/cmake/<pkgName>/<sub>    (nested helper layout)
//   - lib/cmake/<anything>         (NOT — restrict to our pkg
//     so unrelated install rules don't pollute the bundle).
//
// Case-sensitive: cmake's filesystem checks are case-sensitive
// on Linux and the codemodel records the user-written
// destination verbatim.
func cmakeConfigDestination(dest, pkgName string) bool {
	want := "lib/cmake/" + pkgName
	if dest == want {
		return true
	}
	return strings.HasPrefix(dest, want+"/")
}

// exportNamespaceForPackage recovers the install(EXPORT ... NAMESPACE
// ...) prefix the producer declared for the bundle that lands under
// lib/cmake/<pkgName>. The cmake File API codemodel drops the
// namespace (shadow.InstallExportCall documents why), so without this
// cmakecfg falls back to its "<pkgName>::" guess — correct only when
// the project name matches the export namespace. Returns "" when no
// namespace-bearing install(EXPORT) is recoverable, leaving cmakecfg's
// default in place.
func exportNamespaceForPackage(traceRaw []byte, pkgName string) string {
	if len(traceRaw) == 0 {
		return ""
	}
	// classifyInstallExport ignores sourceRoot/knownTargets, so the
	// empty/nil args are fine here.
	exports := shadow.Decode(traceRaw, "", nil).InstallExports
	fallback := ""
	for _, e := range exports {
		if e.Namespace == "" {
			continue
		}
		if fallback == "" {
			fallback = e.Namespace
		}
		// Prefer the export whose DESTINATION is this package's
		// own cmake-config dir; that's the bundle cmakecfg emits.
		if cmakeConfigDestination(strings.TrimSuffix(e.Destination, "/"), pkgName) {
			return e.Namespace
		}
	}
	return fallback
}

// cmakeDefinesToMap parses --cmake-define KEY=VALUE entries into the
// ExtraCacheVars map cmakerun.Configure passes as -D<KEY>=<VALUE>. An entry
// with no '=' maps to an empty value (cmake treats -DKEY as KEY="").
func cmakeDefinesToMap(defs []string) map[string]string {
	if len(defs) == 0 {
		return nil
	}
	m := make(map[string]string, len(defs))
	for _, d := range defs {
		k, v, _ := strings.Cut(d, "=")
		m[k] = v
	}
	return m
}

// buildExportsDoc assembles this element's exports manifest: one
// manifest.Element (named after the bundle's package name) whose
// exports map each importable library's real namespaced cmake target
// (<nsPrefix><target>) to this element's Bazel label
// (//<bazelPkgPath>:<target>). The target set matches cmakecfg's
// bundle exactly (both go through cmakecfg.ImportableTargets), so the
// config-mode and label-mapping channels stay in lockstep. Content is
// source-intrinsic and sorted by cmake_target, so the file is
// byte-stable across rebuilds that don't change the export surface — a
// consumer staging it via --exports-in only re-converts when the
// surface actually changes, not on every producer edit.
// When split is true, an importable target declared under an
// add_subdirectory child resolves to the sub-package label
// (//<bazelPkgPath>/<subdir>:<target>) via pkg.SubPackages, matching
// where --split-packages actually emits the rule. Install-derived
// importable targets (cc_imports) carry no SubPackages entry and stay
// at the element root, so their labels keep the //<bazelPkgPath>:<target>
// form. OFF keeps the legacy single-package label.
func buildExportsDoc(pkg *ir.Package, pkgName, nsPrefix, bazelPkgPath string, aliases []cmakecfg.Alias, split bool) *manifest.Imports {
	pkgPathFor := func(target string) string {
		if !split {
			return bazelPkgPath
		}
		sub := pkg.SubPackages[target]
		switch {
		case sub == "" || sub == ".":
			return bazelPkgPath
		case bazelPkgPath == "":
			return sub
		default:
			return bazelPkgPath + "/" + sub
		}
	}
	label := func(target string) string {
		if pp := pkgPathFor(target); pp != "" {
			return "//" + pp + ":" + target
		}
		return ":" + target
	}
	// Phase 6 (resolved-lift manifest-synth, M3): if the lowering
	// pass projected a declarative install(EXPORT) bundle into the
	// IR, it left a "cmake_config_bundle" filegroup plus one
	// `<lib>_import` cc_import / cc_interface facade per exported
	// target (tagged "cmake-codegen-install-export-import"). Those
	// targets always live at the element root (no SubPackages
	// entry, see ir.Package.SubPackages), so frame their labels
	// with bazelPkgPath directly — the same absolute framing the
	// export BazelLabels above use, so they resolve cross-element.
	// Carrying them on the exports manifest lets a downstream
	// find_package(<Pkg> CONFIG) consumer resolve straight to the
	// synthesized bundle + the per-artifact cc_import facades
	// without re-deriving them. Stays empty for imperative bundles
	// (no cmake_config_bundle in the IR) and non-cmake exports, so
	// non-bundle elements emit byte-identical exports.json.
	bundleLabel, importLabels := cmakeBundleLabels(pkg, bazelPkgPath)
	libs := cmakecfg.ImportableTargets(pkg)
	exports := make([]*manifest.Export, 0, len(libs)+len(aliases))
	for _, lib := range libs {
		ex := &manifest.Export{
			CMakeTarget:            nsPrefix + lib.Name,
			BazelLabel:             label(lib.Name),
			CMakeConfigBundleLabel: bundleLabel,
			CMakeImportLabels:      importLabels,
		}
		// B: variable-only Find modules (no <Pkg>::<Pkg> target)
		// resolve via ${<Pkg>_LIBRARIES} → a path or -l<name>. Carry
		// the produced lib's link name + synth-prefix-anchored path so
		// the consumer's link-fragment redirect (LookupLinkLibrary /
		// LookupLinkPath) maps it to this element whether it resolved
		// against our prefix or the host.
		if name := linkLibName(lib.ArtifactName); name != "" {
			ex.LinkLibraries = []string{name}
			ex.LinkPaths = []string{lower.ManifestPrefixAnchor + installRel(lib)}
		}
		exports = append(exports, ex)
	}
	// Alias entries map the verbatim consumer-facing name (e.g.
	// ZLIB::ZLIB) to the underlying target's label, so a consumer
	// linking the alias resolves to the same element.
	for _, a := range aliases {
		exports = append(exports, &manifest.Export{
			CMakeTarget:            a.Name,
			BazelLabel:             label(a.Underlying),
			CMakeConfigBundleLabel: bundleLabel,
			CMakeImportLabels:      importLabels,
		})
	}
	sort.Slice(exports, func(i, j int) bool {
		return exports[i].CMakeTarget < exports[j].CMakeTarget
	})
	return &manifest.Imports{
		Version:  1,
		Elements: []*manifest.Element{{Name: pkgName, Exports: exports}},
	}
}

// cmakeBundleLabels scans the lowered IR for the Phase 6 declarative
// install(EXPORT) projection and returns the absolute Bazel label of
// the synthesized "cmake_config_bundle" filegroup plus the sorted
// list of `<lib>_import` cc_import / cc_interface facade labels (those
// tagged "cmake-codegen-install-export-import"). All such targets live
// at the element root, so labels are framed with bazelPkgPath — the
// same absolute framing buildExportsDoc's BazelLabels use, so they
// resolve cross-element via the producer's exports manifest. Returns
// "" / nil when no declarative bundle is present (imperative bundles
// emit no cmake_config_bundle filegroup, and non-cmake exports have
// none either) so non-bundle elements emit byte-identical exports.json.
func cmakeBundleLabels(pkg *ir.Package, bazelPkgPath string) (string, []string) {
	rootLabel := func(name string) string {
		if bazelPkgPath != "" {
			return "//" + bazelPkgPath + ":" + name
		}
		return ":" + name
	}
	var bundleLabel string
	var importLabels []string
	for _, t := range pkg.Targets {
		switch {
		case t.Kind == ir.KindFilegroup && t.Name == "cmake_config_bundle":
			bundleLabel = rootLabel(t.Name)
		case (t.Kind == ir.KindCCImport || t.Kind == ir.KindCCInterface) &&
			hasTag(t.Tags, "cmake-codegen-install-export-import"):
			importLabels = append(importLabels, rootLabel(t.Name))
		}
	}
	// No declarative bundle → leave both empty so the exports.json
	// stays byte-identical to the pre-Phase-6 shape. The cc_import
	// facades only exist alongside the bundle filegroup, but guard on
	// the filegroup explicitly so a future shape that emits one
	// without the other can't leak orphan import labels.
	if bundleLabel == "" {
		return "", nil
	}
	sort.Strings(importLabels)
	return bundleLabel, importLabels
}

// hasTag reports whether tags contains want.
func hasTag(tags []string, want string) bool {
	for _, t := range tags {
		if t == want {
			return true
		}
	}
	return false
}

// linkLibName derives the linker -l<name> from a library artifact
// basename (libz.so → z, libfoo.a → foo, libz.so.1.3.1 → z). Returns
// "" for non-library artifacts or unparseable names. Versioned sonames
// collapse to the unversioned name so the key stays source-stable
// across version bumps that don't change the link name.
func linkLibName(artifact string) string {
	if !strings.HasPrefix(artifact, "lib") {
		return ""
	}
	rest := artifact[len("lib"):]
	for _, suffix := range []string{".so", ".a", ".dylib"} {
		if i := strings.Index(rest, suffix); i > 0 {
			return rest[:i]
		}
	}
	return ""
}

// installRel is the install-tree-relative path of a target's artifact
// (<dest>/<artifact>, dest defaulting to "lib"), matching cmakecfg's
// IMPORTED_LOCATION and the path synthprefix stages — so the anchored
// link_path lines up with the consumer's resolved fragment.
func installRel(t ir.Target) string {
	dest := t.InstallDest
	if dest == "" {
		dest = "lib"
	}
	return dest + "/" + t.ArtifactName
}

// recoverAliases extracts add_library(<alias> ALIAS <target>)
// redirects from the trace (the codemodel omits ALIAS targets),
// keeping only those whose underlying target is importable so the
// re-published alias doesn't dangle. Deterministic order (by alias
// name) keeps exports.json + the bundle byte-stable.
func recoverAliases(traceRaw []byte, sourceRoot string, importable map[string]bool) []cmakecfg.Alias {
	if len(traceRaw) == 0 {
		return nil
	}
	// classifyAddLibrary filters to in-source-tree call sites, so the
	// real source root is required (unlike install(EXPORT) recovery,
	// which isn't source-filtered).
	var out []cmakecfg.Alias
	seen := map[string]bool{}
	for _, call := range shadow.Decode(traceRaw, sourceRoot, nil).AddLibraries {
		if call.Type != "ALIAS" || len(call.Aliases) == 0 {
			continue
		}
		underlying := call.Aliases[0]
		if !importable[underlying] || seen[call.Name] {
			continue
		}
		seen[call.Name] = true
		out = append(out, cmakecfg.Alias{Name: call.Name, Underlying: underlying})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func handleError(a cli.Args, err error) int {
	var tier1 *failure.Error
	if errors.As(err, &tier1) {
		fmt.Fprintf(os.Stderr, "convert-element-cmake: %s\n", tier1.Error())
		if a.OutFailure != "" {
			payload, _ := json.MarshalIndent(map[string]any{
				"tier":    1,
				"code":    string(tier1.Code),
				"message": tier1.Message,
			}, "", "  ")
			_ = os.MkdirAll(filepath.Dir(a.OutFailure), 0o755)
			_ = os.WriteFile(a.OutFailure, append(payload, '\n'), 0o644)
		}
		return cli.ExitTier1
	}
	fmt.Fprintf(os.Stderr, "convert-element-cmake: %v\n", err)
	return cli.ExitTier2
}
