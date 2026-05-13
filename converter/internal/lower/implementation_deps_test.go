package lower_test

import (
	"testing"

	"github.com/sstriker/cmake-to-bazel/converter/internal/fileapi"
	"github.com/sstriker/cmake-to-bazel/converter/internal/lower"
	"github.com/sstriker/cmake-to-bazel/internal/manifest"
)

// TestToIR_PrivateDepRoutesToImplementationDeps covers the
// Phase 4 enrichment: when shadow.Decode recovers the
// PUBLIC/PRIVATE/INTERFACE keyword from a
// target_link_libraries trace event, ToIR routes a PRIVATE
// dep to ir.Target.ImplementationDeps rather than ir.Target.Deps.
//
// The trace fixture records `target_link_libraries(client
// PRIVATE Glibc::c)`. The codemodel mirrors the same dep
// (since codemodel and trace observe the same call). After
// lowering: ImplementationDeps = ["//elements/components/glibc:c"]
// and Deps = [].
func TestToIR_PrivateDepRoutesToImplementationDeps(t *testing.T) {
	r := &fileapi.Reply{
		Codemodel: fileapi.Codemodel{
			Paths: fileapi.CodemodelPaths{Source: "/src"},
			Configurations: []fileapi.Configuration{{
				Name:    "Release",
				Targets: []fileapi.ConfigTargetRef{{Name: "client", Id: "client::@1"}},
			}},
		},
		Targets: map[string]fileapi.Target{
			"client::@1": {
				Name:    "client",
				Type:    "STATIC_LIBRARY",
				Sources: []fileapi.TargetSource{{Path: "client.c", CompileGroupIndex: 0}},
				CompileGroups: []fileapi.CompileGroup{{
					Language:      "C",
					SourceIndexes: []int{0},
				}},
				Dependencies: []fileapi.TargetDependency{
					{Id: "Glibc::c::@somehash"},
				},
			},
		},
	}
	rsv, err := manifest.Index(&manifest.Imports{
		Version: 1,
		Elements: []*manifest.Element{{
			Name: "components/glibc",
			Exports: []*manifest.Export{{
				CMakeTarget: "Glibc::c",
				BazelLabel:  "//elements/components/glibc:c",
			}},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	trace := []byte(`{"args":["client","PRIVATE","Glibc::c"],"cmd":"target_link_libraries","file":"/src/CMakeLists.txt","line":3}` + "\n")
	pkg, err := lower.ToIR(r, nil, lower.Options{
		HostSourceRoot: "/src",
		Imports:        rsv,
		TraceRaw:       trace,
	})
	if err != nil {
		t.Fatalf("ToIR: %v", err)
	}
	if len(pkg.Targets) != 1 {
		t.Fatalf("Targets = %d, want 1", len(pkg.Targets))
	}
	tgt := pkg.Targets[0]
	if len(tgt.Deps) != 0 {
		t.Errorf("Deps = %v, want [] (PRIVATE dep should not be here)", tgt.Deps)
	}
	if len(tgt.ImplementationDeps) != 1 || tgt.ImplementationDeps[0] != "//elements/components/glibc:c" {
		t.Errorf("ImplementationDeps = %v, want [//elements/components/glibc:c]", tgt.ImplementationDeps)
	}
}

// TestToIR_PublicDepStaysInDeps covers the no-regression
// path: PUBLIC-keyword deps continue to land in Deps
// (header propagation preserved, matches pre-Phase-4
// behavior).
func TestToIR_PublicDepStaysInDeps(t *testing.T) {
	r := &fileapi.Reply{
		Codemodel: fileapi.Codemodel{
			Paths: fileapi.CodemodelPaths{Source: "/src"},
			Configurations: []fileapi.Configuration{{
				Name:    "Release",
				Targets: []fileapi.ConfigTargetRef{{Name: "client", Id: "client::@1"}},
			}},
		},
		Targets: map[string]fileapi.Target{
			"client::@1": {
				Name:    "client",
				Type:    "STATIC_LIBRARY",
				Sources: []fileapi.TargetSource{{Path: "client.c", CompileGroupIndex: 0}},
				CompileGroups: []fileapi.CompileGroup{{
					Language:      "C",
					SourceIndexes: []int{0},
				}},
				Dependencies: []fileapi.TargetDependency{
					{Id: "Glibc::c::@somehash"},
				},
			},
		},
	}
	rsv, _ := manifest.Index(&manifest.Imports{
		Version: 1,
		Elements: []*manifest.Element{{
			Name: "components/glibc",
			Exports: []*manifest.Export{{
				CMakeTarget: "Glibc::c",
				BazelLabel:  "//elements/components/glibc:c",
			}},
		}},
	})
	trace := []byte(`{"args":["client","PUBLIC","Glibc::c"],"cmd":"target_link_libraries","file":"/src/CMakeLists.txt","line":3}` + "\n")
	pkg, err := lower.ToIR(r, nil, lower.Options{
		HostSourceRoot: "/src",
		Imports:        rsv,
		TraceRaw:       trace,
	})
	if err != nil {
		t.Fatalf("ToIR: %v", err)
	}
	tgt := pkg.Targets[0]
	if len(tgt.ImplementationDeps) != 0 {
		t.Errorf("ImplementationDeps = %v, want [] (PUBLIC dep should not be here)", tgt.ImplementationDeps)
	}
	if len(tgt.Deps) != 1 || tgt.Deps[0] != "//elements/components/glibc:c" {
		t.Errorf("Deps = %v, want [//elements/components/glibc:c]", tgt.Deps)
	}
}

// TestToIR_NoTraceFallsBackToDeps covers the no-signal case:
// when TraceRaw is absent (codemodel-only path), every dep
// folds into Deps regardless of cmake-side scope. Matches
// pre-Phase-4 behavior byte-for-byte.
func TestToIR_NoTraceFallsBackToDeps(t *testing.T) {
	r := &fileapi.Reply{
		Codemodel: fileapi.Codemodel{
			Paths: fileapi.CodemodelPaths{Source: "/src"},
			Configurations: []fileapi.Configuration{{
				Name:    "Release",
				Targets: []fileapi.ConfigTargetRef{{Name: "client", Id: "client::@1"}},
			}},
		},
		Targets: map[string]fileapi.Target{
			"client::@1": {
				Name:    "client",
				Type:    "STATIC_LIBRARY",
				Sources: []fileapi.TargetSource{{Path: "client.c", CompileGroupIndex: 0}},
				CompileGroups: []fileapi.CompileGroup{{
					Language:      "C",
					SourceIndexes: []int{0},
				}},
				Dependencies: []fileapi.TargetDependency{
					{Id: "Glibc::c::@somehash"},
				},
			},
		},
	}
	rsv, _ := manifest.Index(&manifest.Imports{
		Version: 1,
		Elements: []*manifest.Element{{
			Name: "components/glibc",
			Exports: []*manifest.Export{{
				CMakeTarget: "Glibc::c",
				BazelLabel:  "//elements/components/glibc:c",
			}},
		}},
	})
	pkg, err := lower.ToIR(r, nil, lower.Options{
		HostSourceRoot: "/src",
		Imports:        rsv,
		// No TraceRaw — codemodel-only path.
	})
	if err != nil {
		t.Fatalf("ToIR: %v", err)
	}
	tgt := pkg.Targets[0]
	if len(tgt.ImplementationDeps) != 0 {
		t.Errorf("ImplementationDeps = %v, want [] (no trace signal — should fall back to Deps)", tgt.ImplementationDeps)
	}
	if len(tgt.Deps) != 1 {
		t.Errorf("Deps = %v, want 1 element", tgt.Deps)
	}
}

// TestToIR_CCBinaryPrivateDepFoldsIntoDeps covers the
// rule-kind gating: cc_binary doesn't accept
// `implementation_deps` (stock rules_cc reserves it for
// cc_library only). PRIVATE deps on EXECUTABLE targets must
// fold into `Deps` even when the trace records the keyword.
//
// Regression guard: this was the failure mode in PR #123's
// `bazel-e2e` CI job — a cc_binary's BUILD.bazel emitted
// `implementation_deps = [...]` and bazel rejected the load
// at analysis time.
func TestToIR_CCBinaryPrivateDepFoldsIntoDeps(t *testing.T) {
	r := &fileapi.Reply{
		Codemodel: fileapi.Codemodel{
			Paths: fileapi.CodemodelPaths{Source: "/src"},
			Configurations: []fileapi.Configuration{{
				Name:    "Release",
				Targets: []fileapi.ConfigTargetRef{{Name: "client_bin", Id: "client_bin::@1"}},
			}},
		},
		Targets: map[string]fileapi.Target{
			"client_bin::@1": {
				Name:    "client_bin",
				Type:    "EXECUTABLE",
				Sources: []fileapi.TargetSource{{Path: "main.c", CompileGroupIndex: 0}},
				CompileGroups: []fileapi.CompileGroup{{
					Language: "C", SourceIndexes: []int{0},
				}},
				Dependencies: []fileapi.TargetDependency{
					{Id: "Glibc::c::@somehash"},
				},
			},
		},
	}
	rsv, _ := manifest.Index(&manifest.Imports{
		Version: 1,
		Elements: []*manifest.Element{{
			Name: "components/glibc",
			Exports: []*manifest.Export{{
				CMakeTarget: "Glibc::c",
				BazelLabel:  "//elements/components/glibc:c",
			}},
		}},
	})
	trace := []byte(`{"args":["client_bin","PRIVATE","Glibc::c"],"cmd":"target_link_libraries","file":"/src/CMakeLists.txt","line":3}` + "\n")
	pkg, err := lower.ToIR(r, nil, lower.Options{
		HostSourceRoot: "/src",
		Imports:        rsv,
		TraceRaw:       trace,
	})
	if err != nil {
		t.Fatalf("ToIR: %v", err)
	}
	tgt := pkg.Targets[0]
	if len(tgt.ImplementationDeps) != 0 {
		t.Errorf("ImplementationDeps = %v, want [] (cc_binary doesn't accept the attribute)", tgt.ImplementationDeps)
	}
	if len(tgt.Deps) != 1 || tgt.Deps[0] != "//elements/components/glibc:c" {
		t.Errorf("Deps = %v, want [//elements/components/glibc:c]", tgt.Deps)
	}
}

// TestToIR_InCodebasePrivateDepRoutesToImplementationDeps
// covers the in-codebase target case: when the trace
// records `target_link_libraries(consumer PRIVATE helper)`
// and helper is a sibling target in the same codemodel,
// the resulting `:helper` label routes to
// ImplementationDeps rather than Deps.
func TestToIR_InCodebasePrivateDepRoutesToImplementationDeps(t *testing.T) {
	r := &fileapi.Reply{
		Codemodel: fileapi.Codemodel{
			Paths: fileapi.CodemodelPaths{Source: "/src"},
			Configurations: []fileapi.Configuration{{
				Name: "Release",
				Targets: []fileapi.ConfigTargetRef{
					{Name: "consumer", Id: "consumer::@1"},
					{Name: "helper", Id: "helper::@2"},
				},
			}},
		},
		Targets: map[string]fileapi.Target{
			"consumer::@1": {
				Name:    "consumer",
				Type:    "STATIC_LIBRARY",
				Sources: []fileapi.TargetSource{{Path: "consumer.c", CompileGroupIndex: 0}},
				CompileGroups: []fileapi.CompileGroup{{
					Language:      "C",
					SourceIndexes: []int{0},
				}},
				Dependencies: []fileapi.TargetDependency{
					{Id: "helper::@2"},
				},
			},
			"helper::@2": {
				Name:    "helper",
				Type:    "STATIC_LIBRARY",
				Sources: []fileapi.TargetSource{{Path: "helper.c", CompileGroupIndex: 0}},
				CompileGroups: []fileapi.CompileGroup{{
					Language:      "C",
					SourceIndexes: []int{0},
				}},
			},
		},
	}
	trace := []byte(`{"args":["consumer","PRIVATE","helper"],"cmd":"target_link_libraries","file":"/src/CMakeLists.txt","line":3}` + "\n")
	pkg, err := lower.ToIR(r, nil, lower.Options{
		HostSourceRoot: "/src",
		TraceRaw:       trace,
	})
	if err != nil {
		t.Fatalf("ToIR: %v", err)
	}
	// Locate consumer target.
	var consumer *struct {
		Deps               []string
		ImplementationDeps []string
	}
	for _, tgt := range pkg.Targets {
		if tgt.Name == "consumer" {
			consumer = &struct {
				Deps               []string
				ImplementationDeps []string
			}{Deps: tgt.Deps, ImplementationDeps: tgt.ImplementationDeps}
			break
		}
	}
	if consumer == nil {
		t.Fatalf("consumer target not found: %+v", pkg.Targets)
	}
	if len(consumer.Deps) != 0 {
		t.Errorf("Deps = %v, want [] (in-codebase PRIVATE dep should not be here)", consumer.Deps)
	}
	if len(consumer.ImplementationDeps) != 1 || consumer.ImplementationDeps[0] != ":helper" {
		t.Errorf("ImplementationDeps = %v, want [:helper]", consumer.ImplementationDeps)
	}
}
