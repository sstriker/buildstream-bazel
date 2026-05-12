package main

import (
	"strings"
	"testing"
)

// TestWrapAutotoolsPipelineCmds_DefaultDoesNotPassSourceRoot
// pins the legacy AC byte schema: without the opt-in flag,
// the build-tracer invocation must NOT carry --source-root.
// Adding it would invalidate every existing AC entry for
// trace-driven kinds; the flag-gating keeps the no-narrow-audit
// production deployments byte-stable until they explicitly
// rebake.
func TestWrapAutotoolsPipelineCmds_DefaultDoesNotPassSourceRoot(t *testing.T) {
	prev := traceConfig
	t.Cleanup(func() { traceConfig = prev })
	traceConfig.traceSourceRoot = false

	got := wrapAutotoolsPipelineCmds("./configure && make")
	if strings.Contains(got, "--source-root") {
		t.Errorf("default render carries --source-root; AC byte schema would invalidate.\n%s", got)
	}
}

// TestWrapAutotoolsPipelineCmds_OptInPassesSourceRoot exercises
// the audit-gate-enabling case: with traceSourceRoot=true, the
// build-tracer invocation gets --source-root="$$BUILD_ROOT"
// threaded in so openat events populate the trace oracle.
func TestWrapAutotoolsPipelineCmds_OptInPassesSourceRoot(t *testing.T) {
	prev := traceConfig
	t.Cleanup(func() { traceConfig = prev })
	traceConfig.traceSourceRoot = true

	got := wrapAutotoolsPipelineCmds("./configure && make")
	if !strings.Contains(got, `--source-root="$$BUILD_ROOT"`) {
		t.Errorf("opt-in render missing --source-root=$$BUILD_ROOT.\n%s", got)
	}
	// The body cmds must still appear; we shouldn't have
	// shifted the %s arg index in the format string.
	if !strings.Contains(got, "./configure && make") {
		t.Errorf("body cmds missing from rendered wrapper.\n%s", got)
	}
}
