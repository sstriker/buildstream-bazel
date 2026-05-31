package fileapi_test

import (
	"encoding/json"
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/internal/fileapi"
)

// TestCMakeFiles_GlobsDependent pins the cmakeFiles-v1.1 globsDependent
// schema (cmake 3.29+). The JSON mirrors a real
// file(GLOB ... CONFIGURE_DEPENDS) record; cmake 3.28 (the survey pin)
// doesn't emit this field, so the literal is the contract.
func TestCMakeFiles_GlobsDependent(t *testing.T) {
	const in = `{
	  "kind": "cmakeFiles",
	  "version": {"major": 1, "minor": 1},
	  "paths": {"source": "/src", "build": "/build"},
	  "inputs": [{"path": "CMakeLists.txt"}],
	  "globsDependent": [
	    {
	      "expression": "/src/lib/*.c",
	      "recurse": false,
	      "paths": ["/src/lib/a.c", "/src/lib/b.c"]
	    }
	  ]
	}`
	var cf fileapi.CMakeFiles
	if err := json.Unmarshal([]byte(in), &cf); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(cf.GlobsDependent) != 1 {
		t.Fatalf("globsDependent len = %d, want 1", len(cf.GlobsDependent))
	}
	g := cf.GlobsDependent[0]
	if g.Expression != "/src/lib/*.c" {
		t.Errorf("expression = %q", g.Expression)
	}
	if want := []string{"/src/lib/a.c", "/src/lib/b.c"}; len(g.Paths) != 2 || g.Paths[0] != want[0] || g.Paths[1] != want[1] {
		t.Errorf("paths = %v, want %v", g.Paths, want)
	}
}

// TestDirectoryInstaller_ScriptAndNamelink pins the script / code /
// versioned-shared-lib namelink installer fields against the exact JSON
// cmake 3.28 emits (captured from a live reply).
func TestDirectoryInstaller_ScriptAndNamelink(t *testing.T) {
	const in = `{
	  "backtraceGraph": {
	    "commands": ["install"],
	    "files": ["CMakeLists.txt"],
	    "nodes": [{"file": 0}, {"command": 0, "file": 0, "line": 7}]
	  },
	  "installers": [
	    {"backtrace": 1, "destination": "lib", "paths": ["libshar.so.1.2.3", "libshar.so.1"],
	     "targetId": "shar::@x", "targetIndex": 1, "targetInstallNamelink": "skip", "type": "target"},
	    {"backtrace": 1, "destination": "lib", "paths": ["libshar.so"],
	     "targetId": "shar::@x", "targetIndex": 1, "targetInstallNamelink": "only", "type": "target"},
	    {"backtrace": 2, "scriptFile": "post.cmake", "type": "script"},
	    {"backtrace": 3, "type": "code"}
	  ],
	  "paths": {"source": ".", "build": "."}
	}`
	var d fileapi.Directory
	if err := json.Unmarshal([]byte(in), &d); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(d.BacktraceGraph.Nodes) != 2 {
		t.Errorf("backtraceGraph nodes = %d, want 2", len(d.BacktraceGraph.Nodes))
	}
	if got := d.Installers[0].TargetInstallNamelink; got != "skip" {
		t.Errorf("installer[0] namelink = %q, want skip", got)
	}
	if got := d.Installers[1].TargetInstallNamelink; got != "only" {
		t.Errorf("installer[1] namelink = %q, want only", got)
	}
	if got := d.Installers[2].ScriptFile; got != "post.cmake" {
		t.Errorf("installer[2] scriptFile = %q, want post.cmake", got)
	}
	if got := d.Installers[2].Backtrace; got != 2 {
		t.Errorf("installer[2] backtrace = %d, want 2", got)
	}
	if got := d.Installers[3].Type; got != "code" {
		t.Errorf("installer[3] type = %q, want code", got)
	}
}

// TestTarget_Launchers pins the codemodel-v2 minor-7 launchers schema
// (cmake 3.29+). cmake 3.28 (codemodel minor 6) doesn't emit it, so the
// literal is the contract.
func TestTarget_Launchers(t *testing.T) {
	const in = `{
	  "name": "emu", "id": "emu::@1", "type": "EXECUTABLE",
	  "launchers": [
	    {"command": "/usr/bin/qemu-arm", "arguments": ["-L", "/sysroot"], "type": "emulator"},
	    {"command": "/usr/bin/valgrind", "type": "test"}
	  ],
	  "backtraceGraph": {}
	}`
	var tg fileapi.Target
	if err := json.Unmarshal([]byte(in), &tg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(tg.Launchers) != 2 {
		t.Fatalf("launchers len = %d, want 2", len(tg.Launchers))
	}
	if tg.Launchers[0].Command != "/usr/bin/qemu-arm" || tg.Launchers[0].Type != "emulator" {
		t.Errorf("launcher[0] = %+v", tg.Launchers[0])
	}
	if want := []string{"-L", "/sysroot"}; len(tg.Launchers[0].Arguments) != 2 || tg.Launchers[0].Arguments[1] != want[1] {
		t.Errorf("launcher[0] arguments = %v, want %v", tg.Launchers[0].Arguments, want)
	}
	if tg.Launchers[1].Type != "test" {
		t.Errorf("launcher[1] type = %q, want test", tg.Launchers[1].Type)
	}
}
