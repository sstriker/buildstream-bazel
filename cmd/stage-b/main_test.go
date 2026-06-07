package main

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// mkProject lays out a minimal project-A bazel-bin tree and a
// project-B elements/ tree under a fresh temp dir. aOuts maps an
// element name to its converted BUILD.bazel.out content (omit an
// element to model a non-action-graph kind with no project-A
// output). bBuilds maps an element name to the BUILD.bazel content
// already staged in project B (omit to model a project-B element
// dir that exists but has no BUILD.bazel yet).
func mkProject(t *testing.T, aOuts, bBuilds map[string]string) (projectA, projectB string) {
	t.Helper()
	root := t.TempDir()
	projectA = filepath.Join(root, "A")
	projectB = filepath.Join(root, "B")

	for name, content := range aOuts {
		dir := filepath.Join(projectA, "bazel-bin", "elements", name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "BUILD.bazel.out"), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// Every element dir B knows about — union of A's outputs and B's
	// pre-staged builds — gets a directory under B/elements/.
	all := map[string]bool{}
	for name := range aOuts {
		all[name] = true
	}
	for name := range bBuilds {
		all[name] = true
	}
	for name := range all {
		dir := filepath.Join(projectB, "elements", name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if content, ok := bBuilds[name]; ok {
			if err := os.WriteFile(filepath.Join(dir, "BUILD.bazel"), []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	// Ensure bazel-bin/elements exists even when A has no outputs,
	// so the "A was built" precondition holds for those cases.
	if len(aOuts) == 0 {
		if err := os.MkdirAll(filepath.Join(projectA, "bazel-bin", "elements"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return projectA, projectB
}

// TestRun_StagesConversionTodosSidecar verifies the single-package path
// lands conversion-todos.json (the agent-prompts sidecar the converter
// emits next to BUILD.bazel.out) into project B's element dir, and that
// it isn't counted as a "changed" package (it's not a BUILD file).
func TestRun_StagesConversionTodosSidecar(t *testing.T) {
	a, b := mkProject(t, map[string]string{"hello": "cc_library(name=\"hello\")\n"}, nil)
	todos := `{"version":1,"todos":[{"id":"todo-abc","kind":"cmake-p-test"}]}` + "\n"
	if err := os.WriteFile(filepath.Join(a, "bazel-bin", "elements", "hello", "conversion-todos.json"), []byte(todos), 0o644); err != nil {
		t.Fatal(err)
	}
	changed, err := run(args{projectA: a, projectB: b})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !reflect.DeepEqual(changed, []string{"elements/hello"}) {
		t.Errorf("changed = %v; want [elements/hello] (BUILD changed; sidecar must not add extra entries)", changed)
	}
	got, err := os.ReadFile(filepath.Join(b, "elements", "hello", "conversion-todos.json"))
	if err != nil {
		t.Fatalf("sidecar not staged: %v", err)
	}
	if string(got) != todos {
		t.Errorf("staged sidecar = %q; want %q", got, todos)
	}

	// Idempotent: a re-run with no change reports nothing and leaves the
	// sidecar intact.
	changed, err = run(args{projectA: a, projectB: b})
	if err != nil {
		t.Fatalf("re-run: %v", err)
	}
	if len(changed) != 0 {
		t.Errorf("re-stage reported changes %v; want none", changed)
	}
}

// TestRun_NoSidecarWhenAbsent confirms a missing conversion-todos.json
// (e.g. --conversion-todos=false) is not an error and stages nothing.
func TestRun_NoSidecarWhenAbsent(t *testing.T) {
	a, b := mkProject(t, map[string]string{"hello": "cc_library(name=\"hello\")\n"}, nil)
	if _, err := run(args{projectA: a, projectB: b}); err != nil {
		t.Fatalf("run: %v", err)
	}
	if _, err := os.Stat(filepath.Join(b, "elements", "hello", "conversion-todos.json")); !os.IsNotExist(err) {
		t.Errorf("expected no sidecar staged; stat err = %v", err)
	}
}

func TestRun_FirstStage_AllChanged(t *testing.T) {
	a, b := mkProject(t,
		map[string]string{"alpha": "cc_library(name = \"alpha\")\n", "beta": "cc_library(name = \"beta\")\n"},
		map[string]string{"alpha": "BUILD_NOT_YET_STAGED\n", "beta": "BUILD_NOT_YET_STAGED\n"},
	)
	changed, err := run(args{projectA: a, projectB: b})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	want := []string{"elements/alpha", "elements/beta"}
	if !reflect.DeepEqual(changed, want) {
		t.Errorf("changed = %v, want %v", changed, want)
	}
	// The placeholder must have been replaced with the converted output.
	got, _ := os.ReadFile(filepath.Join(b, "elements", "alpha", "BUILD.bazel"))
	if string(got) != "cc_library(name = \"alpha\")\n" {
		t.Errorf("alpha BUILD.bazel not staged: %q", got)
	}
}

func TestRun_Idempotent_NoChangeOnRestage(t *testing.T) {
	a, b := mkProject(t,
		map[string]string{"alpha": "cc_library(name = \"alpha\")\n"},
		map[string]string{"alpha": "BUILD_NOT_YET_STAGED\n"},
	)
	if _, err := run(args{projectA: a, projectB: b}); err != nil {
		t.Fatalf("first run: %v", err)
	}
	// A second stage with no re-conversion in between must report
	// nothing changed — the content-diff signal, not a blind copy.
	changed, err := run(args{projectA: a, projectB: b})
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if len(changed) != 0 {
		t.Errorf("re-stage reported changes %v, want none", changed)
	}
}

func TestRun_OnlyChangedElementReported(t *testing.T) {
	a, b := mkProject(t,
		map[string]string{"alpha": "v1\n", "beta": "beta\n"},
		map[string]string{"alpha": "v1\n", "beta": "beta\n"},
	)
	// Re-convert just alpha (its project-A output changes).
	if err := os.WriteFile(filepath.Join(a, "bazel-bin", "elements", "alpha", "BUILD.bazel.out"), []byte("v2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	changed, err := run(args{projectA: a, projectB: b})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	want := []string{"elements/alpha"}
	if !reflect.DeepEqual(changed, want) {
		t.Errorf("changed = %v, want %v", changed, want)
	}
	if got, _ := os.ReadFile(filepath.Join(b, "elements", "beta", "BUILD.bazel")); string(got) != "beta\n" {
		t.Errorf("beta BUILD.bazel should be untouched, got %q", got)
	}
}

func TestRun_NonActionGraphKindSkipped(t *testing.T) {
	// "stacky" has a project-B element dir + a write-a-rendered
	// BUILD.bazel but no project-A converted output — a kind:stack
	// shape. stage-b must leave it alone and never report it.
	a, b := mkProject(t,
		map[string]string{"alpha": "alpha\n"},
		map[string]string{"alpha": "BUILD_NOT_YET_STAGED\n", "stacky": "filegroup(name = \"stacky\")\n"},
	)
	changed, err := run(args{projectA: a, projectB: b})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	want := []string{"elements/alpha"}
	if !reflect.DeepEqual(changed, want) {
		t.Errorf("changed = %v, want %v", changed, want)
	}
	if got, _ := os.ReadFile(filepath.Join(b, "elements", "stacky", "BUILD.bazel")); string(got) != "filegroup(name = \"stacky\")\n" {
		t.Errorf("non-action-graph element BUILD.bazel must be untouched, got %q", got)
	}
}

func TestRun_ProjectANotBuilt_Errors(t *testing.T) {
	root := t.TempDir()
	projectA := filepath.Join(root, "A") // no bazel-bin/ at all
	projectB := filepath.Join(root, "B")
	if err := os.MkdirAll(filepath.Join(projectB, "elements"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := run(args{projectA: projectA, projectB: projectB}); err == nil {
		t.Fatal("expected an error when project A has no bazel-bin/elements, got nil")
	}
}

func TestParseArgs_RequiresBothRoots(t *testing.T) {
	if _, code := parseArgs([]string{"--project-a", "/tmp/a"}, os.Stderr); code != exitUsage {
		t.Errorf("missing --project-b: code = %d, want %d", code, exitUsage)
	}
	if _, code := parseArgs([]string{"--project-b", "/tmp/b"}, os.Stderr); code != exitUsage {
		t.Errorf("missing --project-a: code = %d, want %d", code, exitUsage)
	}
	a, code := parseArgs([]string{"--project-a", "a", "--project-b", "b"}, os.Stderr)
	if code != exitSuccess {
		t.Fatalf("valid args: code = %d, want %d", code, exitSuccess)
	}
	if !filepath.IsAbs(a.projectA) || !filepath.IsAbs(a.projectB) {
		t.Errorf("parseArgs must absolutize roots: %+v", a)
	}
}
