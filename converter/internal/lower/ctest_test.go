package lower_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sstriker/buildstream-bazel/converter/emit/bazel"
	"github.com/sstriker/buildstream-bazel/converter/internal/ctest"
	"github.com/sstriker/buildstream-bazel/converter/internal/fileapi"
	"github.com/sstriker/buildstream-bazel/converter/internal/lower"
	"github.com/sstriker/buildstream-bazel/converter/ir"
)

// TestToIR_CTest_ClassifiesExecutableAsTest covers the keystone:
// when the CTest registry knows about an EXECUTABLE target, lower
// emits cc_test instead of cc_binary, with TestArgs/TestTimeout/
// TestEnv/TestData/Tags lifted from the registration.
func TestToIR_CTest_ClassifiesExecutableAsTest(t *testing.T) {
	r := &fileapi.Reply{
		Codemodel: fileapi.Codemodel{
			Configurations: []fileapi.Configuration{{
				Name: "Release",
				Targets: []fileapi.ConfigTargetRef{
					{Name: "format-test", Id: "format-test::@a"},
					{Name: "lib", Id: "lib::@b"},
				},
			}},
		},
		Targets: map[string]fileapi.Target{
			"format-test::@a": {
				Name:    "format-test",
				Type:    "EXECUTABLE",
				Sources: []fileapi.TargetSource{{Path: "format-test.cc", CompileGroupIndex: 0}},
				CompileGroups: []fileapi.CompileGroup{{
					Language:      "CXX",
					SourceIndexes: []int{0},
				}},
			},
			"lib::@b": {
				Name:    "lib",
				Type:    "STATIC_LIBRARY",
				Sources: []fileapi.TargetSource{{Path: "lib.cc", CompileGroupIndex: 0}},
				CompileGroups: []fileapi.CompileGroup{{
					Language:      "CXX",
					SourceIndexes: []int{0},
				}},
			},
		},
	}

	registry := buildRegistry(t, fixtureCTestSimple)

	pkg, err := lower.ToIR(r, nil, lower.Options{
		HostSourceRoot: "/nonexistent",
		CTest:          registry,
	})
	if err != nil {
		t.Fatalf("ToIR: %v", err)
	}

	// Expect: lib still cc_library, format-test now cc_test (cc_binary gone).
	var (
		gotLib, gotTest *ir.Target
		anyBinary       bool
	)
	for i := range pkg.Targets {
		switch pkg.Targets[i].Name {
		case "lib":
			gotLib = &pkg.Targets[i]
		case "format-test":
			gotTest = &pkg.Targets[i]
		}
		if pkg.Targets[i].Kind == ir.KindCCBinary {
			anyBinary = true
		}
	}
	if anyBinary {
		t.Errorf("expected no cc_binary rules, got %+v", pkg.Targets)
	}
	if gotLib == nil || gotLib.Kind != ir.KindCCLibrary {
		t.Errorf("lib not lowered as cc_library: %+v", gotLib)
	}
	if gotTest == nil {
		t.Fatalf("format-test missing from output: %+v", pkg.Targets)
	}
	if gotTest.Kind != ir.KindCCTest {
		t.Errorf("format-test Kind = %v, want KindCCTest", gotTest.Kind)
	}
	if got, want := gotTest.TestArgs, []string{"--filter=Bar"}; !equalStrings(got, want) {
		t.Errorf("TestArgs = %v, want %v", got, want)
	}
	if gotTest.TestTimeout != 30*time.Second {
		t.Errorf("TestTimeout = %v, want 30s", gotTest.TestTimeout)
	}
	if got := gotTest.TestEnv; !equalStrings(got, []string{"FOO=1"}) {
		t.Errorf("TestEnv = %v", got)
	}
	if got := gotTest.TestData; !equalStrings(got, []string{"data.txt"}) {
		t.Errorf("TestData = %v", got)
	}
	if !contains(gotTest.Tags, "exclusive") {
		t.Errorf("Tags = %v, want to include exclusive", gotTest.Tags)
	}
}

