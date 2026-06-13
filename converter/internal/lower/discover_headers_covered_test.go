package lower

import (
	"os"
	"path/filepath"
	"testing"
)

// TestDiscoverHeaders_CoveredMissingDirNotRecorded pins the
// wired-include-rejection fix: a missing include dir that a recovered
// producer covers (a build-dir include / generated-output root) is NOT
// recorded in `missing` — its headers are staged by the bake +
// attribution, not this hostSrc walk, so flagging it would reject an
// include that is in fact wired AND recovered. A genuine
// forward-declared empty SOURCE include (no producer) still surfaces.
func TestDiscoverHeaders_CoveredMissingDirNotRecorded(t *testing.T) {
	root := t.TempDir()
	// real source include with a header (must still be discovered)
	if err := os.MkdirAll(filepath.Join(root, "real_inc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "real_inc", "h.h"), []byte("#define H 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// "gen_inc" is absent on disk but a recovered producer covers it;
	// "future_inc" is absent with no producer (forward-declared shape).
	covered := func(inc string) bool { return inc == "gen_inc" }

	missing := map[string]bool{}
	hdrs, err := discoverHeaders(root, []string{"real_inc", "gen_inc", "future_inc"}, nil, missing, covered)
	if err != nil {
		t.Fatalf("discoverHeaders: %v", err)
	}
	if len(hdrs) != 1 || hdrs[0] != "real_inc/h.h" {
		t.Errorf("hdrs = %v; want [real_inc/h.h]", hdrs)
	}
	genAbs := filepath.Join(root, "gen_inc")
	if missing[genAbs] {
		t.Errorf("covered missing dir %q must NOT be recorded as unsupported-source-path", genAbs)
	}
	futureAbs := filepath.Join(root, "future_inc")
	if !missing[futureAbs] {
		t.Errorf("uncovered forward-declared dir %q must still surface as the survey signal", futureAbs)
	}

	// nil covered preserves the original record-every-missing behavior.
	missing2 := map[string]bool{}
	if _, err := discoverHeaders(root, []string{"gen_inc"}, nil, missing2, nil); err != nil {
		t.Fatal(err)
	}
	if !missing2[genAbs] {
		t.Errorf("nil covered must record every missing dir (back-compat)")
	}
}
