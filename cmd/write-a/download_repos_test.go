package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRenderHttpFileRepos(t *testing.T) {
	// Empty → no MODULE.bazel bytes (the bootstrap's first pass / no downloads).
	if got := renderHttpFileRepos(nil); got != "" {
		t.Errorf("empty repos rendered %q; want \"\"", got)
	}
	out := renderHttpFileRepos([]downloadRepoSpec{
		{Repo: "dl_config_h", URL: "https://example.com/config.h", Integrity: "sha256-q8Ej", DownloadedFilePath: "config.h", Rel: "config.h"},
		{Repo: "dl_lib_tar", URL: "https://example.com/lib.tar", DownloadedFilePath: "lib.tar", Rel: "vendor/lib.tar"},
	})
	for _, want := range []string{
		`http_file = use_repo_rule("@bazel_tools//tools/build_defs/repo:http.bzl", "http_file")`,
		`name = "dl_config_h"`,
		`urls = ["https://example.com/config.h"]`,
		`integrity = "sha256-q8Ej"`,
		`downloaded_file_path = "config.h"`,
		`name = "dl_lib_tar"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered MODULE block missing %q:\n%s", want, out)
		}
	}
	// The integrity-less entry omits the attr (rather than integrity = "").
	if strings.Contains(out, `integrity = ""`) {
		t.Errorf("integrity-less repo emitted an empty integrity attr:\n%s", out)
	}
	// use_repo_rule declared exactly once even with multiple repos.
	if n := strings.Count(out, "use_repo_rule("); n != 1 {
		t.Errorf("use_repo_rule declared %d times; want 1:\n%s", n, out)
	}
}

func TestReadDownloadReposLock(t *testing.T) {
	dir := t.TempDir()
	lock := filepath.Join(dir, "download-repos.json")
	// Repos out of name order on disk; readDownloadReposLock sorts by repo.
	if err := os.WriteFile(lock, []byte(`{
  "schema_version": 1,
  "repos": [
    {"repo": "dl_z", "url": "https://x/z", "downloaded_file_path": "z"},
    {"repo": "dl_a", "url": "https://x/a", "integrity": "sha256-aa", "downloaded_file_path": "a"}
  ]
}`), 0o644); err != nil {
		t.Fatal(err)
	}
	repos, err := readDownloadReposLock(lock)
	if err != nil {
		t.Fatalf("readDownloadReposLock: %v", err)
	}
	if len(repos) != 2 || repos[0].Repo != "dl_a" || repos[1].Repo != "dl_z" {
		t.Fatalf("repos not sorted by name: %+v", repos)
	}
	if repos[0].Integrity != "sha256-aa" || repos[0].URL != "https://x/a" {
		t.Errorf("dl_a spec = %+v", repos[0])
	}
	if _, err := readDownloadReposLock(filepath.Join(dir, "missing.json")); err == nil {
		t.Error("missing lockfile must error")
	}
}

func TestUnionDownloadReposLocks(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	// Two elements, each its own lockfile; namespaced repo names don't clash.
	a := write("a.json", `{"schema_version":1,"repos":[{"repo":"dl_elements_foo_config_h","url":"https://x/foo","downloaded_file_path":"config.h"}]}`)
	b := write("b.json", `{"schema_version":1,"repos":[{"repo":"dl_elements_bar_config_h","url":"https://x/bar","integrity":"sha256-bb","downloaded_file_path":"config.h"}]}`)

	repos, err := unionDownloadReposLocks([]string{a, b})
	if err != nil {
		t.Fatalf("union: %v", err)
	}
	// Sorted by repo name: bar before foo.
	if len(repos) != 2 || repos[0].Repo != "dl_elements_bar_config_h" || repos[1].Repo != "dl_elements_foo_config_h" {
		t.Fatalf("union not sorted/complete: %+v", repos)
	}

	// The same lockfile passed twice merges (identical entries), not errors.
	dup, err := unionDownloadReposLocks([]string{a, a})
	if err != nil {
		t.Fatalf("identical-entry union must not error: %v", err)
	}
	if len(dup) != 1 {
		t.Errorf("identical entries must dedup to 1, got %d", len(dup))
	}

	// A genuine name clash (same repo, different url) is a hard error.
	clash := write("clash.json", `{"schema_version":1,"repos":[{"repo":"dl_elements_foo_config_h","url":"https://x/OTHER","downloaded_file_path":"config.h"}]}`)
	if _, err := unionDownloadReposLocks([]string{a, clash}); err == nil {
		t.Error("conflicting repo name with differing url must error")
	}
}

// renderLiftDownloadProjectB renders project B for a trivial kind:cmake
// element with the given cmakeConfig download state, returning project B's
// MODULE.bazel and project A's converter genrule BUILD.
func renderLiftDownloadProjectB(t *testing.T, lift bool, repos []downloadRepoSpec) (moduleBazel, genrule string) {
	t.Helper()
	tmp := t.TempDir()
	srcDir := filepath.Join(tmp, "src")
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "CMakeLists.txt"),
		[]byte("cmake_minimum_required(VERSION 3.20)\nproject(demo C)\nadd_library(thelib STATIC lib.c)\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "lib.c"), []byte("int f(void){return 0;}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	bst := filepath.Join(tmp, "demo.bst")
	if err := os.WriteFile(bst, []byte("kind: cmake\nsources:\n- kind: local\n  path: "+srcDir+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	g, err := loadGraph([]string{bst}, "")
	if err != nil {
		t.Fatalf("loadGraph: %v", err)
	}

	prev := cmakeConfig
	cmakeConfig.liftDownload = lift
	cmakeConfig.downloadRepos = repos
	t.Cleanup(func() { cmakeConfig = prev })

	outA := filepath.Join(tmp, "A")
	if err := writeProjectA(g, outA, fakeConvertBin(t, tmp)); err != nil {
		t.Fatalf("writeProjectA: %v", err)
	}
	outB := filepath.Join(tmp, "B")
	if err := writeProjectB(g, outB); err != nil {
		t.Fatalf("writeProjectB: %v", err)
	}
	mb, err := os.ReadFile(filepath.Join(outB, "MODULE.bazel"))
	if err != nil {
		t.Fatal(err)
	}
	gr, err := os.ReadFile(filepath.Join(outA, "elements/demo/BUILD.bazel"))
	if err != nil {
		t.Fatal(err)
	}
	return string(mb), string(gr)
}

// TestWriter_LiftDownload_Wired covers --lift-download + --download-repos-lock:
// the converter genrule threads --lift-download + --out-download-repos and
// declares the download-repos.json output, and project B's MODULE.bazel
// declares the http_file repos from the committed lockfile.
func TestWriter_LiftDownload_Wired(t *testing.T) {
	repos := []downloadRepoSpec{
		{Repo: "dl_config_h", URL: "https://example.com/config.h", Integrity: "sha256-q8Ej", DownloadedFilePath: "config.h", Rel: "config.h"},
	}
	mb, gr := renderLiftDownloadProjectB(t, true, repos)

	for _, want := range []string{
		"--lift-download=true",
		`--out-download-repos="$(location download-repos.json)"`,
		`"download-repos.json",`,
	} {
		if !strings.Contains(gr, want) {
			t.Errorf("converter genrule missing %q:\n%s", want, gr)
		}
	}
	for _, want := range []string{
		`http_file = use_repo_rule("@bazel_tools//tools/build_defs/repo:http.bzl", "http_file")`,
		`name = "dl_config_h"`,
		`integrity = "sha256-q8Ej"`,
	} {
		if !strings.Contains(mb, want) {
			t.Errorf("project B MODULE.bazel missing %q:\n%s", want, mb)
		}
	}
}

// TestWriter_LiftDownload_Off pins the default: no genrule threading and no
// http_file blocks in MODULE.bazel (byte-shape guarantee for the off path).
func TestWriter_LiftDownload_Off(t *testing.T) {
	mb, gr := renderLiftDownloadProjectB(t, false, nil)
	if strings.Contains(gr, "lift-download") || strings.Contains(gr, "download-repos.json") {
		t.Errorf("converter genrule unexpectedly mentions the download lift with flag off:\n%s", gr)
	}
	if strings.Contains(mb, "http_file") || strings.Contains(mb, "use_repo_rule") {
		t.Errorf("MODULE.bazel unexpectedly declares http_file repos with flag off:\n%s", mb)
	}
}
