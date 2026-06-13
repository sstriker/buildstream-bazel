package lower

import (
	"path/filepath"
	"strings"

	"github.com/sstriker/buildstream-bazel/internal/shadow"
)

// cmake -E WRAPPER normalization (refusal-audit close-out, part 3):
// `cmake -E env K=V <cmd…>` and `cmake -E chdir <dir> <cmd…>` aren't
// operations — they're modifiers around an inner command, exactly the
// keywords execute_process spells as ENVIRONMENT / WORKING_DIRECTORY.
// Unwrapping them before classification folds the modifier into the
// call's fields (which the file-producing lifter now models) and lets
// the INNER command classify on its own merits: `cmake -E env
// GEN_FAST=1 python3 gen.py` is the python3 call, `cmake -E chdir gen
// tool` is the tool call under a cwd. Likewise `cmake -E cat / echo /
// <algo>sum` are portable POSIX-equivalents — rewriting them to the
// raw argv routes them through the existing raw-driver classification
// (OUTPUT_FILE-bearing forms hoist as genrules; console-only forms
// skip benignly via the console arm).
func normalizeCMakeECall(call shadow.ExecuteProcessCall) shadow.ExecuteProcessCall {
	if len(call.Commands) != 1 {
		return call
	}
	argv := call.Commands[0]
	changed := false
	for len(argv) >= 3 && isCMakeDriver(argv[0]) && argv[1] == "-E" {
		op := strings.ToLower(argv[2])
		switch op {
		case "env":
			rest := argv[3:]
			i := 0
			var envs []string
			for ; i < len(rest); i++ {
				a := rest[i]
				if strings.HasPrefix(a, "--") {
					// --unset= / --modify: not modeled; keep the
					// original call so the refusal names the real shape.
					return call
				}
				if !strings.Contains(a, "=") {
					break
				}
				envs = append(envs, a)
			}
			if i >= len(rest) {
				// `env` with no inner command: nothing to unwrap.
				return call
			}
			call.Environment = append(append([]string(nil), call.Environment...), envs...)
			argv = rest[i:]
			changed = true
		case "chdir":
			if len(argv) < 5 {
				return call
			}
			dir := argv[3]
			if !filepath.IsAbs(dir) && call.WorkingDirectory != "" {
				// A relative chdir resolves against the cwd the outer
				// WORKING_DIRECTORY already moved to.
				dir = filepath.Join(call.WorkingDirectory, dir)
			}
			call.WorkingDirectory = dir
			argv = argv[4:]
			changed = true
		default:
			if consoleOnlyDrivers[op] {
				// cmake -E's portable spellings of the POSIX tools
				// (the SAME table the console-only classification
				// consults — one list, no drift); the raw argv routes
				// through the standard classification (cmake's
				// <algo>sum output format matches coreutils').
				argv = append([]string{op}, argv[3:]...)
				call.Commands = [][]string{argv}
				return call
			}
			// A real -E operation (copy, tar, …): classification owns it.
			if changed {
				call.Commands = [][]string{argv}
			}
			return call
		}
	}
	if changed {
		call.Commands = [][]string{argv}
	}
	return call
}

// consoleOnlyDrivers are tools whose stdout is the entire point; with
// no OUTPUT_FILE and nothing captured, the call is configure-time
// console noise (progress messages, checksum prints) with no
// consumable Bazel output — a benign skip, not a refusal.
var consoleOnlyDrivers = map[string]bool{
	"echo":      true,
	"cat":       true,
	"md5sum":    true,
	"sha1sum":   true,
	"sha224sum": true,
	"sha256sum": true,
	"sha384sum": true,
	"sha512sum": true,
}

// stripCMakeEWrappers peels `cmake -E env K=V…` / `cmake -E chdir <dir>`
// prefixes off ONE argv (the per-stage view pipelineHasStampOrProbeStage
// needs — wrappers hide the real driver behind argv[0]=cmake). Returns
// the inner argv and whether the result no longer starts with a cmake
// driver (fullyStripped=false means a cmake stage remains: a real -E
// op, -P script, or configure).
func stripCMakeEWrappers(argv []string) ([]string, bool) {
	for len(argv) >= 3 && isCMakeDriver(argv[0]) && argv[1] == "-E" {
		switch strings.ToLower(argv[2]) {
		case "env":
			rest := argv[3:]
			i := 0
			for ; i < len(rest); i++ {
				if strings.HasPrefix(rest[i], "--") || !strings.Contains(rest[i], "=") {
					break
				}
			}
			if i >= len(rest) || strings.HasPrefix(rest[i], "--") {
				return argv, false
			}
			argv = rest[i:]
		case "chdir":
			if len(argv) < 5 {
				return argv, false
			}
			argv = argv[4:]
		default:
			return argv, false
		}
	}
	return argv, len(argv) > 0 && !isCMakeDriver(argv[0])
}
