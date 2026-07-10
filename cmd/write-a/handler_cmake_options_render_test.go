package main

import (
	"strings"
	"testing"
)

// TestWriter_LiftOptions covers --lift-options threading: write-a
// threads --lift-options + --out-option-settings into the converter
// genrule and declares options-BUILD.bazel as an output (the
// converter always writes it — a header-only placeholder when the
// element lifts nothing — so the declared out is safe). The //options
// package itself is a BUILD-TIME artifact staged into project B by
// stage-b, unlike the statically-rendered //config package.
func TestWriter_LiftOptions(t *testing.T) {
	prev := cmakeConfig
	cmakeConfig.liftOptions = []string{"FOO_FEATURE", "BACKEND"}
	t.Cleanup(func() { cmakeConfig = prev })

	body := renderCmakeProjectA(t)
	if !strings.Contains(body, "--lift-options=FOO_FEATURE,BACKEND") {
		t.Errorf("converter genrule missing --lift-options:\n%s", body)
	}
	if !strings.Contains(body, `--out-option-settings="$(location options-BUILD.bazel)"`) {
		t.Errorf("converter genrule missing --out-option-settings:\n%s", body)
	}
	if !strings.Contains(body, `"options-BUILD.bazel",`) {
		t.Errorf("genrule outs missing options-BUILD.bazel:\n%s", body)
	}
}

// TestWriter_LiftOptions_DefaultOff pins byte-stability: without the
// dial, no option flags or outputs appear.
func TestWriter_LiftOptions_DefaultOff(t *testing.T) {
	body := renderCmakeProjectA(t)
	if strings.Contains(body, "lift-options") || strings.Contains(body, "options-BUILD.bazel") {
		t.Errorf("default render must not carry option-lift fragments:\n%s", body)
	}
}
