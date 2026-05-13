package verify_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/internal/verify"
	"github.com/sstriker/buildstream-bazel/converter/ir"
)

// TestVerify_AgreesWhenIRMatches checks the green path: an IR target
// whose -D and -I sets exactly match what compile_commands.json
// records produces an empty Mismatches slice. This is the
// "compile_commands is independent of codemodel-v2 but they should
// say the same thing" assertion the verify pass exists to enforce.
func TestVerify_AgreesWhenIRMatches(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src", "foo.c")
	if err := os.MkdirAll(filepath.Dir(src), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(src, []byte{}, 0o644); err != nil {
		t.Fatal(err)
	}
	ccPath := filepath.Join(dir, "compile_commands.json")
	body := `[{
		"directory": "` + dir + `/build",
		"file": "` + src + `",
		"command": "/usr/bin/cc -DFOO -DBAR=1 -I` + dir + `/include -c ` + src + `"
	}]`
	if err := os.WriteFile(ccPath, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	pkg := &ir.Package{
		Name: "p",
		Targets: []ir.Target{{
			Name:     "foo",
			Kind:     ir.KindCCLibrary,
			Srcs:     []string{"src/foo.c"},
			Includes: []string{"include"},
			Defines:  []string{"FOO", "BAR=1"},
		}},
	}
	rep, err := verify.Verify(ccPath, pkg, dir)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if len(rep.Mismatches) != 0 {
		t.Errorf("expected no mismatches, got %d: %+v", len(rep.Mismatches), rep.Mismatches)
	}
}

// TestVerify_FlagsDroppedDefine constructs the regression scenario
// the verify pass exists to catch: a -D macro that compile_commands
// records but the IR forgot to surface (e.g. a flag-classification
// bug). Asserts a "missing-define" mismatch is produced.
func TestVerify_FlagsDroppedDefine(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "main.c")
	if err := os.WriteFile(src, []byte{}, 0o644); err != nil {
		t.Fatal(err)
	}
	ccPath := filepath.Join(dir, "compile_commands.json")
	body := `[{
		"directory": "` + dir + `",
		"file": "` + src + `",
		"command": "/usr/bin/cc -DLOST_FLAG -c ` + src + `"
	}]`
	if err := os.WriteFile(ccPath, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	pkg := &ir.Package{
		Name: "p",
		Targets: []ir.Target{{
			Name: "main",
			Kind: ir.KindCCBinary,
			Srcs: []string{"main.c"},
		}},
	}
	rep, err := verify.Verify(ccPath, pkg, dir)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if len(rep.Mismatches) != 1 ||
		rep.Mismatches[0].Kind != "missing-define" ||
		rep.Mismatches[0].Detail != "LOST_FLAG" {
		t.Errorf("expected one missing-define LOST_FLAG, got %+v", rep.Mismatches)
	}
}

// TestVerify_MissingFileIsNotAnError matches the --reply-dir path
// where compile_commands.json wasn't recorded alongside the fixture.
// Verify should return cleanly with an empty report rather than
// erroring; the converter still succeeds.
func TestVerify_MissingFileIsNotAnError(t *testing.T) {
	rep, err := verify.Verify("/nonexistent/compile_commands.json", &ir.Package{}, "/")
	if err != nil {
		t.Fatalf("Verify: unexpected error: %v", err)
	}
	if rep == nil || len(rep.Mismatches) != 0 {
		t.Errorf("expected empty report, got %+v", rep)
	}
}
