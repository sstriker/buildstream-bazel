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
	// KindPickFile renders as the rules_buildstream_bazel
	// `pick_file(name=, src=, path=)` rule: it projects a single
	// file out of a pipeline_install install-root TreeArtifact into
	// a plain File label. The round-2 execute-process fallback emits
	// one per artefact / header the cc_import / sh_binary stubs
	// reference, replacing the old _install_tree_extract tar-untar
	// genrule (no per-consumer re-materialization of the whole tree).
	KindPickFile
	// KindFilegroup carries a list of source files exposed via a
	// Bazel-native `filegroup()` rule. The cmake converter no
	// longer routes install(FILES) / install(DIRECTORY) here —
	// those lower to KindPkgFiles (see below). KindFilegroup
	// remains for the install(EXPORT) cmake_config_bundle shape
	// (lowerExportInstallers) and any other Bazel-native file
	// grouping. filegroup is in the global Bazel namespace (no
	// MODULE.bazel deps).
	KindFilegroup
	// KindPkgFiles renders as rules_pkg's
	// `pkg_files(name=, srcs=[...], prefix="<dest>")` rule. The
	// cmake converter lowers install(FILES ...) /
	// install(DIRECTORY ...) declarations here at convert time
	// (Phase 1 slice 1b of the generator-parity uplift in
	// ROADMAP.md): unlike a bare filegroup, pkg_files carries the
	// install **destination** as the `prefix` attribute, so the
	// converted shape is a real declarative packaging mapping that
	// downstream pkg_tar / pkg_install rules can consume to
	// reconstruct the install layout — instead of an opaque
	// filegroup that loses the destination.
	//
	// Srcs are the installed source-root-relative files (Type=="file")
	// or the files under the installed directory tree
	// (Type=="directory"); PkgPrefix is the install destination
	// (e.g. "lib", "include", "share/foo"). pkg_files comes from
	// @rules_pkg//pkg:mappings.bzl, so a BUILD that emits one needs
	// rules_pkg on the consuming project's MODULE.bazel — the
	// emitter writes the load and write-a adds the bazel_dep (see
	// emit/bazel/emit.go's emitPkgFilesLoad + cmd/write-a's
	// moduleBazelB). Per-file destination renames (cmake's
	// install(FILES ... RENAME ...)) are NOT modeled — the File
	// API codemodel's DirectoryInstaller doesn't expose the
	// rename target cleanly; documented as a follow-up in
	// ROADMAP.md.
	KindPkgFiles
	// KindAlias renders as Bazel-native `alias(name=, actual=)`.
	// Lifts cmake's `add_library(<alias> ALIAS <target>)` shape:
	// downstream Bazel code referencing the alias name resolves
	// to the underlying target. cmake resolves aliases at
	// configure time so the codemodel's TargetDependency.Id
	// points at the actual target; the alias rule exists so
	// operator-written Bazel code (cross-package consumers,
	// scripts, .bzl files) can use either name.
	//
	// Only emitted for non-namespaced aliases — `Pkg::Target`
	// shapes (find_package's IMPORTED-target surface) ride
	// through the imports manifest path instead. Bazel rejects
	// `::` in target names, so the namespaced form has no
	// usable alias label anyway.
	KindAlias
	// KindWriteFile renders as bazel_skylib's
	// `write_file(name=, out=, content=[...], newline="unix")` rule.
	// The cmake converter uses it for the file(GENERATE) "bake" tier
	// — a fully-resolved (or COPYONLY) body whose exact bytes are
	// known at convert time and need no Bazel-time re-evaluation.
	// Instead of the legacy `echo <base64> | base64 -d > $@` genrule
	// (opaque, unmaintainable), write_file carries the content as a
	// human-readable list of lines: WriteFileContent =
	// strings.Split(body, "\n"), which skylib joins with "\n" — an
	// exact round-trip for any \n-only UTF-8 text (Split/Join are
	// inverses; a trailing "" element reproduces a trailing newline).
	// Bodies that aren't \n-only UTF-8 text (binary, CRLF) stay on
	// the base64 genrule, which is byte-exact regardless. write_file
	// comes from @bazel_skylib//rules:write_file.bzl, so a BUILD that
	// emits one needs bazel_skylib on the consuming project's
	// MODULE.bazel — the emitter writes the load and write-a adds the
	// bazel_dep (mirrors the KindPkgFiles / rules_pkg precedent).
	KindWriteFile
	// KindCMakeConfigureFile renders as the rules_buildstream_bazel
	// `cmake_configure_file(...)` rule: the configure_file /
	// file(GENERATE) **lift tier**. Unlike the bake tier (KindWriteFile),
	// the rendered bytes are NOT known-final at convert time — the
	// template is re-rendered at Bazel build time by
	// //tools:cmake-configure-file against the captured cmake variable
	// namespace, so edits to the template re-render through Bazel
	// without re-running the converter.
	//
	// It replaces the previous genrule-with-base64-in-shell lift shape:
	// the substitution inputs (values / genex_values dicts, genex_context
	// JSON, the CONTENT body) ride as readable Starlark attributes; the
	// rule's impl materializes the JSON sidecars via ctx.actions.write
	// and passes an argv array to the tool via ctx.actions.run — so there
	// is no shell-quoting surface and no base64 anywhere. Attributes live
	// on Target.CMakeConfigureFile. Comes from
	// @rules_buildstream_bazel//rules:cmake_configure_file.bzl (the
	// emitter writes the load; the rules module is already a dep wherever
	// the lift's BUILDs land, same precedent as pick_file).
	KindCMakeConfigureFile
	// KindBoolFlag renders as bazel_skylib's
	// `bool_flag(name=, build_setting_default=)` (from
	// @bazel_skylib//rules:common_settings.bzl). The cmake converter
	// emits one when it lifts a configure-time feature probe
	// (execute_process / check_* writing a HAVE_X-style variable) into
	// an explicit, operator-overridable Bazel build setting instead of
	// refusing the probe or silently baking its result: a probe is a
	// deferred declaration, and the faithful Bazel shape is a declared
	// flag the operator can flip (`--//pkg:have_x=False`). Default is
	// BoolFlagDefault (the value cmake probed, when captured).
	KindBoolFlag
	// KindConfigSetting renders as a `config_setting(name=,
	// flag_values={<ConfigSettingFlag>: <ConfigSettingValue>})` — the
	// select()-able condition paired with a KindBoolFlag so consumers
	// of the lifted probe can `select()` on whether the feature is
	// enabled. Built-in rule, no load needed.
	KindConfigSetting
	// KindCCEmbed renders as the rules_buildstream_bazel
	// `cc_embed(...)` rule: the Bazel-native lowering of the "embed a
	// file as a C array" cmake -P codegen idiom (VTK's vtkEncodeString).
	// Instead of running cmake at build time (the --cmake-script-runner
	// bridge) or freezing bytes at convert time (--cmake-script-bake),
	// the rule runs the hermetic //tools:cc-embed tool to turn the input
	// file into a .h + .cxx exposing its bytes as a named symbol — so the
	// converted project needs neither cmake nor the converter at build
	// time. Attributes live on Target.CCEmbed. Comes from
	// @rules_buildstream_bazel//rules:cc_embed.bzl (the emitter writes the
	// load, mirroring KindCMakeConfigureFile).
	KindCCEmbed
	// KindCCHash renders as the rules_buildstream_bazel `cc_hash(...)`
	// rule: the Bazel-native lowering of the "hash a file into a generated
	// header" cmake -P codegen idiom (VTK's vtkHashSource.cmake). Instead of
	// running cmake at build time (the --cmake-script-runner bridge) or
	// freezing the digest at convert time (--cmake-script-bake), the rule
	// runs the hermetic //tools:cc-hash tool to write a header that
	// `#define`s the file's digest as a C string — so the converted project
	// needs neither cmake nor the converter at build time, and the digest
	// auto-refreshes when the input changes (the bake's gap). Attributes
	// live on Target.CCHash. Comes from
	// @rules_buildstream_bazel//rules:cc_hash.bzl (the emitter writes the
	// load, mirroring KindCCEmbed).
	KindCCHash
	// KindCudaLibrary renders as rules_cuda's `cuda_library(...)` (from
	// @rules_cuda//cuda:defs.bzl). It carries `.cu` CUDA device sources that
	// a plain cc_library can't compile — Bazel's cc rules dispatch by file
	// extension and have NO nvcc compile action, so a cc_library with `.cu`
	// srcs is unbuildable as emitted. cuda_library drives nvcc via a
	// registered CUDA toolchain, accepting the same core attributes
	// (srcs/hdrs/deps/copts/includes/defines) so the converter reuses the
	// cc_library view/template to render it.
	//
	// Lowered from a CMake CUDA compile group (cg.Language == "CUDA"):
	// the per-language target split (lower.go) tags the CUDA sub-library
	// with this kind, and a whole-target single-CUDA-group library is
	// retagged in place. Needs rules_cuda on the consuming MODULE.bazel
	// plus a registered CUDA toolchain — the build lens injects both via
	// scripts/build-lens/<name>.conf's EXTRA_BAZEL_DEPS (the orchestrated
	// graph would wire them the same way for a kind:cmake CUDA element).
	// There is no canonical gazelle CUDA extension to hand the layout to,
	// so (like the Fortran partition) the converter owns the rule shape.
	KindCudaLibrary
	// KindCudaBinary renders as rules_cuda's `cuda_binary(...)` — the CUDA
	// analogue of cc_binary for an executable whose compiled sources are all
	// `.cu` (cuda-samples' device-code programs). Folds hdrs into srcs like
	// cc_binary (the cuda_binary macro has no `hdrs` attribute either).
	KindCudaBinary
	// KindCudaTest renders as rules_cuda's `cuda_test(...)` — the CUDA
	// analogue of cc_test for a `.cu`-only test executable.
	KindCudaTest
	// KindFortranLibrary renders as rules_buildstream_bazel's
	// `fortran_library(...)` — the converter's self-contained lowering for a
	// cmake Fortran target (a cc_* rule has no Fortran compile action). It
	// compiles `.f`/`.f90`/... via the cc toolchain's own driver (gfortran)
	// and exposes a CcInfo, so a consuming cc_* target deps on it like any
	// library. The Fortran analogue of KindCudaLibrary; see
	// //rules:fortran.bzl and lower's retagFortranTargets.
	KindFortranLibrary
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
	case KindPickFile:
		return "pick_file"
	case KindFilegroup:
		return "filegroup"
	case KindPkgFiles:
		return "pkg_files"
	case KindAlias:
		return "alias"
	case KindWriteFile:
		return "write_file"
	case KindCMakeConfigureFile:
		return "cmake_configure_file"
	case KindCCEmbed:
		return "cc_embed"
	case KindCCHash:
		return "cc_hash"
	case KindCudaLibrary:
		return "cuda_library"
	case KindCudaBinary:
		return "cuda_binary"
	case KindCudaTest:
		return "cuda_test"
	case KindFortranLibrary:
		return "fortran_library"
	case KindBoolFlag:
		return "bool_flag"
	case KindConfigSetting:
		return "config_setting"
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

	// HeaderComments is a list of attribution / informational lines
	// the emitter renders as `# <line>` between the file-head
	// "Generated by …" comment and the first rule. Empty / nil
	// suppresses. Used by Phase 2 / configureLog consumers to
	// surface find_package resolutions at the top of the BUILD
	// — operators see the external-dep inventory at a glance.
	HeaderComments []string

	// SubPackages maps each real (codemodel-derived) target Name to
	// the element-root-relative directory the cmake CMakeLists that
	// declared it lived in ("" = the root package, e.g. "src/util"
	// for a target declared under add_subdirectory(src/util)). It is
	// the out-of-band signal the --split-packages emit transform
	// consumes to mirror the CMakeLists/add_subdirectory layout as a
	// per-directory BUILD.bazel tree ("gazelle model").
	//
	// The `json:"-"` tag is load-bearing: this field MUST NOT
	// serialize into --out-ir-json. The multi-platform fold path
	// (which round-trips IR through JSON) is mutually exclusive with
	// --split-packages, and keeping the field out of the wire shape
	// guarantees the JSON byte-output is unperturbed by the feature.
	//
	// Targets without an entry (install-derived filegroups, cc_import
	// / cmake_config_bundle synthesized by lowerExportInstallers,
	// aliases, genrules, interface libs, per-language sub-libraries)
	// have no declaring CMakeLists dir and resolve to the root
	// package; the split transform treats a missing key as "".
	SubPackages map[string]string `json:"-"`

	// CodegenHeaderConsumers maps a (codemodel-derived) consuming target
	// Name to the element-root-relative generated-header output paths it
	// needs on its include path — recovered from the consumer's codemodel
	// UTILITY (tablegen / add_custom_target) dependencies whose
	// add_custom_command outputs are generated headers (a tablegen `.inc`).
	// The --split-packages emit transform consumes this to synthesize a
	// per-producing-package `generated_includes` cc_library (textual_hdrs +
	// includes) and wire each consumer to depend on it, so a consumer that
	// `#include`s a generated `.inc` resolves it under Bazel's sandbox.
	//
	// Like SubPackages this is out-of-band (`json:"-"`): it never
	// serializes into --out-ir-json (mutually exclusive with the split
	// transform that reads it). Empty when no consumer references a
	// generated header.
	CodegenHeaderConsumers map[string][]string `json:"-"`

	// SourceByteReads is the element-root-relative set of SOURCE/HEADER files
	// whose BYTES the lowering passes read in a way that AFFECTED the emitted
	// BUILD — the fused-source textual-include scan (a `.c` that `#include`s
	// another `.c`) and the generated-source-root-include rewrite. The
	// converter's translation is otherwise a pure function of the build-system
	// files (CMakeLists / *.cmake / *.in) + codemodel + trace, NOT of source
	// bytes; this field PUBLISHES the few, declared exceptions so the
	// source-narrowing lens can keep exactly these real (no false diff) and
	// FAIL only on an UNDECLARED source-byte read — and report the exception
	// set per member. The assumption is "source-byte reads are the exception,
	// not the rule"; publishing them makes it measurable and enforceable.
	// Sorted/deduped; element-root-relative (the shape of Target.Srcs).
	// Emitted via --out-source-reads.
	//
	// Out-of-band (`json:"-"`): never serializes into --out-ir-json.
	SourceByteReads []string `json:"-"`
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

	// LeadingComment is the author's source comment block recovered from
	// the originating CMakeLists (the contiguous `#` line-comment block
	// immediately above the declaring command, as raw `# ...` tokens).
	// Populated by the lowering pass only when recovery is enabled
	// (lower.Options.RecoverSourceComments) and the declaration site has a
	// leading comment; nil otherwise. The emitter renders it (above the
	// optional `# Source:` provenance breadcrumb) only when
	// emit.Options.EmitSourceComments is set — the two flags are driven
	// together by the `--emit-source-comments` CLI flag.
	LeadingComment []string

	// TrailingComment is an author comment trailing the declaring command on
	// its last line (e.g. `add_library(foo ...)  # core lib`), as a raw token
	// ("# core lib", leading `#` kept). Populated by the recovery pass like
	// LeadingComment; rendered under emit.Options.EmitSourceComments as a `#`
	// suffix on the rule — routing to leading placement on whole-rule-`# keep`
	// kinds (genrule/filegroup/…) to avoid stacking two suffix comments. Empty
	// when the declaration has no trailing comment.
	TrailingComment string

	// Srcs are compilation inputs (.c / .cc / .cpp / .S / etc.).
	Srcs []string

	// ModuleSrcs are Fortran sources that DEFINE a module (gfortran writes a
	// `.mod` for each), ordered so a module's provider precedes any provider
	// that `use`s it. Set only on KindFortranLibrary targets: fortran_library
	// compiles these first into a shared module directory, then compiles Srcs
	// in parallel against it. Empty for module-free Fortran (the F77 bulk) and
	// every non-Fortran rule. See lower's splitFortranModuleSrcs.
	ModuleSrcs []string

	// Hdrs are exported headers reachable via Includes/StripIncludePrefix.
	Hdrs []string

	// TextualHdrs are headers that are textually #included but not
	// standalone-compilable (the x-macro / `.inc` / `.def` idiom, and
	// generated tablegen `.inc` fragments). They render as the cc_library
	// `textual_hdrs` attribute, so Bazel makes them available to dependents
	// without trying to compile them as modular headers.
	TextualHdrs []string

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
	// ROADMAP.md.
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

	// RootInclude is set when the target's headers are include-rooted at the
	// element/package root — cmake's
	// `target_include_directories(t ${CMAKE_SOURCE_DIR})` (rel == ""). Bazel
	// rejects `includes = [""]`, so lower drops the entry from `includes =`
	// and walks the package root for hdrs instead (see walkPkgRootForHdrs /
	// rootWalkByTarget). That's correct for the monolithic emit (the headers
	// keep their root-relative path within the single package), but under
	// --split-packages a target that re-homes into a subpackage loses the
	// root-relative prefix on its headers (`glm/foo.hpp` → package-local
	// `foo.hpp`), breaking `#include <glm/foo.hpp>`. The split emitter reads
	// this signal and emits `include_prefix = <package dir>` to restore the
	// prefix (see rewriteTarget). Ignored by the monolithic emit.
	RootInclude bool

	// Copts, Defines, Linkopts pass through to the cc_* rule of the same name.
	Copts    []string
	Defines  []string
	LinkOpts []string

	// AdditionalLinkerInputs are workspace-relative file paths
	// (typically version-script .map / .exports / .ver files
	// referenced from LinkOpts via `-Wl,--version-script,$(location
	// :foo.map)` substitution). The emitter renders them as
	// `additional_linker_inputs = [...]` so Bazel pins the files
	// into the link action's input closure — closes the gap left
	// by the prior reanchor pass that rewrote the path's prefix
	// but didn't stage the file as a Bazel-visible source.
	AdditionalLinkerInputs []string

	// LocalDefines maps to Bazel's `local_defines = [...]`
	// attribute on cc_library / cc_binary / cc_test. Same shape
	// as Defines but non-transitive: consumers of the rule don't
	// see the macro at compile time. The cmake equivalent is
	// `target_compile_definitions(t PRIVATE FOO)` — PRIVATE
	// scope means the define applies only when compiling t's
	// own sources. Routing PRIVATE-scope defines here keeps the
	// rendered BUILD honest about cmake's scope rules instead
	// of over-propagating defines to consumers via Bazel's
	// transitive `defines`.
	//
	// Populated by applyPrivateScopeToDefines (lower-side
	// post-pass that consults shadow.Decoded.CompileDefinitions)
	// when the trace classifies a define as PRIVATE-scoped. The
	// post-pass only moves defines the trace explicitly tags
	// PRIVATE; codemodel-only paths (no trace) leave the field
	// unset and fold everything into Defines — strictly safe,
	// preserves byte-identical pre-existing emit.
	LocalDefines []string

	// Deps are Bazel labels to other targets whose headers are
	// reachable through this target's public hdrs. Maps to
	// `deps = [...]` on the emitted rule. Per gazelle_cc canon,
	// targets reached via `PUBLIC` or `INTERFACE`
	// target_link_libraries() in CMake belong here.
	Deps []string

	// DynamicDeps are Bazel labels to cc_shared_library targets this target
	// links DYNAMICALLY (the `dynamic_deps = [...]` attribute on
	// cc_binary/cc_test/cc_library). Under faithful-SHARED conversion
	// (EmitSharedLibraries), a consumer of a SHARED/MODULE library keeps the
	// impl cc_library in Deps (for headers) and lists the sibling
	// `<dep>_shared` here, so Bazel links the .so instead of static-linking the
	// impl (and the "owned by at most one shared lib" rule is satisfied). Empty
	// for the default static-collapse emit.
	DynamicDeps []string

	// SharedLibDynamicDeps are the dynamic_deps for the cc_shared_library
	// WRAPPER emitted for this (SharedLibName-bearing) target — the
	// `<dep>_shared` siblings for deps that are themselves shared libs. Without
	// them a shared lib statically re-links a dep that another shared lib also
	// owns, violating cc_shared_library's "a cc_library is linked into at most
	// one shared lib" rule (the brotli brotlidec/brotlicommon case). Empty for
	// non-shared targets and leaf shared libs.
	SharedLibDynamicDeps []string

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
	// ROADMAP.md.
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
	// ROADMAP.md for the cc
	// emitter's convention and its gazelle_cc lineage.
	Visibility []string

	// Linkstatic / Alwayslink only meaningful for KindCCLibrary.
	Linkstatic bool
	Alwayslink bool

	// SharedLibName, when non-empty, marks a KindCCLibrary that cmake declared
	// as a SHARED_LIBRARY / MODULE_LIBRARY: the cc_library is the static
	// implementation, and emit ALSO renders a sibling `cc_shared_library`
	// (name `<target>_shared`, roots=[":<target>"]) producing this real .so
	// (the value is the cmake artifact name, e.g. "libz.so"). Faithful-SHARED
	// conversion — see ROADMAP's cc_shared_library item. Populated by lower only
	// under the opt-in EmitSharedLibraries mode, so the default (static-collapse)
	// emit stays byte-identical; empty for every static lib.
	SharedLibName string

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

	// PkgPrefix is the install destination carried by a
	// KindPkgFiles target — it renders as the `prefix = "<dest>"`
	// attribute on the emitted pkg_files rule. cmake's
	// install(FILES ... DESTINATION lib) / install(DIRECTORY ...
	// DESTINATION include) records the DESTINATION; the lowering
	// pass (lowerDirectoryInstallers) carries it here so the
	// packaging shape preserves where each file lands. Empty on
	// every non-pkg_files target.
	PkgPrefix string

	// FilegroupOutputGroup, when non-empty on a KindFilegroup target,
	// renders the filegroup's `output_group = "<name>"` attribute so the
	// filegroup gathers the named output group of its srcs dependency
	// instead of that dependency's default outputs. The cmake converter
	// emits `output_group = "compilation_outputs"` filegroups over an
	// OBJECT_LIBRARY's cc_library to expose its compiled .o files as an
	// addressable label for `$<TARGET_OBJECTS:t>` resolution (file_generate
	// lowering). Empty on every other filegroup.
	FilegroupOutputGroup string

	// FilegroupGlob, when non-empty on a KindFilegroup target, renders the
	// filegroup's srcs as `glob([<patterns>])` (package-relative) instead
	// of an explicit file list. Used for filegroups synthesized from a
	// cmake `file(GLOB ...)` call: the glob is the source of truth in the
	// original build, so a build-time glob() is the honest representation —
	// project B picks up newly added matches instead of carrying a frozen
	// convert-time snapshot. Mutually exclusive with an explicit Srcs list
	// on the same target.
	FilegroupGlob []string

	// PkgSrcsGlob, when true on a KindPkgFiles target, makes the
	// emitter render `srcs = glob(["<dir>/**"])` (one glob per Srcs
	// entry) instead of the literal `srcs = [...]` list. This is the
	// install(DIRECTORY ...) shape: cmake's install(DIRECTORY) names
	// a *source directory* whose entire tree is packaged, and a bare
	// directory in a pkg_files `srcs` does NOT package the directory's
	// files — a consuming pkg_tar fails with IsADirectoryError. The
	// glob expands the directory to its constituent files so they're
	// addressable Bazel inputs. install(FILES ...) keeps the literal
	// list (PkgSrcsGlob == false) since those srcs are already
	// individual files.
	PkgSrcsGlob bool

	// PkgStripPrefix, when non-empty on a KindPkgFiles target with
	// PkgSrcsGlob, renders as
	// `strip_prefix = strip_prefix.from_pkg("<PkgStripPrefix>")`. It
	// carries the package-relative source directory whose leading path
	// rules_pkg strips before applying `prefix`, so the globbed files
	// land at "<PkgPrefix>/<rel-under-dir>" rather than
	// "<PkgPrefix>/<dir>/<rel>". For cmake's
	// install(DIRECTORY include/ DESTINATION include) (trailing slash
	// = "contents of include/ into DESTINATION") this is "include":
	// glob(["include/**"]) + strip_prefix.from_pkg("include") +
	// prefix="include" packages include/foo.h at include/foo.h.
	//
	// cmake's no-trailing-slash form (install(DIRECTORY include
	// DESTINATION include) — "the include dir itself into DESTINATION",
	// yielding include/include/foo.h) IS modeled: the File API records
	// it as a plain-string path (vs. the trailing-slash form's
	// {"from","to":"."} object), and the lowering then sets
	// PkgStripPrefix to the dir's PARENT (so the dir name is preserved
	// under the prefix) rather than the dir itself. See
	// lowerDirectoryInstallers.
	PkgStripPrefix string

	// PkgRenames, when non-empty on a KindPkgFiles target, renders as
	// `renames = {"<src>": "<dest>", ...}` on the pkg_files rule — the
	// rules_pkg per-file destination override (dest is relative to
	// PkgPrefix). It carries cmake's install(FILES ... RENAME ...): the
	// File API records a renamed file installer as a {"from","to"}
	// object path (vs. the plain string of an un-renamed FILES
	// installer), where "to" is the destination name under the
	// DESTINATION. Each entry maps the source file (as it appears in
	// Srcs) to its renamed destination. Empty for installers with no
	// RENAME. Keys are a subset of Srcs.
	PkgRenames map[string]string

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

	// PickFile-specific fields. Populated only when Kind == KindPickFile.

	// PickSrc is the Bazel label of the pipeline_install target whose
	// install-root TreeArtifact this pick_file projects a file out of
	// (e.g. ":foo_trace_build", the same-package install target).
	PickSrc string

	// PickPath is the path inside the TreeArtifact to project, e.g.
	// "lib/libthelib.a". The projected File keeps this path's basename.
	PickPath string

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

	// GlobSrcGroups, on a KindGenrule, records source-file groups whose
	// members came from a cmake file(GLOB)/file(GLOB_RECURSE) call — the
	// generic globbing-genrule shape, where a genrule's inputs are a glob
	// result rather than an explicit list. Lower records one group per glob
	// but KEEPS the matched files in Srcs (each group's Files names them);
	// split then drops those files and synthesizes a
	// filegroup(srcs = glob([<pattern>])) in the group's owning package,
	// splicing its label into the genrule's srcs so the glob re-evaluates
	// in project B. The monolithic emitter keeps the explicit Srcs (it
	// synthesizes no filegroup), so neither path loses inputs. Empty for
	// ordinary genrules.
	GlobSrcGroups []GlobSrcGroup

	// write_file-specific fields. Populated only when Kind ==
	// KindWriteFile.

	// WriteFileOut is the package-relative output path (the rule's
	// `out` attribute).
	WriteFileOut string

	// WriteFileContent is the file body as a list of lines, which
	// skylib's write_file joins with WriteFileNewline. Built as
	// strings.Split(body, "\n") so the join round-trips the original
	// bytes exactly.
	WriteFileContent []string

	// WriteFileNewline is the skylib `newline` attribute ("unix" for
	// "\n", "windows" for "\r\n"). The converter emits "unix" — it
	// only routes \n-only bodies to write_file; CRLF / binary bodies
	// stay on the base64 genrule.
	WriteFileNewline string

	// CMakeConfigureFile carries the cmake_configure_file rule's
	// attributes. Non-nil only when Kind == KindCMakeConfigureFile.
	CMakeConfigureFile *CMakeConfigureFileSpec

	// CCEmbed carries the cc_embed rule's attributes. Non-nil only when
	// Kind == KindCCEmbed.
	CCEmbed *CCEmbedSpec

	// CCHash carries the cc_hash rule's attributes. Non-nil only when
	// Kind == KindCCHash.
	CCHash *CCHashSpec

	// AliasActual is the Bazel label the alias resolves to.
	// Populated only when Kind == KindAlias; renders as
	// `actual = "<label>"` on the alias rule. Typically a
	// package-relative `:<target>` form for in-tree aliases.
	AliasActual string

	// Build-setting fields, populated only for the lifted-feature-probe
	// pair (KindBoolFlag / KindConfigSetting).

	// BoolFlagDefault is the bool_flag's build_setting_default — the
	// value cmake's probe produced (false when the probe's value
	// wasn't captured). KindBoolFlag only.
	BoolFlagDefault bool

	// ConfigSettingFlag / ConfigSettingValue render the config_setting's
	// flag_values = {<ConfigSettingFlag>: <ConfigSettingValue>}: the
	// label of the paired bool_flag (e.g. ":have_x") and the value that
	// selects it ("True"). KindConfigSetting only.
	ConfigSettingFlag  string
	ConfigSettingValue string

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

