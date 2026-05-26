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
