package toolchain

import "testing"

// TestDefaultVariantMapping_BuildTypes preserves the existing
// CMAKE_BUILD_TYPE → dbg/opt behaviour. Stage 2 layers sanitizer
// classification on top; the build-type fallback must keep working
// for cells that don't carry sanitizer flags.
func TestDefaultVariantMapping_BuildTypes(t *testing.T) {
	cases := []struct {
		buildType string
		want      BazelFeature
	}{
		{"Debug", BazelFeatureDbg},
		{"DEBUG", BazelFeatureDbg},
		{"Release", BazelFeatureOpt},
		{"RelWithDebInfo", BazelFeatureOpt},
		{"MinSizeRel", BazelFeatureOpt},
		{"Custom", BazelFeatureNone},
		{"", BazelFeatureNone},
	}
	for _, tc := range cases {
		t.Run(tc.buildType, func(t *testing.T) {
			v := Variant{Name: tc.buildType, CacheVars: map[string]string{}}
			if tc.buildType != "" {
				v.CacheVars["CMAKE_BUILD_TYPE"] = tc.buildType
			}
			if got := DefaultVariantMapping(v); got != tc.want {
				t.Errorf("DefaultVariantMapping(BuildType=%q) = %q, want %q", tc.buildType, got, tc.want)
			}
		})
	}
}

// TestDefaultVariantMapping_Sanitizers locks in the sanitizer-flag
// → BazelFeature classification. A sanitizer-flagged cell with
// CMAKE_BUILD_TYPE=Debug must classify as the sanitizer, not as
// "dbg" — the sanitizer signal beats the build-type signal.
func TestDefaultVariantMapping_Sanitizers(t *testing.T) {
	cases := []struct {
		name  string
		cFlag string
		want  BazelFeature
	}{
		{"asan", "-fsanitize=address -fno-omit-frame-pointer", BazelFeatureAsan},
		{"tsan", "-fsanitize=thread", BazelFeatureTsan},
		{"msan", "-fsanitize=memory -fno-omit-frame-pointer", BazelFeatureMsan},
		{"ubsan", "-fsanitize=undefined", BazelFeatureUbsan},
		{"coverage-gcc", "--coverage", BazelFeatureCoverage},
		{"coverage-clang-instr", "-fprofile-instr-generate -fcoverage-mapping", BazelFeatureCoverage},
		{"coverage-arcs", "-fprofile-arcs -ftest-coverage", BazelFeatureCoverage},
		{"lto", "-flto", BazelFeatureLto},
		{"asan-beats-dbg", "-fsanitize=address", BazelFeatureAsan},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := Variant{
				Name: tc.name,
				CacheVars: map[string]string{
					"CMAKE_BUILD_TYPE": "Debug", // sanitizer must override
					"CMAKE_C_FLAGS":    tc.cFlag,
				},
			}
			if got := DefaultVariantMapping(v); got != tc.want {
				t.Errorf("DefaultVariantMapping(C_FLAGS=%q) = %q, want %q", tc.cFlag, got, tc.want)
			}
		})
	}
}

// TestDefaultVariantMapping_FallsBackToCXXFlags covers C++-only
// projects whose sanitizer flags land in CMAKE_CXX_FLAGS but not
// CMAKE_C_FLAGS. The classifier should still find them.
func TestDefaultVariantMapping_FallsBackToCXXFlags(t *testing.T) {
	v := Variant{
		Name: "asan",
		CacheVars: map[string]string{
			"CMAKE_BUILD_TYPE": "Debug",
			"CMAKE_CXX_FLAGS":  "-fsanitize=address",
		},
	}
	if got := DefaultVariantMapping(v); got != BazelFeatureAsan {
		t.Errorf("DefaultVariantMapping = %q, want %q", got, BazelFeatureAsan)
	}
}

// TestFeatureVariants_AllRouteCorrectly is a contract test
// between the FeatureVariants catalog and DefaultVariantMapping:
// each catalog entry must classify to its named feature. If
// somebody adds a sanitizer Variant whose flags don't match the
// classifier, this test catches it.
func TestFeatureVariants_AllRouteCorrectly(t *testing.T) {
	for _, v := range FeatureVariants {
		t.Run(v.Name, func(t *testing.T) {
			got := DefaultVariantMapping(v)
			want := BazelFeature(v.Name)
			if got != want {
				t.Errorf("Variant %q classifies as %q; want %q (catalog/classifier mismatch)", v.Name, got, want)
			}
		})
	}
}

// TestFeatureVariants_CoversAllFeatures asserts the catalog
// covers every non-built-in BazelFeature (sanitizers + coverage
// + lto). Adding a new feature without a catalog entry would
// skip it from probe-time discovery.
func TestFeatureVariants_CoversAllFeatures(t *testing.T) {
	want := map[BazelFeature]bool{
		BazelFeatureAsan:     false,
		BazelFeatureTsan:     false,
		BazelFeatureMsan:     false,
		BazelFeatureUbsan:    false,
		BazelFeatureCoverage: false,
		BazelFeatureLto:      false,
	}
	for _, v := range FeatureVariants {
		want[BazelFeature(v.Name)] = true
	}
	for f, found := range want {
		if !found {
			t.Errorf("FeatureVariants is missing an entry for %q", f)
		}
	}
}
