package lower

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/internal/todos"
	"github.com/sstriker/buildstream-bazel/converter/ir"
)

// TestBakeBuildDirFile covers the on-disk bake's contract: bytes present
// under the host build dir bake into a registered producer; missing
// bytes (offline replay) and a missing host build dir both decline.
func TestBakeBuildDirFile(t *testing.T) {
	dir := t.TempDir()
	mustWriteFile(t, filepath.Join(dir, "gen", "x.c"), "int x;\n")

	cc := newCodegenContext()
	lc := targetLowerCtx{cc: cc, hostBuild: dir}

	name, ok := bakeBuildDirFile("gen/x.c", lc)
	if !ok {
		t.Fatal("bakeBuildDirFile declined although the bytes are on disk")
	}
	if want := bakedBuildDirName("gen/x.c"); name != want {
		t.Errorf("rule name = %q; want %q", name, want)
	}
	if cc.OutToGenrule["gen/x.c"] != name {
		t.Errorf("producer not registered: OutToGenrule[gen/x.c] = %q", cc.OutToGenrule["gen/x.c"])
	}
	if len(cc.Genrules) != 1 {
		t.Fatalf("expected 1 bake rule, got %d", len(cc.Genrules))
	}
	rule := cc.Genrules[0]
	if !stringSliceContains(rule.Tags, "cmake-codegen-build-dir-bake") {
		t.Errorf("bake rule tags = %v; want the cmake-codegen-build-dir-bake facet", rule.Tags)
	}
	if rule.WriteFileOut != "gen/x.c" {
		t.Errorf("bake rule out = %q; want gen/x.c", rule.WriteFileOut)
	}

	if _, ok := bakeBuildDirFile("absent.c", lc); ok {
		t.Error("bakeBuildDirFile baked a file that is not on disk")
	}
	if _, ok := bakeBuildDirFile("gen/x.c", targetLowerCtx{cc: cc}); ok {
		t.Error("bakeBuildDirFile baked without a host build dir")
	}
}

// TestBakeConsumedBuildDirHeaders covers the demand-driven walk: an
// orphan on-disk header under a consumed build-dir include bakes and
// attaches to the consumer; cmake bookkeeping, ninja-built outputs,
// already-produced outputs, non-headers, and headers outside the
// consumed include set all stay out; the walk caches per include dir.
func TestBakeConsumedBuildDirHeaders(t *testing.T) {
	dir := t.TempDir()
	mustWriteFile(t, filepath.Join(dir, "inc", "a.h"), "#define A 1\n")
	mustWriteFile(t, filepath.Join(dir, "inc", "CMakeFiles", "skip.h"), "x")
	mustWriteFile(t, filepath.Join(dir, "inc", "built.h"), "x")
	mustWriteFile(t, filepath.Join(dir, "inc", "produced.h"), "x")
	mustWriteFile(t, filepath.Join(dir, "inc", "notes.txt"), "x")
	mustWriteFile(t, filepath.Join(dir, "other", "b.h"), "x")

	cc := newCodegenContext()
	cc.NinjaOuts = map[string]bool{"inc/built.h": true}
	cc.OutToGenrule["inc/produced.h"] = "existing"
	lc := targetLowerCtx{cc: cc, hostBuild: dir}

	irt := &ir.Target{Name: "consumer", Kind: ir.KindCCLibrary}
	incs := map[string]bool{"inc": true}
	bakeConsumedBuildDirHeaders(irt, lc, incs)

	if len(irt.Hdrs) != 1 || irt.Hdrs[0] != "inc/a.h" {
		t.Fatalf("consumer hdrs = %v; want [inc/a.h]", irt.Hdrs)
	}
	if !stringSliceContains(irt.Tags, "has-cmake-codegen") {
		t.Errorf("consumer tags = %v; want has-cmake-codegen", irt.Tags)
	}
	if cc.OutToGenrule["inc/a.h"] == "" {
		t.Error("walked header's producer not registered")
	}
	if _, baked := cc.BuildDirBakedHdrs["other/b.h"]; baked {
		t.Error("header outside the consumed include set baked")
	}
	for _, rel := range []string{"inc/CMakeFiles/skip.h", "inc/built.h", "inc/notes.txt"} {
		if _, baked := cc.BuildDirBakedHdrs[rel]; baked {
			t.Errorf("%s baked; bookkeeping/ninja-built/non-header files must not", rel)
		}
	}
	if cc.OutToGenrule["inc/produced.h"] != "existing" {
		t.Error("already-produced header's producer overwritten")
	}

	// Second consumer over the same include: cached walk, no duplicate
	// rules, header still attaches.
	rules := len(cc.Genrules)
	irt2 := &ir.Target{Name: "consumer2", Kind: ir.KindCCLibrary}
	bakeConsumedBuildDirHeaders(irt2, lc, incs)
	if len(cc.Genrules) != rules {
		t.Errorf("re-walk emitted duplicate rules: %d -> %d", rules, len(cc.Genrules))
	}
	if len(irt2.Hdrs) != 1 || irt2.Hdrs[0] != "inc/a.h" {
		t.Errorf("second consumer hdrs = %v; want [inc/a.h]", irt2.Hdrs)
	}
}

