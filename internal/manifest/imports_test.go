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

// TestTools_LookupAndValidation covers the `tools` section: basename and
// absolute-path matches resolve to their labels, a relative multi-component
// token is matched verbatim only (never by basename), duplicates fail under
// Index, and empty match/label are authoring errors.
func TestTools_LookupAndValidation(t *testing.T) {
	r, err := manifest.Index(&manifest.Imports{
		Version: 1,
		Tools: []*manifest.Tool{
			{Match: "flatc", Label: "@flatbuffers//:flatc"},
			{Match: "/opt/host/bin/gen.py", Label: "//tools:gen"},
		},
	})
	if err != nil {
		t.Fatalf("Index: %v", err)
	}
	if !r.HasTools() {
		t.Error("HasTools should be true")
	}
	// A tools-only manifest has no cmake_target exports, so Empty (which
	// reports export presence) stays true — the genrule fast-path relies on
	// HasTools, not Empty, to proceed.
	if !r.Empty() {
		t.Error("a tools-only manifest should report Empty() (no exports)")
	}
	cases := []struct {
		token, want string
		ok          bool
	}{
		{"flatc", "@flatbuffers//:flatc", true},                // bare basename
		{"/usr/local/bin/flatc", "@flatbuffers//:flatc", true}, // abs, basename match
		{"./flatc", "", false},                                 // caller trims ./ before lookup; raw ./ no match
		{"/opt/host/bin/gen.py", "//tools:gen", true},          // verbatim absolute
		{"gen.py", "", false},                                  // basename of an abs-only entry: no match
		{"build/gen/flatc", "", false},                         // relative multi: not basename-matched
		{"thrift", "", false},                                  // unregistered
	}
	for _, c := range cases {
		got, ok := r.LookupTool(c.token)
		if ok != c.ok || got != c.want {
			t.Errorf("LookupTool(%q) = (%q,%v), want (%q,%v)", c.token, got, ok, c.want, c.ok)
		}
	}

	if _, err := manifest.Index(&manifest.Imports{
		Version: 1,
		Tools:   []*manifest.Tool{{Match: "flatc", Label: "//a"}, {Match: "flatc", Label: "//b"}},
	}); err == nil {
		t.Error("duplicate tool match should be a strict-Index error")
	}
	if _, err := manifest.Index(&manifest.Imports{
		Version: 1, Tools: []*manifest.Tool{{Match: "", Label: "//a"}},
	}); err == nil {
		t.Error("empty match should error")
	}
	if _, err := manifest.Index(&manifest.Imports{
		Version: 1, Tools: []*manifest.Tool{{Match: "flatc", Label: ""}},
	}); err == nil {
		t.Error("empty label should error")
	}
}

