package lower

import (
	"reflect"
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/ir"
)

// TestAddBuildDirIncludes covers the helper that surfaces a build-dir
// include hosting a lifted configure_file / file(GENERATE) output into the
// target's includes — the fix for Catch2, whose cc_library listed the
// generated catch2/catch_user_config.hpp in hdrs but lacked the
// `generated-includes` include path the angle-bracket #include needs.
func TestAddBuildDirIncludes(t *testing.T) {
	// Appends new dirs (sorted), deduped against existing includes.
	tgt := &ir.Target{Includes: []string{"src"}}
	addBuildDirIncludes(tgt, map[string]bool{"generated-includes": true, "build/gen": true})
	if got, want := tgt.Includes, []string{"src", "build/gen", "generated-includes"}; !reflect.DeepEqual(got, want) {
		t.Errorf("Includes = %v, want %v", got, want)
	}

	// Already-present dir isn't duplicated; empty input is a no-op.
	tgt2 := &ir.Target{Includes: []string{"generated-includes"}}
	addBuildDirIncludes(tgt2, map[string]bool{"generated-includes": true})
	addBuildDirIncludes(tgt2, nil)
	if got, want := tgt2.Includes, []string{"generated-includes"}; !reflect.DeepEqual(got, want) {
		t.Errorf("dedup/no-op: Includes = %v, want %v", got, want)
	}

	// The package/workspace root ("" or ".") is skipped — Bazel rejects a
	// root includes entry (#253), and a root-level configured header is
	// consumed via a relative include needing no -I.
	tgt3 := &ir.Target{}
	addBuildDirIncludes(tgt3, map[string]bool{".": true, "": true})
	if len(tgt3.Includes) != 0 {
		t.Errorf("root includes should be skipped; got %v", tgt3.Includes)
	}
}

// TestNeedsPkgRootInclude covers the predicate behind the
// subdir-output-under-root case: a configure_file output in a SUBDIR under
// the ROOT ("") build-dir include is consumed via that subdir path and so
// needs the package root (".") on -I (addBuildDirIncludes skips ""); a
// root-LEVEL output uses a relative include and needs none; and a non-root
// build-dir include is handled by addBuildDirIncludes itself. Reproduces
// libxml2's `<libxml/xmlversion.h>` (configure_file output libxml/xmlversion.h
// under the root build-dir) vs a plain root-level config.h.
func TestNeedsPkgRootInclude(t *testing.T) {
	cases := []struct {
		inc, relOutput string
		want           bool
	}{
		{"", "libxml/xmlversion.h", true},  // subdir output under root build-dir
		{".", "libxml/xmlversion.h", true}, // "." treated as root too
		{"", "config.h", false},            // root-LEVEL output → relative include
		{".", "config.h", false},
		{"generated-includes", "catch2/catch_user_config.hpp", false}, // non-root inc: addBuildDirIncludes handles it
		{"", "", false},                                               // degenerate
	}
	for _, c := range cases {
		if got := needsPkgRootInclude(c.inc, c.relOutput); got != c.want {
			t.Errorf("needsPkgRootInclude(%q, %q) = %v, want %v", c.inc, c.relOutput, got, c.want)
		}
	}
}
