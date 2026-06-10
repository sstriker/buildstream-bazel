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
// Association: a target's comment site is its user-level CallSite when the
// declaring command ran inside a macro/function (the invocation the author
// wrote, where their comment sits), else its Provenance declaration site (the
// add_library/add_executable/… file:line). We read the contiguous `#`
// leading-comment block there. To avoid misattributing one comment to many
// targets, **sites shared by more than one target are skipped** — e.g. a
// single macro invocation that declares several targets, or a helper body
// line with no recoverable call site. cmake-internal and unreadable files are
// skipped (best-effort; offline --reply-dir runs whose recorded paths don't
// exist locally simply recover nothing).
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
	recoverTargetComments(pkg, hostSrc, fileLines)

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

	recoverCodegenGenruleComments(pkg, cmakeSrc, cmakeBuild, fileLines, execProcs, customCmds, customTgts)
}

// commentSite is the source location whose comments describe the target from
// the author's point of view: the user-level macro/function invocation when
// the target was macro-declared (CallSite), else the declaration itself.
func commentSite(t *ir.Target) ir.Provenance {
	if !t.CallSite.IsZero() {
		return t.CallSite
	}
	return t.Provenance
}

// recoverTargetComments recovers each codemodel target's leading/trailing
// source comment from its comment site (the user-level call site for
// macro-declared targets, else the Provenance declaration site). Sites shared
// by >1 target — one macro invocation declaring several targets, or a helper
// body line with no call site recovered — are skipped to avoid misattributing
// one comment to many targets. fileLines reads + caches source files.
func recoverTargetComments(pkg *ir.Package, hostSrc string, fileLines func(string) []string) {
	type site struct {
		file string
		line int
	}
	count := map[site]int{}
	for i := range pkg.Targets {
		p := commentSite(&pkg.Targets[i])
		if p.File == "" || p.Line <= 0 {
			continue
		}
		count[site{p.File, p.Line}]++
	}
	for i := range pkg.Targets {
		t := &pkg.Targets[i]
		p := commentSite(t)
		if len(t.LeadingComment) > 0 || p.File == "" || p.Line <= 0 {
			continue
		}
		key := site{p.File, p.Line}
		if count[key] != 1 {
			continue // shared site: multi-target declaration, ambiguous — skip
		}
		host := provenanceHostPath(p.File, hostSrc)
		// isCMakeInternalPath matches forward-slash substrings; host comes from
		// filepath.Join (OS separators), so normalize for the check only — the
		// original host path is still used for reading.
		if host == "" || isCMakeInternalPath(filepath.ToSlash(host)) {
			continue
		}
		ls := fileLines(host)
		if lc := cmakeargv.LeadingCommentLines(ls, p.Line); len(lc) > 0 {
			t.LeadingComment = lc
		}
		if tc := cmakeargv.TrailingCommentLines(ls, p.Line); tc != "" {
			t.TrailingComment = tc
		}
	}
}

// recoverCodegenGenruleComments recovers leading/trailing comments for
// synthesized codegen genrules (which carry no codemodel backtrace) by matching
// each genrule output basename to its originating trace call (execute_process /
// add_custom_command / add_custom_target — all carry File/Line + outputs), then
// stamping Provenance. When the trace recovered the call's user-level
// invocation (a codegen-wrapping macro/function call — callFile/callLine), the
// comment scan reads THAT site, so the comment above the wrapper invocation
// carries instead of the macro body's, and CallSite is stamped alongside
// Provenance (the breadcrumb then leads with the invocation). Best-effort: an
// unmatched or ambiguous-basename genrule simply gets no comment.
func recoverCodegenGenruleComments(pkg *ir.Package, cmakeSrc, cmakeBuild string, fileLines func(string) []string, execProcs []shadow.ExecuteProcessCall, customCmds []shadow.AddCustomCommandCall, customTgts []shadow.AddCustomTargetCall) {
	siteByBase := buildCodegenSiteIndex(execProcs, customCmds, customTgts)
	if len(siteByBase) == 0 {
		return
	}
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
			scanFile, scanLine := s.file, s.line
			if s.callFile != "" && s.callLine > 0 && !isCMakeInternalPath(filepath.ToSlash(s.callFile)) {
				scanFile, scanLine = s.callFile, s.callLine
			}
			gls := fileLines(scanFile)
			if lc := cmakeargv.LeadingCommentLines(gls, scanLine); len(lc) > 0 {
				t.LeadingComment = lc
			}
			if tc := cmakeargv.TrailingCommentLines(gls, scanLine); tc != "" {
				t.TrailingComment = tc
			}
			if t.Provenance.IsZero() {
				t.Provenance = ir.Provenance{
					File:    reanchorProvenanceFile(s.file, cmakeSrc, cmakeBuild),
					Line:    s.line,
					Command: s.command,
				}
			}
			if t.CallSite.IsZero() && s.callFile != "" && s.callLine > 0 &&
				(s.callFile != s.file || s.callLine != s.line) {
				t.CallSite = ir.Provenance{
					File:    reanchorProvenanceFile(s.callFile, cmakeSrc, cmakeBuild),
					Line:    s.callLine,
					Command: s.callCmd,
				}
			}
			break
		}
	}
}

// codegenSite is a trace-call declaration site for a synthesized genrule,
// plus the user-level invocation (callFile/callLine/callCmd) when the trace
// frame stack recovered one — the macro/function call that wrapped the
// codegen declaration. Empty call fields mean the declaration is
// user-authored where it stands.
type codegenSite struct {
	file     string
	line     int
	command  string
	callFile string
	callLine int
	callCmd  string
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
			add(c.OutputFile, codegenSite{file: c.File, line: c.Line, command: "execute_process",
				callFile: c.CallFile, callLine: c.CallLine, callCmd: c.CallCmd})
		}
	}
	for _, c := range customCmds {
		s := codegenSite{file: c.File, line: c.Line, command: "add_custom_command",
			callFile: c.CallFile, callLine: c.CallLine, callCmd: c.CallCmd}
		for _, o := range c.Outputs {
			add(o, s)
		}
		for _, o := range c.ByProducts {
			add(o, s)
		}
	}
	for _, c := range customTgts {
		s := codegenSite{file: c.File, line: c.Line, command: "add_custom_target",
			callFile: c.CallFile, callLine: c.CallLine, callCmd: c.CallCmd}
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
