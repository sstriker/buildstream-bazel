package lower

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/sstriker/buildstream-bazel/internal/genexeval"
	"github.com/sstriker/buildstream-bazel/internal/shadow"
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
	// file(GENERATE) is verbatim emit (CopyOnly=true on the
	// Bazel-time tool); template == rendered is the verify-pass
	// for genex-free shapes. cmakeVars must not bloat the
	// resulting cmd — the lift always passes an empty values
	// dict regardless of what the operator's namespace looks
	// like, so we can pass a populated cmakeVars here and still
	// expect the cmd to carry `{}`.
	template := "#define BANNER \"hi\"\n"
	rendered := []byte("#define BANNER \"hi\"\n")
	hostSrc, hostBuild := fileGenerateTestSetup(t, "src/banner.h.in", template, "banner.h", rendered)
	calls := []shadow.FileGenerateCall{{
		File:     filepath.Join(hostSrc, "CMakeLists.txt"),
		Output:   filepath.Join(hostBuild, "banner.h"),
		Input:    filepath.Join(hostSrc, "src/banner.h.in"),
		HasInput: true,
	}}
	cc := newCodegenContext()
	out, err := recoverFileGenerate(calls, hostSrc, hostSrc, hostBuild, hostBuild, true, map[string]string{"VERSION": "1.2.3"}, nil, cc)
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
	// file(GENERATE) lifts always pass --copy-only + empty
	// values JSON — cmakeVars don't ride into the cmd.
	if !strings.Contains(g.GenruleCmd, "--copy-only") {
		t.Errorf("file(GENERATE) lifted cmd should carry --copy-only; got %q", g.GenruleCmd)
	}
	// base64("{}") == "e30=" — the empty values dict.
	if !strings.Contains(g.GenruleCmd, "echo e30= | base64 -d") {
		t.Errorf("file(GENERATE) lifted cmd should embed an empty values dict (base64 \"{}\" == \"e30=\"); got %q", g.GenruleCmd)
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
	// file(GENERATE CONTENT) is verbatim emit — the Content
	// string and rendered bytes match by construction (cmake's
	// argument-parsing already substituted any ${VAR} before
	// the file(GENERATE) call ran).
	template := "ver=9.9\n"
	rendered := []byte("ver=9.9\n")
	hostSrc, hostBuild := fileGenerateTestSetup(t, "", "", "ver.txt", rendered)
	calls := []shadow.FileGenerateCall{{
		File:       filepath.Join(hostSrc, "CMakeLists.txt"),
		Output:     filepath.Join(hostBuild, "ver.txt"),
		Content:    template,
		HasContent: true,
	}}
	cc := newCodegenContext()
	out, err := recoverFileGenerate(calls, hostSrc, hostSrc, hostBuild, hostBuild, true, map[string]string{"VERSION": "9.9"}, nil, cc)
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
	if !strings.Contains(g.GenruleCmd, "--copy-only") {
		t.Errorf("file(GENERATE) CONTENT-form lifted cmd should carry --copy-only; got %q", g.GenruleCmd)
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
		File:     filepath.Join(hostSrc, "CMakeLists.txt"),
		Output:   filepath.Join(hostBuild, "g.out"),
		Input:    filepath.Join(hostSrc, "src/g.in"),
		HasInput: true,
	}}
	cc := newCodegenContext()
	if _, err := recoverFileGenerate(calls, hostSrc, hostSrc, hostBuild, hostBuild, true, map[string]string{"VERSION": "1.0"}, nil, cc); err != nil {
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

// TestRecoverFileGenerate_GenexLiftedViaStructuredBase64
// exercises the (b) lift's success path: a template with a
// configure-time-resolvable `$<...>` whose static surround
// uniquely identifies the genex's rendered span. The lifter
// extracts the genex literal → resolved bytes map and emits
// the lifted shape; the cmd carries --genex-values=<sidecar>
// alongside the existing --values=<sidecar>, and the audit
// tag set carries BOTH cmake-codegen-lifted AND
// cmake-codegen-file-generate-genex-lifted so the audit can
// distinguish "lifted via the (b) capture" from "lifted via
// plain non-genex emit". The rendered bytes do NOT appear in
// the cmd (the (b) shape's whole point: rendered output is
// no longer content-load-bearing in srckey).
func TestRecoverFileGenerate_GenexLiftedViaStructuredBase64(t *testing.T) {
	template := "// config: $<CONFIG:Release>\n#define IS_LINUX $<PLATFORM_ID:Linux>\n"
	rendered := []byte("// config: 1\n#define IS_LINUX 1\n")
	hostSrc, hostBuild := fileGenerateTestSetup(t, "src/g.in", template, "g.out", rendered)
	calls := []shadow.FileGenerateCall{{
		File:     filepath.Join(hostSrc, "CMakeLists.txt"),
		Output:   filepath.Join(hostBuild, "g.out"),
		Input:    filepath.Join(hostSrc, "src/g.in"),
		HasInput: true,
	}}
	cc := newCodegenContext()
	if _, err := recoverFileGenerate(calls, hostSrc, hostSrc, hostBuild, hostBuild, true, nil, nil, cc); err != nil {
		t.Fatalf("recover: %v", err)
	}
	if len(cc.Genrules) != 1 {
		t.Fatalf("expected one genrule; got %d", len(cc.Genrules))
	}
	g := cc.Genrules[0]
	for _, want := range []string{
		"cmake-codegen-lifted",
		"cmake-codegen-file-generate-genex-lifted",
	} {
		if !hasTag(g.Tags, want) {
			t.Errorf("missing tag %q in %v", want, g.Tags)
		}
	}
	if hasTag(g.Tags, "cmake-codegen-file-generate-genex") {
		t.Errorf("(b)-lifted shape should NOT carry the legacy-fallback genex tag; got %v", g.Tags)
	}
	if len(g.Srcs) != 1 || g.Srcs[0] != "src/g.in" {
		t.Errorf("INPUT-form lift should stage the template as srcs; got %v", g.Srcs)
	}
	if !strings.Contains(g.GenruleCmd, "--genex-values=") {
		t.Errorf("cmd should pass --genex-values=; got %q", g.GenruleCmd)
	}
	// Decode the staged values + genex-values blobs from the cmd
	// to verify the lift's captured payload is the right shape.
	for _, marker := range []string{
		"GENEX_VALUES=",
		"cmake-configure-file.genex.XXXXXX",
		"//tools:cmake-configure-file",
	} {
		if !strings.Contains(g.GenruleCmd, marker) {
			t.Errorf("cmd missing marker %q; got %q", marker, g.GenruleCmd)
		}
	}
	// Soundness: rendered bytes must NOT appear in the cmd.
	// The (b) lift's whole win is that rendered output is no
	// longer carried byte-for-byte in BUILD.bazel.
	rendEnc := base64.StdEncoding.EncodeToString(rendered)
	if strings.Contains(g.GenruleCmd, rendEnc) {
		t.Errorf("rendered bytes appear in cmd as base64 (%s); the (b) lift should NOT embed them", rendEnc)
	}
	// The captured genex-values payload must round-trip.
	values, ok := extractGenexValuesFromCmd(t, g.GenruleCmd)
	if !ok {
		return // extractor already failed the test
	}
	want := map[string]string{
		"$<CONFIG:Release>":    "1",
		"$<PLATFORM_ID:Linux>": "1",
	}
	if len(values) != len(want) {
		t.Errorf("captured genex values: got %d entries, want %d (%#v)", len(values), len(want), values)
	}
	for k, v := range want {
		if got := values[k]; got != v {
			t.Errorf("genex value for %q: got %q, want %q", k, got, v)
		}
	}
}

// extractGenexValuesFromCmd decodes the base64 blob the lifted
// shell command stages into the GENEX_VALUES sidecar. The blob
// sits between `echo ` and ` | base64 -d > "$$GENEX_VALUES"`
// in the cmd — same pattern the lifter uses for the regular
// VALUES sidecar. Returns the decoded map plus a sentinel for
// extractor-level failures so the calling test can short-
// circuit cleanly.
func extractGenexValuesFromCmd(t *testing.T, cmd string) (map[string]string, bool) {
	t.Helper()
	const before = `echo `
	const after = ` | base64 -d > "$$GENEX_VALUES"`
	a := strings.Index(cmd, after)
	if a < 0 {
		t.Errorf("cmd missing GENEX_VALUES base64-decode pattern")
		return nil, false
	}
	// Walk backward from `a` to find the matching `echo ` prefix.
	// Multiple `echo ... | base64 -d` blocks coexist (VALUES +
	// GENEX_VALUES); pair the GENEX_VALUES output redirect with
	// the nearest preceding `echo `.
	b := strings.LastIndex(cmd[:a], before)
	if b < 0 {
		t.Errorf("cmd's GENEX_VALUES decode pattern has no echo prefix")
		return nil, false
	}
	enc := cmd[b+len(before) : a]
	raw, err := base64.StdEncoding.DecodeString(enc)
	if err != nil {
		t.Errorf("decode genex base64 blob %q: %v", enc, err)
		return nil, false
	}
	var values map[string]string
	if err := json.Unmarshal(raw, &values); err != nil {
		t.Errorf("parse genex JSON %s: %v", raw, err)
		return nil, false
	}
	return values, true
}

// TestRecoverFileGenerate_GenexExtractionFailureFallsBackToLegacy
// pins the (b) lift's failure mode. The template's genex
// value contains the next static anchor's bytes verbatim, so
// the lockstep walker mis-aligns and extraction returns an
// error. The lifter must fall back to the legacy bytes-
// embedded shape with cmake-codegen-file-generate-genex
// (NOT the -lifted variant), so the audit signal stays
// honest.
func TestRecoverFileGenerate_GenexExtractionFailureFallsBackToLegacy(t *testing.T) {
	// Construct a template+rendered pair where the same genex
	// literal resolves to two different rendered values. The
	// (b) lift's literal-replace replay can't represent that
	// (one key → one value); extractor's collision check
	// rejects with "resolves to two different values", and the
	// lifter falls back to legacy.
	template := "first=$<CONFIG> second=$<CONFIG>\n"
	rendered := []byte("first=Release second=Debug\n")
	hostSrc, hostBuild := fileGenerateTestSetup(t, "src/g.in", template, "g.out", rendered)
	calls := []shadow.FileGenerateCall{{
		File:     filepath.Join(hostSrc, "CMakeLists.txt"),
		Output:   filepath.Join(hostBuild, "g.out"),
		Input:    filepath.Join(hostSrc, "src/g.in"),
		HasInput: true,
	}}
	cc := newCodegenContext()
	if _, err := recoverFileGenerate(calls, hostSrc, hostSrc, hostBuild, hostBuild, true, nil, nil, cc); err != nil {
		t.Fatalf("recover: %v", err)
	}
	g := cc.Genrules[0]
	if hasTag(g.Tags, "cmake-codegen-lifted") {
		t.Errorf("extraction failure should fall back to legacy; got lifted tag in %v", g.Tags)
	}
	if !hasTag(g.Tags, "cmake-codegen-file-generate-genex") {
		t.Errorf("legacy fallback after extraction failure must carry the genex audit tag; got %v", g.Tags)
	}
	if hasTag(g.Tags, "cmake-codegen-file-generate-genex-lifted") {
		t.Errorf("extraction-failure fallback must NOT carry the lifted-genex tag; got %v", g.Tags)
	}
}

// TestRecoverFileGenerate_GenexEvaluatedViaGoSideEvaluator
// exercises the (a) lift's success path: a template with a
// `$<CONFIG>` genex, plus a cmakeVars dump carrying
// CMAKE_BUILD_TYPE so the genexeval.Context can resolve at
// convert time. The lifter parses the template, evaluates
// against the Context, confirms the bytes match cmake's
// rendered output, and emits the (a)-shape genrule with
// --genex-context= alongside the existing --values=. Audit
// tags: cmake-codegen-lifted + cmake-codegen-file-generate-
// genex-evaluated. NOT the -lifted variant (that's (b)'s tag),
// NOT the bare -genex variant (legacy fallback).
func TestRecoverFileGenerate_GenexEvaluatedViaGoSideEvaluator(t *testing.T) {
	template := "// build: $<CONFIG>\n#define IS_LINUX $<PLATFORM_ID:Linux>\n"
	rendered := []byte("// build: Release\n#define IS_LINUX 1\n")
	hostSrc, hostBuild := fileGenerateTestSetup(t, "src/g.in", template, "g.out", rendered)
	calls := []shadow.FileGenerateCall{{
		File:     filepath.Join(hostSrc, "CMakeLists.txt"),
		Output:   filepath.Join(hostBuild, "g.out"),
		Input:    filepath.Join(hostSrc, "src/g.in"),
		HasInput: true,
	}}
	cmakeVars := map[string]string{
		"CMAKE_BUILD_TYPE":      "Release",
		"CMAKE_SYSTEM_NAME":     "Linux",
		"CMAKE_C_COMPILER_ID":   "GNU",
		"CMAKE_CXX_COMPILER_ID": "GNU",
	}
	cc := newCodegenContext()
	if _, err := recoverFileGenerate(calls, hostSrc, hostSrc, hostBuild, hostBuild, true, cmakeVars, nil, cc); err != nil {
		t.Fatalf("recover: %v", err)
	}
	if len(cc.Genrules) != 1 {
		t.Fatalf("expected one genrule; got %d", len(cc.Genrules))
	}
	g := cc.Genrules[0]
	for _, want := range []string{
		"cmake-codegen-lifted",
		"cmake-codegen-file-generate-genex-evaluated",
	} {
		if !hasTag(g.Tags, want) {
			t.Errorf("missing tag %q in %v", want, g.Tags)
		}
	}
	for _, unwanted := range []string{
		"cmake-codegen-file-generate-genex",        // legacy fallback
		"cmake-codegen-file-generate-genex-lifted", // (b)-shape only
	} {
		if hasTag(g.Tags, unwanted) {
			t.Errorf("unexpected tag %q in %v", unwanted, g.Tags)
		}
	}
	if !strings.Contains(g.GenruleCmd, "--genex-context=") {
		t.Errorf("cmd should pass --genex-context=; got %q", g.GenruleCmd)
	}
	if strings.Contains(g.GenruleCmd, "--genex-values=") {
		t.Errorf("(a) lift should NOT pass --genex-values=; got %q", g.GenruleCmd)
	}
	// Soundness: rendered bytes must NOT appear in the cmd.
	rendEnc := base64.StdEncoding.EncodeToString(rendered)
	if strings.Contains(g.GenruleCmd, rendEnc) {
		t.Errorf("rendered bytes appear in cmd as base64 (%s); (a) lift should NOT embed them", rendEnc)
	}
	// The Context payload should be small (typical: <100 bytes).
	// Decode it and verify the captured fields.
	ctx := extractGenexContextFromCmd(t, g.GenruleCmd)
	if ctx.Config != "Release" {
		t.Errorf("captured Context.Config = %q want Release", ctx.Config)
	}
	if ctx.PlatformID != "Linux" {
		t.Errorf("captured Context.PlatformID = %q want Linux", ctx.PlatformID)
	}
	if ctx.CompilerID["C"] != "GNU" || ctx.CompilerID["CXX"] != "GNU" {
		t.Errorf("captured Context.CompilerID = %v want C=GNU CXX=GNU", ctx.CompilerID)
	}
}

// extractGenexContextFromCmd decodes the base64 blob the (a)
// lifted shell command stages into the GENEX_CONTEXT sidecar.
// Mirrors extractGenexValuesFromCmd's anchor walk but for the
// genex-context blob.
func extractGenexContextFromCmd(t *testing.T, cmd string) struct {
	Config           string            `json:"config,omitempty"`
	CompilerID       map[string]string `json:"compiler_id,omitempty"`
	PlatformID       string            `json:"platform_id,omitempty"`
	CompilerLanguage string            `json:"compiler_language,omitempty"`
} {
	t.Helper()
	type ctxJSON struct {
		Config           string            `json:"config,omitempty"`
		CompilerID       map[string]string `json:"compiler_id,omitempty"`
		PlatformID       string            `json:"platform_id,omitempty"`
		CompilerLanguage string            `json:"compiler_language,omitempty"`
	}
	var empty ctxJSON
	const before = `echo `
	const after = ` | base64 -d > "$$GENEX_CONTEXT"`
	a := strings.Index(cmd, after)
	if a < 0 {
		t.Errorf("cmd missing GENEX_CONTEXT base64-decode pattern")
		return empty
	}
	b := strings.LastIndex(cmd[:a], before)
	if b < 0 {
		t.Errorf("cmd's GENEX_CONTEXT decode pattern has no echo prefix")
		return empty
	}
	enc := cmd[b+len(before) : a]
	raw, err := base64.StdEncoding.DecodeString(enc)
	if err != nil {
		t.Errorf("decode genex-context base64 blob %q: %v", enc, err)
		return empty
	}
	var ctx ctxJSON
	if err := json.Unmarshal(raw, &ctx); err != nil {
		t.Errorf("parse genex-context JSON %s: %v", raw, err)
		return empty
	}
	return ctx
}

// TestRecoverFileGenerate_GenexEvaluatedFallsBackToCapturedOnUnsupportedOp
// asserts the (a) → (b) → legacy fallthrough order: a template
// with $<TARGET_FILE:...> (which the (a) evaluator refuses via
// UnsupportedError) routes to (b); (b) succeeds when the static
// surround can anchor the value; tag set carries the (b) facet,
// not (a)'s.
func TestRecoverFileGenerate_GenexEvaluatedFallsBackToCapturedOnUnsupportedOp(t *testing.T) {
	// $<TARGET_FILE:foo> is the unsupported op. cmake renders
	// it to "/abs/path/libfoo.a" or similar at generate time;
	// the trace records the resolved bytes in the output. For
	// the test we just need a template+rendered pair where (a)
	// refuses but (b) extracts cleanly.
	template := "// link to $<TARGET_FILE:foo>\n"
	rendered := []byte("// link to /opt/lib/libfoo.a\n")
	hostSrc, hostBuild := fileGenerateTestSetup(t, "src/g.in", template, "g.out", rendered)
	calls := []shadow.FileGenerateCall{{
		File:     filepath.Join(hostSrc, "CMakeLists.txt"),
		Output:   filepath.Join(hostBuild, "g.out"),
		Input:    filepath.Join(hostSrc, "src/g.in"),
		HasInput: true,
	}}
	cmakeVars := map[string]string{"CMAKE_BUILD_TYPE": "Release"}
	cc := newCodegenContext()
	if _, err := recoverFileGenerate(calls, hostSrc, hostSrc, hostBuild, hostBuild, true, cmakeVars, nil, cc); err != nil {
		t.Fatalf("recover: %v", err)
	}
	g := cc.Genrules[0]
	if hasTag(g.Tags, "cmake-codegen-file-generate-genex-evaluated") {
		t.Errorf("unsupported $<TARGET_FILE:> should NOT yield (a) tag; got %v", g.Tags)
	}
	if !hasTag(g.Tags, "cmake-codegen-file-generate-genex-lifted") {
		t.Errorf("expected (b) fallback tag in %v", g.Tags)
	}
	if !hasTag(g.Tags, "cmake-codegen-lifted") {
		t.Errorf("(b) fallback should still carry cmake-codegen-lifted; got %v", g.Tags)
	}
}

// TestRecoverFileGenerate_GenexEvaluatedSkippedWhenCMakeVarsEmpty
// asserts the (a) lift refuses when the Context is unavailable
// (no CMAKE_BUILD_TYPE → genexeval.Context.Config is empty →
// $<CONFIG> evaluation surfaces UnsupportedError). The lifter
// falls through to (b) — same fixture genex shape gets the
// captured-bytes lift when the evaluator can't fire.
func TestRecoverFileGenerate_GenexEvaluatedSkippedWhenCMakeVarsEmpty(t *testing.T) {
	template := "// build: $<CONFIG>\n"
	rendered := []byte("// build: Release\n")
	hostSrc, hostBuild := fileGenerateTestSetup(t, "src/g.in", template, "g.out", rendered)
	calls := []shadow.FileGenerateCall{{
		File:     filepath.Join(hostSrc, "CMakeLists.txt"),
		Output:   filepath.Join(hostBuild, "g.out"),
		Input:    filepath.Join(hostSrc, "src/g.in"),
		HasInput: true,
	}}
	cc := newCodegenContext()
	// nil cmakeVars → empty Context → (a) refuses.
	if _, err := recoverFileGenerate(calls, hostSrc, hostSrc, hostBuild, hostBuild, true, nil, nil, cc); err != nil {
		t.Fatalf("recover: %v", err)
	}
	g := cc.Genrules[0]
	if hasTag(g.Tags, "cmake-codegen-file-generate-genex-evaluated") {
		t.Errorf("empty cmakeVars should NOT yield (a) tag; got %v", g.Tags)
	}
	if !hasTag(g.Tags, "cmake-codegen-file-generate-genex-lifted") {
		t.Errorf("expected (b) fallback tag in %v", g.Tags)
	}
}

// TestRecoverFileGenerate_OutputSideGenexResolved covers the
// OUTPUT-side (a) resolution path: a call recorded as
// `OUTPUT $<CONFIG>/foo.h` had its filename resolved at
// generate-time to `Release/foo.h` (or whatever the active
// config is); the trace carries the literal string and the
// rendered output lives at the resolved path. The lifter must
// resolve the OUTPUT genex via the same Context the body lift
// uses, then continue down the normal lift path with the
// resolved rel as `outs = [...]`.
func TestRecoverFileGenerate_OutputSideGenexResolved(t *testing.T) {
	template := "// banner\n"
	rendered := []byte("// banner\n")
	// The fixture writes the rendered output at the RESOLVED
	// path on disk — that's what cmake does at generate-time.
	hostSrc, hostBuild := fileGenerateTestSetup(t, "src/banner.in", template, "Release/banner.h", rendered)
	calls := []shadow.FileGenerateCall{{
		File: filepath.Join(hostSrc, "CMakeLists.txt"),
		// Trace records the literal `$<CONFIG>` in OUTPUT.
		Output:   filepath.Join(hostBuild, "$<CONFIG>", "banner.h"),
		Input:    filepath.Join(hostSrc, "src/banner.in"),
		HasInput: true,
	}}
	cmakeVars := map[string]string{"CMAKE_BUILD_TYPE": "Release"}
	cc := newCodegenContext()
	out, err := recoverFileGenerate(calls, hostSrc, hostSrc, hostBuild, hostBuild, true, cmakeVars, nil, cc)
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if len(out) != 1 || out[0].RelOutput != "Release/banner.h" {
		t.Fatalf("outs: %+v; want one entry with rel=Release/banner.h", out)
	}
	if len(cc.Genrules) != 1 {
		t.Fatalf("Genrules: %+v", cc.Genrules)
	}
	g := cc.Genrules[0]
	if len(g.GenruleOuts) != 1 || g.GenruleOuts[0] != "Release/banner.h" {
		t.Errorf("genrule outs: %v want [Release/banner.h]", g.GenruleOuts)
	}
}

// TestRecoverFileGenerate_OutputSideGenexUnsupportedDropped
// asserts the drop behaviour preserves the pre-evaluator
// gate's contract: an OUTPUT genex the (a) evaluator can't
// resolve (here $<TARGET_FILE:...>, which always
// UnsupportedError's) still drops the call. No genrule emitted,
// no error surfaced — the operator's audit query for the
// missing file would surface this via the absence of a
// recovered output rather than via a malformed genrule.
func TestRecoverFileGenerate_OutputSideGenexUnsupportedDropped(t *testing.T) {
	hostSrc, hostBuild := fileGenerateTestSetup(t, "src/b.in", "x\n", "Release/b.h", []byte("x\n"))
	calls := []shadow.FileGenerateCall{{
		File:     filepath.Join(hostSrc, "CMakeLists.txt"),
		Output:   filepath.Join(hostBuild, "$<TARGET_FILE:foo>"),
		Input:    filepath.Join(hostSrc, "src/b.in"),
		HasInput: true,
	}}
	cmakeVars := map[string]string{"CMAKE_BUILD_TYPE": "Release"}
	cc := newCodegenContext()
	out, err := recoverFileGenerate(calls, hostSrc, hostSrc, hostBuild, hostBuild, true, cmakeVars, nil, cc)
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if len(out) != 0 {
		t.Errorf("expected no recovered outputs for unresolvable OUTPUT genex; got %+v", out)
	}
	if len(cc.Genrules) != 0 {
		t.Errorf("expected no genrules; got %+v", cc.Genrules)
	}
}

// TestRecoverFileGenerate_OutputSideGenexEmptyContextDropped
// covers the no-Context refusal: $<CONFIG> in OUTPUT without
// CMAKE_BUILD_TYPE in cmakeVars surfaces as UnsupportedError
// from the evaluator (Context.Config is empty) and the call
// is dropped, same as before the evaluator existed.
func TestRecoverFileGenerate_OutputSideGenexEmptyContextDropped(t *testing.T) {
	hostSrc, hostBuild := fileGenerateTestSetup(t, "src/b.in", "x\n", "Release/b.h", []byte("x\n"))
	calls := []shadow.FileGenerateCall{{
		File:     filepath.Join(hostSrc, "CMakeLists.txt"),
		Output:   filepath.Join(hostBuild, "$<CONFIG>", "b.h"),
		Input:    filepath.Join(hostSrc, "src/b.in"),
		HasInput: true,
	}}
	cc := newCodegenContext()
	// nil cmakeVars → empty Context → $<CONFIG> refuses → drop.
	out, err := recoverFileGenerate(calls, hostSrc, hostSrc, hostBuild, hostBuild, true, nil, nil, cc)
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if len(out) != 0 || len(cc.Genrules) != 0 {
		t.Errorf("expected drop on empty Context; got outs=%+v genrules=%+v", out, cc.Genrules)
	}
}

// TestRecoverFileGenerate_InputArgGenexResolved covers the
// (a)-shape INPUT-arg resolution: a call recorded as
// `INPUT $<CONFIG>/foo.in` is resolved at convert time
// against the same Context the body lift consults, the
// resolved literal becomes the on-disk template path, and
// the genrule lifts normally with srcs pointing at the
// resolved template.
func TestRecoverFileGenerate_InputArgGenexResolved(t *testing.T) {
	template := "#define BANNER \"hi\"\n"
	rendered := []byte("#define BANNER \"hi\"\n")
	hostSrc, hostBuild := fileGenerateTestSetup(t, "Release/banner.h.in", template, "banner.h", rendered)
	calls := []shadow.FileGenerateCall{{
		File: filepath.Join(hostSrc, "CMakeLists.txt"),
		// Trace records the literal `$<CONFIG>` in INPUT.
		Input:    filepath.Join(hostSrc, "$<CONFIG>", "banner.h.in"),
		Output:   filepath.Join(hostBuild, "banner.h"),
		HasInput: true,
	}}
	cmakeVars := map[string]string{"CMAKE_BUILD_TYPE": "Release"}
	cc := newCodegenContext()
	if _, err := recoverFileGenerate(calls, hostSrc, hostSrc, hostBuild, hostBuild, true, cmakeVars, nil, cc); err != nil {
		t.Fatalf("recover: %v", err)
	}
	if len(cc.Genrules) != 1 {
		t.Fatalf("Genrules: %+v", cc.Genrules)
	}
	g := cc.Genrules[0]
	if len(g.Srcs) != 1 || g.Srcs[0] != "Release/banner.h.in" {
		t.Errorf("srcs: %v want [Release/banner.h.in]", g.Srcs)
	}
	if !hasTag(g.Tags, "cmake-codegen-lifted") {
		t.Errorf("lifted tag missing: %v", g.Tags)
	}
	if hasTag(g.Tags, "cmake-codegen-file-generate-genex") {
		t.Errorf("resolved INPUT-arg genex should NOT carry the legacy-fallback tag; got %v", g.Tags)
	}
}

// TestRecoverFileGenerate_InputArgGenexUnsupportedFallsBackToLegacy
// covers the fallthrough: $<TARGET_FILE:foo> in INPUT can't be
// resolved by the (a) evaluator, so the lifter retains the
// pre-evaluator behaviour — legacy fallback with the
// cmake-codegen-file-generate-genex audit tag.
func TestRecoverFileGenerate_InputArgGenexUnsupportedFallsBackToLegacy(t *testing.T) {
	hostSrc, hostBuild := fileGenerateTestSetup(t, "src/b.in", "x\n", "b.h", []byte("x\n"))
	calls := []shadow.FileGenerateCall{{
		File:     filepath.Join(hostSrc, "CMakeLists.txt"),
		Input:    filepath.Join(hostSrc, "$<TARGET_FILE:foo>"),
		Output:   filepath.Join(hostBuild, "b.h"),
		HasInput: true,
	}}
	cmakeVars := map[string]string{"CMAKE_BUILD_TYPE": "Release"}
	cc := newCodegenContext()
	if _, err := recoverFileGenerate(calls, hostSrc, hostSrc, hostBuild, hostBuild, true, cmakeVars, nil, cc); err != nil {
		t.Fatalf("recover: %v", err)
	}
	if len(cc.Genrules) != 1 {
		t.Fatalf("Genrules: %+v", cc.Genrules)
	}
	g := cc.Genrules[0]
	if hasTag(g.Tags, "cmake-codegen-lifted") {
		t.Errorf("unresolved INPUT-arg genex should NOT lift; got %v", g.Tags)
	}
	if !hasTag(g.Tags, "cmake-codegen-file-generate-genex") {
		t.Errorf("expected legacy-fallback tag; got %v", g.Tags)
	}
}

// TestRecoverFileGenerate_GenexEvaluatedWithTargetProperty
// exercises the (a) lift's TARGET_PROPERTY path: a template
// referencing `$<TARGET_PROPERTY:fglib,TYPE>` resolves at
// convert time against a Context populated with target info,
// and the marshaled Context payload includes the Targets dict.
func TestRecoverFileGenerate_GenexEvaluatedWithTargetProperty(t *testing.T) {
	template := "// fglib is a $<TARGET_PROPERTY:fglib,TYPE>\n"
	rendered := []byte("// fglib is a STATIC_LIBRARY\n")
	hostSrc, hostBuild := fileGenerateTestSetup(t, "src/g.in", template, "g.out", rendered)
	calls := []shadow.FileGenerateCall{{
		File:     filepath.Join(hostSrc, "CMakeLists.txt"),
		Output:   filepath.Join(hostBuild, "g.out"),
		Input:    filepath.Join(hostSrc, "src/g.in"),
		HasInput: true,
	}}
	genexTargets := map[string]genexeval.TargetInfo{
		"fglib": {Type: "STATIC_LIBRARY"},
	}
	cc := newCodegenContext()
	if _, err := recoverFileGenerate(calls, hostSrc, hostSrc, hostBuild, hostBuild, true, nil, genexTargets, cc); err != nil {
		t.Fatalf("recover: %v", err)
	}
	g := cc.Genrules[0]
	if !hasTag(g.Tags, "cmake-codegen-file-generate-genex-evaluated") {
		t.Errorf("expected (a) tag in %v", g.Tags)
	}
	rendEnc := base64.StdEncoding.EncodeToString(rendered)
	if strings.Contains(g.GenruleCmd, rendEnc) {
		t.Errorf("(a) lift should NOT embed rendered bytes")
	}
	// Targets dict must be present in the marshaled payload
	// since the template references TARGET_PROPERTY.
	blob := string(mustDecodeGenexContextBlob(t, g.GenruleCmd))
	if !strings.Contains(blob, `"targets"`) || !strings.Contains(blob, `"fglib"`) || !strings.Contains(blob, `STATIC_LIBRARY`) {
		t.Errorf("Targets dump missing from marshaled Context payload: %s", blob)
	}
}

// TestRecoverFileGenerate_GenexEvaluatedPrunesTargetsWhenUnused
// asserts the payload-pruning optimization: a template that
// references only CONFIG (no TARGET_PROPERTY) does NOT carry
// the Targets dump in the marshaled Context, keeping the
// lifted cmd small for the common case.
func TestRecoverFileGenerate_GenexEvaluatedPrunesTargetsWhenUnused(t *testing.T) {
	template := "// build: $<CONFIG>\n"
	rendered := []byte("// build: Release\n")
	hostSrc, hostBuild := fileGenerateTestSetup(t, "src/g.in", template, "g.out", rendered)
	calls := []shadow.FileGenerateCall{{
		File:     filepath.Join(hostSrc, "CMakeLists.txt"),
		Output:   filepath.Join(hostBuild, "g.out"),
		Input:    filepath.Join(hostSrc, "src/g.in"),
		HasInput: true,
	}}
	cmakeVars := map[string]string{"CMAKE_BUILD_TYPE": "Release"}
	genexTargets := map[string]genexeval.TargetInfo{
		"foo": {Type: "STATIC_LIBRARY", Sources: "a.c;b.c"},
	}
	cc := newCodegenContext()
	if _, err := recoverFileGenerate(calls, hostSrc, hostSrc, hostBuild, hostBuild, true, cmakeVars, genexTargets, cc); err != nil {
		t.Fatalf("recover: %v", err)
	}
	g := cc.Genrules[0]
	if !hasTag(g.Tags, "cmake-codegen-file-generate-genex-evaluated") {
		t.Errorf("expected (a) tag in %v", g.Tags)
	}
	blob := string(mustDecodeGenexContextBlob(t, g.GenruleCmd))
	if strings.Contains(blob, `"targets"`) {
		t.Errorf("unused Targets should be pruned; cmd carries the dict: %s", blob)
	}
}

// mustDecodeGenexContextBlob extracts and base64-decodes the
// GENEX_CONTEXT payload from the lifted cmd. Helper for the
// payload-shape tests.
func mustDecodeGenexContextBlob(t *testing.T, cmd string) []byte {
	t.Helper()
	const before = `echo `
	const after = ` | base64 -d > "$$GENEX_CONTEXT"`
	a := strings.Index(cmd, after)
	if a < 0 {
		t.Fatalf("cmd missing GENEX_CONTEXT decode pattern: %q", cmd)
	}
	b := strings.LastIndex(cmd[:a], before)
	if b < 0 {
		t.Fatalf("cmd's GENEX_CONTEXT pattern has no echo prefix: %q", cmd)
	}
	raw, err := base64.StdEncoding.DecodeString(cmd[b+len(before) : a])
	if err != nil {
		t.Fatalf("decode genex-context blob: %v", err)
	}
	return raw
}

// TestRecoverFileGenerate_GenexEvaluatedWithTargetFile
// exercises the (a) lift's TARGET_FILE path end-to-end at the
// lifter: a template with $<TARGET_FILE:foo> + a captured
// target carrying FileLocation produces a genrule whose cmd
// passes --target-file=foo=$(location :foo) for Bazel-time
// substitution. The marshaled Context payload must NOT carry
// the FileLocation (wire-omitted for srckey stability).
func TestRecoverFileGenerate_GenexEvaluatedWithTargetFile(t *testing.T) {
	template := "// foo lives at $<TARGET_FILE:foo>\n"
	rendered := []byte("// foo lives at /recording/build/libfoo.a\n")
	hostSrc, hostBuild := fileGenerateTestSetup(t, "src/g.in", template, "g.out", rendered)
	calls := []shadow.FileGenerateCall{{
		File:     filepath.Join(hostSrc, "CMakeLists.txt"),
		Output:   filepath.Join(hostBuild, "g.out"),
		Input:    filepath.Join(hostSrc, "src/g.in"),
		HasInput: true,
	}}
	// FileLocation is set to the recording-machine path
	// matching cmake's rendered output bytes — what
	// buildGenexTargets computes in production.
	genexTargets := map[string]genexeval.TargetInfo{
		"foo": {Type: "STATIC_LIBRARY", FileLocation: "/recording/build/libfoo.a"},
	}
	cc := newCodegenContext()
	if _, err := recoverFileGenerate(calls, hostSrc, hostSrc, hostBuild, hostBuild, true, nil, genexTargets, cc); err != nil {
		t.Fatalf("recover: %v", err)
	}
	g := cc.Genrules[0]
	if !hasTag(g.Tags, "cmake-codegen-file-generate-genex-evaluated") {
		t.Errorf("expected (a) tag in %v", g.Tags)
	}
	// cmd must carry the --target-file flag for foo.
	wantFlag := `--target-file=foo="$(location :foo)"`
	if !strings.Contains(g.GenruleCmd, wantFlag) {
		t.Errorf("cmd should pass %q; got %q", wantFlag, g.GenruleCmd)
	}
	// The marshaled Context payload must NOT contain the
	// recording-machine path (wire struct omits FileLocation).
	blob := string(mustDecodeGenexContextBlob(t, g.GenruleCmd))
	if strings.Contains(blob, "/recording/build/libfoo.a") {
		t.Errorf("FileLocation leaked into marshaled Context: %s", blob)
	}
	if strings.Contains(blob, "file_location") {
		t.Errorf("wire struct should not carry file_location key: %s", blob)
	}
}

// TestRecoverFileGenerate_GenexEvaluated_TargetFileRefsSorted
// asserts the --target-file flags emit in sorted order for
// stable lifted-cmd bytes across runs (vs. Go's randomized map
// iteration).
func TestRecoverFileGenerate_GenexEvaluated_TargetFileRefsSorted(t *testing.T) {
	template := "$<TARGET_FILE:zeta> $<TARGET_FILE:alpha> $<TARGET_FILE:mu>\n"
	rendered := []byte("/z /a /m\n")
	hostSrc, hostBuild := fileGenerateTestSetup(t, "src/g.in", template, "g.out", rendered)
	calls := []shadow.FileGenerateCall{{
		File:     filepath.Join(hostSrc, "CMakeLists.txt"),
		Output:   filepath.Join(hostBuild, "g.out"),
		Input:    filepath.Join(hostSrc, "src/g.in"),
		HasInput: true,
	}}
	genexTargets := map[string]genexeval.TargetInfo{
		"alpha": {FileLocation: "/a"},
		"mu":    {FileLocation: "/m"},
		"zeta":  {FileLocation: "/z"},
	}
	cc := newCodegenContext()
	if _, err := recoverFileGenerate(calls, hostSrc, hostSrc, hostBuild, hostBuild, true, nil, genexTargets, cc); err != nil {
		t.Fatalf("recover: %v", err)
	}
	cmd := cc.Genrules[0].GenruleCmd
	// The flags must appear in alphabetical order: alpha, mu, zeta.
	aIdx := strings.Index(cmd, "--target-file=alpha")
	mIdx := strings.Index(cmd, "--target-file=mu")
	zIdx := strings.Index(cmd, "--target-file=zeta")
	if aIdx < 0 || mIdx < 0 || zIdx < 0 {
		t.Fatalf("missing --target-file flags in cmd %q", cmd)
	}
	if !(aIdx < mIdx && mIdx < zIdx) {
		t.Errorf("--target-file flags not sorted: alpha=%d mu=%d zeta=%d", aIdx, mIdx, zIdx)
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
		File:     filepath.Join(hostSrc, "CMakeLists.txt"),
		Output:   filepath.Join(hostBuild, "g.out"),
		Input:    filepath.Join(hostSrc, "src/g.in"),
		HasInput: true,
	}}
	cc := newCodegenContext()
	if _, err := recoverFileGenerate(calls, hostSrc, hostSrc, hostBuild, hostBuild, false, map[string]string{"VERSION": "1.0"}, nil, cc); err != nil {
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
		File:       filepath.Join(hostSrc, "CMakeLists.txt"),
		Output:     filepath.Join(hostBuild, "skipped.txt"),
		Content:    "anything\n",
		HasContent: true,
	}}
	cc := newCodegenContext()
	out, err := recoverFileGenerate(calls, hostSrc, hostSrc, hostBuild, hostBuild, true, nil, nil, cc)
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
		File:       filepath.Join(hostSrc, "CMakeLists.txt"),
		Output:     filepath.Join(hostBuild, "g.txt"),
		Content:    "hi\n",
		HasContent: true,
	}
	cc := newCodegenContext()
	if _, err := recoverFileGenerate([]shadow.FileGenerateCall{call, call}, hostSrc, hostSrc, hostBuild, hostBuild, true, nil, nil, cc); err != nil {
		t.Fatalf("recover: %v", err)
	}
	if len(cc.Genrules) != 1 {
		t.Errorf("dedupe failed; got %d genrules", len(cc.Genrules))
	}
}

