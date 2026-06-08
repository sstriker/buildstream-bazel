package exportshape

import (
	"path"
	"sort"
	"strings"

	"github.com/sstriker/buildstream-bazel/converter/internal/fileapi"
)

// BuildInputs synthesizes EmitInputs from codemodel-only sources for a
// declarative install(EXPORT) installer. The synthesis is metadata-
// only: no convert-time `cmake --install` runs, no install tree is
// walked. The InstallFiles / CMakeConfigBundleFiles / PublicHeaders
// the caller passes to EmitDeclarative all derive from what the
// codemodel records about what cmake WILL produce when the producer's
// converted BUILD rule builds under Bazel.
//
// Phase 6 of the generator-parity uplift (ROADMAP.md) — closes the
// "EmitInputs from codemodel" slice. The earlier WIP that wired
// convert-time `cmake --build` / `cmake --install` to populate these
// lists was backed out; this function replaces that wiring with a
// metadata-only walk.
//
// Codemodel sources consulted:
//
//   - Target.Install.Destinations[0].Path + Target.NameOnDisk →
//     the artifact path the install would land at. Joined into
//     InstallFiles so EmitDeclarative.emitCCImport's resolve step
//     finds the artifact. First destination wins (cmake encodes
//     ARCHIVE / LIBRARY / RUNTIME under a single Destinations list
//     without per-kind tags; the canonical artifact for a given
//     target type lives in the first matching slot).
//
//   - DirectoryInstaller.Destination + "/" + ExportName + ".cmake"
//     → the per-export "<Pkg>Targets.cmake" file the install(EXPORT)
//     would generate. Surfaced in CMakeConfigBundleFiles so the
//     bundle filegroup carries the export script alongside any
//     companion files. The companion <Pkg>Config.cmake /
//     <Pkg>ConfigVersion.cmake files (from
//     CMakePackageConfigHelpers::write_basic_package_version_file
//     and configure_package_config_file) are NOT synthesized here
//     — they're operator-authored install(FILES …) calls that show
//     up as separate "file"-type installers; the round-1 install-
//     files lowering picks them up via the standard install_files__
//     filegroup path.
//
//   - Target.FileSets[].Type == "HEADERS" with Visibility ∈
//     {"PUBLIC", "INTERFACE"} → public headers exposed via
//     target_sources(... FILE_SET HEADERS …). For each exported
//     target, walks its FileSets + the matching Target.Sources
//     entries (correlated via TargetSource.FileSetIndex), computes
//     each header's path under the first matching BaseDirectory
//     prefix, and joins under the install-prefix "include/"
//     convention. The headers list is keyed by the exported target
//     name so EmitDeclarative produces one filegroup per target.
//
// Out of scope (deliberate):
//
//   - Multi-config / per-config NameOnDisk variance — v1 uses the
//     codemodel's primary configuration; multi-config callers can
//     extend by passing TargetsByConfig and merging per-config
//     artifact paths.
//
//   - Per-FileSet install destinations
//     (install(TARGETS ... FILE_SET HEADERS DESTINATION ...)) — v1
//     uses the GNUInstallDirs default of "include/" for the prefix.
//     A FileSet that overrides this lands its headers at the wrong
//     path; a real fixture forcing the divergence drives the
//     per-FileSet-destination lookup as a follow-on.
//
//   - The convert action does NOT walk the materialized install
//     tree. Callers must NOT pass an installPrefix string —
//     BuildInputs derives every path string from codemodel
//     metadata alone. This is the hard architectural constraint
//     of Phase 6.
//
// Returns an EmitInputs ready to pass to EmitDeclarative. When the
// installer isn't Type=="export" or has no ExportTargets, returns a
// zero EmitInputs (caller short-circuits).
func BuildInputs(inst fileapi.DirectoryInstaller, targets map[string]fileapi.Target) EmitInputs {
	if inst.Type != "export" || len(inst.ExportTargets) == 0 {
		return EmitInputs{}
	}
	in := EmitInputs{
		Installer:     inst,
		Targets:       targets,
		PublicHeaders: map[string][]string{},
	}

	// Accumulate paths in a set so duplicate destinations across
	// targets (rare but possible — two libs landing in the same
	// directory) collapse to one entry. Sort at the end for
	// deterministic emit.
	installSet := map[string]bool{}
	bundleSet := map[string]bool{}

	// Per-target artifact path. First destination wins — see
	// docstring for the rationale.
	//
	// Name source: ExportTarget.Name is the bare target name (as
	// recorded in the schema docs), but the codemodel as of cmake
	// 3.20+ leaves the field empty and emits {"id": ..., "index":
	// ...} entries instead — the canonical name lives on
	// Target.Name (looked up by id). Treat the ExportTarget.Name
	// as a hint and fall back to Target.Name when empty.
	for _, et := range inst.ExportTargets {
		t, ok := targets[et.Id]
		if !ok {
			continue
		}
		name := et.Name
		if name == "" {
			name = t.Name
		}
		if name == "" {
			// Unrecoverable — without a name we can't
			// produce a stable Bazel target label. Skip.
			continue
		}
		// INTERFACE_LIBRARY has no NameOnDisk by construction —
		// header-only; the cc_library emitted by EmitDeclarative
		// doesn't need an artifact path.
		if t.Type == "INTERFACE_LIBRARY" {
			collectHeaders(in.PublicHeaders, name, t)
			continue
		}
		if t.NameOnDisk == "" || t.Install == nil || len(t.Install.Destinations) == 0 {
			// Defensive — the classifier should have rejected
			// any target whose install metadata is missing
			// before BuildInputs runs. Skip silently so the
			// emit still produces something useful for the
			// remaining targets.
			continue
		}
		dest := normalizeInstallDest(t.Install.Destinations[0].Path)
		if dest == "" && t.Install.Destinations[0].Path != "" {
			// Absolute / out-of-prefix destination — skip the
			// artifact path. EmitDeclarative's artifact-resolve
			// loop will fail to find this target and won't emit
			// a cc_import for it; the cmake_config_bundle and
			// headers filegroups still emit.
			continue
		}
		artifact := path.Join(dest, t.NameOnDisk)
		installSet[artifact] = true
		collectHeaders(in.PublicHeaders, name, t)
	}

	// Synthesize the export-script path. The install(EXPORT)
	// invocation produces this file at convert time conceptually;
	// at runtime under Bazel, the producer's converted BUILD rule
	// is responsible for generating it (or the orchestrator's
	// synthprefix bundle drops in a stub). Either way the path is
	// stable.
	if inst.Destination != "" && inst.ExportName != "" {
		bundle := path.Join(inst.Destination, inst.ExportName+".cmake")
		bundleSet[bundle] = true
		installSet[bundle] = true
		// The export script (<Pkg>Targets.cmake) alone is NOT found by
		// find_package(<Pkg> CONFIG): that searches the install tree for
		// <Pkg>Config.cmake as the entry point. cmake projects pair the
		// install(EXPORT) with a configure_package_config_file'd
		// <Pkg>Config.cmake (+ write_basic_package_version_file's
		// <Pkg>ConfigVersion.cmake), but those are install(FILES) of build-dir
		// outputs the converter doesn't lift — so they're dropped, leaving the
		// bundle unfindable (the eigen/catch2/zstd/… "…Config.cmake not
		// generated" survey gap). Generate the standard config-package pair
		// alongside the targets script (renderConfigFile include()s it;
		// renderConfigVersionFile is a permissive version stub) so the bundle is
		// actually consumable. Same dest, so CMAKE_CURRENT_LIST_DIR resolves the
		// sibling targets script.
		pkg := pkgFromBundle(inst.Destination, inst.ExportName)
		for _, f := range []string{pkg + "Config.cmake", pkg + "ConfigVersion.cmake"} {
			p := path.Join(inst.Destination, f)
			bundleSet[p] = true
			installSet[p] = true
		}
	}

	in.InstallFiles = sortedKeys(installSet)
	in.CMakeConfigBundleFiles = sortedKeys(bundleSet)
	return in
}

