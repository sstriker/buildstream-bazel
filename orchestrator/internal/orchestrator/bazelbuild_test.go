//go:build e2e

// bazelbuild_test exercises the M3 downstream-Bazel acceptance gate:
//
//  1. Run the orchestrator against the fdsdk-subset fixture using the
//     real convert-element-cmake binary. Outputs land at <out>/elements/<name>/
//     and the orchestrator emits <out>/MODULE.bazel making <out>/ a
//     self-contained bzlmod project.
//  2. Run `bazel build //elements/components/uses-hello:uses_hello_bin`
//     directly inside <out>/. The build resolves the cross-element
//     dep //elements/components/hello:hello via the orchestrator-stamped
//     imports manifest; success means the BUILD.bazel the converter
//     emitted is consumable by Bazel + rules_cc end-to-end.
//
// Gated behind the `e2e` build tag and skips if `bazel` (or `bazelisk`)
// is not on PATH. CI runs this via `make e2e-bazel-build`.
package orchestrator_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/sstriker/buildstream-bazel/orchestrator/internal/orchestrator"
)

func TestE2E_BazelBuild_DownstreamConsumesConvertedRepos(t *testing.T) {
	bazel := lookupBazel(t)

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

	cmd := exec.CommandContext(context.Background(), bazel,
		"build", "//elements/components/uses-hello:uses_hello_bin")
	cmd.Dir = out
	cmd.Stdout = testLog{t}
	cmd.Stderr = testLog{t}
	if err := cmd.Run(); err != nil {
		t.Fatalf("bazel build inside %s: %v", out, err)
	}
}

func lookupBazel(t *testing.T) string {
	t.Helper()
	for _, name := range []string{"bazelisk", "bazel"} {
		if p, err := exec.LookPath(name); err == nil {
			return p
		}
	}
	t.Skipf("bazel/bazelisk not on PATH; skipping M3 downstream-build acceptance")
	return ""
}
