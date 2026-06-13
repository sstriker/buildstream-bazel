package lower

import (
	"strings"
	"testing"

	"github.com/sstriker/buildstream-bazel/internal/shadow"
)

// TestRecoverExecuteProcess_WorkingDirectory pins the WORKING_DIRECTORY
// lift: a build-dir cwd produces the exec-root-save prologue, a
// relative OUTPUT_FILE anchors UNDER the cwd, and Bazel-resolved
// references gain the `$$_r/` prefix. A source-tree cwd still refuses.
func TestRecoverExecuteProcess_WorkingDirectory(t *testing.T) {
	t.Run("build-dir-wd-lifts", func(t *testing.T) {
		call := shadow.ExecuteProcessCall{
			File:             "/src/CMakeLists.txt",
			Line:             3,
			Commands:         [][]string{{"sed", "-e", "s/a/b/", "/src/tpl.h.in"}},
			OutputFile:       "out/num.h", // relative → resolves under the WD
			WorkingDirectory: "/build/work",
		}
		cc := newCodegenContext()
		_, refusals := recoverExecuteProcess([]shadow.ExecuteProcessCall{call}, "/src", "/src", "", "/build", false, nil, nil, cc)
		if len(refusals) != 0 {
			t.Fatalf("expected lift: %+v", refusals)
		}
		g := cc.Genrules[0]
		if len(g.GenruleOuts) != 1 || g.GenruleOuts[0] != "work/out/num.h" {
			t.Errorf("outs: %v want [work/out/num.h] (relative OUTPUT_FILE resolves under the WD)", g.GenruleOuts)
		}
		for _, want := range []string{`_r="$$PWD"`, `cd work`, `$$_r/$(location tpl.h.in)`, `> "$$_r/$@"`} {
			if !strings.Contains(g.GenruleCmd, want) {
				t.Errorf("cmd missing %q: %q", want, g.GenruleCmd)
			}
		}
	})

	t.Run("source-tree-wd-refuses", func(t *testing.T) {
		call := shadow.ExecuteProcessCall{
			File:             "/src/CMakeLists.txt",
			Line:             5,
			Commands:         [][]string{{"tool"}},
			OutputFile:       "/build/x.h",
			WorkingDirectory: "/src/scripts",
		}
		cc := newCodegenContext()
		_, refusals := recoverExecuteProcess([]shadow.ExecuteProcessCall{call}, "/src", "/src", "", "/build", false, nil, nil, cc)
		if len(refusals) != 1 || !strings.Contains(refusals[0].Reason, "WORKING_DIRECTORY") {
			t.Fatalf("source-tree WD must refuse: %+v", refusals)
		}
	})
}

// TestRecoverExecuteProcess_Environment: the K=V list becomes an `env`
// prefix on the hoisted cmd.
func TestRecoverExecuteProcess_Environment(t *testing.T) {
	call := shadow.ExecuteProcessCall{
		File:        "/src/CMakeLists.txt",
		Line:        7,
		Commands:    [][]string{{"tool", "--gen"}},
		OutputFile:  "/build/gen.h",
		Environment: []string{"LC_ALL=C", "GEN_MODE=fast"},
	}
	cc := newCodegenContext()
	_, refusals := recoverExecuteProcess([]shadow.ExecuteProcessCall{call}, "/src", "/src", "", "/build", false, nil, nil, cc)
	if len(refusals) != 0 {
		t.Fatalf("expected lift: %+v", refusals)
	}
	cmd := cc.Genrules[0].GenruleCmd
	if !strings.Contains(cmd, "env ") || !strings.Contains(cmd, "LC_ALL=C") || !strings.Contains(cmd, "GEN_MODE=fast") {
		t.Errorf("cmd missing env prefix: %q", cmd)
	}
}

// TestRecoverExecuteProcess_Pipeline pins the multi-COMMAND lift: an
// OUTPUT_FILE-bearing pipeline becomes one parenthesized shell pipe
// (stderr redirects then cover every stage); a stamp-stage pipeline
// keeps its stamp classification; a RESULTS_VARIABLE pipeline keeps
// refusing.
func TestRecoverExecuteProcess_Pipeline(t *testing.T) {
	t.Run("output-file-pipeline-lifts", func(t *testing.T) {
		call := shadow.ExecuteProcessCall{
			File:       "/src/CMakeLists.txt",
			Line:       9,
			Commands:   [][]string{{"cat", "/src/words.txt"}, {"sort"}},
			OutputFile: "/build/sorted.h",
		}
		cc := newCodegenContext()
		_, refusals := recoverExecuteProcess([]shadow.ExecuteProcessCall{call}, "/src", "/src", "", "/build", false, nil, nil, cc)
		if len(refusals) != 0 {
			t.Fatalf("expected lift: %+v", refusals)
		}
		g := cc.Genrules[0]
		if !strings.Contains(g.GenruleCmd, `( cat $(location words.txt) | sort ) > "$@"`) {
			t.Errorf("cmd: %q", g.GenruleCmd)
		}
		if len(g.Srcs) != 1 || g.Srcs[0] != "words.txt" {
			t.Errorf("srcs: %v", g.Srcs)
		}
	})

	t.Run("stamp-stage-keeps-stamp-classification", func(t *testing.T) {
		call := shadow.ExecuteProcessCall{
			File:       "/src/CMakeLists.txt",
			Line:       11,
			Commands:   [][]string{{"git", "describe"}, {"sed", "-e", "s/^v//"}},
			OutputFile: "/build/ver.h",
		}
		if v := Classify(call); v.Bucket != BucketStamp {
			t.Fatalf("stamp-stage pipeline must classify BucketStamp, got %v (%s)", v.Bucket, v.Reason)
		}
	})

	t.Run("results-variable-refuses", func(t *testing.T) {
		call := shadow.ExecuteProcessCall{
			File:            "/src/CMakeLists.txt",
			Line:            13,
			Commands:        [][]string{{"a"}, {"b"}},
			OutputFile:      "/build/x.h",
			ResultsVariable: "_rcs",
		}
		if v := Classify(call); v.Bucket != BucketRefuse {
			t.Fatalf("live RESULTS_VARIABLE pipeline must refuse, got %v", v.Bucket)
		}
	})
}
