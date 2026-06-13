package main

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
)

// downloadRepoSpec mirrors converter/internal/lower.DownloadRepoSpec — one
// entry of the file(DOWNLOAD) lockfile (download-repos.json) the converter
// emits under --out-download-repos. write-a sits outside the converter
// module's internal/ boundary and can't import the type, so the shape is
// duplicated (a schema drift would surface as a missing field at render
// time, caught by the gate).
type downloadRepoSpec struct {
	Repo               string `json:"repo"`
	URL                string `json:"url"`
	Integrity          string `json:"integrity,omitempty"`
	DownloadedFilePath string `json:"downloaded_file_path"`
	Rel                string `json:"rel"`
}

// downloadReposLockDoc is the lockfile envelope.
type downloadReposLockDoc struct {
	SchemaVersion int                `json:"schema_version"`
	Repos         []downloadRepoSpec `json:"repos"`
}

// readDownloadReposLock parses the committed file(DOWNLOAD) lockfile passed
// via --download-repos-lock, returning the repos sorted by name for a
// byte-stable MODULE.bazel render.
func readDownloadReposLock(path string) ([]downloadRepoSpec, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var doc downloadReposLockDoc
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	repos := append([]downloadRepoSpec(nil), doc.Repos...)
	sort.Slice(repos, func(i, j int) bool { return repos[i].Repo < repos[j].Repo })
	return repos, nil
}

// renderHttpFileRepos emits the MODULE.bazel block declaring one stock
// http_file repo per committed download via use_repo_rule (the idiomatic
// Bazel 7.1+ inline form — no module extension, no `bazel mod tidy`). The
// @<repo>//file labels the --lift-download fetch genrules reference resolve
// against these; the bytes fetch + integrity-verify at repo-rule time.
// Empty when no downloads were committed (the bootstrap's first pass).
func renderHttpFileRepos(repos []downloadRepoSpec) string {
	if len(repos) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString(`
# file(DOWNLOAD) http_file repos, declared from the committed
# download-repos.json lockfile (--download-repos-lock). The --lift-download
# converter genrules reference @<repo>//file in place of byte-baking; the
# bytes fetch + integrity-verify at repo-rule time.
http_file = use_repo_rule("@bazel_tools//tools/build_defs/repo:http.bzl", "http_file")
`)
	for _, r := range repos {
		fmt.Fprintf(&b, "http_file(\n    name = %q,\n    urls = [%q],\n", r.Repo, r.URL)
		if r.Integrity != "" {
			fmt.Fprintf(&b, "    integrity = %q,\n", r.Integrity)
		}
		fmt.Fprintf(&b, "    downloaded_file_path = %q,\n)\n", r.DownloadedFilePath)
	}
	return b.String()
}
