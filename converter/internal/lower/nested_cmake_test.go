package lower

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/internal/todos"
	"github.com/sstriker/buildstream-bazel/converter/ir"
	"github.com/sstriker/buildstream-bazel/internal/shadow"
)

func TestParseNestedCMakeArgv(t *testing.T) {
	for _, tc := range []struct {
		name string
		argv []string
		want nestedCMakeShape
		ok   bool
	}{
		{
			name: "configure separated",
			argv: []string{"/usr/bin/cmake", "-S", "/src/sub", "-B", "/b/subbuild", "-G", "Ninja"},
			want: nestedCMakeShape{kind: "configure", srcDir: "/src/sub", buildDir: "/b/subbuild"},
			ok:   true,
		},
		{
			name: "configure joined, order swapped",
			argv: []string{"cmake", "-B/b/sb", "-S/src/sub"},
			want: nestedCMakeShape{kind: "configure", srcDir: "/src/sub", buildDir: "/b/sb"},
			ok:   true,
		},
		{
			name: "build",
			argv: []string{"cmake", "--build", "/b/subbuild"},
			want: nestedCMakeShape{kind: "build", buildDir: "/b/subbuild"},
			ok:   true,
		},
		{
			name: "install",
			argv: []string{"cmake", "--install", "/b/subbuild"},
			want: nestedCMakeShape{kind: "install", buildDir: "/b/subbuild"},
			ok:   true,
		},
		{name: "build without dir", argv: []string{"cmake", "--build"}, ok: false},
		{name: "configure missing -B", argv: []string{"cmake", "-S", "/src"}, ok: false},
		{name: "cmake -E is not nested", argv: []string{"cmake", "-E", "copy", "a", "b"}, ok: false},
		{name: "cmake -P is not nested", argv: []string{"cmake", "-P", "x.cmake"}, ok: false},
		{name: "non-cmake driver", argv: []string{"make", "-C", "/b"}, ok: false},
	} {
		driver := executeProcessDriverBasename(tc.argv[0])
		got, ok := parseNestedCMakeArgv(driver, tc.argv)
		if ok != tc.ok {
			t.Errorf("%s: ok = %v, want %v", tc.name, ok, tc.ok)
			continue
		}
		if ok && got != tc.want {
			t.Errorf("%s: got %+v, want %+v", tc.name, got, tc.want)
		}
	}
}

func TestClassifyNestedCMake(t *testing.T) {
	configure := shadow.ExecuteProcessCall{
		File: "/src/CMakeLists.txt", Line: 4,
		Commands: [][]string{{"/usr/bin/cmake", "-S", "/src/sub", "-B", "/b/subbuild"}},
	}
	// RESULT_VARIABLE (exit-status-as-answer) keeps the nested bucket.
	withResult := configure
	withResult.ResultVariable = "_rc"
	for _, call := range []shadow.ExecuteProcessCall{configure, withResult} {
		if got := Classify(call); got.Bucket != BucketNestedCMake {
			t.Errorf("bucket = %q (%s), want nested-cmake", got.Bucket, got.Reason)
		}
	}
	// Byte captures refuse — the lift can't reproduce captured output.
	captured := configure
	captured.OutputVariable = "OUT"
	if got := Classify(captured); got.Bucket != BucketRefuse {
		t.Errorf("captured-output nested configure: bucket = %q, want refuse", got.Bucket)
	}
}

func TestRecoverNestedCMakeCall_SinkAndCompanions(t *testing.T) {
	anc := execAnchors{hostBuildDir: "/b", recordedBuildDir: "/b", hostSrcDir: "/s", recordedSrcDir: "/s"}
	cc := newCodegenContext()
	configure := shadow.ExecuteProcessCall{
		File: "/s/CMakeLists.txt", Line: 4,
		Commands: [][]string{{"cmake", "-S", "/s/sub", "-B", "/b/subbuild"}},
	}
	build := shadow.ExecuteProcessCall{
		File: "/s/CMakeLists.txt", Line: 8,
		Commands: [][]string{{"cmake", "--build", "/b/subbuild"}},
	}
	if ref := recoverNestedCMakeCall(configure, anc, cc); ref != nil {
		t.Fatalf("configure refused: %+v", ref)
	}
	if ref := recoverNestedCMakeCall(build, anc, cc); ref != nil {
		t.Fatalf("companion --build refused: %+v", ref)
	}
	if got := cc.NestedConfigureSink["subbuild"]; got != "/s/sub" {
		t.Fatalf("sink[subbuild] = %q, want /s/sub", got)
	}
	// Orphan companion (no configure) records the dir with empty src.
	orphan := shadow.ExecuteProcessCall{
		File: "/s/CMakeLists.txt", Line: 12,
		Commands: [][]string{{"cmake", "--build", "/b/other"}},
	}
	if ref := recoverNestedCMakeCall(orphan, anc, cc); ref != nil {
		t.Fatalf("orphan companion refused: %+v", ref)
	}
	if src, seen := cc.NestedConfigureSink["other"]; !seen || src != "" {
		t.Fatalf("orphan companion not recorded: %v %q", seen, src)
	}
	// A nested build OUTSIDE the outer build dir can't be staged → refusal.
	outside := shadow.ExecuteProcessCall{
		File: "/s/CMakeLists.txt", Line: 16,
		Commands: [][]string{{"cmake", "-S", "/s/sub", "-B", "/elsewhere/bb"}},
	}
	if ref := recoverNestedCMakeCall(outside, anc, cc); ref == nil {
		t.Fatal("outside-build-dir nested configure must refuse")
	}
}

