package reapi

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sstriker/buildstream-bazel/internal/cas"
)

// fixture writes the standard four-input layout under a tmpdir and
// returns Inputs ready for Build.
func fixture(t *testing.T, withImports, withPrefix bool) Inputs {
	t.Helper()
	root := t.TempDir()

	shadow := filepath.Join(root, "shadow")
	mustMkdirP(t, shadow, "src")
	mustWrite(t, filepath.Join(shadow, "CMakeLists.txt"), "project(x)\n")
	mustWrite(t, filepath.Join(shadow, "src", "x.c"), "")

	conv := filepath.Join(root, "convert-element")
	mustWrite(t, conv, "#!/bin/sh\necho fake\n")
	if err := os.Chmod(conv, 0o755); err != nil {
		t.Fatalf("chmod: %v", err)
	}

	in := Inputs{
		ShadowDir:    shadow,
		ConverterBin: conv,
		Platform: []PlatformProperty{
			{Name: "OSFamily", Value: "linux"},
			{Name: "Arch", Value: "x86_64"},
			{Name: "cmake-version", Value: "3.28.3"},
		},
	}
	if withImports {
		path := filepath.Join(root, "imports.json")
		mustWrite(t, path, `{"version":1,"imports":[]}`)
		in.ImportsManifest = path
	}
	if withPrefix {
		prefix := filepath.Join(root, "prefix")
		mustMkdirP(t, prefix, "lib/cmake/x")
		mustWrite(t, filepath.Join(prefix, "lib/cmake/x/xConfig.cmake"), "")
		in.PrefixDir = prefix
	}
	return in
}

func TestBuild_Deterministic(t *testing.T) {
	in := fixture(t, true, true)
	a, err := Build(in)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	b, err := Build(in)
	if err != nil {
		t.Fatalf("Build (rerun): %v", err)
	}
	if !cas.DigestEqual(a.ActionDigest, b.ActionDigest) {
		t.Fatalf("Action digest unstable: %s vs %s",
			cas.DigestString(a.ActionDigest), cas.DigestString(b.ActionDigest))
	}
	if !cas.DigestEqual(a.CommandDigest, b.CommandDigest) {
		t.Errorf("Command digest unstable: %s vs %s",
			cas.DigestString(a.CommandDigest), cas.DigestString(b.CommandDigest))
	}
	if !cas.DigestEqual(a.InputRoot.RootDigest, b.InputRoot.RootDigest) {
		t.Errorf("InputRoot digest unstable: %s vs %s",
			cas.DigestString(a.InputRoot.RootDigest), cas.DigestString(b.InputRoot.RootDigest))
	}
}

func TestBuild_DifferentPathsSameContent_SameDigest(t *testing.T) {
	in1 := fixture(t, false, false)
	in2 := fixture(t, false, false)
	a, _ := Build(in1)
	b, _ := Build(in2)
	if !cas.DigestEqual(a.ActionDigest, b.ActionDigest) {
		t.Fatalf("two host paths with same content should produce same Action digest, got %s vs %s",
			cas.DigestString(a.ActionDigest), cas.DigestString(b.ActionDigest))
	}
}

func TestBuild_ContentEditChangesActionDigest(t *testing.T) {
	in := fixture(t, false, false)
	before, _ := Build(in)

	// Mutate the converter binary.
	mustWrite(t, in.ConverterBin, "#!/bin/sh\necho different\n")

	after, _ := Build(in)
	if cas.DigestEqual(before.ActionDigest, after.ActionDigest) {
		t.Errorf("converter binary edit should change Action digest, got %s == %s",
			cas.DigestString(before.ActionDigest), cas.DigestString(after.ActionDigest))
	}
}

func TestBuild_PlatformDigestsDiffer(t *testing.T) {
	in1 := fixture(t, false, false)
	in1.Platform = []PlatformProperty{
		{Name: "cmake-version", Value: "3.28.3"},
	}
	in2 := in1
	in2.Platform = []PlatformProperty{
		{Name: "cmake-version", Value: "3.30.0"},
	}
	a1, _ := Build(in1)
	a2, _ := Build(in2)
	if cas.DigestEqual(a1.ActionDigest, a2.ActionDigest) {
		t.Errorf("different cmake-version should produce different Action digests")
	}
}

