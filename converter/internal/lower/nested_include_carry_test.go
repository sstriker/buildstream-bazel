package lower

import (
	"bytes"
	"strings"
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/ir"
)

// TestMergeNestedPackage_CarriesIncludeCalls: a nested build's include() events
// are carried (absolute, under nb.HostBuildDir) into the OUTER cc.IncludeCalls,
// so adoptIncludedRecipeOutput can tie an OUTER consumer's generated source to a
// recipe .cmake produced+include()d inside the nested build.
func TestMergeNestedPackage_CarriesIncludeCalls(t *testing.T) {
	cc := newCodegenContext()
	nb := NestedBuildInput{
		BuildRel:     "ext/foo",
		HostBuildDir: "/tmp/build/ext/foo",
		TraceRaw: []byte(`{"args":["/tmp/build/ext/foo/gen/recipe.cmake"],"cmd":"include","file":"/src/ext/foo/CMakeLists.txt","line":5}
{"args":["/usr/share/cmake/Modules/GNUInstallDirs.cmake"],"cmd":"include","file":"/src/ext/foo/CMakeLists.txt","line":2}
`),
	}
	mergeNestedPackage(&ir.Package{}, &ir.Package{}, nb, cc, Options{}, "/src")

	var got []string
	for _, inc := range cc.IncludeCalls {
		got = append(got, inc.Path)
	}
	want := "/tmp/build/ext/foo/gen/recipe.cmake"
	found := false
	for _, p := range got {
		if p == want {
			found = true
		}
	}
	if !found {
		t.Errorf("nested recipe include not carried into cc.IncludeCalls; got %v", got)
	}
}

// TestMergeNestedPackage_TracelessBreadcrumb: a trace-less nested build (no
// nb.TraceRaw) carries no include events and warns that the OUTPUT->include
// recovery is degraded for its outer consumers.
func TestMergeNestedPackage_TracelessBreadcrumb(t *testing.T) {
	cc := newCodegenContext()
	var warn bytes.Buffer
	nb := NestedBuildInput{BuildRel: "ext/bar", HostBuildDir: "/tmp/build/ext/bar"} // no TraceRaw
	mergeNestedPackage(&ir.Package{}, &ir.Package{}, nb, cc, Options{Warnings: &warn}, "/src")
	if len(cc.IncludeCalls) != 0 {
		t.Errorf("trace-less nested build should carry no include calls; got %v", cc.IncludeCalls)
	}
	if !strings.Contains(warn.String(), "ext/bar") || !strings.Contains(warn.String(), "no trace captured") {
		t.Errorf("expected trace-less breadcrumb; got %q", warn.String())
	}
}
