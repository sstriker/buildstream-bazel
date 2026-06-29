package lower

import "testing"

// TestGenSrcRelToOwningBuild covers the cross-boundary gen-src capture: a
// recipe's generated source is resolved against whichever build dir physically
// owns it — the nested build dir first (a co-located source), else an ancestor
// (outer) build dir (the source the nested UTILITY wrote UP into the outer
// tree). A source under neither returns "" (a plain source-tree input).
func TestGenSrcRelToOwningBuild(t *testing.T) {
	const nested = "/tmp/cb/outer/codegen-build"
	outer := []string{"/tmp/cb/outer"}

	cases := []struct {
		name string
		abs  string
		want string
	}{
		{"co-located in nested build", nested + "/gen/x.c", "gen/x.c"},
		{"escapes nested, owned by outer", "/tmp/cb/outer/generated/type_a.c", "generated/type_a.c"},
		{"nested wins when under both", nested + "/recipe/types.cmake", "recipe/types.cmake"},
		{"outside every build dir", "/tmp/elsewhere/src/foo.c", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := genSrcRelToOwningBuild(tc.abs, nested, outer); got != tc.want {
				t.Errorf("genSrcRelToOwningBuild(%q) = %q, want %q", tc.abs, got, tc.want)
			}
		})
	}

	// Deeper nesting: an ancestor chain (outermost first) — a source owned by
	// the OUTERMOST build still resolves.
	t.Run("ancestor chain, outermost owns it", func(t *testing.T) {
		chain := []string{"/tmp/cb/top", "/tmp/cb/top/mid-build"}
		got := genSrcRelToOwningBuild("/tmp/cb/top/generated/y.c", "/tmp/cb/top/mid-build/leaf-build", chain)
		if got != "generated/y.c" {
			t.Errorf("ancestor-owned source = %q, want %q", got, "generated/y.c")
		}
	})
}

// TestReanchorCrossBoundaryOuts covers the standalone-path cross-boundary output
// re-home: a nested standalone custom-command output that escapes into an
// ANCESTOR (outer) build tree resolves to its owning-build-relative form (so the
// genrule declares a hermetic out, not a leaked absolute path), while a
// nested-owned output and a run with no ancestor builds stay byte-identical.
func TestReanchorCrossBoundaryOuts(t *testing.T) {
	const nested = "/tmp/cb/outer/codegen-build"
	cc := &codegenContext{OuterBuildDirs: []string{"/tmp/cb/outer"}}

	// A cross-boundary absolute out re-homes to the outer-relative form; a
	// nested-owned out (relative or absolute under the nested build) is unchanged.
	got := reanchorCrossBoundaryOuts(
		[]string{"/tmp/cb/outer/generated/type_a.c", "int.tmp"}, nested, cc)
	want := []string{"generated/type_a.c", "int.tmp"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("reanchorCrossBoundaryOuts = %v, want %v", got, want)
	}

	// No ancestor builds (the outer lowering): identity, same slice contents.
	plain := []string{"gen/x.c"}
	if out := reanchorCrossBoundaryOuts(plain, nested, &codegenContext{}); out[0] != "gen/x.c" || len(out) != 1 {
		t.Errorf("no-ancestor reanchor must be identity; got %v", out)
	}

	// nil cc is tolerated (the standalone path runs with a nil cc in unit tests).
	if out := reanchorCrossBoundaryOuts(plain, nested, nil); len(out) != 1 || out[0] != "gen/x.c" {
		t.Errorf("nil-cc reanchor must be identity; got %v", out)
	}
}

// TestReanchorOuterBuildDirsToRuledir covers rewriting an outer-build absolute
// path prefix to $(RULEDIR) in a recovered genrule cmd — so a tool that names
// its OUTER-build output dir absolutely writes where Bazel expects the declared
// (outer-relative) output. Longest dir first so the most-specific ancestor wins.
func TestReanchorOuterBuildDirsToRuledir(t *testing.T) {
	cases := []struct {
		name        string
		cmd         string
		outerBuilds []string
		want        string
	}{
		{
			name:        "single outer build dir",
			cmd:         "mkdir -p /tmp/cb/outer/generated && python3 gen.py /tmp/cb/outer/generated",
			outerBuilds: []string{"/tmp/cb/outer"},
			want:        "mkdir -p $(RULEDIR)/generated && python3 gen.py $(RULEDIR)/generated",
		},
		{
			name:        "longest (most-specific) ancestor wins",
			cmd:         "tool /tmp/cb/top/mid/out/x",
			outerBuilds: []string{"/tmp/cb/top", "/tmp/cb/top/mid"},
			want:        "tool $(RULEDIR)/out/x",
		},
		{
			name:        "no outer dirs is a no-op",
			cmd:         "tool /tmp/cb/outer/generated",
			outerBuilds: nil,
			want:        "tool /tmp/cb/outer/generated",
		},
		{
			name:        "no match is a no-op",
			cmd:         "tool /elsewhere/out",
			outerBuilds: []string{"/tmp/cb/outer"},
			want:        "tool /elsewhere/out",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := reanchorOuterBuildDirsToRuledir(tc.cmd, tc.outerBuilds); got != tc.want {
				t.Errorf("reanchorOuterBuildDirsToRuledir(%q) = %q, want %q", tc.cmd, got, tc.want)
			}
		})
	}
}