// TestRecoverFileGenerate_ContentEmpty_Lifted covers the
// `file(GENERATE OUTPUT ... CONTENT "")` shape: a legitimate
// cmake invocation that writes an empty output file. The
// lifter must distinguish this from "no CONTENT supplied at
// all" — string-emptiness as the discriminator would collapse
// the two and force a legacy fallback (or worse, skip the
// call). HasContent=true + Content="" should route through
// the CONTENT-form lift with --content-base64= carrying the
// empty body.
func TestRecoverFileGenerate_ContentEmpty_Lifted(t *testing.T) {
	rendered := []byte{} // cmake writes an empty file
	hostSrc, hostBuild := fileGenerateTestSetup(t, "", "", "empty.txt", rendered)
	calls := []shadow.FileGenerateCall{{
		File:       filepath.Join(hostSrc, "CMakeLists.txt"),
		Output:     filepath.Join(hostBuild, "empty.txt"),
		Content:    "",
		HasContent: true,
	}}
	cc := newCodegenContext()
	out, err := recoverFileGenerate(calls, hostSrc, hostSrc, hostBuild, hostBuild, true, nil, nil, cc)
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if len(out) != 1 || out[0].RelOutput != "empty.txt" {
		t.Fatalf("outs: %+v", out)
	}
	g := cc.Genrules[0]
	if !hasTag(g.Tags, "cmake-codegen-lifted") {
		t.Errorf("CONTENT \"\" should still lift; tags: %v", g.Tags)
	}
	// --content-base64= followed by a non-blob (space, then
	// "$@") confirms the empty body rode through.
	if !strings.Contains(g.GenruleCmd, "--content-base64= ") {
		t.Errorf("cmd should carry --content-base64= (empty blob); got %q", g.GenruleCmd)
	}
}

