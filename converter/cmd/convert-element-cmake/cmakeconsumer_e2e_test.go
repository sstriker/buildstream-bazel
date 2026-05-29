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
	"strings"
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

// TestE2E_CMakeConsumer_NamespaceDiffersFromProject exercises the
// full producer→consumer export channel end-to-end through the
// converter, with project name != export namespace != target name
// (the zlib-shaped divergence no checked-in sample fixture covers):
//
//   - Producer: project(greetpkg), add_library(greeter),
//     install(EXPORT ... NAMESPACE Greeter::). The synthetic bundle
//     must be keyed on the namespace stem (lib/cmake/Greeter/
//     GreeterConfig.cmake exporting Greeter::greeter), and exports.json
//     must map Greeter::greeter → //elements/greetlib:greeter.
//   - Consumer: find_package(Greeter CONFIG) + link Greeter::greeter,
//     converted with --prefix-dir (the staged bundle) + --exports-in
//     (the producer's exports.json).
//
// The gate: the consumer's BUILD.bazel.out resolves the dep to the
// producer's real label — proving the namespace recovery, the
// namespace-keyed bundle, and the exports.json label mapping all
// compose. Were any link broken, the convention guess (greetlib::
// greetlib) would leave the dep unresolved instead.
func TestE2E_CMakeConsumer_NamespaceDiffersFromProject(t *testing.T) {
	conv := lookupConverter(t)
	if _, err := exec.LookPath("cmake"); err != nil {
		t.Skipf("cmake not on PATH: %v", err)
	}

	// Producer.
	prodSrc := t.TempDir()
	mustWrite(t, filepath.Join(prodSrc, "CMakeLists.txt"), `cmake_minimum_required(VERSION 3.20)
project(greetpkg LANGUAGES C VERSION 1.0.0)
add_library(greeter STATIC greeter.c)
target_include_directories(greeter PUBLIC
    $<BUILD_INTERFACE:${CMAKE_CURRENT_SOURCE_DIR}/include>
    $<INSTALL_INTERFACE:include>)
install(TARGETS greeter EXPORT greetTargets ARCHIVE DESTINATION lib)
install(DIRECTORY include/ DESTINATION include)
install(EXPORT greetTargets FILE greetTargets.cmake NAMESPACE Greeter:: DESTINATION lib/cmake/greetpkg)
`)
	mustWrite(t, filepath.Join(prodSrc, "greeter.c"), "int greet(void){return 42;}\n")
	mustWrite(t, filepath.Join(prodSrc, "include", "greeter.h"), "int greet(void);\n")

	out := t.TempDir()
	bundle := filepath.Join(out, "bundle")
	exportsJSON := filepath.Join(out, "exports.json")
	mustRun(t, exec.CommandContext(context.Background(), conv,
		"--source-root", prodSrc,
		"--bazel-package-path", "elements/greetlib",
		"--out-build", filepath.Join(out, "prod.BUILD"),
		"--out-bundle-dir", bundle,
		"--out-exports", exportsJSON,
	))

	// The bundle must be keyed on the namespace stem, not the project name.
	if _, err := os.Stat(filepath.Join(bundle, "lib", "cmake", "Greeter", "GreeterConfig.cmake")); err != nil {
		t.Fatalf("bundle not keyed on namespace stem (want lib/cmake/Greeter/GreeterConfig.cmake): %v", err)
	}
	if body, _ := os.ReadFile(exportsJSON); !strings.Contains(string(body), `"Greeter::greeter"`) ||
		!strings.Contains(string(body), `"//elements/greetlib:greeter"`) {
		t.Fatalf("exports.json missing Greeter::greeter → //elements/greetlib:greeter:\n%s", body)
	}

	// Consumer: stage the producer bundle into a prefix, then convert.
	prefix := filepath.Join(out, "prefix")
	mustRun(t, exec.CommandContext(context.Background(), "cp", "-r", bundle+"/.", mustMkdir(t, prefix)))

	consSrc := t.TempDir()
	mustWrite(t, filepath.Join(consSrc, "CMakeLists.txt"), `cmake_minimum_required(VERSION 3.20)
project(cons LANGUAGES C VERSION 1.0.0)
find_package(Greeter CONFIG REQUIRED)
add_library(cons STATIC cons.c)
target_link_libraries(cons PUBLIC Greeter::greeter)
install(TARGETS cons ARCHIVE DESTINATION lib)
`)
	mustWrite(t, filepath.Join(consSrc, "cons.c"), "int c(void){return 0;}\n")

	consBuild := filepath.Join(out, "cons.BUILD")
	mustRun(t, exec.CommandContext(context.Background(), conv,
		"--source-root", consSrc,
		"--bazel-package-path", "elements/cons",
		"--prefix-dir", prefix,
		"--exports-in", exportsJSON,
		"--out-build", consBuild,
	))

	body, err := os.ReadFile(consBuild)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `"//elements/greetlib:greeter"`) {
		t.Fatalf("consumer BUILD did not resolve the dep to the producer label //elements/greetlib:greeter:\n%s", body)
	}
}

func mustWrite(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func mustMkdir(t *testing.T, path string) string {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}
