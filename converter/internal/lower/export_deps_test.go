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

// TestExportDeps_StaticLibDirectPathArmsHandled pins that a STATIC_LIBRARY —
// which has no Link section, so lowerLinkFragments never runs for it — still
// has its DIRECT path-form trace arms classified by attributeDirectTraceDeps
// itself: an un-claimed system-library path lifts to -l<name>, and a vendored
// path the manifest doesn't carry surfaces as an unresolved-link-arm gap.
// Without that, these direct deps would vanish silently (the fragment pass
// can't cover a target with no link line).
func TestExportDeps_StaticLibDirectPathArmsHandled(t *testing.T) {
	r := &fileapi.Reply{
		Targets: map[string]fileapi.Target{"mystatic::@": {
			Name: "mystatic", Type: "STATIC_LIBRARY",
			Sources:       []fileapi.TargetSource{{Path: "c.c", CompileGroupIndex: 0}},
			CompileGroups: []fileapi.CompileGroup{{Language: "C", SourceIndexes: []int{0}}},
		}},
		Codemodel: fileapi.Codemodel{
			Configurations: []fileapi.Configuration{{
				Name:    "Release",
				Targets: []fileapi.ConfigTargetRef{{Id: "mystatic::@", Name: "mystatic"}},
			}},
		},
	}
	// Direct path-form links on a static archive (no Link section in the
	// codemodel): a system lib and a vendored path, neither in the manifest.
	traceRaw := []byte(
		`{"args":["mystatic","PRIVATE","/usr/lib/x86_64-linux-gnu/libz.so","/opt/vendor/lib/libfoo.so"],"cmd":"target_link_libraries","file":"/s/CMakeLists.txt","line":3}` + "\n",
	)
	pkg, err := ToIR(r, &ninja.Graph{}, Options{TraceRaw: traceRaw, HostSourceRoot: "/s"})
	if err != nil {
		t.Fatalf("ToIR: %v", err)
	}
	tgt := exportDepsFind(t, pkg, "mystatic")
	// System lib → -lz linkopt (toolchain owns it).
	if !stringSliceContains(tgt.LinkOpts, "-lz") {
		t.Errorf("static-lib system-lib path arm must lift to -lz: %v", tgt.LinkOpts)
	}
	// Vendored path → harvest-gap tag, not a silent drop.
	if !stringSliceContains(tgt.Tags, "cmake-unresolved-link-arm=/opt/vendor/lib/libfoo.so") {
		t.Errorf("static-lib vendored path arm must surface as an unresolved-link-arm gap: %v", tgt.Tags)
	}
}

// TestExportDeps_LinkPathChannel: a binary that links the export's archive
// by PATH — the ${FOO_LIBRARIES} arm that --trace-expand records as the
// resolved absolute path. attributeDirectTraceDeps resolves it via
// LookupLinkPath and the export's declared closure rides the addExport.
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
	// The trace links liba.a by PATH (find_package variable form).
	traceRaw := []byte(
		`{"args":["app","PUBLIC","/opt/prefix/lib/liba.a"],"cmd":"target_link_libraries","file":"/s/CMakeLists.txt","line":3}` + "\n",
	)
	pkg, err := ToIR(r, &ninja.Graph{}, Options{Imports: exportDepsResolver(t), TraceRaw: traceRaw, HostSourceRoot: "/s"})
	if err != nil {
		t.Fatalf("ToIR: %v", err)
	}
	assertExportClosure(t, exportDepsFind(t, pkg, "app").Deps, "link-path channel")
}

// TestExportDeps_PathFormDirectLinkRecovered pins trace-driven attribution in
// the REAL (wrapper-rewritten) shape — every Export.Deps empty, so Bazel
// carries transitivity through the wrappers. app links Pkg::a by NAME and
// Pkg::z by PATH (the find_package variable form the trace records as the
// resolved path); Pkg::b is only a transitive archive on the link line (no
// trace arm). Both direct links are attributed from the trace; the transitive
// one is simply not — it re-enters through Pkg::a's wrapper cc_library's own
// Bazel deps. No gate, no breadcrumb: the direct arms are the whole source.
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
					{Fragment: "/opt/prefix/lib/libb.a", Role: "libraries"}, // transitive ORPHAN → safety net
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
	// libb.a is an ORPHAN: no export declares a dep on //elements/pkg:b_import,
	// so transitive re-entry can never supply it. The fragment pass's safety net
	// wires it directly (a harvest completeness gap) rather than dropping it
	// silently. (When harvest records the real X→b edge, b becomes a non-orphan
	// and rides in through X instead — see TestExportDeps_NonOrphanTransitive*.)
	if !stringSliceContains(app.Deps, "//elements/pkg:b_import") {
		t.Errorf("orphan transitive Pkg::b must be wired by the safety net: %v", app.Deps)
	}
}

