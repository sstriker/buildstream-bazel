package lower

import "strings"

// rewriteCMakeEInvocations rewrites every `cmake -E <op> ...`
// (or `<tool>/cmake -E <op> ...`) occurrence in cmd to its POSIX
// shell equivalent when the op has a clear analogue. Runs after
// rewriteGenruleCmd's anchor / host-bin passes so the cmd is
// already normalised — the cmake-E ops only fire on the bare
// `cmake -E` form that survives the host-bin strip.
//
// Why this matters: cmake's Ninja-side cmd records
// `add_custom_command(... cmake -E copy A B)` shapes verbatim.
// Bazel's hermetic genrule action runs under bash without cmake
// on the PATH guaranteed. POSIX rewrite (`cp A B`) sidesteps the
// dependency and matches what operators would write by hand.
//
// Supported ops + POSIX equivalents:
//
//	make_directory <dirs...>     → mkdir -p <dirs...>
//	create_symlink T L           → ln -sfn T L
//	copy / copy_if_different     → cp <src> <dst>
//	copy_directory               → cp -r <src> <dst>
//	copy_directory_if_different  → cp -r <src> <dst>
//	remove <files...>            → rm -f <files...>
//	remove_directory <dirs...>   → rm -rf <dirs...>
//	rename <src> <dst>           → mv <src> <dst>
//	touch <files...>             → touch <files...>
//	true                         → true
//	echo <args...>               → echo <args...>
//
// Ops without a clear POSIX analogue (`env`, `time`, `chdir`,
// `compare_files` with its specific exit-code semantics) pass
// through unchanged — operators see the original cmd and can
// stage a cmake runner if they need it.
func rewriteCMakeEInvocations(cmd string) string {
	// Loop because the cmd can chain (`cmake -E ... && cmake -E ...`).
	// Each iteration finds the next `cmake -E ` and either rewrites
	// it or skips it via a sentinel.
	for {
		idx := strings.Index(cmd, "cmake -E ")
		if idx < 0 {
			break
		}
		// Find start of the cmake token (including any preceding
		// `/abs/path/`).
		tokStart := idx
		for tokStart > 0 {
			b := cmd[tokStart-1]
			if b == ' ' || b == '\t' || b == ';' || b == '&' || b == '|' {
				break
			}
			tokStart--
		}
		argsStart := idx + len("cmake -E ")
		// Find end of the cmake -E invocation: stop at `&&`, `||`,
		// `;`, or end-of-string.
		argsEnd := len(cmd)
		for j := argsStart; j < len(cmd); j++ {
			if cmd[j] == ';' {
				argsEnd = j
				break
			}
			if j+1 < len(cmd) && (cmd[j] == '&' && cmd[j+1] == '&' || cmd[j] == '|' && cmd[j+1] == '|') {
				argsEnd = j
				break
			}
		}
		// Strip trailing whitespace from the args slice.
		end := argsEnd
		for end > argsStart && (cmd[end-1] == ' ' || cmd[end-1] == '\t') {
			end--
		}
		args := cmd[argsStart:end]
		rewritten, ok := posixForCMakeEGenrule(args)
		if !ok {
			// Skip this occurrence: replace the `cmake -E ` bytes
			// with a sentinel so the next strings.Index doesn't
			// re-find this position and spin.
			cmd = cmd[:idx] + "@CMAKE_E_SKIP@" + cmd[argsStart:]
			continue
		}
		// Preserve whitespace between the rewritten args and a
		// trailing shell separator (`&&` / `||` / `;`). The args
		// slice's trailing-whitespace strip dropped it.
		sep := ""
		if argsEnd < len(cmd) && argsEnd > end {
			sep = " "
		}
		cmd = cmd[:tokStart] + rewritten + sep + cmd[argsEnd:]
	}
	// Restore the skip markers to their original form.
	cmd = strings.ReplaceAll(cmd, "@CMAKE_E_SKIP@", "cmake -E ")
	return cmd
}

// posixForCMakeEGenrule returns the POSIX shell rewrite of a
// `cmake -E <op> <args...>` invocation, given the slice that
// follows `cmake -E `. Returns ok=false for ops we don't rewrite.
//
// Named with the Genrule suffix to disambiguate from cmake's
// execute_process-side handling in execute_process_classify.go;
// the two use cases diverge enough (one rewrites cmd bytes, the
// other classifies the op for handler dispatch) that sharing the
// table would entangle them.
func posixForCMakeEGenrule(args string) (string, bool) {
	op, rest := splitFirstShellToken(args)
	switch op {
	case "make_directory":
		return "mkdir -p " + rest, true
	case "create_symlink":
		return "ln -sfn " + rest, true
	case "copy", "copy_if_different":
		return "cp " + rest, true
	case "copy_directory", "copy_directory_if_different":
		return "cp -r " + rest, true
	case "remove":
		return "rm -f " + rest, true
	case "remove_directory":
		return "rm -rf " + rest, true
	case "rename":
		return "mv " + rest, true
	case "touch":
		return "touch " + rest, true
	case "true":
		return "true", true
	case "echo":
		return "echo " + rest, true
	}
	return "", false
}

// splitFirstShellToken splits s into (firstToken, rest) on the
// first whitespace run. Returns (s, "") when s has no whitespace.
func splitFirstShellToken(s string) (string, string) {
	for i := 0; i < len(s); i++ {
		if s[i] == ' ' || s[i] == '\t' {
			j := i
			for j < len(s) && (s[j] == ' ' || s[j] == '\t') {
				j++
			}
			return s[:i], s[j:]
		}
	}
	return s, ""
}
