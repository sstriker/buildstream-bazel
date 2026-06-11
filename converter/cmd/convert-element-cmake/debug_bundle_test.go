package main

import (
	"os"
	"path/filepath"
	"sort"
	"testing"
)

func TestDebugBundleInput(t *testing.T) {
	for _, tc := range []struct {
		rel  string
		want bool
	}{
		// File API query + reply (outer).
		{".cmake/api/v1/reply/codemodel-v2-abc.json", true},
		{".cmake/api/v1/reply/index-2026.json", true},
		{".cmake/api/v1/query/codemodel-v2", true},
		// Trace / ninja / compile db / vars / cache / configure log.
		{"trace.jsonl", true},
		{"trace-plain.jsonl", true},
		{"build.ninja", true},
		{"CMakeFiles/rules.ninja", true},
		{"CMakeFiles/impl-Debug.ninja", true},
		{"compile_commands.json", true},
		{"cmake-to-bazel.vars.dump", true},
		{"CMakeCache.txt", true},
		{"CMakeFiles/CMakeConfigureLog.yaml", true},
		// Nested/recursive configure dir — same predicate, by subpath.
		{"mbedtls_downloader/.cmake/api/v1/reply/codemodel-v2-x.json", true},
		{"mbedtls_downloader/trace.jsonl", true},
		{"mbedtls_downloader/build.ninja", true},
		// Noise the bundle must NOT capture.
		{"CMakeFiles/foo.dir/src.c.o", false},
		{"libfoo.a", false},
		{"src/main.c", false},
		{"CMakeFiles/cmake.check_cache", false},
		{"downloaded/mbedtls/library/aes.c", false},
		{"random.txt", false},
	} {
		if got := debugBundleInput(tc.rel); got != tc.want {
			t.Errorf("debugBundleInput(%q) = %v, want %v", tc.rel, got, tc.want)
		}
	}
}

// TestSaveDebugBundle exercises the recursive capture end-to-end against a
// synthetic build dir carrying outer + nested-configure inputs plus noise:
// the bundle must mirror exactly the input set (layout preserved, nested
// dir included), exclude the noise, and write the README.
func TestSaveDebugBundle(t *testing.T) {
	buildDir := t.TempDir()
	write := func(rel, body string) {
		p := filepath.Join(buildDir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	inputs := []string{
		".cmake/api/v1/reply/codemodel-v2-abc.json",
		".cmake/api/v1/query/codemodel-v2",
		"trace.jsonl",
		"build.ninja",
		"CMakeFiles/rules.ninja",
		"compile_commands.json",
		"cmake-to-bazel.vars.dump",
		"CMakeCache.txt",
		"CMakeFiles/CMakeConfigureLog.yaml",
		// nested/recursive configure
		"mbedtls_downloader/.cmake/api/v1/reply/codemodel-v2-x.json",
		"mbedtls_downloader/trace.jsonl",
		"mbedtls_downloader/build.ninja",
	}
	noise := []string{
		"CMakeFiles/foo.dir/src.c.o",
		"libfoo.a",
		"downloaded/mbedtls/library/aes.c",
		"random.txt",
	}
	for _, rel := range inputs {
		write(rel, "X:"+rel)
	}
	for _, rel := range noise {
		write(rel, "noise")
	}

	bundleDir := filepath.Join(t.TempDir(), "bundle")
	if err := saveDebugBundle(buildDir, bundleDir); err != nil {
		t.Fatalf("saveDebugBundle: %v", err)
	}

	// Collect what landed in the bundle (minus the README).
	var got []string
	err := filepath.Walk(bundleDir, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(bundleDir, p)
		got = append(got, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(got)

	want := append([]string{"BUNDLE-README.txt"}, inputs...)
	sort.Strings(want)
	if len(got) != len(want) {
		t.Fatalf("bundle contents = %v\nwant %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("bundle contents = %v\nwant %v", got, want)
		}
	}

	// A captured file's bytes must match the source (real copy, not stub).
	body, err := os.ReadFile(filepath.Join(bundleDir, "mbedtls_downloader", "trace.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "X:mbedtls_downloader/trace.jsonl" {
		t.Errorf("captured nested trace body = %q", body)
	}
}
