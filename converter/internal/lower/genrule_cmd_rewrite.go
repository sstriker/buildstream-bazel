package lower

import (
	"path/filepath"
	"strings"
)

// rewriteGenruleCmd strips convert-time-only constructs from a
// recovered ninja CUSTOM_COMMAND cmd before it lands as the
// genrule's `cmd` attribute. Without this normalisation the
// rendered genrule leaks the convert-host filesystem layout
// (`/tmp/<build>/...` paths, `cd <abs-subdir> &&` preambles)
// into the BUILD.bazel, where Bazel's hermetic sandbox can't
// resolve any of them at action time.
//
// Rewrites applied:
//
//   - `cd <abs-under-buildDir> && ` prefix: cmake's Ninja
//     generator prepends this so the command runs in a per-target
//     build subdirectory at convert time. Bazel runs the genrule
//     in $(GENDIR) which doesn't exist at convert time, and the
//     cd path doesn't exist at action time. Drop the prefix
//     entirely when the target dir resolves under buildDir or
//     cmakeSrc.
//   - Verbatim `<buildDir>/`-rooted path references in the cmd
//     body: re-anchor to genrule-output-relative by stripping the
//     convert-time prefix. For files the genrule itself produces
//     this is correct (Bazel puts genrule outputs under $(GENDIR)
//     at the matching relative path).
//   - Verbatim `<cmakeSrc>/`-rooted path references: re-anchor
//     to workspace-relative.
//
// All rewrites are conservative — paths only re-anchor when they
// start with the canonical anchor prefix + "/", so partial-match
// hazards (e.g. `<buildDir>` vs `<buildDir>_other`) are avoided.
//
// Tokens without an embedded anchor pass through unchanged.
//
// Additional normalisations layered on top of the anchor pass:
//
//   - `cmake -E <op> ...` invocations rewrite to their POSIX
//     equivalents when the op has a clear shell analogue
//     (`make_directory` → `mkdir -p`, `create_symlink` → `ln -sfn`,
//     `copy` → `cp`, etc.). Keeps the cmd portable in Bazel's
//     bash-runs-the-genrule shell without needing cmake at action
//     time.
//   - Host-tool prefixes on the leading command token (`/usr/bin/`,
//     `/usr/local/bin/`, `/usr/sbin/`) strip to the bare tool name
//     so the rendered cmd resolves through `$PATH` instead of
//     hardcoding the convert-host's filesystem.
func rewriteGenruleCmd(cmd, cmakeSrc, buildDir string) string {
	if cmd == "" {
		return cmd
	}
	// Strip `cd <abs> && ` prefix when <abs> is under buildDir or
	// cmakeSrc. Bazel runs the genrule in its sandbox-rooted
	// $(GENDIR); the cd is cmake-internal.
	if strings.HasPrefix(cmd, "cd ") {
		if end := strings.Index(cmd, " && "); end > 0 {
			target := strings.TrimSpace(strings.TrimPrefix(cmd[:end], "cd "))
			if filepath.IsAbs(target) {
				drop := false
				if buildDir != "" {
					if _, ok := relativeIfInside(buildDir, target); ok {
						drop = true
					}
				}
				if !drop && cmakeSrc != "" {
					if _, ok := relativeIfInside(cmakeSrc, target); ok {
						drop = true
					}
				}
				if drop {
					cmd = cmd[end+4:]
				}
			}
		}
	}
	// Strip cmakeSrc and buildDir prefixes from the cmd body. The
	// trailing slash on the anchor ensures partial-match safety
	// against e.g. `<buildDir>_other`.
	for _, anchor := range []string{cmakeSrc, buildDir} {
		if anchor == "" {
			continue
		}
		cmd = strings.ReplaceAll(cmd, anchor+"/", "")
	}
	// Also rewrite bare-anchor occurrences (no trailing slash) when
	// the anchor sits at a token boundary — covers cmake-Ninja
	// bookkeeping shapes that don't fit the trailing-slash pass:
	//
	//     /usr/bin/cmake -DCMAKE_BINARY_DIR=<buildDir> -P ...
	//     /usr/bin/cmake --regenerate-during-build -S<cmakeSrc> -B<buildDir>
	//
	// Rewriting to "." points the cmake invocation at the genrule's
	// sandbox cwd — the closest workspace-relative analogue. These
	// cmds typically belong to cmake-internal regen / install edges
	// that a separate filter discards entirely; even if a few slip
	// through, the rendered cmd shouldn't leak the convert-host
	// filesystem layout.
	for _, anchor := range []string{cmakeSrc, buildDir} {
		if anchor == "" {
			continue
		}
		cmd = replaceBareAnchorAtBoundary(cmd, anchor, ".")
	}
	// Rewrite `cmake -E <op> ...` to POSIX equivalents — keeps
	// the cmd portable in Bazel's sandbox shell without needing
	// cmake at action time. Runs after the anchor passes so the
	// args themselves are already workspace-relative.
	cmd = rewriteCMakeEInvocation(cmd)
	// Strip host-tool prefixes from the leading command token
	// (`/usr/bin/`, `/usr/local/bin/`, `/usr/sbin/`) so the cmd
	// resolves through $PATH at action time. Avoids leaking the
	// convert-host's filesystem layout when the tool lives at a
	// distribution-specific path the action's environment may
	// not match.
	cmd = stripHostBinPrefix(cmd)
	return cmd
}

