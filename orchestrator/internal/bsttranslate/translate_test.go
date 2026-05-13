package bsttranslate

import (
	"reflect"
	"testing"

	"github.com/sstriker/buildstream-bazel/orchestrator/internal/element"
)

func TestTranslateElement_GitSource(t *testing.T) {
	in := &element.Element{
		Name: "components/hello",
		Kind: "cmake",
		Sources: []element.Source{
			{
				Kind: "git",
				URL:  "https://github.com/example/hello.git",
				Ref:  "deadbeef",
				Extra: map[string]any{
					"track": "main",
				},
			},
		},
	}
	out, err := TranslateElement(in)
	if err != nil {
		t.Fatalf("TranslateElement: %v", err)
	}
	if len(out.Sources) != 1 {
		t.Fatalf("len(Sources) = %d, want 1", len(out.Sources))
	}
	src := out.Sources[0]
	if src.Kind != "remote-asset" {
		t.Errorf("Kind = %q, want remote-asset", src.Kind)
	}
	if got := src.Extra["uri"]; got != "bst:source:components/hello" {
		t.Errorf("uri = %v, want bst:source:components/hello", got)
	}
	q := src.Extra["qualifiers"].(map[string]any)
	want := map[string]any{
		"bst-source-kind":  "git",
		"bst-source-url":   "https://github.com/example/hello.git",
		"bst-source-ref":   "deadbeef",
		"bst-source-track": "main",
	}
	if !reflect.DeepEqual(q, want) {
		t.Errorf("qualifiers mismatch:\n  got:  %v\n  want: %v", q, want)
	}
}

func TestTranslateElement_LocalPassthrough(t *testing.T) {
	in := &element.Element{
		Name: "components/hello",
		Sources: []element.Source{
			{Kind: "local", Extra: map[string]any{"path": "../files/hello"}},
		},
	}
	out, err := TranslateElement(in)
	if err != nil {
		t.Fatalf("TranslateElement: %v", err)
	}
	if out.Sources[0].Kind != "local" {
		t.Errorf("local source got rewritten: %+v", out.Sources[0])
	}
}

func TestTranslateElement_RemoteAssetPassthrough(t *testing.T) {
	in := &element.Element{
		Name: "components/hello",
		Sources: []element.Source{
			{Kind: "remote-asset", Extra: map[string]any{"uri": "bst:source:components/hello"}},
		},
	}
	out, err := TranslateElement(in)
	if err != nil {
		t.Fatalf("TranslateElement: %v", err)
	}
	if out.Sources[0].Kind != "remote-asset" {
		t.Errorf("remote-asset got mutated: %+v", out.Sources[0])
	}
}

func TestTranslateElement_MissingURL(t *testing.T) {
	in := &element.Element{
		Name: "x",
		Sources: []element.Source{
			{Kind: "git", Ref: "abc"},
		},
	}
	if _, err := TranslateElement(in); err == nil {
		t.Fatal("expected error for missing url")
	}
}

func TestTranslateElement_MultiSourceUsesIndex(t *testing.T) {
	in := &element.Element{
		Name: "components/multi",
		Sources: []element.Source{
			{Kind: "git", URL: "https://example/a.git", Ref: "a1"},
			{Kind: "git", URL: "https://example/b.git", Ref: "b2"},
		},
	}
	out, err := TranslateElement(in)
	if err != nil {
		t.Fatalf("TranslateElement: %v", err)
	}
	uri0 := out.Sources[0].Extra["uri"].(string)
	uri1 := out.Sources[1].Extra["uri"].(string)
	if uri0 != "bst:source:components/multi:0" {
		t.Errorf("source[0].uri = %q, want bst:source:components/multi:0", uri0)
	}
	if uri1 != "bst:source:components/multi:1" {
		t.Errorf("source[1].uri = %q, want bst:source:components/multi:1", uri1)
	}
}

func TestTranslateElement_DoesNotModifyInput(t *testing.T) {
	in := &element.Element{
		Name: "x",
		Sources: []element.Source{
			{Kind: "git", URL: "https://example/x.git", Ref: "abc"},
		},
	}
	if _, err := TranslateElement(in); err != nil {
		t.Fatalf("TranslateElement: %v", err)
	}
	if in.Sources[0].Kind != "git" {
		t.Errorf("input element mutated: %+v", in)
	}
}

func TestQualifierKeys_Sorted(t *testing.T) {
	src := element.Source{
		Kind: "remote-asset",
		Extra: map[string]any{
			"qualifiers": map[string]any{
				"bst-source-url":  "u",
				"bst-source-kind": "git",
				"bst-source-ref":  "r",
			},
		},
	}
	got := QualifierKeys(src)
	want := []string{"bst-source-kind", "bst-source-ref", "bst-source-url"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}
