package fileapi_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/internal/fileapi"
)

func TestLoad_ConfigureLogSidecar(t *testing.T) {
	dir := t.TempDir()
	writeMinimalReplyWithConfigureLog(t, dir)
	r, err := fileapi.Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if r.ConfigureLog == nil {
		t.Fatalf("ConfigureLog not populated")
	}
	if got, want := r.ConfigureLog.Path, "/build/CMakeFiles/CMakeConfigureLog.yaml"; got != want {
		t.Errorf("Path: got %q want %q", got, want)
	}
	wantKinds := []string{"find_package-v1", "try_compile-v1", "message-v1"}
	if got := r.ConfigureLog.EventKindNames; !slicesEqual(got, wantKinds) {
		t.Errorf("EventKindNames: got %v want %v", got, wantKinds)
	}
}

// TestLoad_ConfigureLogAbsent covers cmake < 3.26 or projects whose
// configure fired no configureLog-aware events: Load should succeed
// with ConfigureLog == nil.
func TestLoad_ConfigureLogAbsent(t *testing.T) {
	dir := t.TempDir()
	writeMinimalReply(t, dir)
	r, err := fileapi.Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if r.ConfigureLog != nil {
		t.Fatalf("ConfigureLog should be nil when sidecar absent; got %+v", r.ConfigureLog)
	}
}

func TestLoadConfigureLogYAML_EmptyPath(t *testing.T) {
	events, err := fileapi.LoadConfigureLogYAML("")
	if err != nil {
		t.Fatalf("LoadConfigureLogYAML(\"\"): %v", err)
	}
	if events != nil {
		t.Errorf("events should be nil for empty path; got %v", events)
	}
}

func TestLoadConfigureLogYAML_MissingFile(t *testing.T) {
	events, err := fileapi.LoadConfigureLogYAML(filepath.Join(t.TempDir(), "no-such.yaml"))
	if err != nil {
		t.Fatalf("LoadConfigureLogYAML(missing): %v", err)
	}
	if events != nil {
		t.Errorf("events should be nil for missing file; got %v", events)
	}
}

func TestLoadConfigureLogYAML_TryCompile(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "CMakeConfigureLog.yaml")
	body := `events:
  -
    kind: "try_compile-v1"
    backtrace:
      - "/src/CMakeLists.txt:12 (try_compile)"
    checks:
      - "Detecting C compiler ABI info"
    description: "Detecting C compiler ABI info"
    directories:
      source: "/src"
      binary: "/build/CMakeFiles/CMakeScratch/TryCompile-AbCdEf"
    cmakeVariables:
      CMAKE_C_FLAGS: ""
      CMAKE_C_STANDARD_REQUIRED: ""
    buildResult:
      variable: "CMAKE_C_ABI_COMPILED"
      cached: false
      stdout: "ninja: build stopped\n"
      exitCode: 0
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
	e := events[0]
	if e.Kind != "try_compile-v1" {
		t.Errorf("Kind: got %q want try_compile-v1", e.Kind)
	}
	if len(e.Backtrace) != 1 || e.Backtrace[0] != "/src/CMakeLists.txt:12 (try_compile)" {
		t.Errorf("Backtrace: %v", e.Backtrace)
	}
	if len(e.Checks) != 1 || e.Checks[0] != "Detecting C compiler ABI info" {
		t.Errorf("Checks: %v", e.Checks)
	}
	if e.Directories == nil || e.Directories.Source != "/src" {
		t.Errorf("Directories.Source: %+v", e.Directories)
	}
	if e.CMakeVariables["CMAKE_C_FLAGS"] != "" {
		t.Errorf("CMakeVariables[CMAKE_C_FLAGS]: %q", e.CMakeVariables["CMAKE_C_FLAGS"])
	}
	if e.BuildResult == nil {
		t.Fatalf("BuildResult missing")
	}
	if got, want := e.BuildResult.Variable, "CMAKE_C_ABI_COMPILED"; got != want {
		t.Errorf("BuildResult.Variable: %q want %q", got, want)
	}
	if e.BuildResult.Cached {
		t.Errorf("BuildResult.Cached: got true, want false")
	}
}

func TestLoadConfigureLogYAML_FindPackageFound(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "CMakeConfigureLog.yaml")
	body := `events:
  -
    kind: "find_package-v1"
    backtrace:
      - "/src/CMakeLists.txt:7 (find_package)"
    components:
      - "core"
      - "json"
    found:
      isFound: true
      package: "Boost"
      version: "1.83.0"
      configFile: "/usr/lib/x86_64-linux-gnu/cmake/Boost/BoostConfig.cmake"
      versionFile: "/usr/lib/x86_64-linux-gnu/cmake/Boost/BoostConfigVersion.cmake"
  -
    kind: "find_package-v1"
    backtrace:
      - "/src/CMakeLists.txt:9 (find_package)"
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
	if events[0].Found == nil || !events[0].Found.IsFound || events[0].Found.Package != "Boost" {
		t.Errorf("event[0].Found: %+v", events[0].Found)
	}
	if events[0].Components[0] != "core" || events[0].Components[1] != "json" {
		t.Errorf("event[0].Components: %v", events[0].Components)
	}
	if events[1].Found == nil || events[1].Found.IsFound {
		t.Errorf("event[1].Found: %+v", events[1].Found)
	}
}

