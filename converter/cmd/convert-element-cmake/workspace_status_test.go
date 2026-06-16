package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestWriteWorkspaceStatusScript checks the --out-workspace-status helper: a
// sorted, executable /bin/sh script with one `echo "KEY $(command)"` line per
// recovered stamp key.
func TestWriteWorkspaceStatusScript(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "workspace_status.sh")
	keyToCommand := map[string]string{
		"STABLE_GIT_SHA":      "git rev-parse HEAD",
		"VOLATILE_BUILD_DATE": "date -u +%Y-%m-%d",
	}
	if err := writeWorkspaceStatusScript(dst, keyToCommand); err != nil {
		t.Fatalf("writeWorkspaceStatusScript: %v", err)
	}

	info, err := os.Stat(dst)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm()&0o100 == 0 {
		t.Errorf("script not executable: mode %v", info.Mode())
	}

	body, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	got := string(body)
	if !strings.HasPrefix(got, "#!/bin/sh\n") {
		t.Errorf("missing shebang:\n%s", got)
	}
	for _, want := range []string{
		`echo "STABLE_GIT_SHA $(git rev-parse HEAD)"`,
		`echo "VOLATILE_BUILD_DATE $(date -u +%Y-%m-%d)"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing line %q in:\n%s", want, got)
		}
	}
	// Keys sorted: STABLE_ before VOLATILE_ (deterministic artifact).
	if strings.Index(got, "STABLE_GIT_SHA") > strings.Index(got, "VOLATILE_BUILD_DATE") {
		t.Errorf("keys not sorted:\n%s", got)
	}
}

// TestWriteWorkspaceStatusScript_Empty: no stamped templates still writes a
// valid header-only script (byte-stable, always-present artifact).
func TestWriteWorkspaceStatusScript_Empty(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "workspace_status.sh")
	if err := writeWorkspaceStatusScript(dst, map[string]string{}); err != nil {
		t.Fatalf("writeWorkspaceStatusScript: %v", err)
	}
	body, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	got := string(body)
	if !strings.HasPrefix(got, "#!/bin/sh\n") {
		t.Errorf("empty script missing shebang:\n%s", got)
	}
	if strings.Contains(got, "echo \"") {
		t.Errorf("empty sink should emit no echo lines:\n%s", got)
	}
}
