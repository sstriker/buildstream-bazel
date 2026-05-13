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

	// Visibility, when non-empty, emits as a per-rule
	// `visibility = [...]` attribute on the rendered Bazel
	// rule. The emitter elides the per-rule attribute when
	// Visibility equals the package-level default
	// (`["//visibility:public"]`, emitted as
	// `package(default_visibility=...)` at file head) per
	// gazelle_cc's "package default + per-rule override only"
	// convention; see docs/design/build-output-conventions.md.
	//
	// Empty (zero-value) Visibility resolves to the package-
	// level default `//visibility:public` by virtue of the
	// `package(default_visibility=...)` line at the top of
	// every emitted BUILD. This is the common case across
	// producers — the cmake codemodel-lowering path leaves
	// Visibility unset on most targets and relies on the
	// package default. Producers that want a stricter scope
	// on a specific target populate Visibility explicitly
	// (e.g. `["//visibility:private"]` for internal helpers
	// in lower/execute_process.go, lower/configure_file.go,
	// and lower/genrule.go).
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

	// PerPlatform carries per-platform attribute deltas the
	// per-element multi-platform fold produces. The outer key is
	// the IR attribute name ("srcs", "hdrs", "includes", "copts",
	// "defines", "linkopts", "deps"); the inner key is the Bazel
	// select() arm label (a constraint_value label like
	// "@platforms//os:darwin", or a config_setting label for
	// multi-axis matrices); the value is the delta items that go
	// inside that arm — items the platform observes that the
	// flat baseline doesn't. The flat baseline lives in the
	// regular fields above (Srcs, Copts, ...).
	//
	// Single-platform conversion never populates this field;
	// emit treats nil and empty maps identically — the rendered
	// BUILD.bazel uses the flat-list shape and matches the
	// existing single-platform goldens byte-for-byte. Only the
	// multi-platform fold (lower/elementfold) populates it; only
	// emit/bazel renders select() blocks when it's non-empty.
	//
	// Attribute names match the Bazel attribute spelling so the
	// emitter doesn't have to translate; the lowercase form is
	// the key callers should use ("srcs" not "Srcs").
	PerPlatform map[string]map[string][]string

	// PerPlatformScalar is PerPlatform's sibling for single-string
	// attributes — cc_import.static_library and shared_library
	// today. The outer key is the IR attribute name in lowercase
	// Bazel-attribute spelling ("static_library", "shared_library");
	// the inner key is the select() arm label; the value is the
	// single path string for that arm (no flat baseline; scalars
	// don't compose under "+").
	//
	// Arms are present only for platforms that contribute a non-
	// empty value: the partial-platform cc_import shape — linux
	// supplies static_library only, darwin supplies shared_library
	// only — produces a map with one arm per platform that
	// populated each attr. emit/bazel adds a trailing
	// `"//conditions:default": None` arm at render time so in-
	// matrix platforms that omitted an arm AND out-of-matrix
	// platforms fall through to "attribute unset" rather than
	// hitting a missing-condition analysis error.
	//
	// Populated by elementfold only when the underlying scalar
	// field (StaticLibrary / SharedLibrary) diverges across cells.
	// When every cell agrees, the value lives in the flat scalar
	// field and PerPlatformScalar stays empty so single-platform
	// emission stays byte-identical.
	PerPlatformScalar map[string]map[string]string
}
