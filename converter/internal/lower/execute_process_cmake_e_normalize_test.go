package lower

import (
	"strings"
	"testing"

	"github.com/sstriker/buildstream-bazel/internal/shadow"
)

// TestNormalizeCMakeECall pins the wrapper unwrap: env folds into
// ENVIRONMENT, chdir into WORKING_DIRECTORY (composing, relative
// resolved against the outer cwd), the POSIX-equivalents rewrite to
// raw argv, nested wrappers unwrap iteratively, and the unmodeled
// forms (--unset, env with no command) pass through untouched.
func TestNormalizeCMakeECall(t *testing.T) {
	mk := func(argv ...string) shadow.ExecuteProcessCall {
		return shadow.ExecuteProcessCall{Commands: [][]string{argv}}
	}

	t.Run("env-unwrap", func(t *testing.T) {
		got := normalizeCMakeECall(mk("/usr/bin/cmake", "-E", "env", "A=1", "B=2", "python3", "gen.py"))
		if len(got.Environment) != 2 || got.Environment[0] != "A=1" || got.Environment[1] != "B=2" {
			t.Errorf("env: %v", got.Environment)
		}
		if strings.Join(got.Commands[0], " ") != "python3 gen.py" {
			t.Errorf("argv: %v", got.Commands[0])
		}
	})

	t.Run("chdir-unwrap-composes", func(t *testing.T) {
		call := mk("cmake", "-E", "chdir", "sub", "tool", "x")
		call.WorkingDirectory = "/build/work"
		got := normalizeCMakeECall(call)
		if got.WorkingDirectory != "/build/work/sub" {
			t.Errorf("wd: %q", got.WorkingDirectory)
		}
		if strings.Join(got.Commands[0], " ") != "tool x" {
			t.Errorf("argv: %v", got.Commands[0])
		}
	})

	t.Run("nested-wrappers", func(t *testing.T) {
		got := normalizeCMakeECall(mk("cmake", "-E", "env", "A=1", "cmake", "-E", "chdir", "/build/d", "tool"))
		if len(got.Environment) != 1 || got.WorkingDirectory != "/build/d" || strings.Join(got.Commands[0], " ") != "tool" {
			t.Errorf("nested unwrap: env=%v wd=%q argv=%v", got.Environment, got.WorkingDirectory, got.Commands[0])
		}
	})

	t.Run("posix-equivalents-rewrite", func(t *testing.T) {
		got := normalizeCMakeECall(mk("cmake", "-E", "cat", "/src/a.txt", "/src/b.txt"))
		if strings.Join(got.Commands[0], " ") != "cat /src/a.txt /src/b.txt" {
			t.Errorf("argv: %v", got.Commands[0])
		}
	})

	t.Run("unmodeled-forms-pass-through", func(t *testing.T) {
		for _, argv := range [][]string{
			{"cmake", "-E", "env", "--unset=PATH", "tool"},
			{"cmake", "-E", "env", "A=1"},
			{"cmake", "-E", "tar", "xf", "a.tar"},
		} {
			got := normalizeCMakeECall(mk(argv...))
			if strings.Join(got.Commands[0], " ") != strings.Join(argv, " ") || len(got.Environment) != 0 {
				t.Errorf("%v must pass through; got %v env=%v", argv, got.Commands[0], got.Environment)
			}
		}
	})
}

