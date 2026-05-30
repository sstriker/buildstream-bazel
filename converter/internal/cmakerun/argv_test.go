package cmakerun

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestBuildCmakeArgv_BaselineOrder(t *testing.T) {
	got, err := buildCmakeArgv(Options{
		SourceRoot: "/src",
		BuildDir:   "/build",
		BuildType:  "Release",
	}, "", "", "", "")
	if err != nil {
		t.Fatalf("buildCmakeArgv: %v", err)
	}
	want := []string{
		"-S", "/src",
		"-B", "/build",
		"-G", "Ninja",
		"-DCMAKE_BUILD_TYPE=Release",
		"-DCMAKE_EXPORT_COMPILE_COMMANDS=ON",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("baseline argv mismatch\n got: %q\nwant: %q", got, want)
	}
}

// TestBuildCmakeArgv_ExtraCacheVarsSortedDeterministically locks
// in the lexicographic key order of -D flags from ExtraCacheVars.
// The map's iteration is randomized; the function must impose a
// deterministic order so cmake invocations are byte-stable across
// runs (required for cache hit rates and golden tests).
func TestBuildCmakeArgv_ExtraCacheVarsSortedDeterministically(t *testing.T) {
	opts := Options{
		SourceRoot: "/src",
		BuildDir:   "/build",
		BuildType:  "Debug",
		ExtraCacheVars: map[string]string{
			"CMAKE_C_FLAGS":        "-fsanitize=address -fno-omit-frame-pointer",
			"CMAKE_CXX_COMPILER":   "/usr/bin/clang++-15",
			"CMAKE_C_COMPILER":     "/usr/bin/clang-15",
			"CMAKE_TOOLCHAIN_HOOK": "1",
		},
	}
	// Run twice; argv must match exactly.
	first, err := buildCmakeArgv(opts, "", "", "", "")
	if err != nil {
		t.Fatalf("buildCmakeArgv #1: %v", err)
	}
	second, err := buildCmakeArgv(opts, "", "", "", "")
	if err != nil {
		t.Fatalf("buildCmakeArgv #2: %v", err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("argv not deterministic\n#1: %q\n#2: %q", first, second)
	}

	// Locate the -D flag block (after the dedicated build-type +
	// compile-commands -Ds) and verify ascending key order.
	var dFlags []string
	for _, a := range first {
		if strings.HasPrefix(a, "-D") && !strings.HasPrefix(a, "-DCMAKE_BUILD_TYPE=") && !strings.HasPrefix(a, "-DCMAKE_EXPORT_COMPILE_COMMANDS=") {
			dFlags = append(dFlags, a)
		}
	}
	wantD := []string{
		"-DCMAKE_CXX_COMPILER=/usr/bin/clang++-15",
		"-DCMAKE_C_COMPILER=/usr/bin/clang-15",
		"-DCMAKE_C_FLAGS=-fsanitize=address -fno-omit-frame-pointer",
		"-DCMAKE_TOOLCHAIN_HOOK=1",
	}
	if !reflect.DeepEqual(dFlags, wantD) {
		t.Errorf("ExtraCacheVars -D order mismatch\n got: %q\nwant: %q", dFlags, wantD)
	}
}

// TestBuildCmakeArgv_RejectsBuildTypeInExtras catches the misuse
// where CMAKE_BUILD_TYPE leaks into ExtraCacheVars (it has the
// dedicated BuildType slot). The error keeps cmake's last-wins -D
// semantics from silently picking a winner between the two paths.
func TestBuildCmakeArgv_RejectsBuildTypeInExtras(t *testing.T) {
	_, err := buildCmakeArgv(Options{
		SourceRoot: "/src",
		BuildDir:   "/build",
		BuildType:  "Debug",
		ExtraCacheVars: map[string]string{
			"CMAKE_BUILD_TYPE": "Release",
		},
	}, "", "", "", "")
	if err == nil {
		t.Fatal("expected error; got nil")
	}
	if !strings.Contains(err.Error(), "CMAKE_BUILD_TYPE in ExtraCacheVars") {
		t.Errorf("unexpected error message: %v", err)
	}
}

// TestBuildCmakeArgv_TailOptionsCoexist verifies the optional
// trailing args (DumpVars, TracePath, ToolchainCMakeFile) all
// render correctly together.
func TestBuildCmakeArgv_TailOptionsCoexist(t *testing.T) {
	tmp := t.TempDir()
	tcFile := filepath.Join(tmp, "toolchain.cmake")
	got, err := buildCmakeArgv(Options{
		SourceRoot:         "/src",
		BuildDir:           "/build",
		BuildType:          "Release",
		DumpVars:           true,
		TracePath:          "/tmp/trace.json",
		ToolchainCMakeFile: tcFile,
	}, "/build/dump-vars.cmake", "", "", "")
	if err != nil {
		t.Fatalf("buildCmakeArgv: %v", err)
	}
	tcAbs, _ := filepath.Abs(tcFile)
	wantSubstrs := []string{
		"-DCMAKE_PROJECT_TOP_LEVEL_INCLUDES=/build/dump-vars.cmake",
		"--trace-expand",
		"--trace-format=json-v1",
		"--trace-redirect=/tmp/trace.json",
		"-DCMAKE_TOOLCHAIN_FILE=" + tcAbs,
	}
	for _, want := range wantSubstrs {
		found := false
		for _, a := range got {
			if a == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("missing arg %q in %q", want, got)
		}
	}
}

// TestBuildCmakeArgv_CMP0026ShimAlone verifies the shim slot
// renders as the sole CMAKE_PROJECT_TOP_LEVEL_INCLUDES entry when
// the dump-vars hook isn't also requested. #208.
func TestBuildCmakeArgv_CMP0026ShimAlone(t *testing.T) {
	got, err := buildCmakeArgv(Options{
		SourceRoot:  "/src",
		BuildDir:    "/build",
		BuildType:   "Release",
		CMP0026Shim: true,
	}, "", "/build/cmake-to-bazel.cmp0026-shim.cmake", "", "")
	if err != nil {
		t.Fatalf("buildCmakeArgv: %v", err)
	}
	want := "-DCMAKE_PROJECT_TOP_LEVEL_INCLUDES=/build/cmake-to-bazel.cmp0026-shim.cmake"
	found := false
	for _, a := range got {
		if a == want {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("missing arg %q in %q", want, got)
	}
}

// TestBuildCmakeArgv_CMP0026ShimComposesWithDumpVars verifies the
// two hooks layer onto the same CMAKE_PROJECT_TOP_LEVEL_INCLUDES
// slot via the `;`-joined list cmake honors there, with the shim
// first so its wrapper is installed before dump-vars enumerates
// the namespace. #208.
func TestBuildCmakeArgv_CMP0026ShimComposesWithDumpVars(t *testing.T) {
	got, err := buildCmakeArgv(Options{
		SourceRoot:  "/src",
		BuildDir:    "/build",
		BuildType:   "Release",
		DumpVars:    true,
		CMP0026Shim: true,
	}, "/build/dump-vars.cmake", "/build/cmp0026-shim.cmake", "", "")
	if err != nil {
		t.Fatalf("buildCmakeArgv: %v", err)
	}
	want := "-DCMAKE_PROJECT_TOP_LEVEL_INCLUDES=/build/cmp0026-shim.cmake;/build/dump-vars.cmake"
	found := false
	for _, a := range got {
		if a == want {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("missing arg %q in %q", want, got)
	}
}

// TestBuildCmakeArgv_MultiConfig verifies BuildTypes switches the
// generator to "Ninja Multi-Config" with the entries joined into
// -DCMAKE_CONFIGURATION_TYPES, in declared order.
func TestBuildCmakeArgv_MultiConfig(t *testing.T) {
	got, err := buildCmakeArgv(Options{
		SourceRoot: "/src",
		BuildDir:   "/build",
		BuildTypes: []string{"Release", "Debug", "RelWithDebInfo"},
	}, "", "", "", "")
	if err != nil {
		t.Fatalf("buildCmakeArgv: %v", err)
	}
	wantHead := []string{
		"-S", "/src",
		"-B", "/build",
		"-G", "Ninja Multi-Config",
		"-DCMAKE_CONFIGURATION_TYPES=Release;Debug;RelWithDebInfo",
		"-DCMAKE_EXPORT_COMPILE_COMMANDS=ON",
	}
	if !reflect.DeepEqual(got[:len(wantHead)], wantHead) {
		t.Errorf("argv head mismatch\n got: %q\nwant: %q", got[:len(wantHead)], wantHead)
	}
	// BuildType isn't passed when BuildTypes is set.
	for _, a := range got {
		if strings.HasPrefix(a, "-DCMAKE_BUILD_TYPE=") {
			t.Errorf("unexpected -DCMAKE_BUILD_TYPE in multi-config argv: %v", got)
		}
	}
}

// TestBuildCmakeArgv_MultiConfigCustomTypes covers the sanitizer-
// variant naming pattern: cmake doesn't care that names aren't from
// the standard four, and the codemodel-v2 reply carries one
// Configuration entry per name.
func TestBuildCmakeArgv_MultiConfigCustomTypes(t *testing.T) {
	got, err := buildCmakeArgv(Options{
		SourceRoot: "/src",
		BuildDir:   "/build",
		BuildTypes: []string{"Release", "ASan", "TSan", "UBSan"},
	}, "", "", "", "")
	if err != nil {
		t.Fatalf("buildCmakeArgv: %v", err)
	}
	wantArg := "-DCMAKE_CONFIGURATION_TYPES=Release;ASan;TSan;UBSan"
	found := false
	for _, a := range got {
		if a == wantArg {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("missing %q in argv: %q", wantArg, got)
	}
}

// TestBuildCmakeArgv_MultiConfigRejectsEmptyEntry catches a stray
// empty-string config name (would emit CMAKE_CONFIGURATION_TYPES=";Release;…"
// which cmake interprets as a default-config entry alongside the real
// names — silent footgun).
func TestBuildCmakeArgv_MultiConfigRejectsEmptyEntry(t *testing.T) {
	_, err := buildCmakeArgv(Options{
		SourceRoot: "/src",
		BuildDir:   "/build",
		BuildTypes: []string{"Release", ""},
	}, "", "", "", "")
	if err == nil {
		t.Fatal("expected error; got nil")
	}
	if !strings.Contains(err.Error(), "BuildTypes[1] is empty") {
		t.Errorf("unexpected error: %v", err)
	}
}

// TestBuildCmakeArgv_MultiConfigRejectsDuplicate prevents the codemodel
// from ending up with two same-named Configuration entries — cmake
// would deduplicate silently, but the duplication is a real bug in
// the caller worth surfacing.
func TestBuildCmakeArgv_MultiConfigRejectsDuplicate(t *testing.T) {
	_, err := buildCmakeArgv(Options{
		SourceRoot: "/src",
		BuildDir:   "/build",
		BuildTypes: []string{"Release", "Debug", "Release"},
	}, "", "", "", "")
	if err == nil {
		t.Fatal("expected error; got nil")
	}
	if !strings.Contains(err.Error(), "duplicate") {
		t.Errorf("unexpected error: %v", err)
	}
}

// TestBuildCmakeArgv_RejectsConfigTypesInExtras catches the misuse
// where CMAKE_CONFIGURATION_TYPES leaks into ExtraCacheVars. Same
// rationale as the CMAKE_BUILD_TYPE-in-extras rejection: the
// dedicated BuildTypes slot must win deterministically.
func TestBuildCmakeArgv_RejectsConfigTypesInExtras(t *testing.T) {
	_, err := buildCmakeArgv(Options{
		SourceRoot: "/src",
		BuildDir:   "/build",
		BuildTypes: []string{"Release"},
		ExtraCacheVars: map[string]string{
			"CMAKE_CONFIGURATION_TYPES": "Release;Debug",
		},
	}, "", "", "", "")
	if err == nil {
		t.Fatal("expected error; got nil")
	}
	if !strings.Contains(err.Error(), "CMAKE_CONFIGURATION_TYPES in ExtraCacheVars") {
		t.Errorf("unexpected error: %v", err)
	}
}

// TestBuildCmakeArgv_RejectsBothBuildTypeAndBuildTypes guards the
// mutex: callers must pick exactly one of the two options.
func TestBuildCmakeArgv_RejectsBothBuildTypeAndBuildTypes(t *testing.T) {
	_, err := buildCmakeArgv(Options{
		SourceRoot: "/src",
		BuildDir:   "/build",
		BuildType:  "Release",
		BuildTypes: []string{"Debug"},
	}, "", "", "", "")
	if err == nil {
		t.Fatal("expected error; got nil")
	}
	if !strings.Contains(err.Error(), "mutually exclusive") {
		t.Errorf("unexpected error: %v", err)
	}
}

// TestBuildCmakeArgv_ProbeGenexAlone verifies the genex-probe path
// renders as the sole CMAKE_PROJECT_TOP_LEVEL_INCLUDES entry when
// neither dump-vars nor cmp0026 shim is also requested.
func TestBuildCmakeArgv_ProbeGenexAlone(t *testing.T) {
	got, err := buildCmakeArgv(Options{
		SourceRoot: "/src",
		BuildDir:   "/build",
		BuildType:  "Release",
		ProbeGenex: true,
	}, "", "", "/build/cmake-to-bazel.probe-genex.cmake", "")
	if err != nil {
		t.Fatalf("buildCmakeArgv: %v", err)
	}
	want := "-DCMAKE_PROJECT_TOP_LEVEL_INCLUDES=/build/cmake-to-bazel.probe-genex.cmake"
	found := false
	for _, a := range got {
		if a == want {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("missing arg %q in %q", want, got)
	}
}

// TestBuildCmakeArgv_AllThreeHooksOrder verifies the three TOP_LEVEL
// hooks layer in the documented order: cmp0026 shim first, then
// dump-vars, then probe-genex.
func TestBuildCmakeArgv_AllThreeHooksOrder(t *testing.T) {
	got, err := buildCmakeArgv(Options{
		SourceRoot:  "/src",
		BuildDir:    "/build",
		BuildType:   "Release",
		DumpVars:    true,
		CMP0026Shim: true,
		ProbeGenex:  true,
	}, "/build/dump-vars.cmake", "/build/cmp0026-shim.cmake", "/build/probe-genex.cmake", "")
	if err != nil {
		t.Fatalf("buildCmakeArgv: %v", err)
	}
	want := "-DCMAKE_PROJECT_TOP_LEVEL_INCLUDES=/build/cmp0026-shim.cmake;/build/dump-vars.cmake;/build/probe-genex.cmake"
	found := false
	for _, a := range got {
		if a == want {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("missing arg %q in %q", want, got)
	}
}
