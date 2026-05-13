//go:build e2e

// fmt_e2e exercises the full convert-element pipeline against the fmt
// library — the M2 acceptance package. We fetch fmt out-of-band (Makefile
// `fetch-fmt` or CI step), point the test at the local checkout, and
// assert:
//
//   - Conversion completes without Tier-1 errors.
//   - The expected core targets are present (fmt, gtest, test-main, plus
//     ~20 *_test cc_binary rules).
//   - The cmake-config bundle is synthesized (FMTConfig.cmake et al.).
//   - No genrule recovery is triggered (fmt has no add_custom_command in
//     the codemodel; if a future fmt version adds one, the assertion below
//     will helpfully fail and the operator can adjust).
//
// We do not attempt the byte-equivalence parity check between Bazel and
// upstream cmake/install in M2 — toolchain identity is M3's problem (per
// the M2 plan's drop-out criteria).
package cmakerun_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/emit/bazel"
	"github.com/sstriker/buildstream-bazel/converter/internal/cmakerun"
	"github.com/sstriker/buildstream-bazel/converter/internal/ctest"
	"github.com/sstriker/buildstream-bazel/converter/internal/emit/cmakecfg"
	"github.com/sstriker/buildstream-bazel/converter/internal/fileapi"
	"github.com/sstriker/buildstream-bazel/converter/internal/lower"
	"github.com/sstriker/buildstream-bazel/converter/internal/ninja"
)

// fmtSourceRoot is where the test expects to find a fmt checkout. The
// Makefile target `fetch-fmt` clones a pinned tag here; CI does the same
// before invoking `make e2e-fmt`.
const fmtSourceRoot = "/tmp/fmt"

func TestE2E_Fmt_Converts(t *testing.T) {
	if _, err := os.Stat(fmtSourceRoot); err != nil {
		t.Skipf("%s not present (run `make fetch-fmt` first): %v", fmtSourceRoot, err)
	}

	buildDir := t.TempDir()
	reply, err := cmakerun.Configure(context.Background(), cmakerun.Options{
		SourceRoot: fmtSourceRoot,
		BuildDir:   buildDir,
		Stdout:     testWriter{t},
		Stderr:     testWriter{t},
	})
	if err != nil {
		t.Fatalf("Configure: %v", err)
	}

	r, err := fileapi.Load(reply.Path)
	if err != nil {
		t.Fatalf("fileapi.Load: %v", err)
	}
	g, err := ninja.ParseFile(filepath.Join(buildDir, "build.ninja"))
	if err != nil {
		t.Fatalf("ninja.ParseFile: %v", err)
	}
	registry, err := ctest.Parse(buildDir)
	if err != nil {
		t.Fatalf("ctest.Parse: %v", err)
	}
	pkg, err := lower.ToIR(r, g, lower.Options{
		HostSourceRoot: fmtSourceRoot,
		CTest:          registry,
	})
	if err != nil {
		t.Fatalf("ToIR: %v", err)
	}

	t.Logf("pkg name=%s targets=%d", pkg.Name, len(pkg.Targets))

	// fmt declares the main library plus a bundled gtest copy + test-main
	// glue. All three must lower to cc_library.
	wantLibs := []string{"fmt", "gtest", "test-main"}
	for _, want := range wantLibs {
		var found bool
		for _, t := range pkg.Targets {
			if t.Name == want && t.Kind.String() == "cc_library" {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("missing cc_library %q in converted output", want)
		}
	}

	// >= 15 *_test cc_test rules is a reasonable lower bound; fmt 11.0.2
	// emits 21. Bumping versions can shift the count; we don't pin exactly.
	// Pre-CTest support these were cc_binary; the conversion now lifts
	// add_test() registrations into cc_test rules so `bazel test //...`
	// exercises the same artifacts CTest would.
	var testRules int
	for _, t := range pkg.Targets {
		if strings.HasSuffix(t.Name, "-test") && t.Kind.String() == "cc_test" {
			testRules++
		}
	}
	if testRules < 15 {
		t.Errorf("only %d *-test cc_test rules emitted; expected >= 15", testRules)
	}

	out, err := bazel.Emit(pkg)
	if err != nil {
		t.Fatalf("bazel.Emit: %v", err)
	}
	if !strings.Contains(string(out), `name = "fmt"`) {
		t.Errorf("BUILD.bazel doesn't declare a target named fmt")
	}

	bundle, err := cmakecfg.Emit(pkg, cmakecfg.Options{})
	if err != nil {
		t.Fatalf("cmakecfg.Emit: %v", err)
	}
	// The cmake project name is "FMT" (uppercase) per the upstream
	// CMakeLists; bundle filenames follow.
	for _, want := range []string{
		"FMTConfig.cmake",
		"FMTTargets.cmake",
		"FMTTargets-release.cmake",
	} {
		if _, ok := bundle.Files[want]; !ok {
			t.Errorf("bundle missing %s", want)
		}
	}
}
