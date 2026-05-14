package toolchain

import (
	"reflect"
	"testing"
)

// TestFoldElementSignal covers the additive merge: dirs the element
// signal exposed that the probe baseline missed get appended after
// the existing ones; dirs already present are left untouched; a
// language present only in the signal is not synthesized into Base.
func TestFoldElementSignal(t *testing.T) {
	rt := &ResolvedToolchain{
		Base: &Model{
			Languages: map[string]Language{
				"C": {
					BuiltinIncludeDirs: []string{"/usr/include"},
					BuiltinLinkDirs:    []string{"/usr/lib"},
				},
				"CXX": {
					BuiltinIncludeDirs: []string{"/usr/include", "/usr/include/c++/13"},
				},
			},
		},
	}
	signal := &Model{
		Languages: map[string]Language{
			"C": {
				BuiltinIncludeDirs: []string{"/usr/include", "/opt/sysroot/include"},
				BuiltinLinkDirs:    []string{"/usr/lib", "/opt/sysroot/lib"},
			},
			// Present in the signal, absent from Base: must be skipped.
			"Fortran": {BuiltinIncludeDirs: []string{"/opt/fortran/include"}},
		},
	}

	inc, link := rt.FoldElementSignal(signal)
	if want := []string{"/opt/sysroot/include"}; !reflect.DeepEqual(inc, want) {
		t.Errorf("added include = %v, want %v", inc, want)
	}
	if want := []string{"/opt/sysroot/lib"}; !reflect.DeepEqual(link, want) {
		t.Errorf("added link = %v, want %v", link, want)
	}

	gotC := rt.Base.Languages["C"]
	if want := []string{"/usr/include", "/opt/sysroot/include"}; !reflect.DeepEqual(gotC.BuiltinIncludeDirs, want) {
		t.Errorf("Base C include dirs = %v, want %v", gotC.BuiltinIncludeDirs, want)
	}
	if want := []string{"/usr/lib", "/opt/sysroot/lib"}; !reflect.DeepEqual(gotC.BuiltinLinkDirs, want) {
		t.Errorf("Base C link dirs = %v, want %v", gotC.BuiltinLinkDirs, want)
	}
	if _, ok := rt.Base.Languages["Fortran"]; ok {
		t.Error("Fortran language leaked into Base; only existing languages should be folded")
	}
	if gotCXX := rt.Base.Languages["CXX"]; len(gotCXX.BuiltinIncludeDirs) != 2 {
		t.Errorf("CXX language was mutated despite the signal not touching it: %v", gotCXX.BuiltinIncludeDirs)
	}
}

// TestFoldElementSignal_NoNewDirs: a signal that contributes only
// dirs already in Base reports nothing added and leaves Base alone.
func TestFoldElementSignal_NoNewDirs(t *testing.T) {
	rt := &ResolvedToolchain{
		Base: &Model{
			Languages: map[string]Language{
				"C": {BuiltinIncludeDirs: []string{"/usr/include"}},
			},
		},
	}
	signal := &Model{
		Languages: map[string]Language{
			"C": {BuiltinIncludeDirs: []string{"/usr/include"}},
		},
	}
	inc, link := rt.FoldElementSignal(signal)
	if inc != nil || link != nil {
		t.Errorf("expected nothing added, got inc=%v link=%v", inc, link)
	}
}

// TestFoldElementSignal_NilSafe: the method tolerates nil receivers,
// nil Base, and a nil signal without panicking.
func TestFoldElementSignal_NilSafe(t *testing.T) {
	var nilRT *ResolvedToolchain
	if inc, link := nilRT.FoldElementSignal(&Model{}); inc != nil || link != nil {
		t.Errorf("nil receiver: got inc=%v link=%v", inc, link)
	}
	if inc, link := (&ResolvedToolchain{}).FoldElementSignal(&Model{}); inc != nil || link != nil {
		t.Errorf("nil Base: got inc=%v link=%v", inc, link)
	}
	if inc, link := (&ResolvedToolchain{Base: &Model{}}).FoldElementSignal(nil); inc != nil || link != nil {
		t.Errorf("nil signal: got inc=%v link=%v", inc, link)
	}
}