// collectHeaders walks t.FileSets where Type=="HEADERS" and
// Visibility ∈ {"PUBLIC", "INTERFACE"}, joining each matching
// TargetSource (FileSetIndex pointing back at the set) into
// "include/<rel>" under the GNUInstallDirs default include prefix.
//
// Stores the deduped, sorted slice in headers[name] when at least
// one header resolves. Targets without any PUBLIC/INTERFACE HEADERS
// FileSet leave the map entry unset — EmitDeclarative skips the
// filegroup for those.
func collectHeaders(headers map[string][]string, name string, t fileapi.Target) {
	if len(t.FileSets) == 0 {
		return
	}
	headerSetIdx := map[int]fileapi.TargetFileSet{}
	for i, fs := range t.FileSets {
		if fs.Type != "HEADERS" {
			continue
		}
		// Allowlist PUBLIC / INTERFACE explicitly so unknown
		// visibility values (future cmake additions) drop
		// conservatively rather than slipping through as
		// "exposed by default".
		if fs.Visibility != "PUBLIC" && fs.Visibility != "INTERFACE" {
			continue
		}
		headerSetIdx[i] = fs
	}
	if len(headerSetIdx) == 0 {
		return
	}
	seen := map[string]bool{}
	var hdrs []string
	// Project source root: TargetPaths.Source records the per-
	// target source dir. The codemodel's TargetSource.Path is
	// project-source-root-relative; the FileSet BaseDirectories
	// are absolute on the recording machine. Use Target.Paths.Source
	// as the root for the strip step (BaseDirectories absolute →
	// the strip walks them directly without needing the project
	// root).
	for _, src := range t.Sources {
		if src.FileSetIndex == nil {
			continue
		}
		fs, ok := headerSetIdx[*src.FileSetIndex]
		if !ok {
			continue
		}
		rel := stripFileSetBase(src.Path, fs.BaseDirectories, t.Paths.Source)
		if rel == "" {
			continue
		}
		hdrPath := path.Join("include", rel)
		if seen[hdrPath] {
			continue
		}
		seen[hdrPath] = true
		hdrs = append(hdrs, hdrPath)
	}
	if len(hdrs) > 0 {
		sort.Strings(hdrs)
		headers[name] = hdrs
	}
}

