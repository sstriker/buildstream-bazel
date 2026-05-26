package lower

import (
	"encoding/json"
	"path/filepath"
	"sort"
	"strings"

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

	if len(byKey) == 0 {
		return nil
	}

	// Materialize: stable target order = sorted target name (which
	// embeds both kind and destination).
	keys := make([]string, 0, len(byKey))
	for k := range byKey {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	out := make([]ir.Target, 0, len(keys))
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
// p may be absolute (typically when cmake recorded an out-of-tree
// path) or relative to dirSrc (cmake's per-directory source path).
// Returns "" when p resolves to a location outside cmakeSrc — those
// can't be addressed as a Bazel label in the converted package.
func projectToSourceRoot(p, dirSrc, cmakeSrc string) string {
	abs := p
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(dirSrc, p)
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
