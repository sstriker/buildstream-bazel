package orchestrator_test

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/sstriker/cmake-to-bazel/orchestrator/internal/element"
	"github.com/sstriker/cmake-to-bazel/orchestrator/internal/orchestrator"
)

// TestMain double-duties: when invoked with ORCHESTRATOR_STUB_CONVERTER=1 it
// behaves as a stub `convert-element` binary instead of running the test
// suite. This lets the orchestrator unit tests exec the test binary itself
// as the converter — no separate fixture binary, no shell-script
// dependence.
func TestMain(m *testing.M) {
	if os.Getenv("ORCHESTRATOR_STUB_CONVERTER") == "1" {
		os.Exit(stubConverter())
	}
	os.Exit(m.Run())
}

// stubConverter parses the convert-element flag surface (just enough), then
// produces success / Tier-1 / Tier-2 outputs as directed by env.
func stubConverter() int {
	fs := flag.NewFlagSet("stub", flag.ContinueOnError)
	srcRoot := fs.String("source-root", "", "")
	replyDir := fs.String("reply-dir", "", "")
	outBuild := fs.String("out-build", "", "")
	outBundle := fs.String("out-bundle-dir", "", "")
	outFailure := fs.String("out-failure", "", "")
	outReadPaths := fs.String("out-read-paths", "", "")
	outTimings := fs.String("out-timings", "", "")
	importsManifest := fs.String("imports-manifest", "", "")
	prefixDir := fs.String("prefix-dir", "", "")
	if err := fs.Parse(os.Args[1:]); err != nil {
		return 64
	}
	_, _ = replyDir, prefixDir

	mode := os.Getenv("ORCHESTRATOR_STUB_MODE")
	if mode == "" {
		mode = "success"
	}
	// elementName comes from the orchestrator's EnvVars plumbing; under
	// LocalExecutor srcRoot is the relative input-root path "source",
	// so filepath.Base(srcRoot) is no longer the element identity.
	// Falls back to filepath.Base(*srcRoot) for legacy callers.
	elementName := os.Getenv("ORCHESTRATOR_ELEMENT_NAME")
	if elementName == "" {
		elementName = filepath.Base(*srcRoot)
	}

	// Optional: record the imports-manifest contents so the test can
	// inspect them. Pre-M5, the stub recorded the manifest path, but the
	// path is now relative to the executor's input root (e.g.
	// "imports.json") and unreadable from the test harness — so we
	// record the resolved content instead.
	if recDir := os.Getenv("ORCHESTRATOR_STUB_RECORD_DIR"); recDir != "" {
		_ = os.MkdirAll(recDir, 0o755)
		recPath := filepath.Join(recDir, filepath.Base(elementName)+".imports.txt")
		var content []byte
		if *importsManifest != "" {
			content, _ = os.ReadFile(*importsManifest)
		}
		_ = os.WriteFile(recPath, content, 0o644)
	}

	// Per-element override: ORCHESTRATOR_STUB_MODE_<sanitized-element-name>
	// where the element name is uppercased and non-[A-Z0-9_] chars are
	// replaced with _. Lets one Run invocation drive different
	// elements to different outcomes — needed for dep-failed tests.
	if perElem := os.Getenv("ORCHESTRATOR_STUB_MODE_" + sanitizeStubKey(elementName)); perElem != "" {
		mode = perElem
	}
	// Optional sentinel emitted to stdout, used by tests asserting
	// that worker output reaches the orchestrator.
	if sentinel := os.Getenv("ORCHESTRATOR_STUB_STDOUT_SENTINEL"); sentinel != "" {
		fmt.Println(sentinel)
	}
	// Optional sleep, used by timeout tests. Values parsed via
	// time.ParseDuration; bad values fall through silently (the test
	// will then fail because no work happened).
	if sleep := os.Getenv("ORCHESTRATOR_STUB_SLEEP"); sleep != "" {
		if d, err := time.ParseDuration(sleep); err == nil {
			time.Sleep(d)
		}
	}

	switch mode {
	case "success":
		body := fmt.Sprintf("# stub BUILD.bazel for %s\n", elementName)
		if err := os.MkdirAll(filepath.Dir(*outBuild), 0o755); err != nil {
			return 70
		}
		if err := os.WriteFile(*outBuild, []byte(body), 0o644); err != nil {
			return 70
		}
		if *outBundle != "" {
			// Strip any path components in the element name ("components/hello"
			// -> "hello") so bundle filenames stay short and stable across
			// element layouts.
			pkg := strings.ToLower(filepath.Base(elementName))
			// Synth-prefix layout: cmake-config files live under
			// lib/cmake/<Pkg>/. Mirrors what real convert-element writes
			// via synthprefix.BuildSlice.
			pkgDir := filepath.Join(*outBundle, "lib", "cmake", pkg)
			if err := os.MkdirAll(pkgDir, 0o755); err != nil {
				return 70
			}
			_ = os.WriteFile(filepath.Join(pkgDir, pkg+"Config.cmake"), []byte("# stub config\n"), 0o644)
			// Realistic Targets.cmake so the orchestrator's exports-extraction
			// picks up the import declaration when building the next element's
			// imports manifest.
			targets := fmt.Sprintf("add_library(%s::%s STATIC IMPORTED)\n", pkg, pkg)
			_ = os.WriteFile(filepath.Join(pkgDir, pkg+"Targets.cmake"), []byte(targets), 0o644)
			_ = os.WriteFile(filepath.Join(pkgDir, pkg+"Targets-release.cmake"), []byte("# stub release\n"), 0o644)
		}
		if *outReadPaths != "" {
			_ = os.MkdirAll(filepath.Dir(*outReadPaths), 0o755)
			_ = os.WriteFile(*outReadPaths, []byte("[]\n"), 0o644)
		}
		if *outTimings != "" {
			// Tests assert that aggregation works; the actual numbers
			// don't matter, but the schema does.
			tbody, _ := json.MarshalIndent(map[string]any{
				"version":                 1,
				"cmake_configure_seconds": 1.5,
				"translation_seconds":     0.5,
				"total_seconds":           2.0,
			}, "", "  ")
			_ = os.MkdirAll(filepath.Dir(*outTimings), 0o755)
			_ = os.WriteFile(*outTimings, append(tbody, '\n'), 0o644)
		}
		return 0
	case "tier1":
		failBody, _ := json.MarshalIndent(map[string]any{
			"tier":    1,
			"code":    "configure-failed",
			"message": "stub Tier-1 failure for testing",
		}, "", "  ")
		_ = os.MkdirAll(filepath.Dir(*outFailure), 0o755)
		_ = os.WriteFile(*outFailure, append(failBody, '\n'), 0o644)
		return 1
	case "tier2":
		fmt.Fprintln(os.Stderr, "stub: Tier-2 unexpected error")
		return 65
	}
	return 70
}

