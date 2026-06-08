package exportshape

import (
	"path"
	"sort"
	"strings"

	"github.com/sstriker/buildstream-bazel/converter/internal/fileapi"
	"github.com/sstriker/buildstream-bazel/converter/ir"
)

// EmitInputs aggregates the data the IR projection needs.
type EmitInputs struct {
	// Installer is the install(EXPORT) DirectoryInstaller whose
	// Verdict.Declarative is true. The classifier ran upstream;
	// EmitDeclarative trusts that gate.
	Installer fileapi.DirectoryInstaller

	// Targets is fileapi.Reply.Targets keyed by target id —
	// used to look up each exported target's TYPE + NameOnDisk +
	// Install destinations.
	Targets map[string]fileapi.Target

	// InstallFiles is the slash-form sorted list of paths the
	// install(EXPORT) bundle would land at. EmitDeclarative
	// resolves each exported target's NameOnDisk against this
	// list to find the artifact path. The caller derives the list
	// from codemodel-only sources (Target.Install.Destinations +
	// Target.Artifacts) — the convert action does NOT run
	// cmake --install to materialize the tree; that's Bazel's job
	// under its own action graph. See generator-parity-uplift.md
	// Phase 6 for the "convert is metadata-only" constraint.
	InstallFiles []string

	// CMakeConfigBundleFiles is the subset of InstallFiles that
	// land under the install(EXPORT) destination (typically
	// lib/cmake/<Pkg>/). Emitted as a separate filegroup so
	// downstream cmake-tooling consumers (mixed Bazel + cmake
	// builds) can address the bundle directly.
	CMakeConfigBundleFiles []string

	// PublicHeaders are install tree paths corresponding to the
	// exported targets' public headers (from Target.FileSets
	// HEADERS where the destination matches a canonical include
	// prefix). One filegroup per target, named per the target.
	PublicHeaders map[string][]string

	// EmitConfig opts in to generating the install(EXPORT) config-mode bundle
	// (the real <Pkg>Targets.cmake + the cmake_config_bundle filegroup). OFF by
	// default: in the orchestrated graph the wired bundle is the synthprefix-
	// synthesized one (write-a emits its own tar-based cmake_config_bundle), so
	// the converter's standalone bundle is unused, and emitting it would only
	// break `bazel build //...` (the install(EXPORT)-generated .cmake files
	// don't exist on disk). A project shipping the element for EXTERNAL
	// config-mode consumption opts in (--emit-install-export-config); the
	// converter then GENERATES the real Targets.cmake.
	EmitConfig bool
}

