package lower

import (
	"path/filepath"
	"strings"

	"github.com/sstriker/buildstream-bazel/converter/internal/cmakeargv"
	"github.com/sstriker/buildstream-bazel/converter/ir"
	"github.com/sstriker/buildstream-bazel/internal/shadow"
)

// provenanceHostPath resolves a target's Provenance.File to a readable host
// path: absolute paths are used as-is, source-root-relative paths are joined
// with hostSrc. Empty when no path is available.
func provenanceHostPath(file, hostSrc string) string {
	if file == "" {
		return ""
	}
	if filepath.IsAbs(file) || hostSrc == "" {
		return file
	}
	return filepath.Join(hostSrc, file)
}

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
func recoverSourceComments(pkg *ir.Package, hostSrc, cmakeSrc, cmakeBuild string,
	execProcs []shadow.ExecuteProcessCall,
	customCmds []shadow.AddCustomCommandCall,
	customTgts []shadow.AddCustomTargetCall) {
	if pkg == nil {
		return
	}
	// Read each source file at most once: many targets are declared in one
	// CMakeLists, so re-reading per site would be O(N_targets x file_size).
	lineCache := map[string][]string{}
	fileLines := func(path string) []string {
		if v, ok := lineCache[path]; ok {
			return v
		}
		v, err := cmakeargv.ReadSourceLines(path)
		if err != nil {
			v = nil
		}
		lineCache[path] = v
		return v
	}
	// Codemodel targets already carry the correct declaration site in their
	// Provenance (populated from the codemodel backtrace). Recover the leading
	// comment from that site, reading the host file (Provenance.File is source-
	// root-relative; join it with hostSrc, or use it directly when absolute).
	// Sites shared by >1 target (a helper invoked N times) are skipped to avoid
	// misattributing one body comment to many targets.
	type site struct {
		file string
		line int
	}
	count := map[site]int{}
	for i := range pkg.Targets {
		p := pkg.Targets[i].Provenance
		if p.File == "" || p.Line <= 0 {
			continue
		}
		count[site{p.File, p.Line}]++
	}
	for i := range pkg.Targets {
		t := &pkg.Targets[i]
		if len(t.LeadingComment) > 0 || t.Provenance.File == "" || t.Provenance.Line <= 0 {
			continue
		}
		key := site{t.Provenance.File, t.Provenance.Line}
		if count[key] != 1 {
			continue // shared site: helper-invoked, ambiguous — skip
		}
		host := provenanceHostPath(t.Provenance.File, hostSrc)
		// isCMakeInternalPath matches forward-slash substrings; host comes from
		// filepath.Join (OS separators), so normalize for the check only — the
		// original host path is still used for reading.
		if host == "" || isCMakeInternalPath(filepath.ToSlash(host)) {
			continue
		}
		ls := fileLines(host)
		if lc := cmakeargv.LeadingCommentLines(ls, t.Provenance.Line); len(lc) > 0 {
			t.LeadingComment = lc
		}
		if tc := cmakeargv.TrailingCommentLines(ls, t.Provenance.Line); tc != "" {
			t.TrailingComment = tc
		}
	}

	// File header (scope A): the top-level CMakeLists license/doc block,
	// prepended to HeaderComments so it sits at the very top of the BUILD.
	// HeaderComments are plain text (the emitter prefixes "# "), so strip the
	// recovered raw tokens' leading "#".
	if hostSrc != "" {
		hdr := cmakeargv.FileHeaderCommentLines(fileLines(filepath.Join(hostSrc, "CMakeLists.txt")))
		if len(hdr) > 0 {
			stripped := make([]string, 0, len(hdr))
			for _, ln := range hdr {
				stripped = append(stripped, stripCommentPrefix(ln))
			}
			pkg.HeaderComments = append(stripped, pkg.HeaderComments...)
		}
	}

	// Codegen genrules (the "comments before a codegen" case): synthesized
	// genrules aren't codemodel targets, so they carry no backtrace. Match
	// each to its originating trace call (execute_process / add_custom_command
	// / add_custom_target — all carry File/Line + outputs) by output basename,
	// then recover the leading comment there and stamp Provenance. Best-effort:
	// an unmatched or ambiguous-basename genrule simply gets no comment.
	siteByBase := buildCodegenSiteIndex(execProcs, customCmds, customTgts)
	if len(siteByBase) > 0 {
		for i := range pkg.Targets {
			t := &pkg.Targets[i]
			if t.Kind != ir.KindGenrule || len(t.LeadingComment) > 0 {
				continue
			}
			for _, out := range t.GenruleOuts {
				s, ok := siteByBase[filepath.Base(out)]
				if !ok || s.file == "" || isCMakeInternalPath(filepath.ToSlash(s.file)) {
					continue
				}
				gls := fileLines(s.file)
				if lc := cmakeargv.LeadingCommentLines(gls, s.line); len(lc) > 0 {
					t.LeadingComment = lc
				}
				if tc := cmakeargv.TrailingCommentLines(gls, s.line); tc != "" {
					t.TrailingComment = tc
				}
				if t.Provenance.IsZero() {
					t.Provenance = ir.Provenance{
						File:    reanchorProvenanceFile(s.file, cmakeSrc, cmakeBuild),
						Line:    s.line,
						Command: s.command,
					}
				}
				break
			}
		}
	}
}

// codegenSite is a trace-call declaration site for a synthesized genrule.
type codegenSite struct {
	file    string
	line    int
	command string
}

// buildCodegenSiteIndex maps a codegen output's basename to the trace call that
// declared it. Basenames seen with conflicting sites are dropped (ambiguous →
// don't misattribute). Basename (not full path) is the match key because trace
// outputs are build/source-relative while genrule outs are package-relative;
// the final component is the stable common denominator.
func buildCodegenSiteIndex(
	execProcs []shadow.ExecuteProcessCall,
	customCmds []shadow.AddCustomCommandCall,
	customTgts []shadow.AddCustomTargetCall,
) map[string]codegenSite {
	idx := map[string]codegenSite{}
	ambiguous := map[string]bool{}
	add := func(out string, s codegenSite) {
		base := filepath.Base(out)
		if base == "" || base == "." || base == "/" || s.file == "" || s.line <= 0 {
			return
		}
		if ambiguous[base] {
			return
		}
		if prev, ok := idx[base]; ok && prev != s {
			delete(idx, base)
			ambiguous[base] = true
			return
		}
		idx[base] = s
	}
	for _, c := range execProcs {
		if c.OutputFile != "" {
			add(c.OutputFile, codegenSite{file: c.File, line: c.Line, command: "execute_process"})
		}
	}
	for _, c := range customCmds {
		s := codegenSite{file: c.File, line: c.Line, command: "add_custom_command"}
		for _, o := range c.Outputs {
			add(o, s)
		}
		for _, o := range c.ByProducts {
			add(o, s)
		}
	}
	for _, c := range customTgts {
		s := codegenSite{file: c.File, line: c.Line, command: "add_custom_target"}
		for _, o := range c.ByProducts {
			add(o, s)
		}
	}
	return idx
}

// stripCommentPrefix removes a leading "#" and one optional following space
// from a raw cmake comment token, yielding the plain text HeaderComments
// expects. "# Copyright" -> "Copyright"; "#x" -> "x"; "##" -> "#".
func stripCommentPrefix(token string) string {
	s := strings.TrimSpace(token)
	s = strings.TrimPrefix(s, "#")
	s = strings.TrimPrefix(s, " ") // drop at most one space after the '#'
	return s
}
