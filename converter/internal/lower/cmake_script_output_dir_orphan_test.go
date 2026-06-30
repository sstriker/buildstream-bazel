package lower

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/internal/ninja"
)

// TestPrecreateOutputDirOrphanDirs pins the file-vs-dir heuristic the tool-driven
// extract needs: the OUTPUT_DIR (`-DOUTPUT_DIR=<dir>`, no extension) is created as
// a directory so a tool the script runs into it doesn't fail on a missing dir,
// while a FILE cache arg (`-DMANIFEST=<...>.cmake`) must NOT be MkdirAll'd into a
// directory (that would block the script's file(WRITE) of the manifest).
func TestPrecreateOutputDirOrphanDirs(t *testing.T) {
	buildDir := t.TempDir()
	cc := newCodegenContext()
	b := &ninja.Build{Outputs: []string{filepath.Join(buildDir, "gen", "manifest.cmake")}}
	dArgs := []string{
		"-DOUTPUT_DIR=" + filepath.Join(buildDir, "gen"),
		"-DMANIFEST=" + filepath.Join(buildDir, "gen", "manifest.cmake"),
	}
	cc.precreateOutputDirOrphanDirs(b, dArgs, buildDir, buildDir)

	if st, err := os.Stat(filepath.Join(buildDir, "gen")); err != nil || !st.IsDir() {
		t.Errorf("OUTPUT_DIR gen/ should be created as a dir (err=%v)", err)
	}
	if st, err := os.Stat(filepath.Join(buildDir, "gen", "manifest.cmake")); err == nil && st.IsDir() {
		t.Errorf("the -DMANIFEST file path must NOT be created as a directory")
	}
}

// TestPrecreateOutputDirOrphanDirs_CrossBoundary pins the satellite shape: the
// OUTPUT_DIR lives in an OUTER build tree (a project(NONE) satellite's
// `-DOUTPUT_DIR=<OUTER_BUILD>/gen`), NOT under the satellite's own build dir. The
// pre-create guard must accept the outer roots (cc.OuterBuildDirs) — otherwise it
// refuses to create the dir and the standalone re-trace's tool fails on a missing
// dir whenever the real build hasn't already materialized it. A path outside
// every build tree is still left alone.
func TestPrecreateOutputDirOrphanDirs_CrossBoundary(t *testing.T) {
	satBuildDir := t.TempDir()
	outerBuildDir := t.TempDir()
	outsideDir := t.TempDir()
	cc := newCodegenContext()
	cc.OuterBuildDirs = []string{outerBuildDir}
	b := &ninja.Build{Outputs: []string{filepath.Join(outerBuildDir, "gen", "manifest.cmake")}}
	dArgs := []string{
		"-DOUTPUT_DIR=" + filepath.Join(outerBuildDir, "gen"), // cross-boundary OUTPUT_DIR
		"-DSTRAY=" + filepath.Join(outsideDir, "nope"),        // outside every build tree
	}
	cc.precreateOutputDirOrphanDirs(b, dArgs, satBuildDir, satBuildDir)

	if st, err := os.Stat(filepath.Join(outerBuildDir, "gen")); err != nil || !st.IsDir() {
		t.Errorf("cross-boundary OUTPUT_DIR <outer>/gen should be created as a dir (err=%v)", err)
	}
	if _, err := os.Stat(filepath.Join(outsideDir, "nope")); err == nil {
		t.Errorf("a dir outside every build tree must NOT be pre-created")
	}
}

// TestUnclaimedConsumedOrphans pins the demand-side set: consumed build-dir
// sources MINUS anything a ninja edge produces MINUS anything already claimed —
// the only orphans the OUTPUT_DIR attribution may declare.
func TestUnclaimedConsumedOrphans(t *testing.T) {
	cc := &codegenContext{
		ConsumedBuildRel: map[string]bool{
			"gen/foo.c":      true, // orphan: consumed, no ninja edge, unclaimed
			"gen/foo.h":      true, // orphan
			"gen/built.o":    true, // a ninja edge produces it — NOT an orphan
			"gen/other.c":    true, // already claimed by another recovery
			"gen/manifest.x": true, // orphan (kept; the caller's dir/disk gate filters it)
		},
		NinjaOuts:    map[string]bool{"gen/built.o": true},
		OutToGenrule: map[string]string{"gen/other.c": "gen_other"},
	}
	got := cc.unclaimedConsumedOrphans()
	want := []string{"gen/foo.c", "gen/foo.h", "gen/manifest.x"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("unclaimedConsumedOrphans() = %v, want %v", got, want)
	}
}

// TestUnclaimedConsumedOrphans_Empty pins that with no demand (or everything
// produced/claimed) the orphan set is empty — the early decline.
func TestUnclaimedConsumedOrphans_Empty(t *testing.T) {
	cc := &codegenContext{
		ConsumedBuildRel: map[string]bool{"gen/x.c": true},
		NinjaOuts:        map[string]bool{"gen/x.c": true},
	}
	if got := cc.unclaimedConsumedOrphans(); len(got) != 0 {
		t.Fatalf("expected no orphans when every consumed source is a ninja out; got %v", got)
	}
}

// TestRecoverOutputDirOrphanEdges_GateOff pins that the pass is a strict no-op
// when the codegen flags are off (no re-trace, no cc mutation) — the
// "degrades to today" guarantee. No cmake binary is needed: the gate short-
// circuits before any re-trace.
func TestRecoverOutputDirOrphanEdges_GateOff(t *testing.T) {
	cc := &codegenContext{
		ConsumedBuildRel: map[string]bool{"gen/foo.c": true},
		// RecognizeCodegen / CMakeScriptTrace / CMakeBinary all unset.
	}
	cc.recoverOutputDirOrphanEdges(nil, "/src", "/build")
	if len(cc.Genrules) != 0 || len(cc.OutToGenrule) != 0 {
		t.Fatalf("gate-off pass must not emit; got genrules=%d outToGenrule=%d",
			len(cc.Genrules), len(cc.OutToGenrule))
	}
}
