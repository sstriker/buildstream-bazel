package lower

import (
	"strings"
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/ir"
)

// TestDropNinjaDepfilePlumbing pins the VTK wrap-hierarchy shape: the
// `-MF <out>.d` pair and the `&&`-chained `cmake -E
// cmake_transform_depfile …` segment (absolute cmake path and all)
// are ninja-incrementality machinery a sandboxed genrule must not
// carry; a `-MF` pair naming a DECLARED out survives.
func TestDropNinjaDepfilePlumbing(t *testing.T) {
	cmd := `$(location //e/Wrapping:WrapHierarchy) -MF Common/Core/CMakeFiles/x-hierarchy.txt.Debug.d @Common/Core/CMakeFiles/x.Debug.args -o $(RULEDIR)/lib/hierarchy/x.txt Common/Core/CMakeFiles/x.data && /usr/local/opt/cmake-4.3.3/bin/cmake -E cmake_transform_depfile Ninja\ Multi-Config gccdepfile e e/Common/Core . Common/Core Common/Core/CMakeFiles/x.txt.Debug.d CMakeFiles/d/abc.d`
	got := dropNinjaDepfilePlumbing(cmd, []string{"lib/hierarchy/x.txt"})
	if strings.Contains(got, "-MF") || strings.Contains(got, "cmake_transform_depfile") {
		t.Errorf("depfile plumbing survived:\n%s", got)
	}
	if !strings.Contains(got, "@Common/Core/CMakeFiles/x.Debug.args") || !strings.Contains(got, "-o $(RULEDIR)/lib/hierarchy/x.txt") {
		t.Errorf("payload tokens damaged:\n%s", got)
	}
	declared := dropNinjaDepfilePlumbing("tool -MF out.d -o out.d", []string{"out.d"})
	if !strings.Contains(declared, "-MF out.d") {
		t.Errorf("-MF naming a declared out must survive: %s", declared)
	}
}

