package lower

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/sstriker/buildstream-bazel/internal/shadow"
)

// TestResolveTemplatePath_UmbrellaReanchor pins the umbrella re-anchor
// (the configure_file twin of executeProcessAnchorSource's): when the
// label root sits ABOVE the cmake source dir (workspace promotion,
// --element-source-root overlays, the nested-cmake recursive lowering),
// the host-real read path carries the hostSrcDir→recordedSrcDir prefix
// and the emitted template label is label-root-relative — without it
// the lift's template read misses and the recovery silently bakes.
func TestResolveTemplatePath_UmbrellaReanchor(t *testing.T) {
	root := t.TempDir() // the LABEL root (outer)
	nested := filepath.Join(root, "sub")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	tmpl := filepath.Join(nested, "cfg.h.in")
	if err := os.WriteFile(tmpl, []byte("#define V @V@\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	call := shadow.ConfigureFileCall{CallFile: filepath.Join(nested, "CMakeLists.txt")}

	hostPath, rel, ok := resolveTemplatePath(tmpl, root, nested, call, nil)
	if !ok {
		t.Fatal("umbrella-shaped template did not resolve")
	}
	if rel != "sub/cfg.h.in" {
		t.Errorf("label rel = %q, want label-root-relative sub/cfg.h.in", rel)
	}
	if hostPath != tmpl {
		t.Errorf("host path = %q, want the on-disk template %q", hostPath, tmpl)
	}

	// Non-umbrella (hostSrcDir == recordedSrcDir): unchanged behavior.
	hostPath, rel, ok = resolveTemplatePath(tmpl, nested, nested, call, nil)
	if !ok || rel != "cfg.h.in" || hostPath != tmpl {
		t.Errorf("non-umbrella = (%q, %q, %v), want (%q, cfg.h.in, true)", hostPath, rel, ok, tmpl)
	}

	// Offline replay (hostSrcDir is another machine's path, not an
	// ancestor): keeps the recorded-relative form.
	_, rel, ok = resolveTemplatePath(tmpl, "/recorded/elsewhere", nested, call, nil)
	if !ok || rel != "cfg.h.in" {
		t.Errorf("offline rel = (%q, %v), want (cfg.h.in, true)", rel, ok)
	}
}
