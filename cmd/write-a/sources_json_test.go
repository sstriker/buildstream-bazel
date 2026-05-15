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

// The rendered .bzl shape was previously asserted via tests that
// inspected the in-Go renderSourcesBzl / renderTracesBzl strings.
// Those renderers retired when the rules moved to the in-repo
// rules_buildstream_bazel package — checked-in starlark files are
// the source of truth now, and the per-element BUILD templates
// load them via `@rules_buildstream_bazel//rules:*.bzl` rather
// than as a rendered project-A / project-B `rules/` directory.
// The static .bzl files don't need go-test coverage; they ship
// as version-controlled content alongside this code.
