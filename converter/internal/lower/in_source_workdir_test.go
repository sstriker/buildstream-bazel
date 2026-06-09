package lower

import (
	"strings"
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

// TestTryInSourceWorkdirGenrule_NameIsSourceRelative pins that the in-source
// genrule's NAME derives from the source-relative output, not the absolute
// source path. genruleNameFor relativizes against the BUILD dir, but an
// in-source output lives under cmakeSrc — so the old code baked the absolute
// source root into the name (`gen__tmp_libevent_test_…`), which differs across
// checkout locations (the source-narrowing lens caught it: real tree vs a
// relocated copy produced different names). The name must be stable regardless
// of where the source tree lives.
func TestTryInSourceWorkdirGenrule_NameIsSourceRelative(t *testing.T) {
	const cmakeSrc = "/tmp/libevent"
	cmd := "cd /tmp/libevent/test && python3 ../event_rpcgen.py --quiet regress.rpc"
	outs := []string{"/tmp/libevent/test/regress.gen.c", "/tmp/libevent/test/regress.gen.h"}
	b := &ninja.Build{Outputs: outs}
	cc := &codegenContext{OutToGenrule: map[string]string{}}
	tgt, ok := tryInSourceWorkdirGenrule(b, cmd, []string{"event_rpcgen.py", "test/regress.rpc"},
		outs, cmakeSrc, "/tmp/convert-element-build-999", "", "", cc)
	if !ok {
		t.Fatal("tryInSourceWorkdirGenrule returned ok=false for an in-source workdir command")
	}
	if want := "gen_test_regress_gen_c"; tgt.Name != want {
		t.Errorf("genrule name = %q, want %q", tgt.Name, want)
	}
	// The absolute source root must NOT leak into the name (no determinism leak).
	if strings.Contains(tgt.Name, "tmp") || strings.Contains(tgt.Name, "libevent") {
		t.Errorf("genrule name %q leaks the absolute source path", tgt.Name)
	}
	// OutToGenrule wires the source-relative outputs to that same name.
	if cc.OutToGenrule["test/regress.gen.c"] != tgt.Name {
		t.Errorf("OutToGenrule[test/regress.gen.c] = %q, want %q", cc.OutToGenrule["test/regress.gen.c"], tgt.Name)
	}
}

// TestStageSiblingGeneratedHeaders pins the path-independent sibling-generated-
// header staging: a genrule emitting a .c plus a sibling .h (rpcgen shape —
// libevent's regress.gen.c / regress.gen.h) must add the .h to the SRCS of any
// cc target consuming the .c (the .c #includes the .h by bare same-dir quote,
// and cmake omits the generated header from the target's source list). srcs (not
// hdrs) because cc_test/cc_binary have no hdrs attribute. Covers both flat srcs
// and multi-config select() arms; non-header siblings are ignored; non-consuming
// cc targets untouched.
func TestStageSiblingGeneratedHeaders(t *testing.T) {
	pkg := &ir.Package{
		Targets: []ir.Target{
			{
				Name:        "gen",
				Kind:        ir.KindGenrule,
				GenruleOuts: []string{"test/regress.gen.c", "test/regress.gen.h", "test/notes.txt"},
			},
			{Name: "regress", Kind: ir.KindCCTest, Srcs: []string{"test/regress.c", "test/regress.gen.c"}},
			{
				Name: "lib", Kind: ir.KindCCLibrary,
				PerPlatform: map[string]map[string][]string{
					"srcs": {"//config:debug": {"test/regress.gen.c"}},
				},
			},
			{Name: "other", Kind: ir.KindCCBinary, Srcs: []string{"main.c"}},
		},
	}
	stageSiblingGeneratedHeaders(pkg)
	has := func(tg *ir.Target, s string) bool {
		for _, x := range tg.Srcs {
			if x == s {
				return true
			}
		}
		return false
	}
	regress, lib, other := &pkg.Targets[1], &pkg.Targets[2], &pkg.Targets[3]
	if !has(regress, "test/regress.gen.h") {
		t.Errorf("flat-srcs consumer missing sibling header in srcs; got %v", regress.Srcs)
	}
	if has(regress, "test/notes.txt") {
		t.Errorf("non-header sibling must not be staged; got %v", regress.Srcs)
	}
	if !has(lib, "test/regress.gen.h") {
		t.Errorf("select-arm consumer missing sibling header in srcs; got %v", lib.Srcs)
	}
	if has(other, "test/regress.gen.h") {
		t.Errorf("unrelated target must not get the sibling header; got %v", other.Srcs)
	}
	// Each consumer also gets the header's directory on its include path so the
	// .c's bare same-dir `#include "regress.gen.h"` resolves against the genfiles
	// copy; an unrelated target gets none.
	hasInc := func(tg *ir.Target, d string) bool {
		for _, x := range tg.Includes {
			if x == d {
				return true
			}
		}
		return false
	}
	if !hasInc(regress, "test") {
		t.Errorf("flat-srcs consumer missing generated-header include dir; got %v", regress.Includes)
	}
	if !hasInc(lib, "test") {
		t.Errorf("select-arm consumer missing generated-header include dir; got %v", lib.Includes)
	}
	if len(other.Includes) != 0 {
		t.Errorf("unrelated target must not get an include dir; got %v", other.Includes)
	}
	// Idempotent: neither srcs nor includes duplicate on a second pass.
	stageSiblingGeneratedHeaders(pkg)
	n := 0
	for _, s := range pkg.Targets[1].Srcs {
		if s == "test/regress.gen.h" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("sibling header duplicated in srcs; got %v", pkg.Targets[1].Srcs)
	}
	ni := 0
	for _, inc := range pkg.Targets[1].Includes {
		if inc == "test" {
			ni++
		}
	}
	if ni != 1 {
		t.Errorf("generated-header include dir duplicated; got %v", pkg.Targets[1].Includes)
	}
}
