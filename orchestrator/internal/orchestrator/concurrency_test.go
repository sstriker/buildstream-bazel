package orchestrator_test

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/sstriker/buildstream-bazel/internal/cas"
	"github.com/sstriker/buildstream-bazel/internal/cas/fakecas"
	"github.com/sstriker/buildstream-bazel/orchestrator/internal/orchestrator"
)

// TestRun_Concurrency_DeterministicAcrossLevels: Run is invoked with
// Concurrency=1 and Concurrency=4 against the fdsdk-subset; outputs
// must be byte-identical across both runs. The goroutine pool MUST
// preserve topology (deps land before dependents) and MUST sort
// observable result lists for stable diffs.
func TestRun_Concurrency_DeterministicAcrossLevels(t *testing.T) {
	stub := os.Args[0]
	t.Setenv("ORCHESTRATOR_STUB_CONVERTER", "1")
	t.Setenv("ORCHESTRATOR_STUB_MODE", "success")

	proj, g := mustLoadFixture(t)

	// Use a shared fake CAS so the second pass can hit AC entries the
	// first pass published. The test isn't about cache-share; it's
	// about determinism — but using a real Store path keeps the code
	// path uniform.
	srv := fakecas.New()
	ep := fakecas.Start(t, srv)
	defer ep.Close()
	store, err := cas.NewGRPCStore(context.Background(), cas.GRPCConfig{
		Endpoint: "grpc://" + ep.Addr,
		Insecure: true,
	})
	if err != nil {
		t.Fatalf("dial cas: %v", err)
	}
	defer store.Close()

	out1 := t.TempDir()
	res1, err := orchestrator.Run(context.Background(), orchestrator.Options{
		Project:         proj,
		Graph:           g,
		Out:             out1,
		ConverterBinary: stub,
		Store:           store,
		Concurrency:     1,
		Log:             testLog{t},
	})
	if err != nil {
		t.Fatalf("Run (concurrency=1): %v", err)
	}

	out4 := t.TempDir()
	res4, err := orchestrator.Run(context.Background(), orchestrator.Options{
		Project:         proj,
		Graph:           g,
		Out:             out4,
		ConverterBinary: stub,
		Store:           store,
		Concurrency:     4,
		Log:             testLog{t},
	})
	if err != nil {
		t.Fatalf("Run (concurrency=4): %v", err)
	}

	// Result lists must match exactly (Run sorts internally).
	if !sliceEqual(res1.Converted, res4.Converted) {
		t.Errorf("Converted differs:\n  c=1: %v\n  c=4: %v", res1.Converted, res4.Converted)
	}
	// Cache hits/misses depend on which run cold-populates the AC.
	// Concurrency=1 is cold (first run), c=4 is fully cached.
	if len(res1.CacheMisses) != 2 || len(res1.CacheHits) != 0 {
		t.Errorf("c=1 expected 2 misses 0 hits; got %d/%d", len(res1.CacheMisses), len(res1.CacheHits))
	}
	if len(res4.CacheMisses) != 0 || len(res4.CacheHits) != 2 {
		t.Errorf("c=4 expected 0 misses 2 hits; got %d/%d", len(res4.CacheMisses), len(res4.CacheHits))
	}

	// Per-element output trees must be byte-identical across both runs.
	for _, name := range res1.Converted {
		assertElementOutputsEqualByDigest(t, out1, out4, name)
	}
}

// TestRun_Concurrency_TopologicalOrderingHolds: with Concurrency=2 on
// a graph where uses-hello depends on hello, uses-hello's processing
// MUST observe hello's depRecords. Easiest way to verify: assert
// uses-hello's emitted imports manifest has the cross-element entry.
// If topology broke, uses-hello would race ahead and see an empty
// depRecords map.
func TestRun_Concurrency_TopologicalOrderingHolds(t *testing.T) {
	stub := os.Args[0]
	rec := t.TempDir()
	t.Setenv("ORCHESTRATOR_STUB_CONVERTER", "1")
	t.Setenv("ORCHESTRATOR_STUB_MODE", "success")
	t.Setenv("ORCHESTRATOR_STUB_RECORD_DIR", rec)

	proj, g := mustLoadFixture(t)
	out := t.TempDir()

	if _, err := orchestrator.Run(context.Background(), orchestrator.Options{
		Project:         proj,
		Graph:           g,
		Out:             out,
		ConverterBinary: stub,
		Concurrency:     8,
		Log:             testLog{t},
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	importsBody := mustReadFile(t, filepath.Join(rec, "uses-hello.imports.txt"))
	if len(importsBody) == 0 {
		t.Fatal("uses-hello did not receive --imports-manifest under concurrent run")
	}
	if !strings.Contains(string(importsBody), `"name": "components/hello"`) {
		t.Errorf("uses-hello's imports manifest missing hello entry — topology likely violated:\n%s", importsBody)
	}
}

// assertElementOutputsEqualByDigest is a tighter version of
// assertElementOutputsEqual that compares *sorted* per-file digests
// (cacheshare_test's helper exists for the same purpose; redeclaring
// would conflict).
func assertElementOutputsEqualByDigest(t *testing.T, dirA, dirB, name string) {
	t.Helper()
	pathA := filepath.Join(dirA, "elements", name)
	pathB := filepath.Join(dirB, "elements", name)
	hashesA := hashTree(t, pathA)
	hashesB := hashTree(t, pathB)
	keysA := make([]string, 0, len(hashesA))
	keysB := make([]string, 0, len(hashesB))
	for k := range hashesA {
		keysA = append(keysA, k)
	}
	for k := range hashesB {
		keysB = append(keysB, k)
	}
	sort.Strings(keysA)
	sort.Strings(keysB)
	if strings.Join(keysA, ",") != strings.Join(keysB, ",") {
		t.Errorf("%s: file set differs\n  c=1: %v\n  c=4: %v", name, keysA, keysB)
		return
	}
	for _, rel := range keysA {
		if hashesA[rel] != hashesB[rel] {
			t.Errorf("%s/%s: digest mismatch", name, rel)
		}
	}
}
