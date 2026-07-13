package lower

import (
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/internal/fileapi"
	"github.com/sstriker/buildstream-bazel/converter/internal/ninja"
	"github.com/sstriker/buildstream-bazel/converter/ir"
	"github.com/sstriker/buildstream-bazel/internal/manifest"
)

// exportDepsResolver builds a manifest where Pkg::a is a
// prebuilt-backed export carrying a declared closure (Export.Deps) —
// the shape Bazel transitivity can't recover (a cc_import models no
// deps), which is the missing-symbols mechanism Export.Deps exists to
// fix.
func exportDepsResolver(t *testing.T) *manifest.Resolver {
	t.Helper()
	res, err := manifest.Index(&manifest.Imports{
		Version: 1,
		Elements: []*manifest.Element{{
			Name: "pkg",
			Exports: []*manifest.Export{{
				CMakeTarget:   "Pkg::a",
				BazelLabel:    "//elements/pkg:a_import",
				Deps:          []string{"//elements/pkg:b_import", "@dep//:c"},
				LinkLibraries: []string{"a"},
				LinkPaths:     []string{"/opt/prefix/lib/liba.a"},
			}},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func exportDepsFind(t *testing.T, pkg *ir.Package, name string) *ir.Target {
	t.Helper()
	for i := range pkg.Targets {
		if pkg.Targets[i].Name == name {
			return &pkg.Targets[i]
		}
	}
	t.Fatalf("%s not lowered", name)
	return nil
}

func assertExportClosure(t *testing.T, deps []string, channel string) {
	t.Helper()
	for _, want := range []string{"//elements/pkg:a_import", "//elements/pkg:b_import", "@dep//:c"} {
		if !stringSliceContains(deps, want) {
			t.Errorf("%s: deps missing %q (Export.Deps closure not wired): %v", channel, want, deps)
		}
	}
}

// TestExportDeps_TraceLinkChannel: a STATIC consumer (no link step —
// no fragments to flatten) whose trace records
// target_link_libraries(consumer Pkg::a). Mechanism B from the
// missing-symbols diagnosis: only the directly-named export resolves,
// so its declared closure must ride along.
func TestExportDeps_TraceLinkChannel(t *testing.T) {
	r := &fileapi.Reply{
		Targets: map[string]fileapi.Target{"consumer::@": {
			Name: "consumer", Type: "STATIC_LIBRARY",
			Sources: []fileapi.TargetSource{{Path: "c.c", CompileGroupIndex: 0}},
			CompileGroups: []fileapi.CompileGroup{{
				Language: "C", SourceIndexes: []int{0},
			}},
		}},
		Codemodel: fileapi.Codemodel{
			Configurations: []fileapi.Configuration{{
				Name:    "Release",
				Targets: []fileapi.ConfigTargetRef{{Id: "consumer::@", Name: "consumer"}},
			}},
		},
	}
	traceRaw := []byte(
		`{"args":["consumer","PUBLIC","Pkg::a"],"cmd":"target_link_libraries","file":"/s/CMakeLists.txt","line":3}` + "\n",
	)
	pkg, err := ToIR(r, &ninja.Graph{}, Options{Imports: exportDepsResolver(t), TraceRaw: traceRaw, HostSourceRoot: "/s"})
	if err != nil {
		t.Fatalf("ToIR: %v", err)
	}
	assertExportClosure(t, exportDepsFind(t, pkg, "consumer").Deps, "trace-link channel")
}

// TestExportDeps_LinkPathChannel: a binary whose flattened link line
// names the export's archive (LookupLinkPath hit). The closure rides
// the addExport — which is also what makes the trace-gated
// transitive-only drop sound for prebuilt-backed labels (mechanism A).
func TestExportDeps_LinkPathChannel(t *testing.T) {
	r := &fileapi.Reply{
		Targets: map[string]fileapi.Target{"app::@": {
			Name: "app", Type: "EXECUTABLE",
			Sources: []fileapi.TargetSource{{Path: "m.c", CompileGroupIndex: 0}},
			CompileGroups: []fileapi.CompileGroup{{
				Language: "C", SourceIndexes: []int{0},
			}},
			Link: &fileapi.TargetLink{
				Language: "C",
				CommandFragments: []fileapi.CommandFragment{
					{Fragment: "/opt/prefix/lib/liba.a", Role: "libraries"},
				},
			},
		}},
		Codemodel: fileapi.Codemodel{
			Configurations: []fileapi.Configuration{{
				Name:    "Release",
				Targets: []fileapi.ConfigTargetRef{{Id: "app::@", Name: "app"}},
			}},
		},
	}
	pkg, err := ToIR(r, &ninja.Graph{}, Options{Imports: exportDepsResolver(t)})
	if err != nil {
		t.Fatalf("ToIR: %v", err)
	}
	assertExportClosure(t, exportDepsFind(t, pkg, "app").Deps, "link-path channel")
}

// TestExportDeps_UnreachableEntryEdgeRecovered pins the transitive-drop
// gate's soundness fix: when the trace records app links Pkg::a directly,
// a flattened archive the app does NOT name directly is dropped only when
// it re-enters through Pkg::a's Export.Deps closure. Pkg::b (in a's
// closure) drops with a breadcrumb; Pkg::z (NOT in any named export's
// closure — a DIRECT-link prebuilt entry point) is WIRED instead, or the
// binary would fail to link with undefined symbols.
func TestExportDeps_UnreachableEntryEdgeRecovered(t *testing.T) {
	res, err := manifest.Index(&manifest.Imports{
		Version: 1,
		Elements: []*manifest.Element{{
			Name: "pkg",
			Exports: []*manifest.Export{
				{
					CMakeTarget: "Pkg::a", BazelLabel: "//elements/pkg:a_import",
					Deps:      []string{"//elements/pkg:b_import"},
					LinkPaths: []string{"/opt/prefix/lib/liba.a"},
				},
				{
					CMakeTarget: "Pkg::b", BazelLabel: "//elements/pkg:b_import",
					LinkPaths: []string{"/opt/prefix/lib/libb.a"},
				},
				{
					CMakeTarget: "Pkg::z", BazelLabel: "//elements/pkg:z_import",
					LinkPaths: []string{"/opt/prefix/lib/libz.a"},
				},
			},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	r := &fileapi.Reply{
		Targets: map[string]fileapi.Target{"app::@": {
			Name: "app", Type: "EXECUTABLE",
			Sources:       []fileapi.TargetSource{{Path: "m.c", CompileGroupIndex: 0}},
			CompileGroups: []fileapi.CompileGroup{{Language: "C", SourceIndexes: []int{0}}},
			Link: &fileapi.TargetLink{
				Language: "C",
				CommandFragments: []fileapi.CommandFragment{
					{Fragment: "/opt/prefix/lib/liba.a", Role: "libraries"}, // directly traced
					{Fragment: "/opt/prefix/lib/libb.a", Role: "libraries"}, // transitive via a → drop
					{Fragment: "/opt/prefix/lib/libz.a", Role: "libraries"}, // direct entry → recover
				},
			},
		}},
		Codemodel: fileapi.Codemodel{
			Configurations: []fileapi.Configuration{{
				Name:    "Release",
				Targets: []fileapi.ConfigTargetRef{{Id: "app::@", Name: "app"}},
			}},
		},
	}
	traceRaw := []byte(
		`{"args":["app","PUBLIC","Pkg::a"],"cmd":"target_link_libraries","file":"/s/CMakeLists.txt","line":3}` + "\n",
	)
	pkg, err := ToIR(r, &ninja.Graph{}, Options{Imports: res, TraceRaw: traceRaw, HostSourceRoot: "/s"})
	if err != nil {
		t.Fatalf("ToIR: %v", err)
	}
	app := exportDepsFind(t, pkg, "app")
	// a wired directly; b rides a's closure; z RECOVERED as an entry edge.
	for _, want := range []string{"//elements/pkg:a_import", "//elements/pkg:b_import", "//elements/pkg:z_import"} {
		if !stringSliceContains(app.Deps, want) {
			t.Errorf("app.Deps missing %q: %v", want, app.Deps)
		}
	}
	// The reachable transitive archive still drops — with a breadcrumb.
	if !stringSliceContains(app.Tags, "cmake-transitive-link-drop=Pkg::b") {
		t.Errorf("reachable Pkg::b must drop with a breadcrumb; tags=%v", app.Tags)
	}
	// The unreachable entry point was attributed, NOT dropped.
	if stringSliceContains(app.Tags, "cmake-transitive-link-drop=Pkg::z") {
		t.Errorf("unreachable Pkg::z must be RECOVERED, not dropped; tags=%v", app.Tags)
	}
}

// TestExportDeps_DependencyChannel: the codemodel-dependency channel
// (an out-of-tree dep id resolved via LookupCMakeTarget) carries the
// closure through the same seenDep dedup and scope routing as the
// export's own label.
func TestExportDeps_DependencyChannel(t *testing.T) {
	r := &fileapi.Reply{
		Targets: map[string]fileapi.Target{"consumer::@": {
			Name: "consumer", Type: "STATIC_LIBRARY",
			Sources: []fileapi.TargetSource{{Path: "c.c", CompileGroupIndex: 0}},
			CompileGroups: []fileapi.CompileGroup{{
				Language: "C", SourceIndexes: []int{0},
			}},
			Dependencies: []fileapi.TargetDependency{{Id: "Pkg::a::@hash"}},
		}},
		Codemodel: fileapi.Codemodel{
			Configurations: []fileapi.Configuration{{
				Name:    "Release",
				Targets: []fileapi.ConfigTargetRef{{Id: "consumer::@", Name: "consumer"}},
			}},
		},
	}
	pkg, err := ToIR(r, &ninja.Graph{}, Options{Imports: exportDepsResolver(t)})
	if err != nil {
		t.Fatalf("ToIR: %v", err)
	}
	assertExportClosure(t, exportDepsFind(t, pkg, "consumer").Deps, "dependency channel")
}

// TestExportDeps_WrapperSeedKeepsTransitiveDrop guards the wrapper-rewritten
// model: the directly-traced seed is a cc_library WRAPPER whose Deps were
// cleared but whose transitive closure was PRESERVED in LinkClosure. A
// non-directly-named flattened archive that is in that LinkClosure re-enters
// through the wrapper's Bazel deps and must still DROP — attributing every
// internal archive would over-specify the graph.
func TestExportDeps_WrapperSeedKeepsTransitiveDrop(t *testing.T) {
	res, err := manifest.Index(&manifest.Imports{
		Version: 1,
		Elements: []*manifest.Element{{
			Name: "pkg",
			Exports: []*manifest.Export{
				// Wrapper seed: Deps cleared, transitive closure preserved.
				{CMakeTarget: "Pkg::w", BazelLabel: "//elements/pkg:w", LinkClosure: []string{"//elements/pkg:internal"}, LinkPaths: []string{"/opt/prefix/lib/libw.a"}},
				// Internal archive on the flattened line, in w's closure.
				{CMakeTarget: "Pkg::internal", BazelLabel: "//elements/pkg:internal", LinkPaths: []string{"/opt/prefix/lib/libinternal.a"}},
			},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	r := &fileapi.Reply{
		Targets: map[string]fileapi.Target{"app::@": {
			Name: "app", Type: "EXECUTABLE",
			Sources:       []fileapi.TargetSource{{Path: "m.c", CompileGroupIndex: 0}},
			CompileGroups: []fileapi.CompileGroup{{Language: "C", SourceIndexes: []int{0}}},
			Link: &fileapi.TargetLink{
				Language: "C",
				CommandFragments: []fileapi.CommandFragment{
					{Fragment: "/opt/prefix/lib/libw.a", Role: "libraries"},        // directly traced → wired
					{Fragment: "/opt/prefix/lib/libinternal.a", Role: "libraries"}, // in w's LinkClosure → drop
				},
			},
		}},
		Codemodel: fileapi.Codemodel{
			Configurations: []fileapi.Configuration{{
				Name:    "Release",
				Targets: []fileapi.ConfigTargetRef{{Id: "app::@", Name: "app"}},
			}},
		},
	}
	traceRaw := []byte(
		`{"args":["app","PUBLIC","Pkg::w"],"cmd":"target_link_libraries","file":"/s/CMakeLists.txt","line":3}` + "\n",
	)
	pkg, err := ToIR(r, &ninja.Graph{}, Options{Imports: res, TraceRaw: traceRaw, HostSourceRoot: "/s"})
	if err != nil {
		t.Fatalf("ToIR: %v", err)
	}
	app := exportDepsFind(t, pkg, "app")
	if !stringSliceContains(app.Deps, "//elements/pkg:w") {
		t.Errorf("directly-traced wrapper seed must be wired: %v", app.Deps)
	}
	// The internal archive re-enters via the wrapper's Bazel deps (its label
	// is in w's LinkClosure), so it must NOT be attributed — only breadcrumbed.
	if stringSliceContains(app.Deps, "//elements/pkg:internal") {
		t.Errorf("archive in the wrapper's LinkClosure must DROP, not attribute: %v", app.Deps)
	}
	if !stringSliceContains(app.Tags, "cmake-transitive-link-drop=Pkg::internal") {
		t.Errorf("dropped internal archive must leave a breadcrumb: %v", app.Tags)
	}
}

// TestExportDeps_WrapperRewrittenEntryEdgeRecovered is the core fix: in a
// fully wrapper-rewritten manifest (every Export.Deps CLEARED, closure moved
// to LinkClosure), a directly-linked prebuilt ENTRY POINT that no named
// export's closure pulls in is RECOVERED (attributed) — while a transitive
// internal in a named wrapper's LinkClosure still drops. Before LinkClosure
// preserved reachability, the empty-Deps manifest made the gate short-circuit
// and drop BOTH, failing the entry point's link with undefined symbols.
func TestExportDeps_WrapperRewrittenEntryEdgeRecovered(t *testing.T) {
	res, err := manifest.Index(&manifest.Imports{
		Version: 1,
		Elements: []*manifest.Element{{
			Name: "pkg",
			Exports: []*manifest.Export{
				// Traced wrapper seed: Deps cleared, closure in LinkClosure.
				{CMakeTarget: "Pkg::w", BazelLabel: "//elements/pkg:w", LinkClosure: []string{"//elements/pkg:winternal"}, LinkPaths: []string{"/opt/prefix/lib/libw.a"}},
				{CMakeTarget: "Pkg::winternal", BazelLabel: "//elements/pkg:winternal", LinkPaths: []string{"/opt/prefix/lib/libwinternal.a"}},
				// A separate directly-linked prebuilt entry: no closure names it.
				{CMakeTarget: "Pkg::entry", BazelLabel: "//elements/pkg:entry", LinkPaths: []string{"/opt/prefix/lib/libentry.a"}},
			},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	r := &fileapi.Reply{
		Targets: map[string]fileapi.Target{"app::@": {
			Name: "app", Type: "EXECUTABLE",
			Sources:       []fileapi.TargetSource{{Path: "m.c", CompileGroupIndex: 0}},
			CompileGroups: []fileapi.CompileGroup{{Language: "C", SourceIndexes: []int{0}}},
			Link: &fileapi.TargetLink{
				Language: "C",
				CommandFragments: []fileapi.CommandFragment{
					{Fragment: "/opt/prefix/lib/libw.a", Role: "libraries"},         // traced → wired
					{Fragment: "/opt/prefix/lib/libwinternal.a", Role: "libraries"}, // in w's closure → drop
					{Fragment: "/opt/prefix/lib/libentry.a", Role: "libraries"},     // entry point → recover
				},
			},
		}},
		Codemodel: fileapi.Codemodel{
			Configurations: []fileapi.Configuration{{
				Name:    "Release",
				Targets: []fileapi.ConfigTargetRef{{Id: "app::@", Name: "app"}},
			}},
		},
	}
	traceRaw := []byte(
		`{"args":["app","PUBLIC","Pkg::w"],"cmd":"target_link_libraries","file":"/s/CMakeLists.txt","line":3}` + "\n",
	)
	pkg, err := ToIR(r, &ninja.Graph{}, Options{Imports: res, TraceRaw: traceRaw, HostSourceRoot: "/s"})
	if err != nil {
		t.Fatalf("ToIR: %v", err)
	}
	app := exportDepsFind(t, pkg, "app")
	// The true entry point is RECOVERED (wired), not dropped.
	if !stringSliceContains(app.Deps, "//elements/pkg:entry") {
		t.Errorf("direct-link entry point must be recovered even in an all-empty-Deps manifest: %v", app.Deps)
	}
	if stringSliceContains(app.Tags, "cmake-transitive-link-drop=Pkg::entry") {
		t.Errorf("entry point must be attributed, not dropped: %v", app.Tags)
	}
	// The transitive internal (in w's LinkClosure) still drops.
	if !stringSliceContains(app.Tags, "cmake-transitive-link-drop=Pkg::winternal") {
		t.Errorf("archive in the wrapper's LinkClosure must drop: %v", app.Tags)
	}
	if stringSliceContains(app.Deps, "//elements/pkg:winternal") {
		t.Errorf("closure-reachable internal must not be attributed: %v", app.Deps)
	}
}
