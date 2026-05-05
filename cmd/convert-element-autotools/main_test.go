package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/sstriker/cmake-to-bazel/internal/manifest"
)

// TestParseExecveLine covers the strace text-format shape for
// `-f -e trace=execve -s 4096`. Real-world fixtures: a top-level
// compiler driver call, an internal cc1 sub-call, a failed
// execve, and an interleaved <unfinished>/resumed> pair (the
// latter two should be skipped).
func TestParseExecveLine(t *testing.T) {
	cases := []struct {
		name string
		line string
		want []string
		ok   bool
	}{
		{
			"top-level cc compile-and-link",
			`1748  execve("/usr/bin/cc", ["cc", "-O2", "-o", "greet", "greet.c"], 0x55c68cde2b40 /* 71 vars */) = 0`,
			[]string{"cc", "-O2", "-o", "greet", "greet.c"},
			true,
		},
		{
			"failed execve (ENOENT)",
			`9999  execve("/usr/bin/missing", ["missing"], 0x... /* 0 vars */) = -1 ENOENT (No such file or directory)`,
			nil, false,
		},
		{
			"unfinished call fragment",
			`1234  execve("/usr/bin/cc", ["cc", "-c", "x.c"], 0x... /* 0 vars */ <unfinished ...>`,
			nil, false,
		},
		{
			"non-execve line",
			`1234  openat(AT_FDCWD, "foo", O_RDONLY) = 3`,
			nil, false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := parseExecveLine(c.line)
			if ok != c.ok {
				t.Fatalf("ok = %v, want %v", ok, c.ok)
			}
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("argv = %#v, want %#v", got, c.want)
			}
		})
	}
}

// TestClassifyArgv_Compile / Link / Archive cover the four
// invocation shapes we recognize. Compile and link branch on
// `-c`; archive branches on `ar`'s mode-flag arg. classifyArgv
// keeps every flag in Copts; the default-toolchain strip
// happens later in buildRules with make-db awareness so
// per-target overrides survive.
func TestClassifyArgv(t *testing.T) {
	cases := []struct {
		name string
		argv []string
		ok   bool
		want Event
	}{
		{
			"compile-only",
			[]string{"cc", "-c", "-fstack-protector-strong", "-o", "foo.o", "foo.c"},
			true,
			Event{Kind: EventCompile, Out: "foo.o", Srcs: []string{"foo.c"}, Copts: []string{"-fstack-protector-strong"}},
		},
		{
			"compile-and-link greet-style",
			[]string{"cc", "-fstack-protector-strong", "-o", "greet", "greet.c"},
			true,
			Event{Kind: EventLink, Out: "greet", Srcs: []string{"greet.c"}, Copts: []string{"-fstack-protector-strong"}},
		},
		{
			"link-only with -l",
			[]string{"cc", "-fstack-protector-strong", "-o", "myapp", "myapp.o", "-L.", "-lfoo"},
			true,
			Event{Kind: EventLink, Out: "myapp", Objs: []string{"myapp.o"}, Libs: []string{"foo"}, Copts: []string{"-fstack-protector-strong"}},
		},
		{
			// classifyArgv keeps default-toolchain flags;
			// the strip happens in buildRules with make-db
			// awareness so per-target overrides survive.
			// -DNDEBUG still routes to defines and gets the
			// special "drop, Bazel's opt mode provides it"
			// treatment in the post-strip pass.
			"default-toolchain flags retained at classify",
			[]string{"cc", "-O2", "-fPIC", "-g", "-DNDEBUG", "-c", "-o", "foo.o", "foo.c"},
			true,
			Event{Kind: EventCompile, Out: "foo.o", Srcs: []string{"foo.c"}, Copts: []string{"-O2", "-fPIC", "-g"}},
		},
		{
			"-D extracted to defines",
			[]string{"cc", "-c", "-DFOO=1", "-DBAR", "-o", "foo.o", "foo.c"},
			true,
			Event{Kind: EventCompile, Out: "foo.o", Srcs: []string{"foo.c"}, Defines: []string{"BAR", "FOO=1"}},
		},
		{
			"archive ar rcs",
			[]string{"ar", "rcs", "libfoo.a", "foo.o", "bar.o"},
			true,
			Event{Kind: EventArchive, Out: "libfoo.a", Objs: []string{"foo.o", "bar.o"}},
		},
		{
			"gcc-internal cc1 (filtered)",
			[]string{"/usr/libexec/gcc/x86_64-linux-gnu/13/cc1", "-quiet", "x.c"},
			false, Event{},
		},
		{
			"ar list-mode (filtered: not r/q)",
			[]string{"ar", "t", "libfoo.a"},
			false, Event{},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := classifyArgv(c.argv)
			if ok != c.ok {
				t.Fatalf("ok = %v, want %v", ok, c.ok)
			}
			if !c.ok {
				return
			}
			if got.Kind != c.want.Kind {
				t.Errorf("Kind = %d, want %d", got.Kind, c.want.Kind)
			}
			if got.Out != c.want.Out {
				t.Errorf("Out = %q, want %q", got.Out, c.want.Out)
			}
			if !reflect.DeepEqual(got.Srcs, c.want.Srcs) {
				t.Errorf("Srcs = %#v, want %#v", got.Srcs, c.want.Srcs)
			}
			if !reflect.DeepEqual(got.Objs, c.want.Objs) {
				t.Errorf("Objs = %#v, want %#v", got.Objs, c.want.Objs)
			}
			if !reflect.DeepEqual(got.Libs, c.want.Libs) {
				t.Errorf("Libs = %#v, want %#v", got.Libs, c.want.Libs)
			}
			if !reflect.DeepEqual(got.Copts, c.want.Copts) {
				t.Errorf("Copts = %#v, want %#v", got.Copts, c.want.Copts)
			}
			if !reflect.DeepEqual(got.Defines, c.want.Defines) {
				t.Errorf("Defines = %#v, want %#v", got.Defines, c.want.Defines)
			}
		})
	}
}