// EmitDeclarative projects a declarative install(EXPORT) bundle
// into IR targets the converter's emitter renders:
//
//   - One cc_import per exported target with TYPE in
//     {STATIC_LIBRARY, SHARED_LIBRARY}. The cc_import's
//     static_library / shared_library points at the install-tree
//     path of the artifact, resolved by matching the target's
//     NameOnDisk against InstallFiles under the target's install
//     destination.
//
//   - One cc_library per exported INTERFACE_LIBRARY target —
//     header-only, no on-disk artifact, so cc_import doesn't fit.
//
//   - One filegroup per exported target's public headers, named
//     `<target>_hdrs`. Source list from
//     EmitInputs.PublicHeaders[target.Name].
//
//   - When EmitInputs.EmitConfig is set (opt-in): a write_file
//     generating the real <Pkg>Targets.cmake (imported-target defs)
//     per CMakeConfigBundleFiles entry, plus a "cmake_config_bundle"
//     filegroup referencing those producers. OFF by default — the
//     orchestrated graph wires the synthprefix-synthesized bundle, so
//     the converter's standalone bundle is unused there and emitting a
//     filegroup over the (install-generated, not-on-disk) .cmake files
//     would only break `bazel build //...`.
//
// Returns nil + nil for non-declarative installers (the caller
// should have gated on Classify but the guard is defensive).
// Returns IR targets in deterministic order (alphabetical by name)
// so emit produces byte-stable BUILD output.
//
// Phase 6 of the generator-parity uplift (ROADMAP.md).
func EmitDeclarative(in EmitInputs) []ir.Target {
	if in.Installer.Type != "export" {
		return nil
	}
	if in.Installer.ExportName == "" || len(in.Installer.ExportTargets) == 0 {
		return nil
	}

	var out []ir.Target
	// One cc_import / cc_library per exported target.
	//
	// Name source: ExportTarget.Name is the bare target name (as
	// recorded in the schema docs), but the codemodel as of cmake
	// 3.20+ leaves the field empty and emits {"id": ..., "index":
	// ...} entries instead — the canonical name lives on
	// Target.Name (looked up by id). Treat the ExportTarget.Name
	// as a hint and fall back to Target.Name when empty.
	for _, et := range in.Installer.ExportTargets {
		t, ok := in.Targets[et.Id]
		if !ok {
			continue
		}
		name := et.Name
		if name == "" {
			name = t.Name
		}
		if name == "" {
			// Unrecoverable — skip.
			continue
		}
		switch t.Type {
		case "STATIC_LIBRARY", "SHARED_LIBRARY", "MODULE_LIBRARY":
			tgt := emitCCImport(name, t, in.InstallFiles)
			if tgt.Name != "" {
				out = append(out, tgt)
			}
		case "INTERFACE_LIBRARY":
			tgt := emitCCInterface(name)
			out = append(out, tgt)
		}
		// Headers per target, when populated.
		if hdrs := in.PublicHeaders[name]; len(hdrs) > 0 {
			sortedHdrs := append([]string(nil), hdrs...)
			sort.Strings(sortedHdrs)
			out = append(out, ir.Target{
				Name:       name + "_hdrs",
				Kind:       ir.KindFilegroup,
				Srcs:       sortedHdrs,
				Visibility: []string{"//visibility:public"},
			})
		}
	}

	// install(EXPORT) config-mode bundle — OPT-IN (in.EmitConfig). The bundle's
	// <Pkg>Targets.cmake is install(EXPORT)-GENERATED (not a source file); when
	// opted in the converter GENERATES it with a write_file (real imported-target
	// defs whose IMPORTED_LOCATION / INTERFACE_INCLUDE_DIRECTORIES synthprefix
	// parses) and the cmake_config_bundle filegroup references those producers.
	// OFF by default: the orchestrated graph wires its own synthprefix-synthesized
	// bundle, so the converter's standalone bundle is unused there, and emitting a
	// filegroup over the (not-on-disk) .cmake files would only break
	// `bazel build //...`. The build lens opts in to exercise the generation; see
	// EmitInputs.EmitConfig.
	if in.EmitConfig && len(in.CMakeConfigBundleFiles) > 0 {
		bundle := append([]string(nil), in.CMakeConfigBundleFiles...)
		sort.Strings(bundle)
		var bundleSrcs []string
		for _, f := range bundle {
			gen := "gen_" + sanitizeBundleName(f)
			out = append(out, ir.Target{
				Name:             gen,
				Kind:             ir.KindWriteFile,
				WriteFileOut:     f,
				WriteFileContent: renderBundleFile(in, f),
				WriteFileNewline: "unix",
				// Private: an impl detail surfaced via the public cmake_config_bundle
				// filegroup, which references these producers same-package (the bundle
				// files land in the install-export targets' own package).
				Visibility: []string{"//visibility:private"},
				Tags:       []string{"cmake-codegen-install-export-config"},
			})
			bundleSrcs = append(bundleSrcs, ":"+gen)
		}
		out = append(out, ir.Target{
			Name:       "cmake_config_bundle",
			Kind:       ir.KindFilegroup,
			Srcs:       bundleSrcs,
			Visibility: []string{"//visibility:public"},
		})
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// sanitizeBundleName turns an install-tree path (lib/cmake/Pkg/PkgTargets.cmake)
// into a Bazel-identifier-safe suffix for the write_file producer's name.
func sanitizeBundleName(f string) string {
	var b strings.Builder
	for _, r := range f {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' {
			b.WriteRune(r)
		} else {
			b.WriteRune('_')
		}
	}
	return b.String()
}

// renderBundleFile dispatches a config-mode bundle file to the right renderer
// by its role (basename): the <Pkg>Targets.cmake (the install(EXPORT) export
// script) gets the imported-target definitions; a <Pkg>Config.cmake config
// entry point just include()s the targets script; a <Pkg>ConfigVersion.cmake
// gets a minimal version stub. Emitting Targets content into the Config /
// ConfigVersion files would be semantically wrong (a ConfigVersion with
// add_library() calls breaks find_package() version resolution).
func renderBundleFile(in EmitInputs, bundleFile string) []string {
	base := path.Base(bundleFile)
	switch {
	case strings.HasSuffix(base, "ConfigVersion.cmake"):
		return renderConfigVersionFile()
	case strings.HasSuffix(base, "Config.cmake"):
		return renderConfigFile(in)
	default:
		return renderExportTargetsFile(in, bundleFile)
	}
}

// renderConfigFile generates the <Pkg>Config.cmake config-mode entry point.
// find_package(<Pkg> CONFIG) loads THIS file, so it must include() the imported-
// target script(s). It include()s EVERY sibling *.cmake in the package dir
// except the Config / ConfigVersion files themselves — i.e. all the
// install(EXPORT) target scripts. The glob (rather than a single
// include(<ExportName>.cmake)) is load-bearing for MULTI-EXPORT packages: a
// project that ships separate shared + static exports (zstd:
// zstdExports-shared.cmake + zstdExports-static.cmake) has two export scripts in
// the dir, and BuildInputs runs once per install(EXPORT) — so a single-name
// include would resolve only one export and leave the others' targets
// unreachable. All scripts sit in the same dir, so CMAKE_CURRENT_LIST_DIR
// addresses them. The two excluded names match what BuildInputs generates.
func renderConfigFile(in EmitInputs) []string {
	pkg := pkgFromBundle(in.Installer.Destination, in.Installer.ExportName)
	return []string{
		"# " + pkg + " package config — generated by convert-element-cmake.",
		"# include() every install(EXPORT) target script in this directory so",
		"# find_package(" + pkg + " CONFIG) resolves ALL exported targets — including",
		"# multi-export packages (e.g. separate shared + static exports).",
		`file(GLOB _bsb_target_scripts "${CMAKE_CURRENT_LIST_DIR}/*.cmake")`,
		"foreach(_bsb_script IN LISTS _bsb_target_scripts)",
		`  get_filename_component(_bsb_name "${_bsb_script}" NAME)`,
		`  if(NOT _bsb_name STREQUAL "` + pkg + `Config.cmake" AND NOT _bsb_name STREQUAL "` + pkg + `ConfigVersion.cmake")`,
		`    include("${_bsb_script}")`,
		"  endif()",
		"endforeach()",
		"unset(_bsb_target_scripts)",
		"unset(_bsb_script)",
		"unset(_bsb_name)",
	}
}

// renderConfigVersionFile generates a minimal <Pkg>ConfigVersion.cmake: a
// permissive always-compatible stub. The export's project VERSION isn't carried
// in the codemodel, so a generated bundle can't reproduce
// write_basic_package_version_file's real comparison; this satisfies any
// find_package(<Pkg> <ver>) request, adequate for a bundle a downstream wires
// by label (the imported targets in the Targets script are the real payload).
func renderConfigVersionFile() []string {
	return []string{
		"# Generated by convert-element-cmake — minimal config-version stub",
		"# (the export's project VERSION isn't in the codemodel).",
		`set(PACKAGE_VERSION "0.0.0")`,
		"set(PACKAGE_VERSION_COMPATIBLE TRUE)",
		// EXACT too: find_package(<Pkg> <ver> EXACT) checks PACKAGE_VERSION_EXACT,
		// so the permissive "always-compatible" stub must set it or EXACT requests
		// fail despite the intent.
		"set(PACKAGE_VERSION_EXACT TRUE)",
	}
}

// pkgFromBundle derives the package name (the find_package(<Pkg>) namespace,
// since the codemodel doesn't carry install(EXPORT NAMESPACE)) from the bundle
// destination, falling back to the export name. The <Pkg> segment of the dest
// is preferred — lib/cmake/<Pkg> (last component) or share/<Pkg>/cmake (before
// the trailing "cmake"). But a destination can be exactly a CanonicalDestinations
// prefix with no <Pkg> appended (DESTINATION lib/cmake, lib64/cmake, share/cmake,
// share); there the path carries no package name, so derive <Pkg> from the export
// name instead (the install(EXPORT <Pkg>Targets) convention — strip a trailing
// "Targets"). Keying off CanonicalDestinations keeps this in lockstep with what
// Classify accepts (e.g. the multilib lib64/cmake).
func pkgFromBundle(dest, exportName string) string {
	cleaned := strings.Trim(dest, "/") + "/"
	for _, p := range CanonicalDestinations {
		if cleaned == p {
			return strings.TrimSuffix(exportName, "Targets")
		}
	}
	parts := strings.Split(strings.Trim(dest, "/"), "/")
	cand := parts[len(parts)-1]
	if cand == "cmake" && len(parts) >= 2 {
		cand = parts[len(parts)-2]
	}
	if cand == "" {
		return strings.TrimSuffix(exportName, "Targets")
	}
	return cand
}

// renderExportTargetsFile generates a real install(EXPORT) <Pkg>Targets.cmake:
// imported-target definitions for the exported libraries, with IMPORTED_LOCATION
// + INTERFACE_INCLUDE_DIRECTORIES anchored at ${_IMPORT_PREFIX} (computed by
// climbing from the file's install location to the prefix root). This is the
// format cmake config-mode consumers `include()` AND the format synthprefix
// parses to stub imported paths. The codemodel doesn't carry the export
// NAMESPACE, so the imported-target prefix is derived from the bundle's <Pkg>
// directory (the find_package(<Pkg>) convention, NAMESPACE <Pkg>::).
func renderExportTargetsFile(in EmitInputs, bundleFile string) []string {
	dest := path.Dir(bundleFile)                        // lib/cmake/GTest  (or share/GTest/cmake)
	pkg := pkgFromBundle(dest, in.Installer.ExportName) // GTest
	climb := strings.Count(dest, "/") + 1               // components of dest → climbs to prefix root
	lines := []string{
		"# " + path.Base(bundleFile) + " — generated by convert-element-cmake from",
		"# install(EXPORT " + in.Installer.ExportName + "). Imported targets for",
		"# find_package(" + pkg + " CONFIG) consumers. In the orchestrated graph the",
		"# wired bundle is the synthprefix-synthesized one; this is the bundle a",
		"# project ships for EXTERNAL config-mode consumption.",
		`get_filename_component(_IMPORT_PREFIX "${CMAKE_CURRENT_LIST_FILE}" PATH)`,
	}
	for i := 0; i < climb; i++ {
		lines = append(lines, `get_filename_component(_IMPORT_PREFIX "${_IMPORT_PREFIX}" PATH)`)
	}
	// Normalize a root install prefix (cmake's own generated export files do this
	// after the climb) so "${_IMPORT_PREFIX}/include" doesn't expand to
	// "//include" when the prefix root is "/".
	lines = append(lines,
		`if(_IMPORT_PREFIX STREQUAL "/")`,
		`  set(_IMPORT_PREFIX "")`,
		`endif()`,
	)
	for _, et := range in.Installer.ExportTargets {
		t, ok := in.Targets[et.Id]
		if !ok {
			continue
		}
		name := et.Name
		if name == "" {
			name = t.Name
		}
		if name == "" {
			continue
		}
		imported := pkg + "::" + name
		switch t.Type {
		case "INTERFACE_LIBRARY":
			lines = append(lines,
				"",
				"add_library("+imported+" INTERFACE IMPORTED)",
				"set_target_properties("+imported+" PROPERTIES",
				`  INTERFACE_INCLUDE_DIRECTORIES "${_IMPORT_PREFIX}/include")`,
			)
		case "STATIC_LIBRARY", "SHARED_LIBRARY", "MODULE_LIBRARY":
			artifact := resolveExportArtifact(t, in.InstallFiles)
			if artifact == "" {
				continue
			}
			kind := "STATIC"
			switch t.Type {
			case "SHARED_LIBRARY":
				kind = "SHARED"
			case "MODULE_LIBRARY":
				// cmake distinguishes MODULE IMPORTED from SHARED — a module lib
				// isn't linkable like a shared lib, so don't flatten it to SHARED.
				kind = "MODULE"
			}
			lines = append(lines,
				"",
				"add_library("+imported+" "+kind+" IMPORTED)",
				"set_target_properties("+imported+" PROPERTIES",
				`  INTERFACE_INCLUDE_DIRECTORIES "${_IMPORT_PREFIX}/include")`,
				"set_property(TARGET "+imported+" APPEND PROPERTY IMPORTED_CONFIGURATIONS NOCONFIG)",
				"set_target_properties("+imported+" PROPERTIES",
				`  IMPORTED_LOCATION_NOCONFIG "${_IMPORT_PREFIX}/`+artifact+`")`,
			)
		}
	}
	// Clear the internal _IMPORT_PREFIX so it doesn't leak into the including
	// project's scope (cmake's own generated export scripts do this at the end).
	lines = append(lines, "", "set(_IMPORT_PREFIX)")
	return lines
}

// resolveExportArtifact returns the install-tree path (<dest>/<NameOnDisk>) of
// an exported library target, or "" when it can't be resolved against the
// staged InstallFiles (mirrors emitCCImport's resolution).
func resolveExportArtifact(t fileapi.Target, installFiles []string) string {
	if t.NameOnDisk == "" || t.Install == nil {
		return ""
	}
	for _, dest := range t.Install.Destinations {
		artifact := path.Join(dest.Path, t.NameOnDisk)
		if containsString(installFiles, artifact) {
			return artifact
		}
	}
	return ""
}

// emitCCImport finds t.NameOnDisk under t.Install.Destinations[0]
// in installFiles and emits a cc_import pointing at it. Returns a
// zero ir.Target when the artifact can't be resolved (caller skips
// silently — the install must not have run, or the destination
// shape didn't match).
func emitCCImport(name string, t fileapi.Target, installFiles []string) ir.Target {
	if t.NameOnDisk == "" || t.Install == nil || len(t.Install.Destinations) == 0 {
		return ir.Target{}
	}
	// Try each destination — first that resolves wins.
	for _, dest := range t.Install.Destinations {
		artifact := path.Join(dest.Path, t.NameOnDisk)
		if !containsString(installFiles, artifact) {
			continue
		}
		ti := ir.Target{
			Name:       name,
			Kind:       ir.KindCCImport,
			Visibility: []string{"//visibility:public"},
		}
		// STATIC_LIBRARY → static_library; SHARED / MODULE →
		// shared_library. Match what the existing cc_import
		// emission does for the round-2 fallback's per-target
		// stubs.
		if strings.HasSuffix(t.NameOnDisk, ".a") || t.Type == "STATIC_LIBRARY" {
			ti.StaticLibrary = artifact
		} else {
			ti.SharedLibrary = artifact
		}
		return ti
	}
	return ir.Target{}
}

// emitCCInterface produces a cc_library with no srcs / no hdrs —
// the converter's KindCCInterface shape (cc_library rendered with
// hdrs only in the round-2 fallback). For an exported
// INTERFACE_LIBRARY, downstream consumers depend on the rule for
// transitive INTERFACE_* propagation; the rule itself produces no
// artifact.
//
// When the export ships headers via a FILE_SET, the per-target
// _hdrs filegroup above carries those — the cc_library here is a
// thin wrapper that consumers can depend on uniformly with the
// cc_import-shaped exports.
func emitCCInterface(name string) ir.Target {
	return ir.Target{
		Name:       name,
		Kind:       ir.KindCCInterface,
		Visibility: []string{"//visibility:public"},
	}
}

func containsString(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}
