package orchestrator_test

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/sstriker/buildstream-bazel/orchestrator/internal/orchestrator"
)

// TestRun_DepFailed_OnlyHelloFails: only hello fails Tier-1; uses-hello
// must surface a dep-failed entry pointing at hello (rather than its
// own configure-failed cascade), and the rest of the graph is
// unaffected.
//
// Driven by the stub's per-element mode-override env var:
//
//	ORCHESTRATOR_STUB_MODE_<sanitized-element-name>=tier1
//
// where sanitized-name uppercases and replaces non-[A-Z0-9_] with _.
func TestRun_DepFailed_OnlyHelloFails(t *testing.T) {
	stub := os.Args[0]
	t.Setenv("ORCHESTRATOR_STUB_CONVERTER", "1")
	t.Setenv("ORCHESTRATOR_STUB_MODE", "success") // baseline
	t.Setenv("ORCHESTRATOR_STUB_MODE_COMPONENTS_HELLO", "tier1")

	proj, g := mustLoadFixture(t)
	out := t.TempDir()
	res, err := orchestrator.Run(context.Background(), orchestrator.Options{
		Project:         proj,
		Graph:           g,
		Out:             out,
		ConverterBinary: stub,
		Concurrency:     1,
		Log:             testLog{t},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(res.Converted) != 0 {
		t.Errorf("Converted = %v, want [] (uses-hello should short-circuit)", res.Converted)
	}
	if len(res.Failed) != 2 {
		t.Fatalf("Failed = %v, want 2 entries", res.Failed)
	}
	got := map[string]string{}
	for _, fr := range res.Failed {
		got[fr.Element] = fr.Code
	}
	if got["components/hello"] != "configure-failed" {
		t.Errorf("hello.Code = %q, want configure-failed", got["components/hello"])
	}
	depFailed := got["components/uses-hello"]
	if depFailed != "dep-failed" {
		t.Errorf("uses-hello.Code = %q, want dep-failed", depFailed)
	}
	// Message must name the failing dep so operators can jump straight
	// to the root cause without grep'ing the full log.
	for _, fr := range res.Failed {
		if fr.Element == "components/uses-hello" {
			if !strings.Contains(fr.Message, "components/hello") {
				t.Errorf("dep-failed message should name failing dep, got %q", fr.Message)
			}
		}
	}
}

// TestRun_DepFailed_DependentSkipsConverter: when a dep fails, the
// dependent's stub converter MUST NOT run (no shadow tree, no AC
// lookup, nothing — just the synthetic Tier-1 record). We force the
// stub into a tier2-explode mode for uses-hello so any execution
// would crash the run.
func TestRun_DepFailed_DependentSkipsConverter(t *testing.T) {
	stub := os.Args[0]
	t.Setenv("ORCHESTRATOR_STUB_CONVERTER", "1")
	t.Setenv("ORCHESTRATOR_STUB_MODE", "success")
	t.Setenv("ORCHESTRATOR_STUB_MODE_COMPONENTS_HELLO", "tier1")
	t.Setenv("ORCHESTRATOR_STUB_MODE_COMPONENTS_USES_HELLO", "tier2") // crash on exec

	proj, g := mustLoadFixture(t)
	out := t.TempDir()
	res, err := orchestrator.Run(context.Background(), orchestrator.Options{
		Project:         proj,
		Graph:           g,
		Out:             out,
		ConverterBinary: stub,
		Concurrency:     1,
		Log:             testLog{t},
	})
	if err != nil {
		t.Fatalf("Run: %v (uses-hello should never have been invoked)", err)
	}
	for _, fr := range res.Failed {
		if fr.Element == "components/uses-hello" && fr.Code != "dep-failed" {
			t.Errorf("uses-hello.Code = %q, want dep-failed (its stub would have crashed if invoked)", fr.Code)
		}
	}
}
