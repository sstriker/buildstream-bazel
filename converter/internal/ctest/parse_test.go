package ctest

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

// fixtureBody mirrors the shape `cmake configure` actually emits into
// CTestTestfile.cmake — verified against cmake 3.28. Synthetic so the
// test doesn't require cmake on PATH.
const fixtureTopBody = `# CMake generated Testfile for
# Source directory: /tmp/x
# Build directory: /tmp/x/build
add_test([=[ok]=] "/tmp/x/build/ok")
set_tests_properties([=[ok]=] PROPERTIES  _BACKTRACE_TRIPLES "...")
add_test([=[slow]=] "/tmp/x/build/slow" "--slow")
set_tests_properties([=[slow]=] PROPERTIES  ENVIRONMENT "FOO=1;BAR=2" LABELS "slow;flaky" REQUIRED_FILES "data.txt" RUN_SERIAL "TRUE" TIMEOUT "30" _BACKTRACE_TRIPLES "...")
add_test([=[param-a]=] "/tmp/x/build/parametric" "--case=a")
set_tests_properties([=[param-a]=] PROPERTIES  DISABLED "TRUE" _BACKTRACE_TRIPLES "...")
add_test([=[param-b]=] "/tmp/x/build/parametric" "--case=b")
subdirs("sub")
include("gt_tests-NotInstalled.cmake" OPTIONAL)
`

const fixtureSubBody = `# generated
add_test([=[sub-test]=] "/tmp/x/build/sub/sub_test")
`

func TestParse_Empty(t *testing.T) {
	dir := t.TempDir()
	r, err := Parse(dir)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if r == nil {
		t.Fatal("Parse returned nil registry")
	}
	if len(r.All()) != 0 {
		t.Errorf("expected empty registry, got %d tests", len(r.All()))
	}
}

func TestParse_FullFixture(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "CTestTestfile.cmake"), fixtureTopBody)
	mustWrite(t, filepath.Join(dir, "sub", "CTestTestfile.cmake"), fixtureSubBody)

	r, err := Parse(dir)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	all := r.All()
	if len(all) != 6 {
		t.Fatalf("expected 6 tests (4 add_test + 1 subdir + 1 gtest_discover), got %d: %+v", len(all), all)
	}

	// ok: bare add_test
	got := mustLookup(t, r, "ok")
	if got.Name != "ok" || got.Target != "ok" || len(got.Args) != 0 {
		t.Errorf("ok = %+v", got)
	}

	// slow: full property surface
	got = mustLookup(t, r, "slow")
	want := Test{
		Name:    "slow",
		Target:  "slow",
		Args:    []string{"--slow"},
		Command: []string{"/tmp/x/build/slow", "--slow"},
		Timeout: 30 * time.Second,
		Env:     []string{"FOO=1", "BAR=2"},
		Tags:    []string{"slow", "flaky", "exclusive"},
		Data:    []string{"data.txt"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("slow = %+v, want %+v", got, want)
	}

	// param-a: DISABLED → tags["manual"]
	got = mustLookup(t, r, "parametric")
	tests := r.Lookup("parametric")
	if len(tests) != 2 {
		t.Fatalf("expected 2 parametric tests, got %d", len(tests))
	}
	a := tests[0]
	if a.Name != "param-a" || !contains(a.Tags, "manual") || !equalSlice(a.Args, []string{"--case=a"}) {
		t.Errorf("param-a = %+v", a)
	}
	b := tests[1]
	if b.Name != "param-b" || contains(b.Tags, "manual") || !equalSlice(b.Args, []string{"--case=b"}) {
		t.Errorf("param-b = %+v", b)
	}
	_ = got // satisfy unused

	// sub-test: parsed from subdirs("sub")
	got = mustLookup(t, r, "sub_test")
	if got.Name != "sub-test" {
		t.Errorf("sub-test missing or named wrong: %+v", got)
	}

	// gt: gtest_discover_tests placeholder
	got = mustLookup(t, r, "gt")
	if !contains(got.Tags, "gtest_discover_tests") {
		t.Errorf("gtest_discover_tests synthetic tag missing: %+v", got)
	}
	if len(got.Args) != 0 {
		t.Errorf("gtest_discover_tests Test should have no Args, got %v", got.Args)
	}
}

