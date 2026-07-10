package main

import (
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/internal/fileapi"
)

func TestCmakeTruthy(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want bool
	}{
		{"ON", true}, {"on", true}, {"1", true}, {"TRUE", true}, {"YES", true},
		{"OFF", false}, {"0", false}, {"FALSE", false}, {"NO", false}, {"N", false},
		{"", false}, {"IGNORE", false}, {"NOTFOUND", false}, {"ZLIB-NOTFOUND", false},
		{"y", true}, {"anything", true},
	} {
		if got := cmakeTruthy(tc.in); got != tc.want {
			t.Errorf("cmakeTruthy(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestCanonicalizeFlipTarget_RewritesScratchPaths(t *testing.T) {
	tgt := fileapi.Target{
		Name:    "foo",
		Sources: []fileapi.TargetSource{{Path: "/b/scratch-opt-FOO/gen.c"}},
		CompileGroups: []fileapi.CompileGroup{{
			Language:                "C",
			Includes:                []fileapi.CompileInclude{{Path: "/b/scratch-opt-FOO/include"}},
			Defines:                 []fileapi.CompileDefine{{Define: "DIR=/b/scratch-opt-FOO"}},
			CompileCommandFragments: []fileapi.CommandFragment{{Fragment: "-I/b/scratch-opt-FOO/x"}},
		}},
		Link: &fileapi.TargetLink{
			CommandFragments: []fileapi.CommandFragment{{Fragment: "/b/scratch-opt-FOO/libz.a", Role: "libraries"}},
		},
	}
	canonicalizeFlipTarget(&tgt, "/b/scratch-opt-FOO", "/b/host")
	if got := tgt.Sources[0].Path; got != "/b/host/gen.c" {
		t.Errorf("source path: %q", got)
	}
	if got := tgt.CompileGroups[0].Includes[0].Path; got != "/b/host/include" {
		t.Errorf("include path: %q", got)
	}
	if got := tgt.CompileGroups[0].Defines[0].Define; got != "DIR=/b/host" {
		t.Errorf("define: %q", got)
	}
	if got := tgt.CompileGroups[0].CompileCommandFragments[0].Fragment; got != "-I/b/host/x" {
		t.Errorf("compile fragment: %q", got)
	}
	if got := tgt.Link.CommandFragments[0].Fragment; got != "/b/host/libz.a" {
		t.Errorf("link fragment: %q", got)
	}
}

func TestConfigTargetNameSetAndEquality(t *testing.T) {
	cfgs := []fileapi.Configuration{{
		Name: "Release",
		Targets: []fileapi.ConfigTargetRef{
			{Id: "a::@1", Name: "a"}, {Id: "b::@2", Name: "b"},
		},
	}}
	got := configTargetNameSet(cfgs)
	if len(got) != 2 || !got["a"] || !got["b"] {
		t.Fatalf("name set: %v", got)
	}
	if !stringSetsEqual(got, map[string]bool{"a": true, "b": true}) {
		t.Errorf("sets should be equal")
	}
	if stringSetsEqual(got, map[string]bool{"a": true}) || stringSetsEqual(got, map[string]bool{"a": true, "c": true}) {
		t.Errorf("unequal sets reported equal")
	}
}
