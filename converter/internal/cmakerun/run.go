// Package cmakerun invokes cmake configure against a source/build pair
// and returns the File API reply directory.
//
// One Configure call corresponds to exactly one cmake invocation. The
// caller owns the build dir lifecycle (create, clean up); this package
// only writes inside it.
//
// Hermeticity is the caller's responsibility — typically achieved by
// running cmakerun.Configure inside an REAPI Action whose worker
// provides the sandbox. The package sets the deterministic env
// (SOURCE_DATE_EPOCH, locale, find_package suppression) on the cmake
// child process so configure-time outputs stay byte-stable across hosts
// even when no outer sandbox is in play.
package cmakerun

import (
	"context"
	_ "embed"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// dumpVarsCMake is the script Configure injects via
// -DCMAKE_PROJECT_TOP_LEVEL_INCLUDES (cmake 3.24+; we tried
// -DCMAKE_PROJECT_INCLUDE_AFTER first but it didn't honour the
// -D form in our test fixture, hence the switch). It registers
// a deferred callback that writes every cmake variable's value
// to `<build>/cmake-to-bazel.vars.dump` after all of
// CMakeLists.txt has executed. parseVarsDump reads that file
// back. See the script header for design rationale.
//
//go:embed dump-vars.cmake
var dumpVarsCMake []byte

// cmp0026ShimCMake wraps get_target_property so legacy
// `get_target_property(<v> <tgt> LOCATION)` calls survive cmake
// 4.x's CMP0026 removal by returning $<TARGET_FILE:<tgt>>
// instead. Opt-in via Options.CMP0026Shim; the script header
// documents the trade-offs.
//
//go:embed cmp0026-shim.cmake
var cmp0026ShimCMake []byte

// VarsDumpFilename is the basename of the variable dump
// dump-vars.cmake writes inside the build dir. Exposed so callers
// (offline test fixtures, the orchestrator's record path) can
// stash a recording mirror under the same name.
const VarsDumpFilename = "cmake-to-bazel.vars.dump"

// CMP0026ShimFilename is the basename of the cmp0026 compatibility
// shim Configure stages into the build dir when Options.CMP0026Shim
// is true.
const CMP0026ShimFilename = "cmake-to-bazel.cmp0026-shim.cmake"

// SourceDateEpoch is the project-wide fixed timestamp for deterministic
// configure-time outputs. 2020-01-01T00:00:00Z, picked arbitrarily to be
// visibly synthetic and not collide with real package mtimes.
const SourceDateEpoch = "1577836800"

// Options configures one Configure call.
type Options struct {
	// SourceRoot is the cmake source root (-S).
	SourceRoot string

	// BuildDir is the cmake build directory (-B). Caller owns lifecycle.
	BuildDir string

	// PrefixDir, when non-empty, is added to CMAKE_PREFIX_PATH so
	// find_package picks up the synthetic prefix tree of dep
	// <Pkg>Config.cmake bundles produced by previous conversions.
	PrefixDir string

	// ToolchainCMakeFile, when non-empty, is passed via
	// -DCMAKE_TOOLCHAIN_FILE=. Pre-derived by derive-toolchain;
	// skips cmake's compiler-detection probe, cutting per-conversion
	// configure latency.
	ToolchainCMakeFile string

	// BuildType is passed as -DCMAKE_BUILD_TYPE. Defaults to Release.
	BuildType string

	// ExtraCacheVars are additional cmake cache entries passed as
	// -D<name>=<value>. Rendered in lexicographic key order so the
	// argv is byte-stable across runs. Use this for any cache knob
	// that distinguishes a probe variant (compiler overrides,
	// sanitizer flags, custom toolchain cache vars). Callers must
	// not put CMAKE_BUILD_TYPE here — it has the dedicated BuildType
	// slot above; Configure rejects the duplication explicitly to
	// surface the misuse rather than silently letting cmake's
	// last-wins -D semantics pick a winner.
	ExtraCacheVars map[string]string

	// TracePath, when non-empty, enables `cmake --trace-expand
	// --trace-format=json-v1 --trace-redirect=<TracePath>`.
	TracePath string

	// DumpVars enables the post-configure variable-namespace
	// capture. When true, Configure stages dump-vars.cmake into
	// the build dir and passes
	// `-DCMAKE_PROJECT_TOP_LEVEL_INCLUDES=<staged>`; the
	// resulting cmake-to-bazel.vars.dump file feeds Reply.Vars,
	// which the configure_file lift uses for byte-stable
	// substitution at Bazel build time. Off by default — staging
	// the hook unconditionally would (a) override a project- or
	// operator-supplied CMAKE_PROJECT_TOP_LEVEL_INCLUDES value
	// and (b) emit a "manually-specified variables were not
	// used" warning on cmake < 3.24 (the variable was added
	// there). convert-element-cmake only flips this on when
	// --lift-configure-file is set.
	DumpVars bool

	// CMP0026Shim enables the cmake-4.x compatibility shim that
	// overrides get_target_property to translate LOCATION queries
	// into $<TARGET_FILE:<tgt>> generator expressions. cmake 4.x
	// removed the OLD behaviour of CMP0026, so legacy packages
	// reading `get_target_property(<v> <tgt> LOCATION)` (the
	// pre-3.0 idiom) fatal-error at configure time. The shim is
	// staged into the build dir as cmake-to-bazel.cmp0026-shim.cmake
	// and joined onto CMAKE_PROJECT_TOP_LEVEL_INCLUDES (alongside
	// the dump-vars hook when both are enabled). Off by default;
	// the shim changes get_target_property's return shape for ALL
	// LOCATION reads (generator expression rather than configure-
	// time path), which can break projects that string-compose
	// the LOCATION value at configure time. See #208.
	CMP0026Shim bool

	// Stdout/Stderr capture cmake output. Nil discards.
	Stdout, Stderr io.Writer
}

// Reply is the File API reply directory cmake produced, plus
// the captured variable namespace.
type Reply struct {
	// Path is the .cmake/api/v1/reply directory cmake wrote.
	Path string

	// Vars is the full set of cmake variables observed at end
	// of top-level directory processing — i.e., AFTER every
	// command in CMakeLists.txt has run. Populated from
	// `<build>/cmake-to-bazel.vars.dump` which dump-vars.cmake
	// wrote. Empty if the dump file was missing (e.g., the
	// user's CMakeLists.txt fatal-erred before reaching the
	// deferred callback).
	//
	// Used by the lower's configure_file recovery: with the
	// full namespace in hand, the lifted genrule's Bazel-time
	// Substitute can resolve any @VAR@ the user adds to a
	// .h.in template, even one that wasn't referenced when
	// the values were captured. Closes the soundness gap
	// where a per-template extracted dict would mis-render
	// new markers (PR #94 Copilot review).
	Vars map[string]string
}

// Configure runs cmake -B <build> -S <source>, with File API queries
// pre-staged for codemodel-v2, toolchains-v1, cmakeFiles-v1, and cache-v2.
// Returns the reply directory location on success.
func Configure(ctx context.Context, opts Options) (Reply, error) {
	if opts.SourceRoot == "" || opts.BuildDir == "" {
		return Reply{}, fmt.Errorf("cmakerun: SourceRoot and BuildDir required")
	}
	if opts.BuildType == "" {
		opts.BuildType = "Release"
	}

	queryDir := filepath.Join(opts.BuildDir, ".cmake", "api", "v1", "query")
	if err := os.MkdirAll(queryDir, 0o755); err != nil {
		return Reply{}, fmt.Errorf("cmakerun: stage query dir: %w", err)
	}
	for _, kind := range []string{"codemodel-v2", "toolchains-v1", "cmakeFiles-v1", "cache-v2"} {
		f, err := os.Create(filepath.Join(queryDir, kind))
		if err != nil {
			return Reply{}, fmt.Errorf("cmakerun: stage query %s: %w", kind, err)
		}
		_ = f.Close()
	}

	// Stage the dump-vars hook into the build dir when the
	// caller opted into the namespace capture (see
	// Options.DumpVars). When opted out we leave
	// CMAKE_PROJECT_TOP_LEVEL_INCLUDES alone so projects /
	// operators that set it don't get silently overridden, and
	// cmake < 3.24 doesn't see the unused-variable diagnostic.
	var dumpVarsPath string
	if opts.DumpVars {
		dumpVarsPath = filepath.Join(opts.BuildDir, "cmake-to-bazel.dump-vars.cmake")
		if err := os.WriteFile(dumpVarsPath, dumpVarsCMake, 0o644); err != nil {
			return Reply{}, fmt.Errorf("cmakerun: stage dump-vars hook: %w", err)
		}
	}

	// Stage the cmp0026 shim alongside dump-vars when the caller
	// opted in. Both are layered onto CMAKE_PROJECT_TOP_LEVEL_INCLUDES
	// as a `;`-joined list; cmake includes them in order, so the
	// shim's wrapper is installed before dump-vars enumerates the
	// namespace and after any project-level project() setup.
	var cmp0026ShimPath string
	if opts.CMP0026Shim {
		cmp0026ShimPath = filepath.Join(opts.BuildDir, CMP0026ShimFilename)
		if err := os.WriteFile(cmp0026ShimPath, cmp0026ShimCMake, 0o644); err != nil {
			return Reply{}, fmt.Errorf("cmakerun: stage cmp0026 shim: %w", err)
		}
	}

	// Empty HOME defeats ~/.cmake/packages reads when no outer sandbox
	// rewrites HOME. Best-effort cleanup; cmake only reads from here.
	homeDir, err := os.MkdirTemp("", "cmakerun-home-*")
	if err != nil {
		return Reply{}, fmt.Errorf("cmakerun: stage home: %w", err)
	}
	defer os.RemoveAll(homeDir)

	argv, err := buildCmakeArgv(opts, dumpVarsPath, cmp0026ShimPath)
	if err != nil {
		return Reply{}, err
	}

	cmd := exec.CommandContext(ctx, "cmake", argv...)
	// Tee cmake's stderr into a tail buffer so a failed run can
	// be annotated with hints for well-known incompat patterns
	// (cmake 4.x CMP0026, etc.) without breaking the live
	// op-stderr passthrough. The buffer is bounded to keep
	// memory usage predictable on projects whose configure
	// emits thousands of lines.
	stderrTail := &boundedBuffer{limit: 16 * 1024}
	cmd.Stdout = opts.Stdout
	if opts.Stderr != nil {
		cmd.Stderr = io.MultiWriter(opts.Stderr, stderrTail)
	} else {
		cmd.Stderr = stderrTail
	}
	cmd.Env = configureEnv(homeDir, opts.PrefixDir)

	if err := cmd.Run(); err != nil {
		return Reply{}, annotateConfigureFailure(err, stderrTail.Bytes())
	}

	// Best-effort read of the variable dump (only when we
	// staged the hook in the first place — without
	// opts.DumpVars cmake never sees dump-vars.cmake and the
	// file won't exist). When DumpVars is on but configure
	// fatal-erred before the deferred callback, the file is
	// absent and readVarsDump returns nil values; downstream
	// lower then falls back to either the per-template Extract
	// path or the legacy base64 shape.
	var vars map[string]string
	if opts.DumpVars {
		var err error
		vars, err = readVarsDump(filepath.Join(opts.BuildDir, VarsDumpFilename))
		if err != nil {
			return Reply{}, fmt.Errorf("cmakerun: read vars dump: %w", err)
		}
	}
	return Reply{
		Path: filepath.Join(opts.BuildDir, ".cmake", "api", "v1", "reply"),
		Vars: vars,
	}, nil
}

// buildCmakeArgv assembles cmake's command-line arguments from
// Options. Pure (no I/O) so the rendering is unit-testable. The
// only filesystem-touching step the function performs is
// resolving CMAKE_TOOLCHAIN_FILE to an absolute path — required
// because cmake resolves a relative CMAKE_TOOLCHAIN_FILE against
// the build-dir first, then the source-dir, and our executor's
// input-root layout matches neither.
//
// Argv order is fixed: -S, -B, -G, the dedicated -DCMAKE_BUILD_TYPE
// and -DCMAKE_EXPORT_COMPILE_COMMANDS, then ExtraCacheVars in
// lexicographic key order, then the optional --trace-* and
// CMAKE_TOOLCHAIN_FILE / CMAKE_PROJECT_TOP_LEVEL_INCLUDES tail.
// Stable across runs of the same Options.
func buildCmakeArgv(opts Options, dumpVarsPath, cmp0026ShimPath string) ([]string, error) {
	if _, ok := opts.ExtraCacheVars["CMAKE_BUILD_TYPE"]; ok {
		return nil, fmt.Errorf("cmakerun: CMAKE_BUILD_TYPE in ExtraCacheVars; use Options.BuildType instead")
	}

	argv := []string{
		"-S", opts.SourceRoot,
		"-B", opts.BuildDir,
		"-G", "Ninja",
		"-DCMAKE_BUILD_TYPE=" + opts.BuildType,
		"-DCMAKE_EXPORT_COMPILE_COMMANDS=ON",
	}

	if len(opts.ExtraCacheVars) > 0 {
		keys := make([]string, 0, len(opts.ExtraCacheVars))
		for k := range opts.ExtraCacheVars {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			argv = append(argv, "-D"+k+"="+opts.ExtraCacheVars[k])
		}
	}

	// CMAKE_PROJECT_TOP_LEVEL_INCLUDES (cmake 3.24+) is a
	// list-of-files variable cmake includes in order at the end
	// of the top-level project() call. Both the dump-vars hook
	// and the cmp0026 shim layer onto the same slot; emit a
	// `;`-joined list. shim first so its wrapper is installed
	// before dump-vars runs and any user code reaches LOCATION.
	//
	// Tried CMAKE_PROJECT_INCLUDE_AFTER first; cmake reported it
	// as a "manually-specified variable not used" in the
	// configure-file fixture, suggesting the variable isn't
	// honored when set only via -D (it expects the project to
	// set it via set(CACHE) or for it to be already in the
	// cache). _TOP_LEVEL_INCLUDES is explicitly designed for
	// this CLI-injection pattern.
	var topLevelIncludes []string
	if cmp0026ShimPath != "" {
		topLevelIncludes = append(topLevelIncludes, cmp0026ShimPath)
	}
	if dumpVarsPath != "" {
		topLevelIncludes = append(topLevelIncludes, dumpVarsPath)
	}
	if len(topLevelIncludes) > 0 {
		argv = append(argv, "-DCMAKE_PROJECT_TOP_LEVEL_INCLUDES="+strings.Join(topLevelIncludes, ";"))
	}
	if opts.TracePath != "" {
		argv = append(argv,
			"--trace-expand",
			"--trace-format=json-v1",
			"--trace-redirect="+opts.TracePath,
		)
	}
	if opts.ToolchainCMakeFile != "" {
		toolchainAbs, err := filepath.Abs(opts.ToolchainCMakeFile)
		if err != nil {
			return nil, fmt.Errorf("cmakerun: abs toolchain file: %w", err)
		}
		argv = append(argv, "-DCMAKE_TOOLCHAIN_FILE="+toolchainAbs)
	}
	return argv, nil
}

// ReadVarsDumpFromReplyDir loads cmake-to-bazel.vars.dump from a
// fileapi reply directory. Returns (nil, nil) when the file
// isn't present — same convention as readVarsDump — so offline
// callers can opportunistically pick up the captured cmake
// namespace without requiring it. Used by tests + the offline
// branch of convert-element-cmake where the operator passes a
// pre-recorded reply dir; the live cmakerun.Configure path uses
// the in-process Reply.Vars directly.
func ReadVarsDumpFromReplyDir(replyDir string) (map[string]string, error) {
	return readVarsDump(filepath.Join(replyDir, VarsDumpFilename))
}

// readVarsDump parses the file dump-vars.cmake wrote. Missing
// file → (nil, nil): the caller treats absence as "no vars
// captured" rather than a hard error. Malformed lines are a hard
// error so a buggy dump-vars.cmake doesn't silently produce a
// truncated namespace.
func readVarsDump(path string) (map[string]string, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	return parseVarsDump(body)
}

// parseVarsDump decodes the "<NAME>=<HEX>\n" stream
// dump-vars.cmake emits. Each value is hex-decoded back to its
// raw byte sequence so values containing newlines / quotes /
// semicolons round-trip losslessly. Volatile path-bearing
// variables are filtered (see filterVolatilePaths) so the
// resulting map is byte-stable across cmake invocations
// against the same source tree. Empty input → empty map.
func parseVarsDump(body []byte) (map[string]string, error) {
	out := map[string]string{}
	for i, line := range strings.Split(string(body), "\n") {
		if line == "" {
			continue
		}
		eq := strings.IndexByte(line, '=')
		if eq < 0 {
			return nil, fmt.Errorf("vars dump line %d: missing '=' in %q", i+1, line)
		}
		name := line[:eq]
		if name == "" {
			return nil, fmt.Errorf("vars dump line %d: empty variable name in %q", i+1, line)
		}
		raw, err := hex.DecodeString(line[eq+1:])
		if err != nil {
			return nil, fmt.Errorf("vars dump line %d: decode hex for %q: %w", i+1, name, err)
		}
		out[name] = string(raw)
	}
	return filterVolatilePaths(out), nil
}

// filterVolatilePaths drops the cmake variables whose values
// vary across configure runs of the same source tree — most
// notably the *_BINARY_DIR family (which point at the per-run
// temp build dir Configure created) and any other variable
// whose value happens to contain that build dir as a substring.
//
// Why the filter: the lift embeds the values map into a base64
// blob in BUILD.bazel.cmd, and BUILD.bazel must be byte-stable
// across runs of the same source tree (otherwise convert-
// element's Bazel cache key churns on every invocation). The
// volatile vars contribute no semantic value to typical
// `configure_file` templates — those reference variables like
// `PROJECT_VERSION`, not `CMAKE_BINARY_DIR`. If a template DOES
// reference one of the filtered variables, the lift's verify-
// pass (Substitute(template, values, opts) byte-equal vs cmake's
// rendered output) fails and the converter falls back to the
// legacy base64-of-rendered shape — soundness preserved.
//
// Two-stage filter:
//
//  1. Locate the build dir (CMAKE_BINARY_DIR if it's still in
//     the dump, plus any unique single root prefix all *_BINARY_DIR
//     paths share). Drop every entry whose value contains that
//     path. Catches the long tail of cmake-derived path variables
//     (CMAKE_PLATFORM_INFO_DIR, CMAKE_PROJECT_TOP_LEVEL_INCLUDES,
//     `<Pkg>_BINARY_DIR`, etc.) without a hand-maintained allow-
//     list.
//  2. Drop names matching `*_BINARY_DIR` / `*_SOURCE_DIR`
//     suffixes, plus a handful of explicit names whose VALUES
//     might happen to be stable (CMAKE_BUILD_TOOL, CMAKE_COMMAND,
//     etc., which point at the host-installed cmake binaries —
//     stable across runs but vary across machines).
func filterVolatilePaths(in map[string]string) map[string]string {
	if len(in) == 0 {
		return in
	}
	buildDirPaths := buildDirPrefixes(in)
	out := make(map[string]string, len(in))
	for name, val := range in {
		if isVolatileVarName(name) {
			continue
		}
		if containsAny(val, buildDirPaths) {
			continue
		}
		out[name] = val
	}
	return out
}

// buildDirPrefixes returns every distinct build-dir-shaped path
// the dump references — CMAKE_BINARY_DIR plus every variable
// ending in `_BINARY_DIR` (subprojects, external_projects, etc.
// each define one). Used by filterVolatilePaths to wipe entries
// that name the run's build dir, since the dir is a per-run
// tmpdir whose path varies across invocations even when the
// underlying cmake graph is identical.
//
// Multiple distinct prefixes are common when a project has
// nested CMakeLists in subdirs (each gets its own
// <subdir>_BINARY_DIR pointing at <build>/<subdir>); we collect
// them all rather than picking one. Returns nil when no
// build-dir variable is set (atypical; the caller treats this
// as "skip the volatility filter").
func buildDirPrefixes(in map[string]string) []string {
	seen := map[string]bool{}
	if v, ok := in["CMAKE_BINARY_DIR"]; ok && v != "" {
		seen[v] = true
	}
	for name, val := range in {
		if val == "" {
			continue
		}
		if strings.HasSuffix(name, "_BINARY_DIR") {
			seen[val] = true
		}
	}
	if len(seen) == 0 {
		return nil
	}
	out := make([]string, 0, len(seen))
	for p := range seen {
		out = append(out, p)
	}
	return out
}

func containsAny(s string, needles []string) bool {
	for _, n := range needles {
		if n == "" {
			continue
		}
		if strings.Contains(s, n) {
			return true
		}
	}
	return false
}

func isVolatileVarName(name string) bool {
	if strings.HasSuffix(name, "_BINARY_DIR") || strings.HasSuffix(name, "_SOURCE_DIR") {
		return true
	}
	switch name {
	case "CMAKE_HOME_DIRECTORY",
		"CMAKE_CACHEFILE_DIR",
		"CMAKE_FILES_DIRECTORY",
		"CMAKE_FIND_PACKAGE_REDIRECTS_DIR",
		"CMAKE_CURRENT_FUNCTION_LIST_DIR",
		"CMAKE_CURRENT_FUNCTION_LIST_FILE",
		"CMAKE_CURRENT_LIST_DIR",
		"CMAKE_CURRENT_LIST_FILE",
		"CMAKE_BUILD_TOOL",
		"CMAKE_COMMAND",
		"CMAKE_CTEST_COMMAND",
		"CMAKE_CPACK_COMMAND",
		"CMAKE_ROOT":
		return true
	}
	return false
}

// configureEnv returns the controlled env for the cmake child. PATH is
// inherited so cmake/ninja resolve via whatever the host or worker
// provides; the rest is fixed for cross-host determinism. The
// CMAKE_FIND_USE_*_PATH cluster suppresses host-leak find_package paths
// (see docs/cmake_analysis.md).
func configureEnv(homeDir, prefixDir string) []string {
	env := []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + homeDir,
		"LC_ALL=C",
		"LANG=C",
		"SOURCE_DATE_EPOCH=" + SourceDateEpoch,
		"CMAKE_FIND_USE_CMAKE_ENVIRONMENT_PATH=OFF",
		"CMAKE_FIND_USE_CMAKE_PATH=OFF",
		"CMAKE_FIND_USE_CMAKE_SYSTEM_PATH=OFF",
		"CMAKE_FIND_USE_PACKAGE_REGISTRY=OFF",
		"CMAKE_FIND_USE_SYSTEM_PACKAGE_REGISTRY=OFF",
		"CMAKE_FIND_USE_PACKAGE_ROOT_PATH=ON",
		"CMAKE_FIND_USE_SYSTEM_ENVIRONMENT_PATH=OFF",
		"CMAKE_FIND_PACKAGE_PREFER_CONFIG=ON",
	}
	if prefixDir != "" {
		env = append(env, "CMAKE_PREFIX_PATH="+prefixDir)
	}
	return env
}
