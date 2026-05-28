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
