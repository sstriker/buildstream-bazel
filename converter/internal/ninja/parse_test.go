package ninja_test

import (
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/internal/ninja"
)

func mustParse(t *testing.T, src string) *ninja.Graph {
	t.Helper()
	g, err := ninja.Parse(strings.NewReader(src), "", nil)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return g
}

func TestParse_TopLevelVars(t *testing.T) {
	g := mustParse(t, `# header
ninja_required_version = 1.5

CONFIGURATION = Release
cmake_ninja_workdir = /tmp/foo/
`)
	if got, want := g.Vars["ninja_required_version"], "1.5"; got != want {
		t.Errorf("ninja_required_version = %q, want %q", got, want)
	}
	if got, want := g.Vars["CONFIGURATION"], "Release"; got != want {
		t.Errorf("CONFIGURATION = %q, want %q", got, want)
	}
	if got, want := g.VarOrder, []string{"ninja_required_version", "CONFIGURATION", "cmake_ninja_workdir"}; !sameSlice(got, want) {
		t.Errorf("VarOrder = %v, want %v", got, want)
	}
}

func TestParse_RuleWithMultipleBindings(t *testing.T) {
	g := mustParse(t, `rule C_COMPILER
  depfile = $DEP_FILE
  deps = gcc
  command = ${LAUNCHER}/usr/bin/cc $DEFINES $INCLUDES $FLAGS -o $out -c $in
  description = Building C object $out
`)
	r, ok := g.Rules["C_COMPILER"]
	if !ok {
		t.Fatal("rule C_COMPILER missing")
	}
	if r.Bindings["deps"] != "gcc" {
		t.Errorf("deps = %q", r.Bindings["deps"])
	}
	if r.Bindings["command"] != "${LAUNCHER}/usr/bin/cc $DEFINES $INCLUDES $FLAGS -o $out -c $in" {
		t.Errorf("command = %q", r.Bindings["command"])
	}
	if got, want := r.BindingOrder, []string{"depfile", "deps", "command", "description"}; !sameSlice(got, want) {
		t.Errorf("BindingOrder = %v, want %v", got, want)
	}
}

func TestParse_BuildStmt_SimpleAndOrderOnly(t *testing.T) {
	g := mustParse(t, `rule R
  command = run $in $out

build out.o: R in.c || phony.dep
  FLAGS = -O2
`)
	if len(g.Builds) != 1 {
		t.Fatalf("Builds = %d, want 1", len(g.Builds))
	}
	b := g.Builds[0]
	if got, want := b.Outputs, []string{"out.o"}; !sameSlice(got, want) {
		t.Errorf("Outputs = %v, want %v", got, want)
	}
	if got, want := b.Inputs, []string{"in.c"}; !sameSlice(got, want) {
		t.Errorf("Inputs = %v, want %v", got, want)
	}
	if got, want := b.OrderOnly, []string{"phony.dep"}; !sameSlice(got, want) {
		t.Errorf("OrderOnly = %v, want %v", got, want)
	}
	if b.Bindings["FLAGS"] != "-O2" {
		t.Errorf("FLAGS = %q", b.Bindings["FLAGS"])
	}
}

func TestParse_BuildStmt_ImplicitInputsAndOuts(t *testing.T) {
	g := mustParse(t, `rule R
  command = c

build a b | implicit_out: R x y | implicit_in || order_only
`)
	b := g.Builds[0]
	if got, want := b.Outputs, []string{"a", "b"}; !sameSlice(got, want) {
		t.Errorf("Outputs = %v", got)
	}
	if got, want := b.ImplicitOuts, []string{"implicit_out"}; !sameSlice(got, want) {
		t.Errorf("ImplicitOuts = %v, want %v", got, want)
	}
	if got, want := b.Inputs, []string{"x", "y"}; !sameSlice(got, want) {
		t.Errorf("Inputs = %v", got)
	}
	if got, want := b.ImplicitInputs, []string{"implicit_in"}; !sameSlice(got, want) {
		t.Errorf("ImplicitInputs = %v, want %v", got, want)
	}
	if got, want := b.OrderOnly, []string{"order_only"}; !sameSlice(got, want) {
		t.Errorf("OrderOnly = %v, want %v", got, want)
	}
}

func TestParse_DollarContinuation(t *testing.T) {
	g := mustParse(t, `rule R
  command = a $
b $
c
`)
	if got, want := g.Rules["R"].Bindings["command"], "a b c"; got != want {
		t.Errorf("command = %q, want %q", got, want)
	}
}

func TestParse_Default(t *testing.T) {
	g := mustParse(t, `rule R
  command = c
build a: R
build b: R
default a b
`)
	if got, want := g.Defaults, []string{"a", "b"}; !sameSlice(got, want) {
		t.Errorf("Defaults = %v, want %v", got, want)
	}
}

