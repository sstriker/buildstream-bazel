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
				"convert-element-trace-fake",
				"build-tracer-fake",
				"trace-publish-fake",
				"trace-lookup-fake",
			} {
				if err := os.WriteFile(filepath.Join(tmp, name),
					[]byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			prev := traceConfig
			traceConfig.convertBin = filepath.Join(tmp, "convert-element-trace-fake")
			traceConfig.tracerBin = filepath.Join(tmp, "build-tracer-fake")
			traceConfig.publishBin = filepath.Join(tmp, "trace-publish-fake")
			traceConfig.lookupBin = filepath.Join(tmp, "trace-lookup-fake")
			traceConfig.round2Enabled = true
			t.Cleanup(func() { traceConfig = prev })

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
				`"//tools:convert-element-trace"`,
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
				`//tools:convert-element-trace`,
				`"imports.json"`,
			} {
				if strings.Contains(string(bBody), banned) {
					t.Errorf("[kind:%s] project B round-2 BUILD unexpectedly contains %q\n%s", tc.kind, banned, bBody)
				}
			}
		})
	}
}

// TestWriter_PipelineKindsRound2_MultiPlatform: --platforms-json
// switches project A's per-element render from one converter
// genrule to N (one per platform) + one fold-element genrule.
// Each per-platform genrule reads its platform-tagged trace
// repo; the fold-element invocation consumes their ir.json
// outputs with the right --cell argv shape.
func TestWriter_PipelineKindsRound2_MultiPlatform(t *testing.T) {
	tmp := t.TempDir()
	srcDir := filepath.Join(tmp, "src")
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "Makefile"), []byte("all:\n\techo hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	bst := filepath.Join(tmp, "elem.bst")
	if err := os.WriteFile(bst, []byte("kind: make\nsources:\n- kind: local\n  path: "+srcDir+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{"convert-element-trace-fake", "build-tracer-fake", "trace-publish-fake", "trace-lookup-fake", "fold-element-fake"} {
		if err := os.WriteFile(filepath.Join(tmp, name), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	prev := traceConfig
	traceConfig.convertBin = filepath.Join(tmp, "convert-element-trace-fake")
	traceConfig.tracerBin = filepath.Join(tmp, "build-tracer-fake")
	traceConfig.publishBin = filepath.Join(tmp, "trace-publish-fake")
	traceConfig.lookupBin = filepath.Join(tmp, "trace-lookup-fake")
	traceConfig.foldBin = filepath.Join(tmp, "fold-element-fake")
	traceConfig.round2Enabled = true
	traceConfig.platforms = []tracePlatform{
		{Name: "linux_x86_64", Constraints: []string{"@platforms//os:linux", "@platforms//cpu:x86_64"}},
		{Name: "darwin_arm64", Constraints: []string{"@platforms//os:darwin", "@platforms//cpu:arm64"}},
	}
	t.Cleanup(func() { traceConfig = prev })

	g, err := loadGraph([]string{bst}, "")
	if err != nil {
		t.Fatalf("loadGraph: %v", err)
	}
	binPath := fakeConvertBin(t, tmp)
	outA := filepath.Join(tmp, "A")
	if err := writeProjectA(g, outA, binPath); err != nil {
		t.Fatalf("writeProjectA: %v", err)
	}

	aBody, err := os.ReadFile(filepath.Join(outA, "elements/elem/BUILD.bazel"))
	if err != nil {
		t.Fatal(err)
	}
	got := string(aBody)

	// Two per-platform converter genrules, each reading its
	// platform-tagged trace repo.
	for _, want := range []string{
		`name = "elem__linux_x86_64_ir"`,
		`name = "elem__darwin_arm64_ir"`,
		`"@trace_elem__linux_x86_64//:trace"`,
		`"@trace_elem__darwin_arm64//:trace"`,
		`"linux_x86_64/ir.json"`,
		`"linux_x86_64/BUILD.bazel.out"`,
		`"darwin_arm64/ir.json"`,
		`"darwin_arm64/BUILD.bazel.out"`,
		`--out-ir-json=`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("multi-platform project A missing %q\n%s", want, got)
		}
	}

	// Fold genrule preserves the "<elem>_build" name so
	// downstream consumers don't need to learn a new label.
	for _, want := range []string{
		`name = "elem_build"`,
		`"//tools:fold-element"`,
		`--cell 'linux_x86_64|@platforms//os:linux,@platforms//cpu:x86_64|$(location linux_x86_64/ir.json)'`,
		`--cell 'darwin_arm64|@platforms//os:darwin,@platforms//cpu:arm64|$(location darwin_arm64/ir.json)'`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("multi-platform fold genrule missing %q\n%s", want, got)
		}
	}

	// Legacy single @trace_elem//:trace label should NOT appear
	// — every trace reference is platform-tagged in multi-platform
	// mode.
	if strings.Contains(got, `"@trace_elem//:trace"`) {
		t.Errorf("multi-platform mode unexpectedly contains legacy single-platform @trace_elem//:trace label\n%s", got)
	}

	// traces.json declares one entry per (element, platform) cell.
	tracesJSONBody, err := os.ReadFile(filepath.Join(outA, "tools/traces.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`"key": "elem__linux_x86_64"`,
		`"key": "elem__darwin_arm64"`,
		`"platform": "linux_x86_64"`,
		`"platform": "darwin_arm64"`,
	} {
		if !strings.Contains(string(tracesJSONBody), want) {
			t.Errorf("multi-platform traces.json missing %q\n%s", want, tracesJSONBody)
		}
	}
	// Legacy unsuffixed entry should not appear.
	if strings.Contains(string(tracesJSONBody), `"key": "elem"`) {
		t.Errorf("multi-platform traces.json unexpectedly contains legacy unsuffixed elem entry\n%s", tracesJSONBody)
	}
}

// TestWriter_TraceDrivenRound2A_OperatorSelectLabelOverride: when
// the platforms manifest provides a select_label, the fold's
// --cell argv carries it as the optional 4th field. This is the
// escalation path for matrices the constraint-axis auto-detect
// can't disambiguate ({linux_x86_64, linux_aarch64, darwin_arm64}).
func TestWriter_TraceDrivenRound2A_OperatorSelectLabelOverride(t *testing.T) {
	tmp := t.TempDir()
	srcDir := filepath.Join(tmp, "src")
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "Makefile"), []byte("all:\n\techo hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	bst := filepath.Join(tmp, "elem.bst")
	if err := os.WriteFile(bst, []byte("kind: make\nsources:\n- kind: local\n  path: "+srcDir+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"convert-element-trace-fake", "build-tracer-fake", "trace-publish-fake", "trace-lookup-fake", "fold-element-fake"} {
		if err := os.WriteFile(filepath.Join(tmp, name), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	prev := traceConfig
	traceConfig.convertBin = filepath.Join(tmp, "convert-element-trace-fake")
	traceConfig.tracerBin = filepath.Join(tmp, "build-tracer-fake")
	traceConfig.publishBin = filepath.Join(tmp, "trace-publish-fake")
	traceConfig.lookupBin = filepath.Join(tmp, "trace-lookup-fake")
	traceConfig.foldBin = filepath.Join(tmp, "fold-element-fake")
	traceConfig.round2Enabled = true
	traceConfig.platforms = []tracePlatform{
		{Name: "linux_x86_64", Constraints: []string{"@platforms//os:linux", "@platforms//cpu:x86_64"}, SelectLabel: "//platforms:linux_x86_64"},
		{Name: "linux_aarch64", Constraints: []string{"@platforms//os:linux", "@platforms//cpu:arm64"}, SelectLabel: "//platforms:linux_aarch64"},
		{Name: "darwin_arm64", Constraints: []string{"@platforms//os:darwin", "@platforms//cpu:arm64"}, SelectLabel: "//platforms:darwin_arm64"},
	}
	t.Cleanup(func() { traceConfig = prev })

	g, err := loadGraph([]string{bst}, "")
	if err != nil {
		t.Fatalf("loadGraph: %v", err)
	}
	binPath := fakeConvertBin(t, tmp)
	outA := filepath.Join(tmp, "A")
	if err := writeProjectA(g, outA, binPath); err != nil {
		t.Fatalf("writeProjectA: %v", err)
	}
	aBody, err := os.ReadFile(filepath.Join(outA, "elements/elem/BUILD.bazel"))
	if err != nil {
		t.Fatal(err)
	}
	got := string(aBody)
	// Each cell carries the 4th field with the operator-declared
	// config_setting label.
	for _, want := range []string{
		`--cell 'linux_x86_64|@platforms//os:linux,@platforms//cpu:x86_64|$(location linux_x86_64/ir.json)|//platforms:linux_x86_64'`,
		`--cell 'linux_aarch64|@platforms//os:linux,@platforms//cpu:arm64|$(location linux_aarch64/ir.json)|//platforms:linux_aarch64'`,
		`--cell 'darwin_arm64|@platforms//os:darwin,@platforms//cpu:arm64|$(location darwin_arm64/ir.json)|//platforms:darwin_arm64'`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("multi-platform fold genrule missing select_label cell %q\n%s", want, got)
		}
	}
}
