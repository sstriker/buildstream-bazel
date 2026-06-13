package lower

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/sstriker/buildstream-bazel/converter/ir"
	"github.com/sstriker/buildstream-bazel/internal/configurefile"
	"github.com/sstriker/buildstream-bazel/internal/shadow"
)

// The file() WRITER index — the bake-audit's "tie it to the generation
// command" close-out. The codemodel names exactly which build-dir files
// targets consume (every source/header/include whose parent is NOT
// under the source tree is configure-created — the demand side); the
// expanded trace records the file(WRITE/APPEND/TOUCH/COPY/COPY_FILE/
// DOWNLOAD) calls that created them (the producer side). Consulting
// the index BEFORE the on-disk byte-bake upgrades the recovery:
//
//   - COPY/COPY_FILE → a TRUE lift: a cp genrule from the declared
//     source, re-run at Bazel build time (tracks source edits — the
//     byte-bake froze them);
//   - WRITE(+APPENDs)/TOUCH → write_file from the TRACE bytes, with
//     file:line provenance — same convert-time trade as the byte-bake,
//     but producer-attributed and available OFFLINE (no live build dir
//     needed);
//   - DOWNLOAD → stays a byte-bake by policy (no network at build
//     time), but the bake now carries its own facet and a todo CITING
//     the URL so an author can hand-lift to http_file.
//
// Chains compose in trace order; a chain the model can't compose
// (e.g. COPY then APPEND — content would need the source bytes) just
// declines, leaving the on-disk bake exactly as before.

// buildFileWriterIndex keys the writer calls by build-dir-relative
// output path, preserving trace order per path.
func buildFileWriterIndex(calls []shadow.FileWriterCall, recordedBuildDir string) map[string][]shadow.FileWriterCall {
	if len(calls) == 0 {
		return nil
	}
	idx := map[string][]shadow.FileWriterCall{}
	for _, c := range calls {
		for _, out := range c.Outputs {
			if rel, inside := relativeIfInside(recordedBuildDir, out); inside && rel != "" {
				idx[rel] = append(idx[rel], c)
			}
		}
	}
	return idx
}

// writerChain is the composed end-state of one path's writer calls.
type writerChain struct {
	mode    string // "content", "copy", "download", "" (uncomposable/none)
	content string
	src     string // copy source (absolute, as traced)
	url     string // download URL
	file    string // provenance of the LAST writer
	line    int
	// grounded: the chain's BASE is a full-content writer (WRITE /
	// COPY / DOWNLOAD), so the composed state is the whole file. An
	// APPEND- or TOUCH-created base is NOT grounded — the file may
	// pre-exist from a writer the index can't see (an out-of-tree
	// module's file(WRITE), a refused tool's output), and cmake's
	// APPEND/TOUCH never truncate; composing from empty would bake
	// truncated bytes over a correct file.
	grounded bool
}

// composeWriterChain folds a path's writer calls (trace order) into the
// final state cmake left the file in.
func composeWriterChain(calls []shadow.FileWriterCall) writerChain {
	var ch writerChain
	for _, c := range calls {
		ch.file, ch.line = c.File, c.Line
		switch c.Op {
		case "write":
			ch = writerChain{mode: "content", content: c.Content, file: c.File, line: c.Line, grounded: true}
		case "append":
			switch ch.mode {
			case "content":
				ch.content += c.Content
			case "":
				// APPEND to a path with no indexed base creates it —
				// but only from the index's view; not grounded.
				ch.mode, ch.content, ch.grounded = "content", c.Content, false
			default:
				// APPEND onto a copy/download: composing would need
				// the source bytes; decline the whole chain.
				return writerChain{}
			}
		case "touch":
			if ch.mode == "" {
				// Created-from-empty only in the index's view; an
				// existing file is left untouched (no truncation).
				ch.mode, ch.content, ch.grounded = "content", "", false
			}
		case "copy", "copy_file":
			// writerChainFor narrowed fan-out copies to this output's
			// single source before composing.
			if len(c.Sources) != 1 {
				return writerChain{}
			}
			ch = writerChain{mode: "copy", src: c.Sources[0], file: c.File, line: c.Line, grounded: true}
		case "download":
			ch = writerChain{mode: "download", url: c.URL, file: c.File, line: c.Line, grounded: true}
		}
	}
	return ch
}