// TestRun_StubSuccess: orchestrator drives the stub binary in success mode
// against the fdsdk-subset fixture and produces the expected outputs.
// The fixture has two kind:cmake elements: hello and uses-hello. Both
// should convert successfully under the stub. Topo order puts hello before
// uses-hello (uses-hello build-deps on hello).
func TestRun_StubSuccess(t *testing.T) {
	out := t.TempDir()
	res := runOrchestrator(t, out, "success")
	want := []string{"components/hello", "components/uses-hello"}
	if !sliceEqual(res.Converted, want) {
		t.Errorf("Converted = %v, want %v", res.Converted, want)
	}
	if len(res.Failed) != 0 {
		t.Errorf("Failed = %v, want []", res.Failed)
	}

	// Per-element artifacts staged for both. The stub names its bundle
	// after the source root's basename — which is now the shadow tree
	// path the orchestrator builds (last segment matches the element
	// name's last segment). cmake-config/ is in synth-prefix layout
	// (lib/cmake/<Pkg>/...) — mirrors what real convert-element writes
	// via synthprefix.BuildSlice.
	for _, want := range []string{
		"elements/components/hello/BUILD.bazel",
		"elements/components/hello/cmake-config/lib/cmake/hello/helloConfig.cmake",
		"elements/components/hello/read_paths.json",
		"elements/components/uses-hello/BUILD.bazel",
		"elements/components/uses-hello/cmake-config/lib/cmake/uses-hello/uses-helloConfig.cmake",
		"elements/components/uses-hello/read_paths.json",
	} {
		if _, err := os.Stat(filepath.Join(out, want)); err != nil {
			t.Errorf("missing %s: %v", want, err)
		}
	}

	// Global manifest lists both, sorted.
	body := mustReadFile(t, filepath.Join(out, "manifest", "converted.json"))
	for _, want := range []string{`"components/hello"`, `"components/uses-hello"`} {
		if !strings.Contains(string(body), want) {
			t.Errorf("converted.json missing %s entry: %s", want, body)
		}
	}
	if i, j := strings.Index(string(body), `"components/hello"`), strings.Index(string(body), `"components/uses-hello"`); i > j {
		t.Errorf("converted.json entries not sorted")
	}
	failBody := mustReadFile(t, filepath.Join(out, "manifest", "failures.json"))
	if !strings.Contains(string(failBody), `"elements": []`) && !strings.Contains(string(failBody), `"elements": null`) {
		t.Errorf("failures.json should be empty: %s", failBody)
	}
}