func TestEmitNestedCMakeTodos(t *testing.T) {
	c := todos.New()
	sink := map[string]string{"subbuild": "/src/sub"}
	emitNestedCMakeTodos(c, []string{"subbuild"}, sink, "/src", "/b")
	rep := c.Report(todos.Preamble{}, "")
	if len(rep.Todos) != 1 {
		t.Fatalf("todos = %d, want 1", len(rep.Todos))
	}
	td := rep.Todos[0]
	if td.Kind != "nested-cmake-not-lifted" || td.GroupKey != "subbuild" {
		t.Fatalf("todo shape: %+v", td)
	}
	if got := td.Evidence["nested_source"]; got != "<SRC>/sub" {
		t.Fatalf("nested_source = %v, want normalized <SRC>/sub", got)
	}
}

func TestRecoverNestedCMakeCall_RelativeBuildDir(t *testing.T) {
	anc := execAnchors{hostBuildDir: "/b", recordedBuildDir: "/b", hostSrcDir: "/s", recordedSrcDir: "/s"}
	// Plain relative -B anchors against the outer build root (the cmake
	// process cwd under the runner's cmd.Dir contract).
	cc := newCodegenContext()
	relative := shadow.ExecuteProcessCall{
		File: "/s/CMakeLists.txt", Line: 4,
		Commands: [][]string{{"cmake", "-S", "/s/sub", "-B", "subbuild"}},
	}
	if ref := recoverNestedCMakeCall(relative, anc, cc); ref != nil {
		t.Fatalf("relative -B refused: %+v", ref)
	}
	if got := cc.NestedConfigureSink["subbuild"]; got != "/s/sub" {
		t.Fatalf("sink[subbuild] = %q, want /s/sub", got)
	}
	// Relative -B with WORKING_DIRECTORY moves the resolution base —
	// refuse explicitly rather than anchoring a phantom directory.
	moved := relative
	moved.WorkingDirectory = "/b/deps"
	if ref := recoverNestedCMakeCall(moved, anc, cc); ref == nil {
		t.Fatal("relative -B with WORKING_DIRECTORY must refuse")
	}
}

func TestMergeNestedPackage_IncludeRehoming(t *testing.T) {
	srcDir := t.TempDir()
	buildDir := t.TempDir()
	mkdir := func(root, rel string) {
		if err := os.MkdirAll(filepath.Join(root, rel), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	mkdir(srcDir, "common") // sibling SOURCE include under the outer root
	mkdir(buildDir, "gen")  // nested BUILD-dir include subdir
	nb := NestedBuildInput{BuildRel: "subbuild", HostBuildDir: buildDir}
	nested := &ir.Package{Targets: []ir.Target{{
		Name:     "sublib",
		Kind:     ir.KindCCLibrary,
		Includes: []string{".", "gen", "common", "sub/include"},
	}}}
	pkg := &ir.Package{}
	cc := newCodegenContext()
	mergeNestedPackage(pkg, nested, nb, cc, Options{}, srcDir)
	got := pkg.Targets[0].Includes
	want := []string{"subbuild", "subbuild/gen", "common", "sub/include"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("includes = %v, want %v", got, want)
		}
	}
}

// TestExecuteProcessAnchorSource_UmbrellaReanchor: when the label root
// (hostSrcDir) sits ABOVE the cmake source dir — workspace promotion,
// --element-source-root overlays, the nested-cmake recursive lowering —
// a source operand under cmakeSrc must anchor label-root-relative
// ("sub/x.in"), not bare ("x.in"): the emitted genrule's
// srcs/$(location) resolve at the label root. Same-root (the common
// outer case) and disjoint-root (offline replay) shapes keep the
// recorded-relative form.
func TestExecuteProcessAnchorSource_UmbrellaReanchor(t *testing.T) {
	cases := []struct {
		name string
		anc  execAnchors
		p    string
		want string
		ok   bool
	}{
		{
			name: "nested: host root above recorded root",
			anc:  execAnchors{hostSrcDir: "/repo/outer", recordedSrcDir: "/repo/outer/sub"},
			p:    "/repo/outer/sub/x.in",
			want: "sub/x.in", ok: true,
		},
		{
			name: "same roots keep bare rel",
			anc:  execAnchors{hostSrcDir: "/repo/proj", recordedSrcDir: "/repo/proj"},
			p:    "/repo/proj/x.in",
			want: "x.in", ok: true,
		},
		{
			name: "disjoint roots (offline replay) keep recorded rel",
			anc:  execAnchors{hostSrcDir: "/local/checkout", recordedSrcDir: "/recorder/proj"},
			p:    "/recorder/proj/x.in",
			want: "x.in", ok: true,
		},
		{
			name: "outside both roots declines",
			anc:  execAnchors{hostSrcDir: "/repo/outer", recordedSrcDir: "/repo/outer/sub"},
			p:    "/elsewhere/x.in",
			want: "", ok: false,
		},
	}
	for _, tc := range cases {
		got, ok := executeProcessAnchorSource(tc.p, tc.anc)
		if got != tc.want || ok != tc.ok {
			t.Errorf("%s: anchor(%q) = (%q, %v); want (%q, %v)", tc.name, tc.p, got, ok, tc.want, tc.ok)
		}
	}
}
