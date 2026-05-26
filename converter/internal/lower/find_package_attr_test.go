package lower

import (
	"reflect"
	"strings"
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/internal/fileapi"
)

func TestFindPackageHeaderComments_BasicShape(t *testing.T) {
	events := []fileapi.Event{
		{
			Kind: "find_package-v1",
			Found: &fileapi.EventFindPackageFound{
				IsFound:    true,
				Package:    "Boost",
				Version:    "1.83.0",
				ConfigFile: "/usr/lib/x86_64-linux-gnu/cmake/Boost/BoostConfig.cmake",
			},
		},
		{
			Kind: "find_package-v1",
			Found: &fileapi.EventFindPackageFound{
				IsFound: false,
				Package: "MissingPkg",
			},
		},
		{
			Kind: "find_package-v1",
			Found: &fileapi.EventFindPackageFound{
				IsFound: true,
				Package: "ZLIB",
			},
		},
	}
	got := findPackageHeaderComments(events)
	if got == nil {
		t.Fatal("expected non-nil HeaderComments")
	}
	want := []string{
		"find_package resolutions (from cmake's configureLog):",
		"  - Boost 1.83.0 (via /usr/lib/x86_64-linux-gnu/cmake/Boost/BoostConfig.cmake)",
		"  - MissingPkg (NOT FOUND)",
		"  - ZLIB",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("HeaderComments mismatch:\n got: %v\nwant: %v", got, want)
	}
}

func TestFindPackageHeaderComments_EmptyEvents(t *testing.T) {
	if got := findPackageHeaderComments(nil); got != nil {
		t.Errorf("nil events should return nil; got %v", got)
	}
}

func TestFindPackageHeaderComments_DedupesPackages(t *testing.T) {
	// Same package looked up multiple times — first-write-wins.
	events := []fileapi.Event{
		{Kind: "find_package-v1", Found: &fileapi.EventFindPackageFound{IsFound: true, Package: "Boost", Version: "1.83.0"}},
		{Kind: "find_package-v1", Found: &fileapi.EventFindPackageFound{IsFound: true, Package: "Boost", Version: "1.84.0"}},
	}
	got := findPackageHeaderComments(events)
	if len(got) != 2 {
		t.Errorf("dedup expected; got %v", got)
	}
	if !strings.Contains(strings.Join(got, "\n"), "Boost 1.83.0") {
		t.Errorf("first-write should win: %v", got)
	}
}

func TestFindPackageHeaderComments_SkipsNonFindPackageEvents(t *testing.T) {
	events := []fileapi.Event{
		{Kind: "try_compile-v1"},
		{Kind: "message-v1"},
	}
	if got := findPackageHeaderComments(events); got != nil {
		t.Errorf("non-find_package events should produce nil; got %v", got)
	}
}

func TestOptionsHeaderComments_BasicShape(t *testing.T) {
	cache := fileapi.Cache{Entries: []fileapi.CacheEntry{
		{
			Name:  "FOO_ENABLE_TESTS",
			Type:  "BOOL",
			Value: "ON",
			Properties: []fileapi.CacheEntryProp{
				{Name: "HELPSTRING", Value: "Enable tests"},
			},
		},
		{
			Name:  "FOO_USE_GPU",
			Type:  "BOOL",
			Value: "OFF",
			Properties: []fileapi.CacheEntryProp{
				{Name: "HELPSTRING", Value: "Build with GPU acceleration"},
			},
		},
		// Should be filtered out — CMAKE_ prefix.
		{Name: "CMAKE_VERBOSE_MAKEFILE", Type: "BOOL", Value: "OFF"},
		// Not BOOL — skip.
		{Name: "FOO_PATH", Type: "PATH", Value: "/tmp"},
	}}
	got := optionsHeaderComments(cache)
	if len(got) == 0 {
		t.Fatal("expected header comments")
	}
	combined := strings.Join(got, "\n")
	if !strings.Contains(combined, "FOO_ENABLE_TESTS = ON") {
		t.Errorf("missing FOO_ENABLE_TESTS: %v", got)
	}
	if !strings.Contains(combined, "FOO_USE_GPU = OFF") {
		t.Errorf("missing FOO_USE_GPU: %v", got)
	}
	if strings.Contains(combined, "CMAKE_VERBOSE_MAKEFILE") {
		t.Errorf("CMAKE_ entry should be filtered: %v", got)
	}
	if strings.Contains(combined, "FOO_PATH") {
		t.Errorf("non-BOOL entry should be filtered: %v", got)
	}
}

func TestOptionsHeaderComments_EmptyCache(t *testing.T) {
	if got := optionsHeaderComments(fileapi.Cache{}); got != nil {
		t.Errorf("empty cache should return nil; got %v", got)
	}
}
