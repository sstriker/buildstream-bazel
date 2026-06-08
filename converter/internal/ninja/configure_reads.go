package ninja

import (
	"path/filepath"

	"github.com/sstriker/buildstream-bazel/internal/pathutil"
	"github.com/sstriker/buildstream-bazel/internal/sliceutil"
)

// ProjectToSourceTree filters a raw RERUN_CMAKE implicit-input list to the
// paths that live inside sourceRoot AND outside buildDir. Relative paths in
// the input are resolved against buildDir (cmake writes build-tree paths like
// CMakeCache.txt and CMakeFiles/<ver>/CMakeCCompiler.cmake relative to
// cmake_ninja_workdir).
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
//   - Absolute paths that resolve into buildDir, even when buildDir is
//     itself nested inside sourceRoot (the typical out-of-source-but-
//     within-the-repo build dir layout). Without this check, build-tree
//     artifacts would pass the in-source-root test and leak into the
//     oracle.
//   - Paths that share sourceRoot's parent prefix only (`..` segments
//     after Rel).
//
// sourceRoot and buildDir may be relative or absolute; the function
// runs filepath.Abs on each before comparison. If either is empty,
// the function returns nil (callers without both pieces of context
// can't do the projection).
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
		// Filter out anything that resolves into the build dir,
		// regardless of whether buildDir is in or out of sourceRoot.
		// Without this check, an in-tree build dir layout (e.g.
		// sourceRoot=/work, buildDir=/work/build) would let
		// CMakeCache.txt and friends pass the inside-sourceRoot
		// test below as `build/CMakeCache.txt`.
		if relBuild, err := filepath.Rel(buildAbs, abs); err == nil && pathutil.InsideRoot(relBuild) {
			continue
		}
		// Edge: the buildDir itself (relBuild == ".") shouldn't
		// be in an oracle either; insideRoot returns false for
		// "." so we drop it here.
		if abs == buildAbs {
			continue
		}
		rel, err := filepath.Rel(srcAbs, abs)
		if err != nil {
			continue
		}
		if !pathutil.InsideRoot(rel) {
			continue
		}
		seen[filepath.ToSlash(rel)] = struct{}{}
	}
	if len(seen) == 0 {
		return nil
	}
	out := sliceutil.SortedKeys(seen)
	return out
}
