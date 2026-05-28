package lower

import (
	"reflect"
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/ir"
)

func TestBuildArtifactToLabelMap(t *testing.T) {
	targets := []ir.Target{
		{Name: "WrapHierarchy", Kind: ir.KindCCBinary, ArtifactName: "vtkWrapHierarchy-9.3"},
		{Name: "vtkH5detect", Kind: ir.KindCCBinary},                         // ArtifactName empty → fall back to Name
		{Name: "libcore", Kind: ir.KindCCLibrary, ArtifactName: "libcore.a"}, // libraries don't contribute
		{Name: "gen_thing", Kind: ir.KindGenrule},                            // genrules don't contribute
		{Name: "nameless_artifact", Kind: ir.KindCCBinary, ArtifactName: ""}, // falls back to Name
	}
	got := buildArtifactToLabelMap(targets)
	// Each cc_binary contributes the target Name + the
	// ArtifactName (with basename if rooted). Aliasing the
	// target name and the artifact basename means tokens
	// referencing either form (e.g. `WrapHierarchy` from a
	// trace, or `vtkWrapHierarchy-9.3` from a cmake-emitted
	// cmd) hit the rewrite.
	want := map[string]string{
		"vtkWrapHierarchy-9.3": ":WrapHierarchy",
		"WrapHierarchy":        ":WrapHierarchy",
		"vtkH5detect":          ":vtkH5detect",
		"nameless_artifact":    ":nameless_artifact",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("buildArtifactToLabelMap = %v; want %v", got, want)
	}
}

func TestBuildArtifactToLabelMap_NoBinaries(t *testing.T) {
	got := buildArtifactToLabelMap([]ir.Target{
		{Name: "libfoo", Kind: ir.KindCCLibrary},
	})
	if got != nil {
		t.Errorf("expected nil when no binaries; got %v", got)
	}
}

func TestRewriteToolFromTargetTokens(t *testing.T) {
	m := map[string]string{
		"vtkWrapHierarchy-9.3": ":WrapHierarchy",
		"vtkH5detect":          ":vtkH5detect",
	}
	cases := []struct {
		name      string
		in        string
		wantCmd   string
		wantTools []string
	}{
		{
			name:      "bin-prefix tool",
			in:        "bin/vtkWrapHierarchy-9.3 @args.txt -o output.txt",
			wantCmd:   "$(location :WrapHierarchy) @args.txt -o output.txt",
			wantTools: []string{":WrapHierarchy"},
		},
		{
			name:      "bare artifact name no prefix",
			in:        "vtkH5detect input.h",
			wantCmd:   "$(location :vtkH5detect) input.h",
			wantTools: []string{":vtkH5detect"},
		},
		{
			name:      "multiple tools dedup",
			in:        "bin/vtkWrapHierarchy-9.3 a && bin/vtkWrapHierarchy-9.3 b",
			wantCmd:   "$(location :WrapHierarchy) a && $(location :WrapHierarchy) b",
			wantTools: []string{":WrapHierarchy"},
		},
		{
			name:      "no match passes through",
			in:        "echo hello world",
			wantCmd:   "echo hello world",
			wantTools: nil,
		},
		{
			name:      "argv with leading directory drops to basename match",
			in:        "/path/to/bin/vtkH5detect arg",
			wantCmd:   "$(location :vtkH5detect) arg",
			wantTools: []string{":vtkH5detect"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gotCmd, gotTools := rewriteToolFromTargetTokens(c.in, m)
			if gotCmd != c.wantCmd {
				t.Errorf("cmd = %q; want %q", gotCmd, c.wantCmd)
			}
			if !reflect.DeepEqual(gotTools, c.wantTools) {
				t.Errorf("tools = %v; want %v", gotTools, c.wantTools)
			}
		})
	}
}

func TestRewriteToolFromTargetTokens_NilMap(t *testing.T) {
	gotCmd, gotTools := rewriteToolFromTargetTokens("anything bin/foo", nil)
	if gotCmd != "anything bin/foo" {
		t.Errorf("nil map should pass cmd through; got %q", gotCmd)
	}
	if gotTools != nil {
		t.Errorf("nil map should yield nil tools; got %v", gotTools)
	}
}

func TestRewriteToolFromTargetTokens_EmptyCmd(t *testing.T) {
	m := map[string]string{"foo": ":foo"}
	gotCmd, gotTools := rewriteToolFromTargetTokens("", m)
	if gotCmd != "" || gotTools != nil {
		t.Errorf("empty cmd: got (%q, %v); want (%q, nil)", gotCmd, gotTools, "")
	}
}