// writerChainFor returns the composed chain for one build-relative
// path, matching the chain's copy SOURCE to the path when the call
// fanned out (file(COPY a b DESTINATION d) has two outputs).
func writerChainFor(rel string, lc targetLowerCtx) (writerChain, bool) {
	calls := lc.cc.FileWriterIndex[rel]
	if len(calls) == 0 {
		return writerChain{}, false
	}
	// Narrow fan-out copies to THIS output's source before composing.
	narrowed := make([]shadow.FileWriterCall, 0, len(calls))
	for _, c := range calls {
		if c.Op == "copy" && len(c.Sources) > 1 {
			base := filepath.Base(rel)
			for _, s := range c.Sources {
				if filepath.Base(s) == base {
					nc := c
					nc.Sources = []string{s}
					c = nc
					break
				}
			}
		}
		narrowed = append(narrowed, c)
	}
	ch := composeWriterChain(narrowed)
	return ch, ch.mode != ""
}

// liftBuildDirFileFromWriter recovers one build-relative file from its
// traced writer chain. Returns (ruleName, true) when a producer rule
// was emitted (or already exists); (_, false) leaves the on-disk bake
// as the caller's fallback.
func liftBuildDirFileFromWriter(rel string, lc targetLowerCtx) (string, bool) {
	ch, ok := writerChainFor(rel, lc)
	if !ok {
		return "", false
	}
	cc := lc.cc
	if !writerChainTrustworthy(rel, ch, lc) {
		return "", false
	}
	switch ch.mode {
	case "content":
		// VCS-stamp wiring: a file(WRITE v.h "…${GIT_SHA}…") is a
		// configure_file in disguise — the non-expanded content is the
		// template (the stamp var survives), the expanded content is
		// the rendered output. When the template references a stamp
		// var, route it through the configure_file stamp_values
		// machinery (live workspace-status re-read at build time)
		// instead of baking the frozen revision.
		if name, ok := stampLiftWriterContent(rel, ch, lc); ok {
			return name, true
		}
		name := bakedBuildDirName(rel)
		t := bakeFileTarget(name, rel, []byte(ch.content), fileWriterBakeTags())
		t.Provenance = writerProvenance(ch, lc)
		cc.Genrules = append(cc.Genrules, t)
		cc.OutToGenrule[rel] = name
		return name, true
	case "copy":
		srcRel, ok := writerCopySourceRel(ch.src, lc)
		if !ok {
			return "", false
		}
		name := "copy_" + sanitizeOutputName(rel)
		cc.Genrules = append(cc.Genrules, ir.Target{
			Name:        name,
			Kind:        ir.KindGenrule,
			Srcs:        []string{srcRel},
			GenruleCmd:  fmt.Sprintf(`mkdir -p "$$(dirname "$@")" && cp "$(location %s)" "$@"`, srcRel),
			GenruleOuts: []string{rel},
			Tags:        fileWriterCopyTags(),
			Provenance:  writerProvenance(ch, lc),
			Visibility:  []string{"//visibility:private"},
		})
		cc.OutToGenrule[rel] = name
		return name, true
	}
	// download (and anything unmodeled): the caller's on-disk bake
	// stands; downloadWriterFor lets it cite the URL.
	return "", false
}

