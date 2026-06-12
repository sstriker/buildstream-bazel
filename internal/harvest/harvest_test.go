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
}

func contains(s, sub string) bool { return len(s) >= len(sub) && (s == sub || len(sub) == 0 || indexOf(s, sub) >= 0) }
func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
