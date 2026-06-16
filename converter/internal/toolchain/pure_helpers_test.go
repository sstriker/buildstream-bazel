package toolchain

import (
	"reflect"
	"testing"
)

// Unit coverage for the pure Variant helpers (sanitize/sort/render) and the
// generated-feature catalog accessor — previously 0% (exercised only via the
// subprocess Probe path, which needs a real cmake).

func TestSanitizeVariantName(t *testing.T) {
	cases := map[string]string{
		"":         "baseline", // empty -> the baseline sentinel
		"asan":     "asan",
		"ASan":     "asan",     // uppercase folds to lowercase
		"clang-15": "clang_15", // non-alnum (hyphen) -> _
		"a.b/c":    "a_b_c",
		"x86_64":   "x86_64", // digits + underscore kept
	}
	for in, want := range cases {
		if got := sanitizeVariantName(in); got != want {
			t.Errorf("sanitizeVariantName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSortedCacheVarKeys(t *testing.T) {
	v := Variant{CacheVars: map[string]string{"B": "2", "A": "1", "C": "3"}}
	if got, want := SortedCacheVarKeys(v), []string{"A", "B", "C"}; !reflect.DeepEqual(got, want) {
		t.Errorf("SortedCacheVarKeys = %v, want %v", got, want)
	}
	if got := SortedCacheVarKeys(Variant{}); len(got) != 0 {
		t.Errorf("empty CacheVars: got %v, want empty", got)
	}
}

func TestVariantString(t *testing.T) {
	if got, want := VariantString(Variant{Name: "baseline"}), "baseline{}"; got != want {
		t.Errorf("no CacheVars: got %q, want %q", got, want)
	}
	v := Variant{Name: "asan", CacheVars: map[string]string{"CMAKE_C_FLAGS": "-fsanitize=address", "CMAKE_BUILD_TYPE": "Debug"}}
	// Keys are sorted for a stable rendering.
	if got, want := VariantString(v), "asan{CMAKE_BUILD_TYPE=Debug,CMAKE_C_FLAGS=-fsanitize=address}"; got != want {
		t.Errorf("VariantString = %q, want %q", got, want)
	}
}

func TestGeneratedFeatures(t *testing.T) {
	got := GeneratedFeatures()
	if !reflect.DeepEqual(got, generatedFeatures) {
		t.Errorf("GeneratedFeatures() = %v, want %v", got, generatedFeatures)
	}
	// Must be a COPY — mutating the result must not corrupt the package catalog.
	if len(got) > 0 {
		got[0] = "tampered"
		if generatedFeatures[0] == "tampered" {
			t.Error("GeneratedFeatures() aliased the package-level catalog")
		}
	}
}
