package lower

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/internal/fileapi"
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
		{
			// -S without -B configures into the cwd (which WORKING_DIRECTORY
			// moves); recognized, build dir deferred to the resolver.
			name: "configure -S without -B",
			argv: []string{"cmake", "-S", "/src"},
			want: nestedCMakeShape{kind: "configure", srcDir: "/src"},
			ok:   true,
		},
		{name: "lone -B with no source is not nested", argv: []string{"cmake", "-B", "/b"}, ok: false},
		{
			name: "configure positional source (no -S/-B; build dir from WORKING_DIRECTORY)",
			argv: []string{"cmake", "-G", "Ninja", "."},
			want: nestedCMakeShape{kind: "configure", srcDir: "."},
			ok:   true,
		},
		{
			name: "configure positional source, multi-config generator value not taken as source",
			argv: []string{"/usr/bin/cmake", "-G", "Ninja Multi-Config", "/b/dl"},
			want: nestedCMakeShape{kind: "configure", srcDir: "/b/dl"},
			ok:   true,
		},
		{name: "generator with no source is not nested", argv: []string{"cmake", "-G", "Ninja"}, ok: false},
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

func TestNestedElementRoot(t *testing.T) {
	outer := "/proj/src"
	for _, tc := range []struct {
		name      string
		nestedSrc string
		want      string
	}{
		// In-tree nested source → promote to the outer root (merged labels
		// get the <nested-src-rel>/ prefix).
		{"in-tree subdir", "/proj/src/sub", outer},
		{"equal to outer root", "/proj/src", outer},
		// Build-dir-generated nested source (not under the outer tree) →
		// anchor at its own root, so the lowering doesn't hard-fail (the
		// cryptoauthlib mbedtls-downloader shape).
		{"build-dir source", "/proj/build/mbedtls_downloader", "/proj/build/mbedtls_downloader"},
		{"unrelated abs path", "/elsewhere/dl", "/elsewhere/dl"},
		// Empty keeps the outer root (historical default).
		{"empty", "", outer},
	} {
		if got := nestedElementRoot(outer, tc.nestedSrc); got != tc.want {
			t.Errorf("%s: nestedElementRoot(%q, %q) = %q, want %q", tc.name, outer, tc.nestedSrc, got, tc.want)
		}
	}
}

