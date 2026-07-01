package lower

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/sstriker/buildstream-bazel/internal/shadow"
)

// WORKING_DIRECTORY / ENVIRONMENT / multi-COMMAND support for the
// file-producing hoist (refusal-audit close-out, part 2).
//
//   - WORKING_DIRECTORY: cmake runs the child with cwd moved off the
//     build root. A BUILD-DIR-anchored WD lifts: the genrule saves the
//     exec root (`_r="$$PWD"`), mkdir+cd's into the build-relative WD,
//     and every $(location …) / output reference is `$$_r/`-prefixed so
//     Bazel-resolved paths stay correct from the moved cwd. Relative
//     argv operands keep their cmake semantics for free (they resolved
//     against the WD there, and resolve against the same build-relative
//     cwd here). A SOURCE-tree or unanchorable WD stays refused — the
//     sandbox source tree is read-only, and an out-of-tree cwd has no
//     Bazel analog.
//   - ENVIRONMENT: cmake applies the K=V list to the child; the lift is
//     an `env 'K=V' …` prefix on every stage. Values are the expanded
//     trace's bytes — host-absolute values keep working exactly as far
//     as argv literals do (same portability trade, documented there).
//   - Multi-COMMAND: cmake chains stage stdout exactly like a shell
//     pipe and runs stages concurrently — `a | b` IS the semantics, so
//     an OUTPUT_FILE-bearing pipeline with no stamp/probe stage lifts
//     as a parenthesized pipe (parens give the stderr redirects a
//     single attachment point covering every stage, matching cmake's
//     all-stages ERROR_FILE sink).

// resolveExecWorkingDir anchors a call's WORKING_DIRECTORY under the
// build dir. Returns ("", "", true) when unset; (rel, "", true) for a
// build-dir WD (rel may be "." for the build root itself); a refusal
// reason otherwise.
func resolveExecWorkingDir(call shadow.ExecuteProcessCall, anc execAnchors) (string, string, bool) {
	wd := call.WorkingDirectory
	if wd == "" {
		return "", "", true
	}
	if !filepath.IsAbs(wd) {
		// cmake resolves a relative WORKING_DIRECTORY against its own
		// cwd — the build root under the runner contract.
		wd = filepath.Join(anc.recordedBuildDir, wd)
	}
	if rel, ok := executeProcessAnchorOutput(wd, anc); ok {
		if rel == "" {
			rel = "."
		}
		return rel, "", true
	}
	if anc.recordedBuildDir != "" && filepath.Clean(wd) == filepath.Clean(anc.recordedBuildDir) {
		return ".", "", true
	}
	return "", fmt.Sprintf("WORKING_DIRECTORY %q is not under the build dir (a source-tree cwd is read-only under the sandbox; an out-of-tree cwd has no Bazel analog)", call.WorkingDirectory), false
}

// execFileFieldAbs absolutizes an INPUT_FILE/OUTPUT_FILE/ERROR_FILE
// value: cmake resolves a relative one against the WORKING_DIRECTORY
// (or its own cwd, the build root). Absolute values pass through.
func execFileFieldAbs(p, wdRel string, anc execAnchors) string {
	if p == "" || filepath.IsAbs(p) {
		return p
	}
	base := anc.recordedBuildDir
	if wdRel != "" && wdRel != "." {
		base = filepath.Join(base, filepath.FromSlash(wdRel))
	}
	return filepath.Join(base, p)
}

// execEnvPrefix renders the ENVIRONMENT list as an `env` command
// prefix (empty when the list is).
func execEnvPrefix(environment []string) string {
	if len(environment) == 0 {
		return ""
	}
	parts := make([]string, 0, len(environment)+1)
	parts = append(parts, "env")
	for _, kv := range environment {
		// `$` must double for Bazel's make-variable expansion of the
		// genrule cmd ($ORIGIN-style values are a real idiom) BEFORE
		// shell quoting — otherwise analysis rejects the BUILD with
		// an unknown-make-variable error.
		parts = append(parts, shellQuoteArg(strings.ReplaceAll(kv, "$", "$$")))
	}
	return strings.Join(parts, " ") + " "
}

