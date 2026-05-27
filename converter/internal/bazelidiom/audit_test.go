package bazelidiom_test

import (
	"strings"
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/internal/bazelidiom"
)

func TestAudit_HealthyBUILD(t *testing.T) {
	body := []byte(`load("@rules_cc//cc:defs.bzl", "cc_library")

cc_library(
    name = "foo",
    srcs = ["foo.c"],
    hdrs = ["foo.h"],
)
`)
	findings, err := bazelidiom.Audit(body)
	if err != nil {
		t.Fatalf("Audit: %v", err)
	}
	if len(findings) != 0 {
		t.Errorf("healthy BUILD should produce no findings; got %v", findings)
	}
}

func TestAudit_EmptyCCLibrary(t *testing.T) {
	body := []byte(`cc_library(
    name = "placeholder",
)
`)
	findings, err := bazelidiom.Audit(body)
	if err != nil {
		t.Fatalf("Audit: %v", err)
	}
	if len(findings) != 1 {
		t.Fatalf("want 1 finding; got %d (%v)", len(findings), findings)
	}
	if findings[0].Code != "empty-cc-library" {
		t.Errorf("Code: %q", findings[0].Code)
	}
	if findings[0].Target != "placeholder" {
		t.Errorf("Target: %q", findings[0].Target)
	}
}

func TestAudit_HeaderOnlyLibrary_Allowed(t *testing.T) {
	// cc_library with hdrs but no srcs is the canonical
	// INTERFACE_LIBRARY shape; should NOT be flagged.
	body := []byte(`cc_library(
    name = "interface",
    hdrs = ["foo.h", "bar.h"],
)
`)
	findings, err := bazelidiom.Audit(body)
	if err != nil {
		t.Fatalf("Audit: %v", err)
	}
	if len(findings) != 0 {
		t.Errorf("header-only library shouldn't trigger empty-cc-library; got %v", findings)
	}
}

func TestAudit_EmptyCCImport(t *testing.T) {
	body := []byte(`cc_import(
    name = "broken",
)
`)
	findings, err := bazelidiom.Audit(body)
	if err != nil {
		t.Fatalf("Audit: %v", err)
	}
	if len(findings) != 1 || findings[0].Code != "empty-cc-import" {
		t.Errorf("expected empty-cc-import finding; got %v", findings)
	}
}

func TestAudit_CCImport_StaticOnly_Allowed(t *testing.T) {
	body := []byte(`cc_import(
    name = "ok",
    static_library = "libfoo.a",
)
`)
	findings, err := bazelidiom.Audit(body)
	if err != nil {
		t.Fatalf("Audit: %v", err)
	}
	if len(findings) != 0 {
		t.Errorf("cc_import with static_library shouldn't flag; got %v", findings)
	}
}

func TestAudit_CCBinaryNoSrcs(t *testing.T) {
	body := []byte(`cc_binary(
    name = "broken_bin",
)
`)
	findings, err := bazelidiom.Audit(body)
	if err != nil {
		t.Fatalf("Audit: %v", err)
	}
	if len(findings) != 1 || findings[0].Code != "empty-srcs" {
		t.Errorf("expected empty-srcs finding; got %v", findings)
	}
}

func TestAudit_SelectArmsCountAsSrcs(t *testing.T) {
	// A cc_library whose srcs come from a select() with
	// non-empty arms should NOT be flagged empty.
	body := []byte(`cc_library(
    name = "platform",
    srcs = select({
        "//cpu:x86_64": ["x86.c"],
        "//cpu:arm64": ["arm.c"],
    }),
)
`)
	findings, err := bazelidiom.Audit(body)
	if err != nil {
		t.Fatalf("Audit: %v", err)
	}
	if len(findings) != 0 {
		t.Errorf("select-driven srcs shouldn't flag empty; got %v", findings)
	}
}

func TestAudit_ConcatAttrCounts(t *testing.T) {
	// srcs = [...] + select({...}) should also not flag as empty.
	body := []byte(`cc_library(
    name = "mixed",
    srcs = ["base.c"] + select({
        "//cpu:x86_64": ["x86.c"],
        "//conditions:default": [],
    }),
)
`)
	findings, err := bazelidiom.Audit(body)
	if err != nil {
		t.Fatalf("Audit: %v", err)
	}
	if len(findings) != 0 {
		t.Errorf("concat srcs shouldn't flag empty; got %v", findings)
	}
}

