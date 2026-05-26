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
	// KindFilegroup carries a list of source files exposed via a
	// Bazel-native `filegroup()` rule. The cmake converter uses
	// it to lower install(FILES ...) / install(DIRECTORY ...)
	// declarations at convert time (Phase 1 task 2 of the
	// generator-parity uplift in ROADMAP.md) — exposing the
	// named files as a labeled filegroup downstream consumers
	// can depend on without pulling install_tree.tar through
	// the round-2 fallback. filegroup is in the global Bazel
	// namespace (no MODULE.bazel deps); for richer attribute
	// support (per-file destination renames via pkg_files +
	// rules_pkg) Phase 1 task 2's full pkg_files emission would
	// slot in alongside.
	KindFilegroup
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
	case KindFilegroup:
		return "filegroup"
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

// Provenance records the originating source location of a Target.
// File is typically a cmake CMakeLists.txt path; Line is 1-based.
// Command is the cmake command name (e.g. "add_library",
// "add_executable") that declared the target. Empty File suppresses
// the emit-side provenance comment.
type Provenance struct {
	File    string
	Line    int
	Command string
}

// IsZero reports whether the provenance is empty (no recorded
// source location). Emit-side helpers use this to skip the
// comment without checking each field.
func (p Provenance) IsZero() bool {
	return p.File == "" && p.Line == 0 && p.Command == ""
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

	// Provenance records where this target was declared in the
	// originating build system's source. Populated by lowerers
	// that have backtrace data — the cmake codemodel's
	// BacktraceGraph gives file/line/command per target; trace-
	// based lowerers populate it best-effort from their own
	// per-call call-site data.
	//
	// Emit-side rendering is gated by emit.Options.EmitProvenance:
	// when on, the emitter writes a leading
	// `# Source: <file>:<line> (<command>)` comment above each
	// rule whose Provenance is non-zero. Operators use the
	// annotation to navigate "why does this Bazel target exist?"
	// without re-running the converter.
	//
	// Zero-value Provenance (File == "") suppresses the comment;
	// the IR stays back-compat for lowerers / fixtures that
	// pre-date this field.
	Provenance Provenance

	// Srcs are compilation inputs (.c / .cc / .cpp / .S / etc.).
	Srcs []string

	// Hdrs are exported headers reachable via Includes/StripIncludePrefix.
	Hdrs []string

	// Includes corresponds to the BUILD attribute of the same name: each
	// entry is a directory (package-relative) added to the include search
	// path of dependents.
	Includes []string

	// IncludePrefix, when non-empty, renders as the
	// `include_prefix` attribute on cc_library / cc_binary /
	// cc_test. Bazel prepends the prefix to every header path
	// a consumer sees via this target's hdrs — e.g.
	// `include_prefix = "myelem"` rewrites
	// `hdrs = ["api.h"]` so a downstream `#include
	// "myelem/api.h"` resolves. Matches gazelle_cc's
	// directive-driven attribute; see
	// docs/design/build-output-conventions.md.
	//
	// NOT emitted on cc_import — stock rules_cc's cc_import
	// doesn't accept these attributes; the canonical fix for
	// the round-2 fallback's bracket-include consumers is
	// wrapping cc_import in a cc_library that uses
	// strip_include_prefix (the wrap doesn't ship yet; see
	// converter/internal/lower/execute_process_fallback.go
	// for the gap comment).
	IncludePrefix string

	// StripIncludePrefix, when non-empty, renders as the
	// `strip_include_prefix` attribute on the same rule
	// kinds (cc_library / cc_binary / cc_test). Bazel strips
	// the prefix from header paths before applying
	// IncludePrefix — typical usage pairs them:
	// `strip_include_prefix = "include"` +
	// `include_prefix = "myelem"` turns
	// `hdrs = ["include/myelem/api.h"]` into a consumer-side
	// `#include "myelem/api.h"`.
	StripIncludePrefix string

	// Copts, Defines, Linkopts pass through to the cc_* rule of the same name.
	Copts    []string
	Defines  []string
	LinkOpts []string

	// Deps are Bazel labels to other targets whose headers are
	// reachable through this target's public hdrs. Maps to
	// `deps = [...]` on the emitted rule. Per gazelle_cc canon,
	// targets reached via `PUBLIC` or `INTERFACE`
	// target_link_libraries() in CMake belong here.
	Deps []string

	// ImplementationDeps are Bazel labels to other targets used
	// only in this target's .cc files (`PRIVATE`
	// target_link_libraries() in CMake). Maps to
	// `implementation_deps = [...]` on the emitted rule —
	// headers from these deps are NOT exposed transitively to
	// consumers of this library, giving stricter header hygiene
	// than a single Deps list.
	//
	// Signal availability: CMake codemodel-v2 (the converter's
	// primary input today) does NOT carry per-dependency
	// PUBLIC/PRIVATE scope — it exposes only a flat
	// Target.Dependencies list and the rendered link
	// commandFragments. The shadow trace, however, DOES carry
	// the keyword: cmake records each target_link_libraries
	// call with its PUBLIC/PRIVATE/INTERFACE arm, and
	// internal/shadow/trace_commands.go decodes the arms into
	// per-target lib→keyword maps. The cmake-codemodel
	// lowering consults that map and routes PRIVATE deps to
	// ImplementationDeps. Codemodel-only paths (no
	// --trace-format=json-v1 run available) leave the field
	// unset and fold every dep into Deps — strictly safe,
	// matches pre-Phase-4 behaviour byte-for-byte. Meson
	// introspection and pyproject paths likewise have no scope
	// signal and leave the field unset. Documented in
	// docs/design/build-output-conventions.md.
	ImplementationDeps []string

	// Visibility carries the per-target Bazel visibility list
	// as pure data. The IR stays policy-free: what "empty"
	// means and how non-empty lists render is the consuming
	// emitter's call. Common cases the IR exposes:
	//   - cmake codemodel-lowering (lower/lower.go) commonly
	//     leaves Visibility unset on most targets.
	//   - lower/execute_process.go, lower/configure_file.go,
	//     and lower/genrule.go populate it explicitly with
	//     stricter scopes like `["//visibility:private"]` for
	//     internal helpers.
	//   - meson and trace producers populate it explicitly
	//     with `["//visibility:public"]`.
	//
	// Emitter-side rendering policy lives in the emitter's own
	// docs, not here. `converter/emit/bazel` (the shared cc
	// emitter, used by convert-element-cmake / convert-element-meson
	// / fold-element / convert-element-trace) writes a file-
	// head `package(default_visibility = ["//visibility:public"])`
	// and emits a per-rule `visibility = [...]` line only when
	// Visibility differs from that default; empty Visibility
	// takes the package default. Other emitters
	// (`converter/cmd/convert-element-pyproject/emit.go`,
	// `cmd/write-a/handler_*.go`'s direct BUILD writers) make
	// their own visibility-rendering choices and should
	// document them next to their emit code. See
	// docs/design/build-output-conventions.md for the cc
	// emitter's convention and its gazelle_cc lineage.
	Visibility []string

	// Linkstatic / Alwayslink only meaningful for KindCCLibrary.
	Linkstatic bool
	Alwayslink bool

	// Features routes to Bazel's `features = [...]` attribute on
	// cc_library / cc_binary / cc_test. Each entry is a feature
	// name a cc_toolchain feature() declares (e.g. "lto",
	// "asan"). Empty / nil skips the attribute. Prefix a feature
	// name with "-" to negate the toolchain default (e.g. "-pic"
	// to force off).
	//
	// Used by cmake `INTERPROCEDURAL_OPTIMIZATION` lifting
	// (TargetArchive.LTO / TargetLink.LTO → ["lto"]) and the
	// per-rule sanitizer-as-feature shape when operators wire
	// the sanitizer features through Phase 5's
	// --out-sanitizer-features. Deterministic emit: the emitter
	// sorts the list.
	Features []string

	// Data routes to Bazel's `data = [...]` attribute — build-
	// order dependencies that don't propagate compile/link
	// facts. Used for cmake's `add_dependencies(target dep)`
	// call shape: dep needs to be built before target, but
	// target doesn't link against dep's library or include its
	// headers. Bazel's data attribute is the canonical home for
	// build-order-only edges.
	//
	// Distinct from Deps (target_link_libraries-derived, carries
	// headers + link) and ImplementationDeps (PRIVATE
	// target_link_libraries). Empty / nil skips the attribute.
	Data []string

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