// rewriteExecArgvStage rewrites one COMMAND's argv for the hoisted cmd:
// source-anchored operands become declared srcs referenced via
// $(location …) (locPrefix-prefixed — `$$_r/` when a WORKING_DIRECTORY
// moved the cwd off the exec root), source-root directory operands stay
// literal relative paths (locPrefix-prefixed likewise), an absolute
// argv[0] strips to its portable basename, and everything else passes
// through shell-quoted. Verbatim behavior extraction from
// liftFileProducing's original inline loop.
func rewriteExecArgvStage(argv []string, anc execAnchors, locPrefix string, srcs *[]string, srcSet map[string]bool) []string {
	rewritten := make([]string, 0, len(argv))
	for i, a := range argv {
		if rel, ok := executeProcessAnchorSource(a, anc); ok {
			isDir := rel == "" || isExistingDir(filepath.Join(anc.hostSrcDir, rel))
			if rel == "" {
				rel = "."
			}
			if isDir {
				if locPrefix != "" {
					// Double-quoted, NOT shellQuoteArg: the `$$_r`
					// must expand at action time (single quotes
					// would freeze it).
					rewritten = append(rewritten, `"`+locPrefix+rel+`"`)
					continue
				}
				rewritten = append(rewritten, shellQuoteArg(rel))
				continue
			}
			if !srcSet[rel] {
				srcSet[rel] = true
				*srcs = append(*srcs, rel)
			}
			rewritten = append(rewritten, fmt.Sprintf("%s$(location %s)", locPrefix, rel))
			continue
		}
		if i == 0 && filepath.IsAbs(a) {
			rewritten = append(rewritten, shellQuoteArg(filepath.Base(a)))
			continue
		}
		rewritten = append(rewritten, shellQuoteArg(a))
	}
	return rewritten
}

// pipelineHasStampOrProbeStage reports whether ANY stage's driver is a
// stamp or strong-probe driver — such pipelines keep their stage-0
// stamp/probe classification (or refusal) rather than lifting: hoisting
// a non-hermetic stage re-introduces exactly what the refusal prevents.
func pipelineHasStampOrProbeStage(call shadow.ExecuteProcessCall) bool {
	for _, argv := range call.Commands {
		if len(argv) == 0 {
			continue
		}
		// A `cmake -E env/chdir`-WRAPPED stage hides its real driver
		// behind argv[0]=cmake; strip the wrappers before the check
		// so `cmake -E env TZ=UTC git describe | head -1` is seen as
		// the git stamp it is. Any cmake stage that doesn't fully
		// strip (a real -E op, a -P script, a configure) also blocks
		// the lift — a cmake stage inside a pipe isn't a shape the
		// hoist models.
		stripped, fullyStripped := stripCMakeEWrappers(argv)
		if !fullyStripped && isCMakeDriver(argv[0]) {
			return true
		}
		argv = stripped
		// Also peel bare shell wrappers (env / taskset / nice / … via
		// stripWrapperPrefix) so BOTH the driver classification and the
		// host-detection-script check see the real command: `env GIT_DIR=… git
		// describe | head` is the git stamp it wraps, and `env sh config.guess |
		// head` is the config.guess probe it wraps (else a wrapped probe with
		// OUTPUT_FILE would mis-lift as file-producing → host leakage).
		peeled := stripWrapperPrefix(argv)
		if len(peeled) == 0 {
			peeled = argv
		}
		driver := executeProcessDriverBasename(peeled[0])
		if stampDrivers[driver] || strongProbeDrivers[driver] || executeProcessRunsHostDetectionScript(peeled) {
			return true
		}
	}
	return false
}

// execWdPrologue renders the cwd-move prologue for a WORKING_DIRECTORY
// lift: save the exec root in `_r` (every Bazel-resolved reference is
// then `$$_r/`-prefixed), create the build-relative cwd, move into it.
// Empty when no WORKING_DIRECTORY is set, keeping the historical cmd
// bytes for the common shape.
func execWdPrologue(wdRel string) string {
	if wdRel == "" {
		return ""
	}
	return fmt.Sprintf(`_r="$$PWD" && mkdir -p %s && cd %s && `, shellQuoteArg(wdRel), shellQuoteArg(wdRel))
}

// execOutRef renders the single-out redirect target: the plain `$@` for
// the historical shape, exec-root-prefixed when a WORKING_DIRECTORY
// moved the cwd. One parameter — the prefix is derived, not passed, so
// the wd/prefix pair can't drift apart at call sites.
func execOutRef(wdRel string) string {
	if wdRel == "" {
		return "$@"
	}
	return "$$_r/$@"
}