func TestParse_MissingSubdirIsHarmless(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "CTestTestfile.cmake"), `subdirs("nonexistent")`+"\n")
	r, err := Parse(dir)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(r.All()) != 0 {
		t.Errorf("expected empty registry, got %d", len(r.All()))
	}
}

func TestParse_BareAddTestNoCommand(t *testing.T) {
	// Defensive: cmake never emits this shape, but the parser shouldn't
	// crash if a hand-rolled fixture omits COMMAND.
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "CTestTestfile.cmake"), `add_test([=[bad]=])`+"\n")
	r, err := Parse(dir)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(r.All()) != 0 {
		t.Errorf("malformed add_test should be skipped, got %d", len(r.All()))
	}
}

func TestParse_DoubleQuotedName(t *testing.T) {
	// Older cmake versions emitted double-quoted names instead of
	// bracket-quoted; the parser should accept both.
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "CTestTestfile.cmake"),
		`add_test("dq-test" "/tmp/x/build/dq")`+"\n")
	r, err := Parse(dir)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	tests := r.Lookup("dq")
	if len(tests) != 1 || tests[0].Name != "dq-test" {
		t.Errorf("double-quoted name not parsed: %+v", r.All())
	}
}

func TestParse_ExeSuffixStripped(t *testing.T) {
	// Windows-style executable paths in the COMMAND. The .exe suffix
	// gets stripped so Lookup("foo") works regardless of host.
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "CTestTestfile.cmake"),
		`add_test([=[w]=] "C:/build/foo.exe")`+"\n")
	r, err := Parse(dir)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(r.Lookup("foo")) != 1 {
		t.Errorf("expected target 'foo' after stripping .exe, got %+v", r.byTarget)
	}
}

func TestParse_DuplicateAddTestNamesFirstWins(t *testing.T) {
	// A test NAME is globally unique within a CTest project, so a
	// repeated add_test(<name> ...) is always the Ninja Multi-Config
	// per-configuration branch shape (one add_test per config for the
	// same logical test) — or a hand-edited dup. Either way it's a
	// SINGLE test: the parser keeps the first registration and ignores
	// the rest, so the lower stage emits exactly one cc_test instead
	// of N colliding ones. set_tests_properties applies to the kept
	// (first) entry.
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "CTestTestfile.cmake"),
		`add_test([=[dup]=] "/tmp/x/a")`+"\n"+
			`add_test([=[dup]=] "/tmp/x/b")`+"\n"+
			`set_tests_properties([=[dup]=] PROPERTIES TIMEOUT "5")`+"\n")
	r, err := Parse(dir)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	all := r.All()
	if len(all) != 1 {
		t.Fatalf("expected 1 deduped entry, got %d", len(all))
	}
	// The kept (first) entry carries the first command and the property.
	if all[0].Target != "a" {
		t.Errorf("kept entry should be the first registration (target a), got %q", all[0].Target)
	}
	if all[0].Timeout != 5*time.Second {
		t.Errorf("kept entry should have TIMEOUT 5s, got %v", all[0].Timeout)
	}
}

