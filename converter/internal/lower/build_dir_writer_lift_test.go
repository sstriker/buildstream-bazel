package lower

import (
	"strings"
	"testing"

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
