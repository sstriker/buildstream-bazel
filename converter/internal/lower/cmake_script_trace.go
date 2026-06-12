package lower

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/sstriker/buildstream-bazel/internal/shadow"
	"github.com/sstriker/buildstream-bazel/internal/sliceutil"
)

// execLookPath wraps exec.LookPath. Lives at package scope so
// the test seam (lookupExecutable) can override without
// hand-rolling a separate stdlib indirection.
var execLookPath = exec.LookPath

// PathClass tags an absolute path the cmake -P script tried to
// read by where it sits relative to the convert-host filesystem
// layout. The classifier drives the script-lift's auto-augment
// decision: source-class paths get added to the genrule's `srcs`,
// build-class paths get cross-referenced with the ninja graph
// for a producer, sysroot-class paths warn-and-proceed (the
// build host is responsible), unknown-class paths block the
// lift.
type PathClass int

const (
	// ClassSource — path resolves under the cmake project's
	// source root. Safe to lift: relativize, add to srcs.
	ClassSource PathClass = iota
	// ClassBuild — path resolves under the cmake build dir.
	// Either another lifted rule produces it (substitute
	// $(location :other)), or the script reads a cmake-side
	// configure-time output we can't reproduce at Bazel time.
	ClassBuild
	// ClassSysroot — path under standard system locations
	// (/usr, /lib, /opt — recognized prefixes). Operator's
	// toolchain dependency; we proceed but emit a warning so
	// the operator verifies host availability.
	ClassSysroot
	// ClassUnknown — anything else. Unsafe to lift; refuse.
	ClassUnknown
)

// sysrootPrefixes is the broader allow-list (vs systemLibPrefixes
// which is link-time only) of host directories whose contents are
// universally available at Bazel build time on the operator's
// runner image. Reading from these is safe (the script's
// hardcoded `/usr/bin/awk` resolves on a typical Linux runner);
// only the operator-toolchain dep needs verification.
var sysrootPrefixes = []string{
	"/usr/include/",
	"/usr/share/",
	"/usr/bin/",
	"/usr/sbin/",
	"/usr/local/include/",
	"/usr/local/share/",
	"/usr/local/bin/",
	"/usr/local/sbin/",
	"/bin/",
	"/sbin/",
	"/lib/",
	"/lib64/",
	"/usr/lib/",
	"/usr/lib32/",
	"/usr/lib64/",
	"/usr/local/lib/",
	"/usr/local/lib32/",
	"/usr/local/lib64/",
	"/etc/",
}

// classifyPath bins an absolute path into one of the PathClass
// values. Empty input returns ClassUnknown. Relative paths are
// rejected as ClassUnknown — the caller should resolve to
// absolute first (cmake's --trace records absolute paths so this
// almost never fires).
//
// Order of checks: source first (most common signal), then build
// (the producer-lookup case), then sysroot (host availability),
// else unknown. Source/build paths that happen to alias a
// sysroot prefix (e.g. a project that lives at `/usr/local/src/foo`)
// land in source/build because the more-specific prefix wins.
func classifyPath(path, sourceRoot, buildDir string) PathClass {
	if path == "" || !filepath.IsAbs(path) {
		return ClassUnknown
	}
	if sourceRoot != "" && underPrefix(path, sourceRoot) {
		return ClassSource
	}
	if buildDir != "" && underPrefix(path, buildDir) {
		return ClassBuild
	}
	for _, p := range sysrootPrefixes {
		if strings.HasPrefix(path, p) {
			return ClassSysroot
		}
	}
	return ClassUnknown
}

// underPrefix is filepath-aware HasPrefix: returns true when
// `path` is exactly `prefix` or sits under it as a child. The
// trailing separator nuance avoids `/foo/bar` matching prefix
// `/foo/b` falsely (without a separator after the prefix).
func underPrefix(path, prefix string) bool {
	if path == prefix {
		return true
	}
	if !strings.HasSuffix(prefix, string(filepath.Separator)) {
		prefix += string(filepath.Separator)
	}
	return strings.HasPrefix(path, prefix)
}

