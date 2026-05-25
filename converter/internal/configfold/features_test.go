package configfold_test

import (
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/internal/configfold"
)

func TestSanitizerFeature(t *testing.T) {
	cases := []struct {
		config  string
		feature string
		ok      bool
	}{
		// Standard cmake configs — not feature variants.
		{"Release", "", false},
		{"Debug", "", false},
		{"RelWithDebInfo", "", false},
		{"MinSizeRel", "", false},

		// Address sanitizer aliases.
		{"ASan", "asan", true},
		{"Asan", "asan", true},
		{"asan", "asan", true},
		{"ASAN", "asan", true},
		{"AddressSanitizer", "asan", true},
		{"address_sanitizer", "asan", true},

		// Thread sanitizer.
		{"TSan", "tsan", true},
		{"ThreadSanitizer", "tsan", true},

		// Memory sanitizer.
		{"MSan", "msan", true},
		{"MemorySanitizer", "msan", true},

		// Undefined-behavior sanitizer.
		{"UBSan", "ubsan", true},
		{"UndefinedBehaviorSanitizer", "ubsan", true},
		{"undefined", "ubsan", true},

		// Leak sanitizer.
		{"LSan", "lsan", true},
		{"LeakSanitizer", "lsan", true},

		// Coverage.
		{"Coverage", "coverage", true},
		{"cov", "coverage", true},
		{"gcov", "coverage", true},

		// LTO + suffix variants.
		{"LTO", "lto", true},
		{"ReleaseLTO", "lto", true},
		{"MinSizeRelLTO", "lto", true},
		{"RelWithDebInfoLTO", "lto", true},

		// Unrelated custom names — not features.
		{"Profile", "", false},
		{"Sanitized", "", false},
		{"", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.config, func(t *testing.T) {
			feat, ok := configfold.SanitizerFeature(tc.config)
			if ok != tc.ok || feat != tc.feature {
				t.Errorf("SanitizerFeature(%q) = (%q, %v); want (%q, %v)",
					tc.config, feat, ok, tc.feature, tc.ok)
			}
		})
	}
}