// TestToIR_CTest_MultipleAddTestPerExecutable validates the
// many-cc_test-from-one-binary path: two add_test() calls against the
// same executable produce two cc_test rules sharing srcs/deps.
func TestToIR_CTest_MultipleAddTestPerExecutable(t *testing.T) {
	r := &fileapi.Reply{
		Codemodel: fileapi.Codemodel{
			Configurations: []fileapi.Configuration{{
				Name: "Release",
				Targets: []fileapi.ConfigTargetRef{
					{Name: "parametric", Id: "parametric::@1"},
				},
			}},
		},
		Targets: map[string]fileapi.Target{
			"parametric::@1": {
				Name:    "parametric",
				Type:    "EXECUTABLE",
				Sources: []fileapi.TargetSource{{Path: "parametric.cc", CompileGroupIndex: 0}},
				CompileGroups: []fileapi.CompileGroup{{
					Language: "CXX", SourceIndexes: []int{0},
				}},
			},
		},
	}

	registry := buildRegistry(t, fixtureCTestParametric)

	pkg, err := lower.ToIR(r, nil, lower.Options{CTest: registry})
	if err != nil {
		t.Fatalf("ToIR: %v", err)
	}

	var names []string
	for _, tt := range pkg.Targets {
		if tt.Kind != ir.KindCCTest {
			t.Errorf("expected cc_test, got %v for %q", tt.Kind, tt.Name)
			continue
		}
		names = append(names, tt.Name)
	}
	if len(names) != 2 {
		t.Fatalf("expected 2 cc_test rules, got %d (%v)", len(names), names)
	}
	if !(contains(names, "param-a") && contains(names, "param-b")) {
		t.Errorf("expected param-a and param-b, got %v", names)
	}
}

// TestToIR_CTest_NilRegistryKeepsCcBinary covers the back-compat path:
// when no CTest registry is plumbed (e.g. --reply-dir offline runs),
// EXECUTABLE targets stay cc_binary.
func TestToIR_CTest_NilRegistryKeepsCcBinary(t *testing.T) {
	r := &fileapi.Reply{
		Codemodel: fileapi.Codemodel{
			Configurations: []fileapi.Configuration{{
				Name: "Release",
				Targets: []fileapi.ConfigTargetRef{
					{Name: "format-test", Id: "format-test::@a"},
				},
			}},
		},
		Targets: map[string]fileapi.Target{
			"format-test::@a": {
				Name: "format-test", Type: "EXECUTABLE",
				Sources: []fileapi.TargetSource{{Path: "x.cc", CompileGroupIndex: 0}},
				CompileGroups: []fileapi.CompileGroup{{
					Language: "CXX", SourceIndexes: []int{0},
				}},
			},
		},
	}
	pkg, err := lower.ToIR(r, nil, lower.Options{}) // CTest: nil
	if err != nil {
		t.Fatalf("ToIR: %v", err)
	}
	if len(pkg.Targets) != 1 || pkg.Targets[0].Kind != ir.KindCCBinary {
		t.Errorf("expected single cc_binary, got %+v", pkg.Targets)
	}
}

// TestEmit_CCTest renders a hand-rolled cc_test IR target and asserts
// the generated BUILD.bazel has the expected attributes — covers the
// emitter path including timeout formatting and env dict.
func TestEmit_CCTest(t *testing.T) {
	pkg := &ir.Package{
		Name: "x",
		Targets: []ir.Target{{
			Name:        "myt",
			Kind:        ir.KindCCTest,
			Srcs:        []string{"x.cc"},
			Deps:        []string{":lib"},
			TestArgs:    []string{"--gtest_filter=*"},
			TestTimeout: 30 * time.Second,
			TestEnv:     []string{"FOO=1", "BAR=2"},
			TestData:    []string{"data.txt"},
			Tags:        []string{"exclusive"},
		}},
	}
	out, err := bazel.Emit(pkg)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	body := string(out)
	// Phase 3's buildtools-canonical formatting wraps 2+ entry
	// dicts/lists across lines, so assert on per-entry substrings
	// rather than inline `{...}` / `[...]` shapes for env.
	for _, want := range []string{
		`cc_test(`,
		`name = "myt"`,
		`srcs = ["x.cc"]`,
		`args = ["--gtest_filter=*"]`,
		`"BAR": "2"`,
		`"FOO": "1"`,
		`timeout = "30s"`,
		`data = ["data.txt"]`,
		`tags = ["exclusive"]`,
		`deps = [":lib"]`,
		`load("@rules_cc//cc:defs.bzl", "cc_test")`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("emit missing %q\n%s", want, body)
		}
	}
}

// fixtureCTestSimple is the parsed CTestTestfile.cmake content for
// TestToIR_CTest_ClassifiesExecutableAsTest. One add_test for
// format-test with a full property surface.
const fixtureCTestSimple = `add_test([=[format-test]=] "/build/format-test" "--filter=Bar")
set_tests_properties([=[format-test]=] PROPERTIES TIMEOUT "30" ENVIRONMENT "FOO=1" REQUIRED_FILES "data.txt" RUN_SERIAL "TRUE")
`

// fixtureCTestParametric registers two tests against a single
// parametric executable.
const fixtureCTestParametric = `add_test([=[param-a]=] "/build/parametric" "--case=a")
add_test([=[param-b]=] "/build/parametric" "--case=b")
`

func buildRegistry(t *testing.T, body string) *ctest.Registry {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "CTestTestfile.cmake"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	r, err := ctest.Parse(dir)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func equalStrings(a, b []string) bool {
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
