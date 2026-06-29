package shadow

import (
	"strings"
	"testing"
)

// TestExtractFileWriterCalls pins the writer decode: WRITE/APPEND carry
// concatenated expanded content, TOUCH lists every output, COPY expands
// DESTINATION outputs per source, COPY_FILE and DOWNLOAD carry their
// pairs, filtering/unmodeled variants decline, and out-of-tree call
// sites are gated.
func TestExtractFileWriterCalls(t *testing.T) {
	trace := strings.Join([]string{
		`{"cmd":"file","args":["WRITE","/b/gen.c","int a;","int b;"],"file":"/src/CMakeLists.txt","line":3}`,
		`{"cmd":"file","args":["APPEND","/b/gen.c","int c;"],"file":"/src/CMakeLists.txt","line":4}`,
		`{"cmd":"file","args":["TOUCH","/b/x.stamp","/b/y.stamp"],"file":"/src/CMakeLists.txt","line":5}`,
		`{"cmd":"file","args":["COPY","/src/a.c","/src/b.c","DESTINATION","/b/copied"],"file":"/src/CMakeLists.txt","line":6}`,
		`{"cmd":"file","args":["COPY_FILE","/src/c.c","/b/c2.c"],"file":"/src/CMakeLists.txt","line":7}`,
		`{"cmd":"file","args":["DOWNLOAD","https://x/y.h","/b/dl.h","STATUS","_s"],"file":"/src/CMakeLists.txt","line":8}`,
		// declines:
		`{"cmd":"file","args":["COPY","/src/dir","DESTINATION","/b/d","FILES_MATCHING","PATTERN","*.h"],"file":"/src/CMakeLists.txt","line":9}`,
		`{"cmd":"file","args":["DOWNLOAD","https://x/probe"],"file":"/src/CMakeLists.txt","line":10}`,
		`{"cmd":"file","args":["GLOB","v","*.c"],"file":"/src/CMakeLists.txt","line":11}`,
		// out of tree, no project I/O — dropped (a module's own bookkeeping):
		`{"cmd":"file","args":["WRITE","/usr/share/cmake/Modules/scratch.txt","x"],"file":"/usr/share/cmake/Modules/M.cmake","line":1}`,
	}, "\n")

	calls := ExtractFileWriterCalls([]byte(trace), "/src", "/b")
	if len(calls) != 6 {
		t.Fatalf("expected 6 writer calls, got %d: %+v", len(calls), calls)
	}
	if calls[0].Op != "write" || calls[0].Content != "int a;int b;" || calls[0].Outputs[0] != "/b/gen.c" {
		t.Errorf("WRITE: %+v", calls[0])
	}
	if calls[1].Op != "append" || calls[1].Content != "int c;" {
		t.Errorf("APPEND: %+v", calls[1])
	}
	if calls[2].Op != "touch" || len(calls[2].Outputs) != 2 {
		t.Errorf("TOUCH: %+v", calls[2])
	}
	if calls[3].Op != "copy" || len(calls[3].Outputs) != 2 ||
		calls[3].Outputs[0] != "/b/copied/a.c" || calls[3].Outputs[1] != "/b/copied/b.c" {
		t.Errorf("COPY: %+v", calls[3])
	}
	if calls[4].Op != "copy_file" || calls[4].Sources[0] != "/src/c.c" || calls[4].Outputs[0] != "/b/c2.c" {
		t.Errorf("COPY_FILE: %+v", calls[4])
	}
	if calls[5].Op != "download" || calls[5].URL != "https://x/y.h" || calls[5].Outputs[0] != "/b/dl.h" {
		t.Errorf("DOWNLOAD: %+v", calls[5])
	}
}

// TestExtractFileWriterCalls_CopyPermissionsNotSources pins the
// hardening fix: a file(COPY) carrying FILE_PERMISSIONS /
// DIRECTORY_PERMISSIONS still lifts as a straight copy (permissions
// don't change content), and the mode tokens (OWNER_READ, …) — which
// follow DESTINATION — must NOT be mistaken for source files (they'd
// produce bogus <dest>/OWNER_READ outputs).
func TestExtractFileWriterCalls_CopyPermissionsNotSources(t *testing.T) {
	trace := `{"cmd":"file","args":["COPY","/src/a.c","DESTINATION","/b/out","FILE_PERMISSIONS","OWNER_READ","OWNER_WRITE","GROUP_READ","DIRECTORY_PERMISSIONS","OWNER_READ","OWNER_EXECUTE","USE_SOURCE_PERMISSIONS"],"file":"/src/CMakeLists.txt","line":3}`
	calls := ExtractFileWriterCalls([]byte(trace), "/src", "/b")
	if len(calls) != 1 {
		t.Fatalf("expected 1 copy call, got %d: %+v", len(calls), calls)
	}
	c := calls[0]
	if c.Op != "copy" {
		t.Fatalf("op = %q; want copy", c.Op)
	}
	if len(c.Sources) != 1 || c.Sources[0] != "/src/a.c" {
		t.Errorf("sources = %v; want [/src/a.c] (permission mode tokens leaked in)", c.Sources)
	}
	if len(c.Outputs) != 1 || c.Outputs[0] != "/b/out/a.c" {
		t.Errorf("outputs = %v; want [/b/out/a.c] (bogus <dest>/OWNER_READ etc.)", c.Outputs)
	}
}