// TestRecoverFileGenerate_SkipsCollisionWithOtherLifter
// covers the cross-surface dedup: if another codegen lifter
// (configure_file or execute_process) already claimed an
// output path, recoverFileGenerate must skip rather than
// append a duplicate Bazel rule. cmake itself rejects two
// writers to the same path, so the case is "shouldn't
// happen" — but the guard keeps the recovered BUILD valid
// if it does.
func TestRecoverFileGenerate_SkipsCollisionWithOtherLifter(t *testing.T) {
	rendered := []byte("hi\n")
	hostSrc, hostBuild := fileGenerateTestSetup(t, "", "", "g.txt", rendered)
	cc := newCodegenContext()
	// Pre-seed OutToGenrule as if a sibling lifter already
	// recovered "g.txt".
	cc.OutToGenrule["g.txt"] = "gen_existing_from_other_lifter"
	calls := []shadow.FileGenerateCall{{
		File:       filepath.Join(hostSrc, "CMakeLists.txt"),
		Output:     filepath.Join(hostBuild, "g.txt"),
		Content:    "hi\n",
		HasContent: true,
	}}
	out, err := recoverFileGenerate(calls, hostSrc, hostSrc, hostBuild, hostBuild, true, nil, nil, cc)
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if len(out) != 0 {
		t.Errorf("collision should produce 0 outs; got %+v", out)
	}
	if len(cc.Genrules) != 0 {
		t.Errorf("collision should not append a genrule; got %+v", cc.Genrules)
	}
	if cc.OutToGenrule["g.txt"] != "gen_existing_from_other_lifter" {
		t.Errorf("collision should not overwrite the sibling lifter's claim; got %q", cc.OutToGenrule["g.txt"])
	}
}

