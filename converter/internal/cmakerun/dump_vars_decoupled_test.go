package cmakerun_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/internal/cmakerun"
)

// TestDumpVars_DecoupledFromProbeGenex pins the post-#229
// behavior: dump-vars fires independently of probe-genex /
// lift-configure-file. Pre-decoupling (PR #227 round 3 +
// #229 first commit), DumpVars was implicitly
// `LiftConfigureFile || ProbeGenex` inside
// cmd/convert-element-cmake/main.go. Operators who ran with
// `--probe-genex=false --lift-configure-file=false` lost the
// dump-vars surface silently.
//
// This test asserts the cmakerun layer of the contract: with
// only `Options.DumpVars = true`, the dump file lands and
// carries the `<Pkg>_FOUND` / `<Pkg>_LIBRARIES` keys that the
// option-lift and other CMakeVars consumers read. (External
// dependency attribution no longer flows through CMakeVars —
// it comes from the expanded target_link_libraries trace; see
// lower.attributeDirectTraceDeps and
// scripts/meta-cmake-find-package-variable-form.sh.)
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

	// vars.dump should exist and carry ZLIB_FOUND — the cmakerun-layer
	// contract other CMakeVars consumers (option-lift, configure_file
	// recovery) depend on.
	if len(reply.Vars) == 0 {
		t.Fatal("Reply.Vars empty — dump-vars hook didn't fire even though DumpVars=true")
	}
	if v, ok := reply.Vars["ZLIB_FOUND"]; !ok {
		t.Errorf("vars dump missing ZLIB_FOUND — find_package(ZLIB) didn't run or dump missed it")
	} else if !isTruthy(v) {
		t.Errorf("ZLIB_FOUND = %q; expected a truthy value", v)
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
