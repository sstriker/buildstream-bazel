package lower

import "testing"

// TestBuiltinRecognizers_Compile pins that the embedded built-in recognizers
// compile (they ship in the binary, so a syntax/contract error is a build bug)
// and expose the three expected names.
func TestBuiltinRecognizers_Compile(t *testing.T) {
	got := builtinRecognizers()
	if len(got) == 0 {
		t.Fatal("no embedded recognizers loaded")
	}
	want := map[string]bool{
		"starlark:protoc":    false,
		"starlark:grpc_cpp":  false,
		"starlark:grpc_only": false,
	}
	for _, r := range got {
		if _, ok := want[r.Name()]; ok {
			want[r.Name()] = true
		}
	}
	for name, seen := range want {
		if !seen {
			t.Errorf("embedded recognizer %q not loaded; got %v", name, recognizerNames(got))
		}
	}
}

func recognizerNames(rs []CodegenRecognizer) []string {
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = r.Name()
	}
	return out
}

// An operator recognizer with the same name shadows the embedded built-in
// (override-by-name); a new name appends.
func TestCodegenRegistry_OverrideByName(t *testing.T) {
	override := loadStarFromString(t, "protoc.star", `
def match(cmd):
    return False
def lower(cmd):
    return result(targets = [native_rule("x", "x")], derived_outputs = ["x"])
`)
	reg := codegenRegistry([]CodegenRecognizer{override})
	// Count "starlark:protoc" entries — must be exactly one (the override replaced
	// the embedded one in place, not appended alongside it).
	n := 0
	for _, r := range reg {
		if r.Name() == "starlark:protoc" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("operator protoc.star should shadow the built-in (one entry), got %d", n)
	}
	if len(reg) != len(builtinRecognizers()) {
		t.Errorf("override must not grow the registry: got %d, want %d", len(reg), len(builtinRecognizers()))
	}
}
