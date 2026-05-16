package lower

import (
	"reflect"
	"strings"
	"testing"
)

func TestTopLevelGenexes(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string // genex literals at each detected range
	}{
		{
			name: "empty",
			in:   "",
			want: nil,
		},
		{
			name: "no genex",
			in:   "plain text\nwith @VAR@ and ${VAR}",
			want: nil,
		},
		{
			name: "single top-level",
			in:   "Hello $<CONFIG:Release> world",
			want: []string{"$<CONFIG:Release>"},
		},
		{
			name: "two top-level",
			in:   "$<CONFIG> = $<COMPILER_ID:GNU>",
			want: []string{"$<CONFIG>", "$<COMPILER_ID:GNU>"},
		},
		{
			name: "nested counted once",
			// $<IF:$<CONFIG:Release>,a,b> is a single top-level
			// genex; the inner $<CONFIG:Release> is collapsed
			// into the parent's resolved value at cmake-render
			// time and the extractor doesn't need to track it
			// independently.
			in:   "x=$<IF:$<CONFIG:Release>,a,b>;",
			want: []string{"$<IF:$<CONFIG:Release>,a,b>"},
		},
		{
			name: "deeply nested",
			in:   "$<IF:$<AND:$<CONFIG:Release>,$<COMPILER_ID:GNU>>,y,n>",
			want: []string{"$<IF:$<AND:$<CONFIG:Release>,$<COMPILER_ID:GNU>>,y,n>"},
		},
		{
			name: "unbalanced opener then balanced",
			// `$<CONFIG` with no closing `>` consumes the
			// scanner's nesting tracker but never adds a range.
			// The scanner falls back to literal advance, picks
			// up the later balanced `$<CONFIG:Release>` cleanly.
			// This shape doesn't occur in real cmake templates
			// (cmake's parser would reject the file) but the
			// extractor stays defensive.
			in:   "broken $<CONFIG and $<CONFIG:Release> after",
			want: []string{"$<CONFIG:Release>"},
		},
		{
			name: "lone $ no <",
			in:   "$ alone, $$ pair, $<X>",
			want: []string{"$<X>"},
		},
		{
			name: "literal $< text inside genex stays inside",
			// Inner `$<` is interpreted by the depth counter as a
			// nested opener (cmake's grammar always treats `$<`
			// as a genex opener inside another genex). The outer
			// range covers the whole thing.
			in:   "$<X:$<Y>>",
			want: []string{"$<X:$<Y>>"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ranges := topLevelGenexes([]byte(c.in))
			var got []string
			for _, r := range ranges {
				got = append(got, c.in[r.start:r.end])
			}
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("topLevelGenexes(%q):\n  got:  %#v\n  want: %#v", c.in, got, c.want)
			}
		})
	}
}

