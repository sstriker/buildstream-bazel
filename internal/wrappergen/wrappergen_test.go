package wrappergen

import (
	"strings"
	"testing"

	"github.com/sstriker/buildstream-bazel/internal/manifest"
)

func sampleManifest() *manifest.Imports {
	return &manifest.Imports{
		Version: 1,
		Elements: []*manifest.Element{{
			Name: "greet",
			Exports: []*manifest.Export{
				{
					CMakeTarget:       "Greeter::core",
					BazelLabel:        "//old/prebuilts:core",
					LinkPaths:         []string{"/opt/prefix/lib/libcore.a"},
					InterfaceIncludes: []string{"include"},
					Deps:              []string{"//old/prebuilts:base", "@sys//:z"},
				},
				{
					CMakeTarget: "Greeter::base",
					BazelLabel:  "//old/prebuilts:base",
					LinkPaths:   []string{"/opt/prefix/lib/libbase.a"},
				},
				{
					CMakeTarget:       "Greeter::headers",
					BazelLabel:        "//old/prebuilts:headers",
					InterfaceIncludes: []string{"include"},
				},
			},
		}},
	}
}

// TestGenerate_WrapperShape pins the synthesized BUILD: cc_import per
// archived export (prefix-relative path, kind by extension), the
// consumer-facing cc_library wrapper with the archive + remapped
// closure as REAL deps, header-only exports as glob-driven wrappers
// with no import, and cross-element closure labels passing through.
func TestGenerate_WrapperShape(t *testing.T) {
	build, _, err := Generate(sampleManifest(), "prebuilts/greet", "")
	if err != nil {
		t.Fatal(err)
	}
	s := string(build)
	for _, want := range []string{
		`cc_import(
    name = "core_archive",
    static_library = "lib/libcore.a",`,
		`cc_library(
    name = "core",`,
		`        ":core_archive",
        "//prebuilts/greet:base",
        "@sys//:z",`,
		`cc_library(
    name = "headers",`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("BUILD missing:\n%s\n--- got ---\n%s", want, s)
		}
	}
	if strings.Contains(s, `"headers_archive"`) {
		t.Errorf("header-only export must not get a cc_import:\n%s", s)
	}
	if strings.Contains(s, "//old/prebuilts:base") {
		t.Errorf("closure label naming another export's OLD label must remap to its wrapper:\n%s", s)
	}
}

// TestGenerate_ManifestRewrite pins the consume-and-clear half of the
// Export.Deps invariant: labels repoint at the wrappers, Deps clears,
// the fragment-redirect keys (link_paths) survive.
func TestGenerate_ManifestRewrite(t *testing.T) {
	_, out, err := Generate(sampleManifest(), "prebuilts/greet", "")
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]*manifest.Export{}
	for _, ex := range out.Elements[0].Exports {
		byName[ex.CMakeTarget] = ex
	}
	core := byName["Greeter::core"]
	if core.BazelLabel != "//prebuilts/greet:core" {
		t.Errorf("core label = %q, want the wrapper", core.BazelLabel)
	}
	if len(core.Deps) != 0 {
		t.Errorf("Deps must CLEAR after generation (invariant): %v", core.Deps)
	}
	if len(core.LinkPaths) != 1 || core.LinkPaths[0] != "/opt/prefix/lib/libcore.a" {
		t.Errorf("link_paths must survive (fragment redirects key on them): %v", core.LinkPaths)
	}
	// The input manifest must stay untouched (deep copy).
	in := sampleManifest()
	_, _, _ = Generate(in, "prebuilts/greet", "")
	if in.Elements[0].Exports[0].BazelLabel != "//old/prebuilts:core" || len(in.Elements[0].Exports[0].Deps) != 2 {
		t.Error("Generate mutated its input manifest")
	}
}

