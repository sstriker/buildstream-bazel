package orchestrator_test

import (
	"bytes"
	"context"
	"os"
	"strings"
	"sync"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/sstriker/buildstream-bazel/internal/cas"
	"github.com/sstriker/buildstream-bazel/internal/cas/fakecas"
	"github.com/sstriker/buildstream-bazel/internal/reapi"
	"github.com/sstriker/buildstream-bazel/orchestrator/internal/orchestrator"
)

// TestRun_RemoteExecute_StreamsWorkerStdout verifies that when the
// worker runs the converter remotely, the converter's stdout/stderr
// (captured by the worker, uploaded to CAS as ar.StdoutDigest /
// StderrDigest) makes it back to the orchestrator's log writer.
//
// Operator-facing: lets debugging Tier-1 failures on a remote worker
// proceed without needing bb-browser to fetch the action result by
// hand.
func TestRun_RemoteExecute_StreamsWorkerStdout(t *testing.T) {
	srv := fakecas.New()
	worker := fakecas.NewExecutionServer(srv)
	worker.SkipCacheLookup = true
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
		t.Fatalf("dial executor: %v", err)
	}
	defer conn.Close()
	executor := reapi.NewGRPCExecutor(conn, "")

	stub := os.Args[0]
	t.Setenv("ORCHESTRATOR_STUB_CONVERTER", "1")
	t.Setenv("ORCHESTRATOR_STUB_MODE", "success")
	// The success-mode stub doesn't write to stdout. Add a sentinel
	// via env so the stub emits a recognizable line we can assert on.
	t.Setenv("ORCHESTRATOR_STUB_STDOUT_SENTINEL", "<<<worker-stdout-sentinel>>>")

	proj, g := mustLoadFixture(t)
	out := t.TempDir()

	captured := &syncBuf{}
	if _, err := orchestrator.Run(context.Background(), orchestrator.Options{
		Project:         proj,
		Graph:           g,
		Out:             out,
		ConverterBinary: stub,
		Store:           store,
		Executor:        executor,
		Concurrency:     1,
		Log:             captured,
	}); err != nil {
		t.Fatalf("orchestrator: %v", err)
	}

	if !strings.Contains(captured.String(), "<<<worker-stdout-sentinel>>>") {
		t.Errorf("orchestrator log did not include worker stdout sentinel:\n%s", captured.String())
	}
}

// syncBuf is bytes.Buffer with a mutex — the orchestrator log is
// written from multiple goroutines (per-element progress + worker
// output streamer).
type syncBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}