// TestTools_LoadMergedLastWins: a later doc's tool match overrides an earlier
// one (same last-wins precedence the merge gives every other key).
func TestTools_LoadMergedLastWins(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "base.json")
	over := filepath.Join(dir, "over.json")
	if err := os.WriteFile(base, []byte(`{"version":1,"tools":[{"match":"flatc","label":"//base:flatc"}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(over, []byte(`{"version":1,"tools":[{"match":"flatc","label":"//over:flatc"}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	r, err := manifest.LoadMerged(base, over)
	if err != nil {
		t.Fatalf("LoadMerged: %v", err)
	}
	if got, _ := r.LookupTool("flatc"); got != "//over:flatc" {
		t.Errorf("LookupTool(flatc) = %q, want //over:flatc (last wins)", got)
	}
}

// TestAddToolConventions: built-in conventions register as fallback tool
// mappings on a fresh resolver and do NOT override an operator's existing
// match (operator wins).
func TestAddToolConventions(t *testing.T) {
	r := manifest.NewResolver()
	if r.HasTools() {
		t.Error("fresh resolver should have no tools")
	}
	if err := r.AddToolConventions([]manifest.Tool{{Match: "protoc", Label: "@protobuf//:protoc"}}); err != nil {
		t.Fatal(err)
	}
	if got, _ := r.LookupTool("protoc"); got != "@protobuf//:protoc" {
		t.Errorf("LookupTool(protoc) = %q, want the convention label", got)
	}
	// Operator mapping already present → convention must not override it.
	r2, err := manifest.Index(&manifest.Imports{
		Version: 1, Tools: []*manifest.Tool{{Match: "protoc", Label: "//local:my_protoc"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := r2.AddToolConventions([]manifest.Tool{{Match: "protoc", Label: "@protobuf//:protoc"}}); err != nil {
		t.Fatal(err)
	}
	if got, _ := r2.LookupTool("protoc"); got != "//local:my_protoc" {
		t.Errorf("operator mapping should win, got %q", got)
	}
	// Empty match/label are errors.
	if err := r.AddToolConventions([]manifest.Tool{{Match: "", Label: "//a"}}); err == nil {
		t.Error("empty match should error")
	}
}

// TestProvidedLibName pins the archive-basename → -l<name> derivation and the
// non-archive rejections.
func TestProvidedLibName(t *testing.T) {
	cases := map[string]string{
		"/opt/prefix/lib/libz.a": "z",
		"libz.so.1.2.3":          "z",
		":libNAME.a":             "NAME",
		"/x/lib/libfoo-bar.a":    "foo-bar",
		"pthread":                "", // bare -l name, not an archive
		"-lz":                    "", // flag
		"lib.a":                  "", // empty name
		"/x/notalib.a":           "", // no lib prefix
	}
	for in, want := range cases {
		if got := manifest.ProvidedLibName(in); got != want {
			t.Errorf("ProvidedLibName(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestCanonLibName pins the spelling-tolerant folding (case + hyphen/underscore).
func TestCanonLibName(t *testing.T) {
	for _, in := range []string{"Foo-Bar", "foo_bar", "FOO-BAR", "foo-bar"} {
		if got := manifest.CanonLibName(in); got != "foo_bar" {
			t.Errorf("CanonLibName(%q) = %q, want foo_bar", in, got)
		}
	}
}

// TestLookupArchiveBasename: an absolute archive arm whose exact path isn't in
// byLinkPath still resolves to its export by basename — via a link_paths
// basename OR a link_libraries name — and spelling-tolerantly (libfoo-bar.a
// matches a "foo_bar" link_libraries name).
func TestLookupArchiveBasename(t *testing.T) {
	r, err := manifest.Index(&manifest.Imports{
		Version: 1,
		Elements: []*manifest.Element{{
			Name: "pkg",
			Exports: []*manifest.Export{
				{CMakeTarget: "Pkg::z", BazelLabel: "//p:z", LinkPaths: []string{"/opt/prefix/lib/libz.a"}},
				{CMakeTarget: "Pkg::foo", BazelLabel: "//p:foo", LinkLibraries: []string{"foo_bar"}},
			},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	// Resolves by the export's own link_paths basename, even for a DIFFERENT
	// absolute path spelling (relocated install) — only the basename matters.
	if ex := r.LookupArchiveBasename("/some/other/prefix/lib/libz.a"); ex == nil || ex.BazelLabel != "//p:z" {
		t.Errorf("libz.a basename must resolve to //p:z: %v", ex)
	}
	// Resolves by a link_libraries name, spelling-tolerant: archive libfoo-bar.a
	// (basename → foo-bar) matches the "foo_bar" name.
	if ex := r.LookupArchiveBasename("/x/lib/libfoo-bar.a"); ex == nil || ex.BazelLabel != "//p:foo" {
		t.Errorf("libfoo-bar.a must resolve to //p:foo via hyphen/underscore fold: %v", ex)
	}
	// An archive no export provides → nil (not a false positive).
	if ex := r.LookupArchiveBasename("/x/lib/libunknown.a"); ex != nil {
		t.Errorf("unknown archive must not resolve: %v", ex)
	}
	// A non-archive token → nil.
	if ex := r.LookupArchiveBasename("pthread"); ex != nil {
		t.Errorf("non-archive token must not resolve: %v", ex)
	}
}

// TestLookupArchiveBasename_LinkLibrariesArchiveFragment covers the index
// asymmetry fix: an export whose archive is named ONLY by an archive-shaped
// link_libraries entry (a `:libNAME.a` label fragment or a bare `libNAME.a`),
// with no link_paths, must still be reachable by the consumer's basename
// fallback. Before the fix, indexLinkLibs canon-indexed such an entry verbatim
// (`:libfoo.a`) instead of under its provided name (`foo`), so
// LookupArchiveBasename — which strips to the provided name — never found it.
func TestLookupArchiveBasename_LinkLibrariesArchiveFragment(t *testing.T) {
	r, err := manifest.Index(&manifest.Imports{
		Version: 1,
		Elements: []*manifest.Element{{
			Name: "pkg",
			Exports: []*manifest.Export{
				// pkg-config-sourced export: archive named by a leading-colon
				// label fragment, no link_paths.
				{CMakeTarget: "Pkg::foo", BazelLabel: "//p:foo", LinkLibraries: []string{":libfoo.a"}},
				// bare libNAME.a fragment, hyphen where the wrapper name would
				// use underscore — must fold and resolve.
				{CMakeTarget: "Pkg::bar", BazelLabel: "//p:bar", LinkLibraries: []string{"libbar-baz.a"}},
			},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if ex := r.LookupArchiveBasename("/opt/prefix/lib/libfoo.a"); ex == nil || ex.BazelLabel != "//p:foo" {
		t.Errorf(":libfoo.a link_libraries fragment must resolve libfoo.a to //p:foo: %v", ex)
	}
	if ex := r.LookupArchiveBasename("/opt/prefix/lib/libbar_baz.a"); ex == nil || ex.BazelLabel != "//p:bar" {
		t.Errorf("libbar-baz.a link_libraries fragment must resolve libbar_baz.a to //p:bar via fold: %v", ex)
	}
}

// TestArchiveIsOrphan pins the depended-on set the consumer safety net keys on:
// a label some export lists in Deps is NON-orphan (re-enters transitively); one
// no export depends on is an orphan the consumer must wire directly.
func TestArchiveIsOrphan(t *testing.T) {
	r, err := manifest.Index(&manifest.Imports{
		Version: 1,
		Elements: []*manifest.Element{{
			Name: "pkg",
			Exports: []*manifest.Export{
				// p declares a dep on pdep → pdep is depended on (non-orphan).
				{CMakeTarget: "Pkg::p", BazelLabel: "//p:p", Deps: []string{"//p:pdep"}, LinkPaths: []string{"/opt/prefix/lib/libp.a"}},
				{CMakeTarget: "Pkg::pdep", BazelLabel: "//p:pdep", LinkPaths: []string{"/opt/prefix/lib/libpdep.a"}},
				// orphan: no export depends on its label.
				{CMakeTarget: "Pkg::orphan", BazelLabel: "//p:orphan", LinkPaths: []string{"/opt/prefix/lib/liborphan.a"}},
			},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if r.ArchiveIsOrphan("//p:pdep") {
		t.Errorf("//p:pdep is depended on by //p:p; must not be an orphan")
	}
	if !r.ArchiveIsOrphan("//p:orphan") {
		t.Errorf("//p:orphan is depended on by no export; must be an orphan")
	}
	// A label the manifest doesn't know at all is trivially depended on by
	// nobody → orphan; and the empty label / nil resolver are non-orphan.
	if !r.ArchiveIsOrphan("//p:unknown") {
		t.Errorf("unknown label must be an orphan (depended on by no export)")
	}
	if r.ArchiveIsOrphan("") {
		t.Errorf("empty label must not be an orphan")
	}
	var nilR *manifest.Resolver
	if nilR.ArchiveIsOrphan("//p:p") {
		t.Errorf("nil resolver must report non-orphan")
	}
}
