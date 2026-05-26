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

func TestParse_DuplicateAddTestNamesLastWins(t *testing.T) {
	// CMake itself rejects duplicate names at configure time, but if
	// one slips through (e.g. a hand-edited testfile) the parser
	// shouldn't crash — last write wins for set_tests_properties.
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
	if len(all) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(all))
	}
	// byName index points to the second registration; only it gets the
	// TIMEOUT property.
	if all[1].Timeout != 5*time.Second {
		t.Errorf("second dup should have TIMEOUT 5s, got %v", all[1].Timeout)
	}
	if all[0].Timeout != 0 {
		t.Errorf("first dup should be untouched, got %v", all[0].Timeout)
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
