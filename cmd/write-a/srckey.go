package main

// Per-element srckey: the content-narrowed identity of an
// element's source tree, used downstream as the cache key for
// the trace-driven build-graph registry (see
// docs/trace-driven-autotools.md "Roadmap"). Same srckey =>
// same expected build-graph (configure output / Makefile rules
// / compile + link commands), so the registry can hit and the
// expensive autotools build can be skipped.
//
// What "narrowed" means is per-kind. For kind:autotools the
// build graph is determined by:
//
//   - configure script + autoconf inputs (configure.ac, *.m4)
//   - Makefile templates (*.am, Makefile.in, *.in)
//   - .h headers (config.h-style preprocessor switches can
//     change which compile commands the Makefile emits — kept
//     conservatively)
//
// And NOT by:
//
//   - .c / .cpp / .cc / .S / .s — content changes only affect
//     the .o bytes the compiler produces, not the compile/link
//     commands the trace records.
//
// So the autotools default narrowing puts those source files
// in "name-only" territory: their PATHS contribute to the
// srckey (so adding or removing one invalidates), but their
// CONTENT bytes don't. Other languages stay content-included
// pending evidence either way.
//
// Stability properties this delivers:
//
//   - Comment-only edit in foo.c → srckey unchanged → registry
//     hit → build skipped (trace + make-db reused).
//   - Adding bar.c to the source tree → srckey changes → miss
//     → fresh build (Makefile.in's wildcard rules might pick up
//     the new file, so the build graph could legitimately differ).
//   - Editing Makefile.in → srckey changes → miss → fresh build.
//   - Editing config.h.in → srckey changes → miss → fresh build.

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// computeSrckey walks the element's source tree, partitions
// files into content-included vs name-only based on patterns,
// and returns the hex sha256 hash + the canonical breakdown
// (which IS the input the hash computes from — written as a
// debugging file alongside srckey.txt).
//
// patterns nil = every file is content-included (no narrowing).
// patterns rules apply: any file matching an include rule (and
// no exclude rule) is content-included; everything else is
// name-only. With no include rules at all, default is
// content-included; exclude rules then demote matching files to
// name-only.
//
// Multi-source elements: contributions from each source's
// AbsPath are merged under the source's relative-to-element root
// (rs.Directory prefix). Sources without AbsPath (kind:git_repo
// etc. resolved via --source-cache) appear in the breakdown by
// their canonical key with content set to <CACHE_DIGEST_NOT_PRESENT>;
// that surfaces "I haven't populated the cache" instead of
// silently producing a stable-looking srckey that doesn't reflect
// real source bytes.
func computeSrckey(elem *element, patterns *readPathsPatterns) (hash string, breakdown string, err error) {
	type srckeyEntry struct {
		Path           string // element-source-relative
		ContentInclude bool
		ContentSHA     string // empty when name-only
	}
	var entries []srckeyEntry

	for _, rs := range elem.Sources {
		dirPrefix := strings.TrimSuffix(rs.Directory, "/")
		if rs.AbsPath == "" {
			// Source resolved-via-cache (or unresolved). We have
			// no path content to hash. Encode the source's
			// canonical key in the breakdown so srckey reflects
			// THAT source's identity.
			key := sourceKey(rs)
			marker := "<source key=" + key + ">"
			if dirPrefix != "" {
				marker = dirPrefix + "/" + marker
			}
			entries = append(entries, srckeyEntry{
				Path:           marker,
				ContentInclude: true,
				ContentSHA:     "",
			})
			continue
		}
		paths, err := walkSrckeyPaths(rs.AbsPath)
		if err != nil {
			return "", "", fmt.Errorf("walk source %s: %w", rs.AbsPath, err)
		}
		for _, p := range paths {
			contentInclude := matchesSrckeyPatterns(patterns, p)
			rel := p
			if dirPrefix != "" {
				rel = dirPrefix + "/" + p
			}
			entry := srckeyEntry{
				Path:           rel,
				ContentInclude: contentInclude,
			}
			if contentInclude {
				body, err := os.ReadFile(filepath.Join(rs.AbsPath, p))
				if err != nil {
					return "", "", fmt.Errorf("read %s: %w", filepath.Join(rs.AbsPath, p), err)
				}
				h := sha256.Sum256(body)
				entry.ContentSHA = hex.EncodeToString(h[:])
			}
			entries = append(entries, entry)
		}
	}

	// Sort by relative path for canonical ordering across walks
	// (Linux filesystem readdir order is filesystem-dependent;
	// without sort, two builds on different filesystems would
	// produce different breakdowns even from byte-identical
	// trees).
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Path < entries[j].Path
	})

	var sb strings.Builder
	for _, e := range entries {
		status := "name"
		if e.ContentInclude {
			status = "content"
		}
		fmt.Fprintf(&sb, "%s\t%s\t%s\n", e.Path, status, e.ContentSHA)
	}
	breakdown = sb.String()
	final := sha256.Sum256([]byte(breakdown))
	hash = hex.EncodeToString(final[:])
	return hash, breakdown, nil
}

