//go:build e2e

// cmakeconsumer_e2e_test is the CMake-side consumer gate: an
// unrelated downstream CMake project resolves a converted element's
// synthesized cmake-config bundle via
// find_package(<Pkg> CONFIG REQUIRED).
//
// Configure-time success is the gate. The bundle's
// IMPORTED_LOCATION stubs are zero-byte files (cmake's
// if(NOT EXISTS) check passes via access(R_OK)); we don't link,
// only resolve the imported target and touch its
// IMPORTED_LOCATION_RELEASE.
//
// Re-homed from the (now-deleted) orchestrator/internal/orchestrator/.
// The orchestrator version produced the bundle as a side effect of
// orchestrator.Run()'s synth-prefix tree; convert-element-cmake's
// --out-bundle-dir produces exactly the same lib/cmake/<Pkg>/ layout
// directly, so the re-homed gate calls the converter and points a
// downstream cmake's CMAKE_PREFIX_PATH straight at it — no
// orchestrator, no synth-prefix indirection.
//
// Gated behind the `e2e` build tag; needs real cmake +
// convert-element-cmake. CI runs it via `make e2e-cmake-consumer`.
// Shares lookupConverter / mustRun / testLog with
// fidelity_e2e_test.go (same package).
package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestE2E_CMakeConsumer_FindPackageAgainstBundle(t *testing.T) {
	conv := lookupConverter(t)
	cmakeBin, err := exec.LookPath("cmake")
	if err != nil {
		t.Skipf("cmake not on PATH: %v", err)
	}

	// cross-cmake/prod is a kind:cmake fixture that exports a
	// cmake-config bundle: install(EXPORT prodTargets NAMESPACE
	// prod::). convert-element-cmake synthesizes the consumable
	// lib/cmake/prod/{prodConfig,prodTargets,...}.cmake bundle from it.
	prodSrc, err := filepath.Abs("../../../testdata/meta-project/cross-cmake/sources/prod")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(prodSrc); err != nil {
		t.Fatalf("prod fixture missing: %v", err)
	}

	convOut := t.TempDir()
	bundle := filepath.Join(convOut, "bundle")
	mustRun(t, exec.CommandContext(context.Background(), conv,
		"--source-root", prodSrc,
		"--out-build", filepath.Join(convOut, "BUILD.bazel"),
		"--out-bundle-dir", bundle,
	))
	if _, err := os.Stat(filepath.Join(bundle, "lib", "cmake", "prod")); err != nil {
		t.Fatalf("converter produced no lib/cmake/prod bundle dir: %v", err)
	}

	// Downstream consumer: a standalone cmake project that does
	// find_package(prod CONFIG) against the bundle and touches the
	// imported target. Inlined rather than carried as a fixture file
	// — it's the assertion, not test data.
	consumerSrc := t.TempDir()
	consumerCMake := `cmake_minimum_required(VERSION 3.20)
project(cmake_consumer LANGUAGES C)

# Resolve a converted element via its synthesized cmake-config
# bundle. Configure-time success is the gate; the bundle's
# IMPORTED_LOCATION stubs aren't real archives, so we don't link.
find_package(prod CONFIG REQUIRED)

# Touch the imported target to force the link interface to
# generate; a broken targets file fails here.
get_target_property(_prod_loc prod::prod IMPORTED_LOCATION_RELEASE)
if(NOT _prod_loc)
    message(FATAL_ERROR "find_package(prod) succeeded but prod::prod has no IMPORTED_LOCATION_RELEASE")
endif()
message(STATUS "cmake-consumer resolved prod::prod at ${_prod_loc}")
`
	if err := os.WriteFile(filepath.Join(consumerSrc, "CMakeLists.txt"),
		[]byte(consumerCMake), 0o644); err != nil {
		t.Fatal(err)
	}

	consumerBuild := t.TempDir()
	cmd := exec.CommandContext(context.Background(), cmakeBin,
		"-S", consumerSrc,
		"-B", consumerBuild,
		"-DCMAKE_PREFIX_PATH="+bundle,
	)
	cmd.Stdout = testLog{t}
	cmd.Stderr = testLog{t}
	if err := cmd.Run(); err != nil {
		t.Fatalf("downstream cmake configure failed against CMAKE_PREFIX_PATH=%s: %v", bundle, err)
	}
}
