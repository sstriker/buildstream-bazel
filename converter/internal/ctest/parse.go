// Package ctest parses CTestTestfile.cmake (the file `cmake configure`
// writes into the build directory next to build.ninja) and surfaces
// each `add_test()` registration plus its `set_tests_properties()`
// metadata. The parsed Registry is what the lower stage consults to
// classify EXECUTABLE targets as cc_test instead of cc_binary.
//
// The CMake File API does NOT expose test data — codemodel-v2 lists
// targets without the add_test() side. CTestTestfile.cmake is the
// only place we can recover it from, and only after `cmake configure`
// has run.
package ctest

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Test is one add_test() registration after set_tests_properties()
// merges. Multiple Test entries may share the same Target when a single
// executable is registered for several test cases (one per add_test).
type Test struct {
	// Name is the add_test NAME — globally unique within the project.
	Name string
	// Target is the executable target name (basename of the resolved
	// COMMAND path, with any platform suffix like ".exe" stripped).
	Target string
	// Args are the positional arguments after COMMAND. Empty for
	// gtest_discover_tests placeholders since the binary itself runs
	// gtest's case loop at test time.
	Args []string
	// Timeout is set_tests_properties TIMEOUT, or 0 if unset.
	Timeout time.Duration
	// Env is set_tests_properties ENVIRONMENT, split on `;`.
	Env []string
	// Tags carries LABELS, plus "manual" if DISABLED is truthy and
	// "exclusive" if RUN_SERIAL is truthy. gtest_discover_tests
	// placeholders also get a "gtest_discover_tests" tag for
	// operator visibility.
	Tags []string
	// Data carries REQUIRED_FILES (split on `;`).
	Data []string
}

// Registry indexes parsed Tests by executable target name, preserving
// registration order both within and across CTestTestfile.cmake files.
type Registry struct {
	tests    []Test
	byTarget map[string][]int
	byName   map[string]int // for set_tests_properties enrichment during parse
}

// Lookup returns every test registered against the given executable
// target name. Returns nil when the target has no test registrations.
func (r *Registry) Lookup(target string) []Test {
	if r == nil {
		return nil
	}
	idx := r.byTarget[target]
	if len(idx) == 0 {
		return nil
	}
	out := make([]Test, len(idx))
	for i, n := range idx {
		out[i] = r.tests[n]
	}
	return out
}

// All returns every parsed Test in registration order. Used by the
// lower stage to surface non-binary tests as warnings (registered but
// no matching EXECUTABLE).
func (r *Registry) All() []Test {
	if r == nil {
		return nil
	}
	out := make([]Test, len(r.tests))
	copy(out, r.tests)
	return out
}

// Parse walks <buildDir>/CTestTestfile.cmake and recursively follows
// subdirs(...) entries. A missing top-level file is not an error —
// projects that don't enable_testing() simply have no Registry —
// it returns an empty Registry.
func Parse(buildDir string) (*Registry, error) {
	r := &Registry{
		byTarget: map[string][]int{},
		byName:   map[string]int{},
	}
	top := filepath.Join(buildDir, "CTestTestfile.cmake")
	if _, err := os.Stat(top); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return r, nil
		}
		return nil, err
	}
	if err := r.parseFile(top); err != nil {
		return nil, err
	}
	return r, nil
}

func (r *Registry) parseFile(path string) error {
	body, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("ctest: read %s: %w", path, err)
	}
	calls, err := scanCalls(body)
	if err != nil {
		return fmt.Errorf("ctest: parse %s: %w", path, err)
	}
	dir := filepath.Dir(path)
	for _, c := range calls {
		switch c.name {
		case "add_test":
			r.handleAddTest(c.args)
		case "set_tests_properties":
			r.handleSetProperties(c.args)
		case "subdirs":
			if err := r.handleSubdirs(dir, c.args); err != nil {
				return err
			}
		case "include":
			r.handleInclude(c.args)
		}
	}
	return nil
}