// TestExportDeps_AliasNameKeepsDirectLink pins the alias-label case: the
// manifest carries two cmake names for ONE bazel_label — an alias export
// (no LinkPaths) and the underlying claimant (LinkPaths). The trace names
// the ALIAS, but LookupLinkPath returns the claimant (different CMakeTarget,
// same BazelLabel). The direct-link gate must still KEEP the fragment — via
// the resolved-label check — not drop it as transitive.
func TestExportDeps_AliasNameKeepsDirectLink(t *testing.T) {
	res, err := manifest.Index(&manifest.Imports{
		Version: 1,
		Elements: []*manifest.Element{{
			Name: "pkg",
			Exports: []*manifest.Export{
				// Alias spelling the trace names — same label, no LinkPaths.
				{CMakeTarget: "Pkg::Foo", BazelLabel: "//elements/pkg:foo", LinkLibraries: []string{"foo"}},
				// Underlying claimant that owns the LinkPaths (LookupLinkPath hit).
				{CMakeTarget: "Pkg::foo_impl", BazelLabel: "//elements/pkg:foo", LinkPaths: []string{"/opt/prefix/lib/libfoo.a"}},
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
					{Fragment: "/opt/prefix/lib/libfoo.a", Role: "libraries"},
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
	// Trace names the ALIAS Pkg::Foo, not the LinkPaths-owning Pkg::foo_impl.
	traceRaw := []byte(
		`{"args":["app","PUBLIC","Pkg::Foo"],"cmd":"target_link_libraries","file":"/s/CMakeLists.txt","line":3}` + "\n",
	)
	pkg, err := ToIR(r, &ninja.Graph{}, Options{Imports: res, TraceRaw: traceRaw, HostSourceRoot: "/s"})
	if err != nil {
		t.Fatalf("ToIR: %v", err)
	}
	app := exportDepsFind(t, pkg, "app")
	if !stringSliceContains(app.Deps, "//elements/pkg:foo") {
		t.Errorf("alias-named direct link must be kept via the label check: %v", app.Deps)
	}
	if stringSliceContains(app.Tags, "cmake-transitive-link-drop=Pkg::foo_impl") {
		t.Errorf("alias-named direct link must NOT drop: %v", app.Tags)
	}
}

// TestExportDeps_PathFormPrivateRoutesToImplDeps pins scope routing for a
// PATH-form direct link: target_link_libraries(lib PRIVATE /opt/.../libz.a)
// keys traceLinkScope by the raw path, not by the export's CMakeTarget. The
// gate must consult BOTH spellings so the PRIVATE link lands in
// ImplementationDeps, not Deps.
func TestExportDeps_PathFormPrivateRoutesToImplDeps(t *testing.T) {
	res, err := manifest.Index(&manifest.Imports{
		Version: 1,
		Elements: []*manifest.Element{{
			Name: "pkg",
			Exports: []*manifest.Export{
				{CMakeTarget: "Pkg::z", BazelLabel: "//elements/pkg:z_import", LinkPaths: []string{"/opt/prefix/lib/libz.a"}},
			},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	r := &fileapi.Reply{
		Targets: map[string]fileapi.Target{"lib::@": {
			Name: "lib", Type: "SHARED_LIBRARY",
			Sources:       []fileapi.TargetSource{{Path: "m.c", CompileGroupIndex: 0}},
			CompileGroups: []fileapi.CompileGroup{{Language: "C", SourceIndexes: []int{0}}},
			Link: &fileapi.TargetLink{
				Language: "C",
				CommandFragments: []fileapi.CommandFragment{
					{Fragment: "/opt/prefix/lib/libz.a", Role: "libraries"},
				},
			},
		}},
		Codemodel: fileapi.Codemodel{
			Configurations: []fileapi.Configuration{{
				Name:    "Release",
				Targets: []fileapi.ConfigTargetRef{{Id: "lib::@", Name: "lib"}},
			}},
		},
	}
	// PRIVATE link, named by PATH — traceLinkScope keys off the raw path.
	traceRaw := []byte(
		`{"args":["lib","PRIVATE","/opt/prefix/lib/libz.a"],"cmd":"target_link_libraries","file":"/s/CMakeLists.txt","line":3}` + "\n",
	)
	pkg, err := ToIR(r, &ninja.Graph{}, Options{Imports: res, TraceRaw: traceRaw, HostSourceRoot: "/s"})
	if err != nil {
		t.Fatalf("ToIR: %v", err)
	}
	lib := exportDepsFind(t, pkg, "lib")
	if !stringSliceContains(lib.ImplementationDeps, "//elements/pkg:z_import") {
		t.Errorf("path-form PRIVATE link must route to ImplementationDeps: impl=%v deps=%v", lib.ImplementationDeps, lib.Deps)
	}
	if stringSliceContains(lib.Deps, "//elements/pkg:z_import") {
		t.Errorf("path-form PRIVATE link must NOT land in public Deps: %v", lib.Deps)
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
// Export.Deps are empty (transitivity lives in Bazel), a flattened archive
// on the link line that the trace does NOT name is not attributed — it
// re-enters via the wrapper's own Bazel deps. Attributing every internal
// archive would over-specify the graph; here it simply never enters, because
// only the direct trace arms are a dep source.
// TestExportDeps_OrphanArchiveWiredBySafetyNet pins the consumer safety net: an
// archive on the flattened link line that resolves to an export NO other export
// depends on (an orphan — a harvest completeness gap) is wired directly by
// lowerLinkFragments rather than dropped. Without the net it would vanish: it is
// not a direct trace arm, and no wrapper's manifest closure carries it.
func TestExportDeps_OrphanArchiveWiredBySafetyNet(t *testing.T) {
	res, err := manifest.Index(&manifest.Imports{
		Version: 1,
		Elements: []*manifest.Element{{
			Name: "pkg",
			Exports: []*manifest.Export{
				// Directly-traced seed.
				{CMakeTarget: "Pkg::w", BazelLabel: "//elements/pkg:w", LinkPaths: []string{"/opt/prefix/lib/libw.a"}},
				// Orphan archive on the flattened line: no export depends on its
				// label, so nothing re-enters it → the safety net must wire it.
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
					{Fragment: "/opt/prefix/lib/libinternal.a", Role: "libraries"}, // orphan → safety net
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
		t.Errorf("directly-traced seed must be wired: %v", app.Deps)
	}
	// The orphan archive has no trace arm and no export depends on its label, so
	// the safety net wires it directly — otherwise it would drop silently.
	if !stringSliceContains(app.Deps, "//elements/pkg:internal") {
		t.Errorf("orphan archive must be wired by the safety net: %v", app.Deps)
	}
}

// TestExportDeps_MixedPrebuiltClosureAndOrphan pins the two ways a non-trace-arm
// archive on the flattened link line gets wired, side by side: one directly-named
// PREBUILT seed (Pkg::p, whose manifest models a flattened closure via non-empty
// Deps) and one directly-named seed (Pkg::w, empty Deps):
//
//   - Pkg::pdep is not a trace arm, but its label rides in through Pkg::p's
//     addExport closure — the prebuilt manifest listed it among p's Deps, so it
//     is a NON-orphan; the safety net leaves it to that ride-along (does not
//     double-wire it).
//   - Pkg::winternal is not a trace arm and NO export depends on its label — an
//     orphan. Transitive re-entry can never supply it, so the safety net wires
//     it directly (a harvest completeness gap). When harvest records the real
//     w→winternal edge it becomes a non-orphan and rides in through w instead.
func TestExportDeps_MixedPrebuiltClosureAndOrphan(t *testing.T) {
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
					{Fragment: "/opt/prefix/lib/libpdep.a", Role: "libraries"},      // non-orphan → rides in via p
					{Fragment: "/opt/prefix/lib/libw.a", Role: "libraries"},         // traced seed → wired
					{Fragment: "/opt/prefix/lib/libwinternal.a", Role: "libraries"}, // orphan → safety net
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
	// pdep (non-orphan) re-enters via p's flattened prebuilt closure — the safety
	// net leaves it to that ride-along. winternal (orphan) is wired directly by
	// the safety net. Both land in deps, via different mechanisms.
	if !stringSliceContains(app.Deps, "//elements/pkg:pdep") {
		t.Errorf("non-orphan transitive archive must ride in via p's closure: %v", app.Deps)
	}
	if !stringSliceContains(app.Deps, "//elements/pkg:winternal") {
		t.Errorf("orphan archive must be wired by the safety net: %v", app.Deps)
	}
}

// TestExportDeps_NonOrphanTransitiveStaysSuppressed proves the safety net's
// scope guard: a transitive archive on the link line that SOME export depends on
// (here Pkg::x declares a dep on Pkg::si) is a NON-orphan, so the fragment pass
// leaves it suppressed even when the depending export is not itself linked by
// this target — it trusts si to re-enter through x wherever x is used, rather
// than adding every transitive .a as a direct dep. This is the guard that keeps
// deps trace-driven; without it the net would over-attribute.
func TestExportDeps_NonOrphanTransitiveStaysSuppressed(t *testing.T) {
	res, err := manifest.Index(&manifest.Imports{
		Version: 1,
		Elements: []*manifest.Element{{
			Name: "pkg",
			Exports: []*manifest.Export{
				// Directly-traced seed.
				{CMakeTarget: "Pkg::w", BazelLabel: "//elements/pkg:w", LinkPaths: []string{"/opt/prefix/lib/libw.a"}},
				// x depends on si → si is a NON-orphan. x is NOT linked here.
				{CMakeTarget: "Pkg::x", BazelLabel: "//elements/pkg:x", Deps: []string{"//elements/pkg:si"}, LinkPaths: []string{"/opt/prefix/lib/libx.a"}},
				{CMakeTarget: "Pkg::si", BazelLabel: "//elements/pkg:si", LinkPaths: []string{"/opt/prefix/lib/libsi.a"}},
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
					{Fragment: "/opt/prefix/lib/libw.a", Role: "libraries"},  // directly traced → wired
					{Fragment: "/opt/prefix/lib/libsi.a", Role: "libraries"}, // non-orphan transitive → suppressed
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
		t.Errorf("directly-traced seed must be wired: %v", app.Deps)
	}
	// si is depended on by x → non-orphan → the safety net must NOT wire it, and
	// x is not linked here so there is no ride-along either. si stays out.
	if stringSliceContains(app.Deps, "//elements/pkg:si") {
		t.Errorf("non-orphan transitive si must stay suppressed (not safety-net-wired): %v", app.Deps)
	}
	// Nor is it tagged an unresolved gap — it resolved fine, it just re-enters
	// elsewhere.
	for _, tag := range app.Tags {
		if tag == "cmake-unresolved-link-arm=/opt/prefix/lib/libsi.a" {
			t.Errorf("non-orphan si must not be tagged unresolved: %v", app.Tags)
		}
	}
}

// TestExportDeps_AbsolutePathBasenameFallback pins mode-2: a direct absolute
// archive arm whose EXACT path isn't in the manifest's link_paths still
// resolves to its wrapper by the archive basename — via a link_libraries name,
// and spelling-tolerantly (libfoo-bar.a matches a "foo_bar" name). A
// STATIC_LIBRARY (no link section) exercises the trace pass specifically.
func TestExportDeps_AbsolutePathBasenameFallback(t *testing.T) {
	res, err := manifest.Index(&manifest.Imports{
		Version: 1,
		Elements: []*manifest.Element{{
			Name: "pkg",
			Exports: []*manifest.Export{
				// Provides "foo" by NAME only — NOT by the arm's exact path.
				{CMakeTarget: "Pkg::foo", BazelLabel: "//elements/pkg:foo", LinkLibraries: []string{"foo"}},
				// Provides "foo_bar" by NAME; the arm spells it libfoo-bar.a.
				{CMakeTarget: "Pkg::foobar", BazelLabel: "//elements/pkg:foobar", LinkLibraries: []string{"foo_bar"}},
			},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	r := &fileapi.Reply{
		Targets: map[string]fileapi.Target{"mystatic::@": {
			Name: "mystatic", Type: "STATIC_LIBRARY",
			Sources:       []fileapi.TargetSource{{Path: "c.c", CompileGroupIndex: 0}},
			CompileGroups: []fileapi.CompileGroup{{Language: "C", SourceIndexes: []int{0}}},
		}},
		Codemodel: fileapi.Codemodel{
			Configurations: []fileapi.Configuration{{
				Name:    "Release",
				Targets: []fileapi.ConfigTargetRef{{Id: "mystatic::@", Name: "mystatic"}},
			}},
		},
	}
	traceRaw := []byte(
		`{"args":["mystatic","PUBLIC","/opt/prefix/lib/libfoo.a","/opt/prefix/lib/libfoo-bar.a"],"cmd":"target_link_libraries","file":"/s/CMakeLists.txt","line":3}` + "\n",
	)
	pkg, err := ToIR(r, &ninja.Graph{}, Options{Imports: res, TraceRaw: traceRaw, HostSourceRoot: "/s"})
	if err != nil {
		t.Fatalf("ToIR: %v", err)
	}
	tgt := exportDepsFind(t, pkg, "mystatic")
	for _, want := range []string{"//elements/pkg:foo", "//elements/pkg:foobar"} {
		if !stringSliceContains(tgt.Deps, want) {
			t.Errorf("basename fallback must attribute %q: deps=%v", want, tgt.Deps)
		}
	}
	for _, tag := range tgt.Tags {
		if tag == "cmake-unresolved-link-arm=/opt/prefix/lib/libfoo.a" || tag == "cmake-unresolved-link-arm=/opt/prefix/lib/libfoo-bar.a" {
			t.Errorf("basename-resolved archive must not be tagged unresolved: %v", tgt.Tags)
		}
	}
}
