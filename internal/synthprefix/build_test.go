package synthprefix_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/sstriker/buildstream-bazel/internal/synthprefix"
)

// helloBundle mirrors what convert-element-cmake emits for hello-world: three
// .cmake files with a single STATIC_LIBRARY imported target.
func writeHelloBundle(t *testing.T, dir string) {
	t.Helper()
	files := map[string]string{
		"helloConfig.cmake": `# stub config
include("${CMAKE_CURRENT_LIST_DIR}/helloTargets.cmake")
`,
		"helloTargets.cmake": `get_filename_component(_IMPORT_PREFIX "${CMAKE_CURRENT_LIST_FILE}" PATH)
add_library(hello::hello STATIC IMPORTED)
set_target_properties(hello::hello PROPERTIES
  INTERFACE_INCLUDE_DIRECTORIES "${_IMPORT_PREFIX}/include"
)
`,
		"helloTargets-release.cmake": `set_property(TARGET hello::hello APPEND PROPERTY IMPORTED_CONFIGURATIONS RELEASE)
set_target_properties(hello::hello PROPERTIES
  IMPORTED_LINK_INTERFACE_LANGUAGES_RELEASE "C"
  IMPORTED_LOCATION_RELEASE "${_IMPORT_PREFIX}/lib/libhello.a"
)
list(APPEND _cmake_import_check_targets hello::hello)
list(APPEND _cmake_import_check_files_for_hello::hello "${_IMPORT_PREFIX}/lib/libhello.a")
`,
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// TestBuildSlice_HelloOnly exercises BuildSlice — the converter-side
// builder that takes a FLAT bundle dir of <Pkg>*.cmake files and
// produces a synth-prefix-shaped slice with stubs scanned out of the
// IMPORTED_LOCATION / INTERFACE_INCLUDE_DIRECTORIES references.
func TestBuildSlice_HelloOnly(t *testing.T) {
	tmp := t.TempDir()
	bundle := filepath.Join(tmp, "src-bundle")
	writeHelloBundle(t, bundle)
	dst := filepath.Join(tmp, "prefix")

	if err := synthprefix.BuildSlice(dst, []synthprefix.DepBundle{
		{Pkg: "hello", SourceDir: bundle},
	}); err != nil {
		t.Fatalf("BuildSlice: %v", err)
	}

	// Bundle files copied to lib/cmake/<Pkg>/.
	for _, f := range []string{"helloConfig.cmake", "helloTargets.cmake", "helloTargets-release.cmake"} {
		if _, err := os.Stat(filepath.Join(dst, "lib", "cmake", "hello", f)); err != nil {
			t.Errorf("missing %s: %v", f, err)
		}
	}

	// IMPORTED_LOCATION_RELEASE stub: zero-byte file at lib/libhello.a.
	stubPath := filepath.Join(dst, "lib", "libhello.a")
	info, err := os.Stat(stubPath)
	if err != nil {
		t.Fatalf("missing stub %s: %v", stubPath, err)
	}
	if info.Size() != 0 {
		t.Errorf("stub size = %d, want 0", info.Size())
	}
	if info.IsDir() {
		t.Error("stub is a directory; want regular file")
	}

	// INTERFACE_INCLUDE_DIRECTORIES stub: directory at include/.
	incPath := filepath.Join(dst, "include")
	if info, err := os.Stat(incPath); err != nil || !info.IsDir() {
		t.Errorf("include dir missing or not a dir: %v %v", info, err)
	}
}

func TestBuildSlice_RefusesExistingDst(t *testing.T) {
	tmp := t.TempDir()
	bundle := filepath.Join(tmp, "b")
	writeHelloBundle(t, bundle)
	if err := synthprefix.BuildSlice(tmp /* exists */, []synthprefix.DepBundle{
		{Pkg: "hello", SourceDir: bundle},
	}); err == nil {
		t.Error("expected error for existing dst")
	}
}

// TestBuild_MergesSliceBundles exercises Build — the orchestrator-side
// merger that takes multiple already-synth-prefix-shaped slice bundles
// and overlays them onto a per-element prefix.
func TestBuild_MergesSliceBundles(t *testing.T) {
	tmp := t.TempDir()

	// Build a hello slice via BuildSlice (the source-of-truth shape).
	helloFlat := filepath.Join(tmp, "src-hello")
	writeHelloBundle(t, helloFlat)
	helloSlice := filepath.Join(tmp, "slice-hello")
	if err := synthprefix.BuildSlice(helloSlice, []synthprefix.DepBundle{
		{Pkg: "hello", SourceDir: helloFlat},
	}); err != nil {
		t.Fatalf("BuildSlice hello: %v", err)
	}

	// Build a foo slice the same way.
	fooFlat := filepath.Join(tmp, "src-foo")
	if err := os.MkdirAll(fooFlat, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fooFlat, "fooConfig.cmake"), []byte("# foo config\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fooFlat, "fooTargets-release.cmake"), []byte(`set_target_properties(foo::foo PROPERTIES
  IMPORTED_LOCATION_RELEASE "${_IMPORT_PREFIX}/lib/libfoo.a"
)
`), 0o644); err != nil {
		t.Fatal(err)
	}
	fooSlice := filepath.Join(tmp, "slice-foo")
	if err := synthprefix.BuildSlice(fooSlice, []synthprefix.DepBundle{
		{Pkg: "foo", SourceDir: fooFlat},
	}); err != nil {
		t.Fatalf("BuildSlice foo: %v", err)
	}

	// Merge both slices into a single prefix.
	dst := filepath.Join(tmp, "prefix")
	if err := synthprefix.Build(dst, []synthprefix.DepBundle{
		{Pkg: "foo", SourceDir: fooSlice},
		{Pkg: "hello", SourceDir: helloSlice},
	}); err != nil {
		t.Fatalf("Build: %v", err)
	}

	// Each dep gets its own lib/cmake/<Pkg>/.
	for _, p := range []string{
		"lib/cmake/hello/helloConfig.cmake",
		"lib/cmake/foo/fooConfig.cmake",
	} {
		if _, err := os.Stat(filepath.Join(dst, p)); err != nil {
			t.Errorf("missing %s: %v", p, err)
		}
	}
	// Both stubs share the lib/ dir.
	for _, p := range []string{"lib/libhello.a", "lib/libfoo.a"} {
		if info, err := os.Stat(filepath.Join(dst, p)); err != nil || info.Size() != 0 {
			t.Errorf("stub %s wrong: %v size=%v", p, err, infoSize(info))
		}
	}
}

func TestPkgFromBundle(t *testing.T) {
	// Synth-prefix-shaped bundle: BuildSlice from a flat input gives
	// the canonical layout PkgFromBundle reads.
	tmp := t.TempDir()
	flat := filepath.Join(tmp, "flat")
	if err := os.MkdirAll(flat, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(flat, "fooConfig.cmake"), []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}
	bundle := filepath.Join(tmp, "bundle")
	if err := synthprefix.BuildSlice(bundle, []synthprefix.DepBundle{
		{Pkg: "foo", SourceDir: flat},
	}); err != nil {
		t.Fatalf("BuildSlice: %v", err)
	}
	pkg, err := synthprefix.PkgFromBundle(bundle)
	if err != nil {
		t.Fatal(err)
	}
	if pkg != "foo" {
		t.Errorf("PkgFromBundle = %q, want foo", pkg)
	}

	// Empty bundle dir (no lib/cmake/ at all).
	emptyDir := t.TempDir()
	pkg, err = synthprefix.PkgFromBundle(emptyDir)
	if err != nil {
		t.Fatal(err)
	}
	if pkg != "" {
		t.Errorf("empty bundle returned %q, want \"\"", pkg)
	}
}

func infoSize(info os.FileInfo) any {
	if info == nil {
		return "<nil>"
	}
	return info.Size()
}