// TestGenerate_NameCollision: wrapper names are consumer-facing; a
// post-sanitization collision is an error, not a silent overwrite.
func TestGenerate_NameCollision(t *testing.T) {
	im := &manifest.Imports{
		Version: 1,
		Elements: []*manifest.Element{{
			Name: "x",
			Exports: []*manifest.Export{
				{CMakeTarget: "A::foo+bar", BazelLabel: "//a:1"},
				{CMakeTarget: "B::foo_bar", BazelLabel: "//b:2"},
			},
		}},
	}
	if _, _, err := Generate(im, "p", ""); err == nil {
		t.Error("colliding wrapper names must error")
	}
}

// TestGenerate_ExecutableFilegroup pins the protoc shape: an export
// with Kind=executable gets a filegroup over its prefix-relative bin
// path — never a cc_import (an ELF program is not a static_library)
// or a cc_library wrapper.
func TestGenerate_ExecutableFilegroup(t *testing.T) {
	im := &manifest.Imports{
		Version: 1,
		Elements: []*manifest.Element{{
			Name: "pb",
			Exports: []*manifest.Export{
				{
					CMakeTarget: "protobuf::protoc",
					BazelLabel:  "//old:protoc",
					Kind:        manifest.KindExecutable,
					LinkPaths:   []string{"/opt/prefix/bin/protoc"},
				},
				{
					CMakeTarget: "protobuf::libprotobuf",
					BazelLabel:  "//old:libprotobuf",
					LinkPaths:   []string{"/opt/prefix/lib/libprotobuf.a"},
				},
			},
		}},
	}
	build, rewritten, err := Generate(im, "prebuilts/pb", "")
	if err != nil {
		t.Fatal(err)
	}
	s := string(build)
	want := `filegroup(
    name = "protoc",
    srcs = [
        "bin/protoc",
    ],
)`
	if !strings.Contains(s, want) {
		t.Errorf("executable export must become a filegroup:\n%s\n--- got ---\n%s", want, s)
	}
	if strings.Contains(s, `"protoc_archive"`) || strings.Contains(s, `cc_library(
    name = "protoc"`) {
		t.Errorf("executable export must not get cc_import/cc_library shapes:\n%s", s)
	}
	if !strings.Contains(s, `static_library = "lib/libprotobuf.a"`) {
		t.Errorf("library sibling must keep the cc_import shape:\n%s", s)
	}
	for _, ex := range rewritten.Elements[0].Exports {
		if ex.CMakeTarget == "protobuf::protoc" && ex.Kind != manifest.KindExecutable {
			t.Errorf("rewritten manifest must preserve kind: %+v", ex)
		}
	}
}

// TestGenerate_DepCycleError: Deps that close a cycle among the
// selected exports are refused up front — Bazel would reject the
// generated package at load time, far from the manifest authoring
// mistake.
func TestGenerate_DepCycleError(t *testing.T) {
	im := &manifest.Imports{
		Version: 1,
		Elements: []*manifest.Element{{
			Name: "x",
			Exports: []*manifest.Export{
				{CMakeTarget: "X::a", BazelLabel: "//old:a", Deps: []string{"//old:b"}},
				{CMakeTarget: "X::b", BazelLabel: "//old:b", Deps: []string{"//old:a"}},
			},
		}},
	}
	_, _, err := Generate(im, "p", "")
	if err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Errorf("cyclic Deps must error with the cycle named, got: %v", err)
	}
}

