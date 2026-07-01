package lower

import (
	"context"
	"testing"
)

// TestTraceCmakeScriptCached pins the trace memo that eliminates the superfluous
// `cmake -P` re-traces: the same (scriptPath, dArgs, workDir) tuple runs the
// underlying trace ONCE, and distinct tuples each run once. Any later call site
// (the OUTPUT_DIR write-dirs pass, the tool-calls expansion, the O(N^2)
// over-attribution guard, the per-target / standalone passes) is a cache hit.
func TestTraceCmakeScriptCached(t *testing.T) {
	// Count actual subprocess invocations via the swappable primitive, keyed on
	// the tuple so we can assert per-tuple counts.
	calls := map[string]int{}
	orig := traceCmakeScriptFn
	traceCmakeScriptFn = func(_ context.Context, _ string, script string, dArgs []string, workDir string) ([]byte, error) {
		key := script + "|" + workDir
		for _, d := range dArgs {
			key += "|" + d
		}
		calls[key]++
		return []byte("trace:" + key), nil
	}
	defer func() { traceCmakeScriptFn = orig }()

	cc := newCodegenContext()

	// Same tuple traced 5× (mimicking write-dirs + expand + guard + phases) → one
	// subprocess; every call returns the identical bytes.
	for i := 0; i < 5; i++ {
		raw, err := cc.traceCmakeScriptCached("gen.cmake", []string{"-DOUTPUT_DIR=/b/gen", "-DTOOL=x.py"}, "")
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if string(raw) != "trace:gen.cmake||-DOUTPUT_DIR=/b/gen|-DTOOL=x.py" {
			t.Fatalf("call %d returned %q", i, raw)
		}
	}
	if n := calls["gen.cmake||-DOUTPUT_DIR=/b/gen|-DTOOL=x.py"]; n != 1 {
		t.Fatalf("same tuple must trace once, traced %d times", n)
	}

	// A DIFFERENT dArgs value is a distinct tuple → its own single trace.
	cc.traceCmakeScriptCached("gen.cmake", []string{"-DOUTPUT_DIR=/b/other"}, "")
	cc.traceCmakeScriptCached("gen.cmake", []string{"-DOUTPUT_DIR=/b/other"}, "")
	if n := calls["gen.cmake||-DOUTPUT_DIR=/b/other"]; n != 1 {
		t.Fatalf("distinct dArgs tuple must trace once, traced %d times", n)
	}

	// A DIFFERENT workDir is a distinct tuple (the workdir-buildout shape).
	cc.traceCmakeScriptCached("gen.cmake", []string{"-DOUTPUT_DIR=/b/gen", "-DTOOL=x.py"}, "/scratch")
	if n := calls["gen.cmake|/scratch|-DOUTPUT_DIR=/b/gen|-DTOOL=x.py"]; n != 1 {
		t.Fatalf("distinct workDir tuple must trace once, traced %d times", n)
	}
	// The original ""-workDir tuple was NOT re-run by the workDir variant.
	if n := calls["gen.cmake||-DOUTPUT_DIR=/b/gen|-DTOOL=x.py"]; n != 1 {
		t.Fatalf("workDir variant must not disturb the base tuple's cache; base traced %d times", n)
	}
}

// TestTraceCmakeScriptCached_FailureCached pins that a FAILED trace is memoized
// too — a script that fails to trace isn't re-run at every call site.
func TestTraceCmakeScriptCached_FailureCached(t *testing.T) {
	calls := 0
	orig := traceCmakeScriptFn
	traceCmakeScriptFn = func(context.Context, string, string, []string, string) ([]byte, error) {
		calls++
		return nil, context.DeadlineExceeded
	}
	defer func() { traceCmakeScriptFn = orig }()

	cc := newCodegenContext()
	for i := 0; i < 3; i++ {
		if _, err := cc.traceCmakeScriptCached("bad.cmake", nil, ""); err == nil {
			t.Fatal("expected the trace error to propagate")
		}
	}
	if calls != 1 {
		t.Fatalf("a failing trace must be cached, ran %d times", calls)
	}
}