// TestRewriteGeneratedSrcRefs_Location: generated srcs referenced by
// their build-dir-relative spelling (bare and @-response form)
// rewrite to $(location <src>) so the action reads the staged path;
// split's relocateGenruleSrcs relabels in lockstep.
func TestRewriteGeneratedSrcRefs_Location(t *testing.T) {
	cc := newCodegenContext()
	cc.OutToGenrule["Common/Core/CMakeFiles/x.Debug.args"] = "gen_args"
	cc.OutToGenrule["Common/Core/CMakeFiles/x.data"] = "gen_data"
	srcs := []string{"Common/Core/CMakeFiles/x.Debug.args", "Common/Core/CMakeFiles/x.data", "Common/Core/vtkABI.h"}
	cmd := "tool @Common/Core/CMakeFiles/x.Debug.args -o $(RULEDIR)/x.txt Common/Core/CMakeFiles/x.data"
	got := rewriteGeneratedSrcRefs(cmd, srcs, cc)
	want := "tool @$(location Common/Core/CMakeFiles/x.Debug.args) -o $(RULEDIR)/x.txt $(location Common/Core/CMakeFiles/x.data)"
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

// TestRewriteGeneratedSrcRefs_GendirMarker: a marker-carrying bake
// (content embeds @BSB_GENDIR@) routes through the sed preamble that
// substitutes $(GENDIR) into a scratch copy at action time.
func TestRewriteGeneratedSrcRefs_GendirMarker(t *testing.T) {
	cc := newCodegenContext()
	cc.OutToGenrule["CMakeFiles/x.data"] = "gen_data"
	cc.GendirMarkedOuts["CMakeFiles/x.data"] = true
	cmd := "tool CMakeFiles/x.data -o $(RULEDIR)/x.txt"
	got := rewriteGeneratedSrcRefs(cmd, []string{"CMakeFiles/x.data"}, cc)
	for _, want := range []string{
		"BSB_RD=$$(mktemp -d) && ",
		"sed -e 's|@BSB_GENDIR@|$(GENDIR)|g' $(location CMakeFiles/x.data) > $$BSB_RD/",
		"tool $$BSB_RD/",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if strings.Contains(got, "tool CMakeFiles/x.data") {
		t.Errorf("raw spelling survived:\n%s", got)
	}
}

// TestResponseFileGeneratedHdrs: a marker-carrying response file's
// -I roots under @BSB_GENDIR@/<labelRoot>/ expose the build dir's
// generated headers implicitly (cmake's visibility); the genrule's
// srcs gain every recovered header-ish output under those roots —
// the export-header shape (#include "vtkCommonCoreModule.h") that no
// ninja edge ever declares as an input.
func TestResponseFileGeneratedHdrs(t *testing.T) {
	cc := newCodegenContext()
	cc.GendirMarkedOuts["CMakeFiles/x.args"] = true
	cc.OutToGenrule["CMakeFiles/x.args"] = "gen_args"
	cc.OutToGenrule["Common/Core/vtkCommonCoreModule.h"] = "gen_mod"
	cc.OutToGenrule["Common/Core/vtkABINamespace.h"] = "gen_abi"
	cc.OutToGenrule["Other/place.h"] = "gen_other"
	cc.OutToGenrule["Common/Core/proj.db"] = "gen_db"
	cc.Genrules = append(cc.Genrules, ir.Target{
		Kind:         ir.KindWriteFile,
		WriteFileOut: "CMakeFiles/x.args",
		WriteFileContent: []string{
			"-I'elements/vtk/Common/Core'",
			"-I'@BSB_GENDIR@/elements/vtk/Common/Core'",
			"",
		},
	})
	srcs := []string{"CMakeFiles/x.args", "Common/Core/vtkABINamespace.h"}
	got := responseFileGeneratedHdrs(srcs, cc, "elements/vtk")
	want := []string{"Common/Core/vtkCommonCoreModule.h"}
	if len(got) != 1 || got[0] != want[0] {
		t.Errorf("got %v, want %v (already-present + non-header + out-of-root excluded)", got, want)
	}
	if more := responseFileGeneratedHdrs([]string{"plain.txt"}, cc, "elements/vtk"); more != nil {
		t.Errorf("non-marked srcs must add nothing: %v", more)
	}
}

// TestReanchorResponseContent: source-tree paths re-anchor to the
// exec-root element form; build-dir paths (incl. the per-config
// -cfg-<name> re-configure dirs) re-anchor to the @BSB_GENDIR@
// marker; marked reports whether a marker was emitted.
func TestReanchorResponseContent(t *testing.T) {
	body := []byte("-I'/tmp/vtk/Common/Core'\n" +
		"-I'/tmp/convert-element-build-123/Common/Core'\n" +
		"/tmp/convert-element-build-123-cfg-Debug/Common/Core/vtkABINamespace.h;vtkCommonCore\n" +
		"/tmp/vtk/Common/Core/vtkABI.h;vtkCommonCore\n")
	got, marked := reanchorResponseContent(body, "/tmp/vtk", "/tmp/convert-element-build-123", "elements/vtk")
	want := "-I'elements/vtk/Common/Core'\n" +
		"-I'@BSB_GENDIR@/elements/vtk/Common/Core'\n" +
		"@BSB_GENDIR@/elements/vtk/Common/Core/vtkABINamespace.h;vtkCommonCore\n" +
		"elements/vtk/Common/Core/vtkABI.h;vtkCommonCore\n"
	if string(got) != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
	if !marked {
		t.Error("marked must be true when a build-dir path re-anchored")
	}
	plain, marked2 := reanchorResponseContent([]byte("no paths here\n"), "/tmp/vtk", "/tmp/b", "elements/vtk")
	if string(plain) != "no paths here\n" || marked2 {
		t.Errorf("no-op body must pass through unmarked: %q %v", plain, marked2)
	}
}