// TestCorrelate_LibAndApp exercises the full correlation
// pipeline for an autotools project that produces a static
// library + a binary linking against it. Three compile events
// (foo.c → foo.o, bar.c → bar.o, myapp.c → myapp.o), one
// archive (libfoo.a from foo.o + bar.o), one link (myapp from
// myapp.o + -lfoo).
func TestCorrelate_LibAndApp(t *testing.T) {
	// `-fstack-protector-strong` is a non-default flag that
	// passes through copts (used here so the test can assert
	// copts are wired); `-O2` would be stripped.
	events := []Event{
		{Kind: EventCompile, Out: "foo.o", Srcs: []string{"foo.c"}, Copts: []string{"-fstack-protector-strong"}, Defines: []string{"FOO=1"}},
		{Kind: EventCompile, Out: "bar.o", Srcs: []string{"bar.c"}, Copts: []string{"-fstack-protector-strong"}, Defines: []string{"FOO=1"}},
		{Kind: EventCompile, Out: "myapp.o", Srcs: []string{"myapp.c"}, Copts: []string{"-fstack-protector-strong"}},
		{Kind: EventArchive, Out: "libfoo.a", Objs: []string{"foo.o", "bar.o"}},
		{Kind: EventLink, Out: "myapp", Objs: []string{"myapp.o"}, Libs: []string{"foo"}, Copts: []string{"-fstack-protector-strong"}},
	}
	got := emitBuild(correlate(events), nil, nil)

	for _, marker := range []string{
		`load("@rules_cc//cc:defs.bzl", "cc_binary", "cc_library")`,
		`cc_library(`,
		`name = "foo"`,
		`srcs = ["bar.c", "foo.c"]`,
		`copts = ["-fstack-protector-strong"]`,
		`defines = ["FOO=1"]`,
		`linkstatic = True`,
		`cc_binary(`,
		`name = "myapp"`,
		`srcs = ["myapp.c"]`,
		`deps = [":foo"]`,
	} {
		if !strings.Contains(got, marker) {
			t.Errorf("missing marker %q\n--body--\n%s", marker, got)
		}
	}
	// Library should come before binary (sort: cc_library < cc_binary
	// by alphabetical name happens to also satisfy "libs first").
	libIdx := strings.Index(got, `cc_library(`)
	binIdx := strings.Index(got, `cc_binary(`)
	if libIdx < 0 || binIdx < 0 || libIdx > binIdx {
		t.Errorf("expected cc_library before cc_binary; lib=%d bin=%d", libIdx, binIdx)
	}
}