// TestBakeConsumedBuildDirHeaders_PkgRootInclude: a build-ROOT include
// ("") whose baked header lives in a subdir needs `includes = ["."]` in
// a non-root package (the genfiles-root resolution the configure_file
// attribution applies) — and must NOT get it in a root package, where
// Bazel rejects the entry.
func TestBakeConsumedBuildDirHeaders_PkgRootInclude(t *testing.T) {
	dir := t.TempDir()
	mustWriteFile(t, filepath.Join(dir, "gen", "x.h"), "#define X 1\n")

	cc := newCodegenContext()
	lc := targetLowerCtx{cc: cc, hostBuild: dir, bazelPackagePath: "third_party/proj"}
	irt := &ir.Target{Name: "consumer", Kind: ir.KindCCLibrary}
	bakeConsumedBuildDirHeaders(irt, lc, map[string]bool{"": true})
	if !stringSliceContains(irt.Includes, ".") {
		t.Errorf("includes = %v; want \".\" for a subdir header under a build-root include in a non-root package", irt.Includes)
	}

	ccRoot := newCodegenContext()
	lcRoot := targetLowerCtx{cc: ccRoot, hostBuild: dir, bazelPackagePath: ""}
	irtRoot := &ir.Target{Name: "consumer", Kind: ir.KindCCLibrary}
	bakeConsumedBuildDirHeaders(irtRoot, lcRoot, map[string]bool{"": true})
	if stringSliceContains(irtRoot.Includes, ".") {
		t.Errorf("includes = %v; \".\" must not appear in a root package", irtRoot.Includes)
	}
}

// TestWarnElidedSources covers the loudness backstop: recorded drops
// surface as one aggregate stderr warning (sorted) plus one structured
// source-elided todo per (target, class) with deduped anchors; no
// records means total silence.
func TestWarnElidedSources(t *testing.T) {
	cc := newCodegenContext()
	recordElidedSource(cc, "tgt", "z.c", "build-dir-source")
	recordElidedSource(cc, "tgt", "a.c", "build-dir-source")
	recordElidedSource(cc, "tgt", "a.c", "build-dir-source") // duplicate
	recordElidedSource(cc, "other", "m.c", "missing-source")

	var buf bytes.Buffer
	c := todos.New()
	warnElidedSources(Options{Warnings: &buf, Todos: c}, cc)

	out := buf.String()
	if !strings.Contains(out, "4 codemodel source(s) DROPPED without recovery") {
		t.Errorf("aggregate warning missing/wrong count:\n%s", out)
	}
	if !strings.Contains(out, "tgt: a.c (build-dir-source)") || !strings.Contains(out, "other: m.c (missing-source)") {
		t.Errorf("per-drop lines missing:\n%s", out)
	}

	rep := c.Report(todos.DefaultPreamble(), "")
	if len(rep.Todos) != 2 {
		t.Fatalf("expected 2 todos (one per target|class), got %d", len(rep.Todos))
	}
	for _, td := range rep.Todos {
		if td.Kind != "source-elided" || td.Disposition != todos.Actionable {
			t.Errorf("todo kind/disposition = %q/%q; want source-elided/actionable", td.Kind, td.Disposition)
		}
	}
	var tgt todos.Todo
	for _, td := range rep.Todos {
		if td.GroupKey == "tgt|build-dir-source" {
			tgt = td
		}
	}
	if len(tgt.Anchors) != 2 {
		t.Errorf("anchors = %+v; want 2 deduped (a.c, z.c)", tgt.Anchors)
	}

	// Silence when nothing was dropped.
	var quiet bytes.Buffer
	cQuiet := todos.New()
	warnElidedSources(Options{Warnings: &quiet, Todos: cQuiet}, newCodegenContext())
	if quiet.Len() != 0 || cQuiet.Len() != 0 {
		t.Errorf("warned with no drops: %q, %d todos", quiet.String(), cQuiet.Len())
	}
}

