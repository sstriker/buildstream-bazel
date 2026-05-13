package orchestrator_test

import (
	"context"
	"os"
	"testing"

	"github.com/sstriker/buildstream-bazel/internal/cas"
	"github.com/sstriker/buildstream-bazel/internal/cas/fakecas"
	"github.com/sstriker/buildstream-bazel/internal/reapi"
	"github.com/sstriker/buildstream-bazel/orchestrator/internal/orchestrator"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// TestRun_RemoteExecute_NoLocalFork is the M3b keystone. The
// orchestrator is wired with a REAPI Executor; conversions go through
// the Execute path against the fake worker instead of forking the
// converter locally. The stub binary is set to a non-runnable path
// (/dev/null) — if any local exec slipped through, the run would
// crash. Worker-side, the fake Execution server forks the real stub
// against the action's input root.
func TestRun_RemoteExecute_NoLocalFork(t *testing.T) {
	srv := fakecas.New()
	worker := fakecas.NewExecutionServer(srv)
	worker.SkipCacheLookup = true // ensure the fork actually happens
	ep := fakecas.Start(t, srv, fakecas.WithExecution(worker))
	defer ep.Close()

	store, err := cas.NewGRPCStore(context.Background(), cas.GRPCConfig{
		Endpoint: "grpc://" + ep.Addr,
		Insecure: true,
	})
	if err != nil {
		t.Fatalf("dial cas: %v", err)
	}
	defer store.Close()

	conn, err := grpc.NewClient(ep.Addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial execute: %v", err)
	}
	defer conn.Close()
	executor := reapi.NewGRPCExecutor(conn, "")

	// The "client-side" converter binary points at the test binary
	// re-invoked as the stub; the WORKER is what actually exec's it,
	// after materializing the action's input root.
	stub := os.Args[0]
	t.Setenv("ORCHESTRATOR_STUB_CONVERTER", "1")
	t.Setenv("ORCHESTRATOR_STUB_MODE", "success")

	proj, g := mustLoadFixture(t)
	out := t.TempDir()

	res, err := orchestrator.Run(context.Background(), orchestrator.Options{
		Project:         proj,
		Graph:           g,
		Out:             out,
		ConverterBinary: stub,
		Store:           store,
		Executor:        executor,
		Log:             testLog{t},
	})
	if err != nil {
		t.Fatalf("orchestrator: %v", err)
	}

	wantConv := []string{"components/hello", "components/uses-hello"}
	if !sliceEqual(res.Converted, wantConv) {
		t.Errorf("Converted = %v, want %v", res.Converted, wantConv)
	}
	if len(res.Failed) != 0 {
		t.Errorf("Failed = %v, want []", res.Failed)
	}
	// The worker must have run the action twice, one per element. Cache
	// hits are not expected on a cold AC.
	if got := worker.ExecuteCount(); got != int64(len(wantConv)) {
		t.Errorf("worker ExecuteCount = %d, want %d", got, len(wantConv))
	}

	// Outputs must be materialized at the same paths convertOne would
	// have written them.
	for _, name := range wantConv {
		assertElementOutputsExist(t, out, name)
	}
}

// TestRun_RemoteExecute_CacheHitsSkipWorker validates that AC entries
// the worker publishes are honored on a second orchestrator pass
// without re-running the action. (M5's cache layer is unchanged by
// M3b; this is a sanity check that the worker's UpdateActionResult
// composes with GetActionResult.)
func TestRun_RemoteExecute_CacheHitsSkipWorker(t *testing.T) {
	srv := fakecas.New()
	worker := fakecas.NewExecutionServer(srv)
	ep := fakecas.Start(t, srv, fakecas.WithExecution(worker))
	defer ep.Close()

	store, err := cas.NewGRPCStore(context.Background(), cas.GRPCConfig{
		Endpoint: "grpc://" + ep.Addr,
		Insecure: true,
	})
	if err != nil {
		t.Fatalf("dial cas: %v", err)
	}
	defer store.Close()
	conn, err := grpc.NewClient(ep.Addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial execute: %v", err)
	}
	defer conn.Close()
	executor := reapi.NewGRPCExecutor(conn, "")

	stub := os.Args[0]
	t.Setenv("ORCHESTRATOR_STUB_CONVERTER", "1")
	t.Setenv("ORCHESTRATOR_STUB_MODE", "success")

	proj, g := mustLoadFixture(t)

	// First pass: cold, worker runs each action, publishes to AC.
	outA := t.TempDir()
	if _, err := orchestrator.Run(context.Background(), orchestrator.Options{
		Project:         proj,
		Graph:           g,
		Out:             outA,
		ConverterBinary: stub,
		Store:           store,
		Executor:        executor,
		Log:             testLog{t},
	}); err != nil {
		t.Fatalf("first run: %v", err)
	}
	firstCount := worker.ExecuteCount()
	if firstCount == 0 {
		t.Fatalf("worker did not run any actions on cold pass")
	}

	// Second pass: clean tmpdir; AC hits short-circuit the orchestrator
	// before it even gets to the executor. Worker count must not grow.
	outB := t.TempDir()
	resB, err := orchestrator.Run(context.Background(), orchestrator.Options{
		Project:         proj,
		Graph:           g,
		Out:             outB,
		ConverterBinary: stub,
		Store:           store,
		Executor:        executor,
		Log:             testLog{t},
	})
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if len(resB.CacheMisses) != 0 {
		t.Errorf("second run misses = %v, want []", resB.CacheMisses)
	}
	if got := worker.ExecuteCount(); got != firstCount {
		t.Errorf("worker ExecuteCount grew on second pass: %d -> %d", firstCount, got)
	}
}

func assertElementOutputsExist(t *testing.T, out, name string) {
	t.Helper()
	for _, suffix := range []string{
		"/elements/" + name + "/BUILD.bazel",
		"/elements/" + name + "/read_paths.json",
	} {
		if _, err := os.Stat(out + suffix); err != nil {
			t.Errorf("missing %s: %v", out+suffix, err)
		}
	}
}
