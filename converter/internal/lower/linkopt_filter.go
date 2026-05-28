package lower

import "strings"

// isCompileOnlyLinkFlag reports whether a token from cmake's
// Target.Link.CommandFragments role="flags" has no link-time
// semantics and should be dropped from Bazel linkopts.
//
// cmake's File API serialises CMAKE_<LANG>_FLAGS_<CONFIG> into
// both the per-CompileGroup CompileCommandFragments AND the
// per-target Link.CommandFragments (cmake's link driver invokes
// the same compiler wrapper with the same flag bundle). That's
// fine in cmake — gcc/clang silently ignore irrelevant flags at
// either stage. In Bazel-emitted BUILD files it produces noisy,
// confusing linkopts:
//
//	linkopts = ["-DNDEBUG", "-O3"],
//
// `-DNDEBUG` has zero link-time meaning; `-O3` is debatable
// (LTO-meaningful with `-flto`) but the survey-observed pattern
// always pairs it with the matching copts entry, so dropping the
// linkopts copy keeps fidelity intact while making the BUILD
// match Bazel idiom.
//
// Conservative filter: drop only flags that are unambiguously
// preprocessor / source-language directives:
//
//   - `-D<MACRO>[=<value>]` / `-U<MACRO>` — preprocessor defines /
//     undefines.
//   - `-include <file>` / `-imacros <file>` and the joined forms.
//   - `-std=*` — language standard.
//   - `-I<dir>` / `-iquote<dir>` / `-isystem<dir>` / `-idirafter
//     <dir>` — include search paths.
//   - `-pedantic` / `-W<warning>` (when not `-Wl,...`) — diagnostic
//     controls that affect compile only.
//
// Flags with dual compile/link semantics (`-O*`, `-g*`, `-flto*`,
// `-fno-rtti`, `-pthread`, `-m<arch>`, `-fsanitize=*`) pass
// through unchanged — link-time codegen / ABI / sanitizer-runtime
// flags must reach the linker.
func isCompileOnlyLinkFlag(tok string) bool {
	if tok == "" {
		return false
	}
	switch {
	case strings.HasPrefix(tok, "-D"), strings.HasPrefix(tok, "-U"):
		return true
	case tok == "-include" || tok == "-imacros":
		return true
	case strings.HasPrefix(tok, "-include="), strings.HasPrefix(tok, "-imacros="):
		return true
	case strings.HasPrefix(tok, "-std="):
		return true
	case strings.HasPrefix(tok, "-I"),
		strings.HasPrefix(tok, "-iquote"),
		strings.HasPrefix(tok, "-isystem"),
		strings.HasPrefix(tok, "-idirafter"):
		return true
	case tok == "-pedantic" || tok == "-pedantic-errors":
		return true
	case strings.HasPrefix(tok, "-W") && !strings.HasPrefix(tok, "-Wl,"):
		// Warning controls: -Wall / -Wextra / -Wunused / -Werror etc.
		// `-Wl,...` is the explicit linker-flag prefix and stays.
		return true
	}
	return false
}
