package lower

import (
	"reflect"
	"testing"

	"github.com/sstriker/buildstream-bazel/internal/genexeval"
	"github.com/sstriker/buildstream-bazel/internal/shadow"
)

func TestBuildIncludeDirsGlobal_ResolvesRelativeAgainstCallDir(t *testing.T) {
	calls := []shadow.IncludeDirectoriesCall{
		{File: "/src/lapacke/CMakeLists.txt", Dirs: []string{"include", "/build/gen/include"}},
		{File: "/src/CMakeLists.txt", Dirs: []string{"common/inc"}},
	}
	got := buildIncludeDirsGlobal(calls)
	want := map[string]bool{
		"/src/lapacke/include": true, // relative → resolved against the call's dir
		"/build/gen/include":   true, // absolute → kept
		"/src/common/inc":      true,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v; want %v", got, want)
	}
}

func TestBuildPublicIncludeDirs_OnlyPublicInterface(t *testing.T) {
	calls := []shadow.TargetIncludeCall{{
		Target: "lapacke",
		Groups: []shadow.TargetIncludeGroup{
			{Visibility: "PUBLIC", Dirs: []string{"/src/lapacke/include"}},
			{Visibility: "PRIVATE", Dirs: []string{"/src/lapacke/private"}},
			{Visibility: "INTERFACE", Dirs: []string{"/src/lapacke/iface"}},
		},
	}}
	got := buildPublicIncludeDirs(calls)["lapacke"]
	want := map[string]bool{"/src/lapacke/include": true, "/src/lapacke/iface": true}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("public dirs = %v; want %v (PRIVATE excluded)", got, want)
	}
}

func TestParseInterfaceIncludeDirs(t *testing.T) {
	// BUILD_INTERFACE absolute → kept; INSTALL_INTERFACE → skipped; bare abs
	// kept; unresolved non-BUILD genex skipped.
	in := "$<BUILD_INTERFACE:/src/lapacke/include>;$<INSTALL_INTERFACE:include>;" +
		"/abs/extra/inc;$<SOMETHING:/x>;$<BUILD_INTERFACE:$<CONFIG>/gen>"
	got := parseInterfaceIncludeDirs(in)
	want := map[string]bool{"/src/lapacke/include": true, "/abs/extra/inc": true}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v; want %v", got, want)
	}
}

func TestInterfaceIncludeDirsFor_GatedOnProbe(t *testing.T) {
	genex := map[string]genexeval.TargetInfo{
		"lapacke": {InterfaceIncludeDirectories: "$<BUILD_INTERFACE:/src/lapacke/include>"},
	}
	// No probe → inactive (byte-identical fallback).
	if s := interfaceIncludeDirsFor(genex, "lapacke", false); s.active {
		t.Errorf("expected inactive without probe")
	}
	// Probe + captured → active with the parsed public set.
	s := interfaceIncludeDirsFor(genex, "lapacke", true)
	if !s.active || !s.public["/src/lapacke/include"] {
		t.Errorf("active=%v public=%v; want active with /src/lapacke/include", s.active, s.public)
	}
	// Probe but target absent → inactive.
	if s := interfaceIncludeDirsFor(genex, "missing", true); s.active {
		t.Errorf("expected inactive for un-probed target")
	}
}
