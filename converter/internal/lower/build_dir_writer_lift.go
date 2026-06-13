package lower

import (
	"fmt"
	"path/filepath"

	"github.com/sstriker/buildstream-bazel/converter/ir"
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
}

// composeWriterChain folds a path's writer calls (trace order) into the
// final state cmake left the file in.
func composeWriterChain(calls []shadow.FileWriterCall) writerChain {
	var ch writerChain
	for _, c := range calls {
		ch.file, ch.line = c.File, c.Line
		switch c.Op {
		case "write":
			ch = writerChain{mode: "content", content: c.Content, file: c.File, line: c.Line}
		case "append":
			switch ch.mode {
			case "content":
				ch.content += c.Content
			case "":
				// APPEND to a not-yet-written path creates it.
				ch.mode, ch.content = "content", c.Content
			default:
				// APPEND onto a copy/download: composing would need
				// the source bytes; decline the whole chain.
				return writerChain{}
			}
		case "touch":
			if ch.mode == "" {
				ch.mode, ch.content = "content", ""
			} // an existing file is left untouched (no truncation)
		case "copy", "copy_file":
			ch = writerChain{mode: "copy", file: c.File, line: c.Line}
			for i, out := range c.Outputs {
				_ = out
				if i < len(c.Sources) {
					ch.src = c.Sources[i]
				}
			}
			if len(c.Sources) == 1 {
				ch.src = c.Sources[0]
			}
		case "download":
			ch = writerChain{mode: "download", url: c.URL, file: c.File, line: c.Line}
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
	switch ch.mode {
	case "content":
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
