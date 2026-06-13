package lower

import (
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/sstriker/buildstream-bazel/converter/internal/todos"
	"github.com/sstriker/buildstream-bazel/converter/ir"
)

// file(DOWNLOAD) recovery has two shapes. The DEFAULT is the hermetic
// byte-bake (no network at build time — what the default downstream
// envelopes rely on); the trace carries the URL and EXPECTED_HASH, which
// is exactly the http_file repo's `urls` + `integrity`. The OPT-IN
// (--lift-download) is the faithful repository-rule form: bakeBuildDirFile
// rewires the producer to a genrule copying @<repo>//file from an
// http_file repo, and recordDownloadLift stashes the spec so:
//   - the CLI serializes download-repos.json (the lockfile the staged
//     download_repos.bzl module extension reads at bzlmod-eval time and
//     write-a's use_repo enumerates), and
//   - emitDownloadLiftTodos still surfaces a `download` todo (with the
//     ready-to-paste stanza) for the bake-default case where an author
//     wants to flip a single download by hand.

// downloadLiftRecord is one recovered file(DOWNLOAD) output: the build-rel
// path, its URL, and the traced EXPECTED_HASH ("<algo>=<value>", "" if
// absent).
type downloadLiftRecord struct {
	Rel  string
	URL  string
	Hash string
}

// DownloadRepoSpec is one http_file repo the lift declares: the lockfile
// entry the download_repos.bzl module extension turns into an http_file
// call and write-a's use_repo enumerates. Integrity is the Subresource
// Integrity string http_file's `integrity` attr accepts (empty when the
// cmake call carried no SRI-capable EXPECTED_HASH).
type DownloadRepoSpec struct {
	Repo               string `json:"repo"`
	URL                string `json:"url"`
	Integrity          string `json:"integrity,omitempty"`
	DownloadedFilePath string `json:"downloaded_file_path"`
	Rel                string `json:"rel"`
}

// recordDownloadLift stashes a recovered download for the end-of-lower
// lockfile + todo.
func recordDownloadLift(cc *codegenContext, rel, url, hash string) {
	cc.DownloadLifts = append(cc.DownloadLifts, downloadLiftRecord{Rel: rel, URL: url, Hash: hash})
}

// downloadRepoSpecs turns the recorded downloads into the sorted lockfile
// entries (stable by output rel — the byte-identical-report contract).
func downloadRepoSpecs(pkgPath string, recs []downloadLiftRecord) []DownloadRepoSpec {
	if len(recs) == 0 {
		return nil
	}
	sorted := append([]downloadLiftRecord(nil), recs...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Rel < sorted[j].Rel })
	out := make([]DownloadRepoSpec, 0, len(sorted))
	for _, r := range sorted {
		spec := DownloadRepoSpec{
			Repo:               downloadRepoName(pkgPath, r.Rel),
			URL:                r.URL,
			DownloadedFilePath: filepath.Base(r.Rel),
			Rel:                r.Rel,
		}
		if sri, ok := downloadIntegritySRI(r.Hash); ok {
			spec.Integrity = sri
		}
		out = append(out, spec)
	}
	return out
}

// downloadFetchTarget builds the --lift-download producer: a genrule that
// copies the http_file repo's downloaded bytes (@<repo>//file) to the same
// build-relative path the byte-bake would have produced, so consumers wire
// through OutToGenrule unchanged. A true build-time fetch (the http_file
// repo rule fetches + verifies integrity at repo-fetch time), so it does
// NOT ride the convert-time-bake warning channel.
func downloadFetchTarget(name, outRel, repo string) ir.Target {
	return ir.Target{
		Name:        name,
		Kind:        ir.KindGenrule,
		Tags:        downloadFetchTags(),
		Visibility:  []string{"//visibility:private"},
		Srcs:        []string{"@" + repo + "//file"},
		GenruleOuts: []string{outRel},
		GenruleCmd:  "cp $(SRCS) $@",
	}
}

// downloadFetchTags marks the --lift-download fetch genrule. The
// download_fetch driver facet mirrors the file_copy lift: a real build-time
// action, not a convert-time bake.
func downloadFetchTags() []string {
	return []string{"cmake-codegen", "cmake-codegen-driver=download_fetch", "cmake-codegen-download-fetch"}
}