func TestFormatFindings(t *testing.T) {
	findings := []bazelidiom.Finding{
		{Rule: "cc_library", Target: "foo", Code: "empty-cc-library", Message: "no srcs/hdrs"},
		{Rule: "cc_import", Target: "bar", Code: "empty-cc-import", Message: "no library"},
	}
	out := bazelidiom.FormatFindings(findings)
	if !strings.Contains(out, "cc_library(foo): empty-cc-library:") {
		t.Errorf("missing cc_library entry: %q", out)
	}
	if !strings.Contains(out, "cc_import(bar): empty-cc-import:") {
		t.Errorf("missing cc_import entry: %q", out)
	}
}

func TestFormatFindings_Empty(t *testing.T) {
	if got := bazelidiom.FormatFindings(nil); got != "" {
		t.Errorf("FormatFindings(nil) = %q; want empty", got)
	}
}

func TestAudit_EmptyBody(t *testing.T) {
	findings, err := bazelidiom.Audit(nil)
	if err != nil || findings != nil {
		t.Errorf("Audit(nil) = (%v, %v); want (nil, nil)", findings, err)
	}
}

// TestAudit_SanitizerSelect_CoptsFiring covers Phase 7's
// sanitizer-shaped-select detection: copts driven by a select()
// keyed on //config:asan / :tsan etc. should be a feature.
func TestAudit_SanitizerSelect_CoptsFiring(t *testing.T) {
	body := []byte(`cc_library(
    name = "lib",
    srcs = ["a.c"],
    copts = select({
        "//config:asan": ["-fsanitize=address"],
        "//config:tsan": ["-fsanitize=thread"],
        "//conditions:default": [],
    }),
)
`)
	findings, err := bazelidiom.Audit(body)
	if err != nil {
		t.Fatalf("Audit: %v", err)
	}
	var got *bazelidiom.Finding
	for i, f := range findings {
		if f.Code == "sanitizer-select-not-feature" {
			got = &findings[i]
			break
		}
	}
	if got == nil {
		t.Fatalf("expected sanitizer-select-not-feature finding; got %v", findings)
	}
	if !strings.Contains(got.Message, "copts") {
		t.Errorf("message should name the attr (copts): %q", got.Message)
	}
	if !strings.Contains(got.Message, "asan") {
		t.Errorf("message should name the matched key: %q", got.Message)
	}
}

// TestAudit_SanitizerSelect_LinkoptsFiring covers the same shape on
// linkopts (sanitizer flags often also need link-time -fsanitize=).
func TestAudit_SanitizerSelect_LinkoptsFiring(t *testing.T) {
	body := []byte(`cc_library(
    name = "lib",
    srcs = ["a.c"],
    linkopts = select({
        "//config:asan_enabled": ["-fsanitize=address"],
        "//conditions:default": [],
    }),
)
`)
	findings, err := bazelidiom.Audit(body)
	if err != nil {
		t.Fatalf("Audit: %v", err)
	}
	found := false
	for _, f := range findings {
		if f.Code == "sanitizer-select-not-feature" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected sanitizer-select-not-feature finding for linkopts; got %v", findings)
	}
}

// TestAudit_NonSanitizerSelect_NoFinding confirms unrelated selects
// (platform, cpu) don't trigger the sanitizer check.
func TestAudit_NonSanitizerSelect_NoFinding(t *testing.T) {
	body := []byte(`cc_library(
    name = "lib",
    srcs = ["a.c"],
    copts = select({
        "//cpu:x86_64": ["-msse4.2"],
        "//cpu:arm64": ["-march=armv8-a"],
    }),
)
`)
	findings, err := bazelidiom.Audit(body)
	if err != nil {
		t.Fatalf("Audit: %v", err)
	}
	for _, f := range findings {
		if f.Code == "sanitizer-select-not-feature" {
			t.Errorf("non-sanitizer select shouldn't fire: %v", f)
		}
	}
}

// TestAudit_ConcatLiteralPlusSanitizerSelect catches the common
// shape `copts = ["-O2"] + select({...sanitizer...})`.
func TestAudit_ConcatLiteralPlusSanitizerSelect(t *testing.T) {
	body := []byte(`cc_library(
    name = "lib",
    srcs = ["a.c"],
    copts = ["-O2"] + select({
        "//config:lto": ["-flto"],
        "//conditions:default": [],
    }),
)
`)
	findings, err := bazelidiom.Audit(body)
	if err != nil {
		t.Fatalf("Audit: %v", err)
	}
	found := false
	for _, f := range findings {
		if f.Code == "sanitizer-select-not-feature" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected finding on [literal] + select() shape; got %v", findings)
	}
}

