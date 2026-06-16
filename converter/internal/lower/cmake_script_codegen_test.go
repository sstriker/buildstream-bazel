package lower

import (
	"reflect"
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/internal/ninja"
	"github.com/sstriker/buildstream-bazel/internal/shadow"
)

// TestTracedCmakeScriptForEdge pins the generated-dispatch unwrap: when a ninja
// edge is a cmake-GENERATED dispatch wrapper, the re-trace must target the REAL
// `cmake -P <script>` from the add_custom_command trace (keyed by the edge's
// outputs), not the edge's own dispatch `-P` arg. Synthetic trace data, mirroring
// TestTraceWrapperRealArgv (the generated-dispatch shape doesn't reproduce on
// cmake 4.x + Ninja, which inlines the real command).
func TestTracedCmakeScriptForEdge(t *testing.T) {
	const buildDir = "/abs/build"
	cmds := []shadow.AddCustomCommandCall{
		// A user cmake -P wrapped by a generated dispatch — outputs recorded in
		// the trace's ABSOLUTE (--trace-expand) form.
		{Outputs: []string{"/abs/build/gen/foo.h"}, Commands: [][]string{{"cmake", "-DBIN=/abs/build", "-P", "/abs/src/user.cmake"}}},
		// A non-cmake real command (protoc) — NOT a cmake -P, so not indexed
		// (the recognizer unwrap path owns it).
		{Outputs: []string{"/abs/build/foo.pb.cc"}, Commands: [][]string{{"protoc", "--cpp_out=.", "foo.proto"}}},
		// Multi-COMMAND — skipped (genrule fallback owns it).
		{Outputs: []string{"/abs/build/multi.out"}, Commands: [][]string{{"cmake", "-P", "a.cmake"}, {"cp", "a", "b"}}},
	}
	// A user cmake -P with a positional switch arg (libpng gensrc shape), wrapped
	// by a generated dispatch.
	cmds = append(cmds, shadow.AddCustomCommandCall{
		Outputs:  []string{"/abs/build/pnglibconf.h"},
		Commands: [][]string{{"cmake", "-DPNG=1", "-P", "/abs/src/gensrc.cmake", "pnglibconf.h"}},
	})
	cc := newCodegenContext()
	cc.OutputToCustomCommand = buildOutputToCustomCommand(cmds, buildDir)

	// Build-relative edge output matches the absolute trace output → real script.
	edge := &ninja.Build{Outputs: []string{"gen/foo.h"}}
	ts, ok := cc.tracedCmakeScriptForEdge(edge, buildDir)
	if !ok || ts.script != "/abs/src/user.cmake" {
		t.Fatalf("expected the real user.cmake script; got (%+v, %v)", ts, ok)
	}
	if !reflect.DeepEqual(ts.dArgs, []string{"-DBIN=/abs/build"}) {
		t.Errorf("expected the real command's -D args; got %v", ts.dArgs)
	}
	// realCmakeCommandForEdge rebuilds the full real command for the edge.
	if got := cc.realCmakeCommandForEdge(edge, "cmake -P CMakeFiles/x.dir/d.cmake", buildDir); got != "cmake -DBIN=/abs/build -P /abs/src/user.cmake" {
		t.Errorf("realCmakeCommandForEdge = %q", got)
	}
	// Positional args are carried (libpng switch-arg shape).
	pngEdge := &ninja.Build{Outputs: []string{"pnglibconf.h"}}
	if got := cc.realCmakeCommandForEdge(pngEdge, "cmake -P CMakeFiles/x.dir/d.cmake", buildDir); got != "cmake -DPNG=1 -P /abs/src/gensrc.cmake pnglibconf.h" {
		t.Errorf("positional arg not carried: %q", got)
	}
	// A non-cmake real command isn't indexed (recognizer unwrap handles it).
	if _, ok := cc.tracedCmakeScriptForEdge(&ninja.Build{Outputs: []string{"foo.pb.cc"}}, buildDir); ok {
		t.Errorf("a non-cmake real command must not resolve to a cmake -P script")
	}
	// Multi-COMMAND records are skipped.
	if _, ok := cc.tracedCmakeScriptForEdge(&ninja.Build{Outputs: []string{"multi.out"}}, buildDir); ok {
		t.Errorf("multi-COMMAND records must not resolve")
	}
	// No matching output → no resolution; realCmakeCommandForEdge is a no-op.
	if _, ok := cc.tracedCmakeScriptForEdge(&ninja.Build{Outputs: []string{"unrelated.h"}}, buildDir); ok {
		t.Errorf("an unindexed output must not resolve")
	}
	if got := cc.realCmakeCommandForEdge(&ninja.Build{Outputs: []string{"unrelated.h"}}, "edge cmd", buildDir); got != "edge cmd" {
		t.Errorf("realCmakeCommandForEdge should be a no-op for an unindexed edge; got %q", got)
	}
}

// TestNestedCmakeScriptCall pins the argv-level `cmake -P` wrapper detector that
// drives the P3 recursion: a harvested execute_process that is itself a
// `cmake [-D…] -P <script>` invocation is recognized so expandScriptCalls
// descends into the inner script, and its -D cache args are carried through so
// the inner trace expands the same way the real nested invocation would.
func TestNestedCmakeScriptCall(t *testing.T) {
	mk := func(argv ...string) shadow.ExecuteProcessCall {
		return shadow.ExecuteProcessCall{Commands: [][]string{argv}}
	}
	tests := []struct {
		name       string
		call       shadow.ExecuteProcessCall
		wantOK     bool
		wantScript string
		wantDArgs  []string
	}{
		{
			name:       "resolved cmake path with combined -D",
			call:       mk("/usr/local/bin/cmake", "-DBIN_DIR=/b", "-DSRC_DIR=/s", "-P", "/s/inner.cmake"),
			wantOK:     true,
			wantScript: "/s/inner.cmake",
			wantDArgs:  []string{"-DBIN_DIR=/b", "-DSRC_DIR=/s"},
		},
		{
			name:       "unsubstituted CMAKE_COMMAND literal with separate -D pair",
			call:       mk("${CMAKE_COMMAND}", "-D", "PROTOC=protoc", "-P", "gen.cmake"),
			wantOK:     true,
			wantScript: "gen.cmake",
			wantDArgs:  []string{"-D", "PROTOC=protoc"},
		},
		{
			name:       "cmake.exe driver, no -D args",
			call:       mk("C:\\tools\\cmake.exe", "-P", "C:\\proj\\inner.cmake"),
			wantOK:     true,
			wantScript: "C:\\proj\\inner.cmake",
		},
		{
			name:   "real tool (protoc) is a leaf, not a wrapper",
			call:   mk("/usr/bin/protoc", "--cpp_out=/b", "-I", "/s", "/s/foo.proto"),
			wantOK: false,
		},
		{
			name:   "cmake without -P (a cmake -E call) is not a script wrapper",
			call:   mk("/usr/bin/cmake", "-E", "copy", "a", "b"),
			wantOK: false,
		},
		{
			name:   "empty argv",
			call:   shadow.ExecuteProcessCall{Commands: [][]string{{}}},
			wantOK: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			script, dArgs, ok := nestedCmakeScriptCall(tt.call)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if !ok {
				return
			}
			if script != tt.wantScript {
				t.Errorf("script = %q, want %q", script, tt.wantScript)
			}
			if !reflect.DeepEqual(dArgs, tt.wantDArgs) {
				t.Errorf("dArgs = %#v, want %#v", dArgs, tt.wantDArgs)
			}
		})
	}
}
