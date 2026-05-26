package cmakerun_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/internal/cmakerun"
	"github.com/sstriker/buildstream-bazel/converter/internal/configfold"
	"github.com/sstriker/buildstream-bazel/converter/internal/emit/sanitizerfeatures"
	"github.com/sstriker/buildstream-bazel/converter/internal/fileapi"
)

// TestMultiConfigSanitizerFeatures_LiveCMake exercises the full
// Phase-5 pipeline against the examples/sanitizer-features/cmake-side
// fixture: cmake's Ninja Multi-Config generator drives the
// CMAKE_<LANG>_FLAGS_<CONFIG> cache, configfold.ExtractSanitizerFlags
// projects the canonical sanitizer-shaped configs into
// SanitizerFlagSet maps, and sanitizerfeatures.Emit renders the
// cc_toolchain feature definitions operators drop into their
// toolchain.
//
// Closes round-4's gap: "examples/sanitizer-features/ is
// documented but not gated. Phase 5's end-to-end claim relies on
// operators manually [...]. No automated test verifies any of
// this works end-to-end."
//
// What this test pins:
//   - Multi-Config generator initializes with our sanitizer
//     configs (ASan, TSan, UBSan, Coverage) without rejecting
//     the non-canonical names.
//   - The cache exposes CMAKE_<LANG>_FLAGS_<CONFIG> entries for
//     the requested configs.
//   - ExtractSanitizerFlags + Emit roundtrip produces a
//     feature("asan") declaration in the generated .bzl with the
//     -fsanitize=address flag in its copts list.
//
// Skips cleanly when cmake / ninja aren't on PATH or cmake is
// below the multi-config-codemodel floor.
func TestMultiConfigSanitizerFeatures_LiveCMake(t *testing.T) {
	if _, err := exec.LookPath("cmake"); err != nil {
		t.Skip("cmake not on PATH; skipping multi-config sanitizer e2e")
	}
	if _, err := exec.LookPath("ninja"); err != nil {
		t.Skip("ninja not on PATH; multi-config requires the Ninja generator")
	}
	if _, _, _, err := cmakerun.AssertVersion(context.Background()); err != nil {
		t.Skipf("cmake below codemodel-v2 floor: %v", err)
	}

	src, err := filepath.Abs("../../../examples/sanitizer-features/cmake-side")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(src, "CMakeLists.txt")); err != nil {
		t.Fatalf("examples fixture missing CMakeLists.txt: %v", err)
	}
	// The example's targets reference src/lib.c + src/main.c —
	// stage minimal sources so cmake's add_library / add_executable
	// resolve without ENOENT.
	stagingSrc := filepath.Join(t.TempDir(), "cmake-side")
	if err := copyTree(src, stagingSrc); err != nil {
		t.Fatalf("stage fixture: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(stagingSrc, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(stagingSrc, "include"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stagingSrc, "src", "lib.c"), []byte("int lib(void){return 1;}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stagingSrc, "src", "main.c"), []byte("int main(void){return 0;}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	buildDir := t.TempDir()
	// Drive the sanitizer flag values via -DCMAKE_<LANG>_FLAGS_<CONFIG>
	// rather than relying on the fixture's set(... CACHE STRING ...)
	// blocks. cmake's standard initialization pre-populates the
	// cache with empty entries for every CMAKE_<LANG>_FLAGS_<CONFIG>
	// in CMAKE_CONFIGURATION_TYPES BEFORE any set() in CMakeLists.txt
	// runs, so the example's set(... CACHE STRING) blocks are no-ops
	// on the first configure. Operators in practice either pass -D
	// on the cmake command line (mirrored here) or set
	// CMAKE_<LANG>_FLAGS_<CONFIG>_INIT in a toolchain file before
	// project() — both of which take effect because the init
	// happens before cmake's standard cache initialization.
	reply, err := cmakerun.Configure(context.Background(), cmakerun.Options{
		SourceRoot: stagingSrc,
		BuildDir:   buildDir,
		BuildTypes: []string{"Release", "ASan", "TSan", "UBSan", "Coverage"},
		ExtraCacheVars: map[string]string{
			"CMAKE_C_FLAGS_ASAN":     "-fsanitize=address -fno-omit-frame-pointer -g -O1",
			"CMAKE_CXX_FLAGS_ASAN":   "-fsanitize=address -fno-omit-frame-pointer -g -O1",
			"CMAKE_C_FLAGS_TSAN":     "-fsanitize=thread -g -O1",
			"CMAKE_CXX_FLAGS_TSAN":   "-fsanitize=thread -g -O1",
			"CMAKE_C_FLAGS_UBSAN":    "-fsanitize=undefined -fno-omit-frame-pointer -g -O1",
			"CMAKE_CXX_FLAGS_UBSAN":  "-fsanitize=undefined -fno-omit-frame-pointer -g -O1",
			"CMAKE_C_FLAGS_COVERAGE": "--coverage -g -O0",
		},
		Stdout: os.Stderr,
		Stderr: os.Stderr,
	})
	if err != nil {
		t.Fatalf("cmakerun.Configure (multi-config): %v", err)
	}

	r, err := fileapi.Load(reply.Path)
	if err != nil {
		t.Fatalf("fileapi.Load: %v", err)
	}

	// Multi-config codemodel exposes one configuration entry per
	// requested build type. Sanity-check the configs are present
	// before the feature-extraction step.
	configNames := map[string]bool{}
	for _, c := range r.Codemodel.Configurations {
		configNames[c.Name] = true
	}
	for _, want := range []string{"Release", "ASan", "TSan", "UBSan", "Coverage"} {
		if !configNames[want] {
			t.Errorf("codemodel missing configuration %q (got %v)", want, configNames)
		}
	}

	// Cache sanity: the -D values we passed should land verbatim
	// on CMAKE_C_FLAGS_ASAN; if not, ExtractSanitizerFlags has
	// nothing to project.
	if e := r.Cache.Get("CMAKE_C_FLAGS_ASAN"); e == nil || e.Value == "" {
		t.Fatalf("cache missing/empty CMAKE_C_FLAGS_ASAN (ExtraCacheVars didn't land)")
	}

	sets := configfold.ExtractSanitizerFlags(r.Cache, []string{"Release", "ASan", "TSan", "UBSan", "Coverage"})
	if _, ok := sets["ASan"]; !ok {
		t.Errorf("ExtractSanitizerFlags missing ASan set (got keys %v)", mapKeys(sets))
	}
	if _, ok := sets["TSan"]; !ok {
		t.Errorf("ExtractSanitizerFlags missing TSan set")
	}

	body := sanitizerfeatures.Emit(sets)
	rendered := string(body)
	// The emit shape is documented + tested at the unit layer
	// (sanitizerfeatures.TestEmit_SingleSanitizer); the integration
	// concern is that the live-cmake cache → ExtractSanitizerFlags
	// → Emit chain produces an asan feature definition with the
	// -fsanitize=address flag the fixture set on
	// CMAKE_C_FLAGS_ASAN. Verify those two anchors.
	if !strings.Contains(rendered, `name = "asan"`) {
		t.Errorf("emit missing `name = \"asan\"`; got:\n%s", rendered)
	}
	if !strings.Contains(rendered, "-fsanitize=address") {
		t.Errorf("emit missing -fsanitize=address flag; got:\n%s", rendered)
	}
}

func copyTree(src, dst string) error {
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	for _, e := range entries {
		s := filepath.Join(src, e.Name())
		d := filepath.Join(dst, e.Name())
		if e.IsDir() {
			if err := copyTree(s, d); err != nil {
				return err
			}
			continue
		}
		data, err := os.ReadFile(s)
		if err != nil {
			return err
		}
		if err := os.WriteFile(d, data, 0o644); err != nil {
			return err
		}
	}
	return nil
}

func mapKeys(m map[string]configfold.SanitizerFlagSet) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
