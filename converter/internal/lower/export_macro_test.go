package lower

import "testing"

import "github.com/sstriker/buildstream-bazel/converter/ir"

// TestApplyExportMacro covers the shared/module export-macro routing:
//   - default <target>_EXPORTS, absent from the codemodel defines, is ADDED to
//     local_defines (the libevent event_shared_EXPORTS fidelity gap);
//   - a custom DEFINE_SYMBOL present in transitive defines is MOVED (not
//     duplicated) to local_defines (zlib's ZLIB_DLL leak);
//   - non-shared targets are untouched.
func TestApplyExportMacro(t *testing.T) {
	// (1) Default macro, not in codemodel defines → added to local_defines.
	shared := &ir.Target{Kind: ir.KindCCLibrary}
	applyExportMacro(shared, "SHARED_LIBRARY", "event_shared", "")
	if !localDefineHasName(shared, "event_shared_EXPORTS") {
		t.Errorf("default export macro not added; LocalDefines=%v", shared.LocalDefines)
	}
	// Idempotent — a second pass doesn't duplicate.
	applyExportMacro(shared, "SHARED_LIBRARY", "event_shared", "")
	n := 0
	for _, d := range shared.LocalDefines {
		if d == "event_shared_EXPORTS" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("export macro duplicated; LocalDefines=%v", shared.LocalDefines)
	}

	// (2) Custom DEFINE_SYMBOL surfaced in transitive defines → moved, not left
	// in Defines (so it doesn't leak to consumers), not duplicated.
	custom := &ir.Target{Kind: ir.KindCCLibrary, Defines: []string{"ZLIB_DLL", "OTHER=1"}}
	applyExportMacro(custom, "SHARED_LIBRARY", "zlib", "ZLIB_DLL")
	if !localDefineHasName(custom, "ZLIB_DLL") {
		t.Errorf("custom define symbol not in local_defines; LocalDefines=%v", custom.LocalDefines)
	}
	for _, d := range custom.Defines {
		if d == "ZLIB_DLL" {
			t.Errorf("custom define symbol still in transitive Defines (would leak); Defines=%v", custom.Defines)
		}
	}
	dl := 0
	for _, d := range custom.LocalDefines {
		if d == "ZLIB_DLL" {
			dl++
		}
	}
	if dl != 1 {
		t.Errorf("custom define symbol duplicated in local_defines; LocalDefines=%v", custom.LocalDefines)
	}

	// (3) Non-shared target: no-op.
	stat := &ir.Target{Kind: ir.KindCCLibrary}
	applyExportMacro(stat, "STATIC_LIBRARY", "event_static", "")
	if len(stat.LocalDefines) != 0 {
		t.Errorf("static target got an export macro; LocalDefines=%v", stat.LocalDefines)
	}
}