// TestRecoverExecuteProcess_CMakeEExpansion: end-to-end through the
// recovery — an env-wrapped OUTPUT_FILE generator hoists with the env
// prefix; `cmake -E cat` with OUTPUT_FILE hoists as a concat genrule;
// console-only and exit-status forms skip benignly (no refusal, no
// rule); `cmake -E tar` still refuses (ROADMAP demand signal).
func TestRecoverExecuteProcess_CMakeEExpansion(t *testing.T) {
	run := func(call shadow.ExecuteProcessCall) (*codegenContext, []executeProcessRefusal) {
		cc := newCodegenContext()
		_, refusals := recoverExecuteProcess([]shadow.ExecuteProcessCall{call}, "/src", "/src", "", "/build", false, nil, nil, cc)
		return cc, refusals
	}

	t.Run("env-wrapped-generator-hoists", func(t *testing.T) {
		cc, refusals := run(shadow.ExecuteProcessCall{
			File: "/src/CMakeLists.txt", Line: 3,
			Commands:   [][]string{{"cmake", "-E", "env", "GEN_FAST=1", "python3", "/src/gen.py"}},
			OutputFile: "/build/gen.h",
		})
		if len(refusals) != 0 {
			t.Fatalf("refusals: %+v", refusals)
		}
		cmd := cc.Genrules[0].GenruleCmd
		if !strings.Contains(cmd, "env GEN_FAST=1 python3") {
			t.Errorf("cmd: %q", cmd)
		}
	})

	t.Run("cat-with-output-file-hoists", func(t *testing.T) {
		cc, refusals := run(shadow.ExecuteProcessCall{
			File: "/src/CMakeLists.txt", Line: 5,
			Commands:   [][]string{{"cmake", "-E", "cat", "/src/a.h.in", "/src/b.h.in"}},
			OutputFile: "/build/ab.h",
		})
		if len(refusals) != 0 {
			t.Fatalf("refusals: %+v", refusals)
		}
		g := cc.Genrules[0]
		if !strings.Contains(g.GenruleCmd, "cat $(location a.h.in) $(location b.h.in)") || len(g.Srcs) != 2 {
			t.Errorf("cmd/srcs: %q %v", g.GenruleCmd, g.Srcs)
		}
	})

	t.Run("benign-skips", func(t *testing.T) {
		for _, argv := range [][]string{
			{"cmake", "-E", "echo", "configuring..."},
			{"cmake", "-E", "compare_files", "/build/a", "/build/b"},
			{"cmake", "-E", "true"},
			{"cmake", "-E", "sleep", "1"},
			{"md5sum", "/src/big.bin"},
		} {
			cc, refusals := run(shadow.ExecuteProcessCall{
				File: "/src/CMakeLists.txt", Line: 7,
				Commands: [][]string{argv},
			})
			if len(refusals) != 0 || len(cc.Genrules) != 0 {
				t.Errorf("%v must skip benignly; refusals=%+v rules=%d", argv, refusals, len(cc.Genrules))
			}
		}
	})

	t.Run("tar-still-refuses", func(t *testing.T) {
		_, refusals := run(shadow.ExecuteProcessCall{
			File: "/src/CMakeLists.txt", Line: 9,
			Commands: [][]string{{"cmake", "-E", "tar", "xf", "/src/dist.tar"}},
		})
		if len(refusals) != 1 || !strings.Contains(refusals[0].Reason, "tar") {
			t.Fatalf("tar must keep its loud refusal (demand signal): %+v", refusals)
		}
	})
}

// TestReviewFixes_CMakeEGates pins the stack-review fixes: a live byte
// channel blocks the -E benign skip and the console arm; an exit-status
// capture still skips (probe doctrine); a wrapped stamp stage blocks
// the pipeline lift.
func TestReviewFixes_CMakeEGates(t *testing.T) {
	run := func(call shadow.ExecuteProcessCall) (*codegenContext, []executeProcessRefusal) {
		cc := newCodegenContext()
		_, refusals := recoverExecuteProcess([]shadow.ExecuteProcessCall{call}, "/src", "/src", "", "/build", false, nil, nil, cc)
		return cc, refusals
	}

	t.Run("noop-with-live-byte-channel-refuses", func(t *testing.T) {
		_, refusals := run(shadow.ExecuteProcessCall{
			File: "/src/CMakeLists.txt", Line: 3,
			Commands:      [][]string{{"cmake", "-E", "compare_files", "/build/a", "/build/b"}},
			ErrorVariable: "_cmp_err", // live (never cleared)
		})
		if len(refusals) != 1 || !strings.Contains(refusals[0].Reason, "live byte channel") {
			t.Fatalf("live ERROR_VARIABLE on a noop op must refuse: %+v", refusals)
		}
	})

	t.Run("noop-with-result-variable-skips", func(t *testing.T) {
		cc, refusals := run(shadow.ExecuteProcessCall{
			File: "/src/CMakeLists.txt", Line: 5,
			Commands:       [][]string{{"cmake", "-E", "compare_files", "/build/a", "/build/b"}},
			ResultVariable: "files_differ", // exit-status probe doctrine
		})
		if len(refusals) != 0 || len(cc.Genrules) != 0 {
			t.Fatalf("RESULT_VARIABLE-only compare_files must skip: %+v", refusals)
		}
	})

	t.Run("console-with-live-error-variable-refuses", func(t *testing.T) {
		_, refusals := run(shadow.ExecuteProcessCall{
			File: "/src/CMakeLists.txt", Line: 7,
			Commands:      [][]string{{"cat", "/build/list.txt"}},
			ErrorVariable: "CAT_ERR",
		})
		if len(refusals) != 1 {
			t.Fatalf("console driver with live ERROR_VARIABLE must not benign-skip: %+v", refusals)
		}
	})

	t.Run("wrapped-stamp-pipeline-blocked", func(t *testing.T) {
		call := shadow.ExecuteProcessCall{
			File: "/src/CMakeLists.txt", Line: 9,
			Commands:   [][]string{{"cmake", "-E", "env", "TZ=UTC", "git", "describe"}, {"head", "-1"}},
			OutputFile: "/build/ver.txt",
		}
		if v := Classify(call); v.Bucket == BucketFileProducing {
			t.Fatalf("env-wrapped git stamp stage must not lift as a pipeline: %v (%s)", v.Bucket, v.Reason)
		}
	})
}
