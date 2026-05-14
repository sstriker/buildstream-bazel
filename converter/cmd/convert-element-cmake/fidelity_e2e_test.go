//go:build e2e

// fidelity_e2e_test is the converter fidelity gate: build a cmake
// project two ways (cmake reference vs convert-element-cmake + bazel
// build) and assert the resulting libraries' `nm --defined-only`
// symbol sets are equivalent.
//
// Why this test matters: a clean conversion only means "translation
// parses cleanly", not "produces correct binaries". Symbol-tier is
// the load-bearing assertion: same set of defined symbols means the
// same translation units compiled with effectively-equivalent flags.
//
// This gate was re-homed from orchestrator/internal/orchestrator/ as
// part of the orchestrator absorption (docs/design/orchestrator-
// absorption.md). It never actually used the orchestrator — it calls
// convert-element-cmake directly, so failures isolate on the
// converter's translation logic — it just lived next to the
// orchestrator's other e2e gates for the shared helpers, which now
// travel with it here.
//
// Gated behind the `e2e` build tag; depends on real cmake +
// bazel/bazelisk + convert-element-cmake + nm. CI runs the two cases
// via `make e2e-fidelity` and `make e2e-fidelity-fmt`.
package main

import (
	"context"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sstriker/buildstream-bazel/internal/fidelity"
)

// fidelityCase configures one parameterized run of the fidelity harness.
type fidelityCase struct {
	// Name is shown in test output. Required.
	Name string

	// SourceRoot is the absolute path to the cmake project root used
	// for both the reference (`cmake … && cmake --build`) and the
	// converted (convert-element-cmake + bazel build) builds. Required.
	SourceRoot string

	// LibName is the symbol-diffed library's target name. The
	// reference build's archive is sought as <BuildDir>/lib<LibName>.a
	// or <BuildDir>/<subdir>/lib<LibName>.a (we walk for it). Bazel's
	// archive comes from `bazel cquery --output=files //:<LibName>`.
	LibName string

	// ModuleBazel is the MODULE.bazel content staged at the Bazel
	// workspace root. Defaults to a minimal rules_cc declaration
	// keyed off Name when empty.
	ModuleBazel string

	// SkipFunc, when non-nil, is consulted before the test runs. A
	// non-empty string skips with that reason. Used for fixtures
	// gated on out-of-band downloads (e.g. fmt at /tmp/fmt).
	SkipFunc func(t *testing.T) string
}

func TestE2E_Fidelity_HelloWorld_SymbolEquivalent(t *testing.T) {
	helloSrc, err := filepath.Abs("../../testdata/sample-projects/hello-world")
	if err != nil {
		t.Fatal(err)
	}
	runSymbolFidelityCase(t, fidelityCase{
		Name:       "hello-world",
		SourceRoot: helloSrc,
		LibName:    "hello",
		ModuleBazel: "module(name = \"hello\", version = \"0.0.0\")\n" +
			"bazel_dep(name = \"rules_cc\", version = \"0.0.10\")\n",
	})
}

// TestE2E_Fidelity_Fmt_SymbolEquivalent: same harness, fmt as the
// fixture. Exercises the converter on a real-world cmake project with
// many translation units. Likely to surface converter bugs the way
// hello-world doesn't; see docs/fidelity-known-deltas.md for the
// observed deltas and how each was triaged.
func TestE2E_Fidelity_Fmt_SymbolEquivalent(t *testing.T) {
	runSymbolFidelityCase(t, fidelityCase{
		Name:       "fmt",
		SourceRoot: "/tmp/fmt",
		LibName:    "fmt",
		ModuleBazel: "module(name = \"fmt_fidelity\", version = \"0.0.0\")\n" +
			"bazel_dep(name = \"rules_cc\", version = \"0.0.10\")\n",
		SkipFunc: func(t *testing.T) string {
			if _, err := os.Stat("/tmp/fmt"); err != nil {
				return "/tmp/fmt not present (run `make fetch-fmt` first)"
			}
			return ""
		},
	})
}

