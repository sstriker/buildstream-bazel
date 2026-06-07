package lower

import (
	"path/filepath"
	"strings"

	"github.com/sstriker/buildstream-bazel/converter/internal/cmakeargv"
	"github.com/sstriker/buildstream-bazel/converter/internal/fileapi"
	"github.com/sstriker/buildstream-bazel/converter/ir"
)

// recoverSourceComments carries author comments from raw cmake source into the
// IR: it populates ir.Target.LeadingComment for codemodel targets and prepends
// the top-level CMakeLists header block to pkg.HeaderComments. cmake discards
// comments at lex time, so neither the File API nor the trace carries them —
// they are recoverable only from raw source. Gated by
// Options.RecoverSourceComments; the emitter renders the result under
// EmitSourceComments.
//
// Association: a target's innermost declaration site is its Backtrace node
// (the add_library/add_executable/… file:line). We read the contiguous `#`
// leading-comment block there. To avoid misattributing a helper's body comment
// to the many targets a function/macro produces, **sites shared by more than
// one target are skipped** — a function invoked N times yields N targets at one
// body line, the egregious misattribution case. cmake-internal and unreadable
// files are skipped (best-effort; offline --reply-dir runs whose recorded paths
// don't exist locally simply recover nothing).
func recoverSourceComments(pkg *ir.Package, r *fileapi.Reply, hostSrc string) {
	if pkg == nil || r == nil {
		return
	}
	type site struct {
		file string
		line int
	}
	count := map[site]int{}
	targetSite := map[string]site{} // codemodel target name -> declaration site
	for _, t := range r.Targets {
		if t.IsGeneratorProvided {
			continue
		}
		if t.Backtrace <= 0 || t.Backtrace >= len(t.BacktraceGraph.Nodes) {
			continue
		}
		node := t.BacktraceGraph.Nodes[t.Backtrace]
		if node.File < 0 || node.File >= len(t.BacktraceGraph.Files) {
			continue
		}
		f := t.BacktraceGraph.Files[node.File]
		if f == "" || node.Line <= 0 || isCMakeInternalPath(f) {
			continue
		}
		s := site{file: f, line: node.Line}
		count[s]++
		targetSite[t.Name] = s
	}

	byName := make(map[string]*ir.Target, len(pkg.Targets))
	for i := range pkg.Targets {
		byName[pkg.Targets[i].Name] = &pkg.Targets[i]
	}
	cache := map[site][]string{}
	for name, s := range targetSite {
		if count[s] != 1 {
			continue // shared site: helper-invoked, ambiguous — skip
		}
		irt := byName[name]
		if irt == nil {
			continue
		}
		lines, ok := cache[s]
		if !ok {
			recovered, err := cmakeargv.LeadingComment(s.file, s.line)
			if err != nil {
				recovered = nil
			}
			cache[s] = recovered
			lines = recovered
		}
		if len(lines) > 0 {
			irt.LeadingComment = lines
		}
	}

	// File header (scope A): the top-level CMakeLists license/doc block,
	// prepended to HeaderComments so it sits at the very top of the BUILD.
	// HeaderComments are plain text (the emitter prefixes "# "), so strip the
	// recovered raw tokens' leading "#".
	if hostSrc != "" {
		hdr, err := cmakeargv.FileHeaderComment(filepath.Join(hostSrc, "CMakeLists.txt"))
		if err == nil && len(hdr) > 0 {
			stripped := make([]string, 0, len(hdr))
			for _, ln := range hdr {
				stripped = append(stripped, stripCommentPrefix(ln))
			}
			pkg.HeaderComments = append(stripped, pkg.HeaderComments...)
		}
	}
}

// stripCommentPrefix removes a leading "#" and one optional following space
// from a raw cmake comment token, yielding the plain text HeaderComments
// expects. "# Copyright" -> "Copyright"; "#x" -> "x"; "##" -> "#".
func stripCommentPrefix(token string) string {
	s := strings.TrimSpace(token)
	s = strings.TrimPrefix(s, "#")
	if strings.HasPrefix(s, " ") {
		s = s[1:]
	}
	return s
}
