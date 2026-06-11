package lower

import (
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/internal/todos"
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
