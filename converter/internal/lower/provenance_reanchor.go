package lower

import (
	"path/filepath"
	"strings"

	"github.com/sstriker/buildstream-bazel/converter/internal/fileapi"
	"github.com/sstriker/buildstream-bazel/converter/ir"
)

// targetProvenance projects a codemodel target's Backtrace into its
// declaration-site Provenance (the immediate add_library/add_executable/…
// frame) plus, when that command ran inside a macro/function expansion,
// the user-level CallSite: the outermost user-source frame of the same
// backtrace (the invocation the author actually wrote, where their
// comment sits). callSite is zero for directly-declared targets — the
// declaration IS the user's call — and when no backtrace is available.
func targetProvenance(t *fileapi.Target, cmakeSrc, cmakeBuild string) (decl, callSite ir.Provenance) {
	if t.Backtrace <= 0 || t.Backtrace >= len(t.BacktraceGraph.Nodes) {
		return decl, callSite
	}
	node := t.BacktraceGraph.Nodes[t.Backtrace]
	var file, cmd string
	if node.File >= 0 && node.File < len(t.BacktraceGraph.Files) {
		file = t.BacktraceGraph.Files[node.File]
	}
	if node.Command >= 0 && node.Command < len(t.BacktraceGraph.Commands) {
		cmd = t.BacktraceGraph.Commands[node.Command]
	}
	if file == "" {
		return decl, callSite
	}
	decl = ir.Provenance{
		File:    reanchorProvenanceFile(file, cmakeSrc, cmakeBuild),
		Line:    node.Line,
		Command: cmd,
	}
	cfile, cline, ccmd := outermostUserFrame(t.BacktraceGraph, t.Backtrace)
	if cfile != "" && (cfile != file || cline != node.Line) {
		callSite = ir.Provenance{
			File:    reanchorProvenanceFile(cfile, cmakeSrc, cmakeBuild),
			Line:    cline,
			Command: ccmd,
		}
	}
	return decl, callSite
}

// reanchorProvenanceFile re-anchors an absolute file path from
// the cmake BacktraceGraph to workspace-relative form, matching
// the convention srcs / hdrs / includes use elsewhere in the IR.
//
// cmake's BacktraceGraph stores most file entries relative to
// the source root already, but absolute paths slip through in
// two common cases:
//
//   - `add_subdirectory(${EXTERNAL_DIR}/foo bar)` where
//     EXTERNAL_DIR is outside cmakeSrc. cmake records the
//     CMakeLists.txt absolute path.
//   - Targets declared inside a configure_file-generated
//     CMakeLists.txt under the build dir. cmake records the
//     build-dir-absolute path.
//
// Absolute paths leak the convert-host filesystem layout into
// the rendered `# Source:` comments — useless for downstream
// operators and a byte-identical-emit hazard across runs of the
// same converter on different machines.
//
// Re-anchor strategy (first match wins):
//
//  1. inside cmakeSrc → strip the prefix.
//  2. inside cmakeBuild → strip the prefix (configure_file-
//     generated CMakeLists case).
//  3. inside cmakeSrc's parent (common for `third-party/`
//     directories one level up from cmakeSrc) → relative
//     anchored there.
//  4. otherwise → leave the absolute path untouched. The
//     comment is still informational; operators see the
//     out-of-tree origin verbatim.
//
// Already-relative paths pass through unchanged.
func reanchorProvenanceFile(file, cmakeSrc, cmakeBuild string) string {
	if file == "" || !filepath.IsAbs(file) {
		return file
	}
	if isUsableAnchor(cmakeSrc) {
		if rel, ok := relativeIfInside(cmakeSrc, file); ok {
			return rel
		}
	}
	if isUsableAnchor(cmakeBuild) {
		if rel, ok := relativeIfInside(cmakeBuild, file); ok {
			return rel
		}
	}
	// cmakeSrc's parent — covers the common third-party /
	// vendored sibling shape where the cmake source root is
	// `<project>/<subproject>` and the CMakeLists.txt lives
	// under `<project>/third-party/...`.
	if isUsableAnchor(cmakeSrc) {
		parent := filepath.Dir(cmakeSrc)
		if isUsableAnchor(parent) && parent != cmakeSrc {
			if rel, ok := relativeIfInside(parent, file); ok {
				return rel
			}
		}
	}
	return file
}

// isUsableAnchor reports whether `p` is suitable as a path
// anchor for re-anchoring absolute paths. Filters out the
// degenerate filesystem-root case (`/`, Windows `C:\`) which
// would otherwise match every absolute path and rewrite system
// files to "workspace-relative" form — definitely wrong.
func isUsableAnchor(p string) bool {
	if p == "" || p == "/" {
		return false
	}
	// Windows drive-letter root (`C:\`, `D:\`, ...) — too coarse
	// an anchor.
	if len(p) == 3 && p[1] == ':' && (p[2] == '\\' || p[2] == '/') {
		return false
	}
	if strings.HasSuffix(p, ":\\") {
		return false
	}
	return true
}
