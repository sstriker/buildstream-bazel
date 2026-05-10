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

func contains(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}