// GlobSrcGroup names one cmake file(GLOB)-derived source group: Dir is the
// element-root-relative directory the glob is anchored at, Pattern is the
// Bazel glob pattern relative to Dir ("*.txt" for GLOB, "**/*.txt" for
// GLOB_RECURSE), and Files is the element-root-relative match set that
// seeded the group. split turns each into a build-time glob() filegroup in
// Dir's owning package and drops Files from the genrule's explicit srcs
// (served by the filegroup instead). The monolithic emitter, which doesn't
// synthesize the filegroups, leaves the explicit srcs in place — so lower
// keeps Files in Srcs and only split removes them, keeping both emitters
// correct.
type GlobSrcGroup struct {
	Dir     string
	Pattern string
	Files   []string
}

// CMakeConfigureFileSpec carries the attributes for a
// KindCMakeConfigureFile target — the configure_file / file(GENERATE)
// lift tier's cmake_configure_file rule. The emitter projects these
// directly onto the rule's attributes; the rule's impl re-renders the
// template at Bazel build time via //tools:cmake-configure-file.
type CMakeConfigureFileSpec struct {
	// Out is the package-relative output path (the rule's `out`).
	Out string

	// Template is the package-relative template label/path (the rule's
	// `template`, INPUT form). Empty selects the CONTENT form, where
	// Content carries the inline body instead.
	Template string

	// Content is the inline template body (CONTENT form). Used only when
	// Template is empty; the rule writes it to a file and feeds it to the
	// tool as the template input (no --content-base64). An empty Content
	// with an empty Template is a legitimate empty-template emission.
	Content string

	// Values is the cmake variable -> value substitution map (the tool's
	// --values). Rendered as a readable Starlark string_dict.
	Values map[string]string

	// StampValues maps a template variable -> Bazel workspace-status key
	// (the rule's `stamp_values`; e.g. GIT_SHA -> STABLE_GIT_SHA). For a
	// var written by a VCS-stamp execute_process (git/hg/svn rev-parse),
	// the rule reads the live value from the stable workspace status at
	// build time (under --stamp + --workspace_status_command) and
	// overrides the Values entry — so a `@GIT_SHA@` header re-reads the
	// current revision instead of the convert-time one baked into Values
	// (which stays as the non-stamped fallback). Empty for the common
	// configure_file with no stamp-sourced variable.
	StampValues map[string]string

	// GenexValues is the captured `$<...>` literal -> resolved bytes map
	// for the structured-replay (b)/(b′) lift (the tool's --genex-values).
	// Mutually exclusive with GenexContext.
	GenexValues map[string]string

	// GenexValuesPerConfig carries per-config genex literal -> value maps when
	// the (b′) two-pass probe found a top-level literal resolves to DIFFERENT
	// bytes per build config (Ninja Multi-Config). Keyed by the Bazel config
	// label (`//config:<name>`); each value is that config's literal -> value
	// map. When non-empty it supersedes GenexValues and the emitter renders
	// the `genex_values` attribute as a `select()` over the config labels (the
	// rule's string_dict attr is configurable, so Bazel resolves the active
	// config's map before the rule impl runs — no rule change needed). Empty
	// for the config-uniform case (GenexValues carries the flat map).
	GenexValuesPerConfig map[string]map[string]string

	// GenexContext is the cmake configure-time context JSON the Go-side
	// genex evaluator consults for the (a)-evaluator lift (the tool's
	// --genex-context). Rides as a readable JSON string attribute (not
	// base64). Mutually exclusive with GenexValues.
	GenexContext string

	// TargetFiles maps a Bazel label -> cmake target name for
	// `$<TARGET_FILE:name>` resolution (the rule's `target_files`
	// label-keyed dict; the impl emits one --target-file name=<path>).
	// Label-keyed so Bazel tracks the dependency and resolves the path
	// at action time — no separate srcs entry needed for cross-package
	// labels (which the genrule shape required).
	TargetFiles map[string]string

	// TargetObjects maps a Bazel label -> cmake object-library name for
	// `$<TARGET_OBJECTS:name>` resolution (the rule's `target_objects`
	// label-keyed dict; the impl emits --target-objects name=<colon-joined
	// object paths>).
	TargetObjects map[string]string

	// Tool is the Bazel label of the cmake-configure-file binary (the
	// rule's `tool`), e.g. "//tools:cmake-configure-file". Resolves in
	// whichever repo the BUILD lands, same as the genrule shape's
	// tools=[...] entry.
	Tool string

	// AtOnly / CopyOnly / EscapeQuotes / NewlineStyle mirror cmake's
	// configure_file options, passed through to the tool. NewlineStyle is
	// "" (preserve), "lf", or "crlf".
	AtOnly       bool
	CopyOnly     bool
	EscapeQuotes bool
	NewlineStyle string
}

