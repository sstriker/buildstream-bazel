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

func TestRewriteFeature(t *testing.T) {
	// RewriteFeature is the toolchain-backed subset of Feature: it declines
	// features the toolchains converted projects target by default don't back
	// (the visibility presets, which no toolchain defines; and lsan, which only
	// the example sanitizer-features template defines, not the generated or
	// default toolchain) so the lift keeps those raw instead of silently
	// dropping them onto a no-op feature.
	cases := []struct{ flag, want string }{
		// Toolchain-backed → rewritten.
		{"-fPIC", "pic"},
		{"-flto", "lto"},
		{"-fsanitize=address", "asan"},
		{"-fsanitize=thread", "tsan"},
		{"-fsanitize=memory", "msan"},
		{"-fsanitize=undefined", "ubsan"},
		// Unbacked → declined (stays a raw copt), even though Feature() still
		// names them for the audit.
		{"-fvisibility=hidden", ""},
		{"-fvisibility-inlines-hidden", ""},
		{"-fsanitize=leak", ""},
	}
	for _, c := range cases {
		if got := toolchainfeature.RewriteFeature(c.flag); got != c.want {
			t.Errorf("RewriteFeature(%q) = %q, want %q", c.flag, got, c.want)
		}
	}
	// The declined ones must still be named by Feature so the audit's
	// "could be a feature" hint keeps firing.
	for _, flag := range []string{"-fvisibility=hidden", "-fvisibility-inlines-hidden", "-fsanitize=leak"} {
		if toolchainfeature.Feature(flag) == "" {
			t.Errorf("Feature(%q) = \"\"; audit detection should still name it", flag)
		}
	}
}
