package lower_test

import (
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/internal/fileapi"
	"github.com/sstriker/buildstream-bazel/converter/internal/lower"
)

// TestToIR_TraceInterfaceLib_PlacedInDeclaringSubPackage pins the abseil
// interface-subpackage placement: a trace-synth INTERFACE library declared via a
// function wrapper (abseil's absl_cc_library, whose add_library physically runs
// in CMake/AbseilHelpers.cmake) must be placed — via pkg.SubPackages — in the
// ENCLOSING CMakeLists.txt's directory (recovered through the trace frame
// stack), not the root package and not the helper module's dir. Without this,
// --split-packages emits no BUILD.bazel for absl/<m> even though the lib is
// declared there.
func TestToIR_TraceInterfaceLib_PlacedInDeclaringSubPackage(t *testing.T) {
	r := &fileapi.Reply{
		Codemodel: fileapi.Codemodel{
			Paths:          fileapi.CodemodelPaths{Source: "/src", Build: "/build"},
			Configurations: []fileapi.Configuration{{Name: "Release"}},
		},
	}
	// frame-1 call site in the declaring sub-package CMakeLists, then the
	// function body's add_library at frame 2 in the helper module.
	traceRaw := []byte(
		`{"args":["mylib","INTERFACE"],"cmd":"my_cc_library","file":"/src/sub/mod/CMakeLists.txt","frame":1,"line":3}` + "\n" +
			`{"args":["mylib","INTERFACE"],"cmd":"add_library","file":"/src/cmake/Helpers.cmake","frame":2,"line":50}` + "\n",
	)
	pkg, err := lower.ToIR(r, nil, lower.Options{
		HostSourceRoot: "/src",
		BuildDir:       "/build",
		TraceRaw:       traceRaw,
	})
	if err != nil {
		t.Fatalf("ToIR: %v", err)
	}
	var found bool
	for _, tgt := range pkg.Targets {
		if tgt.Name == "mylib" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("mylib (trace-synth interface lib) not emitted; targets=%v", pkg.Targets)
	}
	if got := pkg.SubPackages["mylib"]; got != "sub/mod" {
		t.Errorf("pkg.SubPackages[mylib] = %q, want \"sub/mod\" (enclosing CMakeLists scope via frame stack, not root or cmake/)", got)
	}
}