// TestRecoverFileGenerate_InputArgGenexTagsLegacy covers the
// INPUT-arg genex case: cmake allows `$<...>` in the INPUT
// path itself (resolved at generate-time) and the trace keeps
// it literal. The lifter can't find the on-disk template, but
// must still tag the legacy fallback with
// cmake-codegen-file-generate-genex so the audit signal
// matches the body-level genex case.
func TestRecoverFileGenerate_InputArgGenexTagsLegacy(t *testing.T) {
	rendered := []byte("v=1\n")
	hostSrc, hostBuild := fileGenerateTestSetup(t, "", "", "v.h", rendered)
	calls := []shadow.FileGenerateCall{{
		File:   filepath.Join(hostSrc, "CMakeLists.txt"),
		Output: filepath.Join(hostBuild, "v.h"),
		// $<CONFIG> in the INPUT path: cmake would resolve at
		// generate-time, the trace records the unresolved
		// string, and resolveTemplatePath would otherwise just
		// fail silently.
		Input:    filepath.Join(hostSrc, "$<CONFIG>/v.in"),
		HasInput: true,
	}}
	cc := newCodegenContext()
	if _, err := recoverFileGenerate(calls, hostSrc, hostSrc, hostBuild, hostBuild, true, nil, nil, cc); err != nil {
		t.Fatalf("recover: %v", err)
	}
	if len(cc.Genrules) != 1 {
		t.Fatalf("expected legacy genrule emission; got %+v", cc.Genrules)
	}
	g := cc.Genrules[0]
	if hasTag(g.Tags, "cmake-codegen-lifted") {
		t.Errorf("INPUT-arg genex must NOT carry cmake-codegen-lifted; got %v", g.Tags)
	}
	if !hasTag(g.Tags, "cmake-codegen-file-generate-genex") {
		t.Errorf("INPUT-arg genex must carry the genex audit tag; got %v", g.Tags)
	}
}