func TestAudit_CCTestWithNoEntry(t *testing.T) {
	body := []byte(`cc_test(
    name = "empty_test",
)
`)
	findings, err := bazelidiom.Audit(body)
	if err != nil {
		t.Fatalf("Audit: %v", err)
	}
	foundEntry := false
	foundSrcs := false
	for _, f := range findings {
		if f.Code == "test-with-no-entry" {
			foundEntry = true
		}
		if f.Code == "empty-srcs" {
			foundSrcs = true
		}
	}
	if !foundEntry {
		t.Errorf("expected test-with-no-entry finding")
	}
	if !foundSrcs {
		t.Errorf("expected empty-srcs finding alongside")
	}
}

func TestAudit_CCTestWithDeps_NoEntryFinding(t *testing.T) {
	// cc_test with no srcs but with deps (the entry point comes
	// from a dep's cc_library main symbol) shouldn't trigger
	// test-with-no-entry.
	body := []byte(`cc_test(
    name = "via_deps",
    deps = [":lib_with_main"],
)
`)
	findings, err := bazelidiom.Audit(body)
	if err != nil {
		t.Fatalf("Audit: %v", err)
	}
	for _, f := range findings {
		if f.Code == "test-with-no-entry" {
			t.Errorf("unexpected test-with-no-entry: %v", f)
		}
	}
}

func TestAudit_RawPICFlag(t *testing.T) {
	body := []byte(`cc_library(
    name = "lib",
    srcs = ["a.c"],
    copts = ["-fPIC", "-O2"],
)
`)
	findings, err := bazelidiom.Audit(body)
	if err != nil {
		t.Fatalf("Audit: %v", err)
	}
	found := false
	for _, f := range findings {
		if f.Code == "raw-toolchain-feature-flag" && strings.Contains(f.Message, "-fPIC") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected raw-toolchain-feature-flag for -fPIC; got %v", findings)
	}
}

func TestAudit_RawSanitizeFlag(t *testing.T) {
	body := []byte(`cc_library(
    name = "lib",
    srcs = ["a.c"],
    copts = ["-fsanitize=address"],
    linkopts = ["-fsanitize=address"],
)
`)
	findings, err := bazelidiom.Audit(body)
	if err != nil {
		t.Fatalf("Audit: %v", err)
	}
	coptHit, linkHit := false, false
	for _, f := range findings {
		if f.Code == "raw-toolchain-feature-flag" {
			if strings.Contains(f.Message, "copts") {
				coptHit = true
			}
			if strings.Contains(f.Message, "linkopts") {
				linkHit = true
			}
		}
	}
	if !coptHit || !linkHit {
		t.Errorf("expected raw-toolchain-feature-flag for copts AND linkopts; got %v", findings)
	}
}

func TestAudit_RawVisibilityFlag(t *testing.T) {
	// `-fvisibility=hidden` + `-fvisibility-inlines-hidden` are the
	// emit-shape outputs of the CMAKE_<LANG>_VISIBILITY_PRESET /
	// VISIBILITY_INLINES_HIDDEN lift; the Bazel-idiomatic form is
	// a cc_toolchain feature so the toolchain owns the flag set
	// instead of every cc_library carrying the same per-rule copts.
	// Surfaced by running the converter against VTK 9.3.0 where
	// every module carries both flags in copts.
	body := []byte(`cc_library(
    name = "lib",
    srcs = ["a.c"],
    copts = ["-fvisibility=hidden", "-fvisibility-inlines-hidden"],
)
`)
	findings, err := bazelidiom.Audit(body)
	if err != nil {
		t.Fatalf("Audit: %v", err)
	}
	visHit, inlineHit := false, false
	for _, f := range findings {
		if f.Code == "raw-toolchain-feature-flag" {
			if strings.Contains(f.Message, "-fvisibility=hidden") &&
				strings.Contains(f.Message, "visibility_hidden") {
				visHit = true
			}
			if strings.Contains(f.Message, "-fvisibility-inlines-hidden") &&
				strings.Contains(f.Message, "visibility_inlines_hidden") {
				inlineHit = true
			}
		}
	}
	if !visHit || !inlineHit {
		t.Errorf("expected raw-toolchain-feature-flag for both visibility flags; got %v", findings)
	}
}

