package ninja

import (
	"path/filepath"
	"sort"
	"strings"
)

// ProjectToSourceTree filters a raw RERUN_CMAKE implicit-input list to the
// paths that live inside sourceRoot. Relative paths in the input are
// resolved against buildDir before the in-source check (cmake writes
// build-tree paths like CMakeCache.txt and CMakeFiles/<ver>/CMakeCCompiler.cmake
// relative to cmake_ninja_workdir).
//
// Output: source-relative slash-form paths, sorted lexicographically and
// deduplicated. Empty input → nil.
//
// Filtered out:
//   - Absolute paths outside sourceRoot (cmake-stdlib /usr/share/cmake-*
//     modules, system find_package results).
//   - Relative paths that resolve into the build tree (CMakeCache.txt,
//     CMakeFiles/<ver>/* — these are configure outputs, not source-tree
//     inputs).
//   - Paths that share sourceRoot's parent prefix only (`..` segments
//     after Rel).
//
// sourceRoot and buildDir should be absolute. If either is empty, the
// function returns nil (callers without both pieces of context can't do
// the projection).
func ProjectToSourceTree(inputs []string, sourceRoot, buildDir string) []string {
	if len(inputs) == 0 || sourceRoot == "" || buildDir == "" {
		return nil
	}
	srcAbs, err := filepath.Abs(sourceRoot)
	if err != nil {
		return nil
	}
	buildAbs, err := filepath.Abs(buildDir)
	if err != nil {
		return nil
	}
	seen := map[string]struct{}{}
	for _, p := range inputs {
		if p == "" {
			continue
		}
		abs := p
		if !filepath.IsAbs(abs) {
			abs = filepath.Join(buildAbs, abs)
		}
		rel, err := filepath.Rel(srcAbs, abs)
		if err != nil {
			continue
		}
		if rel == "." || rel == "" || strings.HasPrefix(rel, "..") {
			continue
		}
		seen[filepath.ToSlash(rel)] = struct{}{}
	}
	if len(seen) == 0 {
		return nil
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
