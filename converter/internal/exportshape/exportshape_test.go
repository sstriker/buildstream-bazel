package exportshape_test

import (
	"strings"
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/internal/exportshape"
	"github.com/sstriker/buildstream-bazel/converter/internal/fileapi"
)

func TestClassify_NotAnExportInstaller(t *testing.T) {
	v := exportshape.Classify(fileapi.DirectoryInstaller{Type: "file"}, nil)
	if v.Declarative {
		t.Errorf("non-export installer classified as declarative: %+v", v)
	}
	if len(v.Reasons) != 1 || v.Reasons[0] != "not an export installer" {
		t.Errorf("Reasons: %v", v.Reasons)
	}
}

func TestClassify_DeclarativeHappyPath(t *testing.T) {
	inst := fileapi.DirectoryInstaller{
		Type:        "export",
		Destination: "lib/cmake/MyPkg",
		ExportName:  "MyPkgTargets",
		ExportTargets: []fileapi.ExportTarget{
			{Id: "foo::@", Name: "foo"},
			{Id: "bar::@", Name: "bar"},
		},
	}
	targets := map[string]fileapi.Target{
		"foo::@": {Name: "foo", Type: "STATIC_LIBRARY"},
		"bar::@": {Name: "bar", Type: "INTERFACE_LIBRARY"},
	}
	v := exportshape.Classify(inst, targets)
	if !v.Declarative {
		t.Errorf("happy path not declarative: %+v", v)
	}
	if len(v.Reasons) != 0 {
		t.Errorf("Reasons should be empty for declarative: %v", v.Reasons)
	}
}

func TestClassify_RejectsNonCanonicalDestination(t *testing.T) {
	inst := fileapi.DirectoryInstaller{
		Type:          "export",
		Destination:   "etc/my-custom-cmake-dir",
		ExportName:    "MyPkgTargets",
		ExportTargets: []fileapi.ExportTarget{{Id: "foo::@", Name: "foo"}},
	}
	targets := map[string]fileapi.Target{"foo::@": {Type: "STATIC_LIBRARY"}}
	v := exportshape.Classify(inst, targets)
	if v.Declarative {
		t.Errorf("non-canonical destination classified declarative")
	}
	if !containsReason(v.Reasons, "destination etc/my-custom-cmake-dir is not canonical") {
		t.Errorf("expected destination rejection in Reasons; got %v", v.Reasons)
	}
}

func TestClassify_RejectsExecutableExport(t *testing.T) {
	inst := fileapi.DirectoryInstaller{
		Type:          "export",
		Destination:   "lib/cmake/MyPkg",
		ExportName:    "MyPkgTargets",
		ExportTargets: []fileapi.ExportTarget{{Id: "tool::@", Name: "tool"}},
	}
	targets := map[string]fileapi.Target{"tool::@": {Name: "tool", Type: "EXECUTABLE"}}
	v := exportshape.Classify(inst, targets)
	if v.Declarative {
		t.Errorf("EXECUTABLE export classified declarative")
	}
	if !containsReason(v.Reasons, "unsupported type EXECUTABLE") {
		t.Errorf("expected EXECUTABLE rejection in Reasons; got %v", v.Reasons)
	}
}

func TestClassify_RejectsExcludeFromAll(t *testing.T) {
	inst := fileapi.DirectoryInstaller{
		Type:             "export",
		Destination:      "lib/cmake/MyPkg",
		ExportName:       "MyPkgTargets",
		ExportTargets:    []fileapi.ExportTarget{{Id: "foo::@", Name: "foo"}},
		IsExcludeFromAll: true,
	}
	targets := map[string]fileapi.Target{"foo::@": {Type: "STATIC_LIBRARY"}}
	v := exportshape.Classify(inst, targets)
	if v.Declarative {
		t.Errorf("EXCLUDE_FROM_ALL classified declarative")
	}
	if !containsReason(v.Reasons, "EXCLUDE_FROM_ALL") {
		t.Errorf("missing exclude-from-all rejection: %v", v.Reasons)
	}
}

func TestClassify_RejectsMissingExportName(t *testing.T) {
	inst := fileapi.DirectoryInstaller{
		Type:          "export",
		Destination:   "lib/cmake/MyPkg",
		ExportTargets: []fileapi.ExportTarget{{Id: "foo::@", Name: "foo"}},
	}
	targets := map[string]fileapi.Target{"foo::@": {Type: "STATIC_LIBRARY"}}
	v := exportshape.Classify(inst, targets)
	if v.Declarative {
		t.Errorf("missing exportName classified declarative")
	}
}

func TestClassify_RejectsTargetNotInCodemodel(t *testing.T) {
	inst := fileapi.DirectoryInstaller{
		Type:          "export",
		Destination:   "lib/cmake/MyPkg",
		ExportName:    "MyPkgTargets",
		ExportTargets: []fileapi.ExportTarget{{Id: "ghost::@", Name: "ghost"}},
	}
	targets := map[string]fileapi.Target{}
	v := exportshape.Classify(inst, targets)
	if v.Declarative {
		t.Errorf("missing target classified declarative")
	}
	if !containsReason(v.Reasons, "ghost") {
		t.Errorf("missing target should appear in reasons: %v", v.Reasons)
	}
}

func TestClassify_MultipleReasons(t *testing.T) {
	// Two failed preconditions; both should appear.
	inst := fileapi.DirectoryInstaller{
		Type:             "export",
		Destination:      "weird/path",
		ExportName:       "",
		IsExcludeFromAll: true,
	}
	v := exportshape.Classify(inst, nil)
	if v.Declarative {
		t.Error("classified declarative with three failures")
	}
	if len(v.Reasons) < 3 {
		t.Errorf("expected 3+ reasons (destination, exportName, exclude); got %d: %v", len(v.Reasons), v.Reasons)
	}
}

func TestClassify_CanonicalDestinationVariants(t *testing.T) {
	cases := []struct {
		dest string
		want bool
	}{
		{"lib/cmake", true},
		{"lib/cmake/MyPkg", true},
		{"lib64/cmake/MyPkg", true},
		{"share/cmake/MyPkg", true},
		{"share/MyPkg/cmake", true}, // matched by "share/" prefix
		{"etc/cmake/MyPkg", false},
		{"opt/cmake", false},
		{"", false},
	}
	for _, tc := range cases {
		inst := fileapi.DirectoryInstaller{
			Type:          "export",
			Destination:   tc.dest,
			ExportName:    "T",
			ExportTargets: []fileapi.ExportTarget{{Id: "foo::@", Name: "foo"}},
		}
		targets := map[string]fileapi.Target{"foo::@": {Type: "STATIC_LIBRARY"}}
		v := exportshape.Classify(inst, targets)
		if v.Declarative != tc.want {
			t.Errorf("destination %q: declarative=%v want %v; reasons=%v", tc.dest, v.Declarative, tc.want, v.Reasons)
		}
	}
}

func containsReason(reasons []string, substr string) bool {
	for _, r := range reasons {
		if strings.Contains(r, substr) {
			return true
		}
	}
	return false
}
