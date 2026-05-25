package configfold_test

import (
	"reflect"
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/internal/configfold"
	"github.com/sstriker/buildstream-bazel/converter/internal/fileapi"
)

func cacheWith(entries ...fileapi.CacheEntry) fileapi.Cache {
	return fileapi.Cache{Entries: entries}
}

func TestExtractSanitizerFlags_ASan(t *testing.T) {
	cache := cacheWith(
		fileapi.CacheEntry{Name: "CMAKE_C_FLAGS_ASAN", Value: "-fsanitize=address -fno-omit-frame-pointer -g -O1"},
		fileapi.CacheEntry{Name: "CMAKE_CXX_FLAGS_ASAN", Value: "-fsanitize=address -fno-omit-frame-pointer -g -O1"},
		fileapi.CacheEntry{Name: "CMAKE_EXE_LINKER_FLAGS_ASAN", Value: "-fsanitize=address"},
		fileapi.CacheEntry{Name: "CMAKE_SHARED_LINKER_FLAGS_ASAN", Value: "-fsanitize=address"},
		// Unrelated entries that shouldn't appear in output.
		fileapi.CacheEntry{Name: "CMAKE_C_FLAGS_RELEASE", Value: "-O3"},
	)
	got := configfold.ExtractSanitizerFlags(cache, []string{"Release", "ASan"})
	if len(got) != 1 {
		t.Fatalf("want 1 entry; got %d (%v)", len(got), got)
	}
	set, ok := got["ASan"]
	if !ok {
		t.Fatalf("ASan entry missing")
	}
	if set.FeatureName != "asan" {
		t.Errorf("FeatureName: %q", set.FeatureName)
	}
	wantCFlags := []string{"-fsanitize=address", "-fno-omit-frame-pointer", "-g", "-O1"}
	if !reflect.DeepEqual(set.CompileFlags["C"], wantCFlags) {
		t.Errorf("C flags: got %v want %v", set.CompileFlags["C"], wantCFlags)
	}
	if !reflect.DeepEqual(set.CompileFlags["CXX"], wantCFlags) {
		t.Errorf("CXX flags: got %v want %v", set.CompileFlags["CXX"], wantCFlags)
	}
	wantLink := []string{"-fsanitize=address"}
	if !reflect.DeepEqual(set.LinkFlags, wantLink) {
		t.Errorf("link flags: got %v want %v", set.LinkFlags, wantLink)
	}
}

func TestExtractSanitizerFlags_DedupesLinkUnion(t *testing.T) {
	// Different EXE vs SHARED link flags merge with first-wins
	// dedup: -fsanitize=thread is identical in both, so it
	// appears once; -Wl,--no-undefined is shared-only.
	cache := cacheWith(
		fileapi.CacheEntry{Name: "CMAKE_C_FLAGS_TSAN", Value: "-fsanitize=thread"},
		fileapi.CacheEntry{Name: "CMAKE_EXE_LINKER_FLAGS_TSAN", Value: "-fsanitize=thread"},
		fileapi.CacheEntry{Name: "CMAKE_SHARED_LINKER_FLAGS_TSAN", Value: "-fsanitize=thread -Wl,--no-undefined"},
	)
	got := configfold.ExtractSanitizerFlags(cache, []string{"TSan"})
	want := []string{"-fsanitize=thread", "-Wl,--no-undefined"}
	if !reflect.DeepEqual(got["TSan"].LinkFlags, want) {
		t.Errorf("merged link flags: %v want %v", got["TSan"].LinkFlags, want)
	}
}

func TestExtractSanitizerFlags_DropsConfigsWithoutFlags(t *testing.T) {
	// ASan recognized by SanitizerFeature but no cache entries —
	// skip rather than emit an empty feature.
	cache := cacheWith()
	got := configfold.ExtractSanitizerFlags(cache, []string{"ASan"})
	if got != nil {
		t.Errorf("expected nil for no cache entries; got %v", got)
	}
}

func TestExtractSanitizerFlags_IgnoresUnrecognizedConfigs(t *testing.T) {
	// Standard configs (Release, Debug) aren't sanitizer-shaped.
	cache := cacheWith(
		fileapi.CacheEntry{Name: "CMAKE_C_FLAGS_RELEASE", Value: "-O3"},
		fileapi.CacheEntry{Name: "CMAKE_C_FLAGS_DEBUG", Value: "-g -O0"},
	)
	got := configfold.ExtractSanitizerFlags(cache, []string{"Release", "Debug"})
	if got != nil {
		t.Errorf("expected nil for non-sanitizer configs; got %v", got)
	}
}

func TestExtractSanitizerFlags_MultipleSanitizers(t *testing.T) {
	cache := cacheWith(
		fileapi.CacheEntry{Name: "CMAKE_C_FLAGS_ASAN", Value: "-fsanitize=address"},
		fileapi.CacheEntry{Name: "CMAKE_C_FLAGS_TSAN", Value: "-fsanitize=thread"},
		fileapi.CacheEntry{Name: "CMAKE_C_FLAGS_UBSAN", Value: "-fsanitize=undefined"},
	)
	got := configfold.ExtractSanitizerFlags(cache,
		[]string{"Release", "ASan", "TSan", "UBSan"})
	if len(got) != 3 {
		t.Fatalf("want 3 entries; got %d", len(got))
	}
	if got["ASan"].FeatureName != "asan" {
		t.Errorf("ASan feature name: %q", got["ASan"].FeatureName)
	}
	if got["TSan"].FeatureName != "tsan" {
		t.Errorf("TSan feature name: %q", got["TSan"].FeatureName)
	}
	if got["UBSan"].FeatureName != "ubsan" {
		t.Errorf("UBSan feature name: %q", got["UBSan"].FeatureName)
	}
}

func TestExtractSanitizerFlags_QuotedFlag(t *testing.T) {
	cache := cacheWith(
		fileapi.CacheEntry{Name: "CMAKE_C_FLAGS_ASAN", Value: `-fsanitize=address "-fsanitize-blacklist=path with space.txt"`},
	)
	got := configfold.ExtractSanitizerFlags(cache, []string{"ASan"})
	want := []string{"-fsanitize=address", "-fsanitize-blacklist=path with space.txt"}
	if !reflect.DeepEqual(got["ASan"].CompileFlags["C"], want) {
		t.Errorf("quoted flag: %v want %v", got["ASan"].CompileFlags["C"], want)
	}
}

func TestSanitizerConfigOrder(t *testing.T) {
	sets := map[string]configfold.SanitizerFlagSet{
		"TSan":     {FeatureName: "tsan"},
		"ASan":     {FeatureName: "asan"},
		"Coverage": {FeatureName: "coverage"},
		"LTO":      {FeatureName: "lto"},
	}
	got := configfold.SanitizerConfigOrder(sets)
	want := []string{"ASan", "Coverage", "LTO", "TSan"} // sorted by feature name
	if !reflect.DeepEqual(got, want) {
		t.Errorf("order: got %v want %v", got, want)
	}
}
