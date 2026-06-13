package lower

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/ir"
	"github.com/sstriker/buildstream-bazel/internal/shadow"
)

// TestComposeWriterChain pins the chain semantics: WRITE resets,
// APPEND concatenates (and creates), TOUCH creates-but-never-truncates,
// COPY/DOWNLOAD replace, and the uncomposable COPY-then-APPEND declines.
func TestComposeWriterChain(t *testing.T) {
	w := func(op, content string) shadow.FileWriterCall {
		return shadow.FileWriterCall{Op: op, Content: content, Outputs: []string{"/b/f"}}
	}
	cases := []struct {
		name  string
		calls []shadow.FileWriterCall
		mode  string
		body  string
	}{
		{"write-append", []shadow.FileWriterCall{w("write", "a"), w("append", "b")}, "content", "ab"},
		{"append-creates", []shadow.FileWriterCall{w("append", "x")}, "content", "x"},
		{"write-resets", []shadow.FileWriterCall{w("write", "a"), w("write", "b")}, "content", "b"},
		{"touch-creates", []shadow.FileWriterCall{w("touch", "")}, "content", ""},
		{"touch-never-truncates", []shadow.FileWriterCall{w("write", "a"), w("touch", "")}, "content", "a"},
		{"copy-then-append-declines", []shadow.FileWriterCall{{Op: "copy", Sources: []string{"/s/x"}, Outputs: []string{"/b/f"}}, w("append", "b")}, "", ""},
		{"download", []shadow.FileWriterCall{{Op: "download", URL: "https://x/y", Outputs: []string{"/b/f"}}}, "download", ""},
	}
	for _, tc := range cases {
		ch := composeWriterChain(tc.calls)
		if ch.mode != tc.mode || ch.content != tc.body {
			t.Errorf("%s: mode=%q content=%q; want %q/%q", tc.name, ch.mode, ch.content, tc.mode, tc.body)
		}
	}
}

// TestLiftBuildDirFileFromWriter pins the lift tiers end-to-end against
// the index: trace-content write_file (works with NO live build dir),
// the cp TRUE lift with the source declared, the fan-out COPY narrowed
// to this output's source, and the download/unindexed declines.
func TestLiftBuildDirFileFromWriter(t *testing.T) {
	mkLC := func(calls []shadow.FileWriterCall) targetLowerCtx {
		cc := newCodegenContext()
		cc.FileWriterIndex = buildFileWriterIndex(calls, "/b")
		return targetLowerCtx{cc: cc, cmakeSrc: "/s", cmakeBuild: "/b"}
	}

	t.Run("write-lifts-offline", func(t *testing.T) {
		lc := mkLC([]shadow.FileWriterCall{{Op: "write", Content: "int a;\n", Outputs: []string{"/b/gen.c"}, File: "/s/CMakeLists.txt", Line: 4}})
		name, ok := liftBuildDirFileFromWriter("gen.c", lc)
		if !ok {
			t.Fatal("write chain must lift without a live build dir")
		}
		g := lc.cc.Genrules[0]
		if g.Name != name || lc.cc.OutToGenrule["gen.c"] != name {
			t.Errorf("producer registration: %+v", lc.cc.OutToGenrule)
		}
		if !stringSliceContains(g.Tags, "cmake-codegen-file-writer-bake") {
			t.Errorf("tags: %v", g.Tags)
		}
		if g.Provenance.Line != 4 || !strings.Contains(g.Provenance.Command, "WRITE") {
			t.Errorf("provenance: %+v", g.Provenance)
		}
	})

	t.Run("copy-true-lift-with-fanout-narrowing", func(t *testing.T) {
		lc := mkLC([]shadow.FileWriterCall{{
			Op:      "copy",
			Sources: []string{"/s/a.c", "/s/b.c"},
			Outputs: []string{"/b/copied/a.c", "/b/copied/b.c"},
			File:    "/s/CMakeLists.txt", Line: 6,
		}})
		_, ok := liftBuildDirFileFromWriter("copied/b.c", lc)
		if !ok {
			t.Fatal("copy must lift")
		}
		g := lc.cc.Genrules[0]
		if len(g.Srcs) != 1 || g.Srcs[0] != "b.c" {
			t.Errorf("fan-out must narrow to THIS output's source: %v", g.Srcs)
		}
		if !strings.Contains(g.GenruleCmd, `cp "$(location b.c)"`) {
			t.Errorf("cmd: %q", g.GenruleCmd)
		}
		if !stringSliceContains(g.Tags, "cmake-codegen-file-writer-copy") {
			t.Errorf("tags: %v", g.Tags)
		}
	})

	t.Run("download-declines-to-bake", func(t *testing.T) {
		lc := mkLC([]shadow.FileWriterCall{{Op: "download", URL: "https://x/y.h", Outputs: []string{"/b/dl.h"}}})
		if _, ok := liftBuildDirFileFromWriter("dl.h", lc); ok {
			t.Fatal("download must decline (bake-and-cite policy)")
		}
		dl, isDL := downloadWriterFor("dl.h", lc)
		if !isDL || dl.url != "https://x/y.h" {
			t.Errorf("download chain: %+v %v", dl, isDL)
		}
	})

	t.Run("unindexed-declines", func(t *testing.T) {
		lc := mkLC(nil)
		if _, ok := liftBuildDirFileFromWriter("nope.c", lc); ok {
			t.Fatal("no chain must decline")
		}
	})
}