// TestEmitElidedSourceTodos_NilCollector: the producer is a no-op
// without a collector (stderr-only runs).
func TestEmitElidedSourceTodos_NilCollector(t *testing.T) {
	emitElidedSourceTodos(nil, []elidedSourceRecord{{Target: "t", Path: "p", Class: "c"}}, "", "")
}

// TestElidedKindsRefuseWhenEmpty: binaries/tests refuse with no srcs;
// libraries only when srcs AND the header surfaces are all empty (a
// header-only library is fine); other kinds never.
func TestElidedKindsRefuseWhenEmpty(t *testing.T) {
	cases := []struct {
		name string
		irt  ir.Target
		want bool
	}{
		{"binary-empty", ir.Target{Kind: ir.KindCCBinary}, true},
		{"binary-with-src", ir.Target{Kind: ir.KindCCBinary, Srcs: []string{"a.c"}}, false},
		{"test-empty", ir.Target{Kind: ir.KindCCTest}, true},
		{"library-empty", ir.Target{Kind: ir.KindCCLibrary}, true},
		{"library-header-only", ir.Target{Kind: ir.KindCCLibrary, Hdrs: []string{"a.h"}}, false},
		{"library-textual", ir.Target{Kind: ir.KindCCLibrary, TextualHdrs: []string{"a.inc"}}, false},
		{"genrule", ir.Target{Kind: ir.KindGenrule}, false},
	}
	for _, tc := range cases {
		if got := elidedKindsRefuseWhenEmpty(&tc.irt); got != tc.want {
			t.Errorf("%s: refuse = %v; want %v", tc.name, got, tc.want)
		}
	}
}

// TestNestedBakeReHome covers the merge-time re-anchoring: a nested
// lowering's build-dir bake rule gains the <buildRel>/ prefix on its
// out AND name, registers in the OUTER producer map, and consumer
// srcs/hdrs entries re-point; non-bake rels stay untouched.
func TestNestedBakeReHome(t *testing.T) {
	nestedPkg := &ir.Package{Targets: []ir.Target{
		{
			Name: "consumer", Kind: ir.KindCCLibrary,
			Srcs: []string{"sub/sub.c", "gen.c"},
			Hdrs: []string{"sub_config.h"},
		},
		{
			Name: bakedBuildDirName("sub_config.h"), Kind: ir.KindWriteFile,
			WriteFileOut: "sub_config.h", Tags: buildDirBakeTags(),
		},
		{
			Name: bakedBuildDirName("gen.c"), Kind: ir.KindGenrule,
			GenruleOuts: []string{"gen.c"}, Tags: buildDirBakeTags(),
		},
	}}
	nb := NestedBuildInput{BuildRel: "subbuild"}

	rehome := nestedBakeReHomes(nestedPkg, nb)
	if rehome["sub_config.h"] != "subbuild/sub_config.h" || rehome["gen.c"] != "subbuild/gen.c" {
		t.Fatalf("rehome map = %v", rehome)
	}

	cc := newCodegenContext()
	for i := range nestedPkg.Targets {
		applyNestedBakeReHome(&nestedPkg.Targets[i], rehome, cc)
	}

	consumer := nestedPkg.Targets[0]
	if consumer.Hdrs[0] != "subbuild/sub_config.h" {
		t.Errorf("consumer hdrs = %v; want re-pointed subbuild/sub_config.h", consumer.Hdrs)
	}
	if consumer.Srcs[0] != "sub/sub.c" || consumer.Srcs[1] != "subbuild/gen.c" {
		t.Errorf("consumer srcs = %v; want source path untouched, baked rel re-pointed", consumer.Srcs)
	}
	wf := nestedPkg.Targets[1]
	if wf.WriteFileOut != "subbuild/sub_config.h" || wf.Name != bakedBuildDirName("subbuild/sub_config.h") {
		t.Errorf("write_file out/name = %q/%q; want subbuild-prefixed", wf.WriteFileOut, wf.Name)
	}
	gr := nestedPkg.Targets[2]
	if gr.GenruleOuts[0] != "subbuild/gen.c" || gr.Name != bakedBuildDirName("subbuild/gen.c") {
		t.Errorf("genrule outs/name = %v/%q; want subbuild-prefixed", gr.GenruleOuts, gr.Name)
	}
	if cc.OutToGenrule["subbuild/sub_config.h"] != wf.Name || cc.OutToGenrule["subbuild/gen.c"] != gr.Name {
		t.Errorf("outer producer map = %v; want re-homed outs registered", cc.OutToGenrule)
	}
}