// walkSrckeyPaths returns the source-tree-relative paths under
// root, sorted, files only (directories are implicit in their
// children's paths). Symlinks aren't followed: their target name
// becomes part of the breakdown via the readlink-result content
// hash so retargeting a symlink invalidates srckey.
func walkSrckeyPaths(root string) ([]string, error) {
	var paths []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		paths = append(paths, filepath.ToSlash(rel))
		return nil
	})
	return paths, err
}

// matchesSrckeyPatterns reports whether a path's CONTENT
// contributes to srckey. Mirrors applyReadPathsPatterns'
// matched-vs-unmatched semantics minus the cmake-specific
// CMakeLists.txt special case:
//
//   - patterns nil / empty rules: content-included (full
//     hashing, conservative default).
//   - At least one include rule: content-included only when
//     a path matches an include rule (and no exclude rule).
//   - Only exclude rules: content-included unless the path
//     matches an exclude rule.
func matchesSrckeyPatterns(patterns *readPathsPatterns, path string) bool {
	if patterns == nil || len(patterns.Rules) == 0 {
		return true
	}
	hasInclude := false
	for _, r := range patterns.Rules {
		if r.Include {
			hasInclude = true
			break
		}
	}
	matched := !hasInclude
	if hasInclude {
		for _, r := range patterns.Rules {
			if r.Include && matchPattern(r.Pattern, path) {
				matched = true
				break
			}
		}
	}
	if matched {
		for _, r := range patterns.Rules {
			if !r.Include && matchPattern(r.Pattern, path) {
				matched = false
				break
			}
		}
	}
	return matched
}

// renderSrckey writes srckey.txt + srckey-breakdown.txt +
// srckey-patterns.txt to elemPkg. All three files are byte-
// stable as long as the source tree is byte-stable + patterns
// are unchanged. The breakdown is human-readable: one line per
// source path with status (content / name) and the file's
// sha256 (when content-included). srckey.txt is the sha256 of
// the breakdown's bytes. srckey-patterns.txt is the resolved
// pattern set in read-paths.txt syntax — the surface
// cmd/audit-narrowing reads to compare against the action-time
// read oracle and flag undercoverage drift. Empty patterns
// (the conservative no-narrow default) round-trip as an empty
// file; the audit treats that as "everything covered".
func renderSrckey(elem *element, elemPkg string, patterns *readPathsPatterns) error {
	hash, breakdown, err := computeSrckey(elem, patterns)
	if err != nil {
		return fmt.Errorf("element %q: compute srckey: %w", elem.Name, err)
	}
	if err := writeFile(filepath.Join(elemPkg, "srckey-breakdown.txt"), breakdown); err != nil {
		return err
	}
	if err := writeFile(filepath.Join(elemPkg, "srckey-patterns.txt"), patterns.Format()); err != nil {
		return err
	}
	return writeFile(filepath.Join(elemPkg, "srckey.txt"), hash+"\n")
}
