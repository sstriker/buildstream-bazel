package cmakerun_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/internal/cmakerun"
	"github.com/sstriker/buildstream-bazel/converter/internal/fileapi"
	"github.com/sstriker/buildstream-bazel/converter/internal/lower"
	"github.com/sstriker/buildstream-bazel/internal/manifest"
)

// TestDumpVars_DecoupledFromProbeGenex pins the post-#229
// behavior: dump-vars fires independently of probe-genex /
// lift-configure-file. Pre-decoupling (PR #227 round 3 +
// #229 first commit), DumpVars was implicitly
// `LiftConfigureFile || ProbeGenex` inside
// cmd/convert-element-cmake/main.go. Operators who ran with
// `--probe-genex=false --lift-configure-file=false` lost the
// variable-form find_package attribution path silently. The
// decoupling (#229 second commit) makes DumpVars its own
// surface (--dump-vars, default true) so attribution fires
// whenever an imports-manifest is provided, regardless of the
// orthogonal genex / configure_file flags.
//
// This test asserts the cmakerun layer of the contract: with
// only `Options.DumpVars = true`, the dump file lands and
// carries the `<Pkg>_FOUND` / `<Pkg>_LIBRARIES` keys
// `buildFindPackageAttrib` needs. The end-to-end attribution
// is covered by emit/bazel.TestEmit_FindPackageVariableForm_Golden
// (offline) and scripts/meta-cmake-find-package-variable-form.sh
// (live cmake). This one fills the cmakerun-layer gap.
func TestDumpVars_DecoupledFromProbeGenex(t *testing.T) {
	if _, err := exec.LookPath("cmake"); err != nil {
		t.Skip("cmake not on PATH")
	}
	major, minor, _, err := cmakerun.AssertVersion(context.Background())
	if err != nil {
		t.Skipf("cmake below codemodel-v2 floor: %v", err)
	}
	if major < 3 || (major == 3 && minor < 24) {
		t.Skipf("cmake %d.%d below CMAKE_PROJECT_TOP_LEVEL_INCLUDES floor (3.24+)", major, minor)
	}

	src, err := filepath.Abs("../../testdata/sample-projects/find-package-variable-form")
	if err != nil {
		t.Fatal(err)
	}

	buildDir := t.TempDir()
	reply, err := cmakerun.Configure(context.Background(), cmakerun.Options{
		SourceRoot: src,
		BuildDir:   buildDir,
		// The point of this test: DumpVars on with everything
		// else off. Pre-decoupling this combination produced no
		// vars.dump file (DumpVars was implicitly false unless
		// ProbeGenex or LiftConfigureFile was on).
		DumpVars:    true,
		ProbeGenex:  false,
		CMP0026Shim: false,
		Stdout:      os.Stderr,
		Stderr:      os.Stderr,
	})
	if err != nil {
		t.Fatalf("Configure: %v", err)
	}

	// vars.dump should exist and carry ZLIB_FOUND.
	if len(reply.Vars) == 0 {
		t.Fatal("Reply.Vars empty — dump-vars hook didn't fire even though DumpVars=true")
	}
	if v, ok := reply.Vars["ZLIB_FOUND"]; !ok {
		t.Errorf("vars dump missing ZLIB_FOUND — find_package(ZLIB) didn't run or dump missed it")
	} else if !isTruthy(v) {
		t.Errorf("ZLIB_FOUND = %q; expected a truthy value", v)
	}

	// End-to-end check: load the reply, run ToIR with the
	// imports manifest, confirm the attribution path used the
	// dump-vars data (no need for configureLog events) and
	// emitted the //elements/zlib label.
	r, err := fileapi.Load(reply.Path)
	if err != nil {
		t.Fatalf("fileapi.Load: %v", err)
	}
	imports, err := manifest.Load(filepath.Join(src, "imports.json"))
	if err != nil {
		t.Fatalf("manifest.Load: %v", err)
	}
	pkg, err := lower.ToIR(r, nil, lower.Options{
		HostSourceRoot: src,
		Imports:        imports,
		// The point: variable-form attribution reads CMakeVars
		// (the dump-vars output) directly. Pre-#229 the only
		// caller that populated this was the convert-element-cmake
		// binary, which gated it on a flag combination that
		// excluded the bare --dump-vars-on-only path.
		CMakeVars: reply.Vars,
	})
	if err != nil {
		t.Fatalf("ToIR: %v", err)
	}
	var depsSeen []string
	for _, target := range pkg.Targets {
		depsSeen = append(depsSeen, target.Deps...)
	}
	found := false
	for _, d := range depsSeen {
		if strings.Contains(d, "//elements/zlib") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("variable-form attribution didn't emit //elements/zlib with DumpVars=true alone (deps seen: %v)", depsSeen)
	}
}

func isTruthy(v string) bool {
	v = strings.ToUpper(strings.TrimSpace(v))
	switch v {
	case "1", "ON", "YES", "TRUE", "Y":
		return true
	}
	return false
}
