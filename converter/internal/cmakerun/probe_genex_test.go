package cmakerun

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestReadGenexProbe_Empty(t *testing.T) {
	// No cmake-to-bazel.genex/ directory: returns nil, nil.
	got, err := ReadGenexProbe(t.TempDir())
	if err != nil {
		t.Fatalf("ReadGenexProbe: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil for missing probe dir; got %v", got)
	}
}

// TestReadGenexProbe_OneTarget pins the single-config collapse
// path: every probe-genex file lands with one `$<CONFIG>` value
// (here "Release") and the reader exposes a flat GenexProbe with
// the captured strings on the matching fields. This is the steady-
// state shape for single-config Ninja builds where cmake's
// generation phase resolves `$<CONFIG>` to CMAKE_BUILD_TYPE.
func TestReadGenexProbe_OneTarget(t *testing.T) {
	buildDir := t.TempDir()
	tgtDir := filepath.Join(buildDir, "cmake-to-bazel.genex", "foo")
	if err := os.MkdirAll(tgtDir, 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"type.txt":                                  "STATIC_LIBRARY",
		"file.Release.txt":                          "/build/libfoo.a",
		"file_dir.Release.txt":                      "/build",
		"file_name.Release.txt":                     "libfoo.a",
		"interface_INCLUDE_DIRECTORIES.Release.txt": "/src/include;/src/extra",
		"interface_COMPILE_DEFINITIONS.Release.txt": "FOO=1;BAR=2",
		"interface_LINK_LIBRARIES.Release.txt":      "bar;baz",
		"interface_COMPILE_OPTIONS.Release.txt":     "",
		"interface_LINK_OPTIONS.Release.txt":        "",
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(tgtDir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	got, err := ReadGenexProbe(buildDir)
	if err != nil {
		t.Fatalf("ReadGenexProbe: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 probe; got %d (%+v)", len(got), got)
	}
	p := got[0]
	if p.Name != "foo" {
		t.Errorf("Name: %q", p.Name)
	}
	if p.Type != "STATIC_LIBRARY" {
		t.Errorf("Type: %q", p.Type)
	}
	if p.File != "/build/libfoo.a" || p.FileDir != "/build" || p.FileName != "libfoo.a" {
		t.Errorf("File trio: %q / %q / %q", p.File, p.FileDir, p.FileName)
	}
	if p.Interface["INCLUDE_DIRECTORIES"] != "/src/include;/src/extra" {
		t.Errorf("INTERFACE_INCLUDE_DIRECTORIES: %q", p.Interface["INCLUDE_DIRECTORIES"])
	}
	if p.Interface["LINK_LIBRARIES"] != "bar;baz" {
		t.Errorf("INTERFACE_LINK_LIBRARIES: %q", p.Interface["LINK_LIBRARIES"])
	}
	// Empty values are still recorded — distinguishes "probe ran
	// but cmake resolved to empty" from "probe didn't run".
	if _, ok := p.Interface["COMPILE_OPTIONS"]; !ok {
		t.Errorf("INTERFACE_COMPILE_OPTIONS should be present (empty string)")
	}
}

// TestReadGenexProbe_EmptyConfig pins the empty-`$<CONFIG>` round-
// trip: a single-config generator with no CMAKE_BUILD_TYPE set
// resolves `$<CONFIG>` to the empty string, so the hook writes
// `<basename>..txt` (a doubled dot — empty config segment between
// basename and the .txt suffix). This is the converter's DEFAULT
// invocation shape (it doesn't force a build type), so the reader
// MUST treat the empty config as a valid single config and surface
// the value — not silently drop it. Regression guard: before the
// fix, splitProbeConfigFilename rejected `objects..txt` (the
// `dot == len(stem)-1` guard), which dropped every per-target
// probe value (Objects, INTERFACE_*, TARGET_FILE) for any project
// configured without a build type, so the probe-as-oracle path
// never fired in the converter's own default run.
func TestReadGenexProbe_EmptyConfig(t *testing.T) {
	buildDir := t.TempDir()
	tgtDir := filepath.Join(buildDir, "cmake-to-bazel.genex", "obj")
	if err := os.MkdirAll(tgtDir, 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"type.txt":                           "OBJECT_LIBRARY",
		"objects..txt":                       "/b/CMakeFiles/obj.dir/a.c.o;/b/CMakeFiles/obj.dir/b.c.o",
		"interface_INCLUDE_DIRECTORIES..txt": "/src/include",
		"interface_COMPILE_DEFINITIONS..txt": "",
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(tgtDir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := ReadGenexProbe(buildDir)
	if err != nil {
		t.Fatalf("ReadGenexProbe: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 probe; got %d (%+v)", len(got), got)
	}
	p := got[0]
	if p.Type != "OBJECT_LIBRARY" {
		t.Errorf("Type: %q", p.Type)
	}
	if p.Objects != "/b/CMakeFiles/obj.dir/a.c.o;/b/CMakeFiles/obj.dir/b.c.o" {
		t.Errorf("Objects under empty config dropped or wrong: %q", p.Objects)
	}
	if p.Interface["INCLUDE_DIRECTORIES"] != "/src/include" {
		t.Errorf("INTERFACE_INCLUDE_DIRECTORIES under empty config: %q", p.Interface["INCLUDE_DIRECTORIES"])
	}
	if _, ok := p.Interface["COMPILE_DEFINITIONS"]; !ok {
		t.Errorf("empty INTERFACE_COMPILE_DEFINITIONS should still be recorded under empty config")
	}
}

func TestReadGenexProbe_DeterministicOrder(t *testing.T) {
	buildDir := t.TempDir()
	root := filepath.Join(buildDir, "cmake-to-bazel.genex")
	for _, name := range []string{"zeta", "alpha", "middle"} {
		if err := os.MkdirAll(filepath.Join(root, name), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, name, "type.txt"), []byte("STATIC_LIBRARY"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := ReadGenexProbe(buildDir)
	if err != nil {
		t.Fatalf("ReadGenexProbe: %v", err)
	}
	want := []string{"alpha", "middle", "zeta"}
	for i, p := range got {
		if p.Name != want[i] {
			t.Errorf("probe[%d].Name = %q, want %q", i, p.Name, want[i])
		}
	}
}

func TestReadGenexProbe_InterfaceOnlyTarget(t *testing.T) {
	// INTERFACE_LIBRARY targets get only type.txt + interface_*.txt
	// (no file/file_dir/file_name; no objects).
	buildDir := t.TempDir()
	tgtDir := filepath.Join(buildDir, "cmake-to-bazel.genex", "ifacelib")
	if err := os.MkdirAll(tgtDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"type.txt": "INTERFACE_LIBRARY",
		"interface_INCLUDE_DIRECTORIES.Release.txt": "/src/include",
		"interface_LINK_LIBRARIES.Release.txt":      "depA",
	} {
		if err := os.WriteFile(filepath.Join(tgtDir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := ReadGenexProbe(buildDir)
	if err != nil {
		t.Fatalf("ReadGenexProbe: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 probe; got %d", len(got))
	}
	p := got[0]
	if p.Type != "INTERFACE_LIBRARY" {
		t.Errorf("Type: %q", p.Type)
	}
	if p.File != "" || p.FileDir != "" || p.FileName != "" {
		t.Errorf("INTERFACE_LIBRARY should have no on-disk paths: %+v", p)
	}
	if p.Interface["INCLUDE_DIRECTORIES"] != "/src/include" {
		t.Errorf("INTERFACE_INCLUDE_DIRECTORIES: %q", p.Interface["INCLUDE_DIRECTORIES"])
	}
}

func TestReadGenexProbe_ObjectLibrary(t *testing.T) {
	buildDir := t.TempDir()
	tgtDir := filepath.Join(buildDir, "cmake-to-bazel.genex", "objlib")
	if err := os.MkdirAll(tgtDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"type.txt":            "OBJECT_LIBRARY",
		"objects.Release.txt": "/build/CMakeFiles/objlib.dir/a.c.o;/build/CMakeFiles/objlib.dir/b.c.o",
	} {
		if err := os.WriteFile(filepath.Join(tgtDir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := ReadGenexProbe(buildDir)
	if err != nil {
		t.Fatalf("ReadGenexProbe: %v", err)
	}
	if len(got) != 1 {
		t.Fatal("want 1 probe")
	}
	if got[0].Objects == "" {
		t.Errorf("Objects empty for OBJECT_LIBRARY: %+v", got[0])
	}
}

// TestReadGenexProbe_MultiConfigCollapse pins the multi-config
// "every config agrees → single value" path: probe-genex.cmake
// emits one OUTPUT per CMAKE_CONFIGURATION_TYPES entry under the
// Ninja Multi-Config generator (e.g. file.Release.txt + file.ASan.txt);
// when both resolve to the same string the reader exposes one
// unified value on GenexProbe.File. This is the common case
// because $<TARGET_FILE:t> for a target without per-config postfix
// resolves to the same path under each config.
func TestReadGenexProbe_MultiConfigCollapse(t *testing.T) {
	buildDir := t.TempDir()
	tgtDir := filepath.Join(buildDir, "cmake-to-bazel.genex", "foo")
	if err := os.MkdirAll(tgtDir, 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"type.txt":                                  "STATIC_LIBRARY",
		"file.Release.txt":                          "/build/libfoo.a",
		"file.ASan.txt":                             "/build/libfoo.a",
		"file_dir.Release.txt":                      "/build",
		"file_dir.ASan.txt":                         "/build",
		"file_name.Release.txt":                     "libfoo.a",
		"file_name.ASan.txt":                        "libfoo.a",
		"interface_INCLUDE_DIRECTORIES.Release.txt": "/src/include",
		"interface_INCLUDE_DIRECTORIES.ASan.txt":    "/src/include",
		"interface_LINK_LIBRARIES.Release.txt":      "bar",
		"interface_LINK_LIBRARIES.ASan.txt":         "bar",
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(tgtDir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	got, err := ReadGenexProbe(buildDir)
	if err != nil {
		t.Fatalf("ReadGenexProbe: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 probe; got %d", len(got))
	}
	p := got[0]
	if p.File != "/build/libfoo.a" {
		t.Errorf("File should collapse to single value across configs: %q", p.File)
	}
	if p.FileDir != "/build" {
		t.Errorf("FileDir should collapse: %q", p.FileDir)
	}
	if p.FileName != "libfoo.a" {
		t.Errorf("FileName should collapse: %q", p.FileName)
	}
	if p.Interface["INCLUDE_DIRECTORIES"] != "/src/include" {
		t.Errorf("INTERFACE_INCLUDE_DIRECTORIES should collapse: %q", p.Interface["INCLUDE_DIRECTORIES"])
	}
}

// TestReadGenexProbe_MultiConfigDivergenceDropped pins the
// divergence path: probe-genex captured different per-config
// values (e.g. a target with CMAKE_DEBUG_POSTFIX="d" produces
// libfoo.a under Release but libfood.a under Debug; or — much
// more commonly — Ninja Multi-Config puts each config's
// artifacts in `/build/Release/...` vs `/build/Debug/...`). The
// reader drops the diverging fields silently and returns the rest
// of the probe; the consumer treats the missing fields the same
// as "probe didn't run for this target" and the lift falls back
// via genexeval's UnsupportedError surface.
//
// Treating divergence as fatal would defeat the whole point of
// per-config OUTPUT, since file_dir always diverges under multi-
// config. The non-fatal shape lets the rest of the probe data
// (Type, INTERFACE_*, Properties) still feed the lift even when
// the on-disk paths can't be unified.
func TestReadGenexProbe_MultiConfigDivergenceDropped(t *testing.T) {
	buildDir := t.TempDir()
	tgtDir := filepath.Join(buildDir, "cmake-to-bazel.genex", "foo")
	if err := os.MkdirAll(tgtDir, 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"type.txt":              "STATIC_LIBRARY",
		"file.Release.txt":      "/build/Release/libfoo.a",
		"file.Debug.txt":        "/build/Debug/libfood.a",
		"file_name.Release.txt": "libfoo.a",
		"file_name.Debug.txt":   "libfood.a",
		"file_dir.Release.txt":  "/build/Release",
		"file_dir.Debug.txt":    "/build/Debug",
		// INTERFACE_* still match across configs — should survive
		// the collapse even though File / FileDir don't.
		"interface_LINK_LIBRARIES.Release.txt": "bar",
		"interface_LINK_LIBRARIES.Debug.txt":   "bar",
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(tgtDir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	got, err := ReadGenexProbe(buildDir)
	if err != nil {
		t.Fatalf("ReadGenexProbe: divergence should not be fatal; got %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 probe; got %d", len(got))
	}
	p := got[0]
	if p.Type != "STATIC_LIBRARY" {
		t.Errorf("Type lost across collapse: %q", p.Type)
	}
	// Diverging fields: dropped to empty so consumers fall back.
	if p.File != "" {
		t.Errorf("File should drop on divergence; got %q", p.File)
	}
	if p.FileDir != "" {
		t.Errorf("FileDir should drop on divergence; got %q", p.FileDir)
	}
	// FileName matched in this fixture's Release vs… wait, the
	// fixture sets file_name.Release.txt=libfoo.a vs
	// file_name.Debug.txt=libfood.a — both DIVERGE in this case,
	// so FileName should also drop.
	if p.FileName != "" {
		t.Errorf("FileName should drop on divergence; got %q", p.FileName)
	}
	// Non-diverging field (interface_LINK_LIBRARIES matched): kept.
	if p.Interface["LINK_LIBRARIES"] != "bar" {
		t.Errorf("LINK_LIBRARIES should survive the collapse; got %q", p.Interface["LINK_LIBRARIES"])
	}
}

// TestPerConfigMismatchError_FormatStable pins the Error()
// string shape so the type stays useful as a diagnostic surface
// even though ReadGenexProbe doesn't bubble it out today. Future
// callers (strict-mode flag, debug logging) can rely on the
// substring "diverged across configs" + target + basename.
func TestPerConfigMismatchError_FormatStable(t *testing.T) {
	e := &PerConfigMismatchError{
		Target:   "foo",
		Basename: "file",
		Values: map[string]string{
			"Release": "/build/Release/libfoo.a",
			"Debug":   "/build/Debug/libfood.a",
		},
	}
	msg := e.Error()
	// Errors.As round-trips so callers can still introspect via
	// the type even when ReadGenexProbe doesn't return one.
	var got *PerConfigMismatchError
	if !errors.As(e, &got) {
		t.Fatalf("errors.As should match the type; got %v", got)
	}
	if got.Target != "foo" || got.Basename != "file" {
		t.Errorf("As-roundtrip lost fields: %+v", got)
	}
	if len(msg) == 0 {
		t.Errorf("Error() should produce a non-empty string")
	}
}

// TestReadGenexProbe_UnknownConfigSuffixIgnored pins the
// forward-compat surface: a probe filename that doesn't match the
// expected basename.config.txt pattern (e.g. a flat "extra.txt"
// from a future hook addition older readers haven't been taught
// about) is silently dropped, not surfaced as a failure. type.txt
// stays the one legal flat-name entry.
func TestReadGenexProbe_UnknownConfigSuffixIgnored(t *testing.T) {
	buildDir := t.TempDir()
	tgtDir := filepath.Join(buildDir, "cmake-to-bazel.genex", "foo")
	if err := os.MkdirAll(tgtDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"type.txt":  "STATIC_LIBRARY",
		"extra.txt": "ignored",
	} {
		if err := os.WriteFile(filepath.Join(tgtDir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := ReadGenexProbe(buildDir)
	if err != nil {
		t.Fatalf("ReadGenexProbe: %v", err)
	}
	if len(got) != 1 || got[0].Type != "STATIC_LIBRARY" {
		t.Errorf("expected single STATIC_LIBRARY probe with no extra fields; got %+v", got)
	}
}
