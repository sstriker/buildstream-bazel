// Package ir is the in-memory representation produced by lower/ and consumed
// by emit/. It is intentionally a flat, plain-data shape: no methods, no I/O,
// no policy.
//
// One Package per CMake source root. One Target per CMake build target
// (cc_library / cc_binary / cc_import equivalent). Genrules from
// add_custom_command (recovered from build.ninja in M2) are also Targets, with
// Kind = KindGenrule.
package ir

import "time"

// Kind is the Bazel rule kind a Target lowers to.
type Kind int

const (
	KindUnknown Kind = iota
	KindCCLibrary
	KindCCBinary
	KindCCImport
	KindCCInterface
	KindGenrule
	KindCCTest
	KindShBinary
)

func (k Kind) String() string {
	switch k {
	case KindCCLibrary:
		return "cc_library"
	case KindCCBinary:
		return "cc_binary"
	case KindCCImport:
		return "cc_import"
	case KindCCInterface:
		return "cc_library" // header-only: cc_library with hdrs only
	case KindGenrule:
		return "genrule"
	case KindCCTest:
		return "cc_test"
	case KindShBinary:
		return "sh_binary"
	}
	return "unknown"
}

// Package is the BUILD.bazel-equivalent for one CMake source root.
type Package struct {
	// Name is the CMake project() name.
	Name string

	// SourceRoot is the absolute path the converter ran cmake against. Stored
	// for reference; emitters must not embed it in output.
	SourceRoot string

	// Targets is the per-target rule list. Stable order: lowering enumerates
	// codemodel targets in their declared order.
	Targets []Target
}

// Target is one rule in the emitted BUILD.bazel.
//
// All path fields are package-relative (rooted at Package.SourceRoot). All
// label fields are full Bazel labels (e.g. ":foo", "@glibc//:c"). String
// slices that contribute to BUILD.bazel attributes are sorted by the emitter
// for deterministic output; lowerers are free to leave them in any order.
type Target struct {
	Name string
	Kind Kind

	// Srcs are compilation inputs (.c / .cc / .cpp / .S / etc.).
	Srcs []string

	// Hdrs are exported headers reachable via Includes/StripIncludePrefix.
	Hdrs []string

	// Includes corresponds to the BUILD attribute of the same name: each
	// entry is a directory (package-relative) added to the include search
	// path of dependents.
	Includes []string

	// Copts, Defines, Linkopts pass through to the cc_* rule of the same name.
	Copts    []string
	Defines  []string
	LinkOpts []string

	// Deps are Bazel labels to other targets.
	Deps []string

	// Visibility defaults to package-private when empty; the emitter writes
	// the explicit slice if set.
	Visibility []string

	// Linkstatic / Alwayslink only meaningful for KindCCLibrary.
	Linkstatic bool
	Alwayslink bool

	// InstallDest is the relative path under the install prefix where the
	// CMake install(TARGETS) rule places this target's artifact (e.g. "lib"
	// for STATIC_LIBRARY). Used by emit/cmakecfg/ to populate
	// IMPORTED_LOCATION in the synthesized <Pkg>Targets-Release.cmake.
	// Empty if the target has no install rule.
	InstallDest string

	// ArtifactName is the on-disk file name produced by the build (e.g.
	// "libhello.a", "calc"). Drives IMPORTED_LOCATION_<CONFIG> in the
	// synthesized cmake-config bundle.
	ArtifactName string

	// LinkLanguage feeds IMPORTED_LINK_INTERFACE_LANGUAGES_<CONFIG> in the
	// per-config bundle file. Single language per target in M1.
	LinkLanguage string

	// Tags maps to Bazel's tags attribute. Stable taxonomy is documented
	// in docs/codegen-tags.md. Sorted by the emitter for deterministic
	// output.
	Tags []string

	// cc_import-specific fields. Populated only when Kind ==
	// KindCCImport. cc_import is the canonical Bazel rule for
	// pre-built archives / shared libraries (and the
	// kind:cmake round-2 fallback's per-target stub shape):
	// static_library = single file label of the .a archive,
	// shared_library = single file label of the .so / .dylib /
	// .dll. Both are mutually exclusive in cmake's
	// STATIC/SHARED/MODULE target-type sense, but cc_import
	// itself accepts both attributes simultaneously (some
	// libraries ship both forms); the IR treats them as
	// independent strings so the lowering layer can carry
	// either or both.

	// StaticLibrary, when non-empty for KindCCImport, is the
	// package-relative path (or full label) of the static
	// archive — the .a file the cc_import wraps. Empty when
	// the imported library has no static form.
	StaticLibrary string

	// SharedLibrary, when non-empty for KindCCImport, is the
	// package-relative path (or full label) of the shared
	// object / dynamic library. Empty when the imported
	// library has no shared form.
	SharedLibrary string

	// Genrule-specific fields. Populated only when Kind == KindGenrule.

	// GenruleCmd is the shell command to run, with $(SRCS), $(OUTS), etc.
	// in Bazel's locations() form (or the literal command if no in-Bazel
	// substitutions are needed).
	GenruleCmd string

	// GenruleOuts are package-relative output paths the genrule produces.
	GenruleOuts []string

	// GenruleTools are full Bazel labels added to the genrule's
	// `tools` attribute (e.g. "//tools:cmake-configure-file").
	// Used by configure_file recovery to invoke the runtime
	// substitution tool with the .h.in template as a real srcs
	// input — see lower/configure_file.go.
	GenruleTools []string

	// cc_test-specific fields. Populated only when Kind == KindCCTest;
	// recovered from set_tests_properties() in CTestTestfile.cmake.

	// TestArgs are the arguments cmake's add_test(... COMMAND <bin> <args>)
	// recorded; map directly onto cc_test's args attribute.
	TestArgs []string

	// TestTimeout maps to set_tests_properties TIMEOUT. Zero leaves the
	// cc_test timeout attribute unset (default Bazel timeout applies).
	TestTimeout time.Duration

	// TestEnv are "K=V" entries from set_tests_properties ENVIRONMENT.
	// Rendered as cc_test's env dict.
	TestEnv []string

	// TestData are package-relative file paths from
	// set_tests_properties REQUIRED_FILES; map onto cc_test's data
	// attribute.
	TestData []string
}
