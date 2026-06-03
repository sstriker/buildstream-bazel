package toolchainfeature_test

import (
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/internal/toolchain"
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
	// RewriteFeature gates on the DEFAULT (generated-toolchain) vocabulary: it
	// declines features that default doesn't back — the visibility presets
	// (neither the generated nor bazel's default toolchain defines them) and
	// lsan (only the example sanitizer-features template defines it) — so the
	// lift keeps those raw instead of silently dropping them onto a no-op
	// feature. (An operator whose toolchain DOES declare them re-enables
	// lifting via RewriteFeatureWith — see the lower package's tests.)
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

// TestRewriteFeature_TracksGeneratedVocabulary locks the toolkit-aware
// invariant: the lift rewrites a flag to a feature iff that feature is in the
// generated toolchain's declared vocabulary (toolchain.GeneratedFeatures)
// plus the built-in `pic`. Computing the expectation from the same source
// RewriteFeature gates on means this fails if RewriteFeature ever stops
// tracking the vocabulary (e.g. reverts to a divergent hardcoded list), and
// auto-adjusts when the generated toolchain's feature set legitimately
// changes — so the lift and the emitted toolchain can't drift.
func TestRewriteFeature_TracksGeneratedVocabulary(t *testing.T) {
	backed := map[string]bool{"pic": true}
	for _, f := range toolchain.GeneratedFeatures() {
		backed[string(f)] = true
	}
	// Every flag Feature() recognizes.
	flags := []string{
		"-fPIC", "-fpic", "-flto",
		"-fvisibility=hidden", "-fvisibility-inlines-hidden",
		"-fsanitize=address", "-fsanitize=thread",
		"-fsanitize=memory", "-fsanitize=undefined", "-fsanitize=leak",
	}
	for _, flag := range flags {
		name := toolchainfeature.Feature(flag)
		want := ""
		if backed[name] {
			want = name
		}
		if got := toolchainfeature.RewriteFeature(flag); got != want {
			t.Errorf("RewriteFeature(%q) = %q, want %q (feature %q, backed=%v)",
				flag, got, want, name, backed[name])
		}
	}
}
