package lower

import (
	"reflect"
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/ir"
)

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
	applied := ApplyPerConfigBakes(pkg, bakes)
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
