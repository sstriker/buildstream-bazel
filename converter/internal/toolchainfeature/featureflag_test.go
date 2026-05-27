package toolchainfeature_test

import (
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/internal/toolchainfeature"
)

func TestFeature(t *testing.T) {
	cases := []struct {
		flag, want string
	}{
		{"-fPIC", "pic"},
		{"-fpic", "pic"},
		{"-flto", "lto"},
		{"-fvisibility=hidden", "visibility_hidden"},
		{"-fvisibility-inlines-hidden", "visibility_inlines_hidden"},
		{"-fsanitize=address", "asan"},
		{"-fsanitize=thread", "tsan"},
		{"-fsanitize=memory", "msan"},
		{"-fsanitize=undefined", "ubsan"},
		{"-fsanitize=leak", "lsan"},
		// Unregistered raw flags get empty so the rewrite
		// declines (caller leaves the flag in copts).
		{"-O2", ""},
		{"-Wall", ""},
		{"-fvisibility=default", ""},
		// Forward-compat: an unknown -fsanitize=<x> shape isn't
		// rewritten (no name to use), but the audit-side
		// LooksLikeFeatureFlag still flags it.
		{"-fsanitize=cfi", ""},
	}
	for _, c := range cases {
		if got := toolchainfeature.Feature(c.flag); got != c.want {
			t.Errorf("Feature(%q) = %q, want %q", c.flag, got, c.want)
		}
	}
}

func TestLooksLikeFeatureFlag_ForwardCompatSanitize(t *testing.T) {
	if !toolchainfeature.LooksLikeFeatureFlag("-fsanitize=cfi") {
		t.Error("LooksLikeFeatureFlag(-fsanitize=cfi) = false; want true (audit-side detection)")
	}
	if toolchainfeature.LooksLikeFeatureFlag("-O2") {
		t.Error("LooksLikeFeatureFlag(-O2) = true; want false")
	}
}
