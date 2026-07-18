package lower

import (
	"bytes"
	"reflect"
	"strings"
	"testing"

	"github.com/sstriker/buildstream-bazel/internal/shadow"
)

// TestLowerTargetEventCommands: a PRE_LINK/POST_BUILD command with BYPRODUCTS
// becomes a genrule producing those byproducts, registered in OutToGenrule so a
// consumer resolves; a command with no byproducts is warned and dropped.
func TestLowerTargetEventCommands(t *testing.T) {
	const buildDir = "/tmp/build"
	cc := newCodegenContext()
	var warn bytes.Buffer
	calls := []shadow.TargetEventCommandCall{
		{
			Target:     "foo",
			Event:      "PRE_LINK",
			Commands:   [][]string{{"/cmake", "-E", "touch", "/tmp/build/foo_stamp.h"}},
			ByProducts: []string{"/tmp/build/foo_stamp.h"},
		},
		{
			Target:   "bar",
			Event:    "POST_BUILD",
			Commands: [][]string{{"/cmake", "-E", "echo", "done"}}, // no byproducts
		},
	}
	lowerTargetEventCommands(calls, cc, "/src", buildDir, "/src", "", &warn)

	// PRE_LINK byproduct → genrule + OutToGenrule registration.
	if cc.OutToGenrule["foo_stamp.h"] != "foo_pre_link" {
		t.Errorf("foo_stamp.h not registered to foo_pre_link: %v", cc.OutToGenrule)
	}
	found := false
	for i := range cc.Genrules {
		if cc.Genrules[i].Name == "foo_pre_link" {
			found = true
			if len(cc.Genrules[i].GenruleOuts) != 1 || cc.Genrules[i].GenruleOuts[0] != "foo_stamp.h" {
				t.Errorf("foo_pre_link outs = %v, want [foo_stamp.h]", cc.Genrules[i].GenruleOuts)
			}
			if !stringSliceContains(cc.Genrules[i].Tags, "cmake-codegen-target-event-command") {
				t.Errorf("missing audit tag: %v", cc.Genrules[i].Tags)
			}
			// output anchored to $(RULEDIR)
			if !strings.Contains(cc.Genrules[i].GenruleCmd, "$(RULEDIR)/foo_stamp.h") {
				t.Errorf("cmd not output-anchored: %q", cc.Genrules[i].GenruleCmd)
			}
		}
	}
	if !found {
		t.Fatal("foo_pre_link genrule not synthesized")
	}
	// No-byproduct command → no genrule, and a warning.
	for i := range cc.Genrules {
		if cc.Genrules[i].Name == "bar_post_build" {
			t.Error("bar POST_BUILD has no byproducts; should not synthesize a genrule")
		}
	}
	if !strings.Contains(warn.String(), "bar") || !strings.Contains(warn.String(), "no recoverable BYPRODUCTS") {
		t.Errorf("expected no-byproducts warning for bar; got %q", warn.String())
	}
}

// TestLowerTargetEventCommands_UnresolvedGenexSkipped: a POST_BUILD command that
// references an unresolved $<TARGET_FILE:…> genex (cmake --trace-expand does NOT
// expand genexes) must NOT emit a genrule with the literal genex (a broken rule)
// — it's skipped + warned, byproduct left for the breadcrumb to surface.
func TestLowerTargetEventCommands_UnresolvedGenexSkipped(t *testing.T) {
	const buildDir = "/tmp/build"
	cc := newCodegenContext()
	var warn bytes.Buffer
	calls := []shadow.TargetEventCommandCall{{
		Target:     "producer",
		Event:      "POST_BUILD",
		Commands:   [][]string{{"cp", "$<TARGET_FILE:producer>", "/tmp/build/producer_copy.a"}},
		ByProducts: []string{"/tmp/build/producer_copy.a"},
	}}
	lowerTargetEventCommands(calls, cc, "/src", buildDir, "/src", "", &warn)

	for i := range cc.Genrules {
		if cc.Genrules[i].Name == "producer_post_build" {
			t.Errorf("emitted a genrule for an unresolved-genex command (would be broken): %+v", cc.Genrules[i])
		}
	}
	if cc.OutToGenrule["producer_copy.a"] != "" {
		t.Errorf("byproduct must not be registered when the command is skipped: %v", cc.OutToGenrule)
	}
	if !strings.Contains(warn.String(), "unresolved generator expression") {
		t.Errorf("expected unresolved-genex skip warning; got %q", warn.String())
	}
}

