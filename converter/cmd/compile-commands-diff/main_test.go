package main

import (
	"strings"
	"testing"
)

func TestFactsFromArgv(t *testing.T) {
	argv := []string{
		"/usr/bin/gcc", "-DFOO", "-DBAR=1", "-D", "BAZ", "-std=c++17",
		"-I/abs/inc", "-Irel/inc", "-isystem", "/sys/inc",
		"-iquote", ".", "-iquote", "q/inc", // exec-root `.` dropped; real -iquote kept
		"-D__DATE__=\"redacted\"", "-D__TIME__=\"redacted\"", // Bazel stamps — must be filtered
		"-fvisibility=hidden", "-fno-rtti", // project copts (kept)
		"-Wall", "-O2", "-g", "-fstack-protector", "-fPIC", // toolchain/build-mode (filtered)
		"-c", "src/foo.cc", "-o", "foo.o",
	}
	f := factsFromArgv(argv)
	for _, want := range []string{"-fvisibility=hidden", "-fno-rtti"} {
		if !f.Copts[want] {
			t.Errorf("missing project copt %q in %v", want, f.Copts)
		}
	}
	for _, no := range []string{"-Wall", "-O2", "-g", "-fstack-protector", "-fPIC", "-c", "-o"} {
		if f.Copts[no] {
			t.Errorf("toolchain/structural flag %q should be filtered from copts", no)
		}
	}
	for _, want := range []string{"FOO", "BAR=1", "BAZ"} {
		if !f.Defines[want] {
			t.Errorf("missing define %q in %v", want, f.Defines)
		}
	}
	if f.Defines["__DATE__=\"redacted\""] || f.Defines["__TIME__=\"redacted\""] {
		t.Errorf("Bazel reproducibility stamps not filtered: %v", f.Defines)
	}
	if f.Std != "c++17" {
		t.Errorf("std = %q want c++17", f.Std)
	}
	for _, want := range []string{"/abs/inc", "rel/inc", "/sys/inc", "q/inc"} { // raw, normalized at diff time
		if !f.IncludeDir[want] {
			t.Errorf("missing raw include %q in %v", want, f.IncludeDir)
		}
	}
	// Bazel's universal exec-root `-iquote .` is structural noise (cmake never
	// emits -iquote) — it must NOT be recorded as a project include dir.
	if f.IncludeDir["."] {
		t.Errorf("exec-root -iquote . should be dropped; got %v", f.IncludeDir)
	}
}

func TestNormalizeInclude(t *testing.T) {
	o := normOpts{cmakeSrc: "/tmp/zlib", cmakeBuild: "/tmp/zbuild", bazelPkg: "elements/zlib"}
	cases := map[string]string{
		"/tmp/zlib/include":     "include",     // cmake source include
		"/tmp/zlib":             ".",           // cmake package root
		"/tmp/zbuild":           "gen:.",       // cmake build dir
		"/tmp/zbuild/gen":       "gen:gen",     // cmake generated subdir
		"/usr/include":          "sys:include", // system
		"elements/zlib/include": "include",     // bazel source include (matches cmake's)
		"elements/zlib":         ".",           // bazel package root (matches cmake's)
		"bazel-out/k8-fastbuild/bin/elements/zlib":   "gen:.",   // bazel generated root
		"bazel-out/k8-fastbuild/bin/elements/zlib/g": "gen:g",   // bazel generated subdir
		"/tmp/zlib/sub/../include":                   "include", // `..` collapses (cmake src/../include shape)
	}
	for in, want := range cases {
		if got := normalizeInclude(in, o); got != want {
			t.Errorf("normalizeInclude(%q) = %q want %q", in, got, want)
		}
	}
	// The point of the exercise: a cmake source include and the matching bazel
	// one collapse to the SAME key, so equivalent header search doesn't show as
	// a spurious mismatch.
	if normalizeInclude("/tmp/zlib/include", o) != normalizeInclude("elements/zlib/include", o) {
		t.Error("cmake and bazel source includes should normalize equal")
	}
}

func TestIgnoredDefine(t *testing.T) {
	for _, d := range []string{"__DATE__=\"redacted\"", "__TIME__", "__TIMESTAMP__=\"x\""} {
		if !ignoredDefine(d) {
			t.Errorf("%q should be ignored", d)
		}
	}
	for _, d := range []string{"ZLIB_DLL", "HAVE_ZLIB=1", "NDEBUG"} {
		if ignoredDefine(d) {
			t.Errorf("%q should NOT be ignored", d)
		}
	}
}

