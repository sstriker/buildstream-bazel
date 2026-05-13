//go:build e2e

// cmakeconsumer_test exercises the M5 CMake-side acceptance gate: an
// unrelated downstream CMake project resolves a converted FDSDK
// element via find_package(<Pkg> CONFIG REQUIRED) against the
// orchestrator's synth-prefix tree.
//
// Configure-time success is the gate. The synth-prefix's
// IMPORTED_LOCATION stubs are zero-byte files (cmake's
// if(NOT EXISTS) check passes via access(R_OK)); we don't try to
// link, only resolve the imported target.
//
// Gated behind the `e2e` build tag — needs real cmake + bwrap +
// convert-element-cmake to run.
package orchestrator_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/sstriker/buildstream-bazel/orchestrator/internal/orchestrator"
)

func TestE2E_CMakeConsumer_FindPackageAgainstSynthPrefix(t *testing.T) {
	conv, err := exec.LookPath("convert-element-cmake")
	if err != nil {
		repoRoot, _ := filepath.Abs("../../..")
		fallback := filepath.Join(repoRoot, "build", "bin", "convert-element-cmake")
		if _, ferr := os.Stat(fallback); ferr == nil {
			conv = fallback
		} else {
			t.Skipf("convert-element-cmake not found (%v / %v)", err, ferr)
		}
	}

	proj, g := mustLoadFixture(t)
	out := t.TempDir()

	if _, err := orchestrator.Run(context.Background(), orchestrator.Options{
		Project:         proj,
		Graph:           g,
		Out:             out,
		ConverterBinary: conv,
		Log:             testLog{t},
	}); err != nil {
		t.Fatalf("orchestrator: %v", err)
	}

	// uses-hello's synth-prefix contains hello's bundle staged at
	// <prefix>/lib/cmake/hello/{helloConfig,helloTargets,helloTargets-release}.cmake
	// plus zero-byte IMPORTED_LOCATION stubs under <prefix>/lib/.
	prefix := filepath.Join(out, "synth-prefix", "components", "uses-hello")
	if _, err := os.Stat(filepath.Join(prefix, "lib", "cmake", "hello")); err != nil {
		t.Fatalf("synth-prefix missing expected bundle: %v", err)
	}

	consumerSrc, err := filepath.Abs("../../testdata/cmake-consumer")
	if err != nil {
		t.Fatal(err)
	}
	consumerBuild := t.TempDir()

	cmakeBin, err := exec.LookPath("cmake")
	if err != nil {
		t.Skipf("cmake not on PATH: %v", err)
	}
	cmd := exec.CommandContext(context.Background(), cmakeBin,
		"-S", consumerSrc,
		"-B", consumerBuild,
		"-DCMAKE_PREFIX_PATH="+prefix,
	)
	cmd.Stdout = testLog{t}
	cmd.Stderr = testLog{t}
	if err := cmd.Run(); err != nil {
		t.Fatalf("cmake -S consumer-src failed against synth-prefix=%s: %v", prefix, err)
	}
}
