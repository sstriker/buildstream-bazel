package lower

import (
	"path/filepath"
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/internal/fileapi"
	"github.com/sstriker/buildstream-bazel/converter/internal/rejection"
	"github.com/sstriker/buildstream-bazel/converter/ir"
)

// TestLowerGeneratedSource_WiredOutputNotRejected locks the OutToGenrule-first
// rule: an IsGenerated codemodel source whose build-dir output a non-
// CUSTOM_COMMAND lift already recovered (configure_file / file(GENERATE) /
// execute_process — recorded in cc.OutToGenrule with no ninja edge for
// recoverGenrule to find) must attach to that producer, NOT be rejected.
//
// Before the guard, the IsGenerated path only tried on-disk → ninja
// CUSTOM_COMMAND → recoverGenrule, none of which see an OutToGenrule-only
// output, so a source we DID wire was falsely rejected — while the identical
// source arriving IsGenerated=false would have been wired (recoverOrElide-
// BuildDirSource). This is the symmetry that closes the asymmetry.
func TestLowerGeneratedSource_WiredOutputNotRejected(t *testing.T) {
	const cmakeSrc, cmakeBuild = "/src", "/build"

	cc := newCodegenContext()
	cc.OutToGenrule["gen/foo.cpp"] = "gen_foo" // recovered by some other lift

	rej := rejection.New()
	lc := targetLowerCtx{cc: cc, cmakeSrc: cmakeSrc, cmakeBuild: cmakeBuild, rejections: rej}

	irt := &ir.Target{Name: "consumer", Kind: ir.KindCCLibrary}
	st := &sourceWalkState{srcEmitPath: map[int]string{}}
	src := fileapi.TargetSource{
		Path:        filepath.Join(cmakeBuild, "gen/foo.cpp"), // abs, under build, not on disk
		IsGenerated: true,
	}

	if err := lowerGeneratedSource(irt, &fileapi.Target{Name: "consumer"}, src, 0, st, true, lc); err != nil {
		t.Fatalf("lowerGeneratedSource returned a hard error: %v", err)
	}
	if rej.Len() != 0 {
		t.Errorf("already-wired output was rejected: %d rejection(s): %+v", rej.Len(), rej.Items())
	}
	if len(irt.Srcs) != 1 || irt.Srcs[0] != "gen/foo.cpp" {
		t.Errorf("srcs = %v; want [gen/foo.cpp] wired to the recovered producer", irt.Srcs)
	}
	if !st.consumesCodegen {
		t.Error("consumesCodegen not set; the recovered genrule edge wasn't recorded as codegen")
	}
}

// TestLowerGeneratedSource_UnwiredStillRejected is the contrast: the same
// IsGenerated source with NO recovered producer (empty OutToGenrule, no ninja
// graph) still records a rejection. The guard only rescues genuinely-wired
// outputs — it must not paper over a real missing producer.
func TestLowerGeneratedSource_UnwiredStillRejected(t *testing.T) {
	const cmakeSrc, cmakeBuild = "/src", "/build"

	cc := newCodegenContext()
	rej := rejection.New()
	lc := targetLowerCtx{cc: cc, cmakeSrc: cmakeSrc, cmakeBuild: cmakeBuild, rejections: rej}

	irt := &ir.Target{Name: "consumer", Kind: ir.KindCCLibrary}
	st := &sourceWalkState{srcEmitPath: map[int]string{}}
	src := fileapi.TargetSource{Path: filepath.Join(cmakeBuild, "gen/foo.cpp"), IsGenerated: true}

	if err := lowerGeneratedSource(irt, &fileapi.Target{Name: "consumer"}, src, 0, st, true, lc); err != nil {
		t.Fatalf("diagnostic mode should soft-record, not hard-error: %v", err)
	}
	if rej.Len() == 0 {
		t.Error("an unwired generated source with no producer should still be rejected")
	}
	if len(irt.Srcs) != 0 {
		t.Errorf("unwired source must not be attached; srcs = %v", irt.Srcs)
	}
}