// TestRun_StubTier1: stub fails Tier-1 for every element; orchestrator
// records all of them and doesn't abort.
//
// Dependents of failed elements get code="dep-failed" (M3d-era polish:
// surfacing the root cause clearly instead of cascading
// configure-failed entries that hide the actual problem). hello fails
// configure-failed; uses-hello short-circuits with dep-failed pointing
// at hello.
func TestRun_StubTier1(t *testing.T) {
	out := t.TempDir()
	res := runOrchestrator(t, out, "tier1")
	if len(res.Converted) != 0 {
		t.Errorf("Converted = %v, want []", res.Converted)
	}
	if len(res.Failed) != 2 {
		t.Fatalf("Failed = %v, want 2 entries", res.Failed)
	}
	got := map[string]string{}
	for _, fr := range res.Failed {
		if fr.Tier != 1 {
			t.Errorf("Tier = %d, want 1: %+v", fr.Tier, fr)
		}
		got[fr.Element] = fr.Code
	}
	if got["components/hello"] != "configure-failed" {
		t.Errorf("hello.Code = %q, want configure-failed", got["components/hello"])
	}
	if got["components/uses-hello"] != "dep-failed" {
		t.Errorf("uses-hello.Code = %q, want dep-failed", got["components/uses-hello"])
	}

	body := mustReadFile(t, filepath.Join(out, "manifest", "failures.json"))
	for _, want := range []string{`"configure-failed"`, `"dep-failed"`} {
		if !strings.Contains(string(body), want) {
			t.Errorf("failures.json missing %s\n%s", want, body)
		}
	}
}

// TestRun_StubTier2: a converter crash bubbles up as an orchestrator-level
// error rather than landing in the failures registry.
func TestRun_StubTier2(t *testing.T) {
	out := t.TempDir()
	_, err := runOrchestratorRaw(t, out, "tier2")
	if err == nil {
		t.Fatal("expected Tier-2 to surface as orchestrator error")
	}
	if !strings.Contains(err.Error(), "components/hello") {
		t.Errorf("err = %v, want to mention element name", err)
	}
}