func TestParse_MultiConfigBranchesCollapseToOneTest(t *testing.T) {
	// The exact Ninja Multi-Config CTestTestfile shape: per-config
	// add_test branches plus a NOT_AVAILABLE fallback, flattened by
	// the scanner. Must yield ONE test (not three), and the
	// NOT_AVAILABLE sentinel must never become a test.
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "CTestTestfile.cmake"),
		`if(CTEST_CONFIGURATION_TYPE MATCHES "Debug")`+"\n"+
			`  add_test(BVH_1 "/b/Debug/BVH_1")`+"\n"+
			`  set_tests_properties(BVH_1 PROPERTIES LABELS "Unsupported")`+"\n"+
			`elseif(CTEST_CONFIGURATION_TYPE MATCHES "Release")`+"\n"+
			`  add_test(BVH_1 "/b/Release/BVH_1")`+"\n"+
			`  set_tests_properties(BVH_1 PROPERTIES LABELS "Unsupported")`+"\n"+
			`else()`+"\n"+
			`  add_test(BVH_1 NOT_AVAILABLE)`+"\n"+
			`endif()`+"\n")
	r, err := Parse(dir)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	got := r.Lookup("BVH_1")
	if len(got) != 1 {
		t.Fatalf("multi-config branches must collapse to ONE test; got %d: %+v", len(got), got)
	}
	if got[0].Name != "BVH_1" || got[0].Target != "BVH_1" {
		t.Errorf("unexpected collapsed test: %+v", got[0])
	}
	// All() must also be 1 — the NOT_AVAILABLE branch produced no test.
	if n := len(r.All()); n != 1 {
		t.Fatalf("expected 1 total test, got %d", n)
	}
}

func TestParse_MultiConfigPathOnlyDifferenceNotFlagged(t *testing.T) {
	// The common case: per-config branches differ ONLY in the artifact
	// path (Debug/ vs Release/). That carries no Bazel intent, so the
	// collapsed test must NOT get the divergence tag.
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "CTestTestfile.cmake"),
		`if(CTEST_CONFIGURATION_TYPE MATCHES "Debug")`+"\n"+
			`  add_test(t1 "/b/Debug/t1" --flag x)`+"\n"+
			`elseif(CTEST_CONFIGURATION_TYPE MATCHES "Release")`+"\n"+
			`  add_test(t1 "/b/Release/t1" --flag x)`+"\n"+
			`endif()`+"\n")
	r, err := Parse(dir)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	got := r.Lookup("t1")
	if len(got) != 1 {
		t.Fatalf("want 1 collapsed test, got %d", len(got))
	}
	if contains(got[0].Tags, "cmake-test-per-config-args-diverge") {
		t.Errorf("path-only difference must NOT flag divergence; tags=%v", got[0].Tags)
	}
}

func TestParse_MultiConfigArgDivergenceFlagged(t *testing.T) {
	// A test whose ARGS genuinely differ per config (beyond the
	// artifact path) must surface the divergence tag rather than
	// silently dropping the non-first config's args.
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "CTestTestfile.cmake"),
		`if(CTEST_CONFIGURATION_TYPE MATCHES "Debug")`+"\n"+
			`  add_test(t1 "/b/Debug/t1" --mode debug)`+"\n"+
			`elseif(CTEST_CONFIGURATION_TYPE MATCHES "Release")`+"\n"+
			`  add_test(t1 "/b/Release/t1" --mode release)`+"\n"+
			`endif()`+"\n")
	r, err := Parse(dir)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	got := r.Lookup("t1")
	if len(got) != 1 {
		t.Fatalf("want 1 collapsed test, got %d", len(got))
	}
	if !contains(got[0].Tags, "cmake-test-per-config-args-diverge") {
		t.Errorf("per-config arg divergence must be flagged; tags=%v", got[0].Tags)
	}
	// First registration's args are the ones kept.
	if !equalSlice(got[0].Args, []string{"--mode", "debug"}) {
		t.Errorf("kept args should be the first config's; got %v", got[0].Args)
	}
}

func TestParse_NotAvailableOnlyYieldsNoTest(t *testing.T) {
	// A test that is NOT_AVAILABLE in every branch (no runnable binary
	// for any configuration) must not surface as a cc_test.
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "CTestTestfile.cmake"),
		`add_test(ghost NOT_AVAILABLE)`+"\n")
	r, err := Parse(dir)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if n := len(r.All()); n != 0 {
		t.Fatalf("NOT_AVAILABLE-only test must yield no entries; got %d", n)
	}
}