// TestLowerTargetEventCommands_InferredOutputs: a command that declares no
// BYPRODUCTS but writes a build-dir file via a compiler `-o <path>` or a `> <file>`
// redirect has that output inferred from the command line (best-effort) rather
// than being dropped as a pure side-effect.
func TestLowerTargetEventCommands_InferredOutputs(t *testing.T) {
	const buildDir = "/tmp/build"
	cc := newCodegenContext()
	var warn bytes.Buffer
	calls := []shadow.TargetEventCommandCall{
		{
			Target:   "comp",
			Event:    "PRE_LINK",
			Commands: [][]string{{"/usr/bin/cc", "-c", "/src/extra.c", "-o", "/tmp/build/extra.o"}},
		},
		{
			Target:   "redir",
			Event:    "POST_BUILD",
			Commands: [][]string{{"/bin/gen", "--emit", ">", "/tmp/build/manifest.txt"}},
		},
	}
	lowerTargetEventCommands(calls, cc, "/src", buildDir, "/src", "", &warn)

	// -o operand inferred as an output, wired through OutToGenrule.
	if cc.OutToGenrule["extra.o"] != "comp_pre_link" {
		t.Errorf("extra.o not inferred/registered to comp_pre_link: %v", cc.OutToGenrule)
	}
	// > redirect target inferred as an output.
	if cc.OutToGenrule["manifest.txt"] != "redir_post_build" {
		t.Errorf("manifest.txt not inferred/registered to redir_post_build: %v", cc.OutToGenrule)
	}
	// The emitted genrule preserves the redirect with the operand anchored to
	// $(RULEDIR) — genrule cmd runs under bash, so the redirect produces the out.
	for i := range cc.Genrules {
		if cc.Genrules[i].Name == "redir_post_build" &&
			!strings.Contains(cc.Genrules[i].GenruleCmd, "> $(RULEDIR)/manifest.txt") {
			t.Errorf("redirect not anchored in genrule cmd: %q", cc.Genrules[i].GenruleCmd)
		}
	}
	// Inferred genrules carry the audit tag distinguishing them from declared ones.
	for i := range cc.Genrules {
		if cc.Genrules[i].Name == "comp_pre_link" &&
			!stringSliceContains(cc.Genrules[i].Tags, "cmake-codegen-target-event-inferred-output") {
			t.Errorf("inferred genrule missing audit tag: %v", cc.Genrules[i].Tags)
		}
	}
	if !strings.Contains(warn.String(), "inferred output") {
		t.Errorf("expected inferred-output breadcrumb; got %q", warn.String())
	}
}

// targetEventGenruleCmd returns the GenruleCmd of the named synthesized genrule,
// or "" if absent.
func targetEventGenruleCmd(cc *codegenContext, name string) string {
	for i := range cc.Genrules {
		if cc.Genrules[i].Name == name {
			return cc.Genrules[i].GenruleCmd
		}
	}
	return ""
}

// TestLowerTargetEventCommands_ShellQuotesMetacharArgs: an argv token carrying
// shell metacharacters (spaces, parens, `$`) is emitted quoted so the shell
// treats it as one literal argument, not word-split or a parse error — while a
// BARE `>` redirect operator stays unquoted so the genrule shell still runs it.
func TestLowerTargetEventCommands_ShellQuotesMetacharArgs(t *testing.T) {
	const buildDir = "/tmp/build"
	cc := newCodegenContext()
	var warn bytes.Buffer
	calls := []shadow.TargetEventCommandCall{{
		Target:     "gen",
		Event:      "POST_BUILD",
		Commands:   [][]string{{"/bin/gen", "--banner", "hello (world) $USER", ">", "/tmp/build/out.h"}},
		ByProducts: []string{"/tmp/build/out.h"},
	}}
	lowerTargetEventCommands(calls, cc, "/src", buildDir, "/src", "", &warn)

	cmd := targetEventGenruleCmd(cc, "gen_post_build")
	if cmd == "" {
		t.Fatalf("gen_post_build genrule not synthesized; warnings=%q", warn.String())
	}
	// The metacharacter argument is a single quoted word.
	if !strings.Contains(cmd, "'hello (world) $USER'") {
		t.Errorf("metacharacter arg not shell-quoted as one word: %q", cmd)
	}
	// The bare redirect operator is preserved (not quoted to '>'), with the
	// operand anchored to $(RULEDIR).
	if !strings.Contains(cmd, "> $(RULEDIR)/out.h") {
		t.Errorf("bare redirect operator must stay unquoted and anchored: %q", cmd)
	}
	if strings.Contains(cmd, "'>'") {
		t.Errorf("redirect operator must not be quoted: %q", cmd)
	}
}

