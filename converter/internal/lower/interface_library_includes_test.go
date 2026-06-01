package lower

import (
	"reflect"
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/internal/fileapi"
	"github.com/sstriker/buildstream-bazel/converter/ir"
)

// callLowerTargetIface is a thin helper that runs lowerTarget against a
// constructed INTERFACE_LIBRARY codemodel target with the long
// parameter list reduced to its zero values. Only the inputs that
// matter for include/hdr extraction are populated by callers.
func callLowerTargetIface(t *testing.T, tgt *fileapi.Target, cmakeSrc string) *ir.Target {
	t.Helper()
	cc := &codegenContext{HeaderWalkCache: map[string][]string{}, MissingIncludeDirs: map[string]bool{}}
	irt, err := lowerTarget(tgt, targetTrace{
		privateIncludeDirs:           map[string]bool{},
		platformConditionalSrcs:      map[string]string{},
		platformConditionalSrcsToAdd: map[string][]string{},
	}, targetLowerCtx{
		cmakeSrc:   cmakeSrc,
		cmakeBuild: "/build",
		cc:         cc,
		idToName:   map[string]string{},
		utilityIDs: map[string]bool{},
	})
	if err != nil {
		t.Fatalf("lowerTarget: %v", err)
	}
	if irt == nil {
		t.Fatal("lowerTarget returned nil target")
	}
	return irt
}

// TestInterfaceLibrary_IncludesFromFileSets pins #308: an
// INTERFACE_LIBRARY target reaching the codemodel path (cmake >= 3.19)
// has no CompileGroups, so its include directories are recovered from
// the HEADERS FileSets' BaseDirectories rather than the CompileGroups
// include loop. Without this the emitted cc_library lacks `includes =`
// and consumers hit "undeclared inclusion" errors.
func TestInterfaceLibrary_IncludesFromFileSets(t *testing.T) {
	hidx := 0
	tgt := &fileapi.Target{
		Name:  "myheaders",
		Id:    "myheaders::@abc",
		Type:  "INTERFACE_LIBRARY",
		Paths: fileapi.TargetPaths{Source: "/src", Build: "/build"},
		Sources: []fileapi.TargetSource{
			{Path: "include/foo.h", FileSetIndex: &hidx},
			{Path: "include/bar.hpp", FileSetIndex: &hidx},
		},
		FileSets: []fileapi.TargetFileSet{
			{Name: "HEADERS", Type: "HEADERS", Visibility: "PUBLIC", BaseDirectories: []string{"/src/include"}},
		},
	}
	got := callLowerTargetIface(t, tgt, "/src")

	if want := []string{"include"}; !reflect.DeepEqual(got.Includes, want) {
		t.Errorf("Includes = %v; want %v", got.Includes, want)
	}
	if want := []string{"include/bar.hpp", "include/foo.h"}; !reflect.DeepEqual(got.Hdrs, want) {
		t.Errorf("Hdrs = %v; want %v", got.Hdrs, want)
	}
	if len(got.Srcs) != 0 {
		t.Errorf("Srcs = %v; want empty", got.Srcs)
	}
}

// TestInterfaceLibrary_PackageRootBaseDirDropped pins the rel=="" path:
// a FILE_SET whose BASE_DIRS is the package root must not emit
// `includes = [""]` (Bazel rejects it) — the package-root walk handles
// header discovery instead.
func TestInterfaceLibrary_PackageRootBaseDirDropped(t *testing.T) {
	hidx := 0
	tgt := &fileapi.Target{
		Name:  "rootheaders",
		Id:    "rootheaders::@abc",
		Type:  "INTERFACE_LIBRARY",
		Paths: fileapi.TargetPaths{Source: "/src", Build: "/build"},
		Sources: []fileapi.TargetSource{
			{Path: "foo.h", FileSetIndex: &hidx},
		},
		FileSets: []fileapi.TargetFileSet{
			{Name: "HEADERS", Type: "HEADERS", Visibility: "PUBLIC", BaseDirectories: []string{"/src"}},
		},
	}
	got := callLowerTargetIface(t, tgt, "/src")
	if len(got.Includes) != 0 {
		t.Errorf("Includes = %v; want empty (package-root base dir dropped)", got.Includes)
	}
}