func TestParse_Include(t *testing.T) {
	rulesNinja := `rule INCLUDED
  command = included $in $out
`
	resolver := func(parentDir, path string) (io.ReadCloser, error) {
		if path == "rules.ninja" {
			return io.NopCloser(strings.NewReader(rulesNinja)), nil
		}
		return nil, errors.New("unknown include")
	}
	g, err := ninja.Parse(strings.NewReader(`
include rules.ninja
build out: INCLUDED in
`), "", &ninja.Parser{FileResolver: resolver})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := g.Rules["INCLUDED"]; !ok {
		t.Errorf("rule from include not present")
	}
	if g.Builds[0].Rule != "INCLUDED" {
		t.Errorf("build references %q, want INCLUDED", g.Builds[0].Rule)
	}
}

func TestParse_BuildIndex(t *testing.T) {
	g := mustParse(t, `rule R
  command = c
build out1 out2 | hidden: R in
`)
	if g.BuildFor("out1") == nil {
		t.Errorf("BuildFor(out1) = nil")
	}
	if g.BuildFor("out2") == nil {
		t.Errorf("BuildFor(out2) = nil")
	}
	if g.BuildFor("hidden") == nil {
		t.Errorf("BuildFor(hidden) = nil — implicit outs should index too")
	}
	if g.BuildFor("nope") != nil {
		t.Errorf("BuildFor(nope) returned non-nil")
	}
}

