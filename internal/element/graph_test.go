package element_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/sstriker/buildstream-bazel/internal/element"
)

func TestBuildGraph_FromFixture(t *testing.T) {
	p, err := element.ReadProject(fixtureRoot, "elements")
	if err != nil {
		t.Fatal(err)
	}
	g, err := element.BuildGraph(p)
	if err != nil {
		t.Fatal(err)
	}
	// hello depends on base; uses-hello depends on base + hello.
	if got := g.Edges["base"]; len(got) != 0 {
		t.Errorf("base edges = %v, want []", got)
	}
	if got, want := g.Edges["components/hello"], []string{"base"}; !sameStringSlice(got, want) {
		t.Errorf("hello edges = %v, want %v", got, want)
	}
	if got, want := g.Edges["components/uses-hello"], []string{"base", "components/hello"}; !sameStringSlice(got, want) {
		t.Errorf("uses-hello edges = %v, want %v", got, want)
	}
}

func TestTopoSort_DepsBeforeDependents(t *testing.T) {
	p, err := element.ReadProject(fixtureRoot, "elements")
	if err != nil {
		t.Fatal(err)
	}
	g, err := element.BuildGraph(p)
	if err != nil {
		t.Fatal(err)
	}
	order, err := g.TopoSort()
	if err != nil {
		t.Fatal(err)
	}
	posOf := map[string]int{}
	for i, n := range order {
		posOf[n] = i
	}
	if posOf["base"] >= posOf["components/hello"] {
		t.Errorf("base must precede hello: order=%v", order)
	}
	if posOf["components/hello"] >= posOf["components/uses-hello"] {
		t.Errorf("hello must precede uses-hello: order=%v", order)
	}
}

func TestTopoSort_StableAcrossRuns(t *testing.T) {
	p, err := element.ReadProject(fixtureRoot, "elements")
	if err != nil {
		t.Fatal(err)
	}
	g, err := element.BuildGraph(p)
	if err != nil {
		t.Fatal(err)
	}
	first, _ := g.TopoSort()
	for i := 0; i < 5; i++ {
		nth, err := g.TopoSort()
		if err != nil {
			t.Fatal(err)
		}
		if !sameStringSlice(first, nth) {
			t.Errorf("topo order shifted: first=%v, nth=%v", first, nth)
		}
	}
}

func TestTopoSort_DetectsCycle(t *testing.T) {
	g := &element.Graph{
		Nodes: map[string]*element.Element{
			"a": {Name: "a"},
			"b": {Name: "b"},
			"c": {Name: "c"},
		},
		Edges: map[string][]string{
			"a": {"b"},
			"b": {"c"},
			"c": {"a"},
		},
	}
	_, err := g.TopoSort()
	if err == nil {
		t.Fatal("expected cycle error")
	}
	var cyc *element.CycleError
	if !errors.As(err, &cyc) {
		t.Fatalf("err = %v, want *CycleError", err)
	}
	for _, want := range []string{"a", "b", "c"} {
		var found bool
		for _, n := range cyc.Nodes {
			if n == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("cycle nodes = %v, want to include %q", cyc.Nodes, want)
		}
	}
}

func TestBuildGraph_RejectsUnknownDep(t *testing.T) {
	p := &element.Project{Elements: map[string]*element.Element{
		"a": {
			Name:    "a",
			Depends: []element.Dep{{Filename: "ghost.bst"}},
		},
	}}
	_, err := element.BuildGraph(p)
	if err == nil || !strings.Contains(err.Error(), "ghost.bst") {
		t.Errorf("err = %v, want unknown-dep failure", err)
	}
}

func TestBuildGraph_RejectsJunctionDep(t *testing.T) {
	p := &element.Project{Elements: map[string]*element.Element{
		"a": {
			Name:    "a",
			Depends: []element.Dep{{Filename: "x.bst", Junction: "ext"}},
		},
		"x": {Name: "x"},
	}}
	_, err := element.BuildGraph(p)
	if err == nil || !strings.Contains(err.Error(), "junction") {
		t.Errorf("err = %v, want junction failure", err)
	}
}

func TestFilterByKind_KeepsOnlyMatching(t *testing.T) {
	p, err := element.ReadProject(fixtureRoot, "elements")
	if err != nil {
		t.Fatal(err)
	}
	g, err := element.BuildGraph(p)
	if err != nil {
		t.Fatal(err)
	}
	all, _ := g.TopoSort()
	cmakeOnly := g.FilterByKind(all, "cmake")
	want := []string{"components/hello", "components/uses-hello"}
	if !sameStringSlice(cmakeOnly, want) {
		t.Errorf("cmake-only = %v, want %v", cmakeOnly, want)
	}
}
