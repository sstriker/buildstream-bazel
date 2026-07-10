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

// TestExportDeps_WrapperSeedKeepsTransitiveDrop guards the wrapper-model
// case: when the directly-traced seed is a cc_library WRAPPER label whose
// Export.Deps are empty (transitivity lives in Bazel), a non-directly-named
// flattened archive re-enters via Bazel and must still DROP — attributing
// every internal archive would over-specify the graph. The entry-edge
// recovery applies only to prebuilt/flattened seeds (non-empty Deps).
func TestExportDeps_WrapperSeedKeepsTransitiveDrop(t *testing.T) {
	res, err := manifest.Index(&manifest.Imports{
		Version: 1,
		Elements: []*manifest.Element{{
			Name: "pkg",
			Exports: []*manifest.Export{
				// Wrapper seed: label models its own deps in Bazel, Deps empty.
				{CMakeTarget: "Pkg::w", BazelLabel: "//elements/pkg:w", LinkPaths: []string{"/opt/prefix/lib/libw.a"}},
				// Internal archive on the flattened line, not named, also a wrapper.
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
					{Fragment: "/opt/prefix/lib/libinternal.a", Role: "libraries"}, // transitive via Bazel → drop
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
	// The internal archive re-enters via the wrapper's Bazel deps, so it must
	// NOT be attributed as a direct dep — only breadcrumbed.
	if stringSliceContains(app.Deps, "//elements/pkg:internal") {
		t.Errorf("wrapper-model internal archive must DROP (re-enters via Bazel), not attribute: %v", app.Deps)
	}
	if !stringSliceContains(app.Tags, "cmake-transitive-link-drop=Pkg::internal") {
		t.Errorf("dropped internal archive must leave a breadcrumb: %v", app.Tags)
	}
}

// TestExportDeps_MixedSeedsAttributeWrapperInternal pins the deliberate
// safe-direction behavior for a MIXED link — one directly-traced PREBUILT
// seed (non-empty Deps) and one WRAPPER seed (empty Deps). seedsModelOwnDeps
// is then false, so the gate uses the manifest closure only: a prebuilt
// transitive archive (in the prebuilt seed's Deps) still drops, but a
// wrapper-side internal archive (re-entering via the wrapper's INVISIBLE
// Bazel closure) is ATTRIBUTED, not dropped. That over-specifies a
// consumer-visible export label — the safe direction — because dropping it
// on a guess would risk undefined symbols if it were really a direct entry.
func TestExportDeps_MixedSeedsAttributeWrapperInternal(t *testing.T) {
	res, err := manifest.Index(&manifest.Imports{
		Version: 1,
		Elements: []*manifest.Element{{
			Name: "pkg",
			Exports: []*manifest.Export{
				// Prebuilt seed: bare cc_import, flattened Deps.
				{CMakeTarget: "Pkg::p", BazelLabel: "//elements/pkg:p", Deps: []string{"//elements/pkg:pdep"}, LinkPaths: []string{"/opt/prefix/lib/libp.a"}},
				{CMakeTarget: "Pkg::pdep", BazelLabel: "//elements/pkg:pdep", LinkPaths: []string{"/opt/prefix/lib/libpdep.a"}},
				// Wrapper seed: empty Deps (transitivity in Bazel).
				{CMakeTarget: "Pkg::w", BazelLabel: "//elements/pkg:w", LinkPaths: []string{"/opt/prefix/lib/libw.a"}},
				// Wrapper's internal archive — re-enters via the wrapper's Bazel
				// closure, which the manifest doesn't expose.
				{CMakeTarget: "Pkg::winternal", BazelLabel: "//elements/pkg:winternal", LinkPaths: []string{"/opt/prefix/lib/libwinternal.a"}},
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
					{Fragment: "/opt/prefix/lib/libp.a", Role: "libraries"},         // traced prebuilt → wired
					{Fragment: "/opt/prefix/lib/libpdep.a", Role: "libraries"},      // prebuilt closure → drop
					{Fragment: "/opt/prefix/lib/libw.a", Role: "libraries"},         // traced wrapper → wired
					{Fragment: "/opt/prefix/lib/libwinternal.a", Role: "libraries"}, // wrapper internal → attribute (safe)
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
		`{"args":["app","PUBLIC","Pkg::p","Pkg::w"],"cmd":"target_link_libraries","file":"/s/CMakeLists.txt","line":3}` + "\n",
	)
	pkg, err := ToIR(r, &ninja.Graph{}, Options{Imports: res, TraceRaw: traceRaw, HostSourceRoot: "/s"})
	if err != nil {
		t.Fatalf("ToIR: %v", err)
	}
	app := exportDepsFind(t, pkg, "app")
	// Prebuilt transitive archive drops via the closure (breadcrumb); its
	// label still rides p's addExport closure.
	if !stringSliceContains(app.Tags, "cmake-transitive-link-drop=Pkg::pdep") {
		t.Errorf("prebuilt transitive archive must drop via the closure: %v", app.Tags)
	}
	// Wrapper internal is ATTRIBUTED (safe direction), NOT dropped.
	if !stringSliceContains(app.Deps, "//elements/pkg:winternal") {
		t.Errorf("mixed-seed wrapper internal must be attributed (safe over-spec): %v", app.Deps)
	}
	if stringSliceContains(app.Tags, "cmake-transitive-link-drop=Pkg::winternal") {
		t.Errorf("wrapper internal must NOT drop in a mixed link (would risk undefined symbols): %v", app.Tags)
	}
}
