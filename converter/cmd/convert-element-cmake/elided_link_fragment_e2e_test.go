//go:build e2e

// elided_link_fragment_e2e_test exercises the #220 audit-tag
// emission against a real cmake configure (not a synthetic
// fileapi.Reply{} struct). Pinned to close the
// synthetic-tests-only critique on PR #228: the unit-level
// TestToIR_ElidedLinkFragment covers the IR-shape logic, but
// it doesn't prove that cmake's codemodel — on the actual
// shapes operators hit — produces a `libraries`-role abs-path
// fragment that flows through to the tag.
//
// Gated behind the `e2e` build tag; needs real cmake +
// convert-element-cmake (same plumbing as the other
// fidelity_e2e / cmakeconsumer_e2e tests in this package).
// Shares lookupConverter / mustRun.
package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestE2E_ElidedLinkFragment_TagFires(t *testing.T) {
	conv := lookupConverter(t)
	cmakeBin, err := exec.LookPath("cmake")
	if err != nil {
		t.Skipf("cmake not on PATH: %v", err)
	}

	// Build a minimal cmake project that links an EXECUTABLE
	// against an IMPORTED SHARED library declared at an
	// absolute path outside both cmakeSrc and cmakeBuild. This
	// is the shape #220 reported: cmake's codemodel records
	// the IMPORTED_LOCATION as a `libraries`-role link fragment
	// the converter previously dropped silently. The new audit
	// tag should fire because:
	//   - No imports manifest is provided (so LookupLinkPath
	//     misses).
	//   - The path is outside the standard system locations, so
	//     it isn't a toolchain -l<name> lift.
	//   - The path lies outside hostPrefix (no HostPrefixDir
	//     set on the converter invocation).
	src := t.TempDir()
	// Place the "external" library under tmp/ — outside the
	// project's source tree but on disk so cmake's
	// IMPORTED_LOCATION access check doesn't fail.
	externalLibDir := t.TempDir()
	externalLib := filepath.Join(externalLibDir, "libmystery.so")
	if err := os.WriteFile(externalLib, []byte{}, 0o644); err != nil {
		t.Fatalf("write external lib stub: %v", err)
	}

	if err := os.WriteFile(filepath.Join(src, "CMakeLists.txt"), []byte(`cmake_minimum_required(VERSION 3.20)
project(elided_lf C)
add_library(extlib SHARED IMPORTED)
set_target_properties(extlib PROPERTIES IMPORTED_LOCATION "`+externalLib+`")
add_executable(tool main.c)
target_link_libraries(tool PRIVATE extlib)
`), 0o644); err != nil {
		t.Fatalf("write CMakeLists.txt: %v", err)
	}
	if err := os.WriteFile(filepath.Join(src, "main.c"), []byte(`int main(void) { return 0; }
`), 0o644); err != nil {
		t.Fatalf("write main.c: %v", err)
	}

	// Run convert-element-cmake against the source root. The
	// converter spawns cmake itself (via --source-root) so we
	// don't have to pre-configure.
	out := t.TempDir()
	build := filepath.Join(out, "BUILD.bazel")
	mustRun(t, exec.CommandContext(context.Background(), conv,
		"--source-root", src,
		"--out-build", build,
	))
	_ = cmakeBin // not invoked directly; convert-element-cmake handles configure

	body, err := os.ReadFile(build)
	if err != nil {
		t.Fatalf("read BUILD.bazel: %v", err)
	}
	got := string(body)
	wantTag := "cmake-unresolved-link-arm=" + externalLib
	if !strings.Contains(got, wantTag) {
		t.Errorf("BUILD.bazel missing audit tag %q\n--- got ---\n%s", wantTag, got)
	}
}
