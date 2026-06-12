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

	"github.com/sstriker/buildstream-bazel/internal/manifest"
	"github.com/sstriker/buildstream-bazel/internal/wrappergen"
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

// TestE2E_CMakeConsumer_AliasTarget covers the alias-target case: the
// producer publishes a consumer-facing add_library(Greeter::Greeter
// ALIAS greeter) whose name differs from any exported target, and the
// consumer links that alias. The codemodel omits ALIAS targets, so
// both channels must re-publish it from the trace: the bundle as an
// `add_library(... ALIAS ...)` line (or the consumer's find_package
// configure fails on the undefined Greeter::Greeter), and exports.json
// as Greeter::Greeter → the underlying target's label.
func TestE2E_CMakeConsumer_AliasTarget(t *testing.T) {
	conv := lookupConverter(t)
	if _, err := exec.LookPath("cmake"); err != nil {
		t.Skipf("cmake not on PATH: %v", err)
	}

	prodSrc := t.TempDir()
	mustWrite(t, filepath.Join(prodSrc, "CMakeLists.txt"), `cmake_minimum_required(VERSION 3.20)
project(greetpkg LANGUAGES C VERSION 1.0.0)
add_library(greeter STATIC greeter.c)
add_library(Greeter::Greeter ALIAS greeter)
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
	if body, _ := os.ReadFile(exportsJSON); !strings.Contains(string(body), `"Greeter::Greeter"`) {
		t.Fatalf("exports.json missing the alias Greeter::Greeter:\n%s", body)
	}

	prefix := filepath.Join(out, "prefix")
	mustRun(t, exec.CommandContext(context.Background(), "cp", "-r", bundle+"/.", mustMkdir(t, prefix)))

	consSrc := t.TempDir()
	// Consumer links the ALIAS name, not the underlying target.
	mustWrite(t, filepath.Join(consSrc, "CMakeLists.txt"), `cmake_minimum_required(VERSION 3.20)
project(cons LANGUAGES C VERSION 1.0.0)
find_package(Greeter CONFIG REQUIRED)
add_library(cons STATIC cons.c)
target_link_libraries(cons PUBLIC Greeter::Greeter)
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
		t.Fatalf("consumer linking the alias Greeter::Greeter did not resolve to //elements/greetlib:greeter:\n%s", body)
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

// TestE2E_CMakeConsumer_ImportedTool is the manifest-provided-TOOL
// round trip (the protoc shape, generalized): the producer installs an
// executable into its export set; the consumer's add_custom_command
// drives it via $<TARGET_FILE:Tool::gen>, which cmake resolves to the
// PREFIX-staged path at configure time. The gates compose across every
// layer this slice touches:
//
//   - exports.json carries Tool::gen → //elements/toolpkg:gen with the
//     anchored bin/ link_path (the producer half);
//   - the bundle publishes add_executable(Tool::gen IMPORTED) and the
//     synth-prefix staging stubs bin/gen (so the consumer's configure
//     resolves TARGET_FILE against the prefix);
//   - the consumer's converted genrule carries
//     $(execpath //elements/toolpkg:gen) + the label in tools — the
//     hostPrefix→anchor remap matching the manifest row (the #596
//     consumer half, now fed by a generated manifest instead of a
//     hand-written one).
func TestE2E_CMakeConsumer_ImportedTool(t *testing.T) {
	conv := lookupConverter(t)
	if _, err := exec.LookPath("cmake"); err != nil {
		t.Skipf("cmake not on PATH: %v", err)
	}

	// Producer: an installed, exported tool.
	prodSrc := t.TempDir()
	mustWrite(t, filepath.Join(prodSrc, "CMakeLists.txt"), `cmake_minimum_required(VERSION 3.20)
project(toolpkg LANGUAGES C VERSION 1.0.0)
add_executable(gen gen.c)
install(TARGETS gen EXPORT toolTargets RUNTIME DESTINATION bin)
install(EXPORT toolTargets FILE toolTargets.cmake NAMESPACE Tool:: DESTINATION lib/cmake/toolpkg)
`)
	mustWrite(t, filepath.Join(prodSrc, "gen.c"),
		"#include <stdio.h>\nint main(void){puts(\"int gen_value(void){return 7;}\");return 0;}\n")

	out := t.TempDir()
	bundle := filepath.Join(out, "bundle")
	exportsJSON := filepath.Join(out, "exports.json")
	mustRun(t, exec.CommandContext(context.Background(), conv,
		"--source-root", prodSrc,
		"--bazel-package-path", "elements/toolpkg",
		"--out-build", filepath.Join(out, "prod.BUILD"),
		"--out-bundle-dir", bundle,
		"--out-exports", exportsJSON,
	))
	if body, _ := os.ReadFile(exportsJSON); !strings.Contains(string(body), `"Tool::gen"`) ||
		!strings.Contains(string(body), `"//elements/toolpkg:gen"`) ||
		!strings.Contains(string(body), `"/opt/prefix/bin/gen"`) {
		t.Fatalf("exports.json missing the tool row (Tool::gen → //elements/toolpkg:gen, anchored bin/gen):\n%s", body)
	}
	tgts, _ := os.ReadFile(filepath.Join(bundle, "lib", "cmake", "Tool", "ToolTargets.cmake"))
	if !strings.Contains(string(tgts), "add_executable(Tool::gen IMPORTED)") {
		t.Fatalf("bundle missing the IMPORTED executable:\n%s", tgts)
	}
	if _, err := os.Stat(filepath.Join(bundle, "bin", "gen")); err != nil {
		t.Fatalf("synth-prefix staging did not stub bin/gen (EXISTS check + TARGET_FILE need it): %v", err)
	}

	// Consumer: a custom command drives the imported tool.
	prefix := filepath.Join(out, "prefix")
	mustRun(t, exec.CommandContext(context.Background(), "cp", "-r", bundle+"/.", mustMkdir(t, prefix)))

	consSrc := t.TempDir()
	mustWrite(t, filepath.Join(consSrc, "CMakeLists.txt"), `cmake_minimum_required(VERSION 3.20)
project(cons LANGUAGES C VERSION 1.0.0)
find_package(Tool CONFIG REQUIRED)
add_custom_command(
    OUTPUT ${CMAKE_CURRENT_BINARY_DIR}/gen_out.c
    COMMAND $<TARGET_FILE:Tool::gen> > ${CMAKE_CURRENT_BINARY_DIR}/gen_out.c
    VERBATIM)
add_library(cons STATIC cons.c ${CMAKE_CURRENT_BINARY_DIR}/gen_out.c)
install(TARGETS cons ARCHIVE DESTINATION lib)
`)
	mustWrite(t, filepath.Join(consSrc, "cons.c"), "int gen_value(void);\nint c(void){return gen_value();}\n")

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
	if !strings.Contains(string(body), "$(execpath //elements/toolpkg:gen)") {
		t.Fatalf("consumer genrule cmd did not lift the prefix tool path to $(execpath //elements/toolpkg:gen):\n%s", body)
	}
	if !strings.Contains(string(body), `"//elements/toolpkg:gen"`) {
		t.Fatalf("consumer genrule missing the tool label in tools:\n%s", body)
	}
	if strings.Contains(string(body), prefix) {
		t.Fatalf("convert-time prefix path leaked into the consumer BUILD:\n%s", body)
	}
}

// TestE2E_CMakeConsumer_ExportDepsClosure: the Export.Deps round trip
// for the shape the field exists for — a HAND-WRITTEN manifest whose
// export label is prebuilt-backed (models no deps), carrying its
// declared closure. The consumer links ONLY Greeter::core; its
// converted BUILD must wire core's label AND the closure labels (the
// missing-symbols mechanisms). The producer side asserts the INVERSE
// invariant: a converted element's generated exports.json carries NO
// deps field — its labels are real rules, and filling Deps would
// double-wire consumers with direct edges the trace-gated drop exists
// to avoid.
func TestE2E_CMakeConsumer_ExportDepsClosure(t *testing.T) {
	conv := lookupConverter(t)
	if _, err := exec.LookPath("cmake"); err != nil {
		t.Skipf("cmake not on PATH: %v", err)
	}

	// Producer half: generated manifests honor the empty-Deps invariant.
	prodSrc := t.TempDir()
	mustWrite(t, filepath.Join(prodSrc, "CMakeLists.txt"), `cmake_minimum_required(VERSION 3.20)
project(greetpkg LANGUAGES C VERSION 1.0.0)
add_library(base STATIC base.c)
add_library(core STATIC core.c)
target_link_libraries(core PUBLIC base)
target_include_directories(core PUBLIC
    $<BUILD_INTERFACE:${CMAKE_CURRENT_SOURCE_DIR}/include>
    $<INSTALL_INTERFACE:include>)
install(TARGETS core base EXPORT greetTargets ARCHIVE DESTINATION lib)
install(DIRECTORY include/ DESTINATION include)
install(EXPORT greetTargets FILE greetTargets.cmake NAMESPACE Greeter:: DESTINATION lib/cmake/greetpkg)
`)
	mustWrite(t, filepath.Join(prodSrc, "base.c"), "int base_v(void){return 1;}\n")
	mustWrite(t, filepath.Join(prodSrc, "core.c"), "int base_v(void);\nint core_v(void){return base_v();}\n")
	mustWrite(t, filepath.Join(prodSrc, "include", "core.h"), "int core_v(void);\n")

	out := t.TempDir()
	bundle := filepath.Join(out, "bundle")
	generatedJSON := filepath.Join(out, "generated-exports.json")
	mustRun(t, exec.CommandContext(context.Background(), conv,
		"--source-root", prodSrc,
		"--bazel-package-path", "elements/greetlib",
		"--out-build", filepath.Join(out, "prod.BUILD"),
		"--out-bundle-dir", bundle,
		"--out-exports", generatedJSON,
	))
	if body, _ := os.ReadFile(generatedJSON); strings.Contains(string(body), `"deps"`) {
		t.Fatalf("converted element's exports.json must not fill Deps (invariant: Deps = unmodeled closure):\n%s", body)
	}

	// Consumer half: a HAND-WRITTEN manifest models the prebuilt-backed
	// shape — same cmake names, but the labels point at a prebuilts
	// package and core's row declares its closure.
	handJSON := filepath.Join(out, "hand-exports.json")
	mustWrite(t, handJSON, `{
  "version": 1,
  "elements": [
    {
      "name": "greetlib",
      "exports": [
        {
          "cmake_target": "Greeter::core",
          "bazel_label": "//prebuilts/greet:core",
          "deps": ["//prebuilts/greet:base"]
        },
        {
          "cmake_target": "Greeter::base",
          "bazel_label": "//prebuilts/greet:base"
        }
      ]
    }
  ]
}
`)

	prefix := filepath.Join(out, "prefix")
	mustRun(t, exec.CommandContext(context.Background(), "cp", "-r", bundle+"/.", mustMkdir(t, prefix)))

	consSrc := t.TempDir()
	mustWrite(t, filepath.Join(consSrc, "CMakeLists.txt"), `cmake_minimum_required(VERSION 3.20)
project(cons LANGUAGES C VERSION 1.0.0)
find_package(Greeter CONFIG REQUIRED)
add_library(cons STATIC cons.c)
target_link_libraries(cons PUBLIC Greeter::core)
install(TARGETS cons ARCHIVE DESTINATION lib)
`)
	mustWrite(t, filepath.Join(consSrc, "cons.c"), "int c(void){return 0;}\n")

	consBuild := filepath.Join(out, "cons.BUILD")
	mustRun(t, exec.CommandContext(context.Background(), conv,
		"--source-root", consSrc,
		"--bazel-package-path", "elements/cons",
		"--prefix-dir", prefix,
		"--exports-in", handJSON,
		"--out-build", consBuild,
	))
	got, err := os.ReadFile(consBuild)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"//prebuilts/greet:core"`, `"//prebuilts/greet:base"`} {
		if !strings.Contains(string(got), want) {
			t.Fatalf("consumer BUILD missing %s (declared closure not wired; the consumer only names Greeter::core):\n%s", want, got)
		}
	}
}

// TestE2E_CMakeConsumer_WrapperGeneratorRoundTrip completes the loop
// the Export.Deps invariant describes: a hand-written prebuilt-backed
// manifest (closure in Deps) runs through imports-wrapper-gen, which
// materializes the closure as REAL Bazel deps on synthesized wrappers
// and CLEARS Deps in its output manifest. A consumer converted against
// the REWRITTEN manifest wires exactly ONE direct edge — the wrapper —
// with the closure riding Bazel transitivity; converting against the
// ORIGINAL manifest (the previous test) wires the closure directly.
// Together the two pin both halves of the invariant.
func TestE2E_CMakeConsumer_WrapperGeneratorRoundTrip(t *testing.T) {
	conv := lookupConverter(t)
	if _, err := exec.LookPath("cmake"); err != nil {
		t.Skipf("cmake not on PATH: %v", err)
	}

	// The same producer/bundle as the closure test (the bundle is only
	// needed so the consumer's find_package configures).
	prodSrc := t.TempDir()
	mustWrite(t, filepath.Join(prodSrc, "CMakeLists.txt"), `cmake_minimum_required(VERSION 3.20)
project(greetpkg LANGUAGES C VERSION 1.0.0)
add_library(core STATIC core.c)
target_include_directories(core PUBLIC
    $<BUILD_INTERFACE:${CMAKE_CURRENT_SOURCE_DIR}/include>
    $<INSTALL_INTERFACE:include>)
install(TARGETS core EXPORT greetTargets ARCHIVE DESTINATION lib)
install(DIRECTORY include/ DESTINATION include)
install(EXPORT greetTargets FILE greetTargets.cmake NAMESPACE Greeter:: DESTINATION lib/cmake/greetpkg)
`)
	mustWrite(t, filepath.Join(prodSrc, "core.c"), "int core_v(void){return 1;}\n")
	mustWrite(t, filepath.Join(prodSrc, "include", "core.h"), "int core_v(void);\n")

	out := t.TempDir()
	bundle := filepath.Join(out, "bundle")
	mustRun(t, exec.CommandContext(context.Background(), conv,
		"--source-root", prodSrc,
		"--bazel-package-path", "elements/greetlib",
		"--out-build", filepath.Join(out, "prod.BUILD"),
		"--out-bundle-dir", bundle,
	))

	// Hand-written prebuilt-backed manifest: core's closure in Deps.
	hand := &manifest.Imports{
		Version: 1,
		Elements: []*manifest.Element{{
			Name: "greetlib",
			Exports: []*manifest.Export{
				{
					CMakeTarget: "Greeter::core",
					BazelLabel:  "//old/prebuilts:core",
					LinkPaths:   []string{"/opt/prefix/lib/libcore.a"},
					Deps:        []string{"//old/prebuilts:base"},
				},
				{
					CMakeTarget: "Greeter::base",
					BazelLabel:  "//old/prebuilts:base",
					LinkPaths:   []string{"/opt/prefix/lib/libbase.a"},
				},
			},
		}},
	}

	// Generator: wrappers + rewritten manifest.
	build, rewritten, err := wrappergen.Generate(hand, "prebuilts/greet", "")
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if !strings.Contains(string(build), `":core_archive",
        "//prebuilts/greet:base",`) {
		t.Fatalf("wrapper BUILD missing the materialized closure:\n%s", build)
	}
	wrappedJSON := filepath.Join(out, "exports.wrapped.json")
	if err := wrappergen.WriteManifest(wrappedJSON, rewritten); err != nil {
		t.Fatal(err)
	}
	if body, _ := os.ReadFile(wrappedJSON); strings.Contains(string(body), `"deps"`) {
		t.Fatalf("rewritten manifest must clear Deps (consume-and-clear):\n%s", body)
	}

	// Consumer against the REWRITTEN manifest: exactly one direct edge.
	prefix := filepath.Join(out, "prefix")
	mustRun(t, exec.CommandContext(context.Background(), "cp", "-r", bundle+"/.", mustMkdir(t, prefix)))
	consSrc := t.TempDir()
	mustWrite(t, filepath.Join(consSrc, "CMakeLists.txt"), `cmake_minimum_required(VERSION 3.20)
project(cons LANGUAGES C VERSION 1.0.0)
find_package(Greeter CONFIG REQUIRED)
add_library(cons STATIC cons.c)
target_link_libraries(cons PUBLIC Greeter::core)
install(TARGETS cons ARCHIVE DESTINATION lib)
`)
	mustWrite(t, filepath.Join(consSrc, "cons.c"), "int c(void){return 0;}\n")

	consBuild := filepath.Join(out, "cons.BUILD")
	mustRun(t, exec.CommandContext(context.Background(), conv,
		"--source-root", consSrc,
		"--bazel-package-path", "elements/cons",
		"--prefix-dir", prefix,
		"--exports-in", wrappedJSON,
		"--out-build", consBuild,
	))
	got, err := os.ReadFile(consBuild)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), `"//prebuilts/greet:core"`) {
		t.Fatalf("consumer BUILD missing the wrapper label:\n%s", got)
	}
	if strings.Contains(string(got), `"//prebuilts/greet:base"`) || strings.Contains(string(got), "//old/prebuilts") {
		t.Fatalf("consumer must wire ONLY the wrapper (closure rides Bazel transitivity; no double-wiring):\n%s", got)
	}
}