func TestIsCMakeTruthy(t *testing.T) {
	for _, in := range []string{"1", "ON", "on", "TRUE", "True", "Y", "YES", "yes"} {
		if !isCMakeTruthy(in) {
			t.Errorf("isCMakeTruthy(%q) = false, want true", in)
		}
	}
	for _, in := range []string{"0", "OFF", "FALSE", "N", "NO", ""} {
		if isCMakeTruthy(in) {
			t.Errorf("isCMakeTruthy(%q) = true, want false", in)
		}
	}
}

// helpers

func mustWrite(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func mustLookup(t *testing.T, r *Registry, target string) Test {
	t.Helper()
	tests := r.Lookup(target)
	if len(tests) == 0 {
		t.Fatalf("Lookup(%q) returned no tests; registry: %+v", target, r.All())
	}
	return tests[0]
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

func equalSlice(a, b []string) bool {
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

func TestParse_AdditionalProperties(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "CTestTestfile.cmake"),
		`add_test([=[wf]=] "exe")`+"\n"+
			`set_tests_properties([=[wf]=] PROPERTIES WILL_FAIL "TRUE")`+"\n"+
			`add_test([=[cwd]=] "exe")`+"\n"+
			`set_tests_properties([=[cwd]=] PROPERTIES WORKING_DIRECTORY "/tmp")`+"\n"+
			`add_test([=[skipre]=] "exe")`+"\n"+
			`set_tests_properties([=[skipre]=] PROPERTIES SKIP_REGULAR_EXPRESSION "SKIP:.*")`+"\n",
	)
	r, err := Parse(dir)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	wantTags := []string{
		"cmake-test-will-fail",
		"cmake-test-cwd=/tmp",
		"cmake-test-skip-regex=SKIP:.*",
	}
	for _, w := range wantTags {
		found := false
		for _, te := range r.All() {
			for _, tag := range te.Tags {
				if tag == w {
					found = true
				}
			}
		}
		if !found {
			t.Errorf("missing tag %q in any test", w)
		}
	}
}

// An unrecognized set_tests_properties key is surfaced as a
// cmake-test-unhandled-prop=<key> tag rather than vanishing silently (the
// no-silent-drops contract). A KNOWN key on the same test must NOT produce
// the tag.
func TestParse_UnhandledPropertySurfaced(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "CTestTestfile.cmake"),
		`add_test([=[t]=] "exe")`+"\n"+
			`set_tests_properties([=[t]=] PROPERTIES PROCESSORS "4" LABELS "fast")`+"\n",
	)
	r, err := Parse(dir)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	got := r.All()
	if len(got) != 1 {
		t.Fatalf("want 1 test, got %d", len(got))
	}
	if !contains(got[0].Tags, "cmake-test-unhandled-prop=PROCESSORS") {
		t.Errorf("unknown key PROCESSORS not surfaced; tags=%v", got[0].Tags)
	}
	// LABELS is modeled (→ Tags), so it must not be tagged as unhandled.
	if contains(got[0].Tags, "cmake-test-unhandled-prop=LABELS") {
		t.Errorf("known key LABELS wrongly tagged unhandled; tags=%v", got[0].Tags)
	}
}

