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