func TestTuKey(t *testing.T) {
	// cmake absolute path under cmakeSrc, and bazel exec-root-relative under
	// bazelPkg, land on the SAME dir-qualified key (so they match + stay distinct
	// from a same-named file in another dir).
	if k := tuKey("/tmp/vtk/Common/Misc/vtkErrorCode.cxx", "/tmp/vtk"); k != "Common/Misc/vtkErrorCode.cxx" {
		t.Errorf("cmake key = %q", k)
	}
	if k := tuKey("elements/vtk/Common/Misc/vtkErrorCode.cxx", "elements/vtk"); k != "Common/Misc/vtkErrorCode.cxx" {
		t.Errorf("bazel key = %q", k)
	}
	// Same basename, different dirs -> distinct keys (no collapse).
	a := tuKey("/tmp/p/a/util.c", "/tmp/p")
	b := tuKey("/tmp/p/b/util.c", "/tmp/p")
	if a == b {
		t.Errorf("same-basename TUs collapsed: %q == %q", a, b)
	}
	// Unrelativizable -> basename fallback.
	if k := tuKey("/other/x.c", "/tmp/p"); k != "x.c" {
		t.Errorf("fallback key = %q want x.c", k)
	}
}

func TestHasParamFileArg(t *testing.T) {
	if !hasParamFileArg([]string{"/usr/bin/gcc", "@bazel-out/k8/bin/foo.params"}) {
		t.Error("should detect @param-file")
	}
	if hasParamFileArg([]string{"gcc", "-DFOO", "-c", "a.c"}) {
		t.Error("should not flag a normal argv")
	}
}

func TestSplitCommand_QuotedDefine(t *testing.T) {
	// Space-bearing quoted define stays ONE token (was the strings.Fields bug).
	got := splitCommand(`gcc "-DGREETING=\"hello world\"" -c foo.c`)
	want := []string{"gcc", `-DGREETING="hello world"`, "-c", "foo.c"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("got %q want %q", got, want)
	}
	// Plain args unchanged.
	if g := splitCommand("gcc -DFOO -I/inc -c a.c"); strings.Join(g, "|") != "gcc|-DFOO|-I/inc|-c|a.c" {
		t.Errorf("plain split = %q", g)
	}
	// Escaped quotes (no spaces) become literal quotes in the value.
	if g := splitCommand(`gcc -DV=\"1.2.3\"`); strings.Join(g, "|") != `gcc|-DV="1.2.3"` {
		t.Errorf("escaped-quote split = %q", g)
	}
}

func TestDiff_ParamSkipAndGenRoot(t *testing.T) {
	cmake := map[string][]tuFacts{
		"a/x.c": {{IncludeDir: map[string]bool{"/b/build/gen": true}, Defines: map[string]bool{}}}, // gen root, no src includes
		"a/y.c": {{IncludeDir: map[string]bool{}, Defines: map[string]bool{}}},                     // param-skipped on bazel side
	}
	bazel := map[string][]tuFacts{
		"a/x.c": {{IncludeDir: map[string]bool{}, Defines: map[string]bool{}}}, // no gen counterpart
	}
	o := normOpts{cmakeBuild: "/b/build"}
	r := diff(cmake, bazel, map[string]bool{"a/y.c": true}, o)
	// y.c is param-skipped -> NOT reported as only-in-cmake.
	for _, k := range r.OnlyCmake {
		if k == "a/y.c" {
			t.Error("param-skipped TU should not be only_cmake")
		}
	}
	// x.c: cmake has a gen: root, bazel has none -> GenRootMissing.
	if _, ok := r.GenRootMissing["a/x.c"]; !ok {
		t.Errorf("expected gen-root-missing for a/x.c; got %v", r.GenRootMissing)
	}
}