func TestBuild_ArgvHasCanonicalPaths(t *testing.T) {
	in := fixture(t, true, true)
	a, err := Build(in)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	args := a.Command.Arguments
	want := []string{
		"bin/convert-element",
		"--source-root", "source",
		"--out-build", "BUILD.bazel",
		"--out-bundle-dir", "cmake-config",
		"--out-failure", "failure.json",
		"--out-read-paths", "read_paths.json",
		"--out-timings", "timings.json",
		"--imports-manifest", "imports.json",
		"--prefix-dir", "prefix",
	}
	if len(args) != len(want) {
		t.Fatalf("argv length: got %d want %d (%v)", len(args), len(want), args)
	}
	for i := range want {
		if args[i] != want[i] {
			t.Errorf("argv[%d]: got %q want %q", i, args[i], want[i])
		}
	}
}

// TestBuild_CollectToolchainSignal: when on, the action gets the
// --out-toolchain-signal-dir flag plus the matching output path,
// and the action digest changes (the flag flip is observable in
// CAS so cached results from before the flip don't get reused).
func TestBuild_CollectToolchainSignal(t *testing.T) {
	in := fixture(t, false, false)
	a1, err := Build(in)
	if err != nil {
		t.Fatalf("Build w/o signal: %v", err)
	}

	in.CollectToolchainSignal = true
	a2, err := Build(in)
	if err != nil {
		t.Fatalf("Build w/ signal: %v", err)
	}

	// Argv has the new flag + path.
	argv := a2.Command.Arguments
	hasFlag := false
	hasPath := false
	for i, a := range argv {
		if a == "--out-toolchain-signal-dir" && i+1 < len(argv) && argv[i+1] == "toolchain-signal" {
			hasFlag = true
		}
	}
	for _, p := range a2.Command.OutputPaths {
		if p == "toolchain-signal" {
			hasPath = true
		}
	}
	if !hasFlag {
		t.Errorf("expected --out-toolchain-signal-dir toolchain-signal in argv; got %v", argv)
	}
	if !hasPath {
		t.Errorf("expected toolchain-signal in OutputPaths; got %v", a2.Command.OutputPaths)
	}

	// Different action digest.
	if a1.ActionDigest.Hash == a2.ActionDigest.Hash {
		t.Errorf("ActionDigest unchanged with CollectToolchainSignal flip; should differ")
	}
}

// TestBuild_EmitIRJSON: when on, the action gets the
// --out-ir-json flag plus the ir.json output path, and the
// action digest shifts so cached results from before the flip
// don't get reused.
func TestBuild_EmitIRJSON(t *testing.T) {
	in := fixture(t, false, false)
	a1, err := Build(in)
	if err != nil {
		t.Fatalf("Build w/o IR JSON: %v", err)
	}

	in.EmitIRJSON = true
	a2, err := Build(in)
	if err != nil {
		t.Fatalf("Build w/ IR JSON: %v", err)
	}

	argv := a2.Command.Arguments
	hasFlag := false
	hasPath := false
	for i, a := range argv {
		if a == "--out-ir-json" && i+1 < len(argv) && argv[i+1] == "ir.json" {
			hasFlag = true
		}
	}
	for _, p := range a2.Command.OutputPaths {
		if p == "ir.json" {
			hasPath = true
		}
	}
	if !hasFlag {
		t.Errorf("expected --out-ir-json ir.json in argv; got %v", argv)
	}
	if !hasPath {
		t.Errorf("expected ir.json in OutputPaths; got %v", a2.Command.OutputPaths)
	}
	if a1.ActionDigest.Hash == a2.ActionDigest.Hash {
		t.Errorf("ActionDigest unchanged with EmitIRJSON flip; should differ")
	}
}

func TestBuild_TimeoutSetsAction(t *testing.T) {
	in := fixture(t, false, false)
	in.Timeout = 17 * time.Minute
	a, err := Build(in)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if a.Action.Timeout == nil {
		t.Fatalf("Action.Timeout is nil; should have been set from Inputs.Timeout")
	}
	if got := a.Action.Timeout.AsDuration(); got != 17*time.Minute {
		t.Errorf("Action.Timeout = %v, want 17m", got)
	}
}

func TestBuild_NoTimeoutLeavesActionUnset(t *testing.T) {
	in := fixture(t, false, false)
	a, err := Build(in)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if a.Action.Timeout != nil {
		t.Errorf("Action.Timeout should be unset when Inputs.Timeout is zero, got %v", a.Action.Timeout.AsDuration())
	}
}

