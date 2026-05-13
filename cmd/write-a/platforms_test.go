package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLoadPlatformsManifest_AmbiguousMatrixRejected: when no
// single constraint axis uniquely identifies each platform in
// the matrix AND no operator-supplied select_label disambiguates,
// loadPlatformsManifest must surface the error at load time
// rather than letting the matrix flow into render — where a nil
// keys map from elementfold.PickSelectKeys would produce
// degenerate select() blocks with empty arm labels.
//
// Canonical ambiguous shape: {linux_x86_64, linux_aarch64,
// darwin_arm64}. `@platforms//os:linux` and
// `@platforms//cpu:arm64` each appear twice, so no axis
// uniquely identifies linux_aarch64. The operator must supply
// select_label per platform; without it the matrix is unbuildable.
func TestLoadPlatformsManifest_AmbiguousMatrixRejected(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "platforms.json")
	if err := os.WriteFile(path, []byte(`[
  {"name": "linux_x86_64",  "constraints": ["@platforms//os:linux",  "@platforms//cpu:x86_64"], "reapi_properties": [{"name":"x","value":"y"}]},
  {"name": "linux_aarch64", "constraints": ["@platforms//os:linux",  "@platforms//cpu:arm64"],  "reapi_properties": [{"name":"x","value":"y"}]},
  {"name": "darwin_arm64",  "constraints": ["@platforms//os:darwin", "@platforms//cpu:arm64"],  "reapi_properties": [{"name":"x","value":"y"}]}
]`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := loadPlatformsManifest(path)
	if err == nil {
		t.Fatal("expected error for ambiguous matrix; got nil")
	}
	// Error should name the offending platform and point at the
	// select_label escalation so the operator knows how to fix.
	for _, want := range []string{
		"linux_aarch64",
		"select_label",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q should mention %q to guide the fix", err, want)
		}
	}
}

// TestLoadPlatformsManifest_PopulatesSelectKey: a well-formed
// matrix loads cleanly with each tracePlatform's SelectKey
// populated. Both project A's fold and project B's
// install_tree.tar filegroup consume SelectKey directly without
// re-running PickSelectKeys, so the invariant matters.
func TestLoadPlatformsManifest_PopulatesSelectKey(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "platforms.json")
	if err := os.WriteFile(path, []byte(`[
  {"name": "linux_x86_64", "constraints": ["@platforms//os:linux",  "@platforms//cpu:x86_64"], "reapi_properties": [{"name":"x","value":"y"}]},
  {"name": "darwin_arm64", "constraints": ["@platforms//os:darwin", "@platforms//cpu:arm64"],  "reapi_properties": [{"name":"x","value":"y"}]}
]`), 0o644); err != nil {
		t.Fatal(err)
	}
	platforms, err := loadPlatformsManifest(path)
	if err != nil {
		t.Fatalf("loadPlatformsManifest: %v", err)
	}
	if len(platforms) != 2 {
		t.Fatalf("want 2 platforms, got %d", len(platforms))
	}
	// PickSelectKeys' auto-detect picks the lex-smallest unique
	// constraint axis; for this matrix that's the cpu axis
	// (`@platforms//cpu:` < `@platforms//os:` alphabetically).
	for _, p := range platforms {
		if p.SelectKey == "" {
			t.Errorf("platform %q: SelectKey unpopulated after load", p.Name)
		}
	}
	gotByName := map[string]string{}
	for _, p := range platforms {
		gotByName[p.Name] = p.SelectKey
	}
	if want, got := "@platforms//cpu:x86_64", gotByName["linux_x86_64"]; got != want {
		t.Errorf("linux_x86_64 SelectKey = %q, want %q", got, want)
	}
	if want, got := "@platforms//cpu:arm64", gotByName["darwin_arm64"]; got != want {
		t.Errorf("darwin_arm64 SelectKey = %q, want %q", got, want)
	}
}

// TestLoadPlatformsManifest_OperatorSelectLabelPropagates: when
// the operator supplies select_label, PickSelectKeys honours it
// verbatim and the resolved SelectKey matches.
func TestLoadPlatformsManifest_OperatorSelectLabelPropagates(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "platforms.json")
	if err := os.WriteFile(path, []byte(`[
  {"name": "linux_x86_64",  "constraints": ["@platforms//os:linux",  "@platforms//cpu:x86_64"], "select_label": "//platforms:linux_x86_64",  "reapi_properties": [{"name":"x","value":"y"}]},
  {"name": "linux_aarch64", "constraints": ["@platforms//os:linux",  "@platforms//cpu:arm64"],  "select_label": "//platforms:linux_aarch64", "reapi_properties": [{"name":"x","value":"y"}]},
  {"name": "darwin_arm64",  "constraints": ["@platforms//os:darwin", "@platforms//cpu:arm64"],  "select_label": "//platforms:darwin_arm64",  "reapi_properties": [{"name":"x","value":"y"}]}
]`), 0o644); err != nil {
		t.Fatal(err)
	}
	platforms, err := loadPlatformsManifest(path)
	if err != nil {
		t.Fatalf("loadPlatformsManifest: %v", err)
	}
	gotByName := map[string]string{}
	for _, p := range platforms {
		gotByName[p.Name] = p.SelectKey
	}
	want := map[string]string{
		"linux_x86_64":  "//platforms:linux_x86_64",
		"linux_aarch64": "//platforms:linux_aarch64",
		"darwin_arm64":  "//platforms:darwin_arm64",
	}
	for name, label := range want {
		if got := gotByName[name]; got != label {
			t.Errorf("platform %q: SelectKey = %q, want %q", name, got, label)
		}
	}
}
