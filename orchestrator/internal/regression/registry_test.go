package regression_test

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/sstriker/buildstream-bazel/orchestrator/internal/regression"
)

// snap builds a deterministic Snapshot for testing — caller controls the
// timestamp so saved JSON is byte-stable across test invocations.
func snap(t *testing.T, r *regression.Run, ts string) regression.Snapshot {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		t.Fatal(err)
	}
	return regression.SnapshotFromRun(r, parsed)
}

func TestSnapshotFromRun_SigIsContentAddressed(t *testing.T) {
	r1 := runFromOutcomes("a",
		conv("hello", "fp1"),
		failed("bad", "configure-failed", "msg"),
	)
	r2 := runFromOutcomes("b",
		conv("hello", "fp1"),
		failed("bad", "configure-failed", "different message but same code"),
	)
	s1 := snap(t, r1, "2026-04-27T00:00:00Z")
	s2 := snap(t, r2, "2026-04-28T12:34:56Z")
	if s1.Sig != s2.Sig {
		t.Errorf("Sig diverged across identical-content runs:\n  s1=%s\n  s2=%s", s1.Sig, s2.Sig)
	}

	// And different content -> different Sig.
	r3 := runFromOutcomes("c", conv("hello", "fp2"))
	s3 := snap(t, r3, "2026-04-27T00:00:00Z")
	if s1.Sig == s3.Sig {
		t.Errorf("Sig didn't shift on content change")
	}
}

func TestHistory_AppendDedupesAdjacent(t *testing.T) {
	r := runFromOutcomes("a", conv("hello", "fp1"))
	s := snap(t, r, "2026-04-27T00:00:00Z")

	h := &regression.History{Version: 1}
	if !h.Append(s) {
		t.Errorf("first Append returned false")
	}
	if h.Append(s) {
		t.Errorf("identical-content Append should dedup")
	}
	if len(h.Snapshots) != 1 {
		t.Errorf("Snapshots = %d, want 1 after dedup", len(h.Snapshots))
	}

	// Different content appends.
	r2 := runFromOutcomes("a", conv("hello", "fp2"))
	s2 := snap(t, r2, "2026-04-27T00:01:00Z")
	if !h.Append(s2) {
		t.Errorf("differing-content Append returned false")
	}
	if len(h.Snapshots) != 2 {
		t.Errorf("Snapshots = %d, want 2", len(h.Snapshots))
	}

	// Returning to the prior state IS appended (not adjacent to its
	// duplicate).
	if !h.Append(s) {
		t.Errorf("Append after non-adjacent duplicate should land")
	}
	if len(h.Snapshots) != 3 {
		t.Errorf("Snapshots = %d, want 3 after returning-to-prior", len(h.Snapshots))
	}
}

func TestHistory_LoadSaveRoundTrip(t *testing.T) {
	r := runFromOutcomes("a", conv("hello", "fp1"))
	s := snap(t, r, "2026-04-27T00:00:00Z")
	h := &regression.History{Version: 1}
	h.Append(s)

	path := filepath.Join(t.TempDir(), "fingerprints.json")
	if err := h.Save(path); err != nil {
		t.Fatal(err)
	}
	loaded, err := regression.LoadHistory(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Snapshots) != 1 {
		t.Fatalf("loaded Snapshots = %d, want 1", len(loaded.Snapshots))
	}
	if loaded.Snapshots[0].Sig != s.Sig {
		t.Errorf("Sig diverged across save/load")
	}
}

func TestLoadHistory_MissingFileIsEmpty(t *testing.T) {
	h, err := regression.LoadHistory(filepath.Join(t.TempDir(), "never.json"))
	if err != nil {
		t.Fatal(err)
	}
	if h.Version != 1 {
		t.Errorf("Version = %d, want 1", h.Version)
	}
	if len(h.Snapshots) != 0 {
		t.Errorf("Snapshots = %v, want []", h.Snapshots)
	}
}

func TestDriftFor_ReturnsChronologicalHistory(t *testing.T) {
	h := &regression.History{Version: 1}
	h.Append(snap(t, runFromOutcomes("r1", conv("hello", "fp1")), "2026-04-27T00:00:00Z"))
	h.Append(snap(t, runFromOutcomes("r2", conv("hello", "fp2")), "2026-04-27T01:00:00Z"))
	h.Append(snap(t, runFromOutcomes("r3", failed("hello", "configure-failed", "")), "2026-04-27T02:00:00Z"))

	drift := h.DriftFor("hello")
	if len(drift) != 3 {
		t.Fatalf("DriftFor = %v, want 3 entries", drift)
	}
	if drift[0].Fingerprint != "fp1" {
		t.Errorf("drift[0] fp = %q", drift[0].Fingerprint)
	}
	if drift[1].Fingerprint != "fp2" {
		t.Errorf("drift[1] fp = %q", drift[1].Fingerprint)
	}
	if !drift[2].Failed || drift[2].Code != "configure-failed" {
		t.Errorf("drift[2] = %+v, want Failed/configure-failed", drift[2])
	}
}

func TestChurnyElements_NamesElementsThatMovedInWindow(t *testing.T) {
	h := &regression.History{Version: 1}
	h.Append(snap(t, runFromOutcomes("r1",
		conv("stable", "fp1"),
		conv("churns", "fp1"),
		conv("appears-later", "fp1"),
	), "2026-04-27T00:00:00Z"))
	h.Append(snap(t, runFromOutcomes("r2",
		conv("stable", "fp1"),
		conv("churns", "fp2"), // moved
		conv("appears-later", "fp1"),
	), "2026-04-27T01:00:00Z"))
	h.Append(snap(t, runFromOutcomes("r3",
		conv("stable", "fp1"),
		conv("churns", "fp2"),
		// "appears-later" disappeared
		conv("brand-new", "fp1"),
	), "2026-04-27T02:00:00Z"))

	churn := h.ChurnyElements(3)
	want := []string{"appears-later", "brand-new", "churns"}
	if !sliceEq(churn, want) {
		t.Errorf("ChurnyElements(3) = %v, want %v", churn, want)
	}

	// Window of 2 covers only r2 + r3, so "churns" appears stable in
	// that window (was fp2 in r2, still fp2 in r3).
	churn2 := h.ChurnyElements(2)
	want2 := []string{"appears-later", "brand-new"}
	if !sliceEq(churn2, want2) {
		t.Errorf("ChurnyElements(2) = %v, want %v", churn2, want2)
	}

	// Window < 2 returns nil.
	if h.ChurnyElements(1) != nil {
		t.Errorf("ChurnyElements(1) should return nil")
	}
}

func TestLoadHistory_RejectsUnknownVersion(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "h.json")
	if err := writeBytesToFile(path, []byte(`{"version":99,"snapshots":[]}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := regression.LoadHistory(path); err == nil {
		t.Errorf("expected version error")
	}
}

// writeBytesToFile is a tiny helper to keep the test self-contained.
func writeBytesToFile(path string, b []byte) error {
	f, err := openTrunc(path)
	if err != nil {
		return err
	}
	_, werr := f.Write(b)
	cerr := f.Close()
	if werr != nil {
		return werr
	}
	return cerr
}