// TestLowerTargetEventCommands_QuotedArgNotFragmentAnchored: an output name that
// also appears INSIDE a quoted argument (a message mentioning the byproduct) must
// not be split out and rewritten to $(RULEDIR) — the quote-aware tokenizer keeps
// the quoted arg atomic, so only the real output operand is anchored.
func TestLowerTargetEventCommands_QuotedArgNotFragmentAnchored(t *testing.T) {
	const buildDir = "/tmp/build"
	cc := newCodegenContext()
	var warn bytes.Buffer
	calls := []shadow.TargetEventCommandCall{{
		Target:     "gen",
		Event:      "POST_BUILD",
		Commands:   [][]string{{"/bin/gen", "--msg", "building out.h now", "-o", "/tmp/build/out.h"}},
		ByProducts: []string{"/tmp/build/out.h"},
	}}
	lowerTargetEventCommands(calls, cc, "/src", buildDir, "/src", "", &warn)

	cmd := targetEventGenruleCmd(cc, "gen_post_build")
	if cmd == "" {
		t.Fatalf("gen_post_build genrule not synthesized; warnings=%q", warn.String())
	}
	// The real output operand is anchored.
	if !strings.Contains(cmd, "-o $(RULEDIR)/out.h") {
		t.Errorf("real output operand not anchored: %q", cmd)
	}
	// The quoted message stays intact — its interior `out.h` must NOT be rewritten.
	if !strings.Contains(cmd, "'building out.h now'") {
		t.Errorf("quoted arg corrupted (interior fragment-anchored?): %q", cmd)
	}
	if strings.Contains(cmd, "building $(RULEDIR)/out.h now") {
		t.Errorf("interior of quoted arg was wrongly anchored: %q", cmd)
	}
}

// TestQuoteAwareTokens: quoted spans stay atomic (incl. internal whitespace and
// escaped quotes); unquoted whitespace splits.
func TestQuoteAwareTokens(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"a b c", []string{"a", "b", "c"}},
		{"a 'b c' d", []string{"a", "'b c'", "d"}},
		{"'a  b'", []string{"'a  b'"}}, // internal double space preserved
		{"gen -o $(RULEDIR)/out.h", []string{"gen", "-o", "$(RULEDIR)/out.h"}},
		{`'it'\''s'`, []string{`'it'\''s'`}}, // shellQuoteArg's escaped-quote form is one token
	}
	for _, c := range cases {
		got := quoteAwareTokens(c.in)
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("quoteAwareTokens(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestLowerTargetEventCommands_QuotesSemicolonJoinedList: the shadow classifier
// splits `;`-joined list COMMAND args into separate tokens upstream, but if one
// still reaches lowering as a single token (a value with an embedded `;`), the
// per-token quoting keeps it one shell argument instead of letting the `;` fan
// the list into separate commands — the lowering-side belt to the classifier's
// braces.
func TestLowerTargetEventCommands_QuotesSemicolonJoinedList(t *testing.T) {
	const buildDir = "/tmp/build"
	cc := newCodegenContext()
	var warn bytes.Buffer
	calls := []shadow.TargetEventCommandCall{{
		Target:     "gen",
		Event:      "PRE_LINK",
		Commands:   [][]string{{"/bin/gen", "--items", "a;b;c", "-o", "/tmp/build/out.h"}},
		ByProducts: []string{"/tmp/build/out.h"},
	}}
	lowerTargetEventCommands(calls, cc, "/src", buildDir, "/src", "", &warn)

	cmd := targetEventGenruleCmd(cc, "gen_pre_link")
	if cmd == "" {
		t.Fatalf("gen_pre_link genrule not synthesized; warnings=%q", warn.String())
	}
	// The `;`-joined list is one quoted argument — the `;` cannot separate commands.
	if !strings.Contains(cmd, "'a;b;c'") {
		t.Errorf("`;`-joined list arg must be quoted as one word: %q", cmd)
	}
	if strings.Contains(cmd, " a;b;c ") {
		t.Errorf("`;`-joined list arg leaked unquoted (shell would fan it): %q", cmd)
	}
}

// TestInferTargetEventOutputs_Variants: the redirect/-o inference accepts plain
// `>` / `1>` and a standalone `-o`, and rejects the append/stderr variants,
// `$`-bearing operands, and out-of-tree paths.
func TestInferTargetEventOutputs_Variants(t *testing.T) {
	const buildDir = "/tmp/build"
	cases := []struct {
		name string
		argv []string
		want []string
	}{
		{"dash-o", []string{"cc", "-o", "/tmp/build/a.o", "x.c"}, []string{"a.o"}},
		{"redirect", []string{"gen", ">", "/tmp/build/b.txt"}, []string{"b.txt"}},
		{"redirect-1", []string{"gen", "1>", "/tmp/build/c.txt"}, []string{"c.txt"}},
		{"append-excluded", []string{"gen", ">>", "/tmp/build/d.txt"}, nil},
		{"stderr-excluded", []string{"gen", "2>", "/tmp/build/e.txt"}, nil},
		{"genex-operand-excluded", []string{"cc", "-o", "$<TARGET_FILE:t>"}, nil},
		{"var-operand-excluded", []string{"cc", "-o", "${OUT}"}, nil},
		{"out-of-tree-excluded", []string{"cc", "-o", "/other/place/f.o"}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := inferTargetEventOutputs([][]string{tc.argv}, buildDir)
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Errorf("inferTargetEventOutputs(%v) = %v, want %v", tc.argv, got, tc.want)
			}
		})
	}
}