// TestExtractFileWriterCalls_DownloadHash pins EXPECTED_HASH / EXPECTED_MD5
// capture on file(DOWNLOAD).
func TestExtractFileWriterCalls_DownloadHash(t *testing.T) {
	trace := `{"cmd":"file","args":["DOWNLOAD","https://x/y.h","/b/y.h","EXPECTED_HASH","SHA256=abc","STATUS","_s"],"file":"/src/CMakeLists.txt","line":3}`
	calls := ExtractFileWriterCalls([]byte(trace), "/src", "/b")
	if len(calls) != 1 || calls[0].Op != "download" || calls[0].Hash != "SHA256=abc" {
		t.Fatalf("download hash: %+v", calls)
	}
	md5 := `{"cmd":"file","args":["DOWNLOAD","https://x/z","/b/z","EXPECTED_MD5","ff"],"file":"/src/CMakeLists.txt","line":4}`
	calls = ExtractFileWriterCalls([]byte(md5), "/src", "/b")
	if len(calls) != 1 || calls[0].Hash != "MD5=ff" {
		t.Fatalf("md5 hash: %+v", calls)
	}
}

// TestExtractFileWriterCalls_BuildTreeIssued pins finding [13]: a writer
// ISSUED from a generated+include()d recipe `.cmake` under the build tree is
// project intent (inProjectScope), not cmake bookkeeping — so a build-tree
// file(COPY) is recovered as a regenerating cp-genrule instead of dropping to
// a frozen on-disk bake. cmake's own try_compile scratch under
// <build>/CMakeFiles stays filtered.
func TestExtractFileWriterCalls_BuildTreeIssued(t *testing.T) {
	trace := strings.Join([]string{
		// build-tree recipe: kept (the [13] gap).
		`{"cmd":"file","args":["COPY","/src/in.h","DESTINATION","/b/gen"],"file":"/b/recipe.cmake","line":2}`,
		// try_compile scratch under <build>/CMakeFiles: still filtered.
		`{"cmd":"file","args":["WRITE","/b/CMakeFiles/scratch.c","x"],"file":"/b/CMakeFiles/TryCompile/CMakeLists.txt","line":1}`,
	}, "\n")
	calls := ExtractFileWriterCalls([]byte(trace), "/src", "/b")
	if len(calls) != 1 {
		t.Fatalf("expected 1 build-tree writer (the recipe COPY), got %d: %+v", len(calls), calls)
	}
	if calls[0].Op != "copy" || calls[0].Outputs[0] != "/b/gen/in.h" {
		t.Errorf("build-tree COPY: %+v", calls[0])
	}
	// Without a build root, the strict source-tree gate drops both — the
	// prior (source-tree-only) behavior.
	if got := ExtractFileWriterCalls([]byte(trace), "/src", ""); len(got) != 0 {
		t.Errorf("buildRoot=\"\" should keep source-tree-only behavior; got %d: %+v", len(got), got)
	}
}

// TestExtractFileWriterCalls_OutOfTreeProjectIORescue pins finding [14]: a
// writer ISSUED from a cmake module OUTSIDE both the source and build trees
// (CMAKE_MODULE_PATH / a find_package prefix) but that TOUCHES the project's
// own I/O — it writes a build-dir output, or copies an in-source-tree file —
// is rescued rather than silently dropped (the projectIO signal, mirroring
// out-of-tree execute_process). A module writer with NO project I/O stays
// dropped.
func TestExtractFileWriterCalls_OutOfTreeProjectIORescue(t *testing.T) {
	trace := strings.Join([]string{
		// out-of-tree module writing a build-dir output: RESCUED.
		`{"cmd":"file","args":["WRITE","/b/gen/cfg.h","#define X 1"],"file":"/opt/cmake-mods/Helper.cmake","line":7}`,
		// out-of-tree module copying an in-source-tree file: RESCUED.
		`{"cmd":"file","args":["COPY_FILE","/src/tpl.h","/b/gen/tpl.h"],"file":"/opt/cmake-mods/Helper.cmake","line":8}`,
		// out-of-tree module touching neither tree: DROPPED (its own bookkeeping).
		`{"cmd":"file","args":["WRITE","/opt/cmake-mods/state.txt","y"],"file":"/opt/cmake-mods/Helper.cmake","line":9}`,
	}, "\n")
	calls := ExtractFileWriterCalls([]byte(trace), "/src", "/b")
	if len(calls) != 2 {
		t.Fatalf("expected 2 rescued out-of-tree writers, got %d: %+v", len(calls), calls)
	}
	if calls[0].Op != "write" || calls[0].Outputs[0] != "/b/gen/cfg.h" {
		t.Errorf("rescued build-dir WRITE: %+v", calls[0])
	}
	if calls[1].Op != "copy_file" || calls[1].Sources[0] != "/src/tpl.h" {
		t.Errorf("rescued in-source COPY_FILE: %+v", calls[1])
	}
}
