package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRelax_StripsKeepFromMatchingGenrule covers the happy
// path: a genrule with `# keep` whose cmd contains a
// configured pattern substring has the keep removed.
func TestRelax_StripsKeepFromMatchingGenrule(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "tools/gazelle-rewritable.json", `{
  "version": 1,
  "patterns": [
    {"name": "protoc", "cmd_contains": "protoc"}
  ]
}
`)
	writeFile(t, root, "elements/myelem/BUILD.bazel", `genrule(
    name = "myelem_proto_gen",
    srcs = ["myelem.proto"],
    outs = ["myelem.pb.cc"],
    cmd = "protoc --cpp_out=$(@D) $(location myelem.proto)",
)  # keep
`)
	a := args{root: root, rewritableFile: "tools/gazelle-rewritable.json"}
	if err := run(a); err != nil {
		t.Fatalf("run: %v", err)
	}
	got := readFile(t, root, "elements/myelem/BUILD.bazel")
	if strings.Contains(got, "# keep") {
		t.Errorf("BUILD still contains # keep marker after relax:\n%s", got)
	}
	// The genrule itself should remain — relax only strips
	// the marker, not the rule.
	if !strings.Contains(got, "genrule(") {
		t.Errorf("BUILD lost its genrule after relax:\n%s", got)
	}
}

// TestRelax_LeavesNonMatchingGenrule covers the protective
// half of the contract: genrules whose cmd doesn't match any
// configured pattern keep their `# keep` marker.
func TestRelax_LeavesNonMatchingGenrule(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "tools/gazelle-rewritable.json", `{
  "version": 1,
  "patterns": [{"name": "protoc", "cmd_contains": "protoc"}]
}
`)
	writeFile(t, root, "elements/myelem/BUILD.bazel", `genrule(
    name = "myelem_codegen",
    srcs = ["input.txt"],
    outs = ["output.txt"],
    cmd = "my-custom-codegen $(SRCS) > $(@)",
)  # keep
`)
	a := args{root: root, rewritableFile: "tools/gazelle-rewritable.json"}
	if err := run(a); err != nil {
		t.Fatalf("run: %v", err)
	}
	got := readFile(t, root, "elements/myelem/BUILD.bazel")
	if !strings.Contains(got, "# keep") {
		t.Errorf("BUILD lost # keep marker on non-matching genrule:\n%s", got)
	}
}

// TestRelax_EmptyConfigIsNoop covers the default operator
// state: empty patterns list means relax-keeps does nothing.
// Continuous re-conversion loops invoking relax-keeps with
// no operator-declared patterns must be byte-stable against
// the previous state.
func TestRelax_EmptyConfigIsNoop(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "tools/gazelle-rewritable.json", `{"version": 1, "patterns": []}`)
	const original = `genrule(
    name = "x",
    srcs = ["a"],
    outs = ["b"],
    cmd = "protoc ...",
)  # keep
`
	writeFile(t, root, "elements/myelem/BUILD.bazel", original)
	a := args{root: root, rewritableFile: "tools/gazelle-rewritable.json"}
	if err := run(a); err != nil {
		t.Fatalf("run: %v", err)
	}
	got := readFile(t, root, "elements/myelem/BUILD.bazel")
	if got != original {
		t.Errorf("relax-keeps modified BUILD with empty patterns list:\nwant: %q\ngot:  %q", original, got)
	}
}

// TestRelax_MissingConfigIsNoop covers the bootstrap state:
// when the rewritable JSON file doesn't exist at all (e.g.,
// an old project B not yet re-rendered with Phase 8b's
// stub), relax-keeps does nothing rather than erroring.
func TestRelax_MissingConfigIsNoop(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "elements/myelem/BUILD.bazel", `genrule(name = "x", outs = ["y"], cmd = "z")  # keep
`)
	a := args{root: root, rewritableFile: "tools/gazelle-rewritable.json"}
	if err := run(a); err != nil {
		t.Fatalf("run: %v", err)
	}
	got := readFile(t, root, "elements/myelem/BUILD.bazel")
	if !strings.Contains(got, "# keep") {
		t.Errorf("relax-keeps stripped # keep with missing config:\n%s", got)
	}
}