// stripFileSetBase computes the relative path of srcPath under the
// first BaseDirectory that contains it. srcPath is project-source-
// root-relative (TargetSource.Path); when resolving against an
// absolute BaseDirectory we join with srcRoot. Returns "" if no
// base directory contains the source.
//
// Implementation mirrors the equivalent helper in lower/execute_process_fallback.go;
// pulled into this package so exportshape doesn't depend on lower.
func stripFileSetBase(srcPath string, baseDirs []string, srcRoot string) string {
	if srcPath == "" {
		return ""
	}
	srcPathSlash := strings.ReplaceAll(srcPath, "\\", "/")
	// Convert relative srcPath to absolute candidate paths the
	// BaseDirectory comparison can match. cmake records both forms
	// (older minor versions emit relative; cmake 3.25+ tends to
	// emit absolute), so try the as-is path first, then a srcRoot-
	// joined absolute form.
	candidates := []string{srcPathSlash}
	if !path.IsAbs(srcPathSlash) && srcRoot != "" {
		candidates = append(candidates, path.Join(srcRoot, srcPathSlash))
	}
	for _, base := range baseDirs {
		baseSlash := strings.ReplaceAll(base, "\\", "/")
		// Normalize trailing slash so the prefix check doesn't
		// accept partial directory matches ("foo" matching
		// "foobar/x.h").
		baseSlash = strings.TrimRight(baseSlash, "/")
		if baseSlash == "" {
			continue
		}
		for _, cand := range candidates {
			if cand == baseSlash {
				// Source is the base directory itself — odd
				// but possible if cmake records the dir as a
				// "header". Use the basename.
				return path.Base(cand)
			}
			if strings.HasPrefix(cand, baseSlash+"/") {
				return strings.TrimPrefix(cand, baseSlash+"/")
			}
		}
	}
	return ""
}

// normalizeInstallDest cleans an install destination string:
//   - Absolute paths return "" (out-of-prefix; the cc_import
//     can't address them).
//   - "." / "" → empty (install at the prefix root).
//   - ".." or "../foo" → empty (escape attempt).
//   - everything else → path.Clean result.
//
// This matches the equivalent guard in
// lower/execute_process_fallback.go's installPathFor.
func normalizeInstallDest(dest string) string {
	if dest == "" {
		return ""
	}
	if path.IsAbs(dest) {
		return ""
	}
	cleaned := path.Clean(dest)
	if cleaned == "." {
		return ""
	}
	if cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return ""
	}
	return cleaned
}

func sortedKeys(set map[string]bool) []string {
	if len(set) == 0 {
		return nil
	}
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
