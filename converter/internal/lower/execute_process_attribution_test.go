package lower_test

import (
	"strings"
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/internal/fileapi"
	"github.com/sstriker/buildstream-bazel/converter/internal/lower"
)

// TestExecuteProcess_ConsumerAttribution_HeaderUnderBuildInclude
// asserts that a recovered execute_process output that lands
// under a target's codemodel-recorded build-dir include path
// surfaces on that target's hdrs (when it's a header) AND
// flips the has-cmake-codegen tag on. Without this, the
// recovered genrule lives on cc.Genrules but no consumer
// references it, so Bazel's sandbox doesn't stage the
// generated header for the consumer's compile.
//
// Mirrors how configure_file consumer attribution already works
// (lower.go's configureFiles attribution block). The shape is
// the round-2-style cmake -E touch into the build dir; a
// target_include_directories that names ${CMAKE_CURRENT_BINARY_DIR}
// surfaces the build path on CompileGroups[0].Includes; the
// attribution loop matches the recovered output against that
// include and attaches it.
func TestExecuteProcess_ConsumerAttribution_HeaderUnderBuildInclude(t *testing.T) {
	r := &fileapi.Reply{
		Codemodel: fileapi.Codemodel{
			Paths: fileapi.CodemodelPaths{Source: "/src", Build: "/build"},
			Configurations: []fileapi.Configuration{{
				Name:    "Release",
				Targets: []fileapi.ConfigTargetRef{{Name: "thelib", Id: "thelib::@1"}},
			}},
		},
		Targets: map[string]fileapi.Target{
			"thelib::@1": {
				Name: "thelib",
				Type: "STATIC_LIBRARY",
				Sources: []fileapi.TargetSource{
					{Path: "src/lib.c", CompileGroupIndex: 0},
				},
				CompileGroups: []fileapi.CompileGroup{{
					Language: "C",
					Includes: []fileapi.CompileInclude{{Path: "/build"}},
				}},
			},
		},
	}
	traceRaw := []byte(
		`{"args":["COMMAND","cmake","-E","touch","/build/generated.h"],"cmd":"execute_process","file":"/src/CMakeLists.txt","line":3}` + "\n",
	)
	pkg, err := lower.ToIR(r, nil, lower.Options{
		HostSourceRoot: "/src",
		BuildDir:       "/build",
		TraceRaw:       traceRaw,
	})
	if err != nil {
		t.Fatalf("ToIR: %v", err)
	}
	var lib *struct {
		Hdrs []string
		Srcs []string
		Tags []string
	}
	for _, tgt := range pkg.Targets {
		if tgt.Name == "thelib" {
			lib = &struct {
				Hdrs []string
				Srcs []string
				Tags []string
			}{Hdrs: tgt.Hdrs, Srcs: tgt.Srcs, Tags: tgt.Tags}
			break
		}
	}
	if lib == nil {
		t.Fatalf("thelib not in pkg.Targets")
	}
	if !contains(lib.Hdrs, "generated.h") {
		t.Errorf("expected generated.h in hdrs (header attribution); got %v", lib.Hdrs)
	}
	if !contains(lib.Tags, "has-cmake-codegen") {
		t.Errorf("expected has-cmake-codegen tag on the consumer; got tags=%v", lib.Tags)
	}
}

