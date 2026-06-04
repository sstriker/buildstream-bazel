package lower_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/internal/fileapi"
	"github.com/sstriker/buildstream-bazel/converter/internal/lower"
)

// systemIncludeReply builds a one-target reply whose single PRIVATE
// include dir (priv) is flagged isSystem per the codemodel, plus a
// matching target_include_directories trace that scopes the dir
// PRIVATE so the converter routes it to compile-only copts. The trace
// dir is the absolute codemodel path so the privateIncludeDirs overlay
// matches inc.Path exactly.
func systemIncludeReply(t *testing.T, isSystem bool) (*fileapi.Reply, lower.Options, string) {
	t.Helper()
	src := t.TempDir()
	priv := filepath.Join(src, "priv")
	if err := os.MkdirAll(priv, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(priv, "p.h"), []byte("#pragma once\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "a.c"), []byte("#include \"p.h\"\nint f(){return 0;}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := &fileapi.Reply{
		Codemodel: fileapi.Codemodel{
			Paths: fileapi.CodemodelPaths{Source: src},
			Configurations: []fileapi.Configuration{{
				Name:    "Release",
				Targets: []fileapi.ConfigTargetRef{{Name: "foo", Id: "foo::@a"}},
			}},
		},
		Targets: map[string]fileapi.Target{
			"foo::@a": {
				Name:    "foo",
				Type:    "STATIC_LIBRARY",
				Sources: []fileapi.TargetSource{{Path: "a.c", CompileGroupIndex: 0}},
				CompileGroups: []fileapi.CompileGroup{{
					Language:      "C",
					SourceIndexes: []int{0},
					Includes:      []fileapi.CompileInclude{{Path: priv, IsSystem: isSystem}},
				}},
			},
		},
	}
	system := ""
	if isSystem {
		system = `"SYSTEM",`
	}
	trace := []byte(`{"args":["foo",` + system + `"PRIVATE",` + `"` + priv + `"],"cmd":"target_include_directories","file":"` + filepath.Join(src, "CMakeLists.txt") + `","line":3}` + "\n")
	return r, lower.Options{HostSourceRoot: src, TraceRaw: trace}, src
}

// TestSystemPrivateInclude_RoutesToIsystem pins the isSystem consumption:
// target_include_directories(foo SYSTEM PRIVATE priv) lands in compile-
// only copts as -isystem<dir> (not -I<dir>), so cmake's warning-
// suppressing SYSTEM flavour survives. PUBLIC system includes ride
// cc_library.includes, which Bazel already emits as -isystem.
func TestSystemPrivateInclude_RoutesToIsystem(t *testing.T) {
	r, opts, _ := systemIncludeReply(t, true)
	pkg, err := lower.ToIR(r, nil, opts)
	if err != nil {
		t.Fatalf("ToIR: %v", err)
	}
	got := pkg.Targets[0]
	if !containsStr(got.Copts, "-isystempriv") {
		t.Errorf("expected -isystempriv in copts; got %v", got.Copts)
	}
	if containsStr(got.Copts, "-Ipriv") {
		t.Errorf("SYSTEM private include should not emit -Ipriv; got %v", got.Copts)
	}
	if containsStr(got.Includes, "priv") {
		t.Errorf("PRIVATE include should not propagate via includes; got %v", got.Includes)
	}
}

// TestNonSystemPrivateInclude_RoutesToI pins the unchanged path: a plain
// PRIVATE include stays -I<dir>.
func TestNonSystemPrivateInclude_RoutesToI(t *testing.T) {
	r, opts, _ := systemIncludeReply(t, false)
	pkg, err := lower.ToIR(r, nil, opts)
	if err != nil {
		t.Fatalf("ToIR: %v", err)
	}
	got := pkg.Targets[0]
	if !containsStr(got.Copts, "-Ipriv") {
		t.Errorf("expected -Ipriv in copts; got %v", got.Copts)
	}
	if containsStr(got.Copts, "-isystempriv") {
		t.Errorf("non-system private include should not emit -isystem; got %v", got.Copts)
	}
}

// TestPrivateRootInclude_SetsRootInclude pins the abseil spinlock_wait/cctz fix:
// a PRIVATE target_include_directories at the ELEMENT ROOT (rel == "") must set
// RootInclude (→ include_prefix=<package dir> under split, so the target's own
// element-root-relative `#include "absl/..."` resolves) rather than fall into
// the private-include copt branch and emit a bogus bare `-I` (reanchor("")=="")
// while leaving RootInclude false.
func TestPrivateRootInclude_SetsRootInclude(t *testing.T) {
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "h.h"), []byte("#pragma once\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "a.c"), []byte("#include \"h.h\"\nint f(){return 0;}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := &fileapi.Reply{
		Codemodel: fileapi.Codemodel{
			Paths: fileapi.CodemodelPaths{Source: src},
			Configurations: []fileapi.Configuration{{
				Name:    "Release",
				Targets: []fileapi.ConfigTargetRef{{Name: "foo", Id: "foo::@a"}},
			}},
		},
		Targets: map[string]fileapi.Target{
			"foo::@a": {
				Name:    "foo",
				Type:    "STATIC_LIBRARY",
				Sources: []fileapi.TargetSource{{Path: "a.c", CompileGroupIndex: 0}},
				CompileGroups: []fileapi.CompileGroup{{
					Language:      "C",
					SourceIndexes: []int{0},
					Includes:      []fileapi.CompileInclude{{Path: src}}, // the element root
				}},
			},
		},
	}
	// Scope the root include PRIVATE (abseil's spinlock_wait/cctz shape).
	trace := []byte(`{"args":["foo","PRIVATE","` + src + `"],"cmd":"target_include_directories","file":"` + filepath.Join(src, "CMakeLists.txt") + `","line":3}` + "\n")
	pkg, err := lower.ToIR(r, nil, lower.Options{HostSourceRoot: src, TraceRaw: trace})
	if err != nil {
		t.Fatalf("ToIR: %v", err)
	}
	got := pkg.Targets[0]
	if !got.RootInclude {
		t.Errorf("PRIVATE root include should set RootInclude=true; got false (copts=%v)", got.Copts)
	}
	if containsStr(got.Copts, "-I") {
		t.Errorf("PRIVATE root include must not emit a bogus bare -I copt; got %v", got.Copts)
	}
	if containsStr(got.Includes, "") {
		t.Errorf("root include must be dropped from includes (Bazel rejects [\"\"]); got %v", got.Includes)
	}
}

func containsStr(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}
