package lower

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/ir"
)

// TestBakeAutoinitIncludeDefine_BakesFileAndRewritesDefine pins
// the VTK AUTOINIT_INCLUDE bake contract: when the define's value
// is a quoted absolute path to a file under the cmake build dir,
// bake reads the file, registers one genrule producing the bytes
// via base64-decode, attaches the basename to the target's Hdrs,
// and rewrites the define to point at the basename.
func TestBakeAutoinitIncludeDefine_BakesFileAndRewritesDefine(t *testing.T) {
	buildDir := t.TempDir()
	subdir := filepath.Join(buildDir, "CMakeFiles")
	if err := os.MkdirAll(subdir, 0o755); err != nil {
		t.Fatal(err)
	}
	contents := []byte("VTK_MODULE_INIT(vtkRenderingOpenGL2)\n")
	hdrPath := filepath.Join(subdir, "vtkModuleAutoInit_abc.h")
	if err := os.WriteFile(hdrPath, contents, 0o644); err != nil {
		t.Fatal(err)
	}
	def := `vtkRenderingCore_AUTOINIT_INCLUDE="` + hdrPath + `"`

	cc := newCodegenContext()
	irt := &ir.Target{Name: "vtkRenderingCore"}

	got, ok := bakeAutoinitIncludeDefine(def, buildDir, cc, irt)
	if !ok {
		t.Fatalf("bake refused; want ok")
	}
	wantDef := `vtkRenderingCore_AUTOINIT_INCLUDE="vtkModuleAutoInit_abc.h"`
	if got != wantDef {
		t.Errorf("rewritten define = %q; want %q", got, wantDef)
	}
	if !stringSliceContains(irt.Hdrs, "vtkModuleAutoInit_abc.h") {
		t.Errorf("Hdrs missing baked header; got %v", irt.Hdrs)
	}
	if !stringSliceContains(irt.Tags, "cmake-codegen-autoinit-bake") {
		t.Errorf("Tags missing autoinit-bake tag; got %v", irt.Tags)
	}
	if len(cc.Genrules) != 1 {
		t.Fatalf("Genrules len = %d; want 1", len(cc.Genrules))
	}
	gen := cc.Genrules[0]
	if gen.Kind != ir.KindGenrule {
		t.Errorf("genrule Kind = %v; want KindGenrule", gen.Kind)
	}
	if want := "vtkModuleAutoInit_abc.h"; len(gen.GenruleOuts) != 1 || gen.GenruleOuts[0] != want {
		t.Errorf("GenruleOuts = %v; want [%q]", gen.GenruleOuts, want)
	}
	// Verify the genrule cmd base64-decodes the right bytes.
	wantEnc := base64.StdEncoding.EncodeToString(contents)
	if !strings.Contains(gen.GenruleCmd, wantEnc) {
		t.Errorf("GenruleCmd missing expected base64 payload; got %q", gen.GenruleCmd)
	}
	if cc.OutToGenrule["vtkModuleAutoInit_abc.h"] != gen.Name {
		t.Errorf("OutToGenrule = %v; want %q→%q", cc.OutToGenrule, "vtkModuleAutoInit_abc.h", gen.Name)
	}
}

// TestBakeAutoinitIncludeDefine_DedupesAcrossTargets pins the
// shared-file optimization: when two targets consume the same
// auto-init header, only one genrule is synthesized but both
// targets pick up the basename in their Hdrs.
func TestBakeAutoinitIncludeDefine_DedupesAcrossTargets(t *testing.T) {
	buildDir := t.TempDir()
	hdrPath := filepath.Join(buildDir, "vtkModuleAutoInit_xy.h")
	if err := os.WriteFile(hdrPath, []byte("// shared\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	def := `KEY="` + hdrPath + `"`

	cc := newCodegenContext()
	first := &ir.Target{Name: "a"}
	second := &ir.Target{Name: "b"}

	if _, ok := bakeAutoinitIncludeDefine(def, buildDir, cc, first); !ok {
		t.Fatalf("first bake refused")
	}
	if _, ok := bakeAutoinitIncludeDefine(def, buildDir, cc, second); !ok {
		t.Fatalf("second bake refused")
	}
	if len(cc.Genrules) != 1 {
		t.Errorf("Genrules len = %d; want 1 (dedup)", len(cc.Genrules))
	}
	if !stringSliceContains(first.Hdrs, "vtkModuleAutoInit_xy.h") {
		t.Errorf("first.Hdrs missing baked header; got %v", first.Hdrs)
	}
	if !stringSliceContains(second.Hdrs, "vtkModuleAutoInit_xy.h") {
		t.Errorf("second.Hdrs missing baked header; got %v", second.Hdrs)
	}
}

// TestBakeAutoinitIncludeDefine_RefusesShapesItDoesNotHandle pins
// the negative cases: bake leaves the input alone (returns ok=false)
// when the define doesn't match the AUTOINIT shape so the caller
// can fall back to reanchorDefineValue's drop / passthrough path.
func TestBakeAutoinitIncludeDefine_RefusesShapesItDoesNotHandle(t *testing.T) {
	buildDir := t.TempDir()
	cases := []struct {
		name string
		def  string
	}{
		{"no-equals", "FOO"},
		{"not-absolute", `FOO="relative/path.h"`},
		{"outside-buildDir", `FOO="/etc/passwd"`},
		{"value-empty", `FOO=""`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cc := newCodegenContext()
			irt := &ir.Target{Name: "x"}
			got, ok := bakeAutoinitIncludeDefine(c.def, buildDir, cc, irt)
			if ok {
				t.Errorf("expected refusal; got rewrite %q", got)
			}
			if got != c.def {
				t.Errorf("on refusal define should be unchanged; got %q want %q", got, c.def)
			}
			if len(cc.Genrules) != 0 {
				t.Errorf("refusal populated Genrules: %v", cc.Genrules)
			}
			if len(irt.Hdrs) != 0 {
				t.Errorf("refusal populated Hdrs: %v", irt.Hdrs)
			}
		})
	}
}

// TestBakeAutoinitIncludeDefine_RefusesMissingFile pins the
// safety case: when the path is *inside* buildDir but the file
// doesn't actually exist at convert time, bake refuses so the
// caller's reanchor-drop path takes over (the file's absence
// will still surface at cmake configure time anyway).
func TestBakeAutoinitIncludeDefine_RefusesMissingFile(t *testing.T) {
	buildDir := t.TempDir()
	def := `KEY="` + filepath.Join(buildDir, "doesnotexist.h") + `"`
	cc := newCodegenContext()
	irt := &ir.Target{Name: "x"}
	got, ok := bakeAutoinitIncludeDefine(def, buildDir, cc, irt)
	if ok {
		t.Errorf("expected refusal on missing file; got rewrite %q", got)
	}
	if len(cc.Genrules) != 0 {
		t.Errorf("missing file refusal populated Genrules: %v", cc.Genrules)
	}
}
