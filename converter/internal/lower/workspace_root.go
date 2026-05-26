package lower

import (
	"os"
	"path/filepath"
)

// detectWorkspaceRoot walks up from dir looking for a directory
// that carries a "this is the workspace root" marker (.git,
// MODULE.bazel, WORKSPACE, WORKSPACE.bazel). Returns the
// marker-bearing directory when found, or "" when no marker
// appears before the filesystem root.
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
	for {
		for _, marker := range workspaceMarkers {
			if _, err := os.Stat(filepath.Join(dir, marker)); err == nil {
				return dir
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			// Hit filesystem root without finding a marker.
			return ""
		}
		dir = parent
	}
}

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