// TestRecoverFileGenerate_OutputGenexDropped covers the case
// where the OUTPUT path itself contains a generator expression
// (`$<CONFIG>` in the filename). The trace records the literal
// `$<...>` so the lifter can't map it back to the on-disk
// filename without a genex evaluator. v1 drops the call —
// surfacing it as a failed disk read would be misleading
// (the build dir read would still fail even with a complete
// fixture), and there's no rel to attach to a placeholder
// genrule. Same Later-roadmap-bullet refusal class as the
// CONTENT/INPUT-genex fallback, just at the OUTPUT level
// where no audit tag can ride along.
func TestRecoverFileGenerate_OutputGenexDropped(t *testing.T) {
	hostSrc := t.TempDir()
	hostBuild := t.TempDir()
	calls := []shadow.FileGenerateCall{{
		File:       filepath.Join(hostSrc, "CMakeLists.txt"),
		Output:     filepath.Join(hostBuild, "$<CONFIG>/banner.h"),
		Content:    "hi\n",
		HasContent: true,
	}}
	cc := newCodegenContext()
	out, err := recoverFileGenerate(calls, hostSrc, hostSrc, hostBuild, hostBuild, true, nil, nil, cc)
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if len(out) != 0 {
		t.Errorf("OUTPUT genex should drop the call; got %+v", out)
	}
	if len(cc.Genrules) != 0 {
		t.Errorf("OUTPUT genex should not emit a genrule; got %+v", cc.Genrules)
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