// TestWriterChainTrustworthy pins the review's verify doctrine: an
// APPEND/TOUCH-created base is NOT grounded (the index can't see
// out-of-tree writers; cmake's APPEND/TOUCH never truncate), so offline
// it declines; with a live build dir the composed state must BYTE-MATCH
// the disk or the on-disk bake wins.
func TestWriterChainTrustworthy(t *testing.T) {
	t.Run("append-base-offline-declines", func(t *testing.T) {
		cc := newCodegenContext()
		cc.FileWriterIndex = buildFileWriterIndex([]shadow.FileWriterCall{
			{Op: "append", Content: "// trailer\n", Outputs: []string{"/b/gen/data.h"}, File: "/s/CMakeLists.txt", Line: 9},
		}, "/b")
		lc := targetLowerCtx{cc: cc, cmakeSrc: "/s", cmakeBuild: "/b"} // no hostBuild: offline
		if _, ok := liftBuildDirFileFromWriter("gen/data.h", lc); ok {
			t.Fatal("APPEND-created base must not lift offline (possible unindexed pre-writer)")
		}
	})

	t.Run("write-base-disk-mismatch-declines", func(t *testing.T) {
		host := t.TempDir()
		mustWriteFile(t, filepath.Join(host, "gen.c"), "int a;\n// appended by an out-of-tree module\n")
		cc := newCodegenContext()
		cc.FileWriterIndex = buildFileWriterIndex([]shadow.FileWriterCall{
			{Op: "write", Content: "int a;\n", Outputs: []string{"/b/gen.c"}, File: "/s/CMakeLists.txt", Line: 3},
		}, "/b")
		lc := targetLowerCtx{cc: cc, cmakeSrc: "/s", cmakeBuild: "/b", hostBuild: host}
		if _, ok := liftBuildDirFileFromWriter("gen.c", lc); ok {
			t.Fatal("composed content differing from disk must decline to the on-disk bake")
		}
	})

	t.Run("write-base-disk-match-lifts", func(t *testing.T) {
		host := t.TempDir()
		mustWriteFile(t, filepath.Join(host, "gen.c"), "int a;\n")
		cc := newCodegenContext()
		cc.FileWriterIndex = buildFileWriterIndex([]shadow.FileWriterCall{
			{Op: "write", Content: "int a;\n", Outputs: []string{"/b/gen.c"}, File: "/s/CMakeLists.txt", Line: 3},
		}, "/b")
		lc := targetLowerCtx{cc: cc, cmakeSrc: "/s", cmakeBuild: "/b", hostBuild: host}
		if _, ok := liftBuildDirFileFromWriter("gen.c", lc); !ok {
			t.Fatal("disk-verified WRITE chain must lift")
		}
	})

	t.Run("touch-base-with-matching-empty-disk-lifts", func(t *testing.T) {
		host := t.TempDir()
		mustWriteFile(t, filepath.Join(host, "x.stamp"), "")
		cc := newCodegenContext()
		cc.FileWriterIndex = buildFileWriterIndex([]shadow.FileWriterCall{
			{Op: "touch", Outputs: []string{"/b/x.stamp"}, File: "/s/CMakeLists.txt", Line: 5},
		}, "/b")
		lc := targetLowerCtx{cc: cc, cmakeSrc: "/s", cmakeBuild: "/b", hostBuild: host}
		if _, ok := liftBuildDirFileFromWriter("x.stamp", lc); !ok {
			t.Fatal("TOUCH base verified empty on disk must lift")
		}
	})
}

