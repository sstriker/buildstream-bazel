package empfold

import (
	"reflect"
	"testing"
)

// TestPartition_AllCellsAgree: every cell observes the same
// value for every name → everything lands in baseline, deltas
// are empty.
func TestPartition_AllCellsAgree(t *testing.T) {
	cells := []string{"a", "b", "c"}
	facts := map[string]map[string]string{
		"X": {"a": "1", "b": "1", "c": "1"},
		"Y": {"a": "hello", "b": "hello", "c": "hello"},
	}
	baseline, deltas := Partition(cells, facts)

	want := map[string]string{"X": "1", "Y": "hello"}
	if !reflect.DeepEqual(baseline, want) {
		t.Errorf("baseline = %v; want %v", baseline, want)
	}
	for _, c := range cells {
		if len(deltas[c]) != 0 {
			t.Errorf("cell %q deltas should be empty; got %v", c, deltas[c])
		}
	}
}

// TestPartition_DisagreeingValue: a fact every cell observed
// but with different values lands in per-cell deltas, NOT in
// baseline. Each cell records its own value.
func TestPartition_DisagreeingValue(t *testing.T) {
	cells := []string{"a", "b"}
	facts := map[string]map[string]string{
		"X": {"a": "1", "b": "2"},       // disagree
		"Y": {"a": "same", "b": "same"}, // agree
	}
	baseline, deltas := Partition(cells, facts)

	if got, want := baseline, (map[string]string{"Y": "same"}); !reflect.DeepEqual(got, want) {
		t.Errorf("baseline = %v; want %v", got, want)
	}
	if got, want := deltas["a"], (map[string]string{"X": "1"}); !reflect.DeepEqual(got, want) {
		t.Errorf("deltas[a] = %v; want %v", got, want)
	}
	if got, want := deltas["b"], (map[string]string{"X": "2"}); !reflect.DeepEqual(got, want) {
		t.Errorf("deltas[b] = %v; want %v", got, want)
	}
}

// TestPartition_MissingFromOneCell: a fact observed in some
// cells but not all is NEVER baseline (cardinality requires
// universal observation). Each cell that observed it gets the
// fact in its delta; cells that didn't have nothing for that
// name.
func TestPartition_MissingFromOneCell(t *testing.T) {
	cells := []string{"a", "b", "c"}
	facts := map[string]map[string]string{
		"X": {"a": "1", "b": "1"}, // c missing
	}
	baseline, deltas := Partition(cells, facts)

	if len(baseline) != 0 {
		t.Errorf("baseline should be empty (X missing from c); got %v", baseline)
	}
	if got, want := deltas["a"], (map[string]string{"X": "1"}); !reflect.DeepEqual(got, want) {
		t.Errorf("deltas[a] = %v; want %v", got, want)
	}
	if got, want := deltas["b"], (map[string]string{"X": "1"}); !reflect.DeepEqual(got, want) {
		t.Errorf("deltas[b] = %v; want %v", got, want)
	}
	if len(deltas["c"]) != 0 {
		t.Errorf("deltas[c] should be empty (c never observed X); got %v", deltas["c"])
	}
}

// TestPartition_SetMembershipShape: the per-element fold lifts
// each list item to a fact with V=bool. This test exercises that
// shape: items present in every cell go to baseline; items
// unique to one cell go to that cell's deltas.
func TestPartition_SetMembershipShape(t *testing.T) {
	// Cell linux has [common.c, linux/foo.c]; cell darwin has
	// [common.c, darwin/foo.c]. Lifting each item to
	// facts[item][cell] = true shows the canonical use shape.
	cells := []string{"linux", "darwin"}
	facts := map[string]map[string]bool{
		"common.c":     {"linux": true, "darwin": true},
		"linux/foo.c":  {"linux": true},
		"darwin/foo.c": {"darwin": true},
	}
	baseline, deltas := Partition(cells, facts)

	if got, want := baseline, (map[string]bool{"common.c": true}); !reflect.DeepEqual(got, want) {
		t.Errorf("baseline = %v; want %v", got, want)
	}
	if got, want := deltas["linux"], (map[string]bool{"linux/foo.c": true}); !reflect.DeepEqual(got, want) {
		t.Errorf("deltas[linux] = %v; want %v", got, want)
	}
	if got, want := deltas["darwin"], (map[string]bool{"darwin/foo.c": true}); !reflect.DeepEqual(got, want) {
		t.Errorf("deltas[darwin] = %v; want %v", got, want)
	}
}

// TestPartition_ReturnsNonNilMaps: even with empty inputs the
// result maps are non-nil so callers can iterate without nil
// checks.
func TestPartition_ReturnsNonNilMaps(t *testing.T) {
	cells := []string{"a", "b"}
	baseline, deltas := Partition[string](cells, nil)
	if baseline == nil {
		t.Error("baseline should be non-nil")
	}
	if deltas == nil {
		t.Error("deltas should be non-nil")
	}
	for _, c := range cells {
		if deltas[c] == nil {
			t.Errorf("deltas[%q] should be non-nil", c)
		}
	}
}

// TestPartition_IgnoresUnknownCells: a fact observed by a cell
// not in the declared cells list is silently dropped (not added
// to deltas under an unknown key). Callers shouldn't be able to
// invent delta buckets via stale fact data.
func TestPartition_IgnoresUnknownCells(t *testing.T) {
	cells := []string{"a", "b"}
	facts := map[string]map[string]string{
		"X": {"a": "1", "b": "1", "ghost": "huh"},
	}
	baseline, deltas := Partition(cells, facts)

	// X is missing from the declared cells iff "ghost" is
	// counted; we ignore "ghost" so X has cardinality 2 == len(cells).
	if got, want := baseline, (map[string]string{"X": "1"}); !reflect.DeepEqual(got, want) {
		t.Errorf("baseline = %v; want %v", got, want)
	}
	if _, ok := deltas["ghost"]; ok {
		t.Errorf("deltas should not contain ghost cell; got %v", deltas)
	}
}
