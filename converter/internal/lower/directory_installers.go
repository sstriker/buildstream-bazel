package lower

import (
	"encoding/json"
	"path/filepath"
	"sort"
	"strings"

	"github.com/sstriker/buildstream-bazel/converter/internal/exportshape"
	"github.com/sstriker/buildstream-bazel/converter/internal/fileapi"
	"github.com/sstriker/buildstream-bazel/converter/ir"
)

// lowerDirectoryInstallers walks every Directory.Installers entry in
// the reply and emits KindFilegroup IR targets for the install(FILES)
// and install(DIRECTORY) shapes:
//
//   - Type == "file" uses plain-string paths in
//     DirectoryInstaller.Paths.
//   - Type == "directory" uses {"from": ..., "to": ...} path
//     objects; install(DIRECTORY) effectively names a source
//     directory (`from`) and a destination subdirectory (`to`,
//     usually empty meaning "preserve hierarchy under DESTINATION").
//     The lift expands the source directory's contents — recursive
//     filesystem walk would extend the converter's I/O scope, so v1
//     records ONLY the source-directory anchor as a filegroup src
//     (Bazel can resolve via glob in a downstream wrapper, or the
//     operator can opt into rules_pkg's pkg_files for richer
//     handling).
//
// install(TARGETS) (Type == "target") is already covered by the
// per-target Install slot in the codemodel.
//
// install(EXPORT) (Type == "export") is the Phase 6 classifier's
// surface — gated separately.
//
// Phase 1 task 2 of the generator-parity uplift (ROADMAP.md).
//
// Grouping rule: one filegroup per (destination) — files and
// directories sharing a destination consolidate into one target.
// The filegroup name is `install_files__<sanitized destination>`
// for Type=="file" and `install_directory__<sanitized destination>`
// for Type=="directory". Empty / unsafe destinations are skipped so
// the conversion doesn't produce names that violate Bazel
// target-name rules.
//
// Returns IR targets in deterministic order (sorted by filegroup
// name) so downstream emit produces byte-stable BUILD output.
func lowerDirectoryInstallers(r *fileapi.Reply) []ir.Target {
	if r == nil || len(r.Directories) == 0 {
		return nil
	}
	// Phase 6: declarative install(EXPORT) projection. Walk each
	// "export"-type installer through the classifier; declarative
	// verdicts produce cc_import + cmake_config_bundle filegroup
	// + per-target headers via exportshape.EmitDeclarative.
	// Imperative installers fall through to the round-2
	// pick_file-projection fallback unchanged (no IR emission
	// here for them — they're outside this function's surface).
	exportTargets := lowerExportInstallers(r)

	cmakeSrc := r.Codemodel.Paths.Source

	// Per-(type, destination) accumulators. Files / directories
	// stored in a map to dedupe (the same path can appear in
	// multiple installer entries under the same destination); the
	// map is then sorted on emit.
	type bucket struct {
		kind  string // "file" or "directory"
		dest  string
		files map[string]bool
	}
	byKey := map[string]*bucket{}

	for _, dir := range r.Directories {
		dirSrc := dir.Paths.Source
		if dirSrc == "" {
			dirSrc = cmakeSrc
		} else if !filepath.IsAbs(dirSrc) {
			dirSrc = filepath.Join(cmakeSrc, dirSrc)
		}
		for _, inst := range dir.Installers {
			if inst.Type != "file" && inst.Type != "directory" {
				continue
			}
			if inst.Destination == "" {
				continue
			}
			// EXCLUDE_FROM_ALL / OPTIONAL installers don't fire
			// under the default install — skip them so the emitted
			// filegroup doesn't promise files that may not exist.
			if inst.IsExcludeFromAll || inst.IsOptional {
				continue
			}
			key := inst.Type + "\x00" + inst.Destination
			b, ok := byKey[key]
			if !ok {
				b = &bucket{kind: inst.Type, dest: inst.Destination, files: map[string]bool{}}
				byKey[key] = b
			}
			for _, raw := range inst.Paths {
				rel := decodeInstallerPath(raw, dirSrc, cmakeSrc, inst.Type)
				if rel == "" {
					continue
				}
				b.files[rel] = true
			}
		}
	}

	if len(byKey) == 0 && len(exportTargets) == 0 {
		return nil
	}

	// Materialize: stable target order = sorted target name (which
	// embeds both kind and destination).
	keys := make([]string, 0, len(byKey))
	for k := range byKey {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	out := make([]ir.Target, 0, len(keys)+len(exportTargets))
	for _, key := range keys {
		b := byKey[key]
		if len(b.files) == 0 {
			continue
		}
		files := make([]string, 0, len(b.files))
		for f := range b.files {
			files = append(files, f)
		}
		sort.Strings(files)
		prefix := "install_files__"
		if b.kind == "directory" {
			prefix = "install_directory__"
		}
		out = append(out, ir.Target{
			Name:       prefix + sanitizeDestination(b.dest),
			Kind:       ir.KindFilegroup,
			Srcs:       files,
			Visibility: []string{"//visibility:public"},
		})
	}
	// Append declarative install(EXPORT) IR after the file /
	// directory filegroups. The slot ordering is cosmetic — the
	// downstream emit sorts by name within each chunk; appending
	// here just gives the IR-rendered BUILD a consistent grouping
	// (install-files / install-directory / export-import).
	out = append(out, exportTargets...)
	return out
}

// lowerExportInstallers walks every Type=="export" DirectoryInstaller
// in the reply, runs the Phase 6 classifier on each, and projects
// declarative verdicts into IR via exportshape.BuildInputs +
// EmitDeclarative.
//
// Per-target cc_import names carry an "_import" suffix to avoid
// colliding with the corresponding cc_library the lowerTarget pass
// emits for the same source target — install(TARGETS foo EXPORT ...)
// declares the same `foo` target that the project authored, so the
// IR carries both: the producer's `foo` cc_library that compiles +
// links the artifact, and a sibling `foo_import` cc_import that
// addresses the artifact's install-relative path for cross-element
// consumers who'd otherwise rely on find_package(Pkg) finding a
// pre-built. The headers filegroup keeps the bare "<name>_hdrs"
// shape exportshape.EmitDeclarative produces — no collision with
// anything else the lower pass emits.
//
// Imperative installers (Verdict.Declarative == false) fall through
// unchanged — they stay on the existing round-2
// pick_file-projection fallback the meson lowering uses.
//
// Phase 6 of the generator-parity uplift (ROADMAP.md). The slice
// projecting from codemodel-only sources — Target.Artifacts /
// Target.Install.Destinations / Target.FileSets HEADERS — without
// running `cmake --install` at convert time.
func lowerExportInstallers(r *fileapi.Reply) []ir.Target {
	if r == nil || len(r.Directories) == 0 {
		return nil
	}
	// Per-target-name dedup so multiple install(EXPORT) calls in
	// the same package (rare but legal — a project may export
	// separate subsets to different config files) don't collide
	// on shared Bazel target names. Specifically: every
	// declarative install(EXPORT) emits a "cmake_config_bundle"
	// filegroup; if we appended raw outputs from N installers,
	// Bazel would fail at load time with "Target
	// 'cmake_config_bundle' already declared". The merge here
	// unions Srcs of same-named filegroups; cc_import targets
	// don't collide post-suffix since each carries its own
	// export's target name.
	byName := map[string]*ir.Target{}
	var order []string
	merge := func(t ir.Target) {
		if existing, ok := byName[t.Name]; ok {
			if t.Kind == ir.KindFilegroup && existing.Kind == ir.KindFilegroup {
				seen := map[string]bool{}
				for _, s := range existing.Srcs {
					seen[s] = true
				}
				for _, s := range t.Srcs {
					if !seen[s] {
						existing.Srcs = append(existing.Srcs, s)
						seen[s] = true
					}
				}
				sort.Strings(existing.Srcs)
			}
			// For non-filegroup collisions (shouldn't occur
			// post-suffix), the first-write-wins below.
			return
		}
		tt := t
		byName[t.Name] = &tt
		order = append(order, t.Name)
	}

	// Walk every directory's installers. install(EXPORT)
	// invocations show up as Type=="export" entries.
	for _, dir := range r.Directories {
		for _, inst := range dir.Installers {
			if inst.Type != "export" {
				continue
			}
			verdict := exportshape.Classify(inst, r.Targets)
			if !verdict.Declarative {
				// Imperative bundle: stays on the round-2
				// pick_file-projection fallback. Phase 6
				// scope deliberately doesn't touch that path
				// — it's the safety net for cmake projects
				// whose install(EXPORT) shape this classifier
				// doesn't model.
				continue
			}
			in := exportshape.BuildInputs(inst, r.Targets)
			declarative := exportshape.EmitDeclarative(in)
			// Suffix per-target cc_import / cc_library names with
			// "_import" to avoid colliding with the cc_library
			// the producer's lowerTarget pass emits for the same
			// source target. install(TARGETS foo EXPORT …)
			// declares the same `foo` target the project
			// authored — both shapes coexist in the producer's
			// BUILD package: `foo` is the producer-internal
			// build rule, `foo_import` is the cross-element
			// import facade. Headers / bundle filegroups already
			// have collision-free names ("<name>_hdrs",
			// "cmake_config_bundle") so leave those alone.
			for i := range declarative {
				if declarative[i].Kind == ir.KindCCImport ||
					declarative[i].Kind == ir.KindCCInterface {
					declarative[i].Name += "_import"
					// Tag with the Phase 6 export-derived
					// marker so downstream consumers
					// (cmakecfg's bundle synthesis) can
					// filter these out — the cc_import
					// here is a Bazel-side facade for the
					// producer's own cc_library; the cmake-
					// config bundle that cmakecfg emits
					// already carries the IMPORTED target
					// for the underlying cc_library and
					// surfacing this sibling would
					// duplicate it.
					declarative[i].Tags = appendTag(declarative[i].Tags, "cmake-codegen-install-export-import")
				}
				merge(declarative[i])
			}
		}
	}
	if len(order) == 0 {
		return nil
	}
	sort.Strings(order)
	out := make([]ir.Target, 0, len(order))
	for _, name := range order {
		out = append(out, *byName[name])
	}
	return out
}

// decodeInstallerPath decodes one DirectoryInstaller.Paths entry
// according to the installer type's expected schema:
//
//   - Type=="file" — plain JSON string. Resolved against dirSrc /
//     cmakeSrc the same way as before.
//
//   - Type=="directory" — JSON object {"from": "...", "to": "..."}.
//     We record the "from" path as the filegroup src (the source
//     directory whose contents install copies). cmake's
//     install(DIRECTORY) also accepts the plain-string short form
//     when "to" is empty / DESTINATION-implicit; the function
//     handles both forms via json.RawMessage probing.
//
// Returns "" when the entry can't be decoded or resolves outside
// the source tree.
func decodeInstallerPath(raw json.RawMessage, dirSrc, cmakeSrc, instType string) string {
	// Try plain string first (file installer's only shape, and
	// directory installer's short form).
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		if s == "" {
			return ""
		}
		return projectToSourceRoot(s, dirSrc, cmakeSrc)
	}
	if instType != "directory" {
		// File installer can't carry an object-shape path.
		return ""
	}
	// Object form: {"from": "...", "to": "..."}.
	var obj struct {
		From string `json:"from"`
		To   string `json:"to"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil {
		return ""
	}
	if obj.From == "" {
		return ""
	}
	return projectToSourceRoot(obj.From, dirSrc, cmakeSrc)
}

// projectToSourceRoot returns the source-tree-relative form of p.
//
// cmake's File API records installer paths (`paths[].from` for both
// FILES and DIRECTORY installer types) as **top-level-source-relative**,
// not per-directory-source-relative. Joining with `dirSrc` —
// cmake's per-directory CMakeLists location — double-prefixes the
// path when the install() call lives in a subdirectory's
// CMakeLists.txt:
//
//	dirSrc = "googletest"
//	p      = "googletest/include"  (cmake-recorded form)
//	wrong  = "googletest/googletest/include"   ← old behaviour
//	right  = "googletest/include"               ← top-level-relative
//
// Same bug surfaced in LLVM (`tools/lto/include/llvm-c/lto.h`
// instead of `include/llvm-c/lto.h`). The fmt + json + zlib +
// libpng surveys masked it because every project's `install()`
// directives live in the top-level CMakeLists where dirSrc = ".".
//
// p may be absolute (typically when cmake recorded an out-of-tree
// path); those resolve via filepath.Rel against cmakeSrc unchanged.
// Returns "" when p resolves to a location outside cmakeSrc —
// those can't be addressed as a Bazel label in the converted
// package.
//
// dirSrc is kept in the signature for back-compat with existing
// callers (the value is no longer consulted).
func projectToSourceRoot(p, dirSrc, cmakeSrc string) string {
	_ = dirSrc
	abs := p
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(cmakeSrc, p)
	}
	abs = filepath.Clean(abs)
	rel, err := filepath.Rel(cmakeSrc, abs)
	if err != nil {
		return ""
	}
	// filepath.Rel returns "../..." when abs is outside cmakeSrc;
	// reject those — Bazel labels can't traverse up the source root.
	if strings.HasPrefix(rel, "..") {
		return ""
	}
	return filepath.ToSlash(rel)
}

// appendTag appends tag to tags only when not already present.
// Preserves the tag-list's existing order so callers can rely on
// stable tag ordering for golden output.
func appendTag(tags []string, tag string) []string {
	for _, t := range tags {
		if t == tag {
			return tags
		}
	}
	return append(tags, tag)
}

// sanitizeDestination converts an install destination like
// "lib/cmake/MyPkg" or "include/foo/bar" into a Bazel target-name-
// safe suffix (alphanumeric + underscore). Replaces "/", ".", "-",
// and other path separators with underscores; collapses runs of
// underscores to one; trims leading/trailing underscores.
func sanitizeDestination(dest string) string {
	clean := filepath.ToSlash(filepath.Clean(dest))
	var sb strings.Builder
	sb.Grow(len(clean))
	lastWasUnderscore := false
	for _, r := range clean {
		isAlnum := (r >= 'a' && r <= 'z') ||
			(r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9')
		if isAlnum {
			sb.WriteRune(r)
			lastWasUnderscore = false
			continue
		}
		if !lastWasUnderscore {
			sb.WriteRune('_')
			lastWasUnderscore = true
		}
	}
	out := sb.String()
	out = strings.Trim(out, "_")
	return out
}