// TraceCmakeScript runs `cmake --trace-expand --trace-format=json-v1
// -P <script> [-D <var>=<val>...]` in a clean tmp dir and returns
// the trace bytes. The convert-time execution carries the same
// platform-coupling caveat documented in
// docs/design/conversion-architecture.md — the converter host
// must have cmake available (which it does today for the
// configure step) and the script's runtime behaviour reflects
// the convert host's environment.
//
// Side-effect risk: cmake -P scripts are arbitrary cmake code
// and can call `execute_process`, `file(REMOVE)`, etc. We run
// in a fresh tmp dir as workDir so any `file(WRITE)` /
// `file(REMOVE)` stays contained, but `execute_process(COMMAND
// rm -rf /...)` can still escape. The lift's `--cmake-script-trace`
// flag is therefore opt-in; operators acknowledge the risk by
// passing it.
//
// Timeout is hard-coded at 60s — well past any reasonable cmake
// -P script's runtime; longer scripts likely have an
// infinite-loop bug.
func TraceCmakeScript(ctx context.Context, cmakeBin, scriptPath string, dArgs []string, workDir string) ([]byte, error) {
	tmpDir, err := os.MkdirTemp("", "cmake-script-trace-*")
	if err != nil {
		return nil, fmt.Errorf("mktmpdir: %w", err)
	}
	defer os.RemoveAll(tmpDir)
	// workDir reproduces the custom command's WORKING_DIRECTORY so
	// cwd-relative reads (a script-mode `include(x.cmake)`) resolve
	// the way the real invocation's would; empty keeps the isolated
	// scratch cwd (the historical shape).
	if workDir == "" {
		workDir = tmpDir
	}

	tracePath := filepath.Join(tmpDir, "trace.jsonl")
	// -D cache args must PRECEDE -P: cmake treats everything after
	// the script path as CMAKE_ARGV script arguments, not variables
	// (a trailing -DALL_SQL_IN=... silently expands to "" inside the
	// script).
	argv := []string{
		"--trace-expand",
		"--trace-format=json-v1",
		"--trace-redirect=" + tracePath,
	}
	argv = append(argv, dArgs...)
	argv = append(argv, "-P", scriptPath)

	tctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	cmd := exec.CommandContext(tctx, cmakeBin, argv...)
	cmd.Dir = workDir
	// Sandbox the script's env — script may consult $HOME / locale
	// / etc. Match the cmakerun.Configure shape so the trace
	// reflects the same environment the configure step ran in.
	cmd.Env = []string{
		"HOME=" + tmpDir,
		"LC_ALL=C",
		"PATH=" + os.Getenv("PATH"),
	}
	// cmake -P writes diagnostics to stderr; capture & drop. The
	// trace itself lands in --trace-redirect.
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	if err := cmd.Run(); err != nil {
		// Even on non-zero exit, cmake may have written partial
		// trace data before failing. Try to read it; if both the
		// run and the read fail, surface the run error (more
		// actionable).
		body, readErr := os.ReadFile(tracePath)
		if readErr != nil {
			return nil, fmt.Errorf("cmake --trace -P: %w", err)
		}
		return body, nil
	}
	return os.ReadFile(tracePath)
}

// ScriptPathClassification is the per-path outcome of walking a
// cmake -P script's trace and classifying every read.
type ScriptPathClassification struct {
	// SourcePaths are package-relative slash-form paths under
	// sourceRoot the script reads. Auto-augmented into the
	// genrule's srcs.
	SourcePaths []string
	// BuildPaths are absolute paths under buildDir the script
	// reads. Each represents a cmake-side configure-time output;
	// the caller cross-references with the ninja graph for a
	// producer.
	BuildPaths []string
	// SysrootPaths are absolute paths under recognized sysroot
	// prefixes. Operator-toolchain-dep; warn-and-proceed.
	SysrootPaths []string
	// UnknownPaths are everything else. Block the lift; these
	// are typically convert-machine /tmp paths or operator-
	// vendored prefixes the Bazel sandbox won't have.
	UnknownPaths []string
}

