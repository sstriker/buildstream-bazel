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
// shape — the simplest declarative install pattern with a known
// path schema (plain strings in DirectoryInstaller.Paths).
//
// install(DIRECTORY) (Type == "directory") uses a different path
// schema ({"from": ..., "to": ...} objects); handled in a follow-on
// slice once a render-gate fixture exercises it.
//
// install(TARGETS) (Type == "target") is already covered by the
// per-target Install slot in the codemodel.
//
// install(EXPORT) (Type == "export") is the Phase 6 classifier's
// surface — gated separately.
//
// Phase 1 task 2 of the generator-parity uplift (ROADMAP.md).
//
// Grouping rule: one filegroup per (destination) — files sharing a
// destination consolidate into one target. The filegroup name is
// `install_files__<sanitized destination>` where sanitization maps
// `/`, `.`, and other path-unsafe characters to `_`. Empty / unsafe
// destinations are skipped so the conversion doesn't produce names
// that violate Bazel target-name rules.
//
// Returns IR targets in deterministic order (sorted by filegroup
// name) so downstream emit produces byte-stable BUILD output.
func lowerDirectoryInstallers(r *fileapi.Reply) []ir.Target {
	if r == nil || len(r.Directories) == 0 {
		return nil
	}
	cmakeSrc := r.Codemodel.Paths.Source

	// Per-destination accumulators. Files are stored in a map to
	// dedupe (the same path can appear in multiple installer entries
	// under the same destination); the map is then sorted on emit.
	type bucket struct {
		dest  string
		files map[string]bool
	}
	byDest := map[string]*bucket{}

	for _, dir := range r.Directories {
		dirSrc := dir.Paths.Source
		if dirSrc == "" {
			dirSrc = cmakeSrc
		} else if !filepath.IsAbs(dirSrc) {
			dirSrc = filepath.Join(cmakeSrc, dirSrc)
		}
		for _, inst := range dir.Installers {
			if inst.Type != "file" {
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
			b, ok := byDest[inst.Destination]
			if !ok {
				b = &bucket{dest: inst.Destination, files: map[string]bool{}}
				byDest[inst.Destination] = b
			}
			for _, raw := range inst.Paths {
				var p string
				if err := json.Unmarshal(raw, &p); err != nil {
					// Not a string — typically an
					// {"from": ..., "to": ...} object from a
					// type="directory" installer that somehow
					// landed under "file". Skip silently.
					continue
				}
				if p == "" {
					continue
				}
				// Resolve to source-tree relative path so the
				// emitted filegroup carries a path the
				// downstream Bazel package can address. Absolute
				// paths outside the source tree are skipped —
				// they'd produce a Bazel label that doesn't
				// resolve.
				rel := projectToSourceRoot(p, dirSrc, cmakeSrc)
				if rel == "" {
					continue
				}
				b.files[rel] = true
			}
		}
	}

	if len(byDest) == 0 {
		return nil
	}

	// Materialize: stable target order = sorted destination.
	dests := make([]string, 0, len(byDest))
	for d := range byDest {
		dests = append(dests, d)
	}
	sort.Strings(dests)

	out := make([]ir.Target, 0, len(dests))
	for _, dest := range dests {
		b := byDest[dest]
		if len(b.files) == 0 {
			continue
		}
		files := make([]string, 0, len(b.files))
		for f := range b.files {
			files = append(files, f)
		}
		sort.Strings(files)
		out = append(out, ir.Target{
			Name:       "install_files__" + sanitizeDestination(dest),
			Kind:       ir.KindFilegroup,
			Srcs:       files,
			Visibility: []string{"//visibility:public"},
		})
	}
	return out
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