func TestExpand_DollarVarFormsAndEscapes(t *testing.T) {
	scope := ninja.Scope{{
		"NAME":  "alice",
		"empty": "",
		"path":  "/usr/bin",
	}}
	cases := []struct {
		in, want string
	}{
		{"hello $NAME", "hello alice"},
		{"hello ${NAME}", "hello alice"},
		{"$$literal", "$literal"},
		{"$NAME and $NAME", "alice and alice"},
		{"$path/cc", "/usr/bin/cc"},
		{"empty=[$empty]", "empty=[]"},
		{"$undefined-here", "-here"}, // identifier stops at hyphen-after-letter
		{"${dot.var}", ""},           // braced refs accept dots; lookup misses
		{"end $", "end $"},           // trailing $
	}
	for _, c := range cases {
		if got := ninja.Expand(c.in, scope); got != c.want {
			t.Errorf("Expand(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestExpand_RecursiveBindings(t *testing.T) {
	scope := ninja.Scope{{
		"a": "[$b]",
		"b": "<$c>",
		"c": "leaf",
	}}
	if got, want := ninja.Expand("$a", scope), "[<leaf>]"; got != want {
		t.Errorf("Expand recursion = %q, want %q", got, want)
	}
}

func TestExpand_CycleDoesntInfinite(t *testing.T) {
	scope := ninja.Scope{{
		"a": "x$b",
		"b": "y$a",
	}}
	got := ninja.Expand("$a", scope)
	// Either side of the cycle is broken: $a or $b stays literal.
	if !strings.Contains(got, "$") {
		t.Errorf("expected literal $ to survive cycle break, got %q", got)
	}
}

func TestCommandFor_BuildOverridesRule(t *testing.T) {
	g := mustParse(t, `rule R
  FLAGS = -O0
  command = cc $FLAGS -o $out -c $in

build x.o: R x.c
  FLAGS = -O2
`)
	cmd, ok := ninja.CommandFor(g, g.Builds[0])
	if !ok {
		t.Fatal("CommandFor returned !ok")
	}
	if want := "cc -O2 -o x.o -c x.c"; cmd != want {
		t.Errorf("command = %q, want %q", cmd, want)
	}
}

func TestCommandFor_FallsBackToRuleAndTopLevel(t *testing.T) {
	g := mustParse(t, `LAUNCHER = ccache
rule R
  command = $LAUNCHER cc $FLAGS -o $out -c $in

build x.o: R x.c
  FLAGS = -O2
`)
	cmd, _ := ninja.CommandFor(g, g.Builds[0])
	if want := "ccache cc -O2 -o x.o -c x.c"; cmd != want {
		t.Errorf("command = %q, want %q", cmd, want)
	}
}

func TestCustomCommandEdges(t *testing.T) {
	g := mustParse(t, `rule CXX_COMPILER
  command = c++ -c $in -o $out
rule CUSTOM_COMMAND
  command = $COMMAND

build foo.o: CXX_COMPILER foo.cc
build generated.h: CUSTOM_COMMAND foo.in
  COMMAND = python gen.py foo.in generated.h
build other.o: CXX_COMPILER other.cc
build version.txt: CUSTOM_COMMAND
  COMMAND = git rev-parse HEAD
`)
	edges := ninja.CustomCommandEdges(g)
	if len(edges) != 2 {
		t.Fatalf("want 2 CUSTOM_COMMAND edges; got %d", len(edges))
	}
	if edges[0].Outputs[0] != "generated.h" || edges[1].Outputs[0] != "version.txt" {
		t.Errorf("edge order: %v / %v", edges[0].Outputs, edges[1].Outputs)
	}
	cmd, ok := ninja.CommandFor(g, edges[0])
	if !ok || cmd != "python gen.py foo.in generated.h" {
		t.Errorf("CommandFor edge[0]: %q ok=%v", cmd, ok)
	}
}

func TestCustomCommandEdges_None(t *testing.T) {
	g := mustParse(t, `rule CXX_COMPILER
  command = c++ -c $in -o $out

build foo.o: CXX_COMPILER foo.cc
`)
	if got := ninja.CustomCommandEdges(g); got != nil {
		t.Errorf("expected nil, got %+v", got)
	}
}

func TestParseFile_HelloWorld(t *testing.T) {
	// The recorded build.ninja `include CMakeFiles/rules.ninja`. That file
	// isn't checked in; rules are regenerated per build dir. Use a custom
	// resolver that skips includes — we test rule discovery elsewhere.
	f, err := os.Open("../../testdata/fileapi/hello-world/build.ninja")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	g, err := ninja.Parse(f, "", &ninja.Parser{
		FileResolver: func(parentDir, path string) (io.ReadCloser, error) {
			return nil, nil // skip
		},
	})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	// hello.c.o must be claimed by the C_COMPILER build statement.
	b := g.BuildFor("CMakeFiles/hello.dir/hello.c.o")
	if b == nil {
		t.Fatal("hello.c.o not in build index")
	}
	if !strings.Contains(b.Rule, "C_COMPILER") {
		t.Errorf("rule = %q, want C_COMPILER...", b.Rule)
	}
	if want := "CMakeFiles/hello.dir/hello.c.o.d"; b.Bindings["DEP_FILE"] != want {
		t.Errorf("DEP_FILE = %q, want %q", b.Bindings["DEP_FILE"], want)
	}
	if !strings.Contains(b.Bindings["FLAGS"], "-O3") {
		t.Errorf("FLAGS = %q, want to contain -O3", b.Bindings["FLAGS"])
	}
	// libhello.a must be claimed by the linker rule.
	la := g.BuildFor("libhello.a")
	if la == nil {
		t.Fatal("libhello.a not in build index")
	}
	if !strings.Contains(la.Rule, "STATIC_LIBRARY_LINKER") {
		t.Errorf("rule = %q, want STATIC_LIBRARY_LINKER...", la.Rule)
	}
}

func TestReconfigureInputs_HelloWorld(t *testing.T) {
	f, err := os.Open("../../testdata/fileapi/hello-world/build.ninja")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	g, err := ninja.Parse(f, "", &ninja.Parser{
		FileResolver: func(parentDir, path string) (io.ReadCloser, error) {
			return nil, nil // skip rules.ninja include
		},
	})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	inputs := g.ReconfigureInputs()
	if len(inputs) == 0 {
		t.Fatal("ReconfigureInputs returned empty list; expected RERUN_CMAKE deps")
	}
	// The fixture's user CMakeLists.txt is the load-bearing entry.
	const wantUserCMakeLists = "/home/user/cmake-to-bazel/converter/testdata/sample-projects/hello-world/CMakeLists.txt"
	found := false
	for _, p := range inputs {
		if p == wantUserCMakeLists {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("ReconfigureInputs missing user CMakeLists.txt; got %d inputs, first 3: %v", len(inputs), firstN(inputs, 3))
	}
	// Build-tree artifacts (relative paths) should be present too —
	// the projection pass filters them out, but the raw oracle keeps
	// them so callers can audit if needed.
	if !containsString(inputs, "CMakeCache.txt") {
		t.Errorf("ReconfigureInputs missing CMakeCache.txt; got first 3: %v", firstN(inputs, 3))
	}
}

func TestReconfigureInputs_NoRerunCmake(t *testing.T) {
	// A hand-written ninja file with no RERUN_CMAKE statement —
	// ReconfigureInputs must return nil, not panic.
	g := mustParse(t, `rule cc
  command = cc -c $in -o $out

build x.o: cc x.c
`)
	if got := g.ReconfigureInputs(); got != nil {
		t.Errorf("ReconfigureInputs = %v, want nil", got)
	}
}

func containsString(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

func firstN(s []string, n int) []string {
	if len(s) < n {
		return s
	}
	return s[:n]
}

func sameSlice(a, b []string) bool {
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