// ClassifyScriptTrace walks the cmake -P trace bytes and groups
// every read path by PathClass. Returns the classification
// struct populated with sorted, deduped lists per class.
//
// Reuses shadow.ParseTrace + a local extractor that mirrors
// shadow.collectReadPath but doesn't gate on sourceRoot — we
// want every path so we can classify ALL of them.
func ClassifyScriptTrace(traceRaw []byte, sourceRoot, buildDir string) ScriptPathClassification {
	events := shadow.ParseTrace(traceRaw)
	bins := map[PathClass]map[string]struct{}{
		ClassSource:  {},
		ClassBuild:   {},
		ClassSysroot: {},
		ClassUnknown: {},
	}
	for _, ev := range events {
		for _, path := range scriptReadPathsFor(ev) {
			if path == "" {
				continue
			}
			if !filepath.IsAbs(path) {
				path = filepath.Join(filepath.Dir(ev.File), path)
			}
			bins[classifyPath(path, sourceRoot, buildDir)][path] = struct{}{}
		}
	}
	out := ScriptPathClassification{}
	for path := range bins[ClassSource] {
		rel, err := filepath.Rel(sourceRoot, path)
		if err != nil {
			continue
		}
		out.SourcePaths = append(out.SourcePaths, filepath.ToSlash(rel))
	}
	sort.Strings(out.SourcePaths)
	out.BuildPaths = sortedKeys(bins[ClassBuild])
	out.SysrootPaths = sortedKeys(bins[ClassSysroot])
	out.UnknownPaths = sortedKeys(bins[ClassUnknown])
	return out
}

// lookupCmakeBinary returns the convert-host cmake binary's
// path. The same `cmake` cmakerun.Configure shells to is what
// the trace step uses (consistent platform + version). Empty
// when cmake isn't on PATH — the trace step then declines, and
// the lift falls back to the un-traced shape.
func lookupCmakeBinary() string {
	p, err := lookupExecutable("cmake")
	if err != nil {
		return ""
	}
	return p
}

// lookupExecutable is a thin shim over exec.LookPath so the
// trace step can be tested against a fake binary path.
var lookupExecutable = func(name string) (string, error) {
	// Indirect through a package var so tests can override.
	return execLookPath(name)
}

// scriptReadPathsFor returns every path the trace event implies
// the script touched. Plural because execute_process(COMMAND
// awk -f /abs/path/to/script ...) carries the tool path AND the
// script's arguments, any of which may be absolute paths the
// sandbox needs.
//
// For include / configure_file / file(READ|...), returns the
// single path the command names. For execute_process, returns
// every absolute path that appears in the COMMAND arg list —
// the conservative heuristic catches both the tool (first arg)
// and any -f / -I / positional arguments that are paths.
// Non-absolute COMMAND args (option flags, literal values)
// are dropped; the classifier rejects relative paths anyway.
func scriptReadPathsFor(ev shadow.TraceEvent) []string {
	switch strings.ToLower(ev.Cmd) {
	case "include", "configure_file":
		if len(ev.Args) > 0 {
			return []string{ev.Args[0]}
		}
	case "file":
		if len(ev.Args) >= 2 {
			switch strings.ToUpper(ev.Args[0]) {
			case "READ", "STRINGS", "MD5", "SHA1", "SHA224", "SHA256", "SHA384", "SHA512":
				return []string{ev.Args[1]}
			}
		}
	case "execute_process":
		var out []string
		inCommand := false
		for _, a := range ev.Args {
			if strings.EqualFold(a, "COMMAND") {
				inCommand = true
				continue
			}
			// New keyword starts a new section; stop
			// collecting from the current COMMAND. cmake's
			// keyword set we care about: WORKING_DIRECTORY,
			// RESULT_VARIABLE, OUTPUT_VARIABLE,
			// ERROR_VARIABLE, OUTPUT_FILE, ERROR_FILE,
			// INPUT_FILE, TIMEOUT, ENCODING. Any uppercase
			// identifier terminates the run; the classifier
			// won't accept it as a path anyway, but stopping
			// here keeps the diagnostic tight.
			if inCommand && len(a) > 0 && isUpperKeyword(a) {
				inCommand = false
				continue
			}
			if !inCommand {
				continue
			}
			if filepath.IsAbs(a) {
				out = append(out, a)
			}
		}
		return out
	}
	return nil
}

// isUpperKeyword: cmake's execute_process keywords are uppercase
// ASCII identifiers. Treat any all-uppercase / underscore /
// digit token longer than two chars as a keyword. False
// positives don't matter much (the classifier rejects them as
// non-absolute paths); the test is to avoid eating the next
// section's arguments as if they were COMMAND paths.
func isUpperKeyword(s string) bool {
	if len(s) < 2 {
		return false
	}
	for _, r := range s {
		if r >= 'A' && r <= 'Z' {
			continue
		}
		if r == '_' {
			continue
		}
		if r >= '0' && r <= '9' {
			continue
		}
		return false
	}
	return true
}

func sortedKeys(m map[string]struct{}) []string {
	if len(m) == 0 {
		return nil
	}
	out := sliceutil.SortedKeys(m)
	return out
}
