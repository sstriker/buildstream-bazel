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

// TestLoadPlatformsManifest_ExecPropertiesMapped: a platform's
// reapi_properties list ({name, value} pairs — the REAPI
// Platform.properties wire shape) is mapped onto an exec_properties
// dict on the loaded tracePlatform. A platform that declares none
// gets a nil ExecProperties map.
func TestLoadPlatformsManifest_ExecPropertiesMapped(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "platforms.json")
	if err := os.WriteFile(path, []byte(`[
  {"name": "linux_x86_64", "constraints": ["@platforms//os:linux", "@platforms//cpu:x86_64"],
   "reapi_properties": [{"name": "OSFamily", "value": "linux"}, {"name": "container-image", "value": "docker://debian:bookworm"}]},
  {"name": "darwin_arm64", "constraints": ["@platforms//os:darwin", "@platforms//cpu:arm64"]}
]`), 0o644); err != nil {
		t.Fatal(err)
	}
	platforms, err := loadPlatformsManifest(path)
	if err != nil {
		t.Fatalf("loadPlatformsManifest: %v", err)
	}
	byName := map[string]tracePlatform{}
	for _, p := range platforms {
		byName[p.Name] = p
	}
	linux := byName["linux_x86_64"]
	if got, want := linux.ExecProperties["OSFamily"], "linux"; got != want {
		t.Errorf("linux_x86_64 ExecProperties[OSFamily] = %q, want %q", got, want)
	}
	if got, want := linux.ExecProperties["container-image"], "docker://debian:bookworm"; got != want {
		t.Errorf("linux_x86_64 ExecProperties[container-image] = %q, want %q", got, want)
	}
	if darwin := byName["darwin_arm64"]; darwin.ExecProperties != nil {
		t.Errorf("darwin_arm64 declared no reapi_properties; ExecProperties should be nil, got %v", darwin.ExecProperties)
	}
}

// TestLoadPlatformsManifest_RejectsDuplicateReapiPropertyName: REAPI
// tolerates a repeated property name but Bazel exec_properties is a
// map, so loadPlatformsManifest must reject the duplicate at load
// time with a diagnostic naming the platform and the key.
func TestLoadPlatformsManifest_RejectsDuplicateReapiPropertyName(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "platforms.json")
	if err := os.WriteFile(path, []byte(`[
  {"name": "linux_x86_64", "constraints": ["@platforms//os:linux", "@platforms//cpu:x86_64"],
   "reapi_properties": [{"name": "pool", "value": "a"}, {"name": "pool", "value": "b"}]}
]`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := loadPlatformsManifest(path)
	if err == nil {
		t.Fatal("expected error for duplicate reapi_properties name; got nil")
	}
	for _, want := range []string{"linux_x86_64", "pool"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q should mention %q", err, want)
		}
	}
}

// TestRenderPlatformsBuild covers the //platforms/BUILD.bazel emit:
// one platform() per declared platform with sorted constraint_values
// + exec_properties; a platform without reapi_properties emits no
// exec_properties attr; an empty matrix emits nothing.
func TestRenderPlatformsBuild(t *testing.T) {
	if got := renderPlatformsBuild(nil); got != "" {
		t.Errorf("empty matrix should render nothing, got %q", got)
	}
	platforms := []tracePlatform{
		{
			Name:        "linux_x86_64",
			Constraints: []string{"@platforms//cpu:x86_64", "@platforms//os:linux"},
			ExecProperties: map[string]string{
				"container-image": "docker://debian:bookworm",
				"OSFamily":        "linux",
			},
		},
		{
			Name:        "darwin_arm64",
			Constraints: []string{"@platforms//os:darwin", "@platforms//cpu:arm64"},
		},
	}
	got := renderPlatformsBuild(platforms)
	for _, want := range []string{
		`platform(`,
		`name = "linux_x86_64",`,
		`name = "darwin_arm64",`,
		`constraint_values = [`,
		`"@platforms//os:linux",`,
		`exec_properties = {`,
		`"OSFamily": "linux",`,
		`"container-image": "docker://debian:bookworm",`,
		`visibility = ["//visibility:public"],`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("renderPlatformsBuild missing %q\n%s", want, got)
		}
	}
	// darwin_arm64 declared no reapi_properties — exactly one
	// exec_properties block (linux's) should appear.
	if n := strings.Count(got, "exec_properties = {"); n != 1 {
		t.Errorf("expected exactly 1 exec_properties block, got %d\n%s", n, got)
	}
	// OSFamily before container-image: dict keys are sorted.
	if strings.Index(got, `"OSFamily"`) > strings.Index(got, `"container-image"`) {
		t.Errorf("exec_properties keys not sorted\n%s", got)
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