// TestCorrelate_GreetStandalone covers the original spike's
// shape: a single compile-and-link invocation without any
// archives. Falls through to the EventLink path with srcs
// directly listed.
func TestCorrelate_GreetStandalone(t *testing.T) {
	events := []Event{
		{Kind: EventLink, Out: "greet", Srcs: []string{"greet.c"}, Copts: []string{"-fstack-protector-strong"}},
	}
	got := emitBuild(correlate(events), nil, nil)
	for _, marker := range []string{
		`load("@rules_cc//cc:defs.bzl", "cc_binary")`,
		`cc_binary(`,
		`name = "greet"`,
		`srcs = ["greet.c"]`,
		`copts = ["-fstack-protector-strong"]`,
	} {
		if !strings.Contains(got, marker) {
			t.Errorf("missing marker %q\n--body--\n%s", marker, got)
		}
	}
	// No cc_library involved → no linkstatic.
	if strings.Contains(got, `linkstatic`) {
		t.Errorf("greet-only render should not include linkstatic")
	}
}

// TestEmitBuild_ImportsManifestFallback covers the
// cross-element dep edge path: a link command's `-l<name>`
// that doesn't match an in-trace archive falls back to the
// imports manifest's LookupLinkLibrary, mapping `-lz` →
// `//elements/zlib:zlib`. Mirrors lower's cmake STATIC
// IMPORTED dep recovery shape.
func TestEmitBuild_ImportsManifestFallback(t *testing.T) {
	tmp := t.TempDir()
	mf := filepath.Join(tmp, "imports.json")
	if err := os.WriteFile(mf, []byte(`{
  "version": 1,
  "elements": [{
    "name": "zlib",
    "exports": [{
      "cmake_target": "ZLIB::ZLIB",
      "bazel_label": "//elements/zlib:zlib",
      "link_libraries": ["z"]
    }]
  }]
}`), 0o644); err != nil {
		t.Fatal(err)
	}
	imports, err := manifest.Load(mf)
	if err != nil {
		t.Fatal(err)
	}

	events := []Event{
		{Kind: EventLink, Out: "myapp", Srcs: []string{"myapp.c"}, Libs: []string{"z"}},
	}
	got := emitBuild(correlate(events), imports, nil)
	if !strings.Contains(got, `deps = ["//elements/zlib:zlib"]`) {
		t.Errorf("expected deps to resolve -lz via manifest:\n%s", got)
	}

	// Negative check: nil manifest (no fallback) drops the
	// unresolved -lz silently.
	got2 := emitBuild(correlate(events), nil, nil)
	if strings.Contains(got2, "deps") {
		t.Errorf("nil manifest should not produce deps; got:\n%s", got2)
	}
}

// TestPerTargetIntent_PreservesAlwaysOptimize covers the
// real-world case the user flagged: a Makefile that
// per-target-bumps optimization for a hot translation unit
// (`hotloop.o: CFLAGS += -O2` even when global CFLAGS=-O0).
// The trace captures `cc -O0 -g -O2 -c hotloop.c -o hotloop.o`;
// without make-db awareness, all three flags get stripped as
// "default-toolchain". With make-db awareness, the per-target
// `-O2` survives because it's in the user's declared intent.
func TestPerTargetIntent_PreservesAlwaysOptimize(t *testing.T) {
	events := []Event{
		// cool.o: gets only global CFLAGS (-O0 -g).
		{Kind: EventCompile, Out: "cool.o", Srcs: []string{"cool.c"}, Copts: []string{"-O0", "-g"}},
		// hotloop.o: gets global CFLAGS + per-target += -O2.
		{Kind: EventCompile, Out: "hotloop.o", Srcs: []string{"hotloop.c"}, Copts: []string{"-O0", "-g", "-O2"}},
		{Kind: EventArchive, Out: "libcool.a", Objs: []string{"cool.o", "hotloop.o"}},
	}
	makeDB := &MakeDB{
		Variables:  map[string]string{"CFLAGS": "-O0 -g"},
		Rules:      map[string]MakeRule{},
		TargetVars: map[string][]TargetVar{"hotloop.o": {{Name: "CFLAGS", Op: "+=", Value: "-O2"}}},
	}
	got := emitBuild(correlate(events), nil, makeDB)
	// hotloop.o's per-target -O2 lands in the cc_library's copts.
	if !strings.Contains(got, `copts = ["-O2"]`) {
		t.Errorf("expected -O2 in copts (per-target intent preserved):\n%s", got)
	}
	// cool.o's flags are all default-strip and have no
	// per-target intent → no copts attribute.
	if strings.Contains(got, `"-O0"`) || strings.Contains(got, `"-g"`) {
		t.Errorf("global CFLAGS leaked through default-strip:\n%s", got)
	}
}

