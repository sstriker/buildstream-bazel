package convmode

import (
	"strings"
	"testing"
)

func TestParseFidelity(t *testing.T) {
	cases := map[string]Fidelity{
		"":            FidelityStrict,
		"strict":      FidelityStrict,
		"best-effort": FidelityBestEffort,
	}
	for in, want := range cases {
		got, err := ParseFidelity(in)
		if err != nil {
			t.Errorf("ParseFidelity(%q) errored: %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("ParseFidelity(%q) = %v, want %v", in, got, want)
		}
	}
	if _, err := ParseFidelity("lax"); err == nil || !strings.Contains(err.Error(), "--fidelity") {
		t.Errorf("expected --fidelity validation error, got %v", err)
	}
}

func TestParseBakeIn(t *testing.T) {
	cases := map[string]BakeIn{
		"":       BakeInWarn,
		"warn":   BakeInWarn,
		"allow":  BakeInAllow,
		"reject": BakeInReject,
	}
	for in, want := range cases {
		got, err := ParseBakeIn(in)
		if err != nil {
			t.Errorf("ParseBakeIn(%q) errored: %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("ParseBakeIn(%q) = %v, want %v", in, got, want)
		}
	}
	if _, err := ParseBakeIn("bogus"); err == nil || !strings.Contains(err.Error(), "--bake-in") {
		t.Errorf("expected --bake-in validation error, got %v", err)
	}
}
