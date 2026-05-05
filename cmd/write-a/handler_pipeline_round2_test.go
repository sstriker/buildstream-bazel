package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestWriter_PipelineKindsRound2 covers the round-2 opt-in for
// the additional pipeline kinds (kind:makemaker, kind:modulebuild,
// kind:manual, kind:script) that joined trace-driven round-2
// after kind:make. Each kind goes through the same
// pipelineHandler dispatch in handler_pipeline.go's
// shouldUseRound2; the test asserts the rendered shape end-to-end
// for each one. kind:make has its own dedicated test
// (handler_make_round2_test.go); the four here share a
// table-driven harness because their per-kind differences are
// all in handler config, not in the round-2 dispatch path.
func TestWriter_PipelineKindsRound2(t *testing.T) {
	cases := []struct {
		kind       string
		bstSnippet string
		extraSrcs  map[string]string
	}{
		{
			kind: "makemaker",
			// Synthetic Perl XS module skeleton. write-a doesn't
			// run the build at render time; the file content just
			// has to exist for the source-staging glob to find it.
			extraSrcs: map[string]string{
				"Makefile.PL": "# Makefile.PL stub\n",
				"lib/Foo.pm":  "package Foo; 1;\n",
				"Foo.xs":      "/* xs stub */\n",
			},
		},
		{
			kind: "modulebuild",
			extraSrcs: map[string]string{
				"Build.PL":   "# Build.PL stub\n",
				"lib/Bar.pm": "package Bar; 1;\n",
			},
		},
		{
			kind: "manual",
			// kind:manual needs config: with phase commands or it
			// renders with empty install-commands (still valid).
			bstSnippet: `
config:
  install-commands:
  - 'mkdir -p "%{install-root}/usr/share/manual" && echo hi > "%{install-root}/usr/share/manual/hi"'
`,
			extraSrcs: map[string]string{
				"placeholder.txt": "manual fixture\n",
			},
		},
		{
			kind: "script",
			// kind:script uses config: commands: [...] (flat).
			bstSnippet: `
config:
  commands:
  - 'mkdir -p "%{install-root}/usr/share/script" && echo hi > "%{install-root}/usr/share/script/hi"'
`,
			extraSrcs: map[string]string{
				"placeholder.txt": "script fixture\n",
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.kind, func(t *testing.T) {
			tmp := t.TempDir()
			srcDir := filepath.Join(tmp, "src")
			if err := os.MkdirAll(srcDir, 0o755); err != nil {
				t.Fatal(err)
			}
			for name, content := range tc.extraSrcs {
				p := filepath.Join(srcDir, name)
				if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			bst := filepath.Join(tmp, "elem.bst")
			body := "kind: " + tc.kind + "\nsources:\n- kind: local\n  path: " + srcDir + "\n" + tc.bstSnippet
			if err := os.WriteFile(bst, []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}

			for _, name := range []string{
				"convert-element-autotools-fake",
				"build-tracer-fake",
				"trace-publish-fake",
				"trace-lookup-fake",
			} {
				if err := os.WriteFile(filepath.Join(tmp, name),
					[]byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			prev := autotoolsConfig
			autotoolsConfig.convertBin = filepath.Join(tmp, "convert-element-autotools-fake")
			autotoolsConfig.tracerBin = filepath.Join(tmp, "build-tracer-fake")
			autotoolsConfig.publishBin = filepath.Join(tmp, "trace-publish-fake")
			autotoolsConfig.lookupBin = filepath.Join(tmp, "trace-lookup-fake")
			autotoolsConfig.round2Enabled = true
			t.Cleanup(func() { autotoolsConfig = prev })

			g, err := loadGraph([]string{bst}, "")
			if err != nil {
				t.Fatalf("loadGraph: %v", err)
			}
			binPath := fakeConvertBin(t, tmp)
			outA := filepath.Join(tmp, "A")
			outB := filepath.Join(tmp, "B")
			if err := writeProjectA(g, outA, binPath); err != nil {
				t.Fatalf("writeProjectA: %v", err)
			}
			if err := writeProjectB(g, outB); err != nil {
				t.Fatalf("writeProjectB: %v", err)
			}

			// A-side: converter genrule consuming @trace_elem//:trace.
			aBody, err := os.ReadFile(filepath.Join(outA, "elements/elem/BUILD.bazel"))
			if err != nil {
				t.Fatal(err)
			}
			for _, want := range []string{
				`name = "elem_build"`,
				`"@trace_elem//:trace"`,
				`"srckey.txt"`,
				`"//tools:convert-element-autotools"`,
				`--trace-dir`,
				"kind:" + tc.kind + " round-2",
			} {
				if !strings.Contains(string(aBody), want) {
					t.Errorf("[kind:%s] project A round-2 BUILD missing %q\n%s", tc.kind, want, aBody)
				}
			}
			// A must NOT host the legacy install genrule under round-2.
			if strings.Contains(string(aBody), `name = "elem_install"`) {
				t.Errorf("[kind:%s] project A unexpectedly contains the install genrule under round-2", tc.kind)
			}

			// srckey.txt rendered in both projects.
			for _, side := range []string{outA, outB} {
				if _, err := os.Stat(filepath.Join(side, "elements/elem/srckey.txt")); err != nil {
					t.Errorf("[kind:%s] srckey.txt missing under %s: %v", tc.kind, side, err)
				}
			}

			// MODULE.bazel declares the traces extension + the per-element repo.
			modA, err := os.ReadFile(filepath.Join(outA, "MODULE.bazel"))
			if err != nil {
				t.Fatal(err)
			}
			for _, want := range []string{
				`use_extension("//rules:traces.bzl", "traces")`,
				`"trace_elem"`,
			} {
				if !strings.Contains(string(modA), want) {
					t.Errorf("[kind:%s] project A MODULE.bazel missing %q\n%s", tc.kind, want, modA)
				}
			}

			// B-side: install genrule + trace-publish; converter is gone.
			bBody, err := os.ReadFile(filepath.Join(outB, "elements/elem/BUILD.bazel"))
			if err != nil {
				t.Fatal(err)
			}
			for _, want := range []string{
				`name = "elem_install"`,
				`"install_tree.tar"`,
				`"trace.log"`,
				`"make-db.txt"`,
				`"//tools:build-tracer"`,
				`"//tools:trace-publish"`,
				`CAS_GRPC_ADDR`,
			} {
				if !strings.Contains(string(bBody), want) {
					t.Errorf("[kind:%s] project B round-2 BUILD missing %q\n%s", tc.kind, want, bBody)
				}
			}
			for _, banned := range []string{
				`"BUILD.bazel.out"`,
				`"install-mapping.json"`,
				`//tools:convert-element-autotools`,
				`"imports.json"`,
			} {
				if strings.Contains(string(bBody), banned) {
					t.Errorf("[kind:%s] project B round-2 BUILD unexpectedly contains %q\n%s", tc.kind, banned, bBody)
				}
			}
		})
	}
}
