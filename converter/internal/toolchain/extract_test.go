package toolchain

import (
	"strings"
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/internal/fileapi"
)

// TestFromReply_HelloWorldFixture exercises FromReply against the
// pre-recorded converter hello-world fileapi reply. We don't pin
// exact values (compiler path / version drift across machines that
// re-record); we assert the schema and the populated-vs-empty
// invariants the emitter depends on.
func TestFromReply_HelloWorldFixture(t *testing.T) {
	r, err := fileapi.Load("../../testdata/fileapi/hello-world")
	if err != nil {
		t.Fatalf("fileapi.Load: %v", err)
	}
	m, err := FromReply(r)
	if err != nil {
		t.Fatalf("FromReply: %v", err)
	}

	// hello-world is a generic cmake project that doesn't FORCE-cache
	// CMAKE_HOST_SYSTEM_NAME / _PROCESSOR — the probe-project fixture
	// (separate, M-toolchain step 3) does. Here we only assert
	// Host == Target (no toolchain file = non-cross-compile, even if
	// both are empty).
	if m.HostPlatform != m.TargetPlatform {
		t.Errorf("Host=%+v Target=%+v; should match for non-cross-compile fixture",
			m.HostPlatform, m.TargetPlatform)
	}

	// At least C must be present (hello-world is a C project).
	c, ok := m.Languages["C"]
	if !ok {
		t.Fatalf("Languages missing C; got %v", langKeys(m.Languages))
	}
	if c.CompilerPath == "" {
		t.Errorf("Languages.C.CompilerPath empty")
	}
	if c.CompilerID == "" {
		t.Errorf("Languages.C.CompilerID empty (expected GNU/Clang/...)")
	}
	if len(c.BuiltinIncludeDirs) == 0 {
		t.Errorf("Languages.C.BuiltinIncludeDirs empty; cmake should report compiler includes")
	}
	// hello-world is configured Release — exact value verified.
	if m.BuildType != "Release" {
		t.Errorf("BuildType = %q, want Release", m.BuildType)
	}
	// CMAKE_C_FLAGS_RELEASE typically contains -O3 / -DNDEBUG; assert
	// at least one of them.
	hasOpt := false
	for _, f := range c.BuildTypeFlags {
		if strings.HasPrefix(f, "-O") || f == "-DNDEBUG" {
			hasOpt = true
			break
		}
	}
	if !hasOpt {
		t.Errorf("Languages.C.BuildTypeFlags = %v; expected -O* or -DNDEBUG", c.BuildTypeFlags)
	}

	// Tools.AR is populated by every host cmake; same for strip.
	if m.Tools.AR == "" {
		t.Errorf("Tools.AR empty")
	}
}

func TestFromReply_NilRejected(t *testing.T) {
	if _, err := FromReply(nil); err == nil {
		t.Error("FromReply(nil) should error")
	}
}

func TestTokenizeCacheFlags_Whitespace(t *testing.T) {
	c := fileapi.Cache{Entries: []fileapi.CacheEntry{
		{Name: "X", Value: " -O3 -DNDEBUG  "},
	}}
	got := tokenizeCacheFlags(c, "X")
	want := []string{"-O3", "-DNDEBUG"}
	if !equalSlices(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestMergeFlags_Dedupes(t *testing.T) {
	got := mergeFlags([]string{"-a", "-b"}, []string{"-b", "-c"})
	want := []string{"-a", "-b", "-c"}
	if !equalSlices(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func langKeys(m map[string]Language) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func equalSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
