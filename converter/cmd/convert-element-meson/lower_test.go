package main

import (
	"strings"
	"testing"

	"github.com/sstriker/cmake-to-bazel/converter/internal/emit/bazel"
	"github.com/sstriker/cmake-to-bazel/converter/internal/ir"
)

func TestLower_StaticLibAndExecutable(t *testing.T) {
	intro := &Introspect{
		ProjectInfo: ProjectInfo{Name: "hello", Version: "0.1.0"},
		Targets: []Target{
			{
				Name:     "hello",
				ID:       "hello@sta",
				Type:     "static library",
				Filename: []string{"/bd/libhello.a"},
				TargetSources: []TargetSource{
					{
						Language:   "c",
						Machine:    "host",
						Compiler:   []string{"cc"},
						Parameters: []string{"-I/src/include", "-DFOO_BUILD", "-Wall", "-O2", "-fPIC", "-fdiagnostics-color=always"},
						Sources:    []string{"/src/src/hello.c"},
					},
					{
						Linker:     []string{"gcc-ar"},
						Parameters: []string{"csrD"},
					},
				},
			},
			{
				Name:     "hello-bin",
				ID:       "hello-bin@exe",
				Type:     "executable",
				Filename: []string{"/bd/hello-bin"},
				TargetSources: []TargetSource{
					{
						Language:   "c",
						Machine:    "host",
						Compiler:   []string{"cc"},
						Parameters: []string{"-I/src", "-Wall"},
						Sources:    []string{"/src/src/main.c"},
					},
					{
						Linker:     []string{"cc"},
						Parameters: []string{"-Wl,--as-needed", "-Wl,--no-undefined", "libhello.a"},
					},
				},
			},
		},
	}
	pkg, err := Lower(intro, LowerOptions{
		SourceRoot: "/src",
		BuildDir:   "/bd",
	})
	if err != nil {
		t.Fatalf("Lower: %v", err)
	}
	out, err := bazel.Emit(pkg)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	got := string(out)

	for _, want := range []string{
		`name = "hello"`,
		`srcs = ["src/hello.c"]`,
		`includes = ["include"]`,
		`defines = ["FOO_BUILD"]`,
		`linkstatic = True`,
		`name = "hello-bin"`,
		`srcs = ["src/main.c"]`,
		`deps = [":hello"]`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q\n--- got ---\n%s", want, got)
		}
	}

	// `-fPIC` and `-fdiagnostics-color=always` are toolchain-handled;
	// they shouldn't appear in copts.
	for _, dropped := range []string{
		"-fPIC",
		"-fdiagnostics-color=always",
		`linkopts = ["csrD"]`, // archiver flag must be dropped
	} {
		if strings.Contains(got, dropped) {
			t.Errorf("output unexpectedly contains %q\n%s", dropped, got)
		}
	}

	// Verify package-level structure: 2 targets, hello first.
	if len(pkg.Targets) != 2 {
		t.Fatalf("len(targets)=%d want 2", len(pkg.Targets))
	}
	if pkg.Targets[0].Name != "hello" || pkg.Targets[0].Kind != ir.KindCCLibrary {
		t.Errorf("targets[0]=%+v want hello/cc_library", pkg.Targets[0])
	}
	if pkg.Targets[1].Name != "hello-bin" || pkg.Targets[1].Kind != ir.KindCCBinary {
		t.Errorf("targets[1]=%+v want hello-bin/cc_binary", pkg.Targets[1])
	}
}

func TestLower_RefusesSubproject(t *testing.T) {
	sub := "subprojects/foo"
	intro := &Introspect{
		ProjectInfo: ProjectInfo{Name: "p"},
		Targets: []Target{
			{
				Name:       "lib",
				ID:         "lib@sta",
				Type:       "static library",
				Subproject: &sub,
			},
		},
	}
	_, err := Lower(intro, LowerOptions{SourceRoot: "/src"})
	if err == nil || !strings.Contains(err.Error(), "unsupported-meson-subproject") {
		t.Fatalf("want unsupported-meson-subproject, got %v", err)
	}
}

func TestLower_RefusesUnknownTargetType(t *testing.T) {
	intro := &Introspect{
		ProjectInfo: ProjectInfo{Name: "p"},
		Targets: []Target{
			{Name: "x", ID: "x@jar", Type: "jar"},
		},
	}
	_, err := Lower(intro, LowerOptions{SourceRoot: "/src"})
	if err == nil || !strings.Contains(err.Error(), "unsupported-meson-target-type") {
		t.Fatalf("want unsupported-meson-target-type, got %v", err)
	}
}

