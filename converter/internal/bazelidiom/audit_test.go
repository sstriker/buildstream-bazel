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