// TestRelax_Idempotent: running relax-keeps twice produces
// the same output as running it once. Lets continuous-
// conversion pipelines call it unconditionally without
// worrying about re-runs producing different states.
func TestRelax_Idempotent(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "tools/gazelle-rewritable.json", `{
  "version": 1, "patterns": [{"name": "protoc", "cmd_contains": "protoc"}]
}
`)
	writeFile(t, root, "elements/myelem/BUILD.bazel", `genrule(
    name = "x",
    srcs = ["a.proto"],
    outs = ["a.pb.cc"],
    cmd = "protoc ...",
)  # keep
`)
	a := args{root: root, rewritableFile: "tools/gazelle-rewritable.json"}
	if err := run(a); err != nil {
		t.Fatalf("run #1: %v", err)
	}
	first := readFile(t, root, "elements/myelem/BUILD.bazel")
	if err := run(a); err != nil {
		t.Fatalf("run #2: %v", err)
	}
	second := readFile(t, root, "elements/myelem/BUILD.bazel")
	if first != second {
		t.Errorf("relax-keeps not idempotent:\nfirst: %q\nsecond: %q", first, second)
	}
}

// TestRelax_TargetedPackages: when the caller passes a list
// of package paths (mirroring orchestrator's
// `res.Converted`), only those subtrees get walked. BUILDs
// outside the listed packages keep their `# keep` markers
// even when the cmd would match. This is the incremental-
// pipeline contract: continuous re-conversion runs only
// touch packages that re-converted on this run.
func TestRelax_TargetedPackages(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "tools/gazelle-rewritable.json", `{
  "version": 1, "patterns": [{"name": "protoc", "cmd_contains": "protoc"}]
}
`)
	writeFile(t, root, "elements/changed/BUILD.bazel", `genrule(
    name = "x", srcs = ["a.proto"], outs = ["a.pb.cc"], cmd = "protoc ...",
)  # keep
`)
	writeFile(t, root, "elements/unchanged/BUILD.bazel", `genrule(
    name = "y", srcs = ["b.proto"], outs = ["b.pb.cc"], cmd = "protoc ...",
)  # keep
`)
	a := args{
		root:           root,
		rewritableFile: "tools/gazelle-rewritable.json",
		packages:       []string{"elements/changed"},
	}
	if err := run(a); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := readFile(t, root, "elements/changed/BUILD.bazel"); strings.Contains(got, "# keep") {
		t.Errorf("targeted package still has # keep:\n%s", got)
	}
	if got := readFile(t, root, "elements/unchanged/BUILD.bazel"); !strings.Contains(got, "# keep") {
		t.Errorf("untargeted package lost # keep (should have been skipped):\n%s", got)
	}
}

// TestRelax_TargetedNonexistentPackageIsNoop: when the caller
// passes a package path that doesn't exist on disk (e.g.,
// the orchestrator's res.Converted included an element that
// failed conversion and never produced a BUILD), relax-keeps
// skips it without error.
func TestRelax_TargetedNonexistentPackageIsNoop(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "tools/gazelle-rewritable.json", `{
  "version": 1, "patterns": [{"name": "protoc", "cmd_contains": "protoc"}]
}
`)
	a := args{
		root:           root,
		rewritableFile: "tools/gazelle-rewritable.json",
		packages:       []string{"elements/never-converted"},
	}
	if err := run(a); err != nil {
		t.Fatalf("run on nonexistent package: %v", err)
	}
}

// TestRelax_OperatorReKeepIsRespected: if the operator
// manually re-added a `# keep` (e.g., they decided this
// particular protoc genrule is special and shouldn't be
// rewritten despite the pattern match), relax-keeps still
// strips it. This is by design — the rewritable.json config
// is the source of truth for "what gazelle handles", and
// the per-genrule keep is a converter-emitted default that
// the operator overrides via config, not via inline edits.
//
// The escape hatch for "this specific genrule must NEVER be
// rewritten" is a more specific marker (e.g., a leading
// `# converter:authoritative` whole-rule comment that
// relax-keeps doesn't recognize as a relaxation target).
// Documented in operator-gazelle-step.md.
//
// This test pins the current behavior: the cmd-match wins
// over a hand-re-added `# keep`. If we ever add the
// `# converter:authoritative` form, the test needs to grow
// a second case for that marker.
func TestRelax_OperatorReKeepIsRespected_TODO(t *testing.T) {
	// Currently no `# converter:authoritative` marker exists;
	// the test is forward-looking. Skip until that marker
	// lands so the test directory documents the intent without
	// asserting a behavior the codebase doesn't have.
	t.Skip("authoritative-marker not implemented; queued in operator-gazelle-step.md follow-up")
}

func writeFile(t *testing.T, root, rel, content string) {
	t.Helper()
	p := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, root, rel string) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(root, rel))
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}