// TestExecuteProcess_ConsumerAttribution_NonHeaderUnderBuildInclude
// is the sister case: a non-header recovered output (e.g.
// cmake -E touch /build/marker.dat) goes to srcs rather than
// hdrs. The attribution loop branches on extension via the
// shared headerExts table.
func TestExecuteProcess_ConsumerAttribution_NonHeaderUnderBuildInclude(t *testing.T) {
	r := &fileapi.Reply{
		Codemodel: fileapi.Codemodel{
			Paths: fileapi.CodemodelPaths{Source: "/src", Build: "/build"},
			Configurations: []fileapi.Configuration{{
				Name:    "Release",
				Targets: []fileapi.ConfigTargetRef{{Name: "thelib", Id: "thelib::@1"}},
			}},
		},
		Targets: map[string]fileapi.Target{
			"thelib::@1": {
				Name: "thelib",
				Type: "STATIC_LIBRARY",
				Sources: []fileapi.TargetSource{
					{Path: "src/lib.c", CompileGroupIndex: 0},
				},
				CompileGroups: []fileapi.CompileGroup{{
					Language: "C",
					Includes: []fileapi.CompileInclude{{Path: "/build"}},
				}},
			},
		},
	}
	traceRaw := []byte(
		`{"args":["COMMAND","cmake","-E","touch","/build/marker.dat"],"cmd":"execute_process","file":"/src/CMakeLists.txt","line":3}` + "\n",
	)
	pkg, err := lower.ToIR(r, nil, lower.Options{
		HostSourceRoot: "/src",
		BuildDir:       "/build",
		TraceRaw:       traceRaw,
	})
	if err != nil {
		t.Fatalf("ToIR: %v", err)
	}
	for _, tgt := range pkg.Targets {
		if tgt.Name != "thelib" {
			continue
		}
		if !contains(tgt.Srcs, "marker.dat") {
			t.Errorf("expected marker.dat in srcs (non-header attribution); got srcs=%v", tgt.Srcs)
		}
		if contains(tgt.Hdrs, "marker.dat") {
			t.Errorf("non-header marker.dat should not land in hdrs; got hdrs=%v", tgt.Hdrs)
		}
		if !contains(tgt.Tags, "has-cmake-codegen") {
			t.Errorf("expected has-cmake-codegen tag; got tags=%v", tgt.Tags)
		}
		return
	}
	t.Fatalf("thelib not in pkg.Targets: %+v", pkg.Targets)
}

// TestExecuteProcess_ConsumerAttribution_NoMatchNoTag asserts
// the negative path: a target whose codemodel-recorded
// includes don't cover the recovered build path doesn't pick
// up the output and doesn't get the has-cmake-codegen tag.
// Guards against the attribution block over-attaching outputs
// to unrelated targets just because the genrule exists.
func TestExecuteProcess_ConsumerAttribution_NoMatchNoTag(t *testing.T) {
	r := &fileapi.Reply{
		Codemodel: fileapi.Codemodel{
			Paths: fileapi.CodemodelPaths{Source: "/src", Build: "/build"},
			Configurations: []fileapi.Configuration{{
				Name:    "Release",
				Targets: []fileapi.ConfigTargetRef{{Name: "thelib", Id: "thelib::@1"}},
			}},
		},
		Targets: map[string]fileapi.Target{
			"thelib::@1": {
				Name: "thelib",
				Type: "STATIC_LIBRARY",
				Sources: []fileapi.TargetSource{
					{Path: "src/lib.c", CompileGroupIndex: 0},
				},
				CompileGroups: []fileapi.CompileGroup{{
					Language: "C",
					// No build-dir entry on Includes; the
					// attribution loop has nothing to match
					// against. Source-tree includes are intentionally
					// empty too — the production header-walk pass
					// would lstat each, and an offline test with
					// synthetic /src paths would crash on missing
					// directories.
					Includes: nil,
				}},
			},
		},
	}
	traceRaw := []byte(
		`{"args":["COMMAND","cmake","-E","touch","/build/generated.h"],"cmd":"execute_process","file":"/src/CMakeLists.txt","line":3}` + "\n",
	)
	pkg, err := lower.ToIR(r, nil, lower.Options{
		HostSourceRoot: "/src",
		BuildDir:       "/build",
		TraceRaw:       traceRaw,
	})
	if err != nil {
		t.Fatalf("ToIR: %v", err)
	}
	for _, tgt := range pkg.Targets {
		if tgt.Name != "thelib" {
			continue
		}
		if contains(tgt.Hdrs, "generated.h") || contains(tgt.Srcs, "generated.h") {
			t.Errorf("target without build-dir include should NOT pick up generated.h; got hdrs=%v srcs=%v", tgt.Hdrs, tgt.Srcs)
		}
		for _, tg := range tgt.Tags {
			if strings.HasPrefix(tg, "has-cmake-codegen") {
				t.Errorf("target without build-dir include should NOT get has-cmake-codegen tag; got tags=%v", tgt.Tags)
			}
		}
		return
	}
	t.Fatalf("thelib not in pkg.Targets: %+v", pkg.Targets)
}
