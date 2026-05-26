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

func TestLowerStandaloneCustomCommands_FiltersCMakeBookkeepingEdges(t *testing.T) {
	// Mirrors the bookkeeping edges cmake's Ninja generator always
	// emits: edit_cache / rebuild_cache utility outputs under
	// CMakeFiles/<name>.util. Plus a real user-declared standalone
	// edge so the test can confirm the filter only skips the
	// bookkeeping shape, not anything else.
	g := mustParseNinja(t, `rule CUSTOM_COMMAND
  command = $COMMAND

build CMakeFiles/edit_cache.util: CUSTOM_COMMAND
  COMMAND = /usr/bin/cmake -E echo No interactive CMake dialog available.
build CMakeFiles/rebuild_cache.util: CUSTOM_COMMAND
  COMMAND = /usr/bin/cmake --regenerate-during-build -S/src -B/build
build version.txt: CUSTOM_COMMAND
  COMMAND = git rev-parse HEAD
`)
	got := lowerStandaloneCustomCommands(g, nil, "/build")
	if len(got) != 1 {
		t.Fatalf("want 1 standalone (bookkeeping edges filtered); got %d (%v)", len(got), got)
	}
	if got[0].Name != "custom_command_version_txt" {
		t.Errorf("got[0].Name = %q; want custom_command_version_txt (the non-bookkeeping edge)", got[0].Name)
	}
}

func TestLowerStandaloneCustomCommands_FiltersNinjaVarOutputs(t *testing.T) {
	// cmake's Ninja generator pairs every real custom-command
	// output with a `${cmake_ninja_workdir}<basename>` implicit
	// output for restat tracking. The walker must drop those
	// from the genrule's outs (Bazel can't declare an outs entry
	// whose path is a ninja-time variable) but keep the real
	// output so the genrule lands with usable outs + name.
	g := mustParseNinja(t, `rule CUSTOM_COMMAND
  command = $COMMAND

build version.txt | ${cmake_ninja_workdir}version.txt: CUSTOM_COMMAND
  COMMAND = touch version.txt
`)
	got := lowerStandaloneCustomCommands(g, nil, "/build")
	if len(got) != 1 {
		t.Fatalf("want 1 standalone; got %d (%v)", len(got), got)
	}
	if want := []string{"version.txt"}; !equalStringSlices(got[0].GenruleOuts, want) {
		t.Errorf("outs = %v, want %v (${cmake_ninja_workdir}-shadow stripped)", got[0].GenruleOuts, want)
	}
	if got[0].Name != "custom_command_version_txt" {
		t.Errorf("name = %q, want custom_command_version_txt (first non-var output)", got[0].Name)
	}
}

func equalStringSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestFilterOutVarRefs(t *testing.T) {
	cases := []struct {
		in   []string
		want []string
	}{
		{
			in:   []string{"version.txt", "${cmake_ninja_workdir}version.txt"},
			want: []string{"version.txt"},
		},
		{
			in:   []string{"${cmake_ninja_workdir}foo", "${cmake_ninja_workdir}bar"},
			want: []string{},
		},
		{
			in:   []string{"a", "b"},
			want: []string{"a", "b"},
		},
		{
			in:   nil,
			want: nil,
		},
	}
	for i, tc := range cases {
		got := filterOutVarRefs(append([]string(nil), tc.in...))
		if len(got) == 0 && len(tc.want) == 0 {
			continue
		}
		if !equalStringSlices(got, tc.want) {
			t.Errorf("case %d: filterOutVarRefs(%v) = %v, want %v", i, tc.in, got, tc.want)
		}
	}
}

func TestIsCMakeBookkeepingOutput(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"CMakeFiles/edit_cache.util", true},
		{"CMakeFiles/rebuild_cache.util", true},
		{"CMakeFiles/install.util", true},
		{"CMakeFiles/package_source.util", true},
		{"CMakeFiles/list_install_components.util", true},
		// User-declared add_custom_command outputs never land
		// under CMakeFiles/ with the .util suffix.
		{"version.txt", false},
		{"gen/version.h", false},
		{"CMakeFiles/stub.dir/src/stub.c.o", false}, // compile artefact, not bookkeeping
		{"some/CMakeFiles/foo.util", false},         // not at the build-dir root
		{"CMakeFiles/foo.txt", false},               // wrong suffix
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			if got := isCMakeBookkeepingOutput(tc.in); got != tc.want {
				t.Errorf("isCMakeBookkeepingOutput(%q) = %v; want %v", tc.in, got, tc.want)
			}
		})
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
