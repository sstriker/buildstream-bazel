package main

import "fmt"

// Shared per-element render fragments used by more than one kind handler.
// Centralizing them keeps the byte-for-byte shapes (which feed golden-tested
// BUILD output) in one place instead of copy-pasted across handler_<kind>.go.

// projectBPlaceholder renders the project-B placeholder BUILD.bazel a native
// handler writes before staging: the driver script overwrites it with project
// A's bazel-bin/elements/<name>/BUILD.bazel.out after the project-A build
// succeeds. kindTag is an optional first-line parenthetical (e.g.
// " (kind:meson native)"); empty for kind:cmake.
func projectBPlaceholder(elemName, kindTag string) string {
	return fmt.Sprintf(`# Placeholder for cmd/write-a-rendered project B%s.
# The driver script overwrites this file with project A's
# bazel-bin/elements/%s/BUILD.bazel.out (the converter's output)
# after the project-A bazel build succeeds. If this file is still
# the placeholder when project B's bazel build runs, the staging
# step was skipped.
filegroup(name = "BUILD_NOT_YET_STAGED", srcs = [])
`, kindTag, elemName)
}

// traceLoadBlock renders the round-2 trace_load target an element's project-A
// BUILD carries when round-2 fallback is enabled: an action-time @trace_<elem>
// lookup via //tools:trace-lookup, keyed by the srckey hash. Shared by the
// kind:cmake and kind:meson handlers (byte-identical wiring).
func traceLoadBlock(elemName, srckeyHash string) string {
	return fmt.Sprintf(`
load("@rules_buildstream_bazel//rules:traces.bzl", "trace_load")

trace_load(
    name = "%[1]s_trace_load",
    srckey = "%[2]s",
    expect_make_db = False,
    expect_config_bundle = True,
    trace_lookup = "//tools:trace-lookup",
)
`, elemName, srckeyHash)
}

// fidelityFlagFragment returns the ` \<newline>            --fidelity=<v>`
// converter-cmd fragment for a non-default fidelity dial, or "" when the dial is
// unset / strict (the default — elided to keep the cmd byte-stable for legacy
// callsites).
func fidelityFlagFragment(fidelity string) string {
	if fidelity == "" || fidelity == fidelityStrict {
		return ""
	}
	return fmt.Sprintf(` \
            --fidelity=%s`, fidelity)
}

// diagnosticsFlagFragment returns the ` \<newline>            --diagnostics=true`
// converter-cmd fragment when the diagnostics dial is on, or "" otherwise.
func diagnosticsFlagFragment(on bool) string {
	if !on {
		return ""
	}
	return ` \
            --diagnostics=true`
}