// rewriteCMakeEInvocation rewrites every `cmake -E <op>` (or
// `<host-bin>/cmake -E <op>`) prefix in cmd to the POSIX
// equivalent. The cmake-E ops are stable across cmake versions;
// rewriting at convert time means the rendered genrule cmd runs
// under Bazel's bash without needing cmake on the action's PATH.
//
// Supported ops + their POSIX equivalents:
//
//	make_directory <dirs...>     → mkdir -p <dirs...>
//	create_symlink T L           → ln -sfn T L
//	copy <src> <dst>             → cp <src> <dst>
//	copy_if_different <src> <dst>→ cp <src> <dst>
//	copy_directory <src> <dst>   → cp -r <src> <dst>
//	remove <files...>            → rm -f <files...>
//	remove_directory <dirs...>   → rm -rf <dirs...>
//	rename <src> <dst>           → mv <src> <dst>
//	touch <files...>             → touch <files...>
//	true                         → true
//	echo <args...>               → echo <args...>
//
// Ops without a clear POSIX equivalent (e.g. `cmake -E env` with
// shell-quoted argv, `cmake -E time`, `cmake -E compare_files`
// with its specific exit-code semantics, `cmake -E chdir`) pass
// through unchanged — operators see the original cmd and can
// stage a cmake runner if needed.
func rewriteCMakeEInvocation(cmd string) string {
	// Find every `cmake -E ` occurrence (with optional preceding
	// host-bin prefix) and rewrite it. Loop because the cmd can
	// chain (e.g. `cmake -E ... && cmake -E ...`).
	for {
		idx := strings.Index(cmd, "cmake -E ")
		if idx < 0 {
			break
		}
		// Find the start of the cmake token (including any
		// preceding `/abs/path/`).
		tokStart := idx
		for tokStart > 0 && cmd[tokStart-1] != ' ' && cmd[tokStart-1] != '\t' && cmd[tokStart-1] != ';' && cmd[tokStart-1] != '&' {
			tokStart--
		}
		// Extract the args after `cmake -E `.
		argsStart := idx + len("cmake -E ")
		// Find end of this cmake -E invocation: stop at `&&`,
		// `||`, `;`, or end-of-string.
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
		// Strip trailing whitespace from the slice.
		end := argsEnd
		for end > argsStart && (cmd[end-1] == ' ' || cmd[end-1] == '\t') {
			end--
		}
		args := cmd[argsStart:end]
		rewritten, ok := posixForCMakeE(args)
		if !ok {
			// Skip this occurrence; replace the `cmake -E ` bytes
			// with a sentinel so the next strings.Index doesn't
			// re-find this one and spin.
			cmd = cmd[:idx] + "@CMAKE_E_SKIP@" + cmd[argsStart:]
			continue
		}
		// Preserve the whitespace between the rewritten args and a
		// trailing shell separator (`&&` / `||` / `;`). The trailing-
		// space strip during `args` slicing dropped it; without
		// re-inserting we'd end up with `mkdir -p a&& next` (a
		// shell-token-fusion bug).
		sep := ""
		if argsEnd < len(cmd) && argsEnd > end {
			sep = " "
		}
		cmd = cmd[:tokStart] + rewritten + sep + cmd[argsEnd:]
	}
	// Undo the skip markers from non-rewritable occurrences.
	cmd = strings.ReplaceAll(cmd, "@CMAKE_E_SKIP@", "cmake -E ")
	return cmd
}

