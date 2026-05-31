package lower

import (
	"bytes"
	"strings"
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/internal/fileapi"
)

func TestSurfaceInstallScriptInstallers_BothShapes(t *testing.T) {
	r := &fileapi.Reply{
		Directories: map[string]fileapi.Directory{
			"dir-a": {Installers: []fileapi.DirectoryInstaller{
				{Type: "script"},
				{Type: "code"},
				{Type: "file"}, // not surfaced
			}},
			"dir-b": {Installers: []fileapi.DirectoryInstaller{
				{Type: "script"},
			}},
		},
	}
	var buf bytes.Buffer
	surfaceInstallScriptInstallers(r, &buf)
	got := buf.String()
	if !strings.Contains(got, "2 install(SCRIPT)") {
		t.Errorf("expected '2 install(SCRIPT)' count; got:\n%s", got)
	}
	if !strings.Contains(got, "1 install(CODE)") {
		t.Errorf("expected '1 install(CODE)' count; got:\n%s", got)
	}
}

func TestSurfaceInstallScriptInstallers_NoScripts_NoOp(t *testing.T) {
	r := &fileapi.Reply{
		Directories: map[string]fileapi.Directory{
			"d": {Installers: []fileapi.DirectoryInstaller{
				{Type: "file"},
				{Type: "target"},
			}},
		},
	}
	var buf bytes.Buffer
	surfaceInstallScriptInstallers(r, &buf)
	if buf.Len() != 0 {
		t.Errorf("expected no output; got %q", buf.String())
	}
}

func TestSurfaceInstallScriptInstallers_NilSink_NoOp(t *testing.T) {
	r := &fileapi.Reply{
		Directories: map[string]fileapi.Directory{
			"d": {Installers: []fileapi.DirectoryInstaller{{Type: "script"}}},
		},
	}
	// Should not panic.
	surfaceInstallScriptInstallers(r, nil)
}

func TestSurfaceInstallScriptInstallers_NilReply_NoOp(t *testing.T) {
	var buf bytes.Buffer
	surfaceInstallScriptInstallers(nil, &buf)
	if buf.Len() != 0 {
		t.Errorf("expected no output for nil reply; got %q", buf.String())
	}
}

// TestSurfaceInstallScriptInstallers_NamesScriptAndSite checks that the
// warning resolves the install() backtrace to a file:line and names the
// install(SCRIPT) script file, so operators can locate the dropped
// directive rather than just see a count.
func TestSurfaceInstallScriptInstallers_NamesScriptAndSite(t *testing.T) {
	// BacktraceGraph: node 1 -> CMakeLists.txt:7 (install command).
	bg := fileapi.BacktraceGraph{
		Commands: []string{"install"},
		Files:    []string{"CMakeLists.txt"},
		Nodes: []fileapi.BacktraceNode{
			{File: 0},                      // node 0: root
			{File: 0, Line: 7, Command: 0}, // node 1: install() at :7
		},
	}
	r := &fileapi.Reply{
		Directories: map[string]fileapi.Directory{
			"dir-a": {
				BacktraceGraph: bg,
				Installers: []fileapi.DirectoryInstaller{
					{Type: "script", ScriptFile: "post.cmake", Backtrace: 1},
				},
			},
		},
	}
	var buf bytes.Buffer
	surfaceInstallScriptInstallers(r, &buf)
	got := buf.String()
	if !strings.Contains(got, "install(SCRIPT post.cmake) at CMakeLists.txt:7") {
		t.Errorf("expected script file + site in warning; got:\n%s", got)
	}
}

// TestSurfaceInstallScriptInstallers_NoBacktrace_OmitsSite verifies the
// warning degrades cleanly when the reply carries no usable backtrace
// (older cmake): the directive is still listed, just without a site.
func TestSurfaceInstallScriptInstallers_NoBacktrace_OmitsSite(t *testing.T) {
	r := &fileapi.Reply{
		Directories: map[string]fileapi.Directory{
			"d": {Installers: []fileapi.DirectoryInstaller{{Type: "code"}}},
		},
	}
	var buf bytes.Buffer
	surfaceInstallScriptInstallers(r, &buf)
	got := buf.String()
	if !strings.Contains(got, "install(CODE)") {
		t.Errorf("expected install(CODE) line; got:\n%s", got)
	}
	if strings.Contains(got, " at ") {
		t.Errorf("expected no site suffix when backtrace absent; got:\n%s", got)
	}
}

func TestSurfaceLauncherTargets(t *testing.T) {
	r := &fileapi.Reply{
		Targets: map[string]fileapi.Target{
			"emu::@1": {Name: "emu", Launchers: []fileapi.TargetLauncher{
				{Command: "/usr/bin/qemu-arm", Arguments: []string{"-L", "/sysroot"}, Type: "emulator"},
			}},
			"plain::@2": {Name: "plain"}, // no launcher
		},
	}
	var buf bytes.Buffer
	surfaceLauncherTargets(r, &buf)
	got := buf.String()
	if !strings.Contains(got, "1 target launcher(s)") {
		t.Errorf("expected launcher count; got:\n%s", got)
	}
	if !strings.Contains(got, `emu: emulator launcher "/usr/bin/qemu-arm"`) {
		t.Errorf("expected named emulator launcher; got:\n%s", got)
	}
}

func TestSurfaceLauncherTargets_None_NoOp(t *testing.T) {
	r := &fileapi.Reply{Targets: map[string]fileapi.Target{"x::@1": {Name: "x"}}}
	var buf bytes.Buffer
	surfaceLauncherTargets(r, &buf)
	if buf.Len() != 0 {
		t.Errorf("expected no output; got %q", buf.String())
	}
	surfaceLauncherTargets(r, nil) // must not panic
}