func runSymbolFidelityCase(t *testing.T, c fidelityCase) {
	t.Helper()
	if c.Name == "" || c.SourceRoot == "" || c.LibName == "" {
		t.Fatalf("fidelityCase: Name, SourceRoot, LibName all required")
	}
	if c.SkipFunc != nil {
		if reason := c.SkipFunc(t); reason != "" {
			t.Skip(reason)
		}
	}
	if _, err := exec.LookPath("nm"); err != nil {
		t.Skipf("nm not on PATH: %v", err)
	}
	if _, err := exec.LookPath("cmake"); err != nil {
		t.Skipf("cmake not on PATH: %v", err)
	}
	bazel := lookupBazel(t)
	conv := lookupConverter(t)

	if _, err := os.Stat(c.SourceRoot); err != nil {
		t.Fatalf("%s: SourceRoot %s missing: %v", c.Name, c.SourceRoot, err)
	}

	// Path A: cmake build out-of-band. Same toolchain the converter
	// would use; we don't pass --toolchain-cmake-file because we WANT
	// cmake to do its native probe — that's the reference.
	cmakeBuild := t.TempDir()
	mustRun(t, exec.CommandContext(context.Background(), "cmake",
		"-S", c.SourceRoot, "-B", cmakeBuild, "-G", "Ninja",
		"-DCMAKE_BUILD_TYPE=Release",
	))
	mustRun(t, exec.CommandContext(context.Background(), "cmake",
		"--build", cmakeBuild, "--target", c.LibName,
	))
	cmakeLib := findStaticArchive(cmakeBuild, "lib"+c.LibName+".a")
	if cmakeLib == "" {
		t.Fatalf("%s: cmake build produced no lib%s.a in %s\n  contents: %v",
			c.Name, c.LibName, cmakeBuild, dirEntries(cmakeBuild))
	}

	// Path B: convert-element-cmake + bazel build. Direct convert-element-cmake
	// call (no orchestrator) so failures isolate on the converter's
	// translation logic.
	convOut := t.TempDir()
	mustRun(t, exec.CommandContext(context.Background(), conv,
		"--source-root", c.SourceRoot,
		"--out-build", filepath.Join(convOut, "BUILD.bazel"),
		"--out-bundle-dir", filepath.Join(convOut, "cmake-config"),
	))

	// Stage a Bazel workspace: copy the source tree, drop the
	// emitted BUILD.bazel at the root, write MODULE.bazel.
	ws := t.TempDir()
	if err := copyTreeFiltered(c.SourceRoot, ws); err != nil {
		t.Fatalf("%s: copy source tree: %v", c.Name, err)
	}
	mustCopyFile(t, filepath.Join(convOut, "BUILD.bazel"), filepath.Join(ws, "BUILD.bazel"))
	module := c.ModuleBazel
	if module == "" {
		module = "module(name = \"" + sanitizeBazelName(c.Name) + "\", version = \"0.0.0\")\n" +
			"bazel_dep(name = \"rules_cc\", version = \"0.0.10\")\n"
	}
	mustWriteString(t, filepath.Join(ws, "MODULE.bazel"), module)

	cmd := exec.CommandContext(context.Background(), bazel, "build", "//:"+c.LibName)
	cmd.Dir = ws
	cmd.Stdout = testLog{t}
	cmd.Stderr = testLog{t}
	if err := cmd.Run(); err != nil {
		t.Fatalf("%s: bazel build //:%s: %v", c.Name, c.LibName, err)
	}

	queryCmd := exec.CommandContext(context.Background(), bazel,
		"cquery", "--output=files", "//:"+c.LibName,
	)
	queryCmd.Dir = ws
	out, err := queryCmd.Output()
	if err != nil {
		t.Fatalf("%s: bazel cquery: %v", c.Name, err)
	}
	bazelLib := pickStaticArchive(strings.TrimSpace(string(out)))
	if bazelLib == "" {
		t.Fatalf("%s: bazel cquery returned no .a archive: %q", c.Name, out)
	}
	if !filepath.IsAbs(bazelLib) {
		bazelLib = filepath.Join(ws, bazelLib)
	}
	if _, err := os.Stat(bazelLib); err != nil {
		t.Fatalf("%s: bazel-built %s missing: %v", c.Name, bazelLib, err)
	}

	cmakeSyms, err := fidelity.SymbolSet(cmakeLib)
	if err != nil {
		t.Fatalf("%s: nm cmake: %v", c.Name, err)
	}
	bazelSyms, err := fidelity.SymbolSet(bazelLib)
	if err != nil {
		t.Fatalf("%s: nm bazel: %v", c.Name, err)
	}

	t.Logf("%s: cmake symbols=%d bazel symbols=%d", c.Name, len(cmakeSyms), len(bazelSyms))

	diff := fidelity.DiffSymbols(cmakeSyms, bazelSyms)
	if !diff.Empty() {
		t.Errorf("%s: fidelity symbol mismatch in lib%s.a\n%s\n  see docs/fidelity-known-deltas.md",
			c.Name, c.LibName, diff.Format())
	}
}

