package lower

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/sstriker/buildstream-bazel/converter/internal/todos"
)

// file(DOWNLOAD) stays a hermetic byte-bake by policy (no network at
// build time — the default downstream envelopes rely on). But the
// faithful repository-rule form is mechanical when an operator WANTS
// it: the trace carries the URL and the EXPECTED_HASH, which is exactly
// the http_file stanza's `urls` + `integrity`. recordDownloadLift
// stashes each baked download; emitDownloadLiftTodos surfaces one
// Actionable `download` todo per output carrying the ready-to-paste
// MODULE-extension http_file stanza and the @repo//file label that
// replaces the baked rule — so an author flips to the repo rule with a
// copy/paste rather than reconstructing it.

// downloadLiftRecord is one baked file(DOWNLOAD) output: the build-rel
// path, its URL, and the traced EXPECTED_HASH ("<algo>=<value>", "" if
// absent).
type downloadLiftRecord struct {
	Rel  string
	URL  string
	Hash string
}

// recordDownloadLift stashes a baked download for the end-of-lower todo.
func recordDownloadLift(cc *codegenContext, rel, url, hash string) {
	cc.DownloadLifts = append(cc.DownloadLifts, downloadLiftRecord{Rel: rel, URL: url, Hash: hash})
}

// emitDownloadLiftTodos surfaces the http_file hand-off: one Actionable
// `download` todo per baked file(DOWNLOAD), with the ready-to-paste
// MODULE stanza in SuggestedShape.
func emitDownloadLiftTodos(c *todos.Collector, recs []downloadLiftRecord) {
	if c == nil || len(recs) == 0 {
		return
	}
	sorted := append([]downloadLiftRecord(nil), recs...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Rel < sorted[j].Rel })
	for _, r := range sorted {
		repo := downloadRepoName(r.Rel)
		c.Add(todos.Todo{
			Kind:        "download",
			Disposition: todos.Improvement,
			GroupKey:    r.Rel,
			Anchors:     []todos.Anchor{{Construct: "file(DOWNLOAD " + r.URL + ")"}},
			Evidence: map[string]any{
				"output":    r.Rel,
				"url":       r.URL,
				"integrity": r.Hash,
				"repo":      repo,
			},
			SuggestedShape: httpFileStanza(repo, r),
			Prompt: "file(DOWNLOAD) baked the fetched bytes at convert time (hermetic — no network at build time). " +
				"To track the upstream URL instead, declare the http_file repo from the stanza in suggested_shape and " +
				"reference @" + repo + "//file:downloaded instead of the baked " + r.Rel + ".",
		})
	}
}

// httpFileStanza renders the ready-to-paste MODULE-extension http_file
// declaration for one download: the URL plus the integrity translated
// from cmake's EXPECTED_HASH (SHA256/384/512 → the `integrity`/`sha256`
// attr; MD5/SHA1 carry a verify-manually note — http_file has no attr).
func httpFileStanza(repo string, r downloadLiftRecord) string {
	var b strings.Builder
	b.WriteString("# MODULE.bazel:\n")
	b.WriteString(`http_file = use_repo_rule("@bazel_tools//tools/build_defs/repo:http.bzl", "http_file")` + "\n")
	b.WriteString("http_file(\n")
	b.WriteString("    name = " + starlarkStr(repo) + ",\n")
	b.WriteString("    urls = [" + starlarkStr(r.URL) + "],\n")
	if attr, val, ok := httpFileIntegrity(r.Hash); ok {
		b.WriteString("    " + attr + " = " + starlarkStr(val) + ",\n")
	} else if r.Hash != "" {
		b.WriteString("    # EXPECTED_HASH " + r.Hash + " — http_file has no attr for this algorithm; verify manually or recompute sha256\n")
	} else {
		b.WriteString("    # no EXPECTED_HASH in the cmake call — add sha256/integrity for a reproducible fetch\n")
	}
	b.WriteString("    downloaded_file_path = " + starlarkStr(filepath.Base(r.Rel)) + ",\n")
	b.WriteString(")\n")
	b.WriteString("# Then reference @" + repo + "//file in place of the baked output.")
	return b.String()
}

// httpFileIntegrity maps cmake's EXPECTED_HASH "<algo>=<value>" to the
// http_file attribute + value. SHA256 → sha256 (hex); SHA384/SHA512 →
// integrity (SRI "<algo>-<base64>" can't be derived from hex here, so
// emit the hex under a sha-keyed note via sha256 only). Returns ok=false
// for algorithms http_file can't take directly.
func httpFileIntegrity(hash string) (attr, val string, ok bool) {
	algo, value, found := strings.Cut(hash, "=")
	if !found {
		return "", "", false
	}
	switch strings.ToUpper(algo) {
	case "SHA256":
		return "sha256", value, true
	default:
		return "", "", false
	}
}

// downloadRepoName derives a stable http_file repo name from the output
// path (sanitized, "dl_" prefixed).
func downloadRepoName(rel string) string {
	return "dl_" + sanitizeOutputName(rel)
}

// starlarkStr quotes a string as a Starlark string literal.
func starlarkStr(s string) string {
	return fmt.Sprintf("%q", s)
}