// TestBuild_ToolchainCMakeFile_GraftsIntoInputRoot: when set, the
// file lands at toolchain.cmake in the input root and argv passes
// --toolchain-cmake-file referencing that path. Two builds with
// the same toolchain file content produce identical Action digests
// (host path independence preserved).
func TestBuild_ToolchainCMakeFile_GraftsIntoInputRoot(t *testing.T) {
	in := fixture(t, false, false)
	tcDir := t.TempDir()
	tcPath := filepath.Join(tcDir, "toolchain.cmake")
	if err := os.WriteFile(tcPath, []byte("# fake toolchain file\n"), 0o644); err != nil {
		t.Fatalf("write toolchain.cmake: %v", err)
	}
	in.ToolchainCMakeFile = tcPath

	a, err := Build(in)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	// Argv references the in-action path, not the host path.
	argv := a.Command.Arguments
	want := []string{"--toolchain-cmake-file", "toolchain.cmake"}
	found := false
	for i := 0; i+1 < len(argv); i++ {
		if argv[i] == want[0] && argv[i+1] == want[1] {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("argv missing --toolchain-cmake-file toolchain.cmake; got %v", argv)
	}
	for _, arg := range argv {
		if strings.Contains(arg, tcDir) {
			t.Errorf("argv leaked host path %q: %v", tcDir, argv)
		}
	}

	// Input root must reference the toolchain file as a top-level
	// FileNode.
	rootDir := a.InputRoot.Root
	var foundFile bool
	for _, f := range rootDir.Files {
		if f.Name == "toolchain.cmake" {
			foundFile = true
			break
		}
	}
	if !foundFile {
		t.Errorf("input root has no toolchain.cmake FileNode; got %v", rootDir.Files)
	}
}

// TestBuild_ToolchainCMakeFile_HostPathIndependence: same file
// content under two different host directories produces identical
// Action digests. The file's bytes (not its host location) drive
// cache identity.
func TestBuild_ToolchainCMakeFile_HostPathIndependence(t *testing.T) {
	body := []byte("# stable content\n")

	in1 := fixture(t, false, false)
	p1 := filepath.Join(t.TempDir(), "toolchain.cmake")
	if err := os.WriteFile(p1, body, 0o644); err != nil {
		t.Fatalf("write 1: %v", err)
	}
	in1.ToolchainCMakeFile = p1

	in2 := fixture(t, false, false)
	p2 := filepath.Join(t.TempDir(), "elsewhere.cmake")
	if err := os.WriteFile(p2, body, 0o644); err != nil {
		t.Fatalf("write 2: %v", err)
	}
	in2.ToolchainCMakeFile = p2

	a1, _ := Build(in1)
	a2, _ := Build(in2)
	if !cas.DigestEqual(a1.ActionDigest, a2.ActionDigest) {
		t.Errorf("Action digests differ for same-content toolchain.cmake: %s vs %s",
			cas.DigestString(a1.ActionDigest), cas.DigestString(a2.ActionDigest))
	}
}

// TestBuild_ToolchainCMakeFile_ContentChangesDigest: editing the
// toolchain file flips the Action digest, so the orchestrator's
// cache invalidates correctly when the operator bumps a pin.
func TestBuild_ToolchainCMakeFile_ContentChangesDigest(t *testing.T) {
	in := fixture(t, false, false)
	p := filepath.Join(t.TempDir(), "toolchain.cmake")
	if err := os.WriteFile(p, []byte("v1"), 0o644); err != nil {
		t.Fatalf("write v1: %v", err)
	}
	in.ToolchainCMakeFile = p
	before, _ := Build(in)

	if err := os.WriteFile(p, []byte("v2"), 0o644); err != nil {
		t.Fatalf("write v2: %v", err)
	}
	after, _ := Build(in)

	if cas.DigestEqual(before.ActionDigest, after.ActionDigest) {
		t.Errorf("Action digest unchanged after toolchain.cmake content edit")
	}
}

func TestBuild_OutputPaths(t *testing.T) {
	in := fixture(t, false, false)
	a, _ := Build(in)
	want := []string{"BUILD.bazel", "cmake-config", "failure.json", "read_paths.json", "timings.json"}
	if len(a.OutputPaths) != len(want) {
		t.Fatalf("output_paths len: got %d want %d", len(a.OutputPaths), len(want))
	}
	for i := range want {
		if a.OutputPaths[i] != want[i] {
			t.Errorf("output_paths[%d]: got %q want %q", i, a.OutputPaths[i], want[i])
		}
	}
}

func TestBuild_InputRootContainsAllBlobs(t *testing.T) {
	in := fixture(t, true, true)
	a, _ := Build(in)
	// Every file referenced from any Directory must have its blob
	// known via InputRoot.Files keyed by digest hash.
	for _, dir := range a.InputRoot.Directories {
		for _, f := range dir.Files {
			if _, ok := a.InputRoot.Files[f.Digest.Hash]; !ok {
				t.Errorf("file %s digest %s not in InputRoot.Files", f.Name, cas.DigestString(f.Digest))
			}
		}
	}
}

func mustMkdirP(t *testing.T, parts ...string) {
	t.Helper()
	p := filepath.Join(parts...)
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", p, err)
	}
}

func mustWrite(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