// TestCorrelate_LibtoolDualCompile is the regression guard for
// the libtool-style dual-compile pattern: the same source
// (foo.c) is compiled twice, once with -fPIC -DPIC into
// .libs/foo.o (archived as libfoo_pic.a) and once without PIC
// into foo.o (archived as libfoo.a). Both .o paths share the
// basename `foo.o` — without exact-path correlation, both
// archives' lookups would resolve to the same (last-written)
// compile event, causing the static lib to inherit the PIC
// compile's -DPIC define. With exact-path correlation, each
// archive maps to its own compile event.
func TestCorrelate_LibtoolDualCompile(t *testing.T) {
	events := []Event{
		// Non-PIC compile (drives libfoo.a).
		{Kind: EventCompile, Out: "foo.o", Srcs: []string{"foo.c"}, Copts: []string{"-O2"}},
		{Kind: EventArchive, Out: "libfoo.a", Objs: []string{"foo.o"}},
		// PIC compile (drives libfoo_pic.a). -fPIC strips as
		// default-toolchain; -DPIC stays as a defines entry.
		{Kind: EventCompile, Out: ".libs/foo.o", Srcs: []string{"foo.c"}, Copts: []string{"-O2", "-fPIC"}, Defines: []string{"PIC"}},
		{Kind: EventArchive, Out: "libfoo_pic.a", Objs: []string{".libs/foo.o"}},
	}
	got := emitBuild(correlate(events), nil, nil)
	// Both rules emit, both have foo.c as src.
	for _, marker := range []string{
		`name = "foo"`,
		`name = "foo_pic"`,
		`srcs = ["foo.c"]`,
	} {
		if !strings.Contains(got, marker) {
			t.Errorf("missing marker %q\n--body--\n%s", marker, got)
		}
	}
	// Scope assertions to each rule's body. The cc_library
	// blocks are separated by blank lines.
	blocks := strings.Split(got, "cc_library(")
	var fooBody, fooPicBody string
	for _, b := range blocks {
		switch {
		case strings.Contains(b, `name = "foo"`) && !strings.Contains(b, `name = "foo_pic"`):
			fooBody = b
		case strings.Contains(b, `name = "foo_pic"`):
			fooPicBody = b
		}
	}
	if fooBody == "" || fooPicBody == "" {
		t.Fatalf("could not isolate rule bodies\n--body--\n%s", got)
	}
	// foo_pic carries the PIC define.
	if !strings.Contains(fooPicBody, `defines = ["PIC"]`) {
		t.Errorf("foo_pic missing defines=[\"PIC\"]\n--foo_pic body--\n%s", fooPicBody)
	}
	// foo (the static lib) must NOT inherit PIC — that's the
	// libtool collision the exact-path correlation prevents.
	if strings.Contains(fooBody, `defines = ["PIC"]`) {
		t.Errorf("foo (static) leaked PIC define from libtool dual-compile\n--foo body--\n%s", fooBody)
	}
	// -fPIC is stripped as a default-toolchain flag in both.
	if strings.Contains(got, `"-fPIC"`) {
		t.Errorf("-fPIC should be stripped (default-toolchain)\n%s", got)
	}
}

// TestPerTargetIntent_NilDBStripsEverything is the regression
// guard for the simple case: nil makeDB falls back to
// unconditional default-toolchain stripping (every -O\d, -g,
// -fPIC dropped).
func TestPerTargetIntent_NilDBStripsEverything(t *testing.T) {
	events := []Event{
		{Kind: EventCompile, Out: "x.o", Srcs: []string{"x.c"}, Copts: []string{"-O2", "-g", "-fPIC"}},
		{Kind: EventArchive, Out: "libx.a", Objs: []string{"x.o"}},
	}
	got := emitBuild(correlate(events), nil, nil)
	if strings.Contains(got, "copts =") {
		t.Errorf("nil makeDB should strip every default flag; got:\n%s", got)
	}
}

