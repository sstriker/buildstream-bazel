package element

import (
	"fmt"
	"sort"
	"strings"
)

// Graph is the dep graph over a Project. Nodes are element names; edges
// run from a dependent to its dependency (so topological sort yields
// dependencies first).
//
// Junctions are not yet supported — any depend with a non-empty Junction
// returns an error from BuildGraph. Same for unknown filenames.
type Graph struct {
	Nodes map[string]*Element
	Edges map[string][]string // dependent -> dependencies (sorted; deduped)
}

// BuildGraph constructs a Graph from a Project. Returns an error if any
// dependency references a non-existent element or uses an unsupported
// junction.
func BuildGraph(p *Project) (*Graph, error) {
	g := &Graph{
		Nodes: make(map[string]*Element, len(p.Elements)),
		Edges: make(map[string][]string, len(p.Elements)),
	}
	for name, el := range p.Elements {
		g.Nodes[name] = el
	}

	var problems []string
	for _, name := range p.SortedNames() {
		el := p.Elements[name]
		var deps []string
		seen := map[string]struct{}{}
		for _, d := range el.Depends {
			if d.Junction != "" {
				problems = append(problems,
					fmt.Sprintf("%s: depends on %q via junction %q (junctions not yet supported)",
						name, d.Filename, d.Junction))
				continue
			}
			depName := strings.TrimSuffix(d.Filename, ".bst")
			if _, ok := g.Nodes[depName]; !ok {
				problems = append(problems,
					fmt.Sprintf("%s: depends on unknown element %q", name, d.Filename))
				continue
			}
			if _, dup := seen[depName]; dup {
				continue
			}
			seen[depName] = struct{}{}
			deps = append(deps, depName)
		}
		sort.Strings(deps)
		g.Edges[name] = deps
	}
	if len(problems) > 0 {
		return g, fmt.Errorf("element graph: %d problem(s):\n  %s",
			len(problems), strings.Join(problems, "\n  "))
	}
	return g, nil
}

// TopoSort returns element names in dependency-first order. If the graph
// contains a cycle, returns the names involved in some cycle as a typed
// CycleError.
//
// Within each topological "level" (set of nodes whose deps are already
// satisfied) names are sorted alphabetically — this is what makes the
// determinism test work without an explicit per-element sort.
func (g *Graph) TopoSort() ([]string, error) {
	indeg := make(map[string]int, len(g.Nodes))
	rdeps := make(map[string][]string, len(g.Nodes)) // dep -> [dependents]
	for name := range g.Nodes {
		indeg[name] = 0
	}
	for dep, ds := range g.Edges {
		indeg[dep] = len(ds)
		for _, d := range ds {
			rdeps[d] = append(rdeps[d], dep)
		}
	}

	// Initial frontier: nodes with no deps. Sort alphabetically for
	// stability.
	var frontier []string
	for n, deg := range indeg {
		if deg == 0 {
			frontier = append(frontier, n)
		}
	}
	sort.Strings(frontier)

	out := make([]string, 0, len(g.Nodes))
	for len(frontier) > 0 {
		// Process the whole frontier as one level so the topological
		// order is stable across runs.
		level := frontier
		frontier = nil
		for _, n := range level {
			out = append(out, n)
			for _, dep := range rdeps[n] {
				indeg[dep]--
				if indeg[dep] == 0 {
					frontier = append(frontier, dep)
				}
			}
		}
		sort.Strings(frontier)
	}

	if len(out) != len(g.Nodes) {
		// Anything still with indeg>0 sits in a cycle.
		var stuck []string
		for n, deg := range indeg {
			if deg > 0 {
				stuck = append(stuck, n)
			}
		}
		sort.Strings(stuck)
		return nil, &CycleError{Nodes: stuck}
	}
	return out, nil
}

// CycleError is returned by TopoSort when the graph isn't a DAG.
type CycleError struct {
	Nodes []string
}

func (e *CycleError) Error() string {
	return fmt.Sprintf("element graph: cycle involves %s",
		strings.Join(e.Nodes, ", "))
}

// FilterByKind returns the subset of names whose element has one of the
// given kinds. Useful for "all kind:cmake elements in topo order":
// graph.TopoSort() then filter through this.
func (g *Graph) FilterByKind(names []string, kinds ...string) []string {
	keep := map[string]bool{}
	for _, k := range kinds {
		keep[k] = true
	}
	out := make([]string, 0, len(names))
	for _, n := range names {
		el, ok := g.Nodes[n]
		if !ok {
			continue
		}
		if keep[el.Kind] {
			out = append(out, n)
		}
	}
	return out
}
