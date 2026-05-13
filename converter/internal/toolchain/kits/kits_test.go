package kits

import (
	"reflect"
	"strings"
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/internal/toolchain"
)

func TestParse_ProducesOneVariantPerKit(t *testing.T) {
	body := []byte(`[
		{
			"name": "GCC 13",
			"compilers": {"C": "/usr/bin/gcc-13", "CXX": "/usr/bin/g++-13"}
		},
		{
			"name": "Clang 15",
			"compilers": {"C": "/usr/bin/clang-15", "CXX": "/usr/bin/clang++-15"}
		}
	]`)
	got, err := Parse(body)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	want := []toolchain.Variant{
		{
			Name: "gcc-13",
			CacheVars: map[string]string{
				"CMAKE_C_COMPILER":   "/usr/bin/gcc-13",
				"CMAKE_CXX_COMPILER": "/usr/bin/g++-13",
			},
		},
		{
			Name: "clang-15",
			CacheVars: map[string]string{
				"CMAKE_C_COMPILER":   "/usr/bin/clang-15",
				"CMAKE_CXX_COMPILER": "/usr/bin/clang++-15",
			},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

// TestParse_ToolchainFileLiftedToCacheVar covers the cross-compile
// kit case: the kit's toolchainFile field becomes
// CMAKE_TOOLCHAIN_FILE in CacheVars, matching how the rest of the
// pipeline expects the cross-toolchain to be specified.
func TestParse_ToolchainFileLiftedToCacheVar(t *testing.T) {
	body := []byte(`[
		{
			"name": "Cross ARM64",
			"compilers": {"C": "/usr/bin/aarch64-linux-gnu-gcc"},
			"toolchainFile": "/work/toolchains/aarch64.cmake"
		}
	]`)
	got, err := Parse(body)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 kit; got %d", len(got))
	}
	if got[0].CacheVars["CMAKE_TOOLCHAIN_FILE"] != "/work/toolchains/aarch64.cmake" {
		t.Errorf("CMAKE_TOOLCHAIN_FILE not lifted: %v", got[0].CacheVars)
	}
}

// TestParse_CmakeSettingsLiftedToCacheVars: kits' cmakeSettings is
// the kit-level -D pass-through; each entry becomes a CacheVar.
// String, bool, and integer values must all coerce correctly.
func TestParse_CmakeSettingsLiftedToCacheVars(t *testing.T) {
	body := []byte(`[
		{
			"name": "kit-with-settings",
			"cmakeSettings": {
				"STRING_VAR": "hello",
				"BOOL_TRUE": true,
				"BOOL_FALSE": false,
				"INT_VAR": 42
			}
		}
	]`)
	got, err := Parse(body)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	want := map[string]string{
		"STRING_VAR": "hello",
		"BOOL_TRUE":  "ON",
		"BOOL_FALSE": "OFF",
		"INT_VAR":    "42",
	}
	if !reflect.DeepEqual(got[0].CacheVars, want) {
		t.Errorf("got %+v, want %+v", got[0].CacheVars, want)
	}
}

// TestStringify_LargeFloats covers the bounds-checked float→int64
// path. JSON numbers larger than int64 max (or smaller than min)
// must fall through to scientific/decimal float formatting rather
// than wrapping into a garbled integer via implementation-defined
// conversion.
func TestStringify_LargeFloats(t *testing.T) {
	cases := map[string]any{
		"42":     float64(42),
		"-7":     float64(-7),
		"1.5":    1.5,
		"1e+20":  1e20, // outside int64 range; expect float formatting
		"-1e+20": -1e20,
	}
	for want, in := range cases {
		got, err := stringify(in)
		if err != nil {
			t.Errorf("stringify(%v): %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("stringify(%v) = %q; want %q", in, got, want)
		}
	}
}

func TestSanitizeKitName(t *testing.T) {
	cases := map[string]string{
		"GCC 13":            "gcc-13",
		"Clang 15 (x86_64)": "clang-15-x86_64",
		"  spaces around  ": "spaces-around",
		"GCC-13!":           "gcc-13",
		"plain":             "plain",
	}
	for in, want := range cases {
		if got := sanitizeKitName(in); got != want {
			t.Errorf("sanitizeKitName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLoadFile_MissingReturnsNil(t *testing.T) {
	got, err := LoadFile("/no/such/path/cmake-kits.json")
	if err != nil {
		t.Errorf("expected nil error; got %v", err)
	}
	if got != nil {
		t.Errorf("expected nil; got %v", got)
	}
}

func TestParse_EmptyNameRejected(t *testing.T) {
	body := []byte(`[{"name": ""}]`)
	_, err := Parse(body)
	if err == nil {
		t.Fatal("expected error for empty name")
	}
}

// TestParse_DuplicateSanitizedNamesRejected: two distinct kit
// names that sanitize to the same Variant.Name (e.g. "GCC 13"
// and "GCC-13" both → "gcc-13") would collide in
// Observe.Variants and produce duplicate Bazel target names.
// Parse must reject the second kit with an error naming both.
func TestParse_DuplicateSanitizedNamesRejected(t *testing.T) {
	body := []byte(`[
		{"name": "GCC 13", "compilers": {"C": "/usr/bin/gcc-13"}},
		{"name": "GCC-13", "compilers": {"C": "/usr/local/bin/gcc-13"}}
	]`)
	_, err := Parse(body)
	if err == nil {
		t.Fatal("expected duplicate-sanitized-name error")
	}
	for _, want := range []string{"GCC 13", "GCC-13", "gcc-13"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing %q", err, want)
		}
	}
}

// TestParse_AllNonAlphanumericRejected: a kit name that consists
// entirely of separators sanitizes to empty, which is
// non-actionable — the resulting Variant has no usable name.
// Reject with a clear error rather than producing a hidden
// surprise.
func TestParse_AllNonAlphanumericRejected(t *testing.T) {
	body := []byte(`[{"name": "!!!"}]`)
	_, err := Parse(body)
	if err == nil {
		t.Fatal("expected error for kit name that sanitizes to empty")
	}
}
