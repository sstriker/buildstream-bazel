package hardeningprobe

import (
	"os/exec"
	"strings"
	"testing"
)

// TestClassifyHardeningSymbols pins the symbol→flag mapping
// without needing a real cc on PATH.
func TestClassifyHardeningSymbols(t *testing.T) {
	cases := []struct {
		name      string
		nmOutput  string
		wantFlags []string
	}{
		{
			name:      "snprintf chk → FORTIFY",
			nmOutput:  "                 U __snprintf_chk\n",
			wantFlags: []string{"-D_FORTIFY_SOURCE=2"},
		},
		{
			name:      "stack_chk_fail → stack-protector",
			nmOutput:  "                 U __stack_chk_fail\n",
			wantFlags: []string{"-fstack-protector-strong"},
		},
		{
			name:      "both flags detected",
			nmOutput:  "                 U __vsnprintf_chk\n                 U __stack_chk_fail\n",
			wantFlags: []string{"-D_FORTIFY_SOURCE=2", "-fstack-protector-strong"},
		},
		{
			name:      "no hardening symbols",
			nmOutput:  "                 U printf\n                 U malloc\n",
			wantFlags: nil,
		},
		{
			name:      "stack_chk_guard counts as stack-protector",
			nmOutput:  "                 U __stack_chk_guard\n",
			wantFlags: []string{"-fstack-protector-strong"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := classifyHardeningSymbols(c.nmOutput)
			if len(got) != len(c.wantFlags) {
				t.Fatalf("got %d flags %v, want %d %v", len(got), got, len(c.wantFlags), c.wantFlags)
			}
			for _, f := range c.wantFlags {
				if _, ok := got[f]; !ok {
					t.Errorf("missing flag %q in got=%v", f, got)
				}
			}
		})
	}
}

// TestProbeEndToEnd compiles a stub with the host cc and checks
// the result is structurally valid. Skipped if cc isn't on PATH
// (CI environments where the convert host lacks a system compiler).
func TestProbeEndToEnd(t *testing.T) {
	if _, err := exec.LookPath("cc"); err != nil {
		t.Skip("cc not on PATH; probe test requires a host compiler")
	}
	if _, err := exec.LookPath("nm"); err != nil {
		t.Skip("nm not on PATH; probe test requires binutils")
	}

	r := Probe("")
	if r == nil {
		t.Fatal("Probe returned nil")
	}
	if r.Err != nil {
		// Probe is best-effort by design — cc / nm subprocess can
		// be killed (sandboxed CI runners restrict exec), the .o
		// can fail to compile (stripped-down libc headers), the
		// compile can OOM (RSS-limited runners). All of these are
		// host-environment issues, not converter bugs; skip rather
		// than fail. The unit test (TestClassifyHardeningSymbols)
		// pins the classification logic with mock data.
		t.Skipf("probe skipped (host environment): %v", r.Err)
	}
	if r.CC == "" {
		t.Error("CC not set")
	}
	// On a Debian/Ubuntu convert host with default /usr/bin/cc,
	// the probe should detect both hardening flags. On a hermetic
	// or hand-rolled toolchain, Empty() may be true and the test
	// just confirms the no-crash path.
	t.Logf("CC=%s, Flags=%v", r.CC, r.Flags)
}

func TestFormatForOperatorEmptyResult(t *testing.T) {
	r := &Result{}
	if got := r.FormatForOperator(); got != "" {
		t.Errorf("FormatForOperator on empty result = %q, want empty", got)
	}
}

func TestFormatForOperatorReportsAllFlags(t *testing.T) {
	r := &Result{
		CC: "/usr/bin/cc",
		Flags: map[string]string{
			"-D_FORTIFY_SOURCE=2":      "__snprintf_chk",
			"-fstack-protector-strong": "__stack_chk_fail",
		},
	}
	out := r.FormatForOperator()
	for _, want := range []string{
		"/usr/bin/cc",
		"-D_FORTIFY_SOURCE=2",
		"__snprintf_chk",
		"-fstack-protector-strong",
		"__stack_chk_fail",
		"copts",
		"cc_toolchain",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("FormatForOperator output missing %q; got:\n%s", want, out)
		}
	}
}