func (r *Registry) handleAddTest(args []string) {
	if len(args) < 2 {
		return
	}
	name := args[0]
	cmd := args[1]
	// Under the Ninja Multi-Config generator, cmake wraps each test
	// in per-configuration branches and emits one add_test per
	// configuration plus a NOT_AVAILABLE fallback:
	//
	//   if(CTEST_CONFIGURATION_TYPE MATCHES "Debug")
	//     add_test(BVH_1 ".../Debug/BVH_1")
	//   elseif(... MATCHES "Release")
	//     add_test(BVH_1 ".../Release/BVH_1")
	//   else()
	//     add_test(BVH_1 NOT_AVAILABLE)
	//   endif()
	//
	// scanCalls flattens the if/elseif/else, so the same test name
	// reaches us once per configuration. At runtime ctest evaluates
	// exactly one branch by the active config, so the test is a
	// SINGLE test — emitting one cc_test per branch produces
	// duplicate Bazel target names and a hard convert failure. A
	// test NAME is globally unique within a CTest project, so a
	// repeat is always the multi-config branch shape (or a
	// hand-edited dup): keep the first real registration and ignore
	// the rest. The NOT_AVAILABLE branch is cmake's "this config has
	// no runnable binary" sentinel, not a real command — never a
	// cc_test.
	if cmd == "NOT_AVAILABLE" {
		return
	}
	newArgs := append([]string(nil), args[2:]...)
	if prev, dup := r.byName[name]; dup {
		// A repeated name is the multi-config branch shape: the same
		// logical test re-emitted once per configuration. We keep the
		// first registration and drop the rest. The branches normally
		// differ only in cmake's per-config artifact PATH (the COMMAND,
		// which Bazel manages itself) — but a test's ARGS can in
		// principle vary per config (e.g. an arg embedding $<CONFIG> or
		// a per-config value). Collapsing such a test to one cc_test
		// would silently lose that divergence, so detect it: compare the
		// arg tails path-normalized, and if they materially differ, tag
		// the kept test so the lift surfaces the gap instead of dropping
		// it silently. (COMMAND-path-only differences carry no Bazel
		// intent and are not flagged.)
		if !argsEquivalent(r.tests[prev].Args, newArgs) {
			r.tests[prev].Tags = appendUniq(r.tests[prev].Tags,
				"cmake-test-per-config-args-diverge")
		}
		return
	}
	target := strings.TrimSuffix(filepath.Base(cmd), ".exe")
	t := Test{
		Name:   name,
		Target: target,
		Args:   newArgs,
	}
	r.byName[name] = len(r.tests)
	r.byTarget[target] = append(r.byTarget[target], len(r.tests))
	r.tests = append(r.tests, t)
}

// argsEquivalent reports whether two add_test argument tails carry the
// same Bazel-relevant intent. Per-config CTestTestfile branches differ
// in cmake's artifact output directory (Debug/ vs Release/ vs
// RelWithDebInfo/ vs MinSizeRel/), which is meaningless to Bazel, so
// those tokens are normalized away before comparison. Anything else
// differing means the test genuinely varies per configuration.
func argsEquivalent(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if normalizeConfigToken(a[i]) != normalizeConfigToken(b[i]) {
			return false
		}
	}
	return true
}

// configNames are the standard CMAKE_CONFIGURATION_TYPES values that
// vary between a multi-config CTestTestfile's per-configuration
// branches. Both forms must normalize away before comparison:
//   - a /<config>/ artifact path segment (e.g. .../Debug/foo), and
//   - a bare config token, which appears when the test is a cmake
//     rebuild invocation: `cmake --build . --target T --config Debug`
//     (Eigen's ei_add_failtest shape). Both are cmake's own per-config
//     plumbing, not Bazel-relevant test intent.
var configNames = []string{"Debug", "Release", "RelWithDebInfo", "MinSizeRel"}

// normalizeConfigToken collapses a single add_test arg token's
// per-config variance: a /<config>/ path segment, or a token that is
// exactly a config name (optionally double-quoted, as CTestTestfile
// emits). Anything left differing after this is genuine per-config
// test intent.
func normalizeConfigToken(s string) string {
	for _, c := range configNames {
		s = strings.ReplaceAll(s, "/"+c+"/", "/<CONFIG>/")
		if s == c || s == `"`+c+`"` {
			return "<CONFIG>"
		}
	}
	return s
}

func (r *Registry) handleSetProperties(args []string) {
	// set_tests_properties(<name> [<name> ...] PROPERTIES <key> <value> ...)
	// In CTestTestfile.cmake there's always exactly one name; spec
	// allows multiple but ctest never emits that shape.
	if len(args) < 4 {
		return
	}
	// Find PROPERTIES sentinel.
	pi := -1
	for i, a := range args {
		if a == "PROPERTIES" {
			pi = i
			break
		}
	}
	if pi < 0 {
		return
	}
	names := args[:pi]
	kvs := args[pi+1:]
	if len(kvs)%2 != 0 {
		return
	}
	for _, name := range names {
		idx, ok := r.byName[name]
		if !ok {
			continue
		}
		t := &r.tests[idx]
		for i := 0; i < len(kvs); i += 2 {
			applyProperty(t, kvs[i], kvs[i+1])
		}
		// Re-index by-target since handleAddTest captured the post-
		// strip name; properties don't change Target. The byTarget
		// slice already points at this index.
	}
}

