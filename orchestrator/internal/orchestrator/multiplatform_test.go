package orchestrator

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLoadPlatformsManifest_SelectLabel: a manifest entry's
// optional select_label flows through to
// convertPlatform.SelectLabel. Empty / absent → empty
// SelectLabel (caller's signal to let PickSelectKeys
// auto-detect from constraints).
func TestLoadPlatformsManifest_SelectLabel(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "platforms.json")
	body := `[
	  {
	    "name": "linux_aarch64",
	    "constraints": ["@platforms//os:linux", "@platforms//cpu:arm64"],
	    "select_label": "//platforms:linux_aarch64",
	    "reapi_properties": [{"name": "OSFamily", "value": "linux"}]
	  },
	  {
	    "name": "darwin_arm64",
	    "constraints": ["@platforms//os:darwin", "@platforms//cpu:arm64"],
	    "reapi_properties": [{"name": "OSFamily", "value": "darwin"}]
	  }
	]`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := loadPlatformsManifest(path)
	if err != nil {
		t.Fatalf("loadPlatformsManifest: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 platforms; got %d", len(got))
	}
	if got[0].SelectLabel != "//platforms:linux_aarch64" {
		t.Errorf("linux_aarch64 SelectLabel = %q; want //platforms:linux_aarch64", got[0].SelectLabel)
	}
	if got[1].SelectLabel != "" {
		t.Errorf("darwin_arm64 SelectLabel = %q; want \"\" (absent in JSON)", got[1].SelectLabel)
	}
}

// TestLoadPlatformsManifest_SelectLabelRejectsDelimiters: a
// select_label that embeds the --cell argv delimiters (',',
// '|') is rejected so it can't break orchestrator-side
// argv encoding downstream.
func TestLoadPlatformsManifest_SelectLabelRejectsDelimiters(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "platforms.json")
	body := `[
	  {
	    "name": "p1",
	    "constraints": ["@platforms//os:linux"],
	    "select_label": "//platforms:bad|label",
	    "reapi_properties": [{"name": "x", "value": "y"}]
	  }
	]`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := loadPlatformsManifest(path)
	if err == nil {
		t.Fatal("expected error for delimiter-containing select_label")
	}
	if !strings.Contains(err.Error(), "delimiter") {
		t.Errorf("error %q should mention delimiter", err)
	}
}
