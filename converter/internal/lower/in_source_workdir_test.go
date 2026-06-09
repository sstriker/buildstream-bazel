package lower

import (
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/internal/ninja"
	"github.com/sstriker/buildstream-bazel/converter/ir"
)

// TestInSourceOutputs covers the detector: ALL outputs must be absolute and
// under cmakeSrc (in-source generation) for ok; a build-dir or mixed output set
// returns false so the caller keeps the normal build-dir-output path.
func TestInSourceOutputs(t *testing.T) {
	const src = "/src"
	cases := []struct {
		name string
		outs []string
		want []string
		ok   bool
	}{
		{"all in-source", []string{"/src/test/a.gen.c", "/src/test/a.gen.h"}, []string{"test/a.gen.c", "test/a.gen.h"}, true},
		{"build-dir absolute", []string{"/build/x.c"}, nil, false},
		{"relative (build-dir-relative)", []string{"gen/x.c"}, nil, false},
		{"mixed source+build", []string{"/src/a.c", "/build/b.c"}, nil, false},
		{"empty", nil, nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := inSourceOutputs(c.outs, src)
			if ok != c.ok {
				t.Fatalf("ok = %v, want %v", ok, c.ok)
			}
			if ok && !equalStringsForCF(got, c.want) {
				t.Errorf("rel = %v, want %v", got, c.want)
			}
		})
	}
}

// TestBuildInSourceWorkdirGenrule pins the scratch-dir recipe for an in-source
// WORKING_DIRECTORY custom command (libevent's event_rpcgen.py shape): inputs
// are materialized at their element-relative positions in a mktemp dir, the
// body runs verbatim from the reconstructed working dir, and each output is
// copied to its $(RULEDIR)/<out> anchored path (which split re-relativizes).
func TestBuildInSourceWorkdirGenrule(t *testing.T) {
	got := buildInSourceWorkdirGenrule(
		"python3 ../gen.py foo.def", "sub",
		[]string{"gen.py", "sub/foo.def"},
		[]string{"sub/foo.gen.c"},
	)
	want := `tmp="$$(mktemp -d)"` +
		` && cp "$(execpath gen.py)" "$$tmp/gen.py"` +
		` && mkdir -p "$$tmp/sub" && cp "$(execpath sub/foo.def)" "$$tmp/sub/foo.def"` +
		` && ( cd "$$tmp/sub" && python3 ../gen.py foo.def )` +
		` && mkdir -p "$(RULEDIR)/sub" && cp "$$tmp/sub/foo.gen.c" "$(RULEDIR)/sub/foo.gen.c"`
	if got != want {
		t.Errorf("cmd mismatch:\n got  %q\n want %q", got, want)
	}
}

// TestAttachSiblingGeneratedHeaders pins the sibling-generated-header staging:
// a custom command emitting a .c plus a sibling .h (rpcgen shape — libevent's
// regress.gen.c / regress.gen.h) must stage the .h as a hdr on a consumer of
// the .c, since cmake omits the generated header from the target's source list
// and the .c #includes it by bare same-dir quote.
func TestAttachSiblingGeneratedHeaders(t *testing.T) {
	irt := &ir.Target{Srcs: []string{"test/regress.gen.c"}}
	b := &ninja.Build{Outputs: []string{
		"/src/test/regress.gen.c",
		"/src/test/regress.gen.h",
		"/src/test/notes.txt", // non-header sibling: ignored
	}}
	attachSiblingGeneratedHeaders(irt, b, "test/regress.gen.c", "/src")
	has := func(s string) bool {
		for _, h := range irt.Hdrs {
			if h == s {
				return true
			}
		}
		return false
	}
	if !has("test/regress.gen.h") {
		t.Errorf("sibling generated header not staged; hdrs=%v", irt.Hdrs)
	}
	if has("test/regress.gen.c") {
		t.Errorf("the just-attached source must not be re-added as a hdr; hdrs=%v", irt.Hdrs)
	}
	if has("test/notes.txt") {
		t.Errorf("non-header sibling must not be staged as a hdr; hdrs=%v", irt.Hdrs)
	}
	// Idempotent: a second call doesn't duplicate.
	attachSiblingGeneratedHeaders(irt, b, "test/regress.gen.c", "/src")
	n := 0
	for _, h := range irt.Hdrs {
		if h == "test/regress.gen.h" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("sibling header duplicated; hdrs=%v", irt.Hdrs)
	}
}
