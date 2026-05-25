package lower

import (
	"strings"
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/internal/ninja"
	"github.com/sstriker/buildstream-bazel/converter/ir"
)

func mustParseNinja(t *testing.T, src string) *ninja.Graph {
	t.Helper()
	g, err := ninja.Parse(strings.NewReader(src), "", nil)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return g
}

func TestLowerStandaloneCustomCommands_StandaloneEdge(t *testing.T) {
	g := mustParseNinja(t, `rule CUSTOM_COMMAND
  command = $COMMAND
rule CXX_COMPILER
  command = c++ -c $in -o $out

build version.txt: CUSTOM_COMMAND
  COMMAND = git rev-parse HEAD
build foo.o: CXX_COMPILER foo.cc
`)
	got := lowerStandaloneCustomCommands(g, nil, "/build")
	if len(got) != 1 {
		t.Fatalf("want 1 standalone; got %d (%v)", len(got), got)
	}
	if got[0].Kind != ir.KindGenrule {
		t.Errorf("Kind: %v", got[0].Kind)
	}
	if got[0].Name != "custom_command_version_txt" {
		t.Errorf("Name: %q", got[0].Name)
	}
	if got[0].GenruleCmd != "git rev-parse HEAD" {
		t.Errorf("Cmd: %q", got[0].GenruleCmd)
	}
	if len(got[0].GenruleOuts) != 1 || got[0].GenruleOuts[0] != "version.txt" {
		t.Errorf("Outs: %v", got[0].GenruleOuts)
	}
}

func TestLowerStandaloneCustomCommands_SkipsCoveredEdge(t *testing.T) {
	g := mustParseNinja(t, `rule CUSTOM_COMMAND
  command = $COMMAND
rule CXX_COMPILER
  command = c++ -c $in -o $out

build generated.h: CUSTOM_COMMAND tmpl
  COMMAND = python gen.py $in $out
build foo.o: CXX_COMPILER foo.cc
`)
	existing := []ir.Target{{
		Name:        "generated_h",
		Kind:        ir.KindGenrule,
		GenruleOuts: []string{"generated.h"},
	}}
	got := lowerStandaloneCustomCommands(g, existing, "/build")
	if len(got) != 0 {
		t.Errorf("expected dedup; got %v", got)
	}
}

func TestLowerStandaloneCustomCommands_HandlesMultipleStandalone(t *testing.T) {
	g := mustParseNinja(t, `rule CUSTOM_COMMAND
  command = $COMMAND

build alpha.stamp: CUSTOM_COMMAND
  COMMAND = touch alpha.stamp
build beta.stamp: CUSTOM_COMMAND
  COMMAND = touch beta.stamp
`)
	got := lowerStandaloneCustomCommands(g, nil, "/build")
	if len(got) != 2 {
		t.Fatalf("want 2 standalone; got %d", len(got))
	}
	// Ninja-source order preserved.
	if got[0].Name != "custom_command_alpha_stamp" {
		t.Errorf("got[0].Name = %q", got[0].Name)
	}
	if got[1].Name != "custom_command_beta_stamp" {
		t.Errorf("got[1].Name = %q", got[1].Name)
	}
}

func TestLowerStandaloneCustomCommands_HandlesNameCollision(t *testing.T) {
	// Two outputs that sanitize to the same name (different
	// extensions wouldn't normally collide; this synthetic edge
	// pair exercises the suffix path).
	g := mustParseNinja(t, `rule CUSTOM_COMMAND
  command = $COMMAND

build foo: CUSTOM_COMMAND
  COMMAND = touch foo
build foo_: CUSTOM_COMMAND
  COMMAND = touch foo_
`)
	got := lowerStandaloneCustomCommands(g, nil, "/build")
	if len(got) != 2 {
		t.Fatalf("want 2; got %d", len(got))
	}
	if got[0].Name == got[1].Name {
		t.Errorf("names should disambiguate; got both %q", got[0].Name)
	}
}

func TestLowerStandaloneCustomCommands_SkipsRuleWithoutCommand(t *testing.T) {
	g := mustParseNinja(t, `rule CUSTOM_COMMAND
  description = no-op

build phony: CUSTOM_COMMAND
`)
	got := lowerStandaloneCustomCommands(g, nil, "/build")
	if len(got) != 0 {
		t.Errorf("expected skip when rule has no command; got %v", got)
	}
}

func TestLowerStandaloneCustomCommands_PreservesImplicitOuts(t *testing.T) {
	g := mustParseNinja(t, `rule CUSTOM_COMMAND
  command = $COMMAND

build main.txt | side.txt: CUSTOM_COMMAND in
  COMMAND = gen $in
`)
	got := lowerStandaloneCustomCommands(g, nil, "/build")
	if len(got) != 1 {
		t.Fatalf("want 1; got %d", len(got))
	}
	// Sorted outs include both main and implicit side.
	if len(got[0].GenruleOuts) != 2 {
		t.Errorf("outs len: %d", len(got[0].GenruleOuts))
	}
}

func TestLowerStandaloneCustomCommands_NilGraph(t *testing.T) {
	if got := lowerStandaloneCustomCommands(nil, nil, "/build"); got != nil {
		t.Errorf("nil graph should return nil; got %v", got)
	}
}

func TestSanitizeOutputName(t *testing.T) {
	cases := []struct{ in, want string }{
		{"version.txt", "version_txt"},
		{"gen/version.h", "gen_version_h"},
		{"foo-bar.cc", "foo_bar_cc"},
		{"./relative", "relative"},
		{"a..b//c", "a_b_c"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			if got := sanitizeOutputName(tc.in); got != tc.want {
				t.Errorf("sanitizeOutputName(%q) = %q; want %q", tc.in, got, tc.want)
			}
		})
	}
}
