package main

import (
	"encoding/json"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestCollectSources_DedupAndOrder(t *testing.T) {
	// Two elements share one source; one element has its own;
	// kind:local gets excluded.
	shared := resolvedSource{
		Kind: "git_repo",
		URL:  "https://github.com/foo/bar.git",
		Ref:  yaml.Node{Kind: yaml.ScalarNode, Value: "v1.2.3"},
	}
	other := resolvedSource{
		Kind: "tar",
		URL:  "https://example.org/baz.tar.gz",
		Ref:  yaml.Node{Kind: yaml.ScalarNode, Value: "abc123"},
	}
	local := resolvedSource{Kind: "local", AbsPath: "/some/local/dir"}

	g := &graph{Elements: []*element{
		{Sources: []resolvedSource{shared, local}},
		{Sources: []resolvedSource{shared, other}},
	}}
	got := collectSources(g)

	if len(got.Sources) != 2 {
		t.Fatalf("want 2 unique non-local sources; got %d (%+v)", len(got.Sources), got.Sources)
	}

	// Output is key-sorted; verify by checking the keys ascend.
	if got.Sources[0].Key >= got.Sources[1].Key {
		t.Errorf("entries should be key-sorted; got %q then %q",
			got.Sources[0].Key, got.Sources[1].Key)
	}

	// kind:local entries are excluded — every key here is non-empty
	// (sourceKey returns "" for kind:local).
	for _, e := range got.Sources {
		if e.Key == "" {
			t.Errorf("kind:local should not produce an entry; got %+v", e)
		}
		if e.Kind == "local" {
			t.Errorf("kind:local entry leaked: %+v", e)
		}
	}
}

func TestCollectSources_EmptyGraph(t *testing.T) {
	got := collectSources(&graph{Elements: nil})
	if got.Sources == nil {
		t.Errorf("collectSources on empty graph should return zero-length slice, not nil; got %v", got.Sources)
	}
	if len(got.Sources) != 0 {
		t.Errorf("empty graph should have no entries; got %d", len(got.Sources))
	}
}

func TestMarshalSourcesJSON_Deterministic(t *testing.T) {
	s := sourcesJSON{Sources: []sourceEntry{
		{Key: "aaa", Kind: "git_repo", URL: "https://g/a.git", Ref: "v1"},
		{Key: "bbb", Kind: "tar", URL: "https://g/b.tar", Ref: "abc"},
	}}
	a, err := marshalSourcesJSON(s)
	if err != nil {
		t.Fatal(err)
	}
	b, err := marshalSourcesJSON(s)
	if err != nil {
		t.Fatal(err)
	}
	if string(a) != string(b) {
		t.Errorf("marshalSourcesJSON not deterministic")
	}
	// Schema sanity: top-level "sources" array, parseable.
	var round struct {
		Sources []sourceEntry `json:"sources"`
	}
	if err := json.Unmarshal(a, &round); err != nil {
		t.Fatalf("emitted JSON doesn't round-trip: %v", err)
	}
	if len(round.Sources) != 2 || round.Sources[0].Key != "aaa" {
		t.Errorf("round-trip mismatch: %+v", round)
	}
	// Trailing newline (POSIX text-file convention).
	if a[len(a)-1] != '\n' {
		t.Errorf("expected trailing newline")
	}
}

func TestMarshalSourcesJSON_OmitsEmptyOptionalFields(t *testing.T) {
	s := sourcesJSON{Sources: []sourceEntry{
		{Key: "aaa", Kind: "git_repo", URL: "https://g/a.git"}, // no Ref/Track/Digest
	}}
	out, err := marshalSourcesJSON(s)
	if err != nil {
		t.Fatal(err)
	}
	str := string(out)
	if strings.Contains(str, `"ref"`) || strings.Contains(str, `"track"`) || strings.Contains(str, `"digest"`) {
		t.Errorf("optional fields with empty values should be omitted; got %s", str)
	}
}

func TestRenderSourcesUseExtension_EmitsUseRepo(t *testing.T) {
	s := sourcesJSON{Sources: []sourceEntry{
		{Key: "aaa"}, {Key: "bbb"},
	}}
	got := renderSourcesUseExtension(s)
	for _, want := range []string{
		`use_extension("//rules:sources.bzl", "sources")`,
		`sources.from_json(path = "//tools:sources.json")`,
		`use_repo(`,
		`"src_aaa"`,
		`"src_bbb"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}

func TestRenderSourcesUseExtension_EmptyGraphSkipsBlock(t *testing.T) {
	if got := renderSourcesUseExtension(sourcesJSON{}); got != "" {
		t.Errorf("zero sources should emit nothing; got %q", got)
	}
}

func TestRenderSourcesBzl_StarlarkShape(t *testing.T) {
	got := renderSourcesBzl()
	for _, want := range []string{
		"def _src_repo_impl(rctx):",
		"_src_repo = repository_rule(",
		"def _sources_impl(module_ctx):",
		"json.decode(raw)",
		`"from_json": tag_class(`,
		"sources = module_extension(",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("rendered .bzl missing %q", want)
		}
	}
}

// TestRenderSourcesBzl_ParameterizesPathPrefix verifies the
// rule reads CAS_DIRECTORY_PREFIX (default "blobs") and uses
// it to build the symlink target. The default keeps cmd/cas-fuse
// users on the historical `<mount>/blobs/directory/<digest>`
// layout; bb_clientd users override the env to land on
// `<mount>/cas/<instance>/blobs/<digest_function>/directory/<digest>`.
func TestRenderSourcesBzl_ParameterizesPathPrefix(t *testing.T) {
	got := renderSourcesBzl()
	for _, want := range []string{
		`rctx.os.environ.get("CAS_DIRECTORY_PREFIX", "blobs")`,
		`mount + "/" + prefix + "/directory/" + digest`,
		`environ = ["CAS_FUSE_MOUNT", "CAS_DIRECTORY_PREFIX"]`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("rendered .bzl missing parameterized-prefix marker %q\n%s", want, got)
		}
	}
	// The pre-parameterization hardcoded shape must NOT survive.
	if strings.Contains(got, `mount + "/blobs/directory/" + digest`) {
		t.Errorf("rendered .bzl still contains the hardcoded /blobs/directory/ shape; should use the prefix var")
	}
}

// TestRenderTracesBzl_ParameterizesPathPrefix is the round-2
// counterpart: the _trace_repo rule must read the same
// CAS_DIRECTORY_PREFIX env so a single --repo_env flag covers
// both extensions.
func TestRenderTracesBzl_ParameterizesPathPrefix(t *testing.T) {
	got := renderTracesBzl()
	for _, want := range []string{
		`rctx.os.environ.get("CAS_DIRECTORY_PREFIX", "blobs")`,
		`mount + "/" + prefix + "/directory/" + hash`,
		`"CAS_DIRECTORY_PREFIX"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("rendered traces.bzl missing parameterized-prefix marker %q\n%s", want, got)
		}
	}
	if strings.Contains(got, `mount + "/blobs/directory/" + hash`) {
		t.Errorf("rendered traces.bzl still contains the hardcoded /blobs/directory/ shape")
	}
}

// TestRenderTracesBzl_AttrPlatformOverridesEnv: under multi-
// platform write-a renders distinct _trace_repo instances per
// (element, platform) cell, each with a pinned `platform` attr.
// The repo rule must prefer the attr over the env var so two
// per-platform repos in one Bazel invocation don't collide on
// CMAKE_TO_BAZEL_PLATFORM. Empty attr keeps the legacy env-var
// fallback for single-platform operators.
func TestRenderTracesBzl_AttrPlatformOverridesEnv(t *testing.T) {
	got := renderTracesBzl()
	for _, want := range []string{
		// The repository rule declares the platform attr with an
		// empty-string default so legacy single-platform renders
		// (which don't populate it) keep working.
		`"platform": attr.string(default = "")`,
		// The impl prefers the attr; only falls back to env when
		// the attr is empty.
		`platform_tag = rctx.attr.platform`,
		`if platform_tag == "":`,
		`platform_tag = rctx.os.environ.get("CMAKE_TO_BAZEL_PLATFORM", "")`,
		// The module extension forwards the per-entry platform
		// field from traces.json to the rule. Legacy entries
		// without the field default to "" via .get().
		`platform = entry.get("platform", ""),`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("rendered traces.bzl missing per-platform marker %q\n%s", want, got)
		}
	}
}