func applyProperty(t *Test, key, value string) {
	switch key {
	case "TIMEOUT":
		secs, err := strconv.ParseFloat(value, 64)
		if err == nil && secs > 0 {
			t.Timeout = time.Duration(secs * float64(time.Second))
		}
	case "ENVIRONMENT":
		t.Env = appendSplitNonEmpty(t.Env, value, ';')
	case "LABELS":
		t.Tags = appendSplitNonEmpty(t.Tags, value, ';')
	case "REQUIRED_FILES":
		t.Data = appendSplitNonEmpty(t.Data, value, ';')
	case "DISABLED":
		if isCMakeTruthy(value) {
			t.Tags = appendUniq(t.Tags, "manual")
		}
	case "RUN_SERIAL":
		if isCMakeTruthy(value) {
			t.Tags = appendUniq(t.Tags, "exclusive")
		}
	case "WILL_FAIL":
		// Bazel cc_test has no native "expected to fail" semantic.
		// The standard equivalent is to wrap with sh_test that
		// inverts the exit code. Surface as a tag so operators
		// can find affected tests via grep.
		if isCMakeTruthy(value) {
			t.Tags = appendUniq(t.Tags, "cmake-test-will-fail")
		}
	case "WORKING_DIRECTORY":
		// Bazel cc_test has no native working_directory attribute.
		// The standard pattern uses a sh wrapper that cd's. Tag
		// surfaces the cmake-side WORKING_DIRECTORY so operators
		// know to wrap or set it via TEST_CWD env var.
		if v := strings.TrimSpace(value); v != "" {
			t.Tags = appendUniq(t.Tags, "cmake-test-cwd="+v)
		}
	case "SKIP_REGULAR_EXPRESSION":
		// Stdout/stderr pattern that, when matched, marks the
		// test as skipped. Bazel test frameworks handle skip via
		// language-specific protocols. Surface as tag.
		if v := strings.TrimSpace(value); v != "" {
			t.Tags = appendUniq(t.Tags, "cmake-test-skip-regex="+v)
		}
	case "FAIL_REGULAR_EXPRESSION":
		// Inverse of PASS_REGULAR_EXPRESSION — pattern that, if
		// seen in stdout/stderr, fails the test even on a 0 exit.
		if v := strings.TrimSpace(value); v != "" {
			t.Tags = appendUniq(t.Tags, "cmake-test-fail-regex="+v)
		}
	case "PASS_REGULAR_EXPRESSION":
		if v := strings.TrimSpace(value); v != "" {
			t.Tags = appendUniq(t.Tags, "cmake-test-pass-regex="+v)
		}
	}
}

func (r *Registry) handleSubdirs(dir string, args []string) error {
	for _, sub := range args {
		next := filepath.Join(dir, sub, "CTestTestfile.cmake")
		if _, err := os.Stat(next); err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return err
		}
		if err := r.parseFile(next); err != nil {
			return err
		}
	}
	return nil
}

func (r *Registry) handleInclude(args []string) {
	// gtest_discover_tests writes
	//   include("<binary>_tests-NotInstalled.cmake" OPTIONAL)
	// into CTestTestfile.cmake at configure time. The included file
	// only exists post-build, so we don't read it; we synthesize one
	// Test for the binary so cc_test gets emitted.
	if len(args) == 0 {
		return
	}
	const suffix = "_tests-NotInstalled.cmake"
	first := args[0]
	base := filepath.Base(first)
	if !strings.HasSuffix(base, suffix) {
		return
	}
	binary := strings.TrimSuffix(base, suffix)
	if binary == "" {
		return
	}
	if _, dup := r.byName[binary]; dup {
		return
	}
	t := Test{
		Name:   binary,
		Target: binary,
		Tags:   []string{"gtest_discover_tests"},
	}
	r.byName[binary] = len(r.tests)
	r.byTarget[binary] = append(r.byTarget[binary], len(r.tests))
	r.tests = append(r.tests, t)
}

// isCMakeTruthy mirrors CMake's quirky truthy set: ON, TRUE, Y, YES,
// non-zero numbers. Case-insensitive.
func isCMakeTruthy(s string) bool {
	switch strings.ToUpper(s) {
	case "1", "ON", "TRUE", "Y", "YES":
		return true
	}
	return false
}

func appendSplitNonEmpty(dst []string, s string, sep byte) []string {
	for _, p := range strings.Split(s, string(sep)) {
		if p == "" {
			continue
		}
		dst = append(dst, p)
	}
	return dst
}

func appendUniq(dst []string, s string) []string {
	for _, e := range dst {
		if e == s {
			return dst
		}
	}
	return append(dst, s)
}
