package lower

import (
	"encoding/json"
	"fmt"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/sstriker/buildstream-bazel/converter/internal/exportshape"
	"github.com/sstriker/buildstream-bazel/converter/internal/fileapi"
	"github.com/sstriker/buildstream-bazel/converter/ir"
)

// lowerDirectoryInstallers walks every Directory.Installers entry in
// the reply and emits KindPkgFiles IR targets for the install(FILES)
// and install(DIRECTORY) shapes:
//
//   - Type == "file" uses plain-string paths in
//     DirectoryInstaller.Paths. The listed files package under the
//     install DESTINATION (the pkg_files `prefix`).
//   - Type == "directory" uses {"from": ..., "to": ...} path
//     objects; install(DIRECTORY) effectively names a source
//     directory (`from`) and a destination subdirectory (`to`,
//     usually empty meaning "preserve hierarchy under DESTINATION").
//     The lift records the source-directory anchor as a pkg_files
//     src (a recursive filesystem walk would extend the converter's
//     I/O scope); pkg_files packages the directory's tree under the
//     DESTINATION prefix.
//
// Phase 1 slice 1b of the generator-parity uplift (ROADMAP.md):
// install(FILES)/install(DIRECTORY) lower to rules_pkg's pkg_files
// rather than a bare filegroup, so the install **destination** rides
// through as the `prefix` attribute and the converted shape is a real
// declarative packaging mapping (filegroup dropped the destination
// entirely). Keeping them off the round-2 install path: a declarative
// pkg_files target is the convert-time answer, not an opaque
// install-root extraction.
//
// install(TARGETS) (Type == "target") is already covered by the
// per-target Install slot in the codemodel.
//
// install(EXPORT) (Type == "export") is the Phase 6 classifier's
// surface — gated separately, still produces KindFilegroup
// cmake_config_bundle shapes via lowerExportInstallers.
//
// Grouping rule: one pkg_files per (destination) — files and
// directories sharing a destination consolidate into one target.
// The target name is `install_files__<sanitized destination>`
// for Type=="file" and `install_directory__<sanitized destination>`
// for Type=="directory". Empty / unsafe destinations are skipped so
// the conversion doesn't produce names that violate Bazel
// target-name rules.
//
// Per-file destination renames (cmake install(FILES ... RENAME ...))
// ARE modeled: the File API records a renamed file installer as a
// {"from","to"} object path (vs. the plain string of an un-renamed
// FILES installer), with "to" the destination name under DESTINATION.
// decodeInstallerPath surfaces it as instFile.rename and the lowering
// lifts it onto the pkg_files `renames` dict (dest relative to the
// prefix). Before this the object-form file entry was dropped entirely
// — the renamed file silently vanished from the package.
//
// The two install(DIRECTORY) shapes are also distinguished (see the
// PkgStripPrefix block below): trailing-slash "contents into dest"
// ({"from","to":"."} object) strips the whole source dir; no-trailing-
// slash "dir itself into dest" (plain string) strips only the dir's
// parent so the dir name survives under the prefix.
//
// Returns IR targets in deterministic order (sorted by target name)
// so downstream emit produces byte-stable BUILD output.
func lowerDirectoryInstallers(r *fileapi.Reply, emitConfig bool) []ir.Target {
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
	exportTargets := lowerExportInstallers(r, emitConfig)

	cmakeSrc := r.Codemodel.Paths.Source

	// Per-(type, destination) accumulators. Files / directories
	// stored in a map to dedupe (the same path can appear in
	// multiple installer entries under the same destination); the
	// map is then sorted on emit. The instFile value carries the
	// per-path metadata the File API surfaces beyond the source path:
	// a rename target (install(FILES ... RENAME ...)) and, for
	// directory installers, the contents-vs-tree mode.
	type bucket struct {
		kind  string // "file" or "directory"
		dest  string
		files map[string]instFile
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
				// install(TARGETS) (Type=="target") rides through the
				// per-target Install slot, not here — including the
				// versioned-shared-lib namelink split cmake records as
				// paired target installers (TargetInstallNamelink
				// "skip" for the real SONAME files, "only" for the
				// libfoo.so symlink). Bazel resolves shared-lib imports
				// by artifact (cc_import), not by SONAME symlink, so the
				// "only" namelink installer is intentionally not
				// reproduced; dropping it here is correct, not lossy.
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
				b = &bucket{kind: inst.Type, dest: inst.Destination, files: map[string]instFile{}}
				byKey[key] = b
			}
			for _, raw := range inst.Paths {
				rel, info, ok := decodeInstallerPath(raw, dirSrc, cmakeSrc, inst.Type)
				if !ok {
					continue
				}
				b.files[rel] = info
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

	// usedNames disambiguates target names that collide after
	// sanitizeDestination. Buckets are keyed by the RAW destination
	// (Type + dest), so two distinct destinations that sanitize to the
	// same target-name-safe string — grpc installs into both
	// `include/grpc` and `include/grpc++` (the `++` collapses to `_`),
	// and `include/grpc` vs `include/grpc/` differ only by a trailing
	// slash — would otherwise emit duplicate target names and Bazel
	// rejects the package. Keep the buckets distinct (their PkgPrefix /
	// files genuinely differ) but give the second-and-later collider a
	// numeric suffix. Deterministic because `keys` is sorted.
	usedNames := make(map[string]bool, len(keys))
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
		name := prefix + sanitizeDestination(b.dest)
		if usedNames[name] {
			base := name
			for n := 2; usedNames[name]; n++ {
				name = fmt.Sprintf("%s_%d", base, n)
			}
		}
		usedNames[name] = true
		t := ir.Target{
			Name: name,
			Kind: ir.KindPkgFiles,
			Srcs: files,
			// PkgPrefix is the install DESTINATION verbatim (e.g.
			// "lib", "include", "share/foo") — pkg_files renders it
			// as the prefix attribute so consumers reconstruct the
			// install layout.
			PkgPrefix:  b.dest,
			Visibility: publicVisibility(),
		}
		if b.kind == "file" {
			// install(FILES ... RENAME <to>): the File API records the
			// renamed entry as a {"from","to"} object, decoded into
			// instFile.rename (the destination name under DESTINATION).
			// Carry each as a rules_pkg `renames` entry (dest relative to
			// the prefix) so the file lands at "<dest>/<rename>" instead
			// of "<dest>/<basename>". Without this the object-form entry
			// used to be dropped entirely (the file silently vanished
			// from the package).
			renames := map[string]string{}
			for f, info := range b.files {
				if info.rename != "" {
					renames[f] = info.rename
				}
			}
			if len(renames) > 0 {
				t.PkgRenames = renames
			}
		}
		if b.kind == "directory" {
			// install(DIRECTORY <dir>/ DESTINATION <dest>) names a
			// SOURCE DIRECTORY whose whole tree is packaged. A bare
			// directory in pkg_files `srcs` does NOT package its files
			// — a consuming pkg_tar fails with IsADirectoryError and
			// the tar carries only the empty dir entry. So we glob the
			// directory's contents (the emitter renders
			// `srcs = glob(["<dir>/**"])`) and strip the source dir so
			// each file lands at "<dest>/<rel>" rather than
			// "<dest>/<dir>/<rel>".
			//
			// strip_prefix: rules_pkg's strip_prefix.from_pkg("<dir>")
			// strips up to the current package plus "<dir>", which is
			// exactly the trailing-slash "contents of <dir>/ into
			// DESTINATION" cmake semantic. Verified under real bazel +
			// rules_pkg 1.0.1: glob(["include/**"]) +
			// strip_prefix.from_pkg("include") + prefix="include"
			// packages include/foo.h at include/foo.h (not a bare
			// include/ entry, not include/include/foo.h).
			//
			// We use the single source dir's path as the strip prefix.
			// When a single DESTINATION bucket aggregates more than one
			// source directory (cmake install(DIRECTORY a b
			// DESTINATION d) — rare), a single strip_prefix can't
			// flatten each independently; we fall back to no
			// strip_prefix so files keep their dir-qualified paths
			// under the prefix (the conservative, never-wrong shape).
			//
			// cmake's two install(DIRECTORY) shapes are distinguished
			// by the File API path encoding and modeled separately:
			//   - trailing slash (install(DIRECTORY inc/ DESTINATION d))
			//     = "contents of inc/ into d" → recorded as the
			//     {"from":"inc","to":"."} object (instFile.dirTree
			//     false). Strip the whole dir so files land at
			//     "d/<rel>".
			//   - no trailing slash (install(DIRECTORY inc DESTINATION d))
			//     = "the inc dir itself into d" → recorded as a plain
			//     string (instFile.dirTree true). Strip the dir's PARENT
			//     so the dir name survives: files land at "d/inc/<rel>".
			//     A parentless dir (sits at the package root) keeps its
			//     name with no strip at all.
			// A single strip_prefix can only flatten one source dir, so
			// when a DESTINATION bucket aggregates more than one source
			// directory (rare) we emit no strip_prefix (files keep their
			// dir-qualified paths under the prefix — the never-wrong
			// conservative shape).
			t.PkgSrcsGlob = true
			if len(files) == 1 {
				if b.files[files[0]].dirTree {
					if parent := path.Dir(files[0]); parent != "." && parent != "/" {
						t.PkgStripPrefix = parent
					}
				} else {
					t.PkgStripPrefix = files[0]
				}
			}
		}
		out = append(out, t)
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
func lowerExportInstallers(r *fileapi.Reply, emitConfig bool) []ir.Target {
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
			in.EmitConfig = emitConfig
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

// instFile is the per-path install metadata the File API surfaces
// beyond the bare source path.
type instFile struct {
	// rename is the destination name under the install DESTINATION for
	// a renamed install(FILES ... RENAME ...) entry (the File API's
	// {"from","to"} object "to" field), relative to the pkg_files
	// prefix. Empty for un-renamed files and for directory installers.
	rename string
	// dirTree distinguishes the two install(DIRECTORY) shapes:
	//   - false (the {"from","to":"."} object) = trailing-slash
	//     "contents of <dir>/ into DESTINATION".
	//   - true (the plain-string form) = no-trailing-slash "the <dir>
	//     itself into DESTINATION" (dir name preserved under dest).
	// Meaningless for file installers.
	dirTree bool
}

// decodeInstallerPath decodes one DirectoryInstaller.Paths entry
// according to the installer type's path encoding, returning the
// source-tree-relative path plus its instFile metadata. The File API
// uses two encodings:
//
//   - plain JSON string — an un-renamed install(FILES) file, or the
//     no-trailing-slash install(DIRECTORY) "dir itself into dest"
//     shape (instFile.dirTree set for directory type).
//
//   - JSON object {"from": "...", "to": "..."} — a renamed
//     install(FILES ... RENAME ...) file (instFile.rename = "to"), or
//     the trailing-slash install(DIRECTORY) "contents into dest" shape
//     ("to":".", instFile.dirTree false).
//
// ok is false when the entry can't be decoded or resolves outside the
// source tree.
func decodeInstallerPath(raw json.RawMessage, dirSrc, cmakeSrc, instType string) (rel string, info instFile, ok bool) {
	// Try plain string first (un-renamed file installer; directory
	// installer's no-trailing-slash short form).
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		if s == "" {
			return "", instFile{}, false
		}
		rel = projectToSourceRoot(s, dirSrc, cmakeSrc)
		if rel == "" {
			return "", instFile{}, false
		}
		if instType == "directory" {
			// Plain string for a directory installer = the
			// no-trailing-slash "dir itself into DESTINATION" shape.
			info.dirTree = true
		}
		return rel, info, true
	}
	// Object form: {"from": "...", "to": "..."}.
	var obj struct {
		From string `json:"from"`
		To   string `json:"to"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil || obj.From == "" {
		return "", instFile{}, false
	}
	rel = projectToSourceRoot(obj.From, dirSrc, cmakeSrc)
	if rel == "" {
		return "", instFile{}, false
	}
	if instType == "file" {
		// File object form only appears for install(FILES ... RENAME
		// <to>): "to" is the destination name under DESTINATION.
		// (Without this the entry was previously dropped — the renamed
		// file silently vanished from the package.)
		info.rename = obj.To
	}
	// For directory installers the object form ("to":".") is the
	// trailing-slash contents-into-dest shape: dirTree stays false.
	return rel, info, true
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