func TestNestedHasCompiledSources(t *testing.T) {
	// Target-less (a downloader / superbuild bootstrap: UTILITY targets the
	// converter skips, so the lowered package has none) — safe to lift as a
	// no-op even when build-dir-sourced.
	if nestedHasCompiledSources(&ir.Package{}) {
		t.Error("empty package reported compiled sources")
	}
	if nestedHasCompiledSources(nil) {
		t.Error("nil package reported compiled sources")
	}
	// A cc target carrying srcs would merge as labels — build-dir-sourced,
	// those would dangle, so the guard must see it.
	withSrcs := &ir.Package{Targets: []ir.Target{{Name: "sublib", Kind: ir.KindCCLibrary, Srcs: []string{"sub.c"}}}}
	if !nestedHasCompiledSources(withSrcs) {
		t.Error("package with a srcs-bearing target reported no compiled sources")
	}
	withHdrs := &ir.Package{Targets: []ir.Target{{Name: "h", Kind: ir.KindCCLibrary, Hdrs: []string{"h.h"}}}}
	if !nestedHasCompiledSources(withHdrs) {
		t.Error("package with an hdrs-bearing target reported no compiled sources")
	}
	// textual_hdrs (fused-source idiom) and data are file labels too — a
	// build-dir-sourced target carrying only one of them would still dangle,
	// so the guard covers every file-bearing attribute.
	withTextual := &ir.Package{Targets: []ir.Target{{Name: "t", Kind: ir.KindCCLibrary, TextualHdrs: []string{"t.inc"}}}}
	if !nestedHasCompiledSources(withTextual) {
		t.Error("package with a textual_hdrs-only target reported no compiled sources")
	}
	withData := &ir.Package{Targets: []ir.Target{{Name: "d", Kind: ir.KindCCLibrary, Data: []string{"d.bin"}}}}
	if !nestedHasCompiledSources(withData) {
		t.Error("package with a data-only target reported no compiled sources")
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
	// Relative -B with WORKING_DIRECTORY resolves against it (the cmake
	// process cwd): -B subbuild under WD /b/deps → /b/deps/subbuild,
	// anchored under the outer build root as deps/subbuild.
	moved := relative
	moved.WorkingDirectory = "/b/deps"
	if ref := recoverNestedCMakeCall(moved, anc, cc); ref != nil {
		t.Fatalf("relative -B with WORKING_DIRECTORY refused: %+v", ref)
	}
	if got := cc.NestedConfigureSink["deps/subbuild"]; got != "/s/sub" {
		t.Fatalf("sink[deps/subbuild] = %q, want /s/sub", got)
	}
}

func TestRecoverNestedCMakeCall_WorkingDirectoryForm(t *testing.T) {
	// The download/build-at-configure idiom (cryptoauthlib's mbedtls
	// downloader): a positional-source configure and `cmake --build .`,
	// both run IN the nested build dir via WORKING_DIRECTORY.
	anc := execAnchors{hostBuildDir: "/b", recordedBuildDir: "/b", hostSrcDir: "/s", recordedSrcDir: "/s"}
	cc := newCodegenContext()
	wd := "/b/mbedtls_downloader"
	configure := shadow.ExecuteProcessCall{
		File: "/s/cmake/mbedtls.cmake", Line: 5,
		Commands:         [][]string{{"cmake", "-G", "Ninja Multi-Config", "."}},
		WorkingDirectory: wd,
	}
	build := shadow.ExecuteProcessCall{
		File: "/s/cmake/mbedtls.cmake", Line: 7,
		Commands:         [][]string{{"cmake", "--build", "."}},
		WorkingDirectory: wd,
	}
	if ref := recoverNestedCMakeCall(configure, anc, cc); ref != nil {
		t.Fatalf("positional+WD configure refused: %+v", ref)
	}
	if ref := recoverNestedCMakeCall(build, anc, cc); ref != nil {
		t.Fatalf("`cmake --build .`+WD companion refused: %+v", ref)
	}
	// The in-source nested root (where the configure_file'd CMakeLists lives)
	// is the working directory; the --build is its companion, not a 2nd entry.
	if got := cc.NestedConfigureSink["mbedtls_downloader"]; got != wd {
		t.Fatalf("sink[mbedtls_downloader] = %q, want %q", got, wd)
	}
	if len(cc.NestedConfigureSink) != 1 {
		t.Fatalf("sink has %d entries, want 1", len(cc.NestedConfigureSink))
	}
	// Classify routes both calls to the nested bucket.
	for _, call := range []shadow.ExecuteProcessCall{configure, build} {
		if got := Classify(call); got.Bucket != BucketNestedCMake {
			t.Errorf("bucket = %q (%s), want nested-cmake", got.Bucket, got.Reason)
		}
	}
	// A positional-source configure with NO WORKING_DIRECTORY can't resolve
	// its build dir — it declines (not classified nested), rather than
	// reconfiguring the outer cwd.
	noWD := configure
	noWD.WorkingDirectory = ""
	if got, ok := classifyNestedCMake("cmake", noWD); ok {
		t.Errorf("positional configure without WORKING_DIRECTORY classified nested: %+v", got)
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

// TestDetectNestedConfigures covers the driver-side worklist's detection
// step over a nested build's raw trace: only LIFTABLE grandchild
// configures surface (the guards mirror classifyNestedCMake +
// recoverNestedCMakeCall, so detection and lift can't drift) — captured
// output, relative -B under a moved WORKING_DIRECTORY, dirs outside the
// nested build dir, and --build/--install companions all skip.
func TestDetectNestedConfigures(t *testing.T) {
	line := func(args ...string) string {
		quoted := make([]string, len(args))
		for i, a := range args {
			quoted[i] = `"` + a + `"`
		}
		return `{"args":[` + strings.Join(quoted, ",") + `],"cmd":"execute_process","file":"/s/sub/CMakeLists.txt","line":5}`
	}
	trace := []byte(strings.Join([]string{
		// Absolute -B under the nested build dir → detected.
		line("COMMAND", "cmake", "-S", "/s/sub/subsub", "-B", "/b/subbuild/subsubbuild", "-G", "Ninja"),
		// Relative -B (cwd contract = the nested build root) → detected.
		line("COMMAND", "cmake", "-S", "/s/sub/other", "-B", "otherbuild"),
		// Captured output: refuses at lowering → not staged.
		line("COMMAND", "cmake", "-S", "/s/sub/cap", "-B", "/b/subbuild/capbuild", "OUTPUT_VARIABLE", "OUT"),
		// Relative -B with a moved cwd: refuses at lowering → not staged.
		line("COMMAND", "cmake", "-S", "/s/sub/moved", "-B", "movedbuild", "WORKING_DIRECTORY", "/b/subbuild/deps"),
		// Build dir OUTSIDE the nested build dir → not liftable.
		line("COMMAND", "cmake", "-S", "/s/sub/esc", "-B", "/tmp/escape"),
		// --build companion: the configure's lift covers it → skipped.
		line("COMMAND", "cmake", "--build", "/b/subbuild/subsubbuild"),
	}, "\n") + "\n")
	got := DetectNestedConfigures(trace, "/s/sub", "/b/subbuild")
	want := map[string]string{
		"subsubbuild": "/s/sub/subsub",
		"otherbuild":  "/s/sub/other",
	}
	if len(got) != len(want) {
		t.Fatalf("detected = %v, want %v", got, want)
	}
	for rel, src := range want {
		if got[rel] != src {
			t.Errorf("detected[%q] = %q, want %q", rel, got[rel], src)
		}
	}
	if DetectNestedConfigures(nil, "/s/sub", "/b/subbuild") != nil {
		t.Error("nil trace must detect nothing")
	}
}

// TestLowerOneNestedBuild_ThreadsChildren pins the recursion plumbing:
// a NestedBuildInput's Children reach the recursive ToIR as its
// Options.NestedBuilds, so the grandchild lowers inside the child and
// the child package carries the (re-homed) grandchild rules when it
// merges outward. The composition itself is exercised end-to-end by
// scripts/meta-cmake-nested-cmake.sh's depth-2 fixture.
func TestLowerOneNestedBuild_ThreadsChildren(t *testing.T) {
	root := t.TempDir()
	childSrc := filepath.Join(root, "sub")
	childBuild := filepath.Join(root, "b", "subbuild")
	grandSrc := filepath.Join(childSrc, "subsub")
	grandBuild := filepath.Join(childBuild, "subsubbuild")
	for _, d := range []string{childSrc, grandSrc, grandBuild} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// The grandchild's configure wrote a header into its build dir; the
	// child reply itself carries no targets (header-only chain link).
	if err := os.WriteFile(filepath.Join(grandBuild, "subsub_config.h"), []byte("#define V 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	emptyReply := func(src, build string) *fileapi.Reply {
		return &fileapi.Reply{
			Codemodel: fileapi.Codemodel{
				Paths:          fileapi.CodemodelPaths{Source: src, Build: build},
				Configurations: []fileapi.Configuration{{Name: "Release"}},
			},
		}
	}
	nb := NestedBuildInput{
		BuildRel:     "subbuild",
		SrcDir:       childSrc,
		Reply:        emptyReply(childSrc, childBuild),
		HostBuildDir: childBuild,
		Children: []NestedBuildInput{{
			BuildRel:     "subsubbuild",
			SrcDir:       grandSrc,
			Reply:        emptyReply(grandSrc, grandBuild),
			HostBuildDir: grandBuild,
		}},
	}
	pkg, err := lowerOneNestedBuild(nb, Options{}, root)
	if err != nil {
		t.Fatalf("lowerOneNestedBuild: %v", err)
	}
	// The grandchild's configure-generated header bakes inside the CHILD
	// lowering at the grandchild's child-relative home — proof the
	// Children threading reached the recursive ToIR's nested pass.
	found := false
	for _, tgt := range pkg.Targets {
		for _, out := range producerOuts(&tgt) {
			if out == "subsubbuild/subsub_config.h" {
				found = true
			}
		}
	}
	if !found {
		t.Fatalf("grandchild header not baked at its child-relative home; targets: %+v", pkg.Targets)
	}
}

// TestApplyNestedProducerReHome_ConfigureFileLift pins the two-site
// re-home pairing for the configure_file LIFT tier (the guard
// producerOuts' comment used to flag): the lift rule's Out enters the
// re-home map via producerOuts AND applyNestedProducerReHome re-anchors
// it with the chain-composed rename — site (1) without site (2) would
// map the out but never apply it, materializing the file at the outer
// package root under a collidable name. Copy-on-write on the spec: the
// nested package's original must not mutate.
func TestApplyNestedProducerReHome_ConfigureFileLift(t *testing.T) {
	spec := &ir.CMakeConfigureFileSpec{
		Out:      "sub_config.h",
		Template: "sub/sub_config.h.in",
		Values:   map[string]string{"SUB_VALUE": "7"},
	}
	lift := ir.Target{
		Name:               "gen_sub_config_h",
		Kind:               ir.KindCMakeConfigureFile,
		CMakeConfigureFile: spec,
	}
	if got := producerOuts(&lift); len(got) != 1 || got[0] != "sub_config.h" {
		t.Fatalf("producerOuts(lift) = %v, want [sub_config.h]", got)
	}
	rehome := map[string]string{"sub_config.h": "subbuild/sub_config.h"}
	applyNestedProducerReHome(&lift, rehome, "subbuild")
	if lift.CMakeConfigureFile.Out != "subbuild/sub_config.h" {
		t.Errorf("lift Out = %q, want subbuild/sub_config.h", lift.CMakeConfigureFile.Out)
	}
	if lift.Name != "subbuild_gen_sub_config_h" {
		t.Errorf("lift name = %q, want chain-composed subbuild_gen_sub_config_h", lift.Name)
	}
	if spec.Out != "sub_config.h" {
		t.Errorf("original spec mutated to %q — the re-home must copy-on-write", spec.Out)
	}
	// A consumer's hdrs entry pointing at the re-homed rel re-points too.
	consumer := ir.Target{Kind: ir.KindCCLibrary, Hdrs: []string{"sub_config.h"}}
	applyNestedProducerReHome(&consumer, rehome, "subbuild")
	if consumer.Hdrs[0] != "subbuild/sub_config.h" {
		t.Errorf("consumer hdrs = %v, want the re-homed rel", consumer.Hdrs)
	}
}

// TestNestedBuildFullyRecovered: a NOT-lifted nested build is "fully recovered"
// (todo redundant) only when it's header-only AND every header was claimed; a
// compiled artifact or an unclaimed header keeps it not-recovered (todo stands).
func TestNestedBuildFullyRecovered(t *testing.T) {
	build := t.TempDir()
	writeTree(t, build, "sub/config.h", "#define X 1\n")
	cc := newCodegenContext()
	if nestedBuildFullyRecovered("sub", build, cc) {
		t.Error("an unclaimed header must NOT count as fully recovered")
	}
	cc.OutToGenrule["sub/config.h"] = "baked_sub_config_h" // the outer build-dir bake claimed it
	if !nestedBuildFullyRecovered("sub", build, cc) {
		t.Error("header-only + every header claimed → fully recovered")
	}
	writeTree(t, build, "sub/libfoo.a", "ar")
	if nestedBuildFullyRecovered("sub", build, cc) {
		t.Error("a library artifact means real targets are missing → not fully recovered")
	}
	if nestedBuildFullyRecovered("sub", "", cc) {
		t.Error("no live build dir → cannot be recovered")
	}
}

// TestWarnUnliftedNestedBuilds_SuppressesFullyRecovered: the not-lifted todo is
// suppressed for a header-only fully-recovered nested build, but still fires
// when the build produces a library (real targets missing).
func TestWarnUnliftedNestedBuilds_SuppressesFullyRecovered(t *testing.T) {
	build := t.TempDir()
	writeTree(t, build, "sub/config.h", "x")
	cc := newCodegenContext()
	cc.NestedConfigureSink["sub"] = "/src/sub" // detected, not lifted
	cc.OutToGenrule["sub/config.h"] = "baked"  // recovered via the outer bake

	c := todos.New()
	warnUnliftedNestedBuilds(Options{Todos: c, BuildDir: build}, cc)
	if c.Len() != 0 {
		t.Errorf("fully-recovered header-only nested build should emit no todo; got %d", c.Len())
	}

	writeTree(t, build, "sub/libfoo.a", "ar") // now it produces a library
	c2 := todos.New()
	warnUnliftedNestedBuilds(Options{Todos: c2, BuildDir: build}, cc)
	if c2.Len() != 1 {
		t.Errorf("a library-producing nested build should still emit the not-lifted todo; got %d", c2.Len())
	}
}

// nestedOptionsClass classifies every Options field by how nestedOptionsFor
// must treat it. The TestNestedOptionsForClassification guard asserts this
// covers the live struct EXACTLY, so a newly added Options field fails the
// build until its propagation policy is decided here on purpose:
//
//   - "perNested": set from the NestedBuildInput, NOT the outer opts.
//   - "forward":   copied verbatim from the outer opts (operator dials +
//     host-independent context that a nested build must honor too).
//   - "drop":      deliberately NOT forwarded — pass-1 orchestration sinks and
//     outer-configure-derived data that would cross-contaminate a nested run.
var nestedOptionsClass = map[string]string{
	"HostSourceRoot":    "perNested",
	"ElementSourceRoot": "perNested",
	"BuildDir":          "perNested",
	"TraceRaw":          "perNested",
	"NestedBuilds":      "perNested",

	"LiftConfigureFile":                 "forward",
	"Imports":                           "forward",
	"BazelPackagePath":                  "forward",
	"CMakeVars":                         "forward",
	"Coverage":                          "forward",
	"Todos":                             "forward",
	"Warnings":                          "forward",
	"BakeIn":                            "forward",
	"CMakeScriptRunner":                 "forward",
	"CMakeScriptBake":                   "forward",
	"CMakeScriptTrace":                  "forward",
	"LiftCCEmbed":                       "forward",
	"LiftCCHash":                        "forward",
	"RecognizeCodegen":                  "forward",
	"ExtraCodegenRecognizers":           "forward",
	"LiftDerivedCodegen":                "forward",
	"EmitStandaloneCustomCommands":      "forward",
	"LiftDownload":                      "forward",
	"DownloadRepos":                     "forward",
	"BackedFeatures":                    "forward",
	"Fidelity":                          "forward",
	"UnsupportedExecuteProcessFallback": "forward",
	"FallbackInstallTarget":             "forward",
	"Rejections":                        "forward",
	"RecoverSourceComments":             "forward",
	"EmitSharedLibraries":               "forward",
	"EmitInstallExportConfig":           "forward",

	"HostPrefixDir":          "drop",
	"CTest":                  "drop",
	"SetAssignments":         "drop",
	"ParentScopeForwards":    "drop",
	"StampVarSink":           "drop",
	"NestedConfigureSink":    "drop",
	"CaptureRefusalSink":     "drop",
	"DeadCaptureVars":        "drop",
	"NonExpandedFileWriters": "drop",
	"GenexProbes":            "drop",
	"ConfigureLog":           "drop",
	"LiteralProbeSink":       "drop",
	"LiteralResolutions":     "drop",
}

// TestNestedOptionsForClassification forces every Options field to carry an
// explicit forward/drop/perNested decision: if a field is added to Options
// without being classified above, this fails — the guard that caught the
// pre-existing drift where most operator opt-in dials silently never reached
// a nested lowering.
func TestNestedOptionsForClassification(t *testing.T) {
	ot := reflect.TypeOf(Options{})
	live := map[string]bool{}
	for i := 0; i < ot.NumField(); i++ {
		name := ot.Field(i).Name
		live[name] = true
		if _, ok := nestedOptionsClass[name]; !ok {
			t.Errorf("Options field %q is unclassified for nested lowering — add it to nestedOptionsClass (forward it in nestedOptionsFor, or document why it must be dropped)", name)
		}
	}
	for name := range nestedOptionsClass {
		if !live[name] {
			t.Errorf("nestedOptionsClass names %q, which is not a live Options field — stale entry", name)
		}
	}
}

// setNonZeroField sets v to a recognizably non-zero value for the kinds the
// Options struct uses, so TestNestedOptionsFor can assert which fields survive
// nestedOptionsFor (forwarded) and which it clears (dropped).
func setNonZeroField(v reflect.Value) {
	switch v.Kind() {
	case reflect.String:
		v.SetString("x")
	case reflect.Bool:
		v.SetBool(true)
	case reflect.Slice:
		v.Set(reflect.MakeSlice(v.Type(), 1, 1))
	case reflect.Map:
		v.Set(reflect.MakeMap(v.Type()))
	case reflect.Ptr:
		v.Set(reflect.New(v.Type().Elem()))
	case reflect.Interface:
		v.Set(reflect.ValueOf(os.Stderr)) // the only interface field is Warnings (io.Writer)
	}
}

// TestNestedOptionsFor sets EVERY outer Options field non-zero, then asserts —
// per nestedOptionsClass — that forwarded fields carry the outer value and
// dropped fields are cleared. This ties the code's explicit clear-list in
// nestedOptionsFor to the classification: forget to clear an outer-scoped
// field (default-forward leaks it) and the drop assertion fires.
func TestNestedOptionsFor(t *testing.T) {
	var outer Options
	ov := reflect.ValueOf(&outer).Elem()
	ot := ov.Type()
	for i := 0; i < ot.NumField(); i++ {
		setNonZeroField(ov.Field(i))
	}
	nb := NestedBuildInput{SrcDir: "/src/sub", HostBuildDir: "/b/sub"}

	got := nestedOptionsFor(nb, outer, "/element/root")
	gv := reflect.ValueOf(got)

	// per-nested context comes from the NestedBuildInput, not the outer opts.
	if got.HostSourceRoot != nb.SrcDir || got.BuildDir != nb.HostBuildDir || got.ElementSourceRoot != "/element/root" {
		t.Errorf("per-nested fields wrong: HostSourceRoot=%q BuildDir=%q ElementSourceRoot=%q", got.HostSourceRoot, got.BuildDir, got.ElementSourceRoot)
	}
	for i := 0; i < ot.NumField(); i++ {
		name := ot.Field(i).Name
		switch nestedOptionsClass[name] {
		case "forward":
			if !reflect.DeepEqual(gv.Field(i).Interface(), ov.Field(i).Interface()) {
				t.Errorf("forward field %q did not propagate into nested lowering", name)
			}
		case "drop":
			if !gv.Field(i).IsZero() {
				t.Errorf("drop field %q must be cleared in nested lowering (default-forward leaked it)", name)
			}
		}
	}
}
