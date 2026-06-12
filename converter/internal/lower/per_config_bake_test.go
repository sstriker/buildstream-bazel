package lower

import (
	"reflect"
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/ir"
)

// TestApplyPerConfigBakes_ReanchorPolicies: per-config bodies
// re-anchor with the SAME policy their target's primary got, so a
// scratch-dir path spelling can't fabricate a select(). A
// file_generate-driven bake whose arms differ ONLY by the per-config
// scratch dir collapses to no select at all (marker policy unifies
// them); a configure_file bake's arms get the strip policy extended
// to the -cfg-<name> dirs.
func TestApplyPerConfigBakes_ReanchorPolicies(t *testing.T) {
	src, build := "/tmp/src", "/tmp/convert-element-build-1"
	pkg := &ir.Package{Targets: []ir.Target{
		{
			Name:             "gen_resp",
			Kind:             ir.KindWriteFile,
			WriteFileOut:     "x.data",
			Tags:             []string{"cmake-codegen", "cmake-codegen-driver=file_generate"},
			WriteFileContent: []string{"@BSB_GENDIR@/e/p/gen.h;m", "e/p/src.h;m", ""},
		},
		{
			Name:             "gen_cfgfile",
			Kind:             ir.KindWriteFile,
			WriteFileOut:     "c.h",
			Tags:             []string{"cmake-codegen-driver=configure_file"},
			WriteFileContent: []string{"#define DIR \"p/inc\"", ""},
		},
	}}
	bakes := map[string]map[string][]byte{
		"x.data": {
			"Debug":   []byte(build + "-cfg-Debug/p/gen.h;m\n" + src + "/p/src.h;m\n"),
			"Release": []byte(build + "-cfg-Release/p/gen.h;m\n" + src + "/p/src.h;m\n"),
		},
		"c.h": {
			"Debug":   []byte("#define DIR \"" + build + "-cfg-Debug/p/inc\"\n"),
			"Release": []byte("#define DIR \"" + src + "/p/inc\"\n"),
		},
	}
	applied := ApplyPerConfigBakes(pkg, bakes, src, build, "e")
	if !reflect.DeepEqual(applied, []string{}) && len(applied) != 0 {
		t.Fatalf("applied = %v, want none (marker policy unifies x.data's arms; strip policy unifies c.h's)", applied)
	}
	if pkg.Targets[0].WriteFileContentByConfig != nil {
		t.Errorf("x.data must not gain select arms: %v", pkg.Targets[0].WriteFileContentByConfig)
	}
	if pkg.Targets[1].WriteFileContentByConfig != nil {
		t.Errorf("c.h must not gain select arms: %v", pkg.Targets[1].WriteFileContentByConfig)
	}
}

// ApplyPerConfigBakes folds differing per-config bodies into
// WriteFileContentByConfig (keyed by //config:* label) + the audit tag;
// identical bodies, non-text bodies, and non-write_file targets are left
// untouched.
func TestApplyPerConfigBakes(t *testing.T) {
	pkg := &ir.Package{Targets: []ir.Target{
		{
			Name:             "gen_differs",
			Kind:             ir.KindWriteFile,
			WriteFileOut:     "cfg.h",
			WriteFileContent: []string{"#define CHECKS 0", ""},
		},
		{
			Name:             "gen_same",
			Kind:             ir.KindWriteFile,
			WriteFileOut:     "same.h",
			WriteFileContent: []string{"#define X 1", ""},
		},
		{
			Name:             "gen_binary",
			Kind:             ir.KindWriteFile,
			WriteFileOut:     "bin.h",
			WriteFileContent: []string{"text", ""},
		},
	}}
	bakes := map[string]map[string][]byte{
		"cfg.h": {
			"Debug":   []byte("#define CHECKS 1\n"),
			"Release": []byte("#define CHECKS 0\n"),
		},
		"same.h": {
			"Debug":   []byte("#define X 1\n"),
			"Release": []byte("#define X 1\n"),
		},
		"bin.h": {
			"Debug":   []byte("text\n"),
			"Release": []byte("bin\x00ary\n"), // fails the text gate
		},
	}
	applied := ApplyPerConfigBakes(pkg, bakes, "", "", "")
	if !reflect.DeepEqual(applied, []string{"gen_differs"}) {
		t.Fatalf("applied = %v, want [gen_differs]", applied)
	}
	d := pkg.Targets[0]
	if got := d.WriteFileContentByConfig["//config:debug"]; !reflect.DeepEqual(got, []string{"#define CHECKS 1", ""}) {
		t.Errorf("debug arm = %v", got)
	}
	if got := d.WriteFileContentByConfig["//config:release"]; !reflect.DeepEqual(got, []string{"#define CHECKS 0", ""}) {
		t.Errorf("release arm = %v", got)
	}
	if !stringSliceContains(d.Tags, "cmake-codegen-per-config-content") {
		t.Errorf("missing audit tag: %v", d.Tags)
	}
	if pkg.Targets[1].WriteFileContentByConfig != nil {
		t.Errorf("identical bodies must not gain per-config content")
	}
	if pkg.Targets[2].WriteFileContentByConfig != nil {
		t.Errorf("non-text body must keep the single primary body")
	}
}
