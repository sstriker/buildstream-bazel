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

// TestExportDeps_PathFormDirectLinkRecovered pins the path-aware direct-link
// gate in the REAL (wrapper-rewritten) shape — every Export.Deps empty, so
// Bazel carries transitivity through the wrappers. app links Pkg::a by NAME
// and Pkg::z by PATH (the find_package variable form the trace records as the
// resolved path); Pkg::b is only a transitive archive. Both direct links are
// kept; the transitive one drops (with a breadcrumb) and re-enters through
// Pkg::a's wrapper cc_library's Bazel deps. This is the #806/#811 entry-point
// fix with no precomputed closure — the path form is just another direct link.
func TestExportDeps_PathFormDirectLinkRecovered(t *testing.T) {
	res, err := manifest.Index(&manifest.Imports{
		Version: 1,
		Elements: []*manifest.Element{{
			Name: "pkg",
			Exports: []*manifest.Export{
				{CMakeTarget: "Pkg::a", BazelLabel: "//elements/pkg:a_import", LinkPaths: []string{"/opt/prefix/lib/liba.a"}},
				{CMakeTarget: "Pkg::b", BazelLabel: "//elements/pkg:b_import", LinkPaths: []string{"/opt/prefix/lib/libb.a"}},
				{CMakeTarget: "Pkg::z", BazelLabel: "//elements/pkg:z_import", LinkPaths: []string{"/opt/prefix/lib/libz.a"}},
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
					{Fragment: "/opt/prefix/lib/liba.a", Role: "libraries"}, // direct by NAME → keep
					{Fragment: "/opt/prefix/lib/libb.a", Role: "libraries"}, // transitive → drop
					{Fragment: "/opt/prefix/lib/libz.a", Role: "libraries"}, // direct by PATH → keep
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
	// The trace names Pkg::a and lists libz.a by PATH (find_package var form).
	traceRaw := []byte(
		`{"args":["app","PUBLIC","Pkg::a","/opt/prefix/lib/libz.a"],"cmd":"target_link_libraries","file":"/s/CMakeLists.txt","line":3}` + "\n",
	)
	pkg, err := ToIR(r, &ninja.Graph{}, Options{Imports: res, TraceRaw: traceRaw, HostSourceRoot: "/s"})
	if err != nil {
		t.Fatalf("ToIR: %v", err)
	}
	app := exportDepsFind(t, pkg, "app")
	// Both direct links kept — a by name, z by path.
	for _, want := range []string{"//elements/pkg:a_import", "//elements/pkg:z_import"} {
		if !stringSliceContains(app.Deps, want) {
			t.Errorf("direct link %q must be kept: %v", want, app.Deps)
		}
	}
	// The transitive archive is NOT attributed (Bazel pulls it via a's wrapper).
	if stringSliceContains(app.Deps, "//elements/pkg:b_import") {
		t.Errorf("transitive Pkg::b must not be attributed (re-enters via a's wrapper): %v", app.Deps)
	}
	if !stringSliceContains(app.Tags, "cmake-transitive-link-drop=Pkg::b") {
		t.Errorf("transitive Pkg::b must drop with a breadcrumb; tags=%v", app.Tags)
	}
	// The path-form direct link is kept, not dropped.
	if stringSliceContains(app.Tags, "cmake-transitive-link-drop=Pkg::z") {
		t.Errorf("path-form direct link Pkg::z must be kept, not dropped: %v", app.Tags)
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

// TestExportDeps_MixedSeedsDropWrapperInternal pins the path-aware gate on a
// MIXED link: one directly-named PREBUILT seed (Pkg::p, whose manifest models
// a flattened closure via non-empty Deps) and one directly-named WRAPPER seed
// (Pkg::w, empty Deps — transitivity lives in Bazel). The gate itself is
// uniform — it drops every fragment this target did not directly name, by
// CMake target or by library path, leaving a breadcrumb. What differs
// downstream is where a dropped archive's label re-enters:
//
//   - Pkg::pdep's fragment drops, but its label still rides in through
//     Pkg::p's addExport closure, because the prebuilt manifest listed it as
//     one of p's flattened Deps.
//   - Pkg::winternal's fragment drops and its label does NOT re-enter here,
//     because the wrapper seed Pkg::w carries no Deps — winternal reaches the
//     link only through w's invisible Bazel closure. Under the old
//     seedsModelOwnDeps split this was over-attributed; Design B drops it.
func TestExportDeps_MixedSeedsDropWrapperInternal(t *testing.T) {
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
	// The two directly-named seeds are attributed.
	if !stringSliceContains(app.Deps, "//elements/pkg:p") {
		t.Errorf("directly-named prebuilt seed must be attributed: %v", app.Deps)
	}
	if !stringSliceContains(app.Deps, "//elements/pkg:w") {
		t.Errorf("directly-named wrapper seed must be attributed: %v", app.Deps)
	}
	// Both non-directly-named fragments drop at the gate (breadcrumbs).
	if !stringSliceContains(app.Tags, "cmake-transitive-link-drop=Pkg::pdep") {
		t.Errorf("prebuilt transitive archive fragment must drop with a breadcrumb: %v", app.Tags)
	}
	if !stringSliceContains(app.Tags, "cmake-transitive-link-drop=Pkg::winternal") {
		t.Errorf("wrapper internal fragment must drop with a breadcrumb: %v", app.Tags)
	}
	// pdep's label re-enters via p's flattened prebuilt closure; winternal's
	// does not, because the wrapper seed w carries no manifest Deps.
	if !stringSliceContains(app.Deps, "//elements/pkg:pdep") {
		t.Errorf("prebuilt transitive archive must ride in via p's closure: %v", app.Deps)
	}
	if stringSliceContains(app.Deps, "//elements/pkg:winternal") {
		t.Errorf("wrapper internal must NOT be attributed (re-enters via w's Bazel closure): %v", app.Deps)
	}
}
