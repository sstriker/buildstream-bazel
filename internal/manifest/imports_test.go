package manifest_test

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/sstriker/buildstream-bazel/internal/manifest"
)

func TestLoad_HandwrittenManifest(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "imports.json")
	body := `{
  "version": 1,
  "elements": [
    {
      "name": "components/glibc",
      "exports": [
        {
          "cmake_target": "Glibc::c",
          "bazel_label": "//elements/components/glibc:c",
          "interface_includes": ["include"]
        }
      ]
    },
    {
      "name": "components/zlib",
      "exports": [
        {
          "cmake_target": "ZLIB::ZLIB",
          "bazel_label": "//elements/components/zlib:zlib",
          "link_libraries": ["-lm"]
        }
      ]
    }
  ]
}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	r, err := manifest.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if r.Empty() {
		t.Fatal("Empty() = true on a non-empty manifest")
	}
	if e := r.LookupCMakeTarget("Glibc::c"); e == nil {
		t.Fatal("Glibc::c not found")
	} else {
		if e.BazelLabel != "//elements/components/glibc:c" {
			t.Errorf("BazelLabel = %q", e.BazelLabel)
		}
		if len(e.InterfaceIncludes) != 1 || e.InterfaceIncludes[0] != "include" {
			t.Errorf("InterfaceIncludes = %v", e.InterfaceIncludes)
		}
	}
	if e := r.LookupCMakeTarget("ZLIB::ZLIB"); e == nil {
		t.Fatal("ZLIB::ZLIB not found")
	} else if len(e.LinkLibraries) != 1 || e.LinkLibraries[0] != "-lm" {
		t.Errorf("LinkLibraries = %v", e.LinkLibraries)
	}
	if r.LookupCMakeTarget("Nonexistent::X") != nil {
		t.Errorf("missing target returned non-nil")
	}
	if el := r.LookupElement("components/glibc"); el == nil || el.Name != "components/glibc" {
		t.Errorf("LookupElement = %v", el)
	}
}

// TestLoadMerged_ProducerWins checks the consumer-side merge: a
// render-time convention base maps zlib::zlib (wrong), a producer
// exports.json maps ZLIB::ZLIB (right), and — crucially — when both
// declare the same cmake_target the later (producer) doc wins,
// instead of erroring as strict Index would.
func TestLoadMerged_ProducerWins(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "imports.json")
	prod := filepath.Join(dir, "exports.json")
	// Convention base: wrong casing + a stale label for a shared key.
	if err := os.WriteFile(base, []byte(`{"version":1,"elements":[
		{"name":"zlib","exports":[
			{"cmake_target":"zlib::zlib","bazel_label":"//elements/zlib:zlib"},
			{"cmake_target":"Shared::Shared","bazel_label":"//stale:label"}]}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	// Producer truth: real ZLIB::ZLIB + a fresh label for the shared key.
	if err := os.WriteFile(prod, []byte(`{"version":1,"elements":[
		{"name":"ZLIB","exports":[
			{"cmake_target":"ZLIB::ZLIB","bazel_label":"//elements/zlib:zlibstatic"},
			{"cmake_target":"Shared::Shared","bazel_label":"//fresh:label"}]}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	r, err := manifest.LoadMerged(base, prod)
	if err != nil {
		t.Fatalf("LoadMerged: %v", err)
	}
	if ex := r.LookupCMakeTarget("ZLIB::ZLIB"); ex == nil || ex.BazelLabel != "//elements/zlib:zlibstatic" {
		t.Errorf("ZLIB::ZLIB = %+v, want //elements/zlib:zlibstatic", ex)
	}
	// Shared key: producer (listed after base) wins.
	if ex := r.LookupCMakeTarget("Shared::Shared"); ex == nil || ex.BazelLabel != "//fresh:label" {
		t.Errorf("Shared::Shared = %+v, want //fresh:label (producer wins)", ex)
	}
	// Base-only key survives.
	if ex := r.LookupCMakeTarget("zlib::zlib"); ex == nil {
		t.Errorf("base-only zlib::zlib should survive the merge")
	}
}

// TestLoadMerged_SkipsEmptyPaths lets callers pass an empty base
// (no --imports-manifest) followed by producer docs.
func TestLoadMerged_SkipsEmptyPaths(t *testing.T) {
	dir := t.TempDir()
	prod := filepath.Join(dir, "exports.json")
	if err := os.WriteFile(prod, []byte(`{"version":1,"elements":[{"name":"P","exports":[{"cmake_target":"P::p","bazel_label":"//e/p:p"}]}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	r, err := manifest.LoadMerged("", prod)
	if err != nil {
		t.Fatalf("LoadMerged: %v", err)
	}
	if ex := r.LookupCMakeTarget("P::p"); ex == nil || ex.BazelLabel != "//e/p:p" {
		t.Errorf("P::p = %+v", ex)
	}
}

func TestIndex_RejectsDuplicateCMakeTarget(t *testing.T) {
	im := &manifest.Imports{
		Version: 1,
		Elements: []*manifest.Element{
			{Name: "a", Exports: []*manifest.Export{{CMakeTarget: "Foo::Foo", BazelLabel: "@a//:f"}}},
			{Name: "b", Exports: []*manifest.Export{{CMakeTarget: "Foo::Foo", BazelLabel: "@b//:f"}}},
		},
	}
	_, err := manifest.Index(im)
	if err == nil {
		t.Fatal("expected duplicate-target error")
	}
	if !strings.Contains(err.Error(), "Foo::Foo") {
		t.Errorf("err = %v, want to mention duplicate target", err)
	}
}

func TestIndex_RejectsUnknownVersion(t *testing.T) {
	if _, err := manifest.Index(&manifest.Imports{Version: 7}); err == nil {
		t.Errorf("expected version error")
	}
}

func TestIndex_RejectsEmptyExportFields(t *testing.T) {
	cases := []struct {
		name string
		im   *manifest.Imports
	}{
		{"empty element name", &manifest.Imports{Version: 1, Elements: []*manifest.Element{{Name: ""}}}},
		{"empty cmake_target", &manifest.Imports{Version: 1, Elements: []*manifest.Element{
			{Name: "a", Exports: []*manifest.Export{{BazelLabel: "@a//:x"}}},
		}}},
		{"empty bazel_label", &manifest.Imports{Version: 1, Elements: []*manifest.Element{
			{Name: "a", Exports: []*manifest.Export{{CMakeTarget: "X::x"}}},
		}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := manifest.Index(c.im); err == nil {
				t.Errorf("expected error")
			}
		})
	}
}

// TestExportedHeadersAndModules covers the schema extension: the
// exported_headers / import_modules fields round-trip through Load,
// and AllExports returns every export in a deterministic
// element-name-sorted order.
func TestExportedHeadersAndModules(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "imports.json")
	body := `{
  "version": 1,
  "elements": [
    {
      "name": "zlib",
      "exports": [
        {"cmake_target": "ZLIB::ZLIB", "bazel_label": "@zlib//:zlib", "exported_headers": ["zlib.h", "zconf.h"]}
      ]
    },
    {
      "name": "aaa",
      "exports": [
        {"cmake_target": "AAA::AAA", "bazel_label": "@aaa//:aaa", "import_modules": ["aaa"]}
      ]
    }
  ]
}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	r, err := manifest.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if got := r.LookupCMakeTarget("ZLIB::ZLIB").ExportedHeaders; !reflect.DeepEqual(got, []string{"zlib.h", "zconf.h"}) {
		t.Errorf("ExportedHeaders = %v", got)
	}
	if got := r.LookupCMakeTarget("AAA::AAA").ImportModules; !reflect.DeepEqual(got, []string{"aaa"}) {
		t.Errorf("ImportModules = %v", got)
	}

	all := r.AllExports()
	if len(all) != 2 {
		t.Fatalf("AllExports len = %d, want 2", len(all))
	}
	// Sorted by element name: "aaa" before "zlib".
	if all[0].CMakeTarget != "AAA::AAA" || all[1].CMakeTarget != "ZLIB::ZLIB" {
		t.Errorf("AllExports order = [%q, %q], want [AAA::AAA, ZLIB::ZLIB]", all[0].CMakeTarget, all[1].CMakeTarget)
	}

	var nilR *manifest.Resolver
	if nilR.AllExports() != nil {
		t.Errorf("nil resolver AllExports should be nil")
	}
}

func TestResolver_NilAndEmpty(t *testing.T) {
	var r *manifest.Resolver
	if !r.Empty() {
		t.Errorf("nil resolver should report empty")
	}
	if r.LookupCMakeTarget("X::x") != nil {
		t.Errorf("nil resolver should return nil")
	}

	r2, err := manifest.Index(&manifest.Imports{Version: 1})
	if err != nil {
		t.Fatal(err)
	}
	if !r2.Empty() {
		t.Errorf("zero-element manifest should report empty")
	}
}

func TestLoad_Phase6CMakeBundleFields(t *testing.T) {
	// Phase 6 of the generator-parity uplift extends the per-export
	// entry with CMakeConfigBundleLabel + CMakeImportLabels so cross-
	// element find_package consumers can resolve to the producer's
	// synthesized bundle. The fields are append-only / omitempty so
	// older manifests still load; this test pins the round-trip
	// shape for the new fields.
	dir := t.TempDir()
	path := filepath.Join(dir, "imports.json")
	body := `{
  "version": 1,
  "elements": [
    {
      "name": "components/mypkg",
      "exports": [
        {
          "cmake_target": "MyPkg::foo",
          "bazel_label": "//elements/components/mypkg:foo",
          "cmake_config_bundle_label": "//elements/components/mypkg:cmake_config_bundle",
          "cmake_import_labels": [
            "//elements/components/mypkg:foo_import",
            "//elements/components/mypkg:bar_import"
          ]
        }
      ]
    }
  ]
}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	r, err := manifest.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	e := r.LookupCMakeTarget("MyPkg::foo")
	if e == nil {
		t.Fatal("MyPkg::foo lookup failed")
	}
	if e.CMakeConfigBundleLabel != "//elements/components/mypkg:cmake_config_bundle" {
		t.Errorf("CMakeConfigBundleLabel = %q", e.CMakeConfigBundleLabel)
	}
	wantImports := []string{
		"//elements/components/mypkg:foo_import",
		"//elements/components/mypkg:bar_import",
	}
	if !reflect.DeepEqual(e.CMakeImportLabels, wantImports) {
		t.Errorf("CMakeImportLabels = %v want %v", e.CMakeImportLabels, wantImports)
	}
}

func TestLoad_LegacyManifestWithoutPhase6Fields(t *testing.T) {
	// Append-only schema contract: older manifests written before
	// Phase 6 must still load without error and the new fields
	// just stay at their zero values.
	dir := t.TempDir()
	path := filepath.Join(dir, "imports.json")
	body := `{
  "version": 1,
  "elements": [
    {
      "name": "old/zlib",
      "exports": [
        {
          "cmake_target": "ZLIB::ZLIB",
          "bazel_label": "//elements/zlib"
        }
      ]
    }
  ]
}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	r, err := manifest.Load(path)
	if err != nil {
		t.Fatalf("legacy manifest should load: %v", err)
	}
	e := r.LookupCMakeTarget("ZLIB::ZLIB")
	if e == nil {
		t.Fatal("ZLIB::ZLIB not found")
	}
	if e.CMakeConfigBundleLabel != "" {
		t.Errorf("legacy manifest should leave CMakeConfigBundleLabel empty: %q", e.CMakeConfigBundleLabel)
	}
	if e.CMakeImportLabels != nil {
		t.Errorf("legacy manifest should leave CMakeImportLabels nil: %v", e.CMakeImportLabels)
	}
}