// buildFileWriterTemplates pairs each EXPANDED file(WRITE/APPEND) output
// to its NON-EXPANDED template content and composes per build-relative
// output. The non-expanded trace keeps BOTH the content (`${GIT_SHA}`
// survives — what the stamp lift needs) AND the output PATH
// (`${CMAKE_CURRENT_BINARY_DIR}/...`) unexpanded, so it can't be keyed
// by output rel directly. The expanded and non-expanded traces log the
// same call at the same source site, so pair by (File, Line): the
// expanded call gives the real build-relative output, the non-expanded
// call at the same site gives the template content. WRITE resets,
// APPEND concatenates — composed in expanded trace order.
func buildFileWriterTemplates(expanded, nonExpanded []shadow.FileWriterCall, recordedBuildDir string) map[string]string {
	if len(nonExpanded) == 0 || len(expanded) == 0 {
		return nil
	}
	type site struct {
		file string
		line int
	}
	tmplBySite := map[site]string{}
	for _, c := range nonExpanded {
		if c.Op == "write" || c.Op == "append" {
			tmplBySite[site{c.File, c.Line}] = c.Content
		}
	}
	out := map[string]string{}
	for _, c := range expanded {
		if c.Op != "write" && c.Op != "append" {
			continue
		}
		tmpl, ok := tmplBySite[site{c.File, c.Line}]
		if !ok {
			continue
		}
		// The non-expanded trace logs the RAW cmake source token, so its
		// escapes (\n \t \" \\ …) are literal where the expanded trace
		// already decoded them to real bytes. Decode here so the
		// template aligns with the rendered output; a ${VAR} reference
		// has no backslash and survives.
		tmpl = unescapeCMakeString(tmpl)
		for _, o := range c.Outputs {
			rel, inside := relativeIfInside(recordedBuildDir, o)
			if !inside || rel == "" {
				continue
			}
			if c.Op == "write" {
				out[rel] = tmpl // WRITE truncates+resets
			} else {
				out[rel] += tmpl // APPEND concatenates
			}
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// unescapeCMakeString decodes the quoted-argument escape sequences the
// NON-EXPANDED trace logs verbatim (\n \t \r → control chars; \\ \" \;
// \$ \<other> → the literal char), producing the real bytes the
// EXPANDED trace already decoded — so a non-expanded template aligns
// with its expanded rendered output for pickValues. A ${VAR} reference
// carries no backslash and is left intact.
func unescapeCMakeString(s string) string {
	if !strings.Contains(s, "\\") {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) {
			switch s[i+1] {
			case 'n':
				b.WriteByte('\n')
			case 't':
				b.WriteByte('\t')
			case 'r':
				b.WriteByte('\r')
			default:
				b.WriteByte(s[i+1]) // \\ \" \; \$ \<other> → the char
			}
			i++
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// stampLiftWriterContent routes a stamp-bearing file(WRITE) through the
// configure_file stamp_values machinery: the non-expanded template
// (cc.FileWriterTemplates[rel]) paired with the expanded rendered
// content (ch.content) yields the frozen Values (pickValues) and the
// live StampValues (stampValuesForTemplate over cc.StampVars). Emits a
// CONTENT-form cmake_configure_file so a `@GIT_SHA@`-style write re-reads
// the current revision from the Bazel workspace status at build time.
// Returns ("", false) — leaving the frozen write_file bake — when the
// lift tier is off (the tool isn't staged), no template was captured
// (no warm non-expanded pass), or the template references no stamp var.
func stampLiftWriterContent(rel string, ch writerChain, lc targetLowerCtx) (string, bool) {
	cc := lc.cc
	if !cc.LiftConfigureFile || len(cc.StampVars) == 0 || len(cc.FileWriterTemplates) == 0 {
		return "", false
	}
	template := cc.FileWriterTemplates[rel]
	if template == "" {
		return "", false
	}
	// Raw file(WRITE): cmake had already substituted both @VAR@ and
	// ${VAR} forms, so the re-substitution is both-forms (AtOnly=false).
	opts := configurefile.Options{}
	stampValues := stampValuesForTemplate([]byte(template), opts, cc.StampVars)
	if len(stampValues) == 0 {
		return "", false
	}
	values, ok := pickValues([]byte(template), []byte(ch.content), opts, cc.CMakeVars)
	if !ok {
		return "", false
	}
	name := configureFileGenruleName(rel)
	spec := newConfigureFileSpec(rel, opts)
	spec.Content = template
	spec.Values = values
	spec.StampValues = stampValues
	t := cmakeConfigureFileTarget(name, spec, fileWriterStampTags())
	t.Provenance = writerProvenance(ch, lc)
	cc.Genrules = append(cc.Genrules, t)
	cc.OutToGenrule[rel] = name
	return name, true
}

// downloadWriterFor returns the composed DOWNLOAD chain for rel, when
// that is what produced it — the bake caller cites the URL.
func downloadWriterFor(rel string, lc targetLowerCtx) (writerChain, bool) {
	ch, ok := writerChainFor(rel, lc)
	if !ok || ch.mode != "download" {
		return writerChain{}, false
	}
	return ch, true
}

// writerCopySourceRel anchors a traced copy SOURCE: under the source
// tree it becomes a label-root-relative declared src (umbrella-aware);
// a build-dir source with a registered producer re-points at that
// producer's out. Anything else declines.
func writerCopySourceRel(src string, lc targetLowerCtx) (string, bool) {
	if src == "" {
		return "", false
	}
	if rel, inside := relativeIfInside(lc.cmakeSrc, src); inside && rel != "" {
		_, reanchor := lc.umbrellaReanchor()
		return reanchor(rel), true
	}
	if rel, inside := relativeIfInside(lc.cmakeBuild, src); inside {
		if _, produced := lc.cc.OutToGenrule[rel]; produced {
			return rel, true
		}
	}
	return "", false
}

// writerProvenance records the producing file() call's site as the
// emitted rule's provenance breadcrumb (`# Source: <file>:<line>
// (file(<OP>))` under --emit-provenance).
func writerProvenance(ch writerChain, lc targetLowerCtx) ir.Provenance {
	file := ch.file
	if rel, inside := relativeIfInside(lc.cmakeSrc, file); inside && rel != "" {
		file = rel
	}
	op := map[string]string{"content": "WRITE", "copy": "COPY", "download": "DOWNLOAD"}[ch.mode]
	return ir.Provenance{File: filepath.ToSlash(file), Line: ch.line, Command: "file(" + op + ")"}
}

// fileWriterBakeTags / fileWriterCopyTags are the audit facets: the
// content bake rides the convert-time-bake channels via its registry
// entry; the copy is a TRUE lift (re-runs at build time) and does not.
func fileWriterBakeTags() []string {
	return []string{"cmake-codegen", "cmake-codegen-file-writer-bake"}
}

func fileWriterCopyTags() []string {
	tags := []string{"cmake-codegen", "cmake-codegen-driver=file_copy", "cmake-codegen-file-writer-copy"}
	return tags
}

// downloadBakeTags marks the cited-URL bake of a file(DOWNLOAD) output.
func downloadBakeTags() []string {
	return []string{"cmake-codegen", "cmake-codegen-download-bake"}
}

// fileWriterStampTags marks a stamp-wired file(WRITE): a LIVE
// cmake_configure_file lift (re-reads workspace-status at build time),
// NOT a convert-time bake — so it shares the configure-file facet and
// does NOT ride the bake-warning channel.
func fileWriterStampTags() []string {
	return []string{"cmake-codegen", "cmake-codegen-configure-file", "cmake-codegen-file-writer-stamp"}
}

// writerChainTrustworthy is the verify pass the lift owes its caller
// (the same doctrine as the configure_file byte-equal verify): the
// index only sees the project's OWN file() calls, so an out-of-tree
// module's writer or a refused tool can have contributed bytes the
// composed chain doesn't model. When the live build dir is readable,
// the composed end-state must MATCH the on-disk bytes (content: the
// composed content; copy: the traced source's bytes) — a mismatch
// declines to the on-disk byte-bake, which is always right. Offline
// (no readable bytes), only a GROUNDED chain (full-content base) is
// trusted; an APPEND/TOUCH-created base declines.
func writerChainTrustworthy(rel string, ch writerChain, lc targetLowerCtx) bool {
	if ch.mode == "download" {
		return true // declines to the cited bake regardless
	}
	var want []byte
	switch ch.mode {
	case "content":
		want = []byte(ch.content)
	case "copy":
		srcBytes, err := os.ReadFile(filepath.FromSlash(ch.src))
		if err != nil {
			// Source unreadable: trust the grounded chain (offline
			// staging) — the declared src fails loudly at build time
			// if it's genuinely absent.
			return ch.grounded
		}
		want = srcBytes
	}
	if lc.hostBuild == "" {
		return ch.grounded
	}
	disk, err := os.ReadFile(filepath.Join(lc.hostBuild, filepath.FromSlash(rel)))
	if err != nil {
		return ch.grounded
	}
	return bytes.Equal(disk, want)
}