func TestExtractGenexValues(t *testing.T) {
	cases := []struct {
		name     string
		template string
		rendered string
		want     map[string]string
		wantErr  string // substring; "" = success
	}{
		{
			name:     "single genex, static surround",
			template: "Hello $<CONFIG:Release> world",
			rendered: "Hello 1 world",
			want:     map[string]string{"$<CONFIG:Release>": "1"},
		},
		{
			name:     "two genexes with static separator",
			template: "cfg=$<CONFIG>;cc=$<COMPILER_ID:GNU>",
			rendered: "cfg=Release;cc=1",
			want: map[string]string{
				"$<CONFIG>":          "Release",
				"$<COMPILER_ID:GNU>": "1",
			},
		},
		{
			name:     "genex at template start",
			template: "$<CONFIG> trailing",
			rendered: "Release trailing",
			want:     map[string]string{"$<CONFIG>": "Release"},
		},
		{
			name:     "genex at template end",
			template: "leading $<CONFIG>",
			rendered: "leading Release",
			want:     map[string]string{"$<CONFIG>": "Release"},
		},
		{
			name:     "template is entirely a genex",
			template: "$<CONFIG>",
			rendered: "Release",
			want:     map[string]string{"$<CONFIG>": "Release"},
		},
		{
			name:     "multiline template",
			template: "// build: $<CONFIG>\n#define ARCH \"$<PLATFORM_ID:Linux>\"\n",
			rendered: "// build: Release\n#define ARCH \"1\"\n",
			want: map[string]string{
				"$<CONFIG>":            "Release",
				"$<PLATFORM_ID:Linux>": "1",
			},
		},
		{
			name:     "nested genex tracked at outer literal",
			template: "x=$<IF:$<CONFIG:Release>,yes,no>;",
			rendered: "x=yes;",
			want:     map[string]string{"$<IF:$<CONFIG:Release>,yes,no>": "yes"},
		},
		{
			name:     "same literal repeats with same value",
			template: "$<CONFIG>+$<CONFIG>",
			rendered: "Release+Release",
			want:     map[string]string{"$<CONFIG>": "Release"},
		},
		{
			name:     "no genex in template",
			template: "plain text",
			rendered: "plain text",
			wantErr:  "no top-level genex",
		},
		{
			name:     "adjacent genexes (no separator)",
			template: "$<CONFIG>$<COMPILER_ID:GNU>",
			rendered: "Release1",
			wantErr:  "adjacent to the next genex",
		},
		{
			name:     "static prefix mismatch",
			template: "Hello $<CONFIG>",
			rendered: "World Release",
			wantErr:  "static chunk",
		},
		{
			name:     "post-genex anchor missing",
			template: "$<CONFIG> END",
			rendered: "Release---",
			wantErr:  "anchor",
		},
		{
			name:     "tail mismatch (only first genex anchors)",
			template: "$<CONFIG> X",
			rendered: "Release Y",
			wantErr:  "anchor",
		},
		{
			name:     "same literal, different resolved values",
			template: "a $<CONFIG> b $<CONFIG> c",
			rendered: "a Release b Debug c",
			wantErr:  "resolves to two different values",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := extractGenexValues([]byte(c.template), []byte(c.rendered))
			if c.wantErr != "" {
				if err == nil {
					t.Fatalf("expected error containing %q; got nil and values %#v", c.wantErr, got)
				}
				if !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("error %q does not contain %q", err.Error(), c.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("extractGenexValues:\n  got:  %#v\n  want: %#v", got, c.want)
			}
		})
	}
}

// TestApplyGenexValues_RoundTrip exercises the extract+apply
// pair: rendering a template via the recovered values dict
// reproduces the original cmake-rendered bytes. This is the
// soundness invariant the (b) lift relies on at Bazel time.
func TestApplyGenexValues_RoundTrip(t *testing.T) {
	cases := []struct {
		name     string
		template string
		rendered string
	}{
		{"single", "Hello $<CONFIG:Release> world", "Hello 1 world"},
		{"two-with-separator", "cfg=$<CONFIG>;cc=$<COMPILER_ID:GNU>", "cfg=Release;cc=1"},
		{"multiline", "// build: $<CONFIG>\n#define ARCH \"$<PLATFORM_ID:Linux>\"\n", "// build: Release\n#define ARCH \"1\"\n"},
		{"nested-outer", "x=$<IF:$<CONFIG:Release>,yes,no>;", "x=yes;"},
		{"repeated-same-value", "$<CONFIG>+$<CONFIG>", "Release+Release"},
		{"genex-at-end", "leading $<CONFIG>", "leading Release"},
		{"genex-at-start", "$<CONFIG> trailing", "Release trailing"},
		{"entire-template-genex", "$<CONFIG>", "Release"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			values, err := extractGenexValues([]byte(c.template), []byte(c.rendered))
			if err != nil {
				t.Fatalf("extract: %v", err)
			}
			roundtrip, err := applyGenexValues([]byte(c.template), values)
			if err != nil {
				t.Fatalf("apply: %v", err)
			}
			if string(roundtrip) != c.rendered {
				t.Errorf("round-trip mismatch:\n  got:  %q\n  want: %q", roundtrip, c.rendered)
			}
		})
	}
}

// TestApplyGenexValues_MissingKey covers the soundness check
// for callers who construct the values dict themselves (rather
// than via extractGenexValues): if the dict lacks a key the
// template carries, the apply step must error instead of
// emitting a literal `$<...>` in the rendered output.
func TestApplyGenexValues_MissingKey(t *testing.T) {
	_, err := applyGenexValues([]byte("Hello $<CONFIG>"), map[string]string{})
	if err == nil {
		t.Fatal("expected error for missing genex value")
	}
	if !strings.Contains(err.Error(), "no value for genex") {
		t.Errorf("error %q does not mention the missing-value case", err.Error())
	}
}
