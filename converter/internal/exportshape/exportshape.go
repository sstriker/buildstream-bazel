// Package exportshape classifies cmake install(EXPORT) bundles into
// "declarative" (safe to pre-resolve at convert time) vs
// "imperative" (needs the round-2 fallback).
//
// Phase 6 of the generator-parity uplift (ROADMAP.md) uses this
// classifier to gate convert-time emission of cc_import + pkg_files
// targets that mirror the bundle a consumer's find_package would
// otherwise pull in. The non-declarative residue stays on the
// existing round-2 pick_file-projection fallback.
//
// "Declarative" here means the bundle is expected to be the canonical
// CMakePackageConfigHelpers::configure_package_config_file shape:
// a generated <Pkg>Config.cmake that includes a sibling
// <Pkg>Targets.cmake and does no host-aware branching. The actual
// bundle contents don't exist at convert time (cmake --install writes
// them later); the classifier inspects the install(EXPORT) call
// metadata available in directory-*.json and infers whether running
// the install would produce a declarative bundle.
package exportshape

import (
	"path"
	"strings"

	"github.com/sstriker/buildstream-bazel/converter/internal/fileapi"
)

// Verdict is the classifier's per-installer result.
type Verdict struct {
	// Declarative is true when the install(EXPORT) call shape
	// suggests CMakePackageConfigHelpers-style bundle output —
	// safe to pre-resolve at convert time via cc_import + pkg_files.
	Declarative bool

	// Reasons records the human-readable rationale for the
	// verdict. Empty for the declarative path; non-empty for
	// imperative, with one entry per failed precondition. Used
	// by audit gates so operators see WHY a specific bundle
	// stayed on the round-2 path.
	Reasons []string
}

// CanonicalDestinations is the set of destination-prefix patterns
// install(EXPORT DESTINATION ...) uses when the operator follows
// the CMakePackageConfigHelpers / GNUInstallDirs convention. The
// classifier rejects any other destination as imperative on the
// theory that a non-canonical layout means a non-canonical bundle.
//
// Each entry is a prefix pattern: the destination must equal one
// of these literally, or be a sub-path under one (e.g.
// "lib/cmake/MyPkg" matches the "lib/cmake/" prefix).
var CanonicalDestinations = []string{
	"lib/cmake/",
	"lib64/cmake/",
	"share/cmake/",
	"share/",
}

// supportedExportArtifactTypes lists the cmake target types whose
// install(EXPORT) artifact maps cleanly to Bazel-side cc_import +
// cc_library. EXECUTABLE is excluded — cc_import doesn't model
// executables, and shipping them through a Bazel-side stub needs
// sh_binary, which the classifier currently leaves on the
// imperative path.
var supportedExportArtifactTypes = map[string]bool{
	"STATIC_LIBRARY":    true,
	"SHARED_LIBRARY":    true,
	"INTERFACE_LIBRARY": true,
	"MODULE_LIBRARY":    true,
}

// Classify decides whether a Type=="export" DirectoryInstaller is
// safe to pre-resolve at convert time. Targets is the
// fileapi.Reply.Targets map (target id → Target) — used to look up
// each export target's TYPE.
//
// For non-export installers the verdict is always
// {Declarative: false, Reasons: ["not an export installer"]} — the
// classifier is scoped to install(EXPORT) and returns early on
// unrelated installers so callers can pass any installer without
// pre-filtering.
func Classify(inst fileapi.DirectoryInstaller, targets map[string]fileapi.Target) Verdict {
	if inst.Type != "export" {
		return Verdict{Reasons: []string{"not an export installer"}}
	}

	var reasons []string

	// Reject EXCLUDE_FROM_ALL: the bundle isn't part of the
	// default install — the operator is doing component-scoped
	// shipping that this classifier doesn't model.
	if inst.IsExcludeFromAll {
		reasons = append(reasons, "installer is EXCLUDE_FROM_ALL")
	}

	// Reject OPTIONAL: same logic — downstream can't rely on the
	// bundle being there.
	if inst.IsOptional {
		reasons = append(reasons, "installer is OPTIONAL")
	}

	// Destination must match the canonical layouts.
	if !isCanonicalDestination(inst.Destination) {
		reasons = append(reasons, "destination "+inst.Destination+" is not canonical (lib/cmake/, share/cmake/, share/)")
	}

	// ExportName must be set (a missing name signals an
	// unrecognized export shape).
	if inst.ExportName == "" {
		reasons = append(reasons, "exportName is empty")
	}

	// ExportTargets must be non-empty.
	if len(inst.ExportTargets) == 0 {
		reasons = append(reasons, "exportTargets is empty")
	}

	// Every exported target's TYPE must be in the supported set.
	// EXECUTABLE in particular requires sh_binary shaping the
	// classifier doesn't currently emit.
	for _, et := range inst.ExportTargets {
		t, ok := targets[et.Id]
		if !ok {
			reasons = append(reasons, "target "+et.Name+" (id "+et.Id+") not in codemodel")
			continue
		}
		if !supportedExportArtifactTypes[t.Type] {
			reasons = append(reasons, "target "+et.Name+" has unsupported type "+t.Type)
		}
	}

	if len(reasons) > 0 {
		return Verdict{Reasons: reasons}
	}
	return Verdict{Declarative: true}
}

// isCanonicalDestination returns true when dest matches one of the
// CanonicalDestinations prefixes literally or as a parent.
//
// path.Clean normalises trailing slashes; the comparison is on the
// canonical form. cmake itself uses POSIX-style slashes in
// destinations regardless of the host platform, so we use
// path.Clean (POSIX) rather than filepath.Clean.
func isCanonicalDestination(dest string) bool {
	if dest == "" {
		return false
	}
	clean := path.Clean(dest)
	for _, prefix := range CanonicalDestinations {
		// Exact match (the operator wrote DESTINATION lib/cmake).
		if clean == strings.TrimSuffix(prefix, "/") {
			return true
		}
		// Sub-path (the operator wrote DESTINATION lib/cmake/MyPkg).
		if strings.HasPrefix(clean+"/", prefix) {
			return true
		}
	}
	return false
}
