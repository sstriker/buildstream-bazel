package fileapi_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/internal/fileapi"
)

// TestEventFindPackageFound_StringShape exercises the cmake 4.3
// find-v1 success shape: `found:` is a scalar string holding the
// resolved absolute path. The polymorphic UnmarshalYAML lifts the
// string into Path and flips IsFound so downstream consumers that
// gate on IsFound continue to see "this was found" without
// branching on event kind.
func TestEventFindPackageFound_StringShape(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "CMakeConfigureLog.yaml")
	body := `events:
  -
    kind: "find-v1"
    variable: "CMAKE_C_COMPILER"
    found: "/usr/bin/cc"
`
	if err := os.WriteFile(logPath, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	events, err := fileapi.LoadConfigureLogYAML(logPath)
	if err != nil {
		t.Fatalf("LoadConfigureLogYAML: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("want 1 event, got %d", len(events))
	}
	f := events[0].Found
	if f == nil {
		t.Fatalf("Found should be populated for string shape")
	}
	if got, want := f.Path, "/usr/bin/cc"; got != want {
		t.Errorf("Path: got %q want %q", got, want)
	}
	if !f.IsFound {
		t.Errorf("IsFound: want true for non-empty string shape")
	}
}

// TestEventFindPackageFound_FalseBoolShape exercises the cmake 4.3
// find-v1 failure shape: `found: false`. The unmarshaler should
// surface this as IsFound=false with an empty Path — equivalent
// semantically to the legacy mapping shape's `isFound: false`.
func TestEventFindPackageFound_FalseBoolShape(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "CMakeConfigureLog.yaml")
	body := `events:
  -
    kind: "find-v1"
    variable: "CMAKE_DLLTOOL"
    found: false
`
	if err := os.WriteFile(logPath, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	events, err := fileapi.LoadConfigureLogYAML(logPath)
	if err != nil {
		t.Fatalf("LoadConfigureLogYAML: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("want 1 event, got %d", len(events))
	}
	f := events[0].Found
	if f == nil {
		t.Fatalf("Found should be populated for bool shape")
	}
	if f.IsFound {
		t.Errorf("IsFound: want false for `found: false`")
	}
	if f.Path != "" {
		t.Errorf("Path: want \"\" got %q", f.Path)
	}
}

// TestEventFindPackageFound_TrueBoolShape covers the
// forward-compatibility carve-out: no cmake version emits
// `found: true` today, but if a future cmake adopts it, we accept
// the value as "located but path unknown" rather than failing the
// whole configureLog parse.
func TestEventFindPackageFound_TrueBoolShape(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "CMakeConfigureLog.yaml")
	body := `events:
  -
    kind: "find-v1"
    variable: "EXAMPLE"
    found: true
`
	if err := os.WriteFile(logPath, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	events, err := fileapi.LoadConfigureLogYAML(logPath)
	if err != nil {
		t.Fatalf("LoadConfigureLogYAML: %v", err)
	}
	f := events[0].Found
	if f == nil || !f.IsFound {
		t.Errorf("IsFound: want true for `found: true` (forward-compat)")
	}
}

// TestEventFindPackageFound_NullShape exercises the cmake 4.3
// find_package-v1 unfound shape: `found: null` (rather than the
// older `isFound: false` mapping). yaml.v3 leaves the parent
// pointer field nil on `null` rather than invoking UnmarshalYAML,
// so callers see `Found == nil` — which carries the same
// "not found" semantic as `IsFound == false` (the existing
// downstream code in lower.findPackageHeaderComments + the
// find_package_attrib builder already guards on
// `e.Found == nil || !e.Found.IsFound`). The contract the parse
// must uphold is "doesn't fail with a yaml unmarshal error".
func TestEventFindPackageFound_NullShape(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "CMakeConfigureLog.yaml")
	body := `events:
  -
    kind: "find_package-v1"
    name: "NonExistentPackage"
    found: null
`
	if err := os.WriteFile(logPath, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	events, err := fileapi.LoadConfigureLogYAML(logPath)
	if err != nil {
		t.Fatalf("LoadConfigureLogYAML: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("want 1 event, got %d", len(events))
	}
	// yaml.v3 normalises `found: null` to a nil pointer rather
	// than calling UnmarshalYAML. That's fine — downstream
	// callers treat nil and IsFound==false identically.
	if f := events[0].Found; f != nil && f.IsFound {
		t.Errorf("event[0].Found: want nil or IsFound=false, got %+v", f)
	}
}

// TestEventFindPackageFound_LegacyStructShape covers the cmake
// 3.26-4.2 find_package-v1 mapping shape that the typed
// EventFindPackageFound fields originally targeted. This is the
// shape the existing repo tests + fixtures use; it must keep
// decoding identically after the polymorphic UnmarshalYAML lands.
func TestEventFindPackageFound_LegacyStructShape(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "CMakeConfigureLog.yaml")
	body := `events:
  -
    kind: "find_package-v1"
    found:
      isFound: true
      package: "Boost"
      version: "1.83.0"
      configFile: "/usr/lib/cmake/Boost/BoostConfig.cmake"
      versionFile: "/usr/lib/cmake/Boost/BoostConfigVersion.cmake"
  -
    kind: "find_package-v1"
    found:
      isFound: false
`
	if err := os.WriteFile(logPath, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	events, err := fileapi.LoadConfigureLogYAML(logPath)
	if err != nil {
		t.Fatalf("LoadConfigureLogYAML: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("want 2 events, got %d", len(events))
	}
	f := events[0].Found
	if f == nil {
		t.Fatalf("event[0].Found should be populated")
	}
	if !f.IsFound {
		t.Errorf("event[0].IsFound: want true")
	}
	if f.Package != "Boost" {
		t.Errorf("event[0].Package: got %q want Boost", f.Package)
	}
	if f.Version != "1.83.0" {
		t.Errorf("event[0].Version: got %q want 1.83.0", f.Version)
	}
	if f.ConfigFile == "" || f.VersionFile == "" {
		t.Errorf("event[0] config/version files: ConfigFile=%q VersionFile=%q",
			f.ConfigFile, f.VersionFile)
	}
	// Path is the new field — empty on mapping-shape decodes.
	if f.Path != "" {
		t.Errorf("event[0].Path: want \"\" on mapping shape, got %q", f.Path)
	}

	f2 := events[1].Found
	if f2 == nil || f2.IsFound {
		t.Errorf("event[1].Found: want IsFound=false, got %+v", f2)
	}
}

// TestParseConfigureLog_CMake4_3_Fixture feeds the captured cmake
// 4.3.3 configureLog fixture (testdata/configurelog/cmake-4.3-find-package.yaml)
// through the full parser. Before the polymorphic decode landed,
// every cmake 4.3 configureLog parse failed with "cannot unmarshal
// !!str into fileapi.EventFindPackageFound" — this test is the
// regression guard.
func TestParseConfigureLog_CMake4_3_Fixture(t *testing.T) {
	events, err := fileapi.LoadConfigureLogYAML(
		filepath.Join("testdata", "configurelog", "cmake-4.3-find-package.yaml"))
	if err != nil {
		t.Fatalf("LoadConfigureLogYAML: %v", err)
	}
	if len(events) == 0 {
		t.Fatal("fixture should produce events; got 0")
	}

	// Count the shapes we expect the fixture to cover. Bumping the
	// fixture should bump this matrix.
	//
	// Notes on the surface mapping:
	//   - `found: "/path"` → Found populated, Path non-empty,
	//     IsFound=true.
	//   - `found: false`  → Found populated, IsFound=false, Path="".
	//   - `found: null`   → Found is nil (yaml.v3 leaves the
	//     pointer field nil on explicit null; equivalent to
	//     "absent" downstream).
	var (
		stringShape int
		falseShape  int
		nilFound    int
	)
	for _, e := range events {
		if e.Found == nil {
			// Either no `found:` key on the event (legitimate
			// for some event kinds) or `found: null`.
			nilFound++
			continue
		}
		switch {
		case e.Found.Path != "":
			stringShape++
		case e.Found.IsFound:
			// Forward-compat `found: true` — fixture doesn't
			// exercise this path.
		default:
			falseShape++
		}
	}
	if stringShape < 2 {
		t.Errorf("fixture string-shape events: got %d, want >= 2", stringShape)
	}
	if falseShape < 1 {
		t.Errorf("fixture false-shape events: got %d, want >= 1", falseShape)
	}
	// Fixture's `found: null` find_package-v1 event + the
	// message-v1 event with no `found:` key together should
	// produce >= 2 nil entries.
	if nilFound < 2 {
		t.Errorf("fixture nil/absent-found events: got %d, want >= 2", nilFound)
	}
}