// findStaticArchive walks root looking for a file named `name`.
// Returns the absolute path or "".
func findStaticArchive(root, name string) string {
	var hit string
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if !d.IsDir() && d.Name() == name {
			hit = path
			return filepath.SkipAll
		}
		return nil
	})
	return hit
}

// copyTreeFiltered mirrors src into dst, skipping cmake build dirs and
// version-control artifacts that don't belong in a Bazel workspace.
func copyTreeFiltered(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return os.MkdirAll(dst, 0o755)
		}
		// Skip noise + things that conflict with bazel's own conventions.
		base := filepath.Base(rel)
		switch base {
		case ".git", ".github", "build", "_build", "out", "bazel-out", "bazel-bin", "MODULE.bazel", "BUILD.bazel", "WORKSPACE", "WORKSPACE.bazel":
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, body, 0o644)
	})
}

func sanitizeBazelName(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '_':
			out = append(out, c)
		default:
			out = append(out, '_')
		}
	}
	return string(out)
}

// pickStaticArchive picks the first whitespace-separated token in s
// that ends with ".a". `bazel cquery --output=files` for a
// cc_library can list multiple files (the archive plus interface
// metadata); we only diff the archive.
func pickStaticArchive(s string) string {
	for _, tok := range strings.Fields(s) {
		if strings.HasSuffix(tok, ".a") {
			return tok
		}
	}
	return ""
}

func dirEntries(dir string) []string {
	es, _ := os.ReadDir(dir)
	out := make([]string, 0, len(es))
	for _, e := range es {
		out = append(out, e.Name())
	}
	return out
}

func mustRun(t *testing.T, cmd *exec.Cmd) {
	t.Helper()
	cmd.Stdout = testLog{t}
	cmd.Stderr = testLog{t}
	if err := cmd.Run(); err != nil {
		t.Fatalf("%v: %v", cmd.Args, err)
	}
}

func mustCopyFile(t *testing.T, src, dst string) {
	t.Helper()
	body, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("read %s: %v", src, err)
	}
	if err := os.WriteFile(dst, body, 0o644); err != nil {
		t.Fatalf("write %s: %v", dst, err)
	}
}

func mustWriteString(t *testing.T, dst, body string) {
	t.Helper()
	if err := os.WriteFile(dst, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", dst, err)
	}
}

// testLog adapts *testing.T into an io.Writer so subprocess stdout /
// stderr lands in the test log.
type testLog struct{ t *testing.T }

func (l testLog) Write(p []byte) (int, error) {
	l.t.Logf("%s", strings.TrimRight(string(p), "\n"))
	return len(p), nil
}

// lookupBazel returns the bazelisk / bazel binary, or skips.
func lookupBazel(t *testing.T) string {
	t.Helper()
	for _, name := range []string{"bazelisk", "bazel"} {
		if p, err := exec.LookPath(name); err == nil {
			return p
		}
	}
	t.Skip("bazel/bazelisk not on PATH; skipping fidelity gate")
	return ""
}

// lookupConverter returns convert-element-cmake from PATH, falling
// back to build/bin/ (repo root is three levels up from this dir).
func lookupConverter(t *testing.T) string {
	t.Helper()
	if p, err := exec.LookPath("convert-element-cmake"); err == nil {
		return p
	}
	repoRoot, _ := filepath.Abs("../../..")
	fallback := filepath.Join(repoRoot, "build", "bin", "convert-element-cmake")
	if _, err := os.Stat(fallback); err == nil {
		return fallback
	}
	t.Skip("convert-element-cmake not on PATH and not in build/bin/")
	return ""
}
