package lower_test

import (
	"path/filepath"
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/internal/fileapi"
	"github.com/sstriker/buildstream-bazel/converter/internal/lower"
)

// TestToIR_SubPackagesFromDirectoryIndex covers the out-of-band
// SubPackages map the --split-packages emit transform consumes: every
// real codemodel target carries its element-root-relative declaring
// directory, derived from ConfigTargetRef.DirectoryIndex →
// ConfigDirectory.Source.
//
// subdir-library declares `toplib` in the top-level CMakeLists (dir ".")
// and `util` under add_subdirectory(src/util) (dir "src/util"), so the
// map must read {"toplib": "", "util": "src/util"}. Install-derived /
// synthesized targets have no declaring CMakeLists dir; they resolve to
// the root package ("") via a missing key (treated as "" downstream).
func TestToIR_SubPackagesFromDirectoryIndex(t *testing.T) {
	r, err := fileapi.Load("../../testdata/fileapi/subdir-library")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	src, err := filepath.Abs("../../testdata/sample-projects/subdir-library")
	if err != nil {
		t.Fatalf("abs: %v", err)
	}

	pkg, err := lower.ToIR(r, nil, lower.Options{HostSourceRoot: src})
	if err != nil {
		t.Fatalf("ToIR: %v", err)
	}

	if pkg.SubPackages == nil {
		t.Fatalf("SubPackages is nil; want populated map")
	}
	if got, ok := pkg.SubPackages["toplib"]; !ok || got != "" {
		t.Errorf("SubPackages[toplib] = %q, ok=%v; want \"\" (root)", got, ok)
	}
	if got, ok := pkg.SubPackages["util"]; !ok || got != "src/util" {
		t.Errorf("SubPackages[util] = %q, ok=%v; want src/util", got, ok)
	}
}
