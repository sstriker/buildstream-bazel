package harvest

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/sstriker/buildstream-bazel/internal/manifest"
)

// writePrefix stages a synthetic install tree exercising the real
// export-bundle grammar: namespaced IMPORTED libs with genex-laden
// INTERFACE_LINK_LIBRARIES, per-config locations via set_property
// APPEND + set_target_properties, an IMPORTED executable, an alias, a
// bundle-less .pc lib with Requires, and a stray bin/ tool.
func writePrefix(t *testing.T) string {
	t.Helper()
	prefix := t.TempDir()
	must := func(rel, body string) {
		t.Helper()
		p := filepath.Join(prefix, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	must("lib/cmake/Greet/GreetTargets.cmake", `# CMake generated Targets file
add_library(Greet::base STATIC IMPORTED)
set_target_properties(Greet::base PROPERTIES
  INTERFACE_INCLUDE_DIRECTORIES "${_IMPORT_PREFIX}/include"
)
add_library(Greet::core STATIC IMPORTED)
set_target_properties(Greet::core PROPERTIES
  INTERFACE_INCLUDE_DIRECTORIES "${_IMPORT_PREFIX}/include"
  INTERFACE_LINK_LIBRARIES "Greet::base;$<LINK_ONLY:Threads::Threads>;-lm;$<$<CONFIG:DEBUG>:Greet::dbg>"
)
add_executable(Greet::gen IMPORTED)
add_library(Greet::Greet ALIAS Greet::core)
`)
	must("lib/cmake/Greet/GreetTargets-release.cmake", `set_property(TARGET Greet::base APPEND PROPERTY IMPORTED_CONFIGURATIONS RELEASE)
set_target_properties(Greet::base PROPERTIES
  IMPORTED_LOCATION_RELEASE "${_IMPORT_PREFIX}/lib/libbase.a"
)
set_target_properties(Greet::core PROPERTIES
  IMPORTED_LOCATION_RELEASE "${_IMPORT_PREFIX}/lib/libcore.a"
)
set_target_properties(Greet::gen PROPERTIES
  IMPORTED_LOCATION_RELEASE "${_IMPORT_PREFIX}/bin/gen"
)
`)
	must("lib/pkgconfig/extra.pc", `prefix=/usr
libdir=${prefix}/lib
Name: extra
Description: bundle-less lib
Version: 1.0
Requires: other >= 1.2
Libs: -L${libdir} -lextra
Cflags: -I${prefix}/include
`)
	must("lib/pkgconfig/other.pc", `Name: other
Version: 1.2
Libs: -lother
`)
	must("lib/libbase.a", "!<arch>\n")
	must("lib/libcore.a", "!<arch>\n")
	must("lib/libextra.a", "!<arch>\n")
	must("lib/libother.a", "!<arch>\n")
	must("include/greet.h", "#pragma once\n")
	if err := os.WriteFile(filepath.Join(prefix, "bin", "gen"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		// bin/ created by the gen IMPORTED_LOCATION write above? No — create dir.
		_ = os.MkdirAll(filepath.Join(prefix, "bin"), 0o755)
		if err := os.WriteFile(filepath.Join(prefix, "bin", "gen"), []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(prefix, "bin", "stray-tool"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return prefix
}

func harvested(t *testing.T) (*manifest.Imports, []string, map[string]*manifest.Export) {
	t.Helper()
	im, warns, err := Harvest(writePrefix(t), "greet", "prebuilts/greet")
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]*manifest.Export{}
	for _, ex := range im.Elements[0].Exports {
		byName[ex.CMakeTarget] = ex
	}
	return im, warns, byName
}

// TestHarvest_BundleGraph pins the cmake-bundle half: targets, anchored
// locations, includes, and DIRECT deps (never flattened — the wrapper
// generator's Bazel transitivity owns the closure): core deps on base's
// synthesized label; $<LINK_ONLY:Threads::Threads> unwraps to the
// builtin pthread mapping; -lm folds to link_libraries; the config-arm
// genex warns and drops.
func TestHarvest_BundleGraph(t *testing.T) {
	_, warns, byName := harvested(t)
	core := byName["Greet::core"]
	if core == nil {
		t.Fatal("Greet::core missing")
	}
	if len(core.Deps) != 1 || core.Deps[0] != "//prebuilts/greet:base" {
		t.Errorf("core.Deps = %v, want DIRECT dep on base's synthesized label", core.Deps)
	}
	wantLibs := map[string]bool{"pthread": true, "m": true}
	for _, l := range core.LinkLibraries {
		delete(wantLibs, l)
	}
	if len(wantLibs) != 0 {
		t.Errorf("core.LinkLibraries missing %v (got %v)", wantLibs, core.LinkLibraries)
	}
	if len(core.LinkPaths) != 1 || core.LinkPaths[0] != manifest.PrefixAnchor+"lib/libcore.a" {
		t.Errorf("core.LinkPaths = %v", core.LinkPaths)
	}
	if len(core.InterfaceIncludes) != 1 || core.InterfaceIncludes[0] != "include" {
		t.Errorf("core.InterfaceIncludes = %v", core.InterfaceIncludes)
	}
	genexWarned := false
	for _, w := range warns {
		if contains(w, "$<$<CONFIG:DEBUG>") {
			genexWarned = true
		}
	}
	if !genexWarned {
		t.Errorf("conservative genex drop must WARN; warnings: %v", warns)
	}
}

// TestHarvest_ExecutablesAliasesAndPC: the IMPORTED executable carries
// its anchored bin/ path; the alias row points at the underlying's
// label; the bundle-claimed binary doesn't duplicate as a stray row
// while the genuinely stray one does; the .pc lib rows in with its
// Requires dep and the bundle never loses to it.
func TestHarvest_ExecutablesAliasesAndPC(t *testing.T) {
	_, _, byName := harvested(t)
	gen := byName["Greet::gen"]
	if gen == nil || len(gen.LinkPaths) != 1 || gen.LinkPaths[0] != manifest.PrefixAnchor+"bin/gen" {
		t.Fatalf("Greet::gen = %+v, want anchored bin/gen", gen)
	}
	if gen.Kind != manifest.KindExecutable {
		t.Errorf("add_executable IMPORTED must mark kind=executable (wrapper-gen emits a filegroup, not a cc_library): %+v", gen)
	}
	alias := byName["Greet::Greet"]
	if alias == nil || alias.BazelLabel != "//prebuilts/greet:core" {
		t.Errorf("alias must resolve to the underlying's label: %+v", alias)
	}
	if byName["greet::bin/gen"] != nil {
		t.Errorf("bundle-claimed binary must not duplicate as a stray row")
	}
	stray := byName["greet::bin/stray-tool"]
	if stray == nil || len(stray.LinkPaths) != 1 || stray.LinkPaths[0] != manifest.PrefixAnchor+"bin/stray-tool" {
		t.Errorf("stray binary row = %+v", stray)
	}
	if stray != nil && stray.Kind != manifest.KindExecutable {
		t.Errorf("bare bin/ row must mark kind=executable: %+v", stray)
	}
	if core := byName["Greet::core"]; core != nil && core.Kind != "" {
		t.Errorf("library export must keep the default kind: %+v", core)
	}
	extra := byName["pkgconfig::extra"]
	if extra == nil {
		t.Fatal("pkgconfig::extra missing")
	}
	if len(extra.Deps) != 1 || extra.Deps[0] != "//prebuilts/greet:other" {
		t.Errorf("pc Requires must become a DIRECT dep: %v", extra.Deps)
	}
	if len(extra.LinkPaths) != 1 || extra.LinkPaths[0] != manifest.PrefixAnchor+"lib/libextra.a" {
		t.Errorf("pc -l resolution = %v", extra.LinkPaths)
	}
	// The fixture's extra.pc says prefix=/usr — the BUILD-TIME prefix of
	// a relocated tree. The harvest seed must win (--define-prefix
	// semantics) or ${prefix}-derived includes silently vanish.
	if len(extra.InterfaceIncludes) != 1 || extra.InterfaceIncludes[0] != "include" {
		t.Errorf("relocated-tree pc includes lost (file prefix= must not clobber the seed): %v", extra.InterfaceIncludes)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(sub) == 0 || indexOf(s, sub) >= 0)
}
func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// TestSplitPCRequires_NoSpaceConstraint pins the "name>=1.2" shape:
// the name splits at the operator instead of dropping silently.
func TestSplitPCRequires_NoSpaceConstraint(t *testing.T) {
	got := splitPCRequires("foo>=1.2, bar , baz >= 3")
	want := []string{"foo", "bar", "baz"}
	if len(got) != len(want) {
		t.Fatalf("splitPCRequires = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("splitPCRequires[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestHarvest_ChannelMerge pins the same-library dedup hardening:
// (a) a .pc whose artifact a bundle claims MERGES its channel keys
// (the -l name keeping the LookupLinkLibrary redirect alive, the
// Requires deps resolving via the alias registration) instead of
// dropping them; (b) a header-only .pc matching a bundle target's
// wrapper NAME (no artifact to match by path) merges too; (c) a
// versioned-soname-only lib (libv.so.1.2.3, no plain .so) still
// resolves through the widened probe, so path identity catches it.
func TestHarvest_ChannelMerge(t *testing.T) {
	prefix := t.TempDir()
	must := func(rel, body string) {
		t.Helper()
		p := filepath.Join(prefix, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	must("lib/cmake/Core/CoreTargets.cmake", `add_library(Core::core STATIC IMPORTED)
add_library(Core::iface INTERFACE IMPORTED)
add_library(Core::v SHARED IMPORTED)
set_target_properties(Core::core PROPERTIES
  IMPORTED_LOCATION "${_IMPORT_PREFIX}/lib/libcore.a"
)
set_target_properties(Core::v PROPERTIES
  IMPORTED_LOCATION "${_IMPORT_PREFIX}/lib/libv.so.1.2.3"
)
`)
	// (a) path-identity merge: core.pc resolves the bundle's archive.
	must("lib/pkgconfig/core.pc", `Name: core
Version: 1
Requires: dep
Libs: -lcore
`)
	must("lib/pkgconfig/dep.pc", `Name: dep
Version: 1
Libs: -ldep
`)
	// (b) name-identity merge: iface.pc is header-only (no Libs), the
	// bundle's Core::iface carries no artifact either.
	must("lib/pkgconfig/iface.pc", `Name: iface
Version: 1
Cflags: -I${prefix}/include
`)
	// (c) versioned soname only — no plain .so.
	must("lib/pkgconfig/v.pc", `Name: v
Version: 1
Libs: -lv
`)
	must("lib/libcore.a", "!<arch>\n")
	must("lib/libdep.a", "!<arch>\n")
	must("lib/libv.so.1.2.3", "\x7fELF\n")
	must("include/core.h", "#pragma once\n")

	im, warns, err := Harvest(prefix, "core", "prebuilts/core")
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]*manifest.Export{}
	for _, ex := range im.Elements[0].Exports {
		byName[ex.CMakeTarget] = ex
	}
	// (a): one row for core, union of channels.
	if byName["pkgconfig::core"] != nil {
		t.Errorf("pc core must merge into the bundle row, not duplicate")
	}
	core := byName["Core::core"]
	if core == nil {
		t.Fatal("Core::core missing")
	}
	if !sliceContains(core.LinkLibraries, "core") {
		t.Errorf("merged -l key lost (LookupLinkLibrary redirect would go dark): %v", core.LinkLibraries)
	}
	if !sliceContains(core.Deps, "//prebuilts/core:dep") {
		t.Errorf("merged pc Requires dep lost: %v", core.Deps)
	}
	// (b): header-only name merge.
	if byName["pkgconfig::iface"] != nil {
		t.Errorf("header-only pc with a name-matching bundle target must merge")
	}
	if iface := byName["Core::iface"]; iface == nil || !sliceContains(iface.InterfaceIncludes, "include") {
		t.Errorf("merged header-only includes lost: %+v", byName["Core::iface"])
	}
	// (c): versioned soname resolved → path identity → merged.
	if byName["pkgconfig::v"] != nil {
		t.Errorf("versioned-soname pc must dedup via the widened probe")
	}
	mergeWarns := 0
	for _, w := range warns {
		if indexOf(w, "channels merged") >= 0 {
			mergeWarns++
		}
	}
	if mergeWarns != 3 {
		t.Errorf("expected 3 channel-merge notices, got %d: %v", mergeWarns, warns)
	}
}

// TestHarvest_SymlinkSonameIdentity: the bundle's IMPORTED_LOCATION
// carries the realpath (libs.so.1.2.3) while the .pc probe finds the
// dev symlink (libs.so → libs.so.1.2.3). Without canonical byPath
// keys the same file gets two anchored spellings — path identity
// misses, both rows "carry artifacts", and the name guard reads the
// true duplicate as a genuine collision. canonicalKey folds the
// spellings so the channels merge; the manifest still records the
// OBSERVED spellings (consumer lookups match trace spellings
// verbatim).
func TestHarvest_SymlinkSonameIdentity(t *testing.T) {
	prefix := t.TempDir()
	must := func(rel, body string) {
		t.Helper()
		p := filepath.Join(prefix, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	must("lib/cmake/S/STargets.cmake", `add_library(S::s SHARED IMPORTED)
set_target_properties(S::s PROPERTIES
  IMPORTED_LOCATION "${_IMPORT_PREFIX}/lib/libs.so.1.2.3"
)
`)
	must("lib/pkgconfig/s.pc", "Name: s\nVersion: 1\nLibs: -ls\n")
	must("lib/libs.so.1.2.3", "\x7fELF\n")
	if err := os.Symlink("libs.so.1.2.3", filepath.Join(prefix, "lib", "libs.so")); err != nil {
		t.Fatal(err)
	}
	im, warns, err := Harvest(prefix, "s", "p")
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]*manifest.Export{}
	for _, ex := range im.Elements[0].Exports {
		byName[ex.CMakeTarget] = ex
	}
	if byName["pkgconfig::s"] != nil {
		t.Errorf("symlink-spelled pc artifact must merge into the bundle row, not duplicate")
	}
	s := byName["S::s"]
	if s == nil {
		t.Fatal("S::s missing")
	}
	if !sliceContains(s.LinkPaths, manifest.PrefixAnchor+"lib/libs.so.1.2.3") ||
		!sliceContains(s.LinkPaths, manifest.PrefixAnchor+"lib/libs.so") {
		t.Errorf("merged row must keep BOTH observed spellings for consumer lookups: %v", s.LinkPaths)
	}
	for _, w := range warns {
		if indexOf(w, "collides") >= 0 {
			t.Errorf("true duplicate misread as a genuine collision: %v", w)
		}
	}
}

// TestHarvest_CycleBreak: mutual .pc Requires (a real shape — circular
// C libraries reference each other) must NOT survive into Deps: Bazel
// rejects cyclic deps once wrappergen materializes them. One edge of
// the cycle is dropped deterministically with a warning naming the
// cycle path; the other survives.
func TestHarvest_CycleBreak(t *testing.T) {
	prefix := t.TempDir()
	must := func(rel, body string) {
		t.Helper()
		p := filepath.Join(prefix, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	must("lib/pkgconfig/alpha.pc", "Name: alpha\nVersion: 1\nRequires: beta\nLibs: -lalpha\n")
	must("lib/pkgconfig/beta.pc", "Name: beta\nVersion: 1\nRequires: alpha\nLibs: -lbeta\n")
	must("lib/libalpha.a", "!<arch>\n")
	must("lib/libbeta.a", "!<arch>\n")
	im, warns, err := Harvest(prefix, "c", "p")
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]*manifest.Export{}
	for _, ex := range im.Elements[0].Exports {
		byName[ex.CMakeTarget] = ex
	}
	a, b := byName["pkgconfig::alpha"], byName["pkgconfig::beta"]
	if a == nil || b == nil {
		t.Fatal("rows missing")
	}
	aDepsB, bDepsA := sliceContains(a.Deps, "//p:beta"), sliceContains(b.Deps, "//p:alpha")
	if aDepsB && bDepsA {
		t.Errorf("cycle survived into Deps (Bazel will reject): alpha.Deps=%v beta.Deps=%v", a.Deps, b.Deps)
	}
	if !aDepsB && !bDepsA {
		t.Errorf("cycle break must drop ONE edge, not both: alpha.Deps=%v beta.Deps=%v", a.Deps, b.Deps)
	}
	warned := false
	for _, w := range warns {
		if indexOf(w, "dependency cycle") >= 0 {
			warned = true
		}
	}
	if !warned {
		t.Errorf("cycle break must warn with the cycle path; warnings: %v", warns)
	}
}

// TestHarvest_GenuineCollisionWarns: two DISTINCT targets (both with
// artifacts) whose wrapper names collide post-sanitization stay
// separate rows and surface a provenance-carrying warning — the early,
// rich form of the generator's late name-collision error.
func TestHarvest_GenuineCollisionWarns(t *testing.T) {
	prefix := t.TempDir()
	p := filepath.Join(prefix, "lib/cmake/X/XTargets.cmake")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(`add_library(A::util STATIC IMPORTED)
add_library(B::util STATIC IMPORTED)
set_target_properties(A::util PROPERTIES
  IMPORTED_LOCATION "${_IMPORT_PREFIX}/lib/liba.a"
)
set_target_properties(B::util PROPERTIES
  IMPORTED_LOCATION "${_IMPORT_PREFIX}/lib/libb.a"
)
`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, warns, err := Harvest(prefix, "x", "p")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, w := range warns {
		if indexOf(w, `wrapper name "util" collides`) >= 0 && indexOf(w, "A::util") >= 0 && indexOf(w, "B::util") >= 0 {
			found = true
		}
	}
	if !found {
		t.Errorf("expected the provenance-carrying collision warning; got %v", warns)
	}
}

func sliceContains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}