// TestDiff_MultiVariantUnion pins the variant-aware comparison: a source
// compiled MORE THAN ONCE under different flags (fmt's gtest-extra.cc — plain in
// test-main, with FMT_HEADER_ONLY in each header-only test) must NOT report a
// define as missing/extra just because the two sides paired different variants.
// The per-source UNION matches, so it's clean; a define in one side's union and
// not the other's is still a real drift.
func TestDiff_MultiVariantUnion(t *testing.T) {
	cmake := map[string][]tuFacts{
		"gtest-extra.cc": {
			{Defines: map[string]bool{"GTEST": true}, IncludeDir: map[string]bool{}},                            // plain (test-main)
			{Defines: map[string]bool{"GTEST": true, "FMT_HEADER_ONLY=1": true}, IncludeDir: map[string]bool{}}, // header-only test
		},
	}
	bazel := map[string][]tuFacts{
		"gtest-extra.cc": {
			// Same two variants, encountered in the opposite order — the union is
			// identical, so NO define mismatch despite the per-variant difference.
			{Defines: map[string]bool{"GTEST": true, "FMT_HEADER_ONLY=1": true}, IncludeDir: map[string]bool{}},
			{Defines: map[string]bool{"GTEST": true}, IncludeDir: map[string]bool{}},
		},
	}
	r := diff(cmake, bazel, nil, normOpts{})
	if d, ok := r.DefineMismatch["gtest-extra.cc"]; ok {
		t.Errorf("multi-variant source falsely flagged: %+v", d)
	}
	// A define in bazel's union but NO cmake variant IS still a real drift.
	bazel["gtest-extra.cc"][0].Defines["WRONG=1"] = true
	r2 := diff(cmake, bazel, nil, normOpts{})
	d, ok := r2.DefineMismatch["gtest-extra.cc"]
	found := false
	for _, x := range d.ExtraInBazel {
		if x == "WRONG=1" {
			found = true
		}
	}
	if !ok || !found {
		t.Errorf("a define only in bazel's union must still surface as extra; got %+v", r2.DefineMismatch)
	}
}

func TestInterestingCopt_ToolchainNoise(t *testing.T) {
	for _, drop := range []string{"-no-canonical-prefixes", "--sysroot=/x", "-fdebug-prefix-map=a=b", "-ffile-prefix-map=a=b"} {
		if interestingCopt(drop) {
			t.Errorf("toolchain-noise flag %q should be filtered", drop)
		}
	}
	for _, keep := range []string{"-fvisibility=hidden", "-fno-rtti", "-fopenmp", "-march=native"} {
		if !interestingCopt(keep) {
			t.Errorf("semantic flag %q should be kept", keep)
		}
	}
}

func TestSourceFromArgv(t *testing.T) {
	if got := sourceFromArgv([]string{"gcc", "-c", "a/b.cc", "-o", "b.o"}); got != "a/b.cc" {
		t.Errorf("got %q want a/b.cc", got)
	}
	// no -c: fall back to the lone source-extension arg
	if got := sourceFromArgv([]string{"gcc", "-O2", "x/y.c", "-o", "y.o"}); got != "x/y.c" {
		t.Errorf("got %q want x/y.c", got)
	}
}

func TestDiff_DefineDelta(t *testing.T) {
	cmake := map[string][]tuFacts{
		"foo.cc": {{Defines: map[string]bool{"A": true, "B": true}, Std: "c++17", IncludeDir: map[string]bool{}}},
		"bar.cc": {{Defines: map[string]bool{"X": true}, IncludeDir: map[string]bool{}}},
	}
	bazel := map[string][]tuFacts{
		// foo: extra ZLIB_DLL, missing B, std mismatch
		"foo.cc": {{Defines: map[string]bool{"A": true, "ZLIB_DLL": true}, Std: "c++20", IncludeDir: map[string]bool{}}},
		"bar.cc": {{Defines: map[string]bool{"X": true}, IncludeDir: map[string]bool{}}},
	}
	r := diff(cmake, bazel, nil, normOpts{})
	if r.Matched != 2 {
		t.Fatalf("matched = %d want 2", r.Matched)
	}
	d, ok := r.DefineMismatch["foo.cc"]
	if !ok {
		t.Fatalf("expected foo.cc define mismatch")
	}
	if len(d.MissingInBazel) != 1 || d.MissingInBazel[0] != "B" {
		t.Errorf("missing-in-bazel = %v want [B]", d.MissingInBazel)
	}
	if len(d.ExtraInBazel) != 1 || d.ExtraInBazel[0] != "ZLIB_DLL" {
		t.Errorf("extra-in-bazel = %v want [ZLIB_DLL]", d.ExtraInBazel)
	}
	if _, ok := r.DefineMismatch["bar.cc"]; ok {
		t.Errorf("bar.cc should have no define mismatch")
	}
	if v, ok := r.StdMismatch["foo.cc"]; !ok || v[0] != "c++17" || v[1] != "c++20" {
		t.Errorf("std mismatch = %v ok=%v", v, ok)
	}
}

