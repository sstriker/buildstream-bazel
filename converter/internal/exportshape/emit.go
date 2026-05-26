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
//   - One filegroup named "cmake_config_bundle" if
//     CMakeConfigBundleFiles is non-empty — the generated
//     <Pkg>Targets.cmake + <Pkg>ConfigVersion.cmake + Config helper.
//     Lets downstream consumers depend on the bundle as a single
//     label.
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
	for _, et := range in.Installer.ExportTargets {
		t, ok := in.Targets[et.Id]
		if !ok {
			continue
		}
		switch t.Type {
		case "STATIC_LIBRARY", "SHARED_LIBRARY", "MODULE_LIBRARY":
			tgt := emitCCImport(et.Name, t, in.InstallFiles)
			if tgt.Name != "" {
				out = append(out, tgt)
			}
		case "INTERFACE_LIBRARY":
			tgt := emitCCInterface(et.Name)
			out = append(out, tgt)
		}
		// Headers per target, when populated.
		if hdrs := in.PublicHeaders[et.Name]; len(hdrs) > 0 {
			sortedHdrs := append([]string(nil), hdrs...)
			sort.Strings(sortedHdrs)
			out = append(out, ir.Target{
				Name:       et.Name + "_hdrs",
				Kind:       ir.KindFilegroup,
				Srcs:       sortedHdrs,
				Visibility: []string{"//visibility:public"},
			})
		}
	}

	// One bundle filegroup if cmake-config files are staged.
	if len(in.CMakeConfigBundleFiles) > 0 {
		bundle := append([]string(nil), in.CMakeConfigBundleFiles...)
		sort.Strings(bundle)
		out = append(out, ir.Target{
			Name:       "cmake_config_bundle",
			Kind:       ir.KindFilegroup,
			Srcs:       bundle,
			Visibility: []string{"//visibility:public"},
		})
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
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