// TestRun_ImportsManifestForDownstream: the second element to convert
// (uses-hello) sees an --imports-manifest path containing hello's exports
// — the dep-export registry is propagating correctly.
func TestRun_ImportsManifestForDownstream(t *testing.T) {
	out := t.TempDir()
	rec := t.TempDir()
	t.Setenv("ORCHESTRATOR_STUB_RECORD_DIR", rec)
	res := runOrchestrator(t, out, "success")
	if len(res.Failed) != 0 {
		t.Fatalf("Failed = %v", res.Failed)
	}

	// hello converts first; its imports manifest should be empty since
	// `base` (its only dep) is kind:manual, not in the export registry.
	// The stub records the manifest contents to <basename>.imports.txt.
	helloRec := mustReadFile(t, filepath.Join(rec, "hello.imports.txt"))
	if len(helloRec) != 0 {
		t.Errorf("hello got --imports-manifest=%q, want empty (no cmake deps)", helloRec)
	}

	// uses-hello sees a non-empty imports.json.
	importsBody := mustReadFile(t, filepath.Join(rec, "uses-hello.imports.txt"))
	if len(importsBody) == 0 {
		t.Fatal("uses-hello did not receive --imports-manifest")
	}
	for _, want := range []string{
		`"version": 1`,
		`"name": "components/hello"`,
		`"cmake_target": "hello::hello"`,
		`"bazel_label": "//elements/components/hello:hello"`,
	} {
		if !strings.Contains(string(importsBody), want) {
			t.Errorf("imports.json missing %q\n%s", want, importsBody)
		}
	}
}

// TestRun_ActionCache_HitsOnSecondRun: a second orchestrator invocation
// against the same source tree + same converter binary reuses every
// element's outputs from the action-key cache instead of re-running the
// converter.
func TestRun_ActionCache_HitsOnSecondRun(t *testing.T) {
	out := t.TempDir()

	first := runOrchestrator(t, out, "success")
	if len(first.CacheHits) != 0 || len(first.CacheMisses) != 2 {
		t.Errorf("first run: hits=%v misses=%v, want 0/2", first.CacheHits, first.CacheMisses)
	}

	// Re-run with the same env into the same out dir. Every element's
	// inputs hash to the same key as before, so all hit cache.
	second := runOrchestrator(t, out, "success")
	if len(second.CacheMisses) != 0 || len(second.CacheHits) != 2 {
		t.Errorf("second run: hits=%v misses=%v, want 2/0", second.CacheHits, second.CacheMisses)
	}
	want := []string{"components/hello", "components/uses-hello"}
	if !sliceEqual(second.CacheHits, want) {
		t.Errorf("CacheHits = %v, want %v", second.CacheHits, want)
	}
	if !sliceEqual(second.Converted, want) {
		t.Errorf("Converted = %v, want %v", second.Converted, want)
	}
}

// TestRun_ActionCache_MissOnSourceEdit: editing an allowlisted file
// (CMakeLists.txt) shifts the shadow tree's hash and triggers a re-run.
// (Editing a non-allowlisted .c keeps the shadow byte-stable and would
// hit cache; that variant lives in step 7's determinism test where the
// invariance is the headline claim.)
func TestRun_ActionCache_MissOnSourceEdit(t *testing.T) {
	out := t.TempDir()
	_ = runOrchestrator(t, out, "success")

	// Touch CMakeLists.txt to add a comment. The shadow tree mirrors it
	// real-content so the hash flips.
	cm := "../../testdata/fdsdk-subset/files/uses-hello/CMakeLists.txt"
	abs, err := filepath.Abs(cm)
	if err != nil {
		t.Fatal(err)
	}
	orig, err := os.ReadFile(abs)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.WriteFile(abs, orig, 0o644) })

	bumped := append([]byte("# action-cache test bump\n"), orig...)
	if err := os.WriteFile(abs, bumped, 0o644); err != nil {
		t.Fatal(err)
	}

	second := runOrchestrator(t, out, "success")
	// hello's shadow is unchanged -> cache hit.
	// uses-hello's shadow CMakeLists changed -> cache miss.
	if !sliceContains(second.CacheHits, "components/hello") {
		t.Errorf("hello expected to cache-hit; got hits=%v misses=%v", second.CacheHits, second.CacheMisses)
	}
	if !sliceContains(second.CacheMisses, "components/uses-hello") {
		t.Errorf("uses-hello expected to cache-miss after CMakeLists edit; got hits=%v misses=%v", second.CacheHits, second.CacheMisses)
	}
}