// A malformed TIMEOUT value is surfaced as a tag (the test would otherwise
// silently get no timeout); a well-formed one sets Timeout and emits no tag.
func TestParse_TimeoutUnparsedSurfaced(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "CTestTestfile.cmake"),
		`add_test([=[bad]=] "exe")`+"\n"+
			`set_tests_properties([=[bad]=] PROPERTIES TIMEOUT "not-a-number")`+"\n"+
			`add_test([=[ok]=] "exe")`+"\n"+
			`set_tests_properties([=[ok]=] PROPERTIES TIMEOUT "30")`+"\n",
	)
	r, err := Parse(dir)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	byName := map[string]Test{}
	for _, te := range r.All() {
		byName[te.Name] = te
	}
	if !contains(byName["bad"].Tags, "cmake-test-timeout-unparsed") {
		t.Errorf("malformed TIMEOUT not surfaced; tags=%v", byName["bad"].Tags)
	}
	if byName["bad"].Timeout != 0 {
		t.Errorf("malformed TIMEOUT should leave Timeout zero; got %v", byName["bad"].Timeout)
	}
	if byName["ok"].Timeout != 30*time.Second {
		t.Errorf("well-formed TIMEOUT = %v, want 30s", byName["ok"].Timeout)
	}
	if contains(byName["ok"].Tags, "cmake-test-timeout-unparsed") {
		t.Errorf("well-formed TIMEOUT wrongly tagged; tags=%v", byName["ok"].Tags)
	}
}

func TestResolveWrappedCommands(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "CTestTestfile.cmake"),
		// wrapped: a python launcher + runner script in front of the driver
		// exe. The driver token basename ("bsl_foo.t") is matched against the
		// EXECUTABLE target set the same way the direct path matches COMMAND[0].
		`add_test([=[bsl_foo]=] "/usr/bin/python3" "/bde/runner.py" "/b/groups/bsl/bsl_foo.t" "--verbose")`+"\n"+
			// direct: COMMAND[0] is the executable itself — must stay untouched
			`add_test([=[plain]=] "/b/plain")`+"\n"+
			// wrapped but no token names a known executable — left as-is
			`add_test([=[orphan]=] "/usr/bin/python3" "/bde/runner.py" "missing.t")`+"\n",
	)
	r, err := Parse(dir)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	// Before resolution the wrapped driver is indexed under "python3".
	if len(r.Lookup("bsl_foo.t")) != 0 {
		t.Fatal("precondition: wrapped driver should not match before resolution")
	}

	exec := map[string]bool{"bsl_foo.t": true, "plain": true}
	r.ResolveWrappedCommands(func(name string) bool { return exec[name] })

	got := mustLookup(t, r, "bsl_foo.t")
	if got.Name != "bsl_foo" || got.Target != "bsl_foo.t" {
		t.Errorf("wrapped driver not re-pointed: %+v", got)
	}
	if !equalSlice(got.Args, []string{"--verbose"}) {
		t.Errorf("driver Args should be the tokens after the driver, got %v", got.Args)
	}
	if !contains(got.Tags, "cmake-test-launcher=python3") {
		t.Errorf("launcher tag missing: %v", got.Tags)
	}
	// The resolved driver is no longer indexed under the launcher: only the
	// unresolved orphan remains under "python3".
	py := r.Lookup("python3")
	if len(py) != 1 || py[0].Name != "orphan" {
		t.Errorf("python3 index should hold only the orphan after re-pointing, got %+v", py)
	}

	// The direct registration is unchanged.
	plain := mustLookup(t, r, "plain")
	if plain.Target != "plain" || contains(plain.Tags, "cmake-test-launcher=python3") {
		t.Errorf("direct test wrongly rewritten: %+v", plain)
	}

	// The orphan (no resolvable driver) is left untouched — still its launcher
	// basename, no unwrap tag, so it never becomes a runnable test target.
	for _, te := range r.All() {
		if te.Name != "orphan" {
			continue
		}
		if te.Target != "python3" || contains(te.Tags, "cmake-test-launcher=python3") {
			t.Errorf("orphan should be untouched (Target=python3, no tag), got %+v", te)
		}
	}
}

func TestResolveWrappedCommands_NilSafe(t *testing.T) {
	var r *Registry
	r.ResolveWrappedCommands(func(string) bool { return true }) // must not panic
	r2 := &Registry{byTarget: map[string][]int{}, byName: map[string]int{}}
	r2.ResolveWrappedCommands(nil) // nil predicate is a no-op
}
