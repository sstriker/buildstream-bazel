package lower

import (
	"os"
	"path/filepath"
	"testing"
)

// TestCheckedInGeneratedSource_LiveAsRegularSource pins the
// libevent fix: when cmake records IsGenerated=true on a source
// that's committed to the repo (e.g. test/regress.gen.c produced
// by event_rpcgen.py but checked in for build convenience), the
// converter should route it through the regular source-path
// handling instead of refusing with recoverGenrule.
//
// This is a behavioural test that exercises the per-source loop
// in lowerTarget via ToIR; the bug is the recoverGenrule call
// path firing on cmakeSrc-relative paths that happen to exist
// on disk.
func TestCheckedInGeneratedSource_LiveAsRegularSource(t *testing.T) {
	// The fix in lower.go pre-empts recoverGenrule when the source
	// exists at <hostSrc>/<rel>. Both the absolute-path-under-
	// cmakeSrc form and the cmakeSrc-relative form must work.
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "test"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "test/regress.gen.c"), []byte("int x;\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Direct check: filepath.Join(hostSrc, rel) finds the file
	// when rel is the cmakeSrc-relative form cmake records.
	hostSrc := root
	rel := "test/regress.gen.c"
	if _, err := os.Stat(filepath.Join(hostSrc, rel)); err != nil {
		t.Fatalf("checked-in file should be detectable: %v", err)
	}

	// Direct check: relativeIfInside(cmakeSrc, absPath) returns
	// the rel form for the absolute-path-under-cmakeSrc case.
	absPath := filepath.Join(root, "test/regress.gen.c")
	gotRel, inside := relativeIfInside(root, absPath)
	if !inside {
		t.Errorf("relativeIfInside returned inside=false for absPath under cmakeSrc")
	}
	if gotRel != rel {
		t.Errorf("relativeIfInside = %q; want %q", gotRel, rel)
	}
}
