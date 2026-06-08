package main

import (
	"fmt"
	"path/filepath"
	"strings"
)

// Shared per-element render fragments used by more than one kind handler.
// Centralizing them keeps the byte-for-byte shapes (which feed golden-tested
// BUILD output) in one place instead of copy-pasted across handler_<kind>.go.

// writeDepsImportsManifest renders elements/<name>/imports.json — the
// manifest.Imports envelope mapping each dep's namespaced cmake target
// (<dep>::<dep>) to its Bazel label (//elements/<dep>:<dep>) — from the
// element's non-stub deps (dep != nil && dep.Bst != nil). Returns (false, nil)
// without writing when the element has no such deps. Shared verbatim by the
// kind:meson and kind:pyproject native handlers; kind:cmake and kind:autotools
// carry their own variants (different dep source / extra export fields).
func writeDepsImportsManifest(elem *element, elemPkg string) (bool, error) {
	var names []string
	for _, dep := range elem.Deps {
		if dep == nil || dep.Bst == nil {
			continue
		}
		names = append(names, dep.Name)
	}
	if len(names) == 0 {
		return false, nil
	}
	var b strings.Builder
	b.WriteString(`{
  "version": 1,
  "elements": [
`)
	for i, name := range names {
		if i > 0 {
			b.WriteString(",\n")
		}
		fmt.Fprintf(&b, `    {
      "name": %q,
      "exports": [
        {
          "cmake_target": %q,
          "bazel_label": "//elements/%s:%s"
        }
      ]
    }`, name,
			name+"::"+name,
			name, name)
	}
	b.WriteString(`
  ]
}
`)
	if err := writeFile(filepath.Join(elemPkg, "imports.json"), b.String()); err != nil {
		return false, err
	}
	return true, nil
}

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