func TestLower_FoldsBuiltinDependencyArgs(t *testing.T) {
	// `dependency('threads')` resolves to `-pthread` without an
	// imports-manifest binding. Confirm the converter folds the
	// flags inline rather than refusing.
	intro := &Introspect{
		ProjectInfo: ProjectInfo{Name: "p"},
		Targets: []Target{
			{
				Name: "lib",
				ID:   "lib@sha",
				Type: "shared library",
				TargetSources: []TargetSource{
					{Language: "c", Machine: "host", Sources: []string{"/src/foo.c"}, Parameters: []string{}},
					{Linker: []string{"cc"}},
				},
				Dependencies: []string{"threads"},
			},
		},
		Dependencies: []Dependency{
			{Name: "threads", Type: "system", CompileArgs: []string{"-pthread"}, LinkArgs: []string{"-pthread"}},
		},
	}
	pkg, err := Lower(intro, LowerOptions{SourceRoot: "/src"})
	if err != nil {
		t.Fatalf("Lower: %v", err)
	}
	tgt := pkg.Targets[0]
	if !contains(tgt.Copts, "-pthread") {
		t.Errorf("copts missing -pthread: %v", tgt.Copts)
	}
	if !contains(tgt.LinkOpts, "-pthread") {
		t.Errorf("linkopts missing -pthread: %v", tgt.LinkOpts)
	}
}

func TestRelativizeOutputs(t *testing.T) {
	// Subdir paths are preserved.
	got, err := relativizeOutputs([]string{"/bd/gen/out.h", "/bd/foo.c"}, "/bd")
	if err != nil {
		t.Fatalf("relativizeOutputs: %v", err)
	}
	want := []string{"gen/out.h", "foo.c"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("got[%d]=%q want %q", i, got[i], want[i])
		}
	}
	// Basename collision (same basename in two subdirs) refuses.
	if _, err := relativizeOutputs([]string{"/bd/a.h", "/bd/sub/a.h"}, ""); err == nil ||
		!strings.Contains(err.Error(), "unsupported-meson-custom-target") {
		t.Errorf("collision case: want refusal, got %v", err)
	}
	// Empty BuildDir falls back to basename.
	got2, err := relativizeOutputs([]string{"/anywhere/foo.h"}, "")
	if err != nil {
		t.Fatalf("relativizeOutputs (empty BuildDir): %v", err)
	}
	if len(got2) != 1 || got2[0] != "foo.h" {
		t.Errorf("empty BuildDir: got %v want [foo.h]", got2)
	}
}

func TestLower_RefusesMultiEntryCustomTarget(t *testing.T) {
	// Two target_sources entries → multi-group / multi-COMMAND
	// custom_target shape; v1 refuses rather than silently
	// dropping the second entry's inputs.
	intro := &Introspect{
		ProjectInfo: ProjectInfo{Name: "p"},
		Targets: []Target{
			{
				Name:     "ct",
				ID:       "ct@cus",
				Type:     "custom",
				Filename: []string{"/bd/out.h"},
				TargetSources: []TargetSource{
					{Compiler: []string{"cp", "@INPUT@", "@OUTPUT@"}, Sources: []string{"/src/a.in"}},
					{Compiler: []string{"sed", "@INPUT@"}, Sources: []string{"/src/b.in"}},
				},
			},
		},
	}
	_, err := Lower(intro, LowerOptions{SourceRoot: "/src"})
	if err == nil || !strings.Contains(err.Error(), "unsupported-meson-custom-target") {
		t.Fatalf("want unsupported-meson-custom-target, got %v", err)
	}
}

func TestLower_RefusesUnboundExternalDep(t *testing.T) {
	intro := &Introspect{
		ProjectInfo: ProjectInfo{Name: "p"},
		Targets: []Target{
			{
				Name: "lib",
				ID:   "lib@sha",
				Type: "shared library",
				TargetSources: []TargetSource{
					{Language: "c", Machine: "host", Sources: []string{"/src/foo.c"}},
				},
				Dependencies: []string{"glib-2.0"},
			},
		},
	}
	_, err := Lower(intro, LowerOptions{SourceRoot: "/src"})
	if err == nil || !strings.Contains(err.Error(), "unresolved-meson-dependency") {
		t.Fatalf("want unresolved-meson-dependency, got %v", err)
	}
}

func TestLower_SoVersionedSuffix(t *testing.T) {
	cases := map[string]bool{
		"libfoo.so":         false, // plain .so is handled by HasSuffix already
		"libfoo.so.1":       true,
		"libfoo.so.1.2.3":   true,
		"libfoo.so.notanum": false,
		"libfoo":            false,
	}
	for in, want := range cases {
		if got := soVersionedSuffix(in); got != want {
			t.Errorf("soVersionedSuffix(%q)=%v want %v", in, got, want)
		}
	}
}

func TestIsUnderDir(t *testing.T) {
	cases := []struct {
		name string
		path string
		dir  string
		want bool
	}{
		{"same dir", "/tmp/bd", "/tmp/bd", true},
		{"descendant", "/tmp/bd/sub/x", "/tmp/bd", true},
		{"sibling-prefix not descendant", "/tmp/bd2/include", "/tmp/bd", false},
		{"unrelated", "/usr/include", "/tmp/bd", false},
		{"empty dir", "/anything", "", false},
		{"trailing slash on dir", "/tmp/bd/x", "/tmp/bd/", true},
	}
	for _, c := range cases {
		if got := isUnderDir(c.path, c.dir); got != c.want {
			t.Errorf("%s: isUnderDir(%q, %q)=%v want %v", c.name, c.path, c.dir, got, c.want)
		}
	}
}

