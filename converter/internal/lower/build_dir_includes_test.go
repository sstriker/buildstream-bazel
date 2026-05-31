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