// -fvisibility=default is the default; don't flag it.
func TestAudit_VisibilityDefault_NoFinding(t *testing.T) {
	body := []byte(`cc_library(
    name = "lib",
    srcs = ["a.c"],
    copts = ["-fvisibility=default"],
)
`)
	findings, err := bazelidiom.Audit(body)
	if err != nil {
		t.Fatalf("Audit: %v", err)
	}
	for _, f := range findings {
		if f.Code == "raw-toolchain-feature-flag" {
			t.Errorf("unexpected raw-toolchain-feature-flag for -fvisibility=default: %v", f)
		}
	}
}

func TestAudit_NonFeatureFlag_NoFinding(t *testing.T) {
	// -O2 isn't a toolchain-feature equivalent; should pass silently.
	body := []byte(`cc_library(
    name = "lib",
    srcs = ["a.c"],
    copts = ["-O2", "-Wall"],
)
`)
	findings, err := bazelidiom.Audit(body)
	if err != nil {
		t.Fatalf("Audit: %v", err)
	}
	for _, f := range findings {
		if f.Code == "raw-toolchain-feature-flag" {
			t.Errorf("unexpected raw-toolchain-feature-flag for plain -O2: %v", f)
		}
	}
}

func TestAudit_CmakeCodegenPCHTag(t *testing.T) {
	body := []byte(`cc_library(
    name = "lib",
    srcs = ["a.c"],
    tags = ["cmake-codegen-pch"],
)
`)
	findings, err := bazelidiom.Audit(body)
	if err != nil {
		t.Fatalf("Audit: %v", err)
	}
	found := false
	for _, f := range findings {
		if f.Code == "pch-toolchain-feature-needed" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected pch-toolchain-feature-needed; got %v", findings)
	}
}

func TestAudit_CmakeCodegenQtTags(t *testing.T) {
	body := []byte(`cc_library(
    name = "qtwidget",
    srcs = ["w.cc"],
    tags = ["cmake-codegen-qt-automoc", "cmake-codegen-qt-autouic"],
)
`)
	findings, err := bazelidiom.Audit(body)
	if err != nil {
		t.Fatalf("Audit: %v", err)
	}
	codes := map[string]bool{}
	for _, f := range findings {
		codes[f.Code] = true
	}
	if !codes["qt-automoc-host-tool-needed"] {
		t.Errorf("missing automoc finding: %v", findings)
	}
	if !codes["qt-autouic-host-tool-needed"] {
		t.Errorf("missing autouic finding: %v", findings)
	}
}

func TestAudit_CmakeCodegenLanguageOverrideTag(t *testing.T) {
	body := []byte(`cc_library(
    name = "mixed",
    srcs = ["a.c"],
    tags = ["cmake-codegen-language-override=CXX"],
)
`)
	findings, err := bazelidiom.Audit(body)
	if err != nil {
		t.Fatalf("Audit: %v", err)
	}
	found := false
	for _, f := range findings {
		if f.Code == "language-override-needs-split" {
			found = true
			if !strings.Contains(f.Message, "CXX") {
				t.Errorf("message should include lang token: %q", f.Message)
			}
		}
	}
	if !found {
		t.Errorf("expected language-override-needs-split; got %v", findings)
	}
}

func TestAudit_CmakeCodegenFindPackageFallback(t *testing.T) {
	body := []byte(`cc_library(
    name = "iostreams",
    srcs = ["zlib.cpp"],
    tags = ["cmake-codegen-find-package-fallback=ZLIB=libz.so"],
)
`)
	findings, err := bazelidiom.Audit(body)
	if err != nil {
		t.Fatalf("Audit: %v", err)
	}
	found := false
	for _, f := range findings {
		if f.Code == "find-package-dep-unresolved" {
			found = true
			if !strings.Contains(f.Message, "ZLIB") {
				t.Errorf("message should name ZLIB: %q", f.Message)
			}
		}
	}
	if !found {
		t.Errorf("expected find-package-dep-unresolved; got %v", findings)
	}
}