func TestApplyCompileParameters_BuildDirSiblingNotDropped(t *testing.T) {
	// Regression for the strings.HasPrefix bug: an include that
	// starts with the build-dir's path component but lives in a
	// SIBLING dir must NOT be filtered out.
	tgt := &ir.Target{}
	applyCompileParameters(tgt, []string{
		"-I/tmp/bd/private.p", // genuine build-dir include — drop
		"-I/tmp/bd2/include",  // sibling-prefix — KEEP (as Copts since outside source-root)
		"-I/src/include",      // source-tree include — KEEP (project to "include")
	}, LowerOptions{
		SourceRoot: "/src",
		BuildDir:   "/tmp/bd",
	})
	for _, c := range tgt.Copts {
		if strings.HasPrefix(c, "-I/tmp/bd/") {
			t.Errorf("build-dir include leaked into copts: %q", c)
		}
	}
	if !contains(tgt.Copts, "-I/tmp/bd2/include") {
		t.Errorf("sibling-dir include incorrectly dropped; copts=%v", tgt.Copts)
	}
	if !contains(tgt.Includes, "include") {
		t.Errorf("source-tree include not projected; includes=%v", tgt.Includes)
	}
}

func TestShellQuote(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		// Safe shapes pass through unquoted.
		{"foo", "foo"},
		{"--flag=value", "--flag=value"},
		{"path/to/file.c", "path/to/file.c"},
		{"abc-123_xyz", "abc-123_xyz"},
		// Anything else gets single-quoted. The full POSIX shell
		// metacharacter set must be covered (regression for the
		// previous metachar-list bug that omitted ; | & < > ( )
		// newline).
		{"a;b", "'a;b'"},
		{"a|b", "'a|b'"},
		{"a&b", "'a&b'"},
		{"a<b", "'a<b'"},
		{"a>b", "'a>b'"},
		{"a(b", "'a(b'"},
		{"a)b", "'a)b'"},
		{"a b", "'a b'"},
		{"a\nb", "'a\nb'"},
		{"a$b", "'a$b'"},
		{"a`b", "'a`b'"},
		{"a*b", "'a*b'"},
		// Embedded single quotes are escaped via the canonical
		// '\'' dance so the surrounding quoting stays sound.
		{"a'b", `'a'\''b'`},
		// Empty arg becomes literal '' — preserves the argv slot.
		{"", "''"},
	}
	for _, c := range cases {
		if got := shellQuote(c.in); got != c.want {
			t.Errorf("shellQuote(%q)=%q want %q", c.in, got, c.want)
		}
	}
}

func TestRenderCustomCmd_RefusesEmbeddedAndUnreferencedTokens(t *testing.T) {
	// Embedded @INPUT@ in a larger token must refuse.
	if _, err := renderCustomCmd([]string{"--in=@INPUT@", "@OUTPUT@"}, []string{"a"}, []string{"b"}); err == nil ||
		!strings.Contains(err.Error(), "unsupported-meson-custom-target") {
		t.Errorf("embedded @INPUT@ accepted: %v", err)
	}
	// Indexed @OUTPUT0@ must refuse.
	if _, err := renderCustomCmd([]string{"@OUTPUT0@"}, nil, []string{"b"}); err == nil ||
		!strings.Contains(err.Error(), "unsupported-meson-custom-target") {
		t.Errorf("indexed @OUTPUT0@ accepted: %v", err)
	}
	// argv with srcs but no @INPUT@ token must refuse.
	if _, err := renderCustomCmd([]string{"cp", "/literal/path", "@OUTPUT@"}, []string{"a"}, []string{"b"}); err == nil ||
		!strings.Contains(err.Error(), "doesn't reference @INPUT@") {
		t.Errorf("argv missing @INPUT@ but with srcs accepted: %v", err)
	}
	// argv missing @OUTPUT@ must refuse.
	if _, err := renderCustomCmd([]string{"cp", "@INPUT@", "/dst"}, []string{"a"}, []string{"b"}); err == nil ||
		!strings.Contains(err.Error(), "doesn't reference @OUTPUT@") {
		t.Errorf("argv missing @OUTPUT@ accepted: %v", err)
	}
	// Canonical liftable shape lifts.
	got, err := renderCustomCmd([]string{"cp", "@INPUT@", "@OUTPUT@"}, []string{"a"}, []string{"b"})
	if err != nil {
		t.Fatalf("canonical shape refused: %v", err)
	}
	if !strings.Contains(got, "$(location a)") || !strings.Contains(got, "$(location b)") {
		t.Errorf("canonical shape didn't substitute: %q", got)
	}
}

func contains(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}