func sliceContains(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

// TestRun_RejectsUnknownConverter: missing converter binary surfaces the
// PATH lookup error before iterating.
func TestRun_RejectsUnknownConverter(t *testing.T) {
	proj, g := mustLoadFixture(t)
	_, err := orchestrator.Run(context.Background(), orchestrator.Options{
		Project:         proj,
		Graph:           g,
		Out:             t.TempDir(),
		ConverterBinary: "/no/such/binary",
	})
	if err == nil || !strings.Contains(err.Error(), "/no/such/binary") {
		t.Errorf("err = %v, want PATH lookup failure", err)
	}
}

// ---- shared helpers --------------------------------------------------------

func runOrchestrator(t *testing.T, out, mode string) *orchestrator.Result {
	t.Helper()
	res, err := runOrchestratorRaw(t, out, mode)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return res
}

func runOrchestratorRaw(t *testing.T, out, mode string) (*orchestrator.Result, error) {
	t.Helper()
	proj, g := mustLoadFixture(t)

	// Self-as-stub: re-invoke the test binary with ORCHESTRATOR_STUB_CONVERTER=1.
	stub := os.Args[0]
	t.Setenv("ORCHESTRATOR_STUB_CONVERTER", "1")
	t.Setenv("ORCHESTRATOR_STUB_MODE", mode)

	return orchestrator.Run(context.Background(), orchestrator.Options{
		Project:         proj,
		Graph:           g,
		Out:             out,
		ConverterBinary: stub,
		Log:             testLog{t},
	})
}

func mustLoadFixture(t *testing.T) (*element.Project, *element.Graph) {
	t.Helper()
	root, err := filepath.Abs("../../testdata/fdsdk-subset")
	if err != nil {
		t.Fatal(err)
	}
	proj, err := element.ReadProject(root, "elements")
	if err != nil {
		t.Fatal(err)
	}
	g, err := element.BuildGraph(proj)
	if err != nil {
		t.Fatal(err)
	}
	return proj, g
}

func mustReadFile(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read %s: %v", p, err)
	}
	return b
}

func sliceEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

type testLog struct{ t *testing.T }

func (l testLog) Write(p []byte) (int, error) {
	l.t.Logf("%s", strings.TrimRight(string(p), "\n"))
	return len(p), nil
}

// keep runtime imported for readability of platform-conditional debugging
// when the stub binary detection ever needs it; vet is happy with the
// blank import.
var _ = runtime.GOOS

// sanitizeStubKey turns a path into the alphanumeric-uppercase form
// used by ORCHESTRATOR_STUB_MODE_<...> env vars. The element name lives
// in --source-root's last segments; for a path like
//
//	/tmp/.../shadow/components/hello
//
// we use "components/hello" (the trailing two segments) and emit
// "COMPONENTS_HELLO".
//
// The orchestrator builds shadow trees at <out>/shadow/<element-name>/
// so element-name is always the path relative to that root. We can't
// know <out> here, so we conservatively use the last two segments
// joined; a single-segment element name (rare) falls back to that
// segment alone.
func sanitizeStubKey(srcRoot string) string {
	parts := strings.Split(filepath.ToSlash(srcRoot), "/")
	// Take the last two segments to form "components/hello".
	tail := parts
	if len(parts) >= 2 {
		tail = parts[len(parts)-2:]
	}
	joined := strings.Join(tail, "/")
	var b strings.Builder
	for i := 0; i < len(joined); i++ {
		c := joined[i]
		switch {
		case c >= 'a' && c <= 'z':
			b.WriteByte(c - 'a' + 'A')
		case c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
			b.WriteByte(c)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}
