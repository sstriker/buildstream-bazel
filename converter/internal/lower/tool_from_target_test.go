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
	want := map[string]string{
		"vtkWrapHierarchy-9.3": ":WrapHierarchy",
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

func TestApplyToolFromTargetToGenrules(t *testing.T) {
	pkg := &ir.Package{
		Targets: []ir.Target{
			{Name: "H5detect", Kind: ir.KindCCBinary, ArtifactName: "vtkH5detect"},
			{
				Name:        "gen_H5Tinit",
				Kind:        ir.KindGenrule,
				Srcs:        []string{"bin/vtkH5detect", "other.h"},
				GenruleCmd:  "bin/vtkH5detect H5Tinit.c",
				GenruleOuts: []string{"H5Tinit.c"},
			},
			{
				Name:       "unrelated",
				Kind:       ir.KindCCLibrary,
				Srcs:       []string{"lib.cc"},
				GenruleCmd: "should not touch this cc_library",
			},
		},
	}
	artifactToLabel := buildArtifactToLabelMap(pkg.Targets)
	applyToolFromTargetToGenrules(pkg, artifactToLabel)

	gen := pkg.Targets[1]
	wantCmd := "$(location :H5detect) H5Tinit.c"
	if gen.GenruleCmd != wantCmd {
		t.Errorf("cmd = %q; want %q", gen.GenruleCmd, wantCmd)
	}
	if len(gen.GenruleTools) != 1 || gen.GenruleTools[0] != ":H5detect" {
		t.Errorf("tools = %v; want [:H5detect]", gen.GenruleTools)
	}
	// srcs should have bin/vtkH5detect dropped; other.h kept.
	if len(gen.Srcs) != 1 || gen.Srcs[0] != "other.h" {
		t.Errorf("srcs after rewrite = %v; want [other.h]", gen.Srcs)
	}

	lib := pkg.Targets[2]
	if lib.GenruleCmd != "should not touch this cc_library" {
		t.Errorf("cc_library cmd was mutated: %q", lib.GenruleCmd)
	}
}

func TestApplyToolFromTargetToGenrules_NoMatch(t *testing.T) {
	pkg := &ir.Package{
		Targets: []ir.Target{
			{Name: "unmatched", Kind: ir.KindCCBinary, ArtifactName: "different"},
			{
				Name:       "gen_thing",
				Kind:       ir.KindGenrule,
				Srcs:       []string{"a.h"},
				GenruleCmd: "echo hi > $@",
			},
		},
	}
	applyToolFromTargetToGenrules(pkg, buildArtifactToLabelMap(pkg.Targets))
	gen := pkg.Targets[1]
	if gen.GenruleCmd != "echo hi > $@" {
		t.Errorf("cmd should pass through; got %q", gen.GenruleCmd)
	}
	if gen.GenruleTools != nil {
		t.Errorf("tools should stay nil; got %v", gen.GenruleTools)
	}
}