func TestLoadConfigureLogYAML_Message(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "CMakeConfigureLog.yaml")
	body := `events:
  -
    kind: "message-v1"
    backtrace:
      - "/src/CMakeLists.txt:4 (message)"
    mode: "STATUS"
    message: "Configuring done"
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
	if got, want := events[0].Mode, "STATUS"; got != want {
		t.Errorf("Mode: %q want %q", got, want)
	}
	if got, want := events[0].Message, "Configuring done"; got != want {
		t.Errorf("Message: %q want %q", got, want)
	}
}

// writeMinimalReplyWithConfigureLog stages a 5-kind reply (the
// four mandatory kinds plus configureLog-v1).
func writeMinimalReplyWithConfigureLog(t *testing.T, dir string) {
	t.Helper()
	writeFile(t, dir, "codemodel-v2.json", `{"kind":"codemodel","version":{"major":2,"minor":0},"paths":{"source":"/src","build":"/b"},"configurations":[]}`)
	writeFile(t, dir, "cache-v2.json", `{"kind":"cache","version":{"major":2,"minor":0},"entries":[]}`)
	writeFile(t, dir, "toolchains-v1.json", `{"kind":"toolchains","version":{"major":1,"minor":0},"toolchains":[]}`)
	writeFile(t, dir, "cmakeFiles-v1.json", `{"kind":"cmakeFiles","version":{"major":1,"minor":0},"paths":{"source":"/src","build":"/b"},"inputs":[]}`)
	writeFile(t, dir, "configureLog-v1.json", `{"kind":"configureLog","version":{"major":1,"minor":0},"path":"/build/CMakeFiles/CMakeConfigureLog.yaml","eventKindNames":["find_package-v1","try_compile-v1","message-v1"]}`)
	writeFile(t, dir, "index-2026.json", `{
		"cmake": { "generator": { "name": "Ninja", "multiConfig": false }, "paths": { "cmake": "/usr/bin/cmake", "ctest": "/usr/bin/ctest", "cpack": "/usr/bin/cpack", "root": "/usr" }, "version": { "major": 3, "minor": 28, "patch": 3, "string": "3.28.3", "suffix": "", "isDirty": false } },
		"objects" : [
			{ "kind" : "codemodel", "version": { "major": 2, "minor": 0 }, "jsonFile": "codemodel-v2.json" },
			{ "kind" : "cache", "version": { "major": 2, "minor": 0 }, "jsonFile": "cache-v2.json" },
			{ "kind" : "toolchains", "version": { "major": 1, "minor": 0 }, "jsonFile": "toolchains-v1.json" },
			{ "kind" : "cmakeFiles", "version": { "major": 1, "minor": 0 }, "jsonFile": "cmakeFiles-v1.json" },
			{ "kind" : "configureLog", "version": { "major": 1, "minor": 0 }, "jsonFile": "configureLog-v1.json" }
		]
	}`)
}

func slicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