// CCEmbedSpec carries the attributes for a KindCCEmbed target — the
// cc_embed rule that embeds a file's bytes as a named C symbol (the
// native lowering of vtkEncodeString-shaped cmake -P codegen). The
// emitter projects these onto the rule's attributes; the rule runs
// //tools:cc-embed at build time.
type CCEmbedSpec struct {
	// Src is the package-relative path of the file whose bytes are
	// embedded (the rule's `src`).
	Src string
	// Symbol is the C symbol name for the embedded data (the rule's
	// `symbol`; the consumer references it).
	Symbol string
	// OutHeader / OutSource are the package-relative predeclared outputs
	// (the rule's `out_header` / `out_source`), e.g. "<name>.h" / "<name>.cxx".
	OutHeader string
	OutSource string
	// Binary emits an unsigned char[] byte array instead of a C string;
	// NulTerminate appends a trailing NUL (binary only). Mirror
	// vtk_encode_string's BINARY / NUL_TERMINATE.
	Binary       bool
	NulTerminate bool
	// ExportSymbol / ExportHeader mirror vtk_encode_string's EXPORT_SYMBOL
	// / EXPORT_HEADER (a visibility macro + the header providing it). Both
	// empty for the common case.
	ExportSymbol string
	ExportHeader string
}

// CCHashSpec carries the attributes for a KindCCHash target — the cc_hash
// rule that hashes a file's bytes and writes a header `#define`-ing the
// digest as a C string (the native lowering of vtkHashSource-shaped cmake
// -P codegen). The emitter projects these onto the rule's attributes; the
// rule runs //tools:cc-hash at build time.
type CCHashSpec struct {
	// Src is the package-relative path of the file whose bytes are hashed
	// (the rule's `src`).
	Src string
	// Name is the C `#define` name (and include-guard) the digest is bound
	// to (the rule's `define_name`; the consumer references it). Mirrors
	// vtk_hash_source's NAME.
	Name string
	// Algorithm is the digest: MD5, SHA1, SHA224, SHA256, SHA384 or SHA512
	// (the rule's `algorithm`). Mirrors vtk_hash_source's ALGORITHM.
	Algorithm string
	// OutHeader is the package-relative predeclared header output (the
	// rule's `out_header`), e.g. "<name>.h".
	OutHeader string
}
