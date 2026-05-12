package lower

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/sstriker/cmake-to-bazel/internal/shadow"
)

// fileGenerateTestSetup writes a host source tree containing a
// template at <hostSrc>/<inRel> + a host build tree with the
// rendered output at <hostBuild>/<outRel>. The recordedSrcDir
// / recordedBuildDir paths are the same as the host paths in
// these tests (offline-fixture parity isn't part of the v1
// lifter's contract).
func fileGenerateTestSetup(t *testing.T, inRel, templateBody, outRel string, rendered []byte) (hostSrc, hostBuild string) {
	t.Helper()
	hostSrc = t.TempDir()
	hostBuild = t.TempDir()
	if inRel != "" {
		path := filepath.Join(hostSrc, inRel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(templateBody), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	outPath := filepath.Join(hostBuild, outRel)
	if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(outPath, rendered, 0o644); err != nil {
		t.Fatal(err)
	}
	return hostSrc, hostBuild
}

func hasTag(tags []string, want string) bool {
	for _, t := range tags {
		if t == want {
			return true
		}
	}
	return false
}

// TestRecoverFileGenerate_InputForm_Lifted exercises the INPUT
// shape on a genex-free template with a recoverable values
// dict: the lifter emits a genrule with srcs=<template>,
// tools=//tools:cmake-configure-file, cmake-codegen-lifted
// tag, and a cmd that references the template via $(location).
func TestRecoverFileGenerate_InputForm_Lifted(t *testing.T) {
	template := "#define VERSION \"@VERSION@\"\n"
	rendered := []byte("#define VERSION \"1.2.3\"\n")
	hostSrc, hostBuild := fileGenerateTestSetup(t, "src/banner.h.in", template, "banner.h", rendered)
	calls := []shadow.FileGenerateCall{{
		File:   filepath.Join(hostSrc, "CMakeLists.txt"),
		Output: filepath.Join(hostBuild, "banner.h"),
		Input:  filepath.Join(hostSrc, "src/banner.h.in"),
	}}
	cc := newCodegenContext()
	out, err := recoverFileGenerate(calls, hostSrc, hostSrc, hostBuild, hostBuild, true, map[string]string{"VERSION": "1.2.3"}, cc)
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if len(out) != 1 || out[0].RelOutput != "banner.h" {
		t.Fatalf("outs: %+v", out)
	}
	if len(cc.Genrules) != 1 {
		t.Fatalf("Genrules: %+v", cc.Genrules)
	}
	g := cc.Genrules[0]
	if g.Name != "gen_banner_h" {
		t.Errorf("name: %q want gen_banner_h", g.Name)
	}
	if len(g.Srcs) != 1 || g.Srcs[0] != "src/banner.h.in" {
		t.Errorf("srcs: %v want [src/banner.h.in]", g.Srcs)
	}
	if len(g.GenruleTools) != 1 || g.GenruleTools[0] != "//tools:cmake-configure-file" {
		t.Errorf("tools: %v", g.GenruleTools)
	}
	if !strings.Contains(g.GenruleCmd, "$(location src/banner.h.in)") {
		t.Errorf("cmd should reference $(location src/banner.h.in); got %q", g.GenruleCmd)
	}
	if !hasTag(g.Tags, "cmake-codegen-lifted") {
		t.Errorf("lifted tag missing: %v", g.Tags)
	}
	if !hasTag(g.Tags, "cmake-codegen-file-generate") {
		t.Errorf("driver-facet tag missing: %v", g.Tags)
	}
	if !sort.StringsAreSorted(g.Tags) {
		t.Errorf("tags not sorted: %v", g.Tags)
	}
}

// TestRecoverFileGenerate_ContentForm_Lifted exercises the
// CONTENT shape: no template on disk, the body comes from the
// call's Content field. Lifted shape uses --content-base64 in
// the cmd (instead of a srcs+$(location) reference) and emits
// the same cmake-codegen-lifted tag.
func TestRecoverFileGenerate_ContentForm_Lifted(t *testing.T) {
	template := "ver=@VERSION@\n"
	rendered := []byte("ver=9.9\n")
	hostSrc, hostBuild := fileGenerateTestSetup(t, "", "", "ver.txt", rendered)
	calls := []shadow.FileGenerateCall{{
		File:    filepath.Join(hostSrc, "CMakeLists.txt"),
		Output:  filepath.Join(hostBuild, "ver.txt"),
		Content: template,
	}}
	cc := newCodegenContext()
	out, err := recoverFileGenerate(calls, hostSrc, hostSrc, hostBuild, hostBuild, true, map[string]string{"VERSION": "9.9"}, cc)
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if len(out) != 1 || out[0].RelOutput != "ver.txt" {
		t.Fatalf("outs: %+v", out)
	}
	g := cc.Genrules[0]
	if len(g.Srcs) != 0 {
		t.Errorf("srcs should be empty for CONTENT form; got %v", g.Srcs)
	}
	wantBlob := "--content-base64=" + base64.StdEncoding.EncodeToString([]byte(template))
	if !strings.Contains(g.GenruleCmd, wantBlob) {
		t.Errorf("cmd should embed --content-base64 of the template; got %q", g.GenruleCmd)
	}
	if !hasTag(g.Tags, "cmake-codegen-lifted") {
		t.Errorf("lifted tag missing: %v", g.Tags)
	}
}

// TestRecoverFileGenerate_GenexFallsBackToLegacy asserts that
// a template containing $<...> short-circuits to legacy
// (rendered bytes embedded in cmd) and gets the explicit
// cmake-codegen-file-generate-genex audit tag. The cmake-
// codegen-lifted tag must NOT appear on the same genrule.
func TestRecoverFileGenerate_GenexFallsBackToLegacy(t *testing.T) {
	template := "tag=$<CONFIG:Release>;ver=@VERSION@\n"
	rendered := []byte("tag=1;ver=1.0\n")
	hostSrc, hostBuild := fileGenerateTestSetup(t, "src/g.in", template, "g.out", rendered)
	calls := []shadow.FileGenerateCall{{
		File:   filepath.Join(hostSrc, "CMakeLists.txt"),
		Output: filepath.Join(hostBuild, "g.out"),
		Input:  filepath.Join(hostSrc, "src/g.in"),
	}}
	cc := newCodegenContext()
	if _, err := recoverFileGenerate(calls, hostSrc, hostSrc, hostBuild, hostBuild, true, map[string]string{"VERSION": "1.0"}, cc); err != nil {
		t.Fatalf("recover: %v", err)
	}
	g := cc.Genrules[0]
	if len(g.Srcs) != 0 {
		t.Errorf("legacy fallback should not stage srcs; got %v", g.Srcs)
	}
	if hasTag(g.Tags, "cmake-codegen-lifted") {
		t.Errorf("genex-bearing template should NOT carry cmake-codegen-lifted; got %v", g.Tags)
	}
	if !hasTag(g.Tags, "cmake-codegen-file-generate-genex") {
		t.Errorf("genex audit tag missing: %v", g.Tags)
	}
	if !strings.Contains(g.GenruleCmd, "base64 -d") {
		t.Errorf("legacy cmd should base64-decode rendered bytes; got %q", g.GenruleCmd)
	}
}

// TestRecoverFileGenerate_LegacyWhenLiftDisabled covers the
// pre-lift compatibility shape: --lift-configure-file=false
// keeps every file(GENERATE) on the legacy bytes-embedded
// shape regardless of template content.
func TestRecoverFileGenerate_LegacyWhenLiftDisabled(t *testing.T) {
	template := "ver=@VERSION@\n"
	rendered := []byte("ver=1.0\n")
	hostSrc, hostBuild := fileGenerateTestSetup(t, "src/g.in", template, "g.out", rendered)
	calls := []shadow.FileGenerateCall{{
		File:   filepath.Join(hostSrc, "CMakeLists.txt"),
		Output: filepath.Join(hostBuild, "g.out"),
		Input:  filepath.Join(hostSrc, "src/g.in"),
	}}
	cc := newCodegenContext()
	if _, err := recoverFileGenerate(calls, hostSrc, hostSrc, hostBuild, hostBuild, false, map[string]string{"VERSION": "1.0"}, cc); err != nil {
		t.Fatalf("recover: %v", err)
	}
	g := cc.Genrules[0]
	if hasTag(g.Tags, "cmake-codegen-lifted") {
		t.Errorf("liftEnabled=false should not produce lifted tag; got %v", g.Tags)
	}
	if !hasTag(g.Tags, "cmake-codegen-file-generate") {
		t.Errorf("driver-facet tag missing: %v", g.Tags)
	}
}

// TestRecoverFileGenerate_MissingRenderedOutputDropsCall
// covers the CONDITION-false / fixture-missing case: when the
// recorded output doesn't materialize on disk, the lifter
// drops the call silently rather than synthesizing a genrule
// with no rendered bytes to embed. CONDITION evaluation
// happens at generate-time in cmake; the lifter detects the
// "didn't write" outcome by the absence of the file in the
// build dir.
func TestRecoverFileGenerate_MissingRenderedOutputDropsCall(t *testing.T) {
	hostSrc := t.TempDir()
	hostBuild := t.TempDir()
	calls := []shadow.FileGenerateCall{{
		File:    filepath.Join(hostSrc, "CMakeLists.txt"),
		Output:  filepath.Join(hostBuild, "skipped.txt"),
		Content: "anything\n",
	}}
	cc := newCodegenContext()
	out, err := recoverFileGenerate(calls, hostSrc, hostSrc, hostBuild, hostBuild, true, nil, cc)
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if len(out) != 0 {
		t.Errorf("missing rendered output should produce 0 outs; got %+v", out)
	}
	if len(cc.Genrules) != 0 {
		t.Errorf("missing rendered output should produce 0 genrules; got %+v", cc.Genrules)
	}
}

// TestRecoverFileGenerate_DedupesDuplicateOutputs confirms
// that two trace records for the same OUTPUT (cmake re-emits
// the call across frames) collapse to a single genrule.
func TestRecoverFileGenerate_DedupesDuplicateOutputs(t *testing.T) {
	rendered := []byte("hi\n")
	hostSrc, hostBuild := fileGenerateTestSetup(t, "", "", "g.txt", rendered)
	call := shadow.FileGenerateCall{
		File:    filepath.Join(hostSrc, "CMakeLists.txt"),
		Output:  filepath.Join(hostBuild, "g.txt"),
		Content: "hi\n",
	}
	cc := newCodegenContext()
	if _, err := recoverFileGenerate([]shadow.FileGenerateCall{call, call}, hostSrc, hostSrc, hostBuild, hostBuild, true, nil, cc); err != nil {
		t.Fatalf("recover: %v", err)
	}
	if len(cc.Genrules) != 1 {
		t.Errorf("dedupe failed; got %d genrules", len(cc.Genrules))
	}
}

// TestHasGenex covers the genex detector: any "$<" substring
// triggers true regardless of position, balance, or contents.
func TestHasGenex(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"", false},
		{"@VAR@", false},
		{"${VAR}", false},
		{"foo $< bar", true},
		{"$<CONFIG:Release>", true},
		{"line one\nline two with $<TARGET_FILE:foo>\n", true},
		{"$ < CONFIG", false}, // not contiguous
	}
	for _, c := range cases {
		if got := hasGenex([]byte(c.in)); got != c.want {
			t.Errorf("hasGenex(%q) = %v want %v", c.in, got, c.want)
		}
	}
}