// TestStripLibPrefixSuffix covers the lib<name>.a → <name>
// conversion used to (a) name the cc_library rule and
// (b) match -l<name> link flags.
func TestStripLibPrefixSuffix(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"libfoo.a", "foo"},
		{"./libfoo.a", "foo"},
		{"build/libfoo.a", "foo"},
		{"lib.a", ""},
		{"foo.a", "foo"}, // no `lib` prefix: leave name intact
	}
	for _, c := range cases {
		if got := stripLibPrefixSuffix(c.in); got != c.want {
			t.Errorf("stripLibPrefixSuffix(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestCorrelate_SubdirsCwdDisambiguation guards the recursive-
// automake (`SUBDIRS`) collision case: two sub-Makefiles each
// compile a `parent.c` (same basename, same `-o parent.o`)
// inside their own subdir. Without cwd-keyed correlation, both
// subdir archives' lookups would resolve to whichever compile
// event happened to be written last into objByPath / objByBasename
// — collapsing the two archives' sources together. With cwd
// captured by the build-tracer and threaded through to the
// archive event, each archive resolves to its own subdir's
// compile.
func TestCorrelate_SubdirsCwdDisambiguation(t *testing.T) {
	events := []Event{
		// libA/Makefile recipe runs from libA/ and produces libA/parent.o.
		// The Out path is what `-o` produced relative to the recipe's
		// cwd ("parent.o"); cwd is the absolute libA path.
		{Kind: EventCompile, Cwd: "/build/libA", Out: "parent.o", Srcs: []string{"parent.c"}, Defines: []string{"VARIANT=A"}},
		{Kind: EventArchive, Cwd: "/build/libA", Out: "libA.a", Objs: []string{"parent.o"}},
		// libB/Makefile: identical basename / identical Out, different cwd.
		{Kind: EventCompile, Cwd: "/build/libB", Out: "parent.o", Srcs: []string{"parent.c"}, Defines: []string{"VARIANT=B"}},
		{Kind: EventArchive, Cwd: "/build/libB", Out: "libB.a", Objs: []string{"parent.o"}},
	}
	got := emitBuild(correlate(events), nil, nil)

	// Both archives should appear with their own define preserved.
	blocks := strings.Split(got, "cc_library(")
	var aBody, bBody string
	for _, b := range blocks {
		switch {
		case strings.Contains(b, `name = "A"`):
			aBody = b
		case strings.Contains(b, `name = "B"`):
			bBody = b
		}
	}
	if aBody == "" || bBody == "" {
		t.Fatalf("could not isolate A/B rule bodies\n--body--\n%s", got)
	}
	if !strings.Contains(aBody, `defines = ["VARIANT=A"]`) {
		t.Errorf("libA leaked the wrong VARIANT (cwd disambiguation failed)\n--A body--\n%s", aBody)
	}
	if !strings.Contains(bBody, `defines = ["VARIANT=B"]`) {
		t.Errorf("libB leaked the wrong VARIANT (cwd disambiguation failed)\n--B body--\n%s", bBody)
	}
}

// TestParseCwdAnnotation covers the build-tracer-extension
// `cwd:"..."` annotation that the native tracer emits between
// the pid prefix and the execve(...) call. Strace traces (no
// cwd capture) must round-trip as empty.
func TestParseCwdAnnotation(t *testing.T) {
	cases := []struct {
		name string
		line string
		want string
	}{
		{
			"native tracer line with cwd",
			`1748  cwd:"/build/libA"  execve("/usr/bin/cc", ["cc", "-c", "-o", "parent.o", "parent.c"], 0x... /* 0 vars */) = 0`,
			"/build/libA",
		},
		{
			"strace line with no cwd annotation",
			`1748  execve("/usr/bin/cc", ["cc", "-c", "-o", "parent.o", "parent.c"], 0x... /* 0 vars */) = 0`,
			"",
		},
		{
			"cwd containing an escaped quote",
			`1748  cwd:"/build/weird\"dir"  execve("/usr/bin/cc", ["cc"], 0x... /* 0 vars */) = 0`,
			`/build/weird"dir`,
		},
		{
			"cwd token appears AFTER execve (not the annotation we want)",
			`1748  execve("/usr/bin/cwd:\"trap\"", ["cwd"], 0x... /* 0 vars */) = 0`,
			"",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := parseCwdAnnotation(c.line); got != c.want {
				t.Errorf("parseCwdAnnotation = %q, want %q", got, c.want)
			}
		})
	}
}
