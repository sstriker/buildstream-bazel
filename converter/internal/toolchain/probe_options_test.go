package toolchain

import (
	"reflect"
	"testing"
)

// TestCmakeOptionsFor_BuildTypeRoutedToDedicatedSlot covers the
// CMAKE_BUILD_TYPE → cmakerun.Options.BuildType routing. Putting
// it in ExtraCacheVars instead would error from buildCmakeArgv;
// the helper must lift it out before forwarding.
func TestCmakeOptionsFor_BuildTypeRoutedToDedicatedSlot(t *testing.T) {
	v := Variant{
		Name: "debug",
		CacheVars: map[string]string{
			"CMAKE_BUILD_TYPE": "Debug",
		},
	}
	got := cmakeOptionsFor(v, "/src", "/build/debug")
	if got.BuildType != "Debug" {
		t.Errorf("BuildType = %q; want %q", got.BuildType, "Debug")
	}
	if _, has := got.ExtraCacheVars["CMAKE_BUILD_TYPE"]; has {
		t.Errorf("ExtraCacheVars must not include CMAKE_BUILD_TYPE; got %v", got.ExtraCacheVars)
	}
	if got.SourceRoot != "/src" || got.BuildDir != "/build/debug" {
		t.Errorf("source/build paths not forwarded: %+v", got)
	}
}

// TestCmakeOptionsFor_NonBuildTypeCacheVarsForwarded asserts the
// sanitizer-flavored CacheVars (CMAKE_C_FLAGS, etc.) flow through
// to ExtraCacheVars unchanged. This is the Stage-1 mechanism that
// lets Stage 2's sanitizer Variants reach cmake.
func TestCmakeOptionsFor_NonBuildTypeCacheVarsForwarded(t *testing.T) {
	v := Variant{
		Name: "asan",
		CacheVars: map[string]string{
			"CMAKE_BUILD_TYPE": "Debug",
			"CMAKE_C_FLAGS":    "-fsanitize=address -fno-omit-frame-pointer",
			"CMAKE_CXX_FLAGS":  "-fsanitize=address",
			"CMAKE_C_COMPILER": "/usr/bin/clang-15",
		},
	}
	got := cmakeOptionsFor(v, "/src", "/build/asan")
	if got.BuildType != "Debug" {
		t.Errorf("BuildType = %q; want Debug", got.BuildType)
	}
	want := map[string]string{
		"CMAKE_C_FLAGS":    "-fsanitize=address -fno-omit-frame-pointer",
		"CMAKE_CXX_FLAGS":  "-fsanitize=address",
		"CMAKE_C_COMPILER": "/usr/bin/clang-15",
	}
	if !reflect.DeepEqual(got.ExtraCacheVars, want) {
		t.Errorf("ExtraCacheVars mismatch\n got: %v\nwant: %v", got.ExtraCacheVars, want)
	}
}

// TestCmakeOptionsFor_BaselineNoCacheVars covers the empty-CacheVars
// case (the canonical "baseline" variant). ExtraCacheVars must stay
// nil, not an empty map — buildCmakeArgv's len() check is fine
// either way, but a nil map is the documented baseline.
func TestCmakeOptionsFor_BaselineNoCacheVars(t *testing.T) {
	got := cmakeOptionsFor(Variant{Name: "baseline"}, "/src", "/build/baseline")
	if got.ExtraCacheVars != nil {
		t.Errorf("ExtraCacheVars = %v; want nil", got.ExtraCacheVars)
	}
	if got.BuildType != "" {
		t.Errorf("BuildType = %q; want empty (defaults to Release in cmakerun)", got.BuildType)
	}
}
