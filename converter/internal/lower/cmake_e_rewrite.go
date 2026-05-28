package lower

import "strings"

// rewriteCmakeECommand rewrites `cmake -E <op> <args...>` shell
// invocations into POSIX-portable shell equivalents. cmake's
// `-E` mode is a builtin command runner (create_symlink,
// make_directory, copy, etc.) that requires cmake to be on PATH
// at action time. Bazel sandboxes commonly have ln / mkdir /
// cp / rm available but not cmake — the rewrite makes the
// genrule cmd Bazel-portable without depending on the host
// having cmake staged as a Bazel tool.
//
// Operations covered (each maps to a single POSIX equivalent;
// other `-E` forms pass through unchanged):
//
//   - create_symlink <target> <link>       → ln -sf <target> <link>
//   - make_directory <dir>...               → mkdir -p <dir>...
//   - copy <src>... <dst>                   → cp <src>... <dst>
//   - copy_directory <src> <dst>            → cp -r <src> <dst>
//   - copy_directory_if_different <s> <d>   → cp -r <s> <d>
//   - copy_if_different <src> <dst>         → cp <src> <dst>
//   - remove_directory <dir>...             → rm -rf <dir>...
//   - remove [-f] <file>...                 → rm [-f] <file>...
//   - rename <old> <new>                    → mv <old> <new>
//   - touch <file>...                       → touch <file>...
//   - true                                  → true
//   - echo <args...>                        → echo <args...>
//
// Multi-command chains (e.g. `cmake -E remove_directory X &&
// cmake -E copy_directory Y X`) are rewritten per-invocation;
// the chain separators stay intact.
//
// Token splitting is by whitespace only — single- and
// double-quoted arguments aren't preserved (no codegen survey
// case observed has quoted `-E` args). Should one appear, the
// helper conservatively skips the unknown shape and the cmake
// driver invocation passes through.
func rewriteCmakeECommand(cmd string) string {
	if !strings.Contains(cmd, "cmake -E ") &&
		!strings.Contains(cmd, "cmake.exe -E ") {
		return cmd
	}
	// Walk the cmd splitting on chain separators (` && `,
	// ` || `, ` ; `) so each sub-cmd gets normalized
	// independently. Preserve the separators in the output.
	var b strings.Builder
	rest := cmd
	for {
		// Find next chain separator.
		minIdx := len(rest)
		sepLen := 0
		for _, sep := range []string{" && ", " || ", " ; "} {
			if idx := strings.Index(rest, sep); idx >= 0 && idx < minIdx {
				minIdx = idx
				sepLen = len(sep)
			}
		}
		sub := rest[:minIdx]
		b.WriteString(rewriteCmakeESingle(sub))
		if sepLen == 0 {
			break
		}
		b.WriteString(rest[minIdx : minIdx+sepLen])
		rest = rest[minIdx+sepLen:]
	}
	return b.String()
}

// rewriteCmakeESingle handles one sub-command (no chain
// separators). Falls through unchanged when the sub-cmd isn't
// a `cmake -E <op>` shape this helper recognises.
func rewriteCmakeESingle(sub string) string {
	tokens := strings.Fields(sub)
	if len(tokens) < 3 {
		return sub
	}
	// First token must be `cmake` (basename match) and second
	// must be `-E`.
	first := tokens[0]
	first = first[strings.LastIndex(first, "/")+1:]
	if first != "cmake" && first != "cmake.exe" {
		return sub
	}
	if tokens[1] != "-E" {
		return sub
	}
	op := tokens[2]
	args := tokens[3:]
	switch op {
	case "create_symlink":
		if len(args) != 2 {
			return sub
		}
		return "ln -sf " + args[0] + " " + args[1]
	case "make_directory":
		if len(args) == 0 {
			return sub
		}
		return "mkdir -p " + strings.Join(args, " ")
	case "copy", "copy_if_different":
		if len(args) < 2 {
			return sub
		}
		return "cp " + strings.Join(args, " ")
	case "copy_directory", "copy_directory_if_different":
		if len(args) != 2 {
			return sub
		}
		return "cp -r " + args[0] + " " + args[1]
	case "remove_directory":
		if len(args) == 0 {
			return sub
		}
		return "rm -rf " + strings.Join(args, " ")
	case "remove":
		if len(args) == 0 {
			return sub
		}
		return "rm -f " + strings.Join(args, " ")
	case "rename":
		if len(args) != 2 {
			return sub
		}
		return "mv " + args[0] + " " + args[1]
	case "touch":
		if len(args) == 0 {
			return sub
		}
		return "touch " + strings.Join(args, " ")
	case "true":
		return "true"
	case "echo":
		return "echo " + strings.Join(args, " ")
	}
	return sub
}
