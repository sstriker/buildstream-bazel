package toolchain

import (
	"reflect"
	"testing"
)

func TestVariantMatrix_NoKitsPassThrough(t *testing.T) {
	// No compiler axis → build variants returned unchanged (Kit empty).
	// This is the pre-kits no-op path.
	variants := []Variant{
		{Name: "baseline"},
		{Name: "release", CacheVars: map[string]string{"CMAKE_BUILD_TYPE": "Release"}},
	}
	got := VariantMatrix(nil, variants)
	if !reflect.DeepEqual(got, variants) {
		t.Fatalf("VariantMatrix(nil, variants) = %v; want pass-through %v", got, variants)
	}
	for _, v := range got {
		if v.Kit != "" {
			t.Errorf("pass-through variant %q got Kit=%q; want empty", v.Name, v.Kit)
		}
	}
}

func TestVariantMatrix_KitsOnly(t *testing.T) {
	kits := []Variant{
		{Name: "gcc-13", CacheVars: map[string]string{"CMAKE_C_COMPILER": "/usr/bin/gcc-13"}},
		{Name: "clang-15", CacheVars: map[string]string{"CMAKE_C_COMPILER": "/usr/bin/clang-15"}},
	}
	got := VariantMatrix(kits, nil)
	if len(got) != 2 {
		t.Fatalf("kits-only matrix len = %d; want 2", len(got))
	}
	for i, v := range got {
		if v.Kit != kits[i].Name || v.Name != kits[i].Name {
			t.Errorf("cell %d = {Name:%q Kit:%q}; want Name/Kit %q", i, v.Name, v.Kit, kits[i].Name)
		}
	}
	// Mutating a returned map must not bleed into the source kit.
	got[0].CacheVars["CMAKE_C_COMPILER"] = "mutated"
	if kits[0].CacheVars["CMAKE_C_COMPILER"] == "mutated" {
		t.Error("VariantMatrix aliased the source kit's CacheVars map")
	}
}

func TestVariantMatrix_CrossProduct(t *testing.T) {
	kits := []Variant{
		{Name: "gcc-13", CacheVars: map[string]string{"CMAKE_C_COMPILER": "/usr/bin/gcc-13", "CMAKE_CXX_COMPILER": "/usr/bin/g++-13"}},
		{Name: "clang-15", CacheVars: map[string]string{"CMAKE_C_COMPILER": "/usr/bin/clang-15"}},
	}
	variants := []Variant{
		{Name: "baseline"},
		{Name: "debug", CacheVars: map[string]string{"CMAKE_BUILD_TYPE": "Debug"}},
	}
	got := VariantMatrix(kits, variants)
	if len(got) != 4 {
		t.Fatalf("cross-product len = %d; want 4 (2 kits x 2 variants)", len(got))
	}

	// Iteration order is kits-outer, variants-inner.
	want := []struct {
		name, kit string
		cache     map[string]string
	}{
		{"gcc-13-baseline", "gcc-13", map[string]string{"CMAKE_C_COMPILER": "/usr/bin/gcc-13", "CMAKE_CXX_COMPILER": "/usr/bin/g++-13"}},
		{"gcc-13-debug", "gcc-13", map[string]string{"CMAKE_C_COMPILER": "/usr/bin/gcc-13", "CMAKE_CXX_COMPILER": "/usr/bin/g++-13", "CMAKE_BUILD_TYPE": "Debug"}},
		{"clang-15-baseline", "clang-15", map[string]string{"CMAKE_C_COMPILER": "/usr/bin/clang-15"}},
		{"clang-15-debug", "clang-15", map[string]string{"CMAKE_C_COMPILER": "/usr/bin/clang-15", "CMAKE_BUILD_TYPE": "Debug"}},
	}
	for i, w := range want {
		if got[i].Name != w.name {
			t.Errorf("cell %d Name = %q; want %q", i, got[i].Name, w.name)
		}
		if got[i].Kit != w.kit {
			t.Errorf("cell %d Kit = %q; want %q", i, got[i].Kit, w.kit)
		}
		if !reflect.DeepEqual(got[i].CacheVars, w.cache) {
			t.Errorf("cell %d CacheVars = %v; want %v", i, got[i].CacheVars, w.cache)
		}
	}
}

func TestVariantMatrix_KitWinsOnCollision(t *testing.T) {
	// A build variant that also pins a compiler must lose to the kit:
	// the kit is the authoritative compiler axis.
	kits := []Variant{
		{Name: "clang-15", CacheVars: map[string]string{"CMAKE_C_COMPILER": "/usr/bin/clang-15"}},
	}
	variants := []Variant{
		{Name: "weird", CacheVars: map[string]string{"CMAKE_C_COMPILER": "/usr/bin/gcc", "CMAKE_BUILD_TYPE": "Release"}},
	}
	got := VariantMatrix(kits, variants)
	if len(got) != 1 {
		t.Fatalf("len = %d; want 1", len(got))
	}
	if got[0].CacheVars["CMAKE_C_COMPILER"] != "/usr/bin/clang-15" {
		t.Errorf("collision: CMAKE_C_COMPILER = %q; want kit's /usr/bin/clang-15", got[0].CacheVars["CMAKE_C_COMPILER"])
	}
	if got[0].CacheVars["CMAKE_BUILD_TYPE"] != "Release" {
		t.Errorf("orthogonal var lost: CMAKE_BUILD_TYPE = %q; want Release", got[0].CacheVars["CMAKE_BUILD_TYPE"])
	}
}
