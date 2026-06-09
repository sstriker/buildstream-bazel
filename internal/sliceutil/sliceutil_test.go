package sliceutil

import (
	"slices"
	"testing"
)

func TestSortedKeys(t *testing.T) {
	tests := []struct {
		name string
		in   map[string]int
		want []string
	}{
		{"empty", map[string]int{}, []string{}},
		{"single", map[string]int{"a": 1}, []string{"a"}},
		{"sorts", map[string]int{"c": 1, "a": 2, "b": 3}, []string{"a", "b", "c"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := SortedKeys(tt.in)
			if !slices.Equal(got, tt.want) {
				t.Errorf("SortedKeys(%v) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

// nil map yields an empty (zero-length) result, never a panic.
func TestSortedKeysNil(t *testing.T) {
	var m map[string]int
	if got := SortedKeys(m); len(got) != 0 {
		t.Errorf("SortedKeys(nil) = %v, want empty", got)
	}
}

// SortedKeys is generic over the key type, not just string.
func TestSortedKeysIntKey(t *testing.T) {
	got := SortedKeys(map[int]string{3: "c", 1: "a", 2: "b"})
	if want := []int{1, 2, 3}; !slices.Equal(got, want) {
		t.Errorf("SortedKeys = %v, want %v", got, want)
	}
}

func TestSortedUnique(t *testing.T) {
	tests := []struct {
		name string
		in   []string
		want []string
	}{
		{"nil", nil, nil},
		{"empty", []string{}, nil},
		{"single", []string{"a"}, []string{"a"}},
		{"already sorted unique", []string{"a", "b", "c"}, []string{"a", "b", "c"}},
		{"unsorted with dups", []string{"c", "a", "b", "a", "c"}, []string{"a", "b", "c"}},
		{"all same", []string{"x", "x", "x"}, []string{"x"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := SortedUnique(tt.in)
			if !slices.Equal(got, tt.want) {
				t.Errorf("SortedUnique(%v) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

// SortedUnique must not mutate its input (it clones before sorting).
func TestSortedUniqueNoMutate(t *testing.T) {
	in := []string{"c", "a", "b"}
	orig := slices.Clone(in)
	_ = SortedUnique(in)
	if !slices.Equal(in, orig) {
		t.Errorf("SortedUnique mutated input: got %v, want %v", in, orig)
	}
}
