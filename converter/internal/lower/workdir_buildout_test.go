package lower

import (
	"strings"
	"testing"
)

// TestBuildWorkdirBuildOutGenrule pins the dual-scratch shape on the
// proj_db-like command: srcs materialize element-root-relative under
// $$tmp, the cd lands in the source workdir, build-relative path
// tokens (an out with lib/../ noise, a file(WRITE) intermediate, a
// copy-to-dir dest) re-point under $$bld path.Clean'd, exec-root
// references ($(location …) tool, labelRoot-prefixed script path)
// absolutize via $$root, and the declared out copies to $(RULEDIR).
func TestBuildWorkdirBuildOutGenrule(t *testing.T) {
	body := "rm -f lib/../share/proj/proj.db && cmake -DALL_SQL_IN=Third/data/all.sql.in -DEXE_SQLITE3=$(location //e/sqlite:sqlitebin) -DPROJ_DB=lib/../share/proj/proj.db -P e/Third/data/gen.cmake && cp lib/../share/proj/proj.db Third/data/for_tests"
	got := buildWorkdirBuildOutGenrule(body,
		"Third/data",
		[]string{"Third/data/gen.cmake", "Third/data/sql/a.sql"},
		[]string{"share/proj/proj.db"},
		"e")
	for _, want := range []string{
		`root="$$PWD" && tmp="$$(mktemp -d)" && bld="$$(mktemp -d)"`,
		`cp "$(execpath Third/data/gen.cmake)" "$$tmp/Third/data/gen.cmake"`,
		`( cd "$$tmp/Third/data" && `,
		`-DEXE_SQLITE3=$$root/$(location //e/sqlite:sqlitebin)`,
		`-DPROJ_DB=$$bld/share/proj/proj.db`,
		`-DALL_SQL_IN=$$bld/Third/data/all.sql.in`,
		`-P $$root/e/Third/data/gen.cmake`,
		`cp $$bld/share/proj/proj.db $$bld/Third/data/for_tests`,
		`mkdir -p "$$bld/Third/data/for_tests"`,
		`&& cp "$$bld/share/proj/proj.db" "$(RULEDIR)/share/proj/proj.db"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if strings.Contains(got, "lib/../") {
		t.Errorf("lib/../ spelling survived path.Clean:\n%s", got)
	}
	if strings.Contains(got, "\x00") || strings.Contains(got, "\x01") {
		t.Errorf("make-ref protection bytes leaked:\n%s", got)
	}
}

// TestTryWorkdirBuildOutGenrule_Gates: no cd → decline; cd outside
// the source tree → decline; in-source outputs → decline (that's
// tryInSourceWorkdirGenrule's shape).
func TestTryWorkdirBuildOutGenrule_Gates(t *testing.T) {
	cc := newCodegenContext()
	if _, ok := tryWorkdirBuildOutGenrule(nil, "tool -o x", nil, []string{"x"}, "/src", "/bld", "", "e", nil, cc); ok {
		t.Error("no-cd cmd must decline")
	}
	if _, ok := tryWorkdirBuildOutGenrule(nil, "cd /elsewhere && tool", nil, []string{"x"}, "/src", "/bld", "", "e", nil, cc); ok {
		t.Error("out-of-source workdir must decline")
	}
	if _, ok := tryWorkdirBuildOutGenrule(nil, "cd /src/d && tool", nil, []string{"/src/d/out.c"}, "/src", "/bld", "", "e", nil, cc); ok {
		t.Error("in-source outputs belong to tryInSourceWorkdirGenrule")
	}
}

// TestBuildWorkdirBuildOutGenrule_RootLevelOut pins the slash-less
// (build-root-level) output case: a declared out like `proj.db` has
// no "/" for the path-shape heuristic to key on, so the outSet match
// must re-point it under $$bld — otherwise the trailing copy reads
// from a $$bld path nothing wrote to and the genrule always fails.
// Slash-less NON-out words (the script name resolved by the cd, bare
// flags) must stay untouched.
func TestBuildWorkdirBuildOutGenrule_RootLevelOut(t *testing.T) {
	body := "cmake -DPROJ_DB=proj.db -P e/data/gen.cmake && touch proj.db done.marker"
	got := buildWorkdirBuildOutGenrule(body,
		"data",
		[]string{"data/gen.cmake"},
		[]string{"proj.db"},
		"e")
	for _, want := range []string{
		`-DPROJ_DB=$$bld/proj.db`,
		`touch $$bld/proj.db done.marker`,
		`&& cp "$$bld/proj.db" "$(RULEDIR)/proj.db"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if strings.Contains(got, "$$bld/done.marker") {
		t.Errorf("slash-less non-out word hijacked to $$bld:\n%s", got)
	}
}

// TestTryWorkdirBuildOutGenrule_MoreGates: a mixed edge with an
// ABSOLUTE out slips past the all-or-nothing inSourceOutputs check
// but can't land as a Bazel outs entry — decline. An empty
// package+umbrella srcBase (convert-at-root mode) leaves source
// tokens indistinguishable from build-relative ones — decline.
func TestTryWorkdirBuildOutGenrule_MoreGates(t *testing.T) {
	cc := newCodegenContext()
	if _, ok := tryWorkdirBuildOutGenrule(nil, "cd /src/d && tool", nil,
		[]string{"out.db", "/src/d/side.txt"}, "/src", "/bld", "", "e", nil, cc); ok {
		t.Error("mixed edge with absolute out must decline")
	}
	if _, ok := tryWorkdirBuildOutGenrule(nil, "cd /src/d && tool", nil,
		[]string{"out.db"}, "/src", "/bld", "", "", nil, cc); ok {
		t.Error("empty package+umbrella (convert-at-root) must decline")
	}
}
