package pathutil

import (
	"path/filepath"
	"testing"
)

func TestInsideRoot(t *testing.T) {
	tests := []struct {
		name string
		rel  string
		want bool
	}{
		{"empty is not inside", "", false},
		{"dot is the root itself", ".", false},
		{"dotdot is the parent", "..", false},
		{"plain child", "foo", true},
		{"nested child", "foo/bar", true},
		{"escapes via dotdot prefix", ".." + string(filepath.Separator) + "foo", false},
		{"deep escape", ".." + string(filepath.Separator) + ".." + string(filepath.Separator) + "x", false},
		// A "dotdot" segment that's part of a name, not a traversal, stays inside.
		{"dotdot embedded in name", "..foo", true},
		{"interior dotdot is not a prefix escape", "foo/../bar", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := InsideRoot(tt.rel); got != tt.want {
				t.Errorf("InsideRoot(%q) = %v, want %v", tt.rel, got, tt.want)
			}
		})
	}
}
