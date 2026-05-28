package lower

import "strings"

// isCompileOnlyLinkFlag reports whether a token from cmake's
// `linkLibraries` / "flags"-role command fragment is a
// compile-only flag that should not appear in Bazel's
// `linkopts`. cmake folds CMAKE_C_FLAGS / CMAKE_CXX_FLAGS into
// the link command because the compiler driver (gcc / clang) is
// also the linker driver — the compile-only bits ride along
// silently. Bazel separates compile and link cleanly: copts go
// to the compile action, linkopts go to the link action, and
// compile-only flags in linkopts are at best wasted bytes, at
// worst trigger an unrecognized-option warning from the linker.
//
// Filtered families:
//
//   - `-W*` (every warning flag — `-Wall`, `-Wextra`, `-Wno-*`,
//     `-Werror`, `-Werror=<diag>`). Warnings are compile-time.
//   - `-D<NAME>[=<VAL>]` preprocessor defines. Belong in
//     `defines` / `local_defines`, not the link line.
//   - `-I<dir>` / `-isystem <dir>` include directories. Belong in
//     `includes` / `copts`.
//   - `-std=<lang>` C / C++ standard selection. Affects parsing,
//     not linking. The standard is a copts concern.
//   - `-pedantic` / `-pedantic-errors` — diagnostic strictness;
//     compile-time only.
//
// NOT filtered (still belong in linkopts):
//
//   - `-flto` family — these DO affect the link step (LTO codegen
//     runs at link time).
//   - `-fuse-ld=*` — selects the linker.
//   - `-Wl,*` — explicit linker driver passthrough.
//   - `-l<lib>` / `-L<dir>` / `-rpath` — link inputs / search.
//   - `-shared` / `-static` / `-pie` / `-no-pie` / `-rdynamic`
//     — link mode selectors.
//
// The function operates on a single already-tokenised flag (no
// whitespace splitting); the caller is responsible for argv
// tokenisation per cmake's File API
// `commandFragments[].fragment` shape.
func isCompileOnlyLinkFlag(tok string) bool {
	if tok == "" {
		return false
	}
	// Warning flags — every `-W` form is compile-time. Includes
	// `-Wall`, `-Wextra`, `-Wno-*`, `-Werror`, `-Werror=<diag>`,
	// `-Wp,<assembler-pp>` (note: `-Wl,<linker>` IS link, so
	// exclude that).
	if strings.HasPrefix(tok, "-W") &&
		!strings.HasPrefix(tok, "-Wl,") &&
		tok != "-W" {
		return true
	}
	// Preprocessor defines.
	if strings.HasPrefix(tok, "-D") && len(tok) > 2 {
		return true
	}
	// Include dirs — both joined-form and isystem.
	if strings.HasPrefix(tok, "-I") && len(tok) > 2 {
		return true
	}
	if tok == "-isystem" {
		// Standalone -isystem with a following arg; the arg
		// passes through. Filter only the flag itself. Edge case:
		// the consumer may then have a dangling include-dir arg.
		// In practice cmake emits joined `-isystem<dir>` form on
		// modern toolchains; the split form is rare enough that
		// minor stranding is acceptable.
		return true
	}
	if strings.HasPrefix(tok, "-isystem") && len(tok) > len("-isystem") {
		return true
	}
	// Language standard.
	if strings.HasPrefix(tok, "-std=") {
		return true
	}
	// Strict-diagnostic switches.
	if tok == "-pedantic" || tok == "-pedantic-errors" {
		return true
	}
	// Conservative -f filter: only specific compile-only forms.
	// `-flto` and family stay (they DO affect linking).
	switch tok {
	case "-fno-semantic-interposition",
		"-fno-lifetime-dse",
		"-fstrict-aliasing",
		"-fno-strict-aliasing",
		"-fwrapv",
		"-fno-wrapv",
		"-fno-omit-frame-pointer",
		"-fomit-frame-pointer",
		"-fno-rtti",
		"-frtti",
		"-fno-exceptions",
		"-fexceptions",
		"-fno-common",
		"-fcommon",
		"-fstack-protector",
		"-fstack-protector-strong",
		"-fstack-protector-all",
		"-fno-stack-protector":
		return true
	}
	// `-f<dialect>` family — file/macro/debug prefix maps stripped
	// elsewhere (#268). Visibility (`-fvisibility=hidden`) is
	// compile-time but lifted to features by the toolchain-feature
	// pass.
	if strings.HasPrefix(tok, "-fvisibility=") ||
		strings.HasPrefix(tok, "-fvisibility-inlines-") {
		return true
	}
	// `-fdiagnostics-*` family — diagnostic output formatting
	// (color, format selection, message-line-length, etc.).
	// Affects how the compiler prints messages; not link-time.
	if strings.HasPrefix(tok, "-fdiagnostics-") {
		return true
	}
	// `-ffunction-sections` / `-fdata-sections` — compile-time
	// flags that emit each function / data symbol into its own
	// section. The link-time partner is `-Wl,--gc-sections`
	// (passed through unchanged), but the compile-time
	// preconditions don't belong on the link command.
	if tok == "-ffunction-sections" || tok == "-fdata-sections" {
		return true
	}
	return false
}
