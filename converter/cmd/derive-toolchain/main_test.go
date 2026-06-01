package main

import "testing"

func TestResolveHardeningMode(t *testing.T) {
	// A probe stub that records whether it was consulted.
	probeCalled := false
	detectedProbe := func() (bool, string) {
		probeCalled = true
		return true, "-D_FORTIFY_SOURCE=2, -fstack-protector-strong"
	}
	cleanProbe := func() (bool, string) {
		probeCalled = true
		return false, "none"
	}

	tests := []struct {
		name        string
		mode        string
		probe       func() (bool, string)
		wantEnabled bool
		wantErr     bool
		wantProbe   bool
		wantNote    bool
	}{
		{name: "default off", mode: "off", probe: detectedProbe, wantEnabled: false, wantProbe: false, wantNote: false},
		{name: "empty is off", mode: "", probe: detectedProbe, wantEnabled: false, wantProbe: false, wantNote: false},
		{name: "false alias", mode: "false", probe: detectedProbe, wantEnabled: false, wantProbe: false, wantNote: false},
		{name: "on", mode: "on", probe: detectedProbe, wantEnabled: true, wantProbe: false, wantNote: true},
		{name: "true alias", mode: "true", probe: detectedProbe, wantEnabled: true, wantProbe: false, wantNote: true},
		{name: "auto detected", mode: "auto", probe: detectedProbe, wantEnabled: true, wantProbe: true, wantNote: true},
		{name: "auto clean", mode: "auto", probe: cleanProbe, wantEnabled: false, wantProbe: true, wantNote: true},
		{name: "invalid", mode: "yes", probe: detectedProbe, wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			probeCalled = false
			enabled, note, err := resolveHardeningMode(tc.mode, tc.probe)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("resolveHardeningMode(%q) = nil err, want error", tc.mode)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveHardeningMode(%q) unexpected err: %v", tc.mode, err)
			}
			if enabled != tc.wantEnabled {
				t.Errorf("enabled = %v, want %v", enabled, tc.wantEnabled)
			}
			if probeCalled != tc.wantProbe {
				t.Errorf("probe consulted = %v, want %v (auto must probe; on/off must not)", probeCalled, tc.wantProbe)
			}
			if (note != "") != tc.wantNote {
				t.Errorf("note=%q present=%v, want present=%v", note, note != "", tc.wantNote)
			}
		})
	}
}
