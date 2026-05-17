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
	"strings"
	"time"

	"github.com/sstriker/buildstream-bazel/converter/emit/bazel"
	"github.com/sstriker/buildstream-bazel/converter/internal/cli"
	"github.com/sstriker/buildstream-bazel/converter/internal/cmakerun"
	"github.com/sstriker/buildstream-bazel/converter/internal/ctest"
	"github.com/sstriker/buildstream-bazel/converter/internal/emit/cmakecfg"
	"github.com/sstriker/buildstream-bazel/converter/internal/failure"
	"github.com/sstriker/buildstream-bazel/converter/internal/fileapi"
	"github.com/sstriker/buildstream-bazel/converter/internal/lower"
	"github.com/sstriker/buildstream-bazel/converter/internal/ninja"
	"github.com/sstriker/buildstream-bazel/converter/internal/verify"
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

func main() {
	args, code := cli.Parse(os.Args[1:], os.Stderr)
	if code != cli.ExitSuccess {
		os.Exit(code)
	}
	if err := run(args); err != nil {
		os.Exit(handleError(args, err))
	}
}

func run(a cli.Args) error {
	t0 := time.Now()
	var configureElapsed time.Duration

	replyDir := a.ReplyDir
	var ninjaPath string
	var hostBuildDir string
	var cmakeVars map[string]string
	if replyDir == "" {
		ctx := context.Background()

		// Architectural floor: cmake >= 3.20 (codemodel-v2 minimum). The
		// orchestrator (M3) must always run with a pinned cmake; the
		// escape hatch is for local dev only.
		if !a.AllowCMakeVersionMismatch {
			if _, _, _, err := cmakerun.AssertVersion(ctx); err != nil {
				return failure.New(failure.ConfigureFailed, "%v", err)
			}
		}

		// Real-cmake path: spin a tmp build dir, configure cmake against
		// it, then load the reply.
		buildDir, err := os.MkdirTemp("", "convert-element-build-*")
		if err != nil {
			return err
		}
		hostBuildDir = buildDir
		defer os.RemoveAll(buildDir)

		opts := cmakerun.Options{
			SourceRoot:         a.SourceRoot,
			BuildDir:           buildDir,
			PrefixDir:          a.PrefixDir,
			ToolchainCMakeFile: a.ToolchainCMakeFile,
			// DumpVars only when --lift-configure-file is on:
			// the dump hook overrides project/operator-supplied
			// CMAKE_PROJECT_TOP_LEVEL_INCLUDES and triggers a
			// "manually-specified variable not used" warning on
			// cmake < 3.24, so we don't pay that cost for
			// elements that don't need the captured namespace.
			DumpVars:    a.LiftConfigureFile,
			CMP0026Shim: a.CMP0026Shim,
			Stdout:      os.Stderr, // route cmake noise to our stderr
			Stderr:      os.Stderr,
		}
		if a.OutReadPaths != "" {
			opts.TracePath = filepath.Join(buildDir, "trace.jsonl")
		}
		configureStart := time.Now()
		reply, err := cmakerun.Configure(ctx, opts)
		configureElapsed = time.Since(configureStart)
		if err != nil {
			return failure.New(failure.ConfigureFailed, "%v", err)
		}
		replyDir = reply.Path
		cmakeVars = reply.Vars
		ninjaPath = filepath.Join(buildDir, "build.ninja")
	} else {
		// Offline path: a build.ninja is sometimes checked in alongside the
		// reply (recording script captures both); use it if present.
		candidate := filepath.Join(filepath.Dir(replyDir), "..", "..", "..", "build.ninja")
		// fileapi reply directory layout is <build>/.cmake/api/v1/reply, so
		// build.ninja lives four parents up. Resolve and check.
		candidate, _ = filepath.Abs(candidate)
		if _, err := os.Stat(candidate); err == nil {
			ninjaPath = candidate
		}
		// Test fixtures stash build.ninja directly inside the reply dir for
		// convenience; check there too.
		if direct := filepath.Join(replyDir, "build.ninja"); ninjaPath == "" {
			if _, err := os.Stat(direct); err == nil {
				ninjaPath = direct
			}
		}
		// Offline path: opportunistically pick up the captured
		// cmake variable namespace from cmake-to-bazel.vars.dump
		// in the reply dir. The live path gets this via
		// cmakerun.Configure's Reply.Vars; the offline path
		// previously left cmakeVars nil, which silently disabled
		// the (a) genex evaluator (Context derived from cmakeVars
		// is empty → every genex.UnsupportedError → fall back to
		// (b) / legacy). Missing dump file → nil map, same
		// behaviour as before; recorded fixtures opt in by
		// stashing the dump alongside the fileapi reply.
		if vars, err := cmakerun.ReadVarsDumpFromReplyDir(replyDir); err != nil {
			return failure.New(failure.FileAPIMissing, "read vars dump: %v", err)
		} else if len(vars) > 0 {
			cmakeVars = vars
		}
	}

	r, err := fileapi.Load(replyDir)
	if err != nil {
		return failure.New(failure.FileAPIMissing, "load reply: %v", err)
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
	if a.ImportsManifest != "" {
		imports, err = manifest.Load(a.ImportsManifest)
		if err != nil {
			return err
		}
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
		tracePath = filepath.Join(a.ReplyDir, "trace.jsonl")
	}
	if tracePath != "" {
		if body, readErr := os.ReadFile(tracePath); readErr == nil {
			traceRaw = body
		}
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

	pkg, err := lower.ToIR(r, g, lower.Options{
		HostSourceRoot:                    a.SourceRoot,
		HostPrefixDir:                     prefixAbs,
		BuildDir:                          hostBuildOrReply,
		Imports:                           imports,
		CTest:                             testRegistry,
		TraceRaw:                          traceRaw,
		LiftConfigureFile:                 a.LiftConfigureFile,
		CMakeVars:                         cmakeVars,
		UnsupportedExecuteProcessFallback: a.UnsupportedExecuteProcessFallback,
	})
	if err != nil {
		return err
	}
	if a.Verify {
		ccPath := compileCommandsPath(hostBuildDir, a.ReplyDir)
		if ccPath != "" {
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
		}
	}

	out, err := bazel.EmitWithOptions(pkg, bazel.Options{
		SourceKey:        a.SourceKey,
		BazelPackagePath: a.BazelPackagePath,
	})
	if err != nil {
		// EmitWithOptions surfaces two error shapes: typed
		// Tier-1 failures from the pre-emit constraint pass
		// (already structured) and bytes-unparseable errors
		// from canonicalize. Wrap the latter as a Tier-1
		// `bazel-canonicalize-failed` so operators get
		// failure.json with a stable code instead of a
		// raw process exit (#210).
		var tier1 *failure.Error
		if !errors.As(err, &tier1) {
			err = failure.New(failure.BazelCanonicalizeFailed, "%v", err)
		}
		return err
	}
	if err := os.MkdirAll(filepath.Dir(a.OutBuild), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(a.OutBuild, out, 0o644); err != nil {
		return err
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

	if a.OutBundleDir != "" {
		bundle, err := cmakecfg.Emit(pkg, cmakecfg.Options{})
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
			Pkg:       pkg.Name,
			SourceDir: flatDir,
		}}); err != nil {
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
			reads = ninja.ProjectToSourceTree(g.ReconfigureInputs(), a.SourceRoot, buildDirForProj)
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
