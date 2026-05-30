package lower

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/internal/cmakerun"
	"github.com/sstriker/buildstream-bazel/converter/internal/fileapi"
	"github.com/sstriker/buildstream-bazel/internal/genexeval"
	"github.com/sstriker/buildstream-bazel/internal/manifest"
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
	out, err := recoverFileGenerate(calls, hostSrc, hostSrc, hostBuild, hostBuild, true, map[string]string{"VERSION": "1.2.3"}, nil, nil, cc)
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
	out, err := recoverFileGenerate(calls, hostSrc, hostSrc, hostBuild, hostBuild, true, map[string]string{"VERSION": "9.9"}, nil, nil, cc)
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
// cmake-codegen-genex-unresolved audit tag. The cmake-
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
	if _, err := recoverFileGenerate(calls, hostSrc, hostSrc, hostBuild, hostBuild, true, map[string]string{"VERSION": "1.0"}, nil, nil, cc); err != nil {
		t.Fatalf("recover: %v", err)
	}
	g := cc.Genrules[0]
	if len(g.Srcs) != 0 {
		t.Errorf("legacy fallback should not stage srcs; got %v", g.Srcs)
	}
	if hasTag(g.Tags, "cmake-codegen-lifted") {
		t.Errorf("genex-bearing template should NOT carry cmake-codegen-lifted; got %v", g.Tags)
	}
	if !hasTag(g.Tags, "cmake-codegen-genex-unresolved") {
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
// cmake-codegen-genex-resolved so the audit can
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
	if _, err := recoverFileGenerate(calls, hostSrc, hostSrc, hostBuild, hostBuild, true, nil, nil, nil, cc); err != nil {
		t.Fatalf("recover: %v", err)
	}
	if len(cc.Genrules) != 1 {
		t.Fatalf("expected one genrule; got %d", len(cc.Genrules))
	}
	g := cc.Genrules[0]
	for _, want := range []string{
		"cmake-codegen-lifted",
		"cmake-codegen-genex-resolved",
	} {
		if !hasTag(g.Tags, want) {
			t.Errorf("missing tag %q in %v", want, g.Tags)
		}
	}
	if hasTag(g.Tags, "cmake-codegen-genex-unresolved") {
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
// embedded shape with cmake-codegen-genex-unresolved
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
	if _, err := recoverFileGenerate(calls, hostSrc, hostSrc, hostBuild, hostBuild, true, nil, nil, nil, cc); err != nil {
		t.Fatalf("recover: %v", err)
	}
	g := cc.Genrules[0]
	if hasTag(g.Tags, "cmake-codegen-lifted") {
		t.Errorf("extraction failure should fall back to legacy; got lifted tag in %v", g.Tags)
	}
	if !hasTag(g.Tags, "cmake-codegen-genex-unresolved") {
		t.Errorf("legacy fallback after extraction failure must carry the genex audit tag; got %v", g.Tags)
	}
	if hasTag(g.Tags, "cmake-codegen-genex-resolved") {
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
	if _, err := recoverFileGenerate(calls, hostSrc, hostSrc, hostBuild, hostBuild, true, cmakeVars, nil, nil, cc); err != nil {
		t.Fatalf("recover: %v", err)
	}
	if len(cc.Genrules) != 1 {
		t.Fatalf("expected one genrule; got %d", len(cc.Genrules))
	}
	g := cc.Genrules[0]
	for _, want := range []string{
		"cmake-codegen-lifted",
		"cmake-codegen-genex-resolved",
	} {
		if !hasTag(g.Tags, want) {
			t.Errorf("missing tag %q in %v", want, g.Tags)
		}
	}
	// Post Phase-3 tag collapse the (a) and (b) shapes share the
	// single cmake-codegen-genex-resolved tag; the (a)-vs-(b)
	// distinction is no longer a tag-level fact, so we assert it
	// via the cmd (--genex-context= is the (a) evaluator wire).
	if hasTag(g.Tags, "cmake-codegen-genex-unresolved") {
		t.Errorf("unexpected legacy-fallback tag in %v", g.Tags)
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
// with a typed-refused op (which the (a) evaluator refuses via
// UnsupportedError) routes to (b); (b) succeeds when the static
// surround can anchor the value; tag set carries the (b) facet,
// not (a)'s.
//
// Uses $<TARGET_OBJECTS:foo> with `foo` IN the local codemodel
// (passes the cross-package gate) but Objects EMPTY in the
// captured TargetInfo (so the (a) evaluator's evalTargetObjects
// returns UnsupportedError — typical pre-probe-genex offline
// state). The lifter falls through to (b) capture. Earlier this
// test used $<TARGET_FILE:foo> without genexTargets; the
// cross-package soundness gate now catches that shape, so the
// canonical "(a) refuses → (b) lifts" path is best demonstrated
// with TARGET_OBJECTS on a known-but-objects-empty target.
func TestRecoverFileGenerate_GenexEvaluatedFallsBackToCapturedOnUnsupportedOp(t *testing.T) {
	template := "// objs: $<TARGET_OBJECTS:foo>\n"
	rendered := []byte("// objs: /opt/build/foo.dir/a.c.o;/opt/build/foo.dir/b.c.o\n")
	hostSrc, hostBuild := fileGenerateTestSetup(t, "src/g.in", template, "g.out", rendered)
	calls := []shadow.FileGenerateCall{{
		File:     filepath.Join(hostSrc, "CMakeLists.txt"),
		Output:   filepath.Join(hostBuild, "g.out"),
		Input:    filepath.Join(hostSrc, "src/g.in"),
		HasInput: true,
	}}
	cmakeVars := map[string]string{"CMAKE_BUILD_TYPE": "Release"}
	// `foo` is locally known but its Objects field is empty —
	// matches the no-probe-genex offline state where the
	// codemodel surfaces the target but the OBJECT_LIBRARY's
	// .o list is unavailable. evalTargetObjects returns
	// UnsupportedError on empty Objects.
	genexTargets := map[string]genexeval.TargetInfo{
		"foo": {Type: "OBJECT_LIBRARY"},
	}
	cc := newCodegenContext()
	if _, err := recoverFileGenerate(calls, hostSrc, hostSrc, hostBuild, hostBuild, true, cmakeVars, genexTargets, nil, cc); err != nil {
		t.Fatalf("recover: %v", err)
	}
	g := cc.Genrules[0]
	// Post Phase-3 collapse the (a) evaluator and (b) capture
	// share cmake-codegen-genex-resolved; the (a)-refused-but-(b)-
	// succeeded shape is verified via the cmd wire below
	// (--genex-values= is the (b) capture, --genex-context= the
	// (a) evaluator).
	if !hasTag(g.Tags, "cmake-codegen-genex-resolved") {
		t.Errorf("expected (b) capture to resolve the genex; got %v", g.Tags)
	}
	if strings.Contains(g.GenruleCmd, "--genex-context=") {
		t.Errorf("(a) evaluator should have refused $<TARGET_OBJECTS:> with empty Objects; got %q", g.GenruleCmd)
	}
	if !strings.Contains(g.GenruleCmd, "--genex-values=") {
		t.Errorf("expected (b) capture wire --genex-values=; got %q", g.GenruleCmd)
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
	if _, err := recoverFileGenerate(calls, hostSrc, hostSrc, hostBuild, hostBuild, true, nil, nil, nil, cc); err != nil {
		t.Fatalf("recover: %v", err)
	}
	g := cc.Genrules[0]
	// Post Phase-3 collapse, (a) and (b) share the single
	// cmake-codegen-genex-resolved tag. The "(a) refused, (b)
	// succeeded" shape shows in the cmd wire: (b) uses
	// --genex-values=, (a) uses --genex-context=.
	if !hasTag(g.Tags, "cmake-codegen-genex-resolved") {
		t.Errorf("expected (b) capture to resolve the genex; got %v", g.Tags)
	}
	if strings.Contains(g.GenruleCmd, "--genex-context=") {
		t.Errorf("empty cmakeVars should make (a) refuse; got %q", g.GenruleCmd)
	}
	if !strings.Contains(g.GenruleCmd, "--genex-values=") {
		t.Errorf("expected (b) capture wire --genex-values=; got %q", g.GenruleCmd)
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
	out, err := recoverFileGenerate(calls, hostSrc, hostSrc, hostBuild, hostBuild, true, cmakeVars, nil, nil, cc)
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
	out, err := recoverFileGenerate(calls, hostSrc, hostSrc, hostBuild, hostBuild, true, cmakeVars, nil, nil, cc)
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
	out, err := recoverFileGenerate(calls, hostSrc, hostSrc, hostBuild, hostBuild, true, nil, nil, nil, cc)
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
	if _, err := recoverFileGenerate(calls, hostSrc, hostSrc, hostBuild, hostBuild, true, cmakeVars, nil, nil, cc); err != nil {
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
	if hasTag(g.Tags, "cmake-codegen-genex-unresolved") {
		t.Errorf("resolved INPUT-arg genex should NOT carry the legacy-fallback tag; got %v", g.Tags)
	}
}

// TestRecoverFileGenerate_InputArgGenexUnsupportedFallsBackToLegacy
// covers the fallthrough: $<TARGET_FILE:foo> in INPUT can't be
// resolved by the (a) evaluator, so the lifter retains the
// pre-evaluator behaviour — legacy fallback with the
// cmake-codegen-genex-unresolved audit tag.
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
	if _, err := recoverFileGenerate(calls, hostSrc, hostSrc, hostBuild, hostBuild, true, cmakeVars, nil, nil, cc); err != nil {
		t.Fatalf("recover: %v", err)
	}
	if len(cc.Genrules) != 1 {
		t.Fatalf("Genrules: %+v", cc.Genrules)
	}
	g := cc.Genrules[0]
	if hasTag(g.Tags, "cmake-codegen-lifted") {
		t.Errorf("unresolved INPUT-arg genex should NOT lift; got %v", g.Tags)
	}
	if !hasTag(g.Tags, "cmake-codegen-genex-unresolved") {
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
	if _, err := recoverFileGenerate(calls, hostSrc, hostSrc, hostBuild, hostBuild, true, nil, genexTargets, nil, cc); err != nil {
		t.Fatalf("recover: %v", err)
	}
	g := cc.Genrules[0]
	if !hasTag(g.Tags, "cmake-codegen-genex-resolved") {
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
	if _, err := recoverFileGenerate(calls, hostSrc, hostSrc, hostBuild, hostBuild, true, cmakeVars, genexTargets, nil, cc); err != nil {
		t.Fatalf("recover: %v", err)
	}
	g := cc.Genrules[0]
	if !hasTag(g.Tags, "cmake-codegen-genex-resolved") {
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
	if _, err := recoverFileGenerate(calls, hostSrc, hostSrc, hostBuild, hostBuild, true, nil, genexTargets, nil, cc); err != nil {
		t.Fatalf("recover: %v", err)
	}
	g := cc.Genrules[0]
	if !hasTag(g.Tags, "cmake-codegen-genex-resolved") {
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

// TestRecoverFileGenerate_GenexEvaluatedWithTargetFileVariants
// covers the on-disk-path variant ops (FILE_DIR, FILE_NAME,
// LINKER_FILE*, SONAME_FILE): a template referencing any of
// them must trigger the same `--target-file=foo=$(location :foo)`
// flag emission as TARGET_FILE, since all six derive from
// FileLocation at Bazel time via the genexeval evaluator.
// One target referenced via three different op forms must emit
// exactly ONE flag (the union is what the lifter needs to
// stage, not one flag per op-form occurrence).
func TestRecoverFileGenerate_GenexEvaluatedWithTargetFileVariants(t *testing.T) {
	template := "" +
		"// dir:    $<TARGET_FILE_DIR:foo>\n" +
		"// name:   $<TARGET_FILE_NAME:foo>\n" +
		"// linker: $<TARGET_LINKER_FILE:foo>\n"
	rendered := []byte("" +
		"// dir:    /recording/build/lib\n" +
		"// name:   libfoo.a\n" +
		"// linker: /recording/build/lib/libfoo.a\n")
	hostSrc, hostBuild := fileGenerateTestSetup(t, "src/g.in", template, "g.out", rendered)
	calls := []shadow.FileGenerateCall{{
		File:     filepath.Join(hostSrc, "CMakeLists.txt"),
		Output:   filepath.Join(hostBuild, "g.out"),
		Input:    filepath.Join(hostSrc, "src/g.in"),
		HasInput: true,
	}}
	genexTargets := map[string]genexeval.TargetInfo{
		"foo": {Type: "STATIC_LIBRARY", FileLocation: "/recording/build/lib/libfoo.a"},
	}
	cc := newCodegenContext()
	if _, err := recoverFileGenerate(calls, hostSrc, hostSrc, hostBuild, hostBuild, true, nil, genexTargets, nil, cc); err != nil {
		t.Fatalf("recover: %v", err)
	}
	g := cc.Genrules[0]
	if !hasTag(g.Tags, "cmake-codegen-genex-resolved") {
		t.Errorf("expected (a) tag in %v", g.Tags)
	}
	// Exactly one --target-file flag for foo (not one per op form).
	count := strings.Count(g.GenruleCmd, "--target-file=foo=")
	if count != 1 {
		t.Errorf("expected exactly 1 --target-file=foo= flag (three op forms collapse to one wire), got %d in %q", count, g.GenruleCmd)
	}
	wantFlag := `--target-file=foo="$(location :foo)"`
	if !strings.Contains(g.GenruleCmd, wantFlag) {
		t.Errorf("cmd should pass %q; got %q", wantFlag, g.GenruleCmd)
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
	if _, err := recoverFileGenerate(calls, hostSrc, hostSrc, hostBuild, hostBuild, true, nil, genexTargets, nil, cc); err != nil {
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

// TestRecoverFileGenerate_GenexEvaluatedWithTargetObjects
// exercises the (a) lift's TARGET_OBJECTS path end-to-end at the
// lifter: a template with $<TARGET_OBJECTS:objlib> + a captured
// OBJECT_LIBRARY carrying Objects (the probe-genex hook's
// recorded .o list) produces a genrule whose cmd passes
// --target-objects=objlib="$(echo $(locations :objlib) | tr ' ' ':')"
// for Bazel-time substitution. The marshaled Context payload
// carries Objects (no wire-omit, unlike FileLocation) because the
// authoritative value comes from the probe at convert time; the
// Bazel-time --target-objects flag is what the cross-machine
// executor actually consumes.
func TestRecoverFileGenerate_GenexEvaluatedWithTargetObjects(t *testing.T) {
	template := "// objs: $<TARGET_OBJECTS:objlib>\n"
	objectsList := "/recording/build/CMakeFiles/objlib.dir/a.c.o;/recording/build/CMakeFiles/objlib.dir/b.c.o"
	rendered := []byte("// objs: " + objectsList + "\n")
	hostSrc, hostBuild := fileGenerateTestSetup(t, "src/g.in", template, "g.out", rendered)
	calls := []shadow.FileGenerateCall{{
		File:     filepath.Join(hostSrc, "CMakeLists.txt"),
		Output:   filepath.Join(hostBuild, "g.out"),
		Input:    filepath.Join(hostSrc, "src/g.in"),
		HasInput: true,
	}}
	// Objects is populated (matches what buildGenexTargets does
	// when probes carry a TARGET_OBJECTS:objlib entry). The (a)
	// evaluator consults this for the convert-time byte-equal
	// check; the lifted cmd's --target-objects flag is what
	// overrides at Bazel time.
	genexTargets := map[string]genexeval.TargetInfo{
		"objlib": {Type: "OBJECT_LIBRARY", Objects: objectsList},
	}
	cc := newCodegenContext()
	if _, err := recoverFileGenerate(calls, hostSrc, hostSrc, hostBuild, hostBuild, true, nil, genexTargets, nil, cc); err != nil {
		t.Fatalf("recover: %v", err)
	}
	g := cc.Genrules[0]
	if !hasTag(g.Tags, "cmake-codegen-genex-resolved") {
		t.Errorf("expected (a) tag in %v", g.Tags)
	}
	// cmd must carry the --target-objects flag for objlib with the
	// $(locations :t) | tr ' ' ':' shell rewrite. The exact shape
	// is load-bearing — operators reading the lifted BUILD file
	// shouldn't need to dig to figure out which paths get expanded.
	wantFlag := `--target-objects=objlib="$$(echo $(locations :objlib) | tr ' ' ':')"`
	if !strings.Contains(g.GenruleCmd, wantFlag) {
		t.Errorf("cmd should pass %q; got %q", wantFlag, g.GenruleCmd)
	}
}

// TestRecoverFileGenerate_GenexEvaluatedWithTargetObjects_Sorted
// asserts the --target-objects flags emit in sorted order for
// stable lifted-cmd bytes across runs (vs. Go's randomized map
// iteration).
func TestRecoverFileGenerate_GenexEvaluatedWithTargetObjects_Sorted(t *testing.T) {
	template := "$<TARGET_OBJECTS:zeta> $<TARGET_OBJECTS:alpha> $<TARGET_OBJECTS:mu>\n"
	rendered := []byte("/z.o /a.o /m.o\n")
	hostSrc, hostBuild := fileGenerateTestSetup(t, "src/g.in", template, "g.out", rendered)
	calls := []shadow.FileGenerateCall{{
		File:     filepath.Join(hostSrc, "CMakeLists.txt"),
		Output:   filepath.Join(hostBuild, "g.out"),
		Input:    filepath.Join(hostSrc, "src/g.in"),
		HasInput: true,
	}}
	genexTargets := map[string]genexeval.TargetInfo{
		"alpha": {Type: "OBJECT_LIBRARY", Objects: "/a.o"},
		"mu":    {Type: "OBJECT_LIBRARY", Objects: "/m.o"},
		"zeta":  {Type: "OBJECT_LIBRARY", Objects: "/z.o"},
	}
	cc := newCodegenContext()
	if _, err := recoverFileGenerate(calls, hostSrc, hostSrc, hostBuild, hostBuild, true, nil, genexTargets, nil, cc); err != nil {
		t.Fatalf("recover: %v", err)
	}
	cmd := cc.Genrules[0].GenruleCmd
	// The flags must appear in alphabetical order: alpha, mu, zeta.
	aIdx := strings.Index(cmd, "--target-objects=alpha")
	mIdx := strings.Index(cmd, "--target-objects=mu")
	zIdx := strings.Index(cmd, "--target-objects=zeta")
	if aIdx < 0 || mIdx < 0 || zIdx < 0 {
		t.Fatalf("missing --target-objects flags in cmd %q", cmd)
	}
	if !(aIdx < mIdx && mIdx < zIdx) {
		t.Errorf("--target-objects flags not sorted: alpha=%d mu=%d zeta=%d", aIdx, mIdx, zIdx)
	}
}

// TestRecoverFileGenerate_GenexEvaluatedWithTargetObjects_Deduped
// asserts that one target referenced via N TARGET_OBJECTS
// occurrences collapses to ONE --target-objects flag (vs N flags).
// The Bazel-time expansion is the same path list regardless of
// how many references the template carries, so emitting one flag
// per occurrence would waste bytes and break the "stable cmd"
// contract on edits that change reference count without changing
// the target set.
func TestRecoverFileGenerate_GenexEvaluatedWithTargetObjects_Deduped(t *testing.T) {
	template := "// a: $<TARGET_OBJECTS:objlib>\n// b: $<TARGET_OBJECTS:objlib>\n"
	rendered := []byte("// a: /o.o\n// b: /o.o\n")
	hostSrc, hostBuild := fileGenerateTestSetup(t, "src/g.in", template, "g.out", rendered)
	calls := []shadow.FileGenerateCall{{
		File:     filepath.Join(hostSrc, "CMakeLists.txt"),
		Output:   filepath.Join(hostBuild, "g.out"),
		Input:    filepath.Join(hostSrc, "src/g.in"),
		HasInput: true,
	}}
	genexTargets := map[string]genexeval.TargetInfo{
		"objlib": {Type: "OBJECT_LIBRARY", Objects: "/o.o"},
	}
	cc := newCodegenContext()
	if _, err := recoverFileGenerate(calls, hostSrc, hostSrc, hostBuild, hostBuild, true, nil, genexTargets, nil, cc); err != nil {
		t.Fatalf("recover: %v", err)
	}
	count := strings.Count(cc.Genrules[0].GenruleCmd, "--target-objects=objlib=")
	if count != 1 {
		t.Errorf("expected exactly 1 --target-objects=objlib= flag (N occurrences collapse to one wire), got %d in %q", count, cc.Genrules[0].GenruleCmd)
	}
}

// TestExtractTargetObjectsRefs covers the prefix scanner: dedupe,
// sorted order, multiple targets, no false-positive on
// $<TARGET_FILE:foo> (the lifter's two-axis extraction keeps the
// flag emissions distinct).
func TestExtractTargetObjectsRefs(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{"none", "no genex here", nil},
		{"single", "$<TARGET_OBJECTS:foo>", []string{"foo"}},
		{"dedupes", "$<TARGET_OBJECTS:foo>+$<TARGET_OBJECTS:foo>", []string{"foo"}},
		{"sorted", "$<TARGET_OBJECTS:zeta> $<TARGET_OBJECTS:alpha>", []string{"alpha", "zeta"}},
		{"no false positive on TARGET_FILE", "$<TARGET_FILE:foo>+$<TARGET_OBJECTS:bar>", []string{"bar"}},
		{"empty name skipped", "$<TARGET_OBJECTS:>", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := extractTargetObjectsRefs([]byte(c.in))
			if !sliceEq(got, c.want) {
				t.Errorf("extractTargetObjectsRefs(%q) = %v; want %v", c.in, got, c.want)
			}
		})
	}
}

func sliceEq(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
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
	if _, err := recoverFileGenerate(calls, hostSrc, hostSrc, hostBuild, hostBuild, false, map[string]string{"VERSION": "1.0"}, nil, nil, cc); err != nil {
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
	out, err := recoverFileGenerate(calls, hostSrc, hostSrc, hostBuild, hostBuild, true, nil, nil, nil, cc)
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
	if _, err := recoverFileGenerate([]shadow.FileGenerateCall{call, call}, hostSrc, hostSrc, hostBuild, hostBuild, true, nil, nil, nil, cc); err != nil {
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
	out, err := recoverFileGenerate(calls, hostSrc, hostSrc, hostBuild, hostBuild, true, nil, nil, nil, cc)
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
	out, err := recoverFileGenerate(calls, hostSrc, hostSrc, hostBuild, hostBuild, true, nil, nil, nil, cc)
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
// cmake-codegen-genex-unresolved so the audit signal
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
	if _, err := recoverFileGenerate(calls, hostSrc, hostSrc, hostBuild, hostBuild, true, nil, nil, nil, cc); err != nil {
		t.Fatalf("recover: %v", err)
	}
	if len(cc.Genrules) != 1 {
		t.Fatalf("expected legacy genrule emission; got %+v", cc.Genrules)
	}
	g := cc.Genrules[0]
	if hasTag(g.Tags, "cmake-codegen-lifted") {
		t.Errorf("INPUT-arg genex must NOT carry cmake-codegen-lifted; got %v", g.Tags)
	}
	if !hasTag(g.Tags, "cmake-codegen-genex-unresolved") {
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
	out, err := recoverFileGenerate(calls, hostSrc, hostSrc, hostBuild, hostBuild, true, nil, nil, nil, cc)
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

// TestRecoverFileGenerate_CrossPackageTargetFile_Refused
// covers the soundness gate from
// ROADMAP.md: a template
// referencing `$<TARGET_FILE:Foo::bar>` for a target NOT in
// the local codemodel AND NOT in the imports.json manifest
// must refuse the lift entirely. The genrule still emits (so
// the consumer-attribution pass finds the output) but its cmd
// is a `false; echo <diagnostic>` exit-1 stub and the audit
// tag set carries cmake-codegen-genex-cross-package.
//
// This is the fix for a latent soundness bug: pre-gate, the
// lift would refuse via (a) and fall through to (b), which
// captures cmake's rendered bytes (the recording-machine
// absolute path) and ships them into Bazel — those paths
// don't exist on the executor.
func TestRecoverFileGenerate_CrossPackageTargetFile_Refused(t *testing.T) {
	template := "// tool=$<TARGET_FILE:Foo::bar>\n"
	rendered := []byte("// tool=/recording/build/libbar.a\n")
	hostSrc, hostBuild := fileGenerateTestSetup(t, "src/g.in", template, "g.out", rendered)
	calls := []shadow.FileGenerateCall{{
		File:     filepath.Join(hostSrc, "CMakeLists.txt"),
		Output:   filepath.Join(hostBuild, "g.out"),
		Input:    filepath.Join(hostSrc, "src/g.in"),
		HasInput: true,
	}}
	cc := newCodegenContext()
	// Empty genexTargets + nil imports resolver: Foo::bar
	// resolves to neither → soundness gate fires.
	if _, err := recoverFileGenerate(calls, hostSrc, hostSrc, hostBuild, hostBuild, true, map[string]string{"CMAKE_BUILD_TYPE": "Release"}, nil, nil, cc); err != nil {
		t.Fatalf("recover: %v", err)
	}
	if len(cc.Genrules) != 1 {
		t.Fatalf("Genrules: %+v", cc.Genrules)
	}
	g := cc.Genrules[0]
	if !hasTag(g.Tags, "cmake-codegen-genex-cross-package") {
		t.Errorf("cross-package refusal tag missing: %v", g.Tags)
	}
	// The lift-success facets must NOT appear — this isn't a
	// successful lift, it's a refusal stub.
	for _, banned := range []string{
		"cmake-codegen-lifted",
		"cmake-codegen-genex-resolved",
	} {
		if hasTag(g.Tags, banned) {
			t.Errorf("refusal stub should NOT carry %q; got %v", banned, g.Tags)
		}
	}
	// The cmd is a fail-with-diagnostic stub.
	if !strings.Contains(g.GenruleCmd, "exit 1") {
		t.Errorf("refusal stub cmd should exit 1; got %q", g.GenruleCmd)
	}
	if !strings.Contains(g.GenruleCmd, "Foo::bar") {
		t.Errorf("refusal stub cmd should name the unresolved target Foo::bar; got %q", g.GenruleCmd)
	}
}

// TestRecoverFileGenerate_CrossPackageTargetFile_VariantOps
// confirms the soundness gate also fires for the six
// FileLocation-derived variant ops (FILE_DIR, FILE_NAME,
// LINKER_FILE, LINKER_FILE_DIR, LINKER_FILE_NAME,
// SONAME_FILE), not just TARGET_FILE itself. All seven share
// the same wire under the (a) shape and the same wrong-bytes
// hazard under (b)/legacy fallback.
func TestRecoverFileGenerate_CrossPackageTargetFile_VariantOps(t *testing.T) {
	// Each variant in turn, against an unresolvable target.
	variants := []string{
		"TARGET_FILE_DIR",
		"TARGET_FILE_NAME",
		"TARGET_LINKER_FILE",
		"TARGET_LINKER_FILE_DIR",
		"TARGET_LINKER_FILE_NAME",
		"TARGET_SONAME_FILE",
	}
	for _, op := range variants {
		t.Run(op, func(t *testing.T) {
			template := "// path=$<" + op + ":Foo::bar>\n"
			rendered := []byte("// path=/recording/build/libbar.a\n")
			hostSrc, hostBuild := fileGenerateTestSetup(t, "src/g.in", template, op+".out", rendered)
			calls := []shadow.FileGenerateCall{{
				File:     filepath.Join(hostSrc, "CMakeLists.txt"),
				Output:   filepath.Join(hostBuild, op+".out"),
				Input:    filepath.Join(hostSrc, "src/g.in"),
				HasInput: true,
			}}
			cc := newCodegenContext()
			if _, err := recoverFileGenerate(calls, hostSrc, hostSrc, hostBuild, hostBuild, true, nil, nil, nil, cc); err != nil {
				t.Fatalf("recover: %v", err)
			}
			g := cc.Genrules[0]
			if !hasTag(g.Tags, "cmake-codegen-genex-cross-package") {
				t.Errorf("%s: cross-package refusal tag missing: %v", op, g.Tags)
			}
		})
	}
}

// TestRecoverFileGenerate_CrossPackageTargetFile_Resolvable
// confirms the soundness gate does NOT fire when the target
// resolves cleanly via the imports.json manifest. This test
// passes genexTargets=nil to recoverFileGenerate explicitly —
// PR 2's imports-fold (buildGenexTargets folding manifest
// entries into the TargetInfo map) doesn't run here, so the
// (a) lift's FileLocation lookup misses and the call routes
// to (b)/legacy. The dedicated PR 2 end-to-end test below
// (TestRecoverFileGenerate_CrossPackageTargetFile_ResolvedLift)
// covers the full resolved-lift path. This test pins the
// narrower invariant: even without the fold, the soundness
// gate must stay quiet when the resolver knows the target.
func TestRecoverFileGenerate_CrossPackageTargetFile_Resolvable(t *testing.T) {
	template := "// tool=$<TARGET_FILE:Foo::bar>\n"
	rendered := []byte("// tool=/some/path/libbar.a\n")
	hostSrc, hostBuild := fileGenerateTestSetup(t, "src/g.in", template, "g.out", rendered)
	calls := []shadow.FileGenerateCall{{
		File:     filepath.Join(hostSrc, "CMakeLists.txt"),
		Output:   filepath.Join(hostBuild, "g.out"),
		Input:    filepath.Join(hostSrc, "src/g.in"),
		HasInput: true,
	}}
	// Construct an imports.json resolver via Index — same path
	// production uses (manifest.Load → manifest.Index).
	imports, err := manifest.Index(&manifest.Imports{
		Version: 1,
		Elements: []*manifest.Element{{
			Name: "components/foo",
			Exports: []*manifest.Export{{
				CMakeTarget: "Foo::bar",
				BazelLabel:  "//elements/components/foo:bar",
			}},
		}},
	})
	if err != nil {
		t.Fatalf("manifest.Index: %v", err)
	}
	cc := newCodegenContext()
	if _, err := recoverFileGenerate(calls, hostSrc, hostSrc, hostBuild, hostBuild, true, nil, nil, imports, cc); err != nil {
		t.Fatalf("recover: %v", err)
	}
	g := cc.Genrules[0]
	if hasTag(g.Tags, "cmake-codegen-genex-cross-package") {
		t.Errorf("soundness gate should NOT fire when imports manifest resolves Foo::bar; got tags %v", g.Tags)
	}
	// Without PR 2's resolution wiring, the lift still falls
	// through to (b) (genexTargets has no FileLocation for
	// Foo::bar). That's expected and documented. The (b) bytes
	// are wrong-but-already-broken — PR 2 fixes that. This test
	// pins that the SOUNDNESS GATE quiets correctly when the
	// resolver knows the target; the wrong-bytes-via-(b) leak
	// for resolvable cases is what PR 2 closes.
	if !hasTag(g.Tags, "cmake-codegen-genex-resolved") &&
		!hasTag(g.Tags, "cmake-codegen-genex-unresolved") {
		t.Errorf("expected (b) or legacy genex tag in fallback; got %v", g.Tags)
	}
}

// TestBuildGenexTargets_FoldsProbeData covers Phase 3's probe-data
// fold: when GenexProbes carries entries, buildGenexTargets merges
// the INTERFACE_* aggregates and Objects list into the matching
// codemodel-derived TargetInfo. Probes for unknown targets (no
// codemodel entry) are dropped silently.
func TestBuildGenexTargets_FoldsProbeData(t *testing.T) {
	reply := &fileapi.Reply{
		Targets: map[string]fileapi.Target{
			"foo": {
				Name:      "foo",
				Type:      "STATIC_LIBRARY",
				Artifacts: []fileapi.TargetArtifact{{Path: "libfoo.a"}},
			},
			"objlib": {
				Name:      "objlib",
				Type:      "OBJECT_LIBRARY",
				Artifacts: []fileapi.TargetArtifact{},
			},
		},
	}
	probes := []cmakerun.GenexProbe{
		{
			Name: "foo",
			Type: "STATIC_LIBRARY",
			Interface: map[string]string{
				"INCLUDE_DIRECTORIES": "/src/include",
				"LINK_LIBRARIES":      "bar;baz",
				"COMPILE_DEFINITIONS": "FOO=1",
			},
		},
		{
			Name:    "objlib",
			Type:    "OBJECT_LIBRARY",
			Objects: "/build/CMakeFiles/objlib.dir/a.c.o;/build/CMakeFiles/objlib.dir/b.c.o",
		},
		{
			// No matching codemodel entry — dropped silently.
			Name: "ghost",
			Type: "EXECUTABLE",
		},
	}
	got := buildGenexTargets(reply, "/build", probes, nil, nil)

	if got["foo"].InterfaceIncludeDirectories != "/src/include" {
		t.Errorf("foo InterfaceIncludeDirectories = %q", got["foo"].InterfaceIncludeDirectories)
	}
	if got["foo"].InterfaceLinkLibraries != "bar;baz" {
		t.Errorf("foo InterfaceLinkLibraries = %q", got["foo"].InterfaceLinkLibraries)
	}
	if got["foo"].InterfaceCompileDefinitions != "FOO=1" {
		t.Errorf("foo InterfaceCompileDefinitions = %q", got["foo"].InterfaceCompileDefinitions)
	}
	if got["objlib"].Objects != "/build/CMakeFiles/objlib.dir/a.c.o;/build/CMakeFiles/objlib.dir/b.c.o" {
		t.Errorf("objlib Objects = %q", got["objlib"].Objects)
	}
	if _, ok := got["ghost"]; ok {
		t.Errorf("ghost target should have been dropped: %+v", got["ghost"])
	}
	// Codemodel-side fields survive the merge.
	if got["foo"].Type != "STATIC_LIBRARY" {
		t.Errorf("foo Type lost after merge: %q", got["foo"].Type)
	}
	if got["foo"].FileLocation != "/build/libfoo.a" {
		t.Errorf("foo FileLocation lost after merge: %q", got["foo"].FileLocation)
	}
}

// TestBuildGenexTargets_NoProbes confirms behavior is unchanged
// when probes is empty — the probe-fold path is opt-in.
func TestBuildGenexTargets_NoProbes(t *testing.T) {
	reply := &fileapi.Reply{
		Targets: map[string]fileapi.Target{
			"foo": {Name: "foo", Type: "STATIC_LIBRARY"},
		},
	}
	got := buildGenexTargets(reply, "/build", nil, nil, nil)
	if got["foo"].InterfaceIncludeDirectories != "" {
		t.Errorf("InterfaceIncludeDirectories should be empty without probe; got %q", got["foo"].InterfaceIncludeDirectories)
	}
	if got["foo"].Objects != "" {
		t.Errorf("Objects should be empty without probe; got %q", got["foo"].Objects)
	}
}

// TestBuildGenexTargets_FoldsImportedTargets covers PR 2's
// imports-manifest fold: each Export surfaces as an Imported=true
// TargetInfo keyed by the namespaced cmake name, with
// FileLocation seeded from LinkPaths[0] so the byte-equal check
// at convert time matches cmake's `$<TARGET_FILE:Foo::bar>`
// expansion to the IMPORTED_LOCATION-recorded absolute path.
func TestBuildGenexTargets_FoldsImportedTargets(t *testing.T) {
	reply := &fileapi.Reply{
		Targets: map[string]fileapi.Target{
			"local": {Name: "local", Type: "STATIC_LIBRARY"},
		},
	}
	imports, err := manifest.Index(&manifest.Imports{
		Version: 1,
		Elements: []*manifest.Element{{
			Name: "components/foo",
			Exports: []*manifest.Export{{
				CMakeTarget: "Foo::bar",
				BazelLabel:  "//elements/components/foo:bar",
				LinkPaths:   []string{"/prefix/lib/libbar.a"},
			}},
		}},
	})
	if err != nil {
		t.Fatalf("manifest.Index: %v", err)
	}
	got := buildGenexTargets(reply, "/build", nil, nil, imports)
	// Local target survives untouched.
	if got["local"].Type != "STATIC_LIBRARY" {
		t.Errorf("local Type lost: %q", got["local"].Type)
	}
	if got["local"].Imported {
		t.Errorf("local should NOT carry Imported=true")
	}
	// Imported target lands with the manifest's LinkPaths[0] as
	// FileLocation. Imported=true marks it as PR 2-resolved.
	imp, ok := got["Foo::bar"]
	if !ok {
		t.Fatalf("imported Foo::bar missing from genexTargets: %+v", got)
	}
	if !imp.Imported {
		t.Errorf("imported target should carry Imported=true; got %+v", imp)
	}
	if imp.FileLocation != "/prefix/lib/libbar.a" {
		t.Errorf("imported FileLocation = %q, want /prefix/lib/libbar.a", imp.FileLocation)
	}
}

// TestBuildGenexTargets_ImportsFold_NoLinkPathsLeavesFileLocationEmpty
// covers the manifest entry with no LinkPaths: the fold injects
// the target (so the soundness gate sees it resolves) but
// FileLocation stays empty. The (a) lift's byte-equal check
// then fails for that target and the call falls through to (b)
// — same fallback shape as a manifest entry that does carry
// LinkPaths but whose value doesn't match cmake's rendered
// bytes.
func TestBuildGenexTargets_ImportsFold_NoLinkPathsLeavesFileLocationEmpty(t *testing.T) {
	reply := &fileapi.Reply{
		Targets: map[string]fileapi.Target{
			"local": {Name: "local", Type: "STATIC_LIBRARY"},
		},
	}
	imports, err := manifest.Index(&manifest.Imports{
		Version: 1,
		Elements: []*manifest.Element{{
			Name: "components/foo",
			Exports: []*manifest.Export{{
				CMakeTarget: "Foo::bar",
				BazelLabel:  "//elements/components/foo:bar",
				// LinkPaths intentionally empty.
			}},
		}},
	})
	if err != nil {
		t.Fatalf("manifest.Index: %v", err)
	}
	got := buildGenexTargets(reply, "/build", nil, nil, imports)
	imp, ok := got["Foo::bar"]
	if !ok {
		t.Fatalf("imported Foo::bar missing from genexTargets: %+v", got)
	}
	if !imp.Imported {
		t.Errorf("imported target should still carry Imported=true; got %+v", imp)
	}
	if imp.FileLocation != "" {
		t.Errorf("FileLocation should be empty when LinkPaths empty; got %q", imp.FileLocation)
	}
}

// TestBuildGenexTargets_LocalWinsOnNameCollision pins that
// a codemodel-local target wins over a manifest entry sharing
// the same cmake target name. The name collision is rare in
// practice (cmake namespacing usually keeps imported / local
// names apart), but the policy is "codemodel is ground truth"
// — the fold preserves the local entry.
func TestBuildGenexTargets_LocalWinsOnNameCollision(t *testing.T) {
	reply := &fileapi.Reply{
		Targets: map[string]fileapi.Target{
			"Foo::bar": {Name: "Foo::bar", Type: "STATIC_LIBRARY"},
		},
	}
	imports, err := manifest.Index(&manifest.Imports{
		Version: 1,
		Elements: []*manifest.Element{{
			Name: "components/foo",
			Exports: []*manifest.Export{{
				CMakeTarget: "Foo::bar",
				BazelLabel:  "//elements/components/foo:bar",
				LinkPaths:   []string{"/prefix/lib/libbar.a"},
			}},
		}},
	})
	if err != nil {
		t.Fatalf("manifest.Index: %v", err)
	}
	got := buildGenexTargets(reply, "/build", nil, nil, imports)
	ti := got["Foo::bar"]
	if ti.Imported {
		t.Errorf("local codemodel entry should win — Imported=false expected; got %+v", ti)
	}
	if ti.Type != "STATIC_LIBRARY" {
		t.Errorf("local Type lost: %q", ti.Type)
	}
}

// TestBuildGenexTargets_ImportsOnly_NoLocalCodemodel covers the
// imports-only edge case: the reply has no codemodel targets
// (empty cmake project, or an element that only generates
// configuration files from imported deps). The fold still
// surfaces the imports so a template referencing
// `$<TARGET_FILE:Foo::bar>` can lift via PR 2's resolved path.
func TestBuildGenexTargets_ImportsOnly_NoLocalCodemodel(t *testing.T) {
	reply := &fileapi.Reply{Targets: map[string]fileapi.Target{}}
	imports, err := manifest.Index(&manifest.Imports{
		Version: 1,
		Elements: []*manifest.Element{{
			Name: "components/foo",
			Exports: []*manifest.Export{{
				CMakeTarget: "Foo::bar",
				BazelLabel:  "//elements/components/foo:bar",
				LinkPaths:   []string{"/prefix/lib/libbar.a"},
			}},
		}},
	})
	if err != nil {
		t.Fatalf("manifest.Index: %v", err)
	}
	got := buildGenexTargets(reply, "/build", nil, nil, imports)
	imp, ok := got["Foo::bar"]
	if !ok {
		t.Fatalf("imports-only fold lost Foo::bar: %+v", got)
	}
	if imp.FileLocation != "/prefix/lib/libbar.a" {
		t.Errorf("imports-only fold FileLocation = %q", imp.FileLocation)
	}
}

// TestRecoverFileGenerate_CrossPackageTargetFile_ResolvedLift
// is PR 2's end-to-end test: a template referencing
// `$<TARGET_FILE:Foo::bar>` for a target NOT in the local
// codemodel BUT in the imports.json manifest with LinkPaths
// matching cmake's rendered output lifts via (a). The lifted
// cmd carries `--target-file=Foo::bar="$(location //elements/foo:bar)"`
// (the manifest-resolved full Bazel label, NOT `:Foo::bar`)
// and the genrule's srcs picks up the cross-package label so
// Bazel resolves $(location) at action time.
func TestRecoverFileGenerate_CrossPackageTargetFile_ResolvedLift(t *testing.T) {
	template := "// tool=$<TARGET_FILE:Foo::bar>\n"
	rendered := []byte("// tool=/prefix/lib/libbar.a\n")
	hostSrc, hostBuild := fileGenerateTestSetup(t, "src/g.in", template, "g.out", rendered)
	calls := []shadow.FileGenerateCall{{
		File:     filepath.Join(hostSrc, "CMakeLists.txt"),
		Output:   filepath.Join(hostBuild, "g.out"),
		Input:    filepath.Join(hostSrc, "src/g.in"),
		HasInput: true,
	}}
	// Imports manifest carries Foo::bar with LinkPaths matching
	// cmake's rendered bytes — what the orchestrator's
	// IMPORTED_LOCATION extractor produces for a synth-prefix-
	// hosted cross-element dep.
	imports, err := manifest.Index(&manifest.Imports{
		Version: 1,
		Elements: []*manifest.Element{{
			Name: "components/foo",
			Exports: []*manifest.Export{{
				CMakeTarget: "Foo::bar",
				BazelLabel:  "//elements/components/foo:bar",
				LinkPaths:   []string{"/prefix/lib/libbar.a"},
			}},
		}},
	})
	if err != nil {
		t.Fatalf("manifest.Index: %v", err)
	}
	// genexTargets is built as if from the codemodel via
	// buildGenexTargets — which in PR 2's flow includes the
	// imports fold.
	reply := &fileapi.Reply{Targets: map[string]fileapi.Target{}}
	genexTargets := buildGenexTargets(reply, hostBuild, nil, nil, imports)
	cmakeVars := map[string]string{"CMAKE_BUILD_TYPE": "Release"}
	cc := newCodegenContext()
	if _, err := recoverFileGenerate(calls, hostSrc, hostSrc, hostBuild, hostBuild, true, cmakeVars, genexTargets, imports, cc); err != nil {
		t.Fatalf("recover: %v", err)
	}
	if len(cc.Genrules) != 1 {
		t.Fatalf("Genrules: %+v", cc.Genrules)
	}
	g := cc.Genrules[0]
	// PR 2: the (a) lift fires — the byte-equal check matches
	// because FileLocation came from the manifest's LinkPaths.
	if !hasTag(g.Tags, "cmake-codegen-genex-resolved") {
		t.Errorf("expected (a) tag in %v", g.Tags)
	}
	// The refusal-stub tag must NOT appear — the resolved lift
	// path explicitly handles this case.
	if hasTag(g.Tags, "cmake-codegen-genex-cross-package") {
		t.Errorf("refusal-stub tag should NOT fire for manifest-resolved: %v", g.Tags)
	}
	// cmd carries the manifest-resolved label, NOT `:Foo::bar`.
	wantFlag := `--target-file=Foo::bar="$(location //elements/components/foo:bar)"`
	if !strings.Contains(g.GenruleCmd, wantFlag) {
		t.Errorf("cmd should pass %q; got %q", wantFlag, g.GenruleCmd)
	}
	if strings.Contains(g.GenruleCmd, `--target-file=Foo::bar="$(location :Foo::bar)"`) {
		t.Errorf("cmd should NOT carry same-package label for cross-package target; got %q", g.GenruleCmd)
	}
	// The cross-package label rides in srcs so Bazel's
	// $(location //pkg:t) substitution resolves at action time.
	if !containsString(g.Srcs, "//elements/components/foo:bar") {
		t.Errorf("genrule.srcs should carry the cross-package label; got %v", g.Srcs)
	}
	// FileLocation must NOT leak into the marshaled Context
	// payload (wire-omitted per the json:"-" tag).
	blob := string(mustDecodeGenexContextBlob(t, g.GenruleCmd))
	if strings.Contains(blob, "/prefix/lib/libbar.a") {
		t.Errorf("FileLocation leaked into marshaled Context: %s", blob)
	}
}

// TestRecoverFileGenerate_CrossPackageTargetFile_MixedSameAndCrossPackage
// covers a template referencing BOTH a same-package target
// AND a manifest-resolved target. The lifter emits two
// `--target-file` flags — one with the `:name` shorthand, one
// with the full cross-package label — and the cross-package
// label rides in srcs while the same-package one does not.
func TestRecoverFileGenerate_CrossPackageTargetFile_MixedSameAndCrossPackage(t *testing.T) {
	template := "// local=$<TARGET_FILE:foo> remote=$<TARGET_FILE:Foo::bar>\n"
	rendered := []byte("// local=/recording/build/libfoo.a remote=/prefix/lib/libbar.a\n")
	hostSrc, hostBuild := fileGenerateTestSetup(t, "src/g.in", template, "g.out", rendered)
	calls := []shadow.FileGenerateCall{{
		File:     filepath.Join(hostSrc, "CMakeLists.txt"),
		Output:   filepath.Join(hostBuild, "g.out"),
		Input:    filepath.Join(hostSrc, "src/g.in"),
		HasInput: true,
	}}
	imports, err := manifest.Index(&manifest.Imports{
		Version: 1,
		Elements: []*manifest.Element{{
			Name: "components/foo",
			Exports: []*manifest.Export{{
				CMakeTarget: "Foo::bar",
				BazelLabel:  "//elements/components/foo:bar",
				LinkPaths:   []string{"/prefix/lib/libbar.a"},
			}},
		}},
	})
	if err != nil {
		t.Fatalf("manifest.Index: %v", err)
	}
	// Local target foo lives in genexTargets directly (not via
	// manifest fold); the manifest fold also adds Foo::bar.
	genexTargets := map[string]genexeval.TargetInfo{
		"foo": {Type: "STATIC_LIBRARY", FileLocation: "/recording/build/libfoo.a"},
	}
	foldImportedTargets(genexTargets, imports)
	cc := newCodegenContext()
	if _, err := recoverFileGenerate(calls, hostSrc, hostSrc, hostBuild, hostBuild, true, nil, genexTargets, imports, cc); err != nil {
		t.Fatalf("recover: %v", err)
	}
	g := cc.Genrules[0]
	if !hasTag(g.Tags, "cmake-codegen-genex-resolved") {
		t.Errorf("expected (a) tag in %v", g.Tags)
	}
	if !strings.Contains(g.GenruleCmd, `--target-file=foo="$(location :foo)"`) {
		t.Errorf("missing same-package flag; cmd=%q", g.GenruleCmd)
	}
	if !strings.Contains(g.GenruleCmd, `--target-file=Foo::bar="$(location //elements/components/foo:bar)"`) {
		t.Errorf("missing cross-package flag; cmd=%q", g.GenruleCmd)
	}
	// srcs carries the cross-package label but NOT the
	// same-package one (Bazel finds `:foo` via package-internal
	// lookup; cross-package needs the explicit srcs entry).
	if !containsString(g.Srcs, "//elements/components/foo:bar") {
		t.Errorf("srcs missing cross-package label; got %v", g.Srcs)
	}
	if containsString(g.Srcs, ":foo") {
		t.Errorf("srcs should NOT carry same-package label `:foo`; got %v", g.Srcs)
	}
}

// TestResolveTargetFileLabels covers the per-target label
// resolution helper directly: same-package, manifest-resolved,
// and dropped (neither) cases.
func TestResolveTargetFileLabels(t *testing.T) {
	imports, err := manifest.Index(&manifest.Imports{
		Version: 1,
		Elements: []*manifest.Element{{
			Name: "components/foo",
			Exports: []*manifest.Export{{
				CMakeTarget: "Foo::bar",
				BazelLabel:  "//elements/components/foo:bar",
			}},
		}},
	})
	if err != nil {
		t.Fatalf("manifest.Index: %v", err)
	}
	genexTargets := map[string]genexeval.TargetInfo{
		"local":    {Type: "STATIC_LIBRARY"},
		"Foo::bar": {Imported: true, FileLocation: "/p/libbar.a"},
		// `unknown` not present — no entry.
	}
	labels, crossPackage := resolveTargetFileLabels([]string{"local", "Foo::bar", "unknown"}, genexTargets, imports)
	if labels["local"] != ":local" {
		t.Errorf("local label = %q, want :local", labels["local"])
	}
	if labels["Foo::bar"] != "//elements/components/foo:bar" {
		t.Errorf("Foo::bar label = %q, want //elements/components/foo:bar", labels["Foo::bar"])
	}
	if _, ok := labels["unknown"]; ok {
		t.Errorf("unknown should be dropped; got %q", labels["unknown"])
	}
	if len(crossPackage) != 1 || crossPackage[0] != "//elements/components/foo:bar" {
		t.Errorf("crossPackage = %v, want [//elements/components/foo:bar]", crossPackage)
	}
}

// containsString reports whether haystack contains needle. Used
// in the cross-package srcs assertions above.
func containsString(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}

// TestAggregateInterface_SingleDep is the simplest case: a
// consumer target with one INTERFACE_LIBRARY dep — the dep's
// PUBLIC/INTERFACE target_include_directories must surface on
// the consumer's INTERFACE_INCLUDE_DIRECTORIES at the aggregate.
func TestAggregateInterface_SingleDep(t *testing.T) {
	reply := &fileapi.Reply{
		Targets: map[string]fileapi.Target{
			"base": {
				Name: "base",
				Id:   "base::@x",
				Type: "INTERFACE_LIBRARY",
			},
			"leaf": {
				Name: "leaf",
				Id:   "leaf::@x",
				Type: "STATIC_LIBRARY",
				Dependencies: []fileapi.TargetDependency{
					{Id: "base::@x"},
				},
			},
		},
	}
	decoded := &shadow.Decoded{
		Includes: []shadow.TargetIncludeCall{{
			Target: "base",
			Groups: []shadow.TargetIncludeGroup{{
				Visibility: "INTERFACE",
				Dirs:       []string{"/src/include"},
			}},
		}},
	}
	got := buildGenexTargets(reply, "/build", nil, decoded, nil)
	if got["leaf"].InterfaceIncludeDirectories != "/src/include" {
		t.Errorf("leaf InterfaceIncludeDirectories = %q, want %q", got["leaf"].InterfaceIncludeDirectories, "/src/include")
	}
	// base's own aggregate exposes its own contribution.
	if got["base"].InterfaceIncludeDirectories != "/src/include" {
		t.Errorf("base InterfaceIncludeDirectories = %q, want %q", got["base"].InterfaceIncludeDirectories, "/src/include")
	}
}

// TestAggregateInterface_MultiDepOrdering pins the documented
// cmake first-listed-first ordering across two direct
// dependencies. The codemodel records Dependencies[] in
// target_link_libraries' arg order; the aggregation must walk in
// that order.
func TestAggregateInterface_MultiDepOrdering(t *testing.T) {
	reply := &fileapi.Reply{
		Targets: map[string]fileapi.Target{
			"a": {Name: "a", Id: "a::@x", Type: "INTERFACE_LIBRARY"},
			"b": {Name: "b", Id: "b::@x", Type: "INTERFACE_LIBRARY"},
			"consumer": {
				Name: "consumer",
				Id:   "consumer::@x",
				Type: "STATIC_LIBRARY",
				Dependencies: []fileapi.TargetDependency{
					{Id: "a::@x"},
					{Id: "b::@x"},
				},
			},
		},
	}
	decoded := &shadow.Decoded{
		Includes: []shadow.TargetIncludeCall{
			{
				Target: "a",
				Groups: []shadow.TargetIncludeGroup{{Visibility: "INTERFACE", Dirs: []string{"/a"}}},
			},
			{
				Target: "b",
				Groups: []shadow.TargetIncludeGroup{{Visibility: "INTERFACE", Dirs: []string{"/b"}}},
			},
		},
	}
	got := buildGenexTargets(reply, "/build", nil, decoded, nil)
	want := "/a;/b"
	if got["consumer"].InterfaceIncludeDirectories != want {
		t.Errorf("consumer InterfaceIncludeDirectories = %q, want %q", got["consumer"].InterfaceIncludeDirectories, want)
	}
}

// TestAggregateInterface_TransitiveChain pins the recursive
// walk: A → B → C, prop on C surfaces on A. The roadmap
// fixture is exactly this shape.
func TestAggregateInterface_TransitiveChain(t *testing.T) {
	reply := &fileapi.Reply{
		Targets: map[string]fileapi.Target{
			"c": {Name: "c", Id: "c::@x", Type: "INTERFACE_LIBRARY"},
			"b": {
				Name: "b", Id: "b::@x", Type: "INTERFACE_LIBRARY",
				Dependencies: []fileapi.TargetDependency{{Id: "c::@x"}},
			},
			"a": {
				Name: "a", Id: "a::@x", Type: "STATIC_LIBRARY",
				Dependencies: []fileapi.TargetDependency{{Id: "b::@x"}},
			},
		},
	}
	decoded := &shadow.Decoded{
		Includes: []shadow.TargetIncludeCall{
			{Target: "c", Groups: []shadow.TargetIncludeGroup{{Visibility: "INTERFACE", Dirs: []string{"/c"}}}},
		},
	}
	got := buildGenexTargets(reply, "/build", nil, decoded, nil)
	if got["a"].InterfaceIncludeDirectories != "/c" {
		t.Errorf("a InterfaceIncludeDirectories = %q, want %q", got["a"].InterfaceIncludeDirectories, "/c")
	}
	if got["b"].InterfaceIncludeDirectories != "/c" {
		t.Errorf("b InterfaceIncludeDirectories = %q, want %q", got["b"].InterfaceIncludeDirectories, "/c")
	}
	if got["c"].InterfaceIncludeDirectories != "/c" {
		t.Errorf("c InterfaceIncludeDirectories = %q, want %q", got["c"].InterfaceIncludeDirectories, "/c")
	}
}

// TestAggregateInterface_Dedup confirms the same value reached
// via two paths surfaces once at the consumer. Diamond shape:
// consumer depends on left and right, both of which depend on
// base; base's INTERFACE value must appear once in consumer's
// aggregate.
func TestAggregateInterface_Dedup(t *testing.T) {
	reply := &fileapi.Reply{
		Targets: map[string]fileapi.Target{
			"base": {Name: "base", Id: "base::@x", Type: "INTERFACE_LIBRARY"},
			"left": {
				Name: "left", Id: "left::@x", Type: "INTERFACE_LIBRARY",
				Dependencies: []fileapi.TargetDependency{{Id: "base::@x"}},
			},
			"right": {
				Name: "right", Id: "right::@x", Type: "INTERFACE_LIBRARY",
				Dependencies: []fileapi.TargetDependency{{Id: "base::@x"}},
			},
			"consumer": {
				Name: "consumer", Id: "consumer::@x", Type: "STATIC_LIBRARY",
				Dependencies: []fileapi.TargetDependency{
					{Id: "left::@x"},
					{Id: "right::@x"},
				},
			},
		},
	}
	decoded := &shadow.Decoded{
		Includes: []shadow.TargetIncludeCall{
			{Target: "base", Groups: []shadow.TargetIncludeGroup{{Visibility: "INTERFACE", Dirs: []string{"/base"}}}},
		},
	}
	got := buildGenexTargets(reply, "/build", nil, decoded, nil)
	if got["consumer"].InterfaceIncludeDirectories != "/base" {
		t.Errorf("consumer InterfaceIncludeDirectories = %q, want %q (single occurrence)", got["consumer"].InterfaceIncludeDirectories, "/base")
	}
}

// TestAggregateInterface_Cycle confirms the walk terminates on
// a cyclic graph (A → B → A). cmake itself rejects cycles; the
// aggregation just needs not to infinite-loop. Each target sees
// its own direct contribution; the cycle break drops the
// re-entered branch.
func TestAggregateInterface_Cycle(t *testing.T) {
	reply := &fileapi.Reply{
		Targets: map[string]fileapi.Target{
			"a": {
				Name: "a", Id: "a::@x", Type: "INTERFACE_LIBRARY",
				Dependencies: []fileapi.TargetDependency{{Id: "b::@x"}},
			},
			"b": {
				Name: "b", Id: "b::@x", Type: "INTERFACE_LIBRARY",
				Dependencies: []fileapi.TargetDependency{{Id: "a::@x"}},
			},
		},
	}
	decoded := &shadow.Decoded{
		Includes: []shadow.TargetIncludeCall{
			{Target: "a", Groups: []shadow.TargetIncludeGroup{{Visibility: "INTERFACE", Dirs: []string{"/a"}}}},
			{Target: "b", Groups: []shadow.TargetIncludeGroup{{Visibility: "INTERFACE", Dirs: []string{"/b"}}}},
		},
	}
	// The primary contract is termination — cmake itself errors
	// on cycles, so as long as we don't infinite-loop, the
	// result we hand back can be a partial aggregate. We do
	// require each target's own direct contribution to land
	// (the cycle break only drops the re-entered branch, not
	// the start frame's own bytes).
	got := buildGenexTargets(reply, "/build", nil, decoded, nil)
	for _, name := range []string{"a", "b"} {
		want := "/" + name
		parts := strings.Split(got[name].InterfaceIncludeDirectories, ";")
		seen := map[string]bool{}
		for _, p := range parts {
			seen[p] = true
		}
		if !seen[want] {
			t.Errorf("%s InterfaceIncludeDirectories = %q (missing %q)", name, got[name].InterfaceIncludeDirectories, want)
		}
	}
}

// TestAggregateInterface_AllFourProperties covers
// INTERFACE_INCLUDE_DIRECTORIES + INTERFACE_COMPILE_DEFINITIONS
// + INTERFACE_COMPILE_OPTIONS + INTERFACE_LINK_LIBRARIES in one
// pass, against the same dep graph: base → mid → leaf with a
// distinct kind of contribution on each property at base.
func TestAggregateInterface_AllFourProperties(t *testing.T) {
	reply := &fileapi.Reply{
		Targets: map[string]fileapi.Target{
			"base": {Name: "base", Id: "base::@x", Type: "INTERFACE_LIBRARY"},
			"mid": {
				Name: "mid", Id: "mid::@x", Type: "INTERFACE_LIBRARY",
				Dependencies: []fileapi.TargetDependency{{Id: "base::@x"}},
			},
			"leaf": {
				Name: "leaf", Id: "leaf::@x", Type: "STATIC_LIBRARY",
				Dependencies: []fileapi.TargetDependency{{Id: "mid::@x"}},
			},
		},
	}
	decoded := &shadow.Decoded{
		Includes: []shadow.TargetIncludeCall{
			{Target: "base", Groups: []shadow.TargetIncludeGroup{{Visibility: "INTERFACE", Dirs: []string{"/include"}}}},
		},
		CompileDefinitions: []shadow.TargetCompileCall{
			{Target: "base", Cmd: "target_compile_definitions", Groups: []shadow.TargetCompileGroup{{Visibility: "INTERFACE", Items: []string{"FOO=1"}}}},
		},
		CompileOptions: []shadow.TargetCompileCall{
			{Target: "base", Cmd: "target_compile_options", Groups: []shadow.TargetCompileGroup{{Visibility: "INTERFACE", Items: []string{"-Wall"}}}},
		},
		Links: []shadow.TargetLinkCall{
			{Target: "mid", Groups: []shadow.TargetLinkGroup{{Visibility: "INTERFACE", Libs: []string{"base"}}}},
		},
	}
	got := buildGenexTargets(reply, "/build", nil, decoded, nil)
	if got["leaf"].InterfaceIncludeDirectories != "/include" {
		t.Errorf("leaf InterfaceIncludeDirectories = %q", got["leaf"].InterfaceIncludeDirectories)
	}
	if got["leaf"].InterfaceCompileDefinitions != "FOO=1" {
		t.Errorf("leaf InterfaceCompileDefinitions = %q", got["leaf"].InterfaceCompileDefinitions)
	}
	if got["leaf"].InterfaceCompileOptions != "-Wall" {
		t.Errorf("leaf InterfaceCompileOptions = %q", got["leaf"].InterfaceCompileOptions)
	}
	if got["leaf"].InterfaceLinkLibraries != "base" {
		t.Errorf("leaf InterfaceLinkLibraries = %q", got["leaf"].InterfaceLinkLibraries)
	}
}

// TestAggregateInterface_PrivateExcluded confirms PRIVATE
// contributions don't propagate. base has PRIVATE include /
// define / option / link arms; leaf must not see them.
func TestAggregateInterface_PrivateExcluded(t *testing.T) {
	reply := &fileapi.Reply{
		Targets: map[string]fileapi.Target{
			"base": {Name: "base", Id: "base::@x", Type: "STATIC_LIBRARY"},
			"leaf": {
				Name: "leaf", Id: "leaf::@x", Type: "STATIC_LIBRARY",
				Dependencies: []fileapi.TargetDependency{{Id: "base::@x"}},
			},
		},
	}
	decoded := &shadow.Decoded{
		Includes: []shadow.TargetIncludeCall{
			{Target: "base", Groups: []shadow.TargetIncludeGroup{
				{Visibility: "PRIVATE", Dirs: []string{"/private/include"}},
				{Visibility: "PUBLIC", Dirs: []string{"/public/include"}},
			}},
		},
	}
	got := buildGenexTargets(reply, "/build", nil, decoded, nil)
	want := "/public/include"
	if got["leaf"].InterfaceIncludeDirectories != want {
		t.Errorf("leaf InterfaceIncludeDirectories = %q, want %q", got["leaf"].InterfaceIncludeDirectories, want)
	}
	if strings.Contains(got["leaf"].InterfaceIncludeDirectories, "/private/include") {
		t.Errorf("PRIVATE include should not propagate; got %q", got["leaf"].InterfaceIncludeDirectories)
	}
}

// TestAggregateInterface_MissingTargetSurfacesUnsupported pins
// the existing evaluator behavior — a TARGET_PROPERTY lookup
// against a target absent from the codemodel surfaces an
// UnsupportedError. Cross-package INTERFACE_* is out of scope
// for this PR; the gate is the same as TARGET_FILE's.
func TestAggregateInterface_MissingTargetSurfacesUnsupported(t *testing.T) {
	reply := &fileapi.Reply{
		Targets: map[string]fileapi.Target{
			"foo": {Name: "foo", Id: "foo::@x", Type: "STATIC_LIBRARY"},
		},
	}
	got := buildGenexTargets(reply, "/build", nil, &shadow.Decoded{}, nil)
	if _, ok := got["ghost"]; ok {
		t.Errorf("unknown target 'ghost' should not have an entry; got %+v", got["ghost"])
	}
}

// TestAggregateInterface_ProbeOverridesAggregate locks the
// layering: when probes carry a value AND the convert-time
// aggregate produces a different value, the probe wins (it's
// cmake's own evaluator's output).
func TestAggregateInterface_ProbeOverridesAggregate(t *testing.T) {
	reply := &fileapi.Reply{
		Targets: map[string]fileapi.Target{
			"foo": {Name: "foo", Id: "foo::@x", Type: "STATIC_LIBRARY"},
		},
	}
	decoded := &shadow.Decoded{
		Includes: []shadow.TargetIncludeCall{
			{Target: "foo", Groups: []shadow.TargetIncludeGroup{{Visibility: "INTERFACE", Dirs: []string{"/from-trace"}}}},
		},
	}
	probes := []cmakerun.GenexProbe{{
		Name:      "foo",
		Interface: map[string]string{"INCLUDE_DIRECTORIES": "/from-probe"},
	}}
	got := buildGenexTargets(reply, "/build", probes, decoded, nil)
	if got["foo"].InterfaceIncludeDirectories != "/from-probe" {
		t.Errorf("probe must override aggregate; got %q want %q", got["foo"].InterfaceIncludeDirectories, "/from-probe")
	}
}
