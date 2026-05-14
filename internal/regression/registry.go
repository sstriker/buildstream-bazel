package regression

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// Snapshot is one orchestrator run's per-element state captured for the
// fingerprint history. Two snapshots with identical Sig represent the
// same converted project; the registry uses Sig for content-addressed
// dedupe so "rerun, no source changes" doesn't bloat the history.
type Snapshot struct {
	// Sig is sha256 over a deterministic projection of the run's
	// per-element outcomes. Same outputs across machines → same Sig.
	Sig string `json:"sig"`

	// RunPath is the absolute path of the run dir at snapshot time.
	// Captured for traceability; not part of Sig (would defeat the
	// cross-machine determinism property).
	RunPath string `json:"run_path"`

	// Timestamp is RFC3339 UTC at snapshot time. Like RunPath, captured
	// but not folded into Sig.
	Timestamp string `json:"timestamp"`

	// Elements is the deterministically-ordered per-element record
	// snapshotted from the source Run.
	Elements []ElementSnapshot `json:"elements"`
}

// ElementSnapshot is one element's status in one Snapshot.
//
// Fingerprint is the per-element sha256 from determinism.json (empty
// when the orchestrator didn't write determinism.json — e.g. a Tier-1
// failed element has no outputs to hash). Failed/Code carry Tier-1
// state for elements that didn't convert.
type ElementSnapshot struct {
	Name        string `json:"name"`
	Fingerprint string `json:"fingerprint,omitempty"`
	Failed      bool   `json:"failed,omitempty"`
	Code        string `json:"code,omitempty"`
}

// History is the persistent registry. Append-only: the operator's
// drift-over-N-runs queries assume insertion order matches chronological
// order, which Append maintains.
type History struct {
	Version   int        `json:"version"`
	Snapshots []Snapshot `json:"snapshots"`
}

// LoadHistory reads <path> if it exists, returning an empty version-1
// history if the file is absent. Errors only on parse failure or
// unreadable file.
func LoadHistory(path string) (*History, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return &History{Version: 1}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("regression: load history %s: %w", path, err)
	}
	var h History
	if err := json.Unmarshal(b, &h); err != nil {
		return nil, fmt.Errorf("regression: parse history %s: %w", path, err)
	}
	if h.Version != 1 {
		return nil, fmt.Errorf("regression: history %s: unsupported version %d", path, h.Version)
	}
	return &h, nil
}

// Save writes the history to disk atomically (tmp + rename).
func (h *History) Save(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	body, err := json.MarshalIndent(h, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(body, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// SnapshotFromRun projects a Run into a Snapshot, computing Sig from a
// stable serialization of the per-element outcomes.
//
// `now` is taken as a parameter (not time.Now()) so tests can pin
// timestamps for deterministic JSON outputs. Production callers use
// time.Now().UTC().
func SnapshotFromRun(r *Run, now time.Time) Snapshot {
	names := r.Names()
	elems := make([]ElementSnapshot, 0, len(names))
	for _, n := range names {
		oc := r.Outcomes[n]
		es := ElementSnapshot{Name: n}
		switch {
		case oc.Failure != nil:
			es.Failed = true
			es.Code = oc.Failure.Code
		case oc.Converted != nil:
			es.Fingerprint = oc.Converted.Fingerprint
		}
		elems = append(elems, es)
	}
	return Snapshot{
		Sig:       computeSig(elems),
		RunPath:   r.Path,
		Timestamp: now.UTC().Format(time.RFC3339),
		Elements:  elems,
	}
}

// Append records snap unless the latest snapshot has the same Sig
// (content-addressed dedup). Returns true if appended, false if the
// content was a duplicate of the most-recent entry.
//
// Dedup compares only against the latest entry — older duplicates are
// permitted because operators may want a temporal record of returning
// to a prior state.
func (h *History) Append(snap Snapshot) bool {
	if n := len(h.Snapshots); n > 0 && h.Snapshots[n-1].Sig == snap.Sig {
		return false
	}
	h.Snapshots = append(h.Snapshots, snap)
	return true
}

// DriftFor returns this element's history across snapshots in
// chronological order, oldest first. Useful for plotting churn or
// answering "when did this element last change?".
func (h *History) DriftFor(name string) []ElementSnapshot {
	var out []ElementSnapshot
	for _, snap := range h.Snapshots {
		for _, e := range snap.Elements {
			if e.Name == name {
				out = append(out, e)
				break
			}
		}
	}
	return out
}

// ChurnyElements returns names of elements whose Fingerprint or
// Failed/Code state differs across the last `window` snapshots.
// `window` is clamped to len(h.Snapshots); a window of <2 returns nil
// (no churn computable).
//
// Result is sorted alphabetically for stable output.
func (h *History) ChurnyElements(window int) []string {
	if window < 2 || len(h.Snapshots) < 2 {
		return nil
	}
	if window > len(h.Snapshots) {
		window = len(h.Snapshots)
	}
	span := h.Snapshots[len(h.Snapshots)-window:]
	first := perElement(span[0])
	churned := map[string]struct{}{}
	for _, snap := range span[1:] {
		this := perElement(snap)
		for n, es := range this {
			prev, ok := first[n]
			if !ok || prev != es {
				churned[n] = struct{}{}
			}
		}
		// Also catch elements present in first but absent in this.
		for n := range first {
			if _, ok := this[n]; !ok {
				churned[n] = struct{}{}
			}
		}
	}
	out := make([]string, 0, len(churned))
	for n := range churned {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// perElement projects a snapshot's elements into a name -> serialized
// state map. Equality of the serialized form means the element didn't
// change between snapshots.
func perElement(snap Snapshot) map[string]string {
	out := make(map[string]string, len(snap.Elements))
	for _, e := range snap.Elements {
		out[e.Name] = fmt.Sprintf("%t|%s|%s", e.Failed, e.Code, e.Fingerprint)
	}
	return out
}

// computeSig returns a hex sha256 over a deterministic line-form of
// the elements list. Identical element sets across machines and
// timestamps hash identically.
func computeSig(elems []ElementSnapshot) string {
	h := sha256.New()
	for _, e := range elems {
		fmt.Fprintf(h, "%s\t%t\t%s\t%s\n", e.Name, e.Failed, e.Code, e.Fingerprint)
	}
	return hex.EncodeToString(h.Sum(nil))
}