// TestAudit_CmakeCodegenFindPackageAttributionMissed pins the
// audit finding for the dual case of the fallback tag above:
// the operator opted into find_package attribution (manifest
// provided) but neither cmake 3.32 find_package-v1 event nor
// cmakeVars `<Pkg>_FOUND` surfaced — attribution couldn't fire.
// The tag's basename anchor (libz.so) must appear in the
// message so operators have a grep target.
func TestAudit_CmakeCodegenFindPackageAttributionMissed(t *testing.T) {
	body := []byte(`cc_library(
    name = "iostreams",
    srcs = ["zlib.cpp"],
    tags = ["cmake-codegen-find-package-attribution-missed=libz.so"],
)
`)
	findings, err := bazelidiom.Audit(body)
	if err != nil {
		t.Fatalf("Audit: %v", err)
	}
	found := false
	for _, f := range findings {
		if f.Code == "find-package-attribution-missed" {
			found = true
			if !strings.Contains(f.Message, "libz.so") {
				t.Errorf("message should name libz.so: %q", f.Message)
			}
		}
	}
	if !found {
		t.Errorf("expected find-package-attribution-missed; got %v", findings)
	}
}

func TestAudit_CmakeCodegenInformationalTags_NoFinding(t *testing.T) {
	// cmake-codegen-version=… and cmake-codegen-soversion=… are
	// informational; no audit finding.
	body := []byte(`cc_library(
    name = "lib",
    srcs = ["a.c"],
    tags = ["cmake-codegen-version=1.2.3", "cmake-codegen-soversion=1"],
)
`)
	findings, err := bazelidiom.Audit(body)
	if err != nil {
		t.Fatalf("Audit: %v", err)
	}
	for _, f := range findings {
		if strings.Contains(f.Code, "version") || strings.Contains(f.Code, "soversion") {
			t.Errorf("informational tag should not produce finding: %v", f)
		}
	}
}

// TestAudit_CmakeElidedLinkFragment pins the #220 audit
// finding: the cmake-elided-link-fragment=<path> tag surfaces
// as `unresolved-link-fragment` with the path in the message
// so operators see which library needs imports-manifest
// coverage.
func TestAudit_CmakeElidedLinkFragment(t *testing.T) {
	body := []byte(`cc_binary(
    name = "tool",
    srcs = ["main.c"],
    tags = ["cmake-elided-link-fragment=/opt/vendor/lib/libmystery.so"],
)
`)
	findings, err := bazelidiom.Audit(body)
	if err != nil {
		t.Fatalf("Audit: %v", err)
	}
	found := false
	for _, f := range findings {
		if f.Code == "unresolved-link-fragment" {
			found = true
			if !strings.Contains(f.Message, "/opt/vendor/lib/libmystery.so") {
				t.Errorf("message should name the path: %q", f.Message)
			}
		}
	}
	if !found {
		t.Errorf("expected unresolved-link-fragment; got %v", findings)
	}
}

// TestAudit_CmakeElidedPrefixInclude pins the #219 audit
// finding: the cmake-elided-prefix-include=<path> tag surfaces
// as `unresolved-prefix-include` with the path in the message.
func TestAudit_CmakeElidedPrefixInclude(t *testing.T) {
	body := []byte(`cc_library(
    name = "consumer",
    srcs = ["consumer.c"],
    tags = ["cmake-elided-prefix-include=usr/include/external_dep"],
)
`)
	findings, err := bazelidiom.Audit(body)
	if err != nil {
		t.Fatalf("Audit: %v", err)
	}
	found := false
	for _, f := range findings {
		if f.Code == "unresolved-prefix-include" {
			found = true
			if !strings.Contains(f.Message, "usr/include/external_dep") {
				t.Errorf("message should name the path: %q", f.Message)
			}
		}
	}
	if !found {
		t.Errorf("expected unresolved-prefix-include; got %v", findings)
	}
}

// TestAudit_PreExistingElidedTags_NoFinding pins that the
// existing cmake-elided-build-dir-source / -missing-source /
// -compiler-artifact tags (file-existence filtering signals,
// not operator-action gaps) intentionally produce no audit
// findings. Keeps the audit-eligible elision-tag taxonomy
// scoped to the new #219/#220 cases.
func TestAudit_PreExistingElidedTags_NoFinding(t *testing.T) {
	body := []byte(`cc_library(
    name = "lib",
    srcs = ["a.c"],
    tags = ["cmake-elided-build-dir-source", "cmake-elided-missing-source", "cmake-elided-compiler-artifact"],
)
`)
	findings, err := bazelidiom.Audit(body)
	if err != nil {
		t.Fatalf("Audit: %v", err)
	}
	for _, f := range findings {
		if strings.HasPrefix(f.Code, "unresolved-") || strings.Contains(f.Code, "elided") {
			t.Errorf("pre-existing elided tag should not produce audit finding: %v", f)
		}
	}
}
