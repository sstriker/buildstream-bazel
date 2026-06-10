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
// the user-level CallSite: the outermost user-source INVOCATION frame of
// the same backtrace (the call the author actually wrote, where their
// comment sits). callSite is zero for directly-declared targets — the
// declaration IS the user's call — including declarations at the top
// level of an include()d project .cmake file (an inclusion is a scope
// change, not an invocation; see callSiteFrame), and when no backtrace
// is available.
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
	cfile, cline, ccmd := callSiteFrame(t.BacktraceGraph, t.Backtrace)
	if cfile != "" && (cfile != file || cline != node.Line) {
		callSite = ir.Provenance{
			File:    reanchorProvenanceFile(cfile, cmakeSrc, cmakeBuild),
			Line:    cline,
			Command: ccmd,
		}
	}
	return decl, callSite
}

// callSiteFrame walks the backtrace's parent chain from the declaring node
// up through macro/function INVOCATION frames only, returning the outermost
// user-source invocation — the call the author wrote. Returns zeros when the
// declaring node has no invocation above it (a directly-declared target).
//
// Unlike outermostUserFrame (link-scope recovery's walk), inclusion frames —
// include() / find_package() / add_subdirectory() — terminate the walk: they
// change scope but don't expand a body, so a command at an included file's
// top level is user-authored where it stands. Treating the include() line as
// the call site would misattribute the file's comments (and one included
// file declaring several targets would collapse them all onto one ambiguous
// shared site). cmake-internal frames (a bundled module's macro body) are
// ascended through but never returned as the call site.
func callSiteFrame(g fileapi.BacktraceGraph, start int) (file string, line int, command string) {
	if start <= 0 || start >= len(g.Nodes) {
		return "", 0, ""
	}
	cur := start
	best := -1 // outermost user-source invocation frame above start
	for {
		parent := g.Nodes[cur].Parent
		if parent == nil || *parent <= 0 || *parent >= len(g.Nodes) {
			break
		}
		p := *parent
		pnode := g.Nodes[p]
		if pnode.Line <= 0 {
			break // file-root frame, not a call
		}
		var pcmd string
		if pnode.Command >= 0 && pnode.Command < len(g.Commands) {
			pcmd = g.Commands[pnode.Command]
		}
		if isInclusionCommand(pcmd) {
			break // scope change: cur's location is authored at top scope
		}
		cur = p
		var pfile string
		if pnode.File >= 0 && pnode.File < len(g.Files) {
			pfile = g.Files[pnode.File]
		}
		if pfile != "" && !isCMakeInternalPath(pfile) {
			best = p
		}
	}
	if best < 0 {
		return "", 0, ""
	}
	n := g.Nodes[best]
	if n.File >= 0 && n.File < len(g.Files) {
		file = g.Files[n.File]
	}
	if n.Command >= 0 && n.Command < len(g.Commands) {
		command = g.Commands[n.Command]
	}
	return file, n.Line, command
}

// isInclusionCommand identifies frames that pull another file/directory into
// the configure without expanding a callable body: their children are
// authored at the included file's own top scope, not at the caller's line.
func isInclusionCommand(cmd string) bool {
	switch strings.ToLower(cmd) {
	case "include", "find_package", "add_subdirectory", "subdirs":
		return true
	}
	return false
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