// TestGenerate_HeaderGlobSurface: the hdrs glob covers the full
// header-ish family, not just *.h — with allow_empty a too-narrow
// pattern fails SILENTLY (abseil ships .inc; C++ prefixes .hpp etc.).
func TestGenerate_HeaderGlobSurface(t *testing.T) {
	build, _, err := Generate(sampleManifest(), "prebuilts/greet", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"include/**/*.h"`, `"include/**/*.inc"`, `"include/**/*.hpp"`, `"include/**/*.def"`} {
		if !strings.Contains(string(build), want) {
			t.Errorf("hdrs glob missing %s:\n%s", want, build)
		}
	}
}

// TestGenerate_LinkLibrariesToLinkopts pins that a wrapper carries the
// harvested system-lib fragments (Export.LinkLibraries — Threads::Threads →
// pthread, ${CMAKE_DL_LIBS} → dl, a bare m → -lm) as cc_library linkopts. Without
// them a consumer that pulls the wrapper still fails at the FINAL link on those
// leaf system libs even though the target-label deps resolved. Bare names get a
// -l prefix; an already-flag fragment passes through; SOURCE ORDER is preserved
// (not sorted) since linker argv order is semantically significant.
func TestGenerate_LinkLibrariesToLinkopts(t *testing.T) {
	im := &manifest.Imports{
		Version: 1,
		Elements: []*manifest.Element{{
			Name: "app",
			Exports: []*manifest.Export{{
				CMakeTarget: "App::app",
				BazelLabel:  "//old:app",
				LinkPaths:   []string{"/opt/prefix/lib/libapp.a"},
				// Deliberately NOT alphabetical: -Wl,--as-needed must precede the
				// libs it gates, and pthread must stay before m, so the emitted
				// order has to mirror this input, not sort it.
				LinkLibraries: []string{"-Wl,--as-needed", "pthread", "m"},
			}},
		}},
	}
	build, _, err := Generate(im, "prebuilts/app", "")
	if err != nil {
		t.Fatal(err)
	}
	s := string(build)
	want := "    linkopts = [\n        \"-Wl,--as-needed\",\n        \"-lpthread\",\n        \"-lm\",\n    ],\n"
	if !strings.Contains(s, want) {
		t.Errorf("wrapper linkopts must be emitted in SOURCE order (bare names -l-prefixed); want block:\n%s\n--- got ---\n%s", want, s)
	}
}

// TestGenerate_LinkLibrariesSelfNameNotRelinked pins the self case: an
// export whose LinkLibraries carries its OWN archive-provided name (libz.a →
// "z", kept for the consumer LookupLinkLibrary redirect) alongside a genuine
// leaf system lib ("m"). The cc_import already provides "z", so it must NOT be
// re-emitted as -lz; only -lm survives as a linkopt.
func TestGenerate_LinkLibrariesSelfNameNotRelinked(t *testing.T) {
	im := &manifest.Imports{
		Version: 1,
		Elements: []*manifest.Element{{
			Name: "zlib",
			Exports: []*manifest.Export{{
				CMakeTarget:   "ZLIB::ZLIB",
				BazelLabel:    "//old:zlib",
				LinkPaths:     []string{"/opt/prefix/lib/libz.a"},
				LinkLibraries: []string{"z", "m"},
			}},
		}},
	}
	build, _, err := Generate(im, "prebuilts/zlib", "")
	if err != nil {
		t.Fatal(err)
	}
	s := string(build)
	if !strings.Contains(s, "\"-lm\"") {
		t.Errorf("genuine leaf -lm must survive as a linkopt:\n%s", s)
	}
	if strings.Contains(s, "\"-lz\"") {
		t.Errorf("the export's own archive name must NOT be re-linked as -lz (cc_import provides it):\n%s", s)
	}
	if !strings.Contains(s, "cc_import(") {
		t.Errorf("expected a cc_import over libz.a:\n%s", s)
	}
}

// TestGenerate_LinkLibrariesSiblingArchiveToDep pins kind 1: a LinkLibraries
// name that is ANOTHER export's archive-provided name becomes a dep edge to
// that export's wrapper, never a -l flag (the manifest owns the archive).
func TestGenerate_LinkLibrariesSiblingArchiveToDep(t *testing.T) {
	im := &manifest.Imports{
		Version: 1,
		Elements: []*manifest.Element{{
			Name: "pkg",
			Exports: []*manifest.Export{
				{CMakeTarget: "Pkg::a", BazelLabel: "//old:a", LinkPaths: []string{"/opt/prefix/lib/liba.a"}, LinkLibraries: []string{"b", "m"}},
				{CMakeTarget: "Pkg::b", BazelLabel: "//old:b", LinkPaths: []string{"/opt/prefix/lib/libb.a"}},
			},
		}},
	}
	build, _, err := Generate(im, "prebuilts/pkg", "")
	if err != nil {
		t.Fatal(err)
	}
	s := string(build)
	if !strings.Contains(s, "\"//prebuilts/pkg:b\"") {
		t.Errorf("sibling archive name b must become a dep on its wrapper //prebuilts/pkg:b:\n%s", s)
	}
	if strings.Contains(s, "\"-lb\"") {
		t.Errorf("a manifest-owned archive name must NOT become a -l flag:\n%s", s)
	}
	if !strings.Contains(s, "\"-lm\"") {
		t.Errorf("the genuine leaf -lm must still survive:\n%s", s)
	}
}

// TestGenerate_LinkLibrariesHeaderOnlyToDep pins kind 2: a LinkLibraries name
// that resolves to a HEADER-ONLY export (no link_path) becomes a dep to that
// export's wrapper (for its includes) and emits no -l.
func TestGenerate_LinkLibrariesHeaderOnlyToDep(t *testing.T) {
	im := &manifest.Imports{
		Version: 1,
		Elements: []*manifest.Element{{
			Name: "pkg",
			Exports: []*manifest.Export{
				{CMakeTarget: "Pkg::a", BazelLabel: "//old:a", LinkPaths: []string{"/opt/prefix/lib/liba.a"}, LinkLibraries: []string{"hdr"}},
				// Header-only: no LinkPaths; its wrapper name is "hdr".
				{CMakeTarget: "Pkg::hdr", BazelLabel: "//old:hdr", InterfaceIncludes: []string{"include"}},
			},
		}},
	}
	build, _, err := Generate(im, "prebuilts/pkg", "")
	if err != nil {
		t.Fatal(err)
	}
	s := string(build)
	if !strings.Contains(s, "\"//prebuilts/pkg:hdr\"") {
		t.Errorf("header-only name hdr must become a dep on its wrapper //prebuilts/pkg:hdr:\n%s", s)
	}
	if strings.Contains(s, "\"-lhdr\"") {
		t.Errorf("a header-only export name must NEVER become a -l flag:\n%s", s)
	}
}

// TestGenerate_AlwaysLinkCCImport: an Export flagged AlwaysLink (a cyclic
// static-archive SCC member) gets alwayslink = True on its cc_import — the
// Bazel whole-archive equivalent of cmake's link-line repetition.
func TestGenerate_AlwaysLinkCCImport(t *testing.T) {
	im := &manifest.Imports{
		Version: 1,
		Elements: []*manifest.Element{{
			Name: "pkg",
			Exports: []*manifest.Export{
				{CMakeTarget: "Pkg::cyc", BazelLabel: "//old:cyc", AlwaysLink: true, LinkPaths: []string{"/opt/prefix/lib/libcyc.a"}},
				{CMakeTarget: "Pkg::plain", BazelLabel: "//old:plain", LinkPaths: []string{"/opt/prefix/lib/libplain.a"}},
			},
		}},
	}
	build, _, err := Generate(im, "prebuilts/pkg", "")
	if err != nil {
		t.Fatal(err)
	}
	s := string(build)
	want := `cc_import(
    name = "cyc_archive",
    static_library = "lib/libcyc.a",
    alwayslink = True,`
	if !strings.Contains(s, want) {
		t.Errorf("AlwaysLink export must emit alwayslink on its cc_import:\n%s\n--- got ---\n%s", want, s)
	}
	// The plain sibling must NOT get alwayslink.
	if strings.Contains(s, `static_library = "lib/libplain.a",
    alwayslink = True,`) {
		t.Errorf("plain export must not get alwayslink:\n%s", s)
	}
}
