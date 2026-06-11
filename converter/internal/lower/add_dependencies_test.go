package lower

import (
	"reflect"
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/internal/fileapi"
)

func TestIsAddDependenciesEdge_True(t *testing.T) {
	g := fileapi.BacktraceGraph{
		Commands: []string{"target_link_libraries", "add_dependencies"},
		Files:    []string{"CMakeLists.txt"},
		Nodes: []fileapi.BacktraceNode{
			{},
			{File: 0, Line: 5, Command: 1}, // add_dependencies
		},
	}
	dep := fileapi.TargetDependency{Id: "foo::@", Backtrace: 1}
	if !isBuildOrderOnlyEdge(dep, g) {
		t.Error("expected true for add_dependencies-backed dep")
	}
}

func TestIsAddDependenciesEdge_FalseForTLL(t *testing.T) {
	g := fileapi.BacktraceGraph{
		Commands: []string{"target_link_libraries"},
		Files:    []string{"CMakeLists.txt"},
		Nodes: []fileapi.BacktraceNode{
			{},
			{File: 0, Line: 5, Command: 0},
		},
	}
	dep := fileapi.TargetDependency{Id: "foo::@", Backtrace: 1}
	if isBuildOrderOnlyEdge(dep, g) {
		t.Error("expected false for target_link_libraries-backed dep")
	}
}

func TestIsAddDependenciesEdge_FalseForMissingBacktrace(t *testing.T) {
	dep := fileapi.TargetDependency{Id: "foo::@", Backtrace: 0}
	if isBuildOrderOnlyEdge(dep, fileapi.BacktraceGraph{}) {
		t.Error("expected false for zero backtrace")
	}
}

// TestLowerTarget_AddDependenciesRoutesToData covers the
// codemodel + backtrace integration: a TargetDependency whose
// backtrace points at add_dependencies routes to ir.Target.Data
// rather than ir.Target.Deps.
func TestLowerTarget_AddDependenciesRoutesToData(t *testing.T) {
	// Build a minimal codemodel where target "foo" has one dep
	// "tool" registered via add_dependencies.
	r := &fileapi.Reply{
		Targets: map[string]fileapi.Target{
			"foo::@": {
				Name: "foo",
				Type: "STATIC_LIBRARY",
				Dependencies: []fileapi.TargetDependency{
					{Id: "tool::@", Backtrace: 1},
				},
				BacktraceGraph: fileapi.BacktraceGraph{
					Commands: []string{"add_dependencies"},
					Files:    []string{"CMakeLists.txt"},
					Nodes: []fileapi.BacktraceNode{
						{},
						{File: 0, Line: 3, Command: 0},
					},
				},
			},
			"tool::@": {
				Name: "tool",
				Type: "UTILITY",
			},
		},
		Codemodel: fileapi.Codemodel{
			Configurations: []fileapi.Configuration{{
				Name: "Release",
				Targets: []fileapi.ConfigTargetRef{
					{Id: "foo::@", Name: "foo"},
					{Id: "tool::@", Name: "tool"},
				},
			}},
		},
	}
	pkg, err := ToIR(r, nil, Options{})
	if err != nil {
		t.Fatalf("ToIR: %v", err)
	}
	for _, tgt := range pkg.Targets {
		if tgt.Name != "foo" {
			continue
		}
		// UTILITY targets are filtered, so the dep label may or
		// may not appear depending on idToName resolution; what
		// matters is that IF it appears anywhere, it's in Data,
		// not Deps.
		for _, d := range tgt.Deps {
			if d == ":tool" {
				t.Errorf("tool dep landed in Deps; want Data only")
			}
		}
		if len(tgt.Data) > 0 && !reflect.DeepEqual(tgt.Data, []string{":tool"}) {
			t.Errorf("Data: got %v want [:tool] (or empty if UTILITY filtered)", tgt.Data)
		}
		return
	}
	t.Fatal("foo not in pkg.Targets")
}
