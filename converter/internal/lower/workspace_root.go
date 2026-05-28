package lower

import (
	"os"
	"path/filepath"
)

// detectWorkspaceRoot walks up at most workspaceMarkerMaxDepth
// levels from dir looking for a directory that carries a "this
// is the workspace root" marker (.git, MODULE.bazel, WORKSPACE,
// WORKSPACE.bazel). Returns the marker-bearing directory when
// found, or "" when no marker appears within the depth cap.
//
// Used to anchor source-path label relativization for projects
// whose cmake source dir (CMAKE_SOURCE_DIR) sits below the repo
// root. zstd's `build/cmake/CMakeLists.txt` layout is the
// canonical case: cmake's source tree is `build/cmake/` but
// CMakeLists.txt references sources at `${ZSTD_SOURCE_DIR}/lib/...`
// (one or two levels above). Without this detection, the lower's
// per-source absolute-path normalizer would refuse those paths
// with unsupported-source-path because they sit outside cmakeSrc
// AND outside cmakeBuild — even though they're inside the actual
// workspace.
//
// Why the depth cap: an unbounded walk-up catches any .git
// arbitrarily far above the cmake source dir — which is wrong
// for the common "this is a subdir of a git repo, not a
// separate workspace" case (it would promote every existing
// in-repo test fixture's workspace to the *repo* root and break
// per-target include-dir resolution). Real-world build/<X>/
// layouts (zstd, lz4, brotli, snappy) put the marker 1 or 2
// levels above cmakeSrc; 3 leaves a small headroom for slightly
// deeper variants (build/cmake/native/) without false-positives.
//
// Markers chosen as a superset of "this is a workspace root":
//   - .git/ — any git checkout (works without bzlmod)
//   - MODULE.bazel — bzlmod project
//   - WORKSPACE / WORKSPACE.bazel — legacy WORKSPACE project
//
// The detection is a heuristic, not a guarantee. The common
// orchestrator path (write-a's shadow stage) copies a specific
// subset of sources into a fresh dir with none of those markers,
// so workspaceRoot stays "" and label relativization falls back
// to cmakeSrc — preserving existing behavior. Ad-hoc operator
// runs of convert-element-cmake against an unstaged project tree
// (the zstd test case) trip the detection and pick up the wider
// label namespace.
//
// Returns "" on empty input or on filesystem errors so callers
// can branch cleanly on a non-empty result without an extra
// "did we even look" flag.
func detectWorkspaceRoot(dir string) string {
	if dir == "" {
		return ""
	}
	dir = filepath.Clean(dir)
	cmakeSrc := dir
	// Hard markers (`.git`, MODULE.bazel, WORKSPACE) short-circuit
	// the walk — they're unambiguous workspace identifiers. The
	// umbrella heuristic (`.gitignore` / `.gitattributes` in a
	// CMakeLists-free dir) is fuzzier: zstd's `build/.gitignore`
	// fires the umbrella check but `build/` isn't the project root
	// — `zstd-1.5.6/` (one level up) is. Collect all umbrella
	// candidates as we walk, then return the HIGHEST one within
	// the depth cap. This way nested .gitignore files in build
	// subdirs lose to the project-root .gitignore.
	var highestUmbrella string
	for steps := 0; steps <= workspaceMarkerMaxDepth; steps++ {
		for _, marker := range workspaceMarkers {
			if _, err := os.Stat(filepath.Join(dir, marker)); err == nil {
				return dir
			}
		}
		// Umbrella / monorepo marker: dirs that aren't a cmake
		// project themselves (no CMakeLists.txt at the top)
		// but carry `.gitignore` or `.gitattributes`. LLVM's
		// shape — `llvm-project/llvm/CMakeLists.txt` is the
		// cmake source but `llvm-project/third-party/benchmark/`
		// is a sibling tree the build references. The umbrella
		// root has no CMakeLists.txt and carries `.gitignore`
		// (the monorepo's git-housekeeping). Skip cmakeSrc
		// itself — it may have a `.gitignore` for git-tracked
		// build artefacts and we want the higher umbrella.
		if dir != cmakeSrc {
			for _, marker := range umbrellaMarkers {
				if _, err := os.Stat(filepath.Join(dir, marker)); err == nil {
					if _, err := os.Stat(filepath.Join(dir, "CMakeLists.txt")); err != nil {
						// Record but keep walking — a higher
						// ancestor's marker is more
						// authoritative than this nested one.
						highestUmbrella = dir
						break
					}
				}
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			// Hit filesystem root.
			break
		}
		dir = parent
	}
	return highestUmbrella
}

// workspaceMarkerMaxDepth caps detectWorkspaceRoot's walk-up.
// The starting dir itself is depth 0; each parent step adds one.
// 3 covers zstd/lz4/brotli's `build/cmake/CMakeLists.txt` (depth
// 2) plus one level of headroom for slightly deeper variants.
const workspaceMarkerMaxDepth = 3

// workspaceMarkers is the ordered set of files / dirs whose
// presence in a directory marks it as the workspace root.
// Ordered most-discriminating-first: .git is by far the most
// common; MODULE.bazel narrows it to bzlmod; WORKSPACE catches
// pre-bzlmod Bazel repos.
var workspaceMarkers = []string{
	".git",
	"MODULE.bazel",
	"WORKSPACE",
	"WORKSPACE.bazel",
}

// umbrellaMarkers identify a monorepo / multi-project parent
// directory that isn't itself a cmake project but contains
// multiple subtrees referenced by sibling cmake projects.
// Distinct from workspaceMarkers because the test is conditional
// on the absence of a top-level CMakeLists.txt — these markers
// would otherwise false-positive on the cmake source dir itself
// (single-project repos like spdlog/ have both `.gitignore` AND
// a CMakeLists.txt). LLVM's `llvm-project/` shape: no top-level
// CMakeLists.txt but carries `.gitignore` and `.gitattributes`
// for the monorepo's git housekeeping.
var umbrellaMarkers = []string{
	".gitignore",
	".gitattributes",
}