func TestLibIdentity(t *testing.T) {
	cases := map[string]string{
		"-lpthread": "pthread",
		"-lstdc++":  "stdc++",
		"-lm":       "m",
		"/usr/lib/x86_64-linux-gnu/libpthread.so.0": "pthread",
		"/usr/lib/libz.a":           "", // z is not in the system allowlist (it's zlib)
		"-lelements_Szlib_Slibzlib": "", // project archive, not a system lib
		"-O3":                       "",
	}
	for in, want := range cases {
		if got := libIdentity(in); got != want {
			t.Errorf("libIdentity(%q) = %q want %q", in, got, want)
		}
	}
}

func TestOrderInversions(t *testing.T) {
	// same order -> none
	if inv := orderInversions([]string{"m", "pthread", "dl"}, []string{"m", "pthread", "dl"}); len(inv) != 0 {
		t.Errorf("same order should have no inversions, got %v", inv)
	}
	// pthread/m swapped -> one inversion (only common libs considered; rt absent in a)
	inv := orderInversions([]string{"m", "pthread"}, []string{"pthread", "m", "rt"})
	if len(inv) != 1 || inv[0] != "m<->pthread" {
		t.Errorf("got %v want [m<->pthread]", inv)
	}
}

func TestDemangleBazelSolib(t *testing.T) {
	cases := map[string]string{
		"-lelements_Szlib_Slibzlib": "zlib", // elements/zlib/libzlib -> zlib
		"-lelements_Scurl_Slibcurl": "curl", // -> curl
		"-lstdc++":                  "",     // no _S: not a mangled project solib
		"-lpthread":                 "",     // system, not mangled
		"/usr/lib/libfoo.a":         "",     // not a -l ref
	}
	for in, want := range cases {
		if got := demangleBazelSolib(in); got != want {
			t.Errorf("demangleBazelSolib(%q) = %q want %q", in, got, want)
		}
	}
}

func TestOrderedLibIdentities(t *testing.T) {
	n2t := map[string]string{"libcurl.a": "libcurl", "libz.so": "zlib"}
	// cmake side: in-tree path fragments resolve via the NameOnDisk map; system
	// libs as paths; order preserved.
	cmake := []string{"lib/libcurl.a", "/usr/lib/x86_64-linux-gnu/libz.so.1", "-lpthread"}
	got := orderedLibIdentities(cmake, n2t)
	want := []string{"tgt:libcurl", "tgt:zlib", "sys:pthread"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("cmake order = %v want %v", got, want)
	}
	// bazel side: demangled solib + system, no map needed.
	bz := []string{"-lelements_Scurl_Slibcurl", "-lpthread"}
	if g := orderedLibIdentities(bz, nil); strings.Join(g, ",") != "tgt:curl,sys:pthread" {
		t.Errorf("bazel order = %v", g)
	}
}

// TestFactsFromArgv_IDirAfter routes -idirafter dirs (joined and
// separate-pair) through IncludeDir like -I/-isystem — cmake's absolute
// host dir and the converted exec-root form are the same compiler input
// in two path spaces, reconciled by the include normalization. Raw-token
// comparison would flag every TU of an -idirafter project.
func TestFactsFromArgv_IDirAfter(t *testing.T) {
	f := factsFromArgv([]string{"-idirafter/tmp/SDL/src/video/khronos", "-idirafter", "extra", "-O2"})
	for _, want := range []string{"/tmp/SDL/src/video/khronos", "extra"} {
		if !f.IncludeDir[want] {
			t.Errorf("IncludeDir missing %q: %v", want, f.IncludeDir)
		}
	}
	for c := range f.Copts {
		if strings.Contains(c, "idirafter") {
			t.Errorf("-idirafter leaked into opaque copts: %v", f.Copts)
		}
	}
}
