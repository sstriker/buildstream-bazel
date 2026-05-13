package orchestrator_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/sstriker/buildstream-bazel/orchestrator/internal/orchestrator"
)

// TestRun_TimingsAggregation: a successful pass writes per-element
// timings.json files; the orchestrator aggregates them and emits a
// summary with the configure-vs-translation ratio in the log + the
// per-element breakdown in <out>/manifest/timings.json.
func TestRun_TimingsAggregation(t *testing.T) {
	stub := os.Args[0]
	t.Setenv("ORCHESTRATOR_STUB_CONVERTER", "1")
	t.Setenv("ORCHESTRATOR_STUB_MODE", "success")

	proj, g := mustLoadFixture(t)
	out := t.TempDir()

	captured := &timingsBuf{}
	res, err := orchestrator.Run(context.Background(), orchestrator.Options{
		Project:         proj,
		Graph:           g,
		Out:             out,
		ConverterBinary: stub,
		Concurrency:     1,
		Log:             captured,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Stub writes 1.5s configure + 0.5s translate per element; two
	// converted elements = 3.0s + 1.0s + ratio 3.0.
	if got := res.Timings.TotalCMakeConfigureSecs; got != 3.0 {
		t.Errorf("TotalCMakeConfigureSecs = %v, want 3.0", got)
	}
	if got := res.Timings.TotalTranslationSecs; got != 1.0 {
		t.Errorf("TotalTranslationSecs = %v, want 1.0", got)
	}
	if got := res.Timings.ConfigureToTranslationRatio; got != 3.0 {
		t.Errorf("ratio = %v, want 3.0", got)
	}
	if len(res.Timings.PerElement) != 2 {
		t.Errorf("PerElement count = %d, want 2", len(res.Timings.PerElement))
	}

	// Structured summary records must appear in the log (slog text handler
	// format: key=value pairs after the msg).
	logText := captured.String()
	for _, want := range []string{
		`msg="run summary"`,
		"converted=2",
		`msg="converter wall-clock"`,
		"total_seconds=4",
		"configure_to_translation_ratio=3",
	} {
		if !strings.Contains(logText, want) {
			t.Errorf("log missing %q\n%s", want, logText)
		}
	}

	// manifest/timings.json must contain the schema-versioned summary.
	body, err := os.ReadFile(filepath.Join(out, "manifest", "timings.json"))
	if err != nil {
		t.Fatalf("read timings.json: %v", err)
	}
	var doc struct {
		Version int `json:"version"`
		Summary struct {
			TotalCMakeConfigureSecs float64 `json:"total_cmake_configure_seconds"`
		} `json:"summary"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatalf("parse timings.json: %v", err)
	}
	if doc.Version != 1 {
		t.Errorf("manifest version = %d, want 1", doc.Version)
	}
	if doc.Summary.TotalCMakeConfigureSecs != 3.0 {
		t.Errorf("manifest cumulative configure = %v, want 3.0", doc.Summary.TotalCMakeConfigureSecs)
	}
}

// TestRun_TimingsExcludedFromDeterminism: a second run on the same
// inputs (with cache hits) must produce a determinism.json byte-
// identical to the first, even though timings would differ across
// runs in the wild.
func TestRun_TimingsExcludedFromDeterminism(t *testing.T) {
	stub := os.Args[0]
	t.Setenv("ORCHESTRATOR_STUB_CONVERTER", "1")
	t.Setenv("ORCHESTRATOR_STUB_MODE", "success")

	proj, g := mustLoadFixture(t)

	out1 := t.TempDir()
	if _, err := orchestrator.Run(context.Background(), orchestrator.Options{
		Project:         proj,
		Graph:           g,
		Out:             out1,
		ConverterBinary: stub,
		Concurrency:     1,
		Log:             testLog{t},
	}); err != nil {
		t.Fatalf("Run #1: %v", err)
	}
	out2 := t.TempDir()
	if _, err := orchestrator.Run(context.Background(), orchestrator.Options{
		Project:         proj,
		Graph:           g,
		Out:             out2,
		ConverterBinary: stub,
		Concurrency:     1,
		Log:             testLog{t},
	}); err != nil {
		t.Fatalf("Run #2: %v", err)
	}

	d1, _ := os.ReadFile(filepath.Join(out1, "manifest", "determinism.json"))
	d2, _ := os.ReadFile(filepath.Join(out2, "manifest", "determinism.json"))
	if string(d1) != string(d2) {
		t.Errorf("determinism.json drifted across runs (timings.json should be excluded)")
	}
}

type timingsBuf struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *timingsBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *timingsBuf) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
