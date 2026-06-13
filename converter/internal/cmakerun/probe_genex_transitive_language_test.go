package cmakerun

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestProbeGenex_TransitiveLanguage_LiveCMake pins the link-closure
// extension of the language-conditional skip: a consumer whose OWN
// interface is clean but which links (PUBLIC, possibly multi-hop) a
// dependency whose interface carries $<COMPILE_LANGUAGE:…> must still
// skip the affected interface probe — cmake aggregates the closure into
// $<TARGET_PROPERTY:consumer,INTERFACE_COMPILE_OPTIONS>, so a
// direct-value scan would let the generate step abort with "Evaluation
// file to be written multiple times with different content".
//
// Load-bearing: reverting _cmtb_iface_lang_gate to the direct-value
// scan reproduces the abort on `consumer`/`top` here.
func TestProbeGenex_TransitiveLanguage_LiveCMake(t *testing.T) {
	if _, err := exec.LookPath("cmake"); err != nil {
		t.Skip("cmake not on PATH; skipping probe-genex transitive-language live test")
	}
	major, minor, _, err := AssertVersion(context.Background())
	if err != nil {
		t.Skipf("cmake below codemodel-v2 floor: %v", err)
	}
	if major < 3 || (major == 3 && minor < 24) {
		t.Skipf("cmake %d.%d below probe-genex floor (3.24+); skipping", major, minor)
	}
	src, err := filepath.Abs("../../testdata/sample-projects/probe-genex-transitive-language")
	if err != nil {
		t.Fatal(err)
	}
	hook, err := filepath.Abs("probe-genex.cmake")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name      string
		generator string
		config    string
		extraArgs []string
	}{
		{"single-config", "Ninja", "Release", []string{"-DCMAKE_BUILD_TYPE=Release"}},
		{"multi-config", "Ninja Multi-Config", "Release", []string{"-DCMAKE_CONFIGURATION_TYPES=Release;Debug"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			buildDir := t.TempDir()
			args := []string{"-S", src, "-B", buildDir, "-G", tc.generator}
			args = append(args, tc.extraArgs...)
			args = append(args, "-DCMAKE_PROJECT_TOP_LEVEL_INCLUDES="+hook)
			out, err := exec.CommandContext(context.Background(), "cmake", args...).CombinedOutput()
			if err != nil {
				t.Fatalf("configure+generate aborted on the transitive gate (the closure walk should skip it): %v\n%s", err, out)
			}
			for _, tgt := range []string{"consumer", "top"} {
				dir := filepath.Join(buildDir, ProbeGenexDirname, tgt)
				if _, err := os.Stat(filepath.Join(dir, "type.txt")); err != nil {
					t.Errorf("%s type.txt missing — the skip must be per-property, not per-target: %v", tgt, err)
				}
				gated := filepath.Join(dir, "interface_COMPILE_OPTIONS."+tc.config+".txt")
				if _, err := os.Stat(gated); err == nil {
					t.Errorf("%s interface_COMPILE_OPTIONS was probed despite a TRANSITIVE $<COMPILE_LANGUAGE> gate from a linked dep", tgt)
				}
				clean := filepath.Join(dir, "interface_INCLUDE_DIRECTORIES."+tc.config+".txt")
				if _, err := os.Stat(clean); err != nil {
					t.Errorf("%s clean interface_INCLUDE_DIRECTORIES must still probe: %v", tgt, err)
				}
			}
		})
	}
}
