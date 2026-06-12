package cmakerun

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestProbeGenex_CompileLanguage_LiveCMake pins the fix for the
// language-conditional-property regression: probe-genex.cmake must not
// emit a file(GENERATE) whose content carries a
// $<COMPILE_LANGUAGE:…> / $<LINK_LANGUAGE:…> arm.
//
// The fixture is a MULTI-LANGUAGE (C + CXX) project whose INTERFACE
// library carries `$<$<COMPILE_LANGUAGE:CXX>:…>` usage requirements —
// the textbook header-only idiom. cmake evaluates file(GENERATE)
// content once per enabled language; the gated arm makes the
// per-language results diverge, and cmake fatal-errors with
// "Evaluation file to be written multiple times with different
// content", aborting the whole generate step (and the whole
// conversion, --probe-genex being default-on). The fix scans each
// property's RAW direct value and skips the probe for the
// language-conditional property only.
//
// Drives cmake directly with the hook injected — the failure mode is
// in cmake's generation step, so we assert on the exit code, then that
// the SKIP was surgical: the language-conditional properties' files
// are absent while the target's remaining probe surface (type.txt,
// the clean INTERFACE_INCLUDE_DIRECTORIES) is present. Skips cleanly
// without cmake >= 3.24 (the CMAKE_PROJECT_TOP_LEVEL_INCLUDES floor).
func TestProbeGenex_CompileLanguage_LiveCMake(t *testing.T) {
	if _, err := exec.LookPath("cmake"); err != nil {
		t.Skip("cmake not on PATH; skipping probe-genex compile-language live test")
	}
	major, minor, _, err := AssertVersion(context.Background())
	if err != nil {
		t.Skipf("cmake below codemodel-v2 floor: %v", err)
	}
	if major < 3 || (major == 3 && minor < 24) {
		t.Skipf("cmake %d.%d below probe-genex floor (3.24+); skipping", major, minor)
	}

	src, err := filepath.Abs("../../testdata/sample-projects/probe-genex-compile-language")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(src); err != nil {
		t.Fatalf("fixture missing: %v", err)
	}
	hook, err := filepath.Abs("probe-genex.cmake")
	if err != nil {
		t.Fatal(err)
	}

	// Cover both generator modes: the divergence is per-LANGUAGE, so it
	// exists regardless of config count — but the skip lives in the
	// per-target loop shared by the single- and multi-config emit paths.
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
			cmd := exec.CommandContext(context.Background(), "cmake", args...)
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("cmake configure+generate with probe-genex hook failed on the compile-language fixture (the fix should skip the language-conditional probes, not abort): %v\n%s", err, out)
			}

			probed := filepath.Join(buildDir, ProbeGenexDirname, "iface")
			// The target's clean surface still probes.
			if _, err := os.Stat(filepath.Join(probed, "type.txt")); err != nil {
				t.Errorf("iface type.txt missing — the skip must be per-property, not per-target: %v", err)
			}
			if _, err := os.Stat(filepath.Join(probed, "interface_INCLUDE_DIRECTORIES."+tc.config+".txt")); err != nil {
				t.Errorf("iface interface_INCLUDE_DIRECTORIES missing — a clean property must still probe: %v", err)
			}
			// The language-conditional properties are skipped (absent,
			// not empty — the fold keeps the trace-derived aggregate).
			for _, gated := range []string{"interface_COMPILE_DEFINITIONS", "interface_COMPILE_OPTIONS"} {
				if _, err := os.Stat(filepath.Join(probed, gated+"."+tc.config+".txt")); err == nil {
					t.Errorf("%s was probed despite its $<COMPILE_LANGUAGE:…> arm — multi-language projects abort on this", gated)
				}
			}
		})
	}
}