// emitDownloadLiftTodos surfaces the http_file hand-off: one Actionable
// `download` todo per recovered file(DOWNLOAD), with the ready-to-paste
// MODULE stanza in SuggestedShape. (Independent of --lift-download — under
// the lift the producer is already the repo-rule form, but the todo still
// documents the upstream URL + integrity.)
func emitDownloadLiftTodos(c *todos.Collector, pkgPath string, recs []downloadLiftRecord) {
	if c == nil || len(recs) == 0 {
		return
	}
	sorted := append([]downloadLiftRecord(nil), recs...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Rel < sorted[j].Rel })
	for _, r := range sorted {
		repo := downloadRepoName(pkgPath, r.Rel)
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
				"reference @" + repo + "//file:downloaded instead of the baked " + r.Rel + " (or pass --lift-download to do this for every download).",
		})
	}
}

// httpFileStanza renders the ready-to-paste MODULE-extension http_file
// declaration for one download: the URL plus the integrity translated from
// cmake's EXPECTED_HASH.
func httpFileStanza(repo string, r downloadLiftRecord) string {
	var b strings.Builder
	b.WriteString("# MODULE.bazel:\n")
	b.WriteString(`http_file = use_repo_rule("@bazel_tools//tools/build_defs/repo:http.bzl", "http_file")` + "\n")
	b.WriteString("http_file(\n")
	b.WriteString("    name = " + starlarkStr(repo) + ",\n")
	b.WriteString("    urls = [" + starlarkStr(r.URL) + "],\n")
	if sri, ok := downloadIntegritySRI(r.Hash); ok {
		b.WriteString("    integrity = " + starlarkStr(sri) + ",\n")
	} else if r.Hash != "" {
		b.WriteString("    # EXPECTED_HASH " + r.Hash + " — not an SRI-capable algorithm (SHA256/384/512); verify manually\n")
	} else {
		b.WriteString("    # no EXPECTED_HASH in the cmake call — add integrity for a reproducible fetch\n")
	}
	b.WriteString("    downloaded_file_path = " + starlarkStr(filepath.Base(r.Rel)) + ",\n")
	b.WriteString(")\n")
	b.WriteString("# Then reference @" + repo + "//file in place of the baked output.")
	return b.String()
}

// downloadIntegritySRI maps cmake's EXPECTED_HASH "<algo>=<hexvalue>" to a
// Subresource-Integrity string ("<algo>-<base64(rawdigest)>"), the form
// http_file's `integrity` attr accepts. SHA256/384/512 all resolve via the
// mechanical hex → bytes → base64 transform (not just SHA256). MD5/SHA1
// have no SRI form, and a malformed hex value can't be transformed →
// ok=false (the caller emits a verify-manually note).
func downloadIntegritySRI(hash string) (sri string, ok bool) {
	algo, value, found := strings.Cut(hash, "=")
	if !found {
		return "", false
	}
	var prefix string
	switch strings.ToUpper(strings.TrimSpace(algo)) {
	case "SHA256":
		prefix = "sha256"
	case "SHA384":
		prefix = "sha384"
	case "SHA512":
		prefix = "sha512"
	default:
		return "", false
	}
	raw, err := hex.DecodeString(strings.TrimSpace(value))
	if err != nil || len(raw) == 0 {
		return "", false
	}
	return prefix + "-" + base64.StdEncoding.EncodeToString(raw), true
}

// downloadRepoName derives a globally-unique http_file repo name from the
// element's bazel package path + the output path. The package-path prefix
// is what makes a multi-element envelope safe: build-dir-relative outputs
// commonly collide (two elements each writing config.h → the same rel), so
// keying on rel alone would clash in project B's project-wide repo
// namespace. A root/empty package path (standalone convert, e.g. the
// render gate) stays the bare "dl_<rel>" form.
func downloadRepoName(pkgPath, rel string) string {
	if pkgPathIsRoot(pkgPath) {
		return "dl_" + sanitizeOutputName(rel)
	}
	return "dl_" + sanitizeOutputName(pkgPath) + "_" + sanitizeOutputName(rel)
}

// starlarkStr quotes a string as a Starlark string literal.
func starlarkStr(s string) string {
	return fmt.Sprintf("%q", s)
}