// TestStampLiftWriterContent pins the file(WRITE) stamp wiring: a
// stamp-bearing file(WRITE) routes through the configure_file
// stamp_values machinery (live workspace-status) under --lift; without
// a stamp var, without the lift tier, or without a non-expanded
// template, it declines (frozen bake stands).
func TestStampLiftWriterContent(t *testing.T) {
	mkLC := func(lift bool, stampVars map[string]string, templates map[string]string) targetLowerCtx {
		cc := newCodegenContext()
		cc.LiftConfigureFile = lift
		cc.StampVars = stampVars
		cc.FileWriterTemplates = templates
		cc.CMakeVars = map[string]string{"GIT_SHA": "abc123"}
		// the EXPANDED writer index (rendered content)
		cc.FileWriterIndex = buildFileWriterIndex([]shadow.FileWriterCall{
			{Op: "write", Content: "#define SHA \"abc123\"\n", Outputs: []string{"/b/ver.h"}, File: "/s/CMakeLists.txt", Line: 5},
		}, "/b")
		return targetLowerCtx{cc: cc, cmakeSrc: "/s", cmakeBuild: "/b"}
	}
	tmpl := map[string]string{"ver.h": "#define SHA \"${GIT_SHA}\"\n"}
	stamps := map[string]string{"GIT_SHA": "STABLE_GIT_SHA"}

	t.Run("wires-stamp-under-lift", func(t *testing.T) {
		lc := mkLC(true, stamps, tmpl)
		name, ok := stampLiftWriterContent("ver.h", writerChain{mode: "content", content: "#define SHA \"abc123\"\n"}, lc)
		if !ok {
			t.Fatal("expected stamp lift")
		}
		g := lc.cc.Genrules[0]
		if g.Name != name || g.Kind != ir.KindCMakeConfigureFile || g.CMakeConfigureFile == nil {
			t.Fatalf("expected cmake_configure_file, got %+v", g)
		}
		if g.CMakeConfigureFile.StampValues["GIT_SHA"] != "STABLE_GIT_SHA" {
			t.Errorf("stamp_values = %v; want GIT_SHA->STABLE_GIT_SHA", g.CMakeConfigureFile.StampValues)
		}
		if g.CMakeConfigureFile.Values["GIT_SHA"] != "abc123" {
			t.Errorf("frozen fallback Values = %v; want GIT_SHA->abc123", g.CMakeConfigureFile.Values)
		}
		if !stringSliceContains(g.Tags, "cmake-codegen-file-writer-stamp") {
			t.Errorf("tags = %v; want the file-writer-stamp facet", g.Tags)
		}
	})

	t.Run("declines-without-lift", func(t *testing.T) {
		lc := mkLC(false, stamps, tmpl)
		if _, ok := stampLiftWriterContent("ver.h", writerChain{mode: "content", content: "x"}, lc); ok {
			t.Error("must decline when the lift tier is off (tool not staged)")
		}
	})

	t.Run("declines-without-stamp-var", func(t *testing.T) {
		lc := mkLC(true, map[string]string{}, map[string]string{"ver.h": "#define V 1\n"})
		if _, ok := stampLiftWriterContent("ver.h", writerChain{mode: "content", content: "#define V 1\n"}, lc); ok {
			t.Error("must decline when the template references no stamp var")
		}
	})

	t.Run("declines-without-template", func(t *testing.T) {
		lc := mkLC(true, stamps, nil)
		if _, ok := stampLiftWriterContent("ver.h", writerChain{mode: "content", content: "x"}, lc); ok {
			t.Error("must decline without a non-expanded template (no warm pass)")
		}
	})
}