// posixForCMakeE returns the POSIX shell rewrite of a `cmake -E
// <op> <args...>` invocation given the slice that follows `cmake
// -E `. Returns ok=false for ops we don't rewrite.
func posixForCMakeE(args string) (string, bool) {
	// Split off the op name (first token).
	op, rest := splitFirstToken(args)
	switch op {
	case "make_directory":
		return "mkdir -p " + rest, true
	case "create_symlink":
		// cmake's create_symlink takes (target, linkname). POSIX
		// `ln -sfn <target> <linkname>` mirrors that.
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

// splitFirstToken splits s into (firstToken, rest) on the first
// whitespace run. Returns (s, "") when s has no whitespace.
func splitFirstToken(s string) (string, string) {
	for i := 0; i < len(s); i++ {
		if s[i] == ' ' || s[i] == '\t' {
			// Skip the whitespace run.
			j := i
			for j < len(s) && (s[j] == ' ' || s[j] == '\t') {
				j++
			}
			return s[:i], s[j:]
		}
	}
	return s, ""
}

// stripHostBinPrefix removes `/usr/bin/`, `/usr/local/bin/`, and
// `/usr/sbin/` from the leading command token so the cmd resolves
// through $PATH at action time instead of hardcoding the
// convert-host's distribution layout. Only strips when the prefix
// is at the very start OR follows a shell separator (`&&`, `||`,
// `;`); embedded references (e.g. `--toolchain=/usr/bin/gcc`)
// pass through unchanged.
func stripHostBinPrefix(cmd string) string {
	prefixes := []string{"/usr/bin/", "/usr/local/bin/", "/usr/sbin/"}
	// Process every shell-separator boundary.
	out := strings.Builder{}
	i := 0
	for i < len(cmd) {
		// Possible leading-position strip: at start OR after a
		// shell-token boundary.
		stripped := false
		for _, p := range prefixes {
			if strings.HasPrefix(cmd[i:], p) {
				out.WriteString(cmd[i+len(p):][:0]) // no-op
				// Verify the prefix isn't a substring of a longer
				// token: byte before must be a shell boundary or
				// start-of-string.
				if i == 0 || cmd[i-1] == ' ' || cmd[i-1] == '\t' || cmd[i-1] == ';' || cmd[i-1] == '&' || cmd[i-1] == '|' {
					i += len(p)
					stripped = true
					break
				}
			}
		}
		if stripped {
			continue
		}
		out.WriteByte(cmd[i])
		i++
	}
	return out.String()
}

// replaceBareAnchorAtBoundary rewrites every occurrence of
// `anchor` in `s` where the anchor sits at a token boundary — i.e.
// preceded by start-of-string, whitespace, `=`, OR the cmake CLI
// `-S` / `-B` flag prefix (these fuse the path to the flag with no
// separating space), AND followed by end-of-string, whitespace,
// `=`, `&`, or `;`. Occurrences inside a longer token
// (`<anchor>foo`) pass through unchanged — those are caller-handled
// via the strings.ReplaceAll trailing-slash pass.
func replaceBareAnchorAtBoundary(s, anchor, replacement string) string {
	if anchor == "" || !strings.Contains(s, anchor) {
		return s
	}
	var b strings.Builder
	i := 0
	for i < len(s) {
		idx := strings.Index(s[i:], anchor)
		if idx < 0 {
			b.WriteString(s[i:])
			break
		}
		start := i + idx
		end := start + len(anchor)
		// Boundary check: byte before.
		prevOK := start == 0
		if !prevOK {
			c := s[start-1]
			if c == ' ' || c == '\t' || c == '=' {
				prevOK = true
			} else if start >= 2 && s[start-2] == '-' && (s[start-1] == 'S' || s[start-1] == 'B') {
				// cmake CLI: `-S<path>` / `-B<path>` fuses the
				// path to the flag with no separating space.
				prevOK = true
			}
		}
		// Boundary check: byte after.
		nextOK := end == len(s)
		if !nextOK {
			c := s[end]
			if c == ' ' || c == '\t' || c == '=' || c == '&' || c == ';' {
				nextOK = true
			}
		}
		b.WriteString(s[i:start])
		if prevOK && nextOK {
			b.WriteString(replacement)
		} else {
			b.WriteString(anchor)
		}
		i = end
	}
	return b.String()
}
