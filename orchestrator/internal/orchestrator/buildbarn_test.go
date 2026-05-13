//go:build buildbarn

// buildbarn_test validates the M5 cache substrate against a real
// Buildbarn instance brought up via deploy/buildbarn/docker-compose.yml.
// Mirrors the cache-share keystone but talks to actual bb-storage
// instead of the in-process fake.
//
// Gated behind the `buildbarn` build tag and skips at runtime if
// BUILDBARN_ENDPOINT (or the default localhost endpoint) isn't
// reachable. CI's `e2e-buildbarn` target runs `docker compose up`
// before invoking this; locally the operator is expected to start
// Buildbarn first per deploy/buildbarn/README.md.
package orchestrator_test

import (
	"context"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/sstriker/buildstream-bazel/internal/cas"
	"github.com/sstriker/buildstream-bazel/orchestrator/internal/orchestrator"
)

func TestE2E_Buildbarn_CacheShareAcrossOrchestrators(t *testing.T) {
	endpoint := os.Getenv("BUILDBARN_ENDPOINT")
	if endpoint == "" {
		endpoint = "127.0.0.1:8980"
	}
	if !strings.HasPrefix(endpoint, "grpc") {
		endpoint = "grpc://" + endpoint
	}
	if err := probeEndpoint(strings.TrimPrefix(strings.TrimPrefix(endpoint, "grpc://"), "grpcs://")); err != nil {
		t.Skipf("Buildbarn endpoint %s unreachable: %v\n  bring up with: docker compose -f deploy/buildbarn/docker-compose.yml up -d", endpoint, err)
	}

	storeA, err := cas.NewGRPCStore(context.Background(), cas.GRPCConfig{
		Endpoint: endpoint,
		Insecure: true,
	})
	if err != nil {
		t.Fatalf("dial storeA: %v", err)
	}
	defer storeA.Close()
	storeB, err := cas.NewGRPCStore(context.Background(), cas.GRPCConfig{
		Endpoint: endpoint,
		Insecure: true,
	})
	if err != nil {
		t.Fatalf("dial storeB: %v", err)
	}
	defer storeB.Close()

	stub := os.Args[0]
	t.Setenv("ORCHESTRATOR_STUB_CONVERTER", "1")
	t.Setenv("ORCHESTRATOR_STUB_MODE", "success")

	proj, g := mustLoadFixture(t)
	wantConv := []string{"components/hello", "components/uses-hello"}

	// Pass A — cold against the real Buildbarn instance.
	outA := t.TempDir()
	resA, err := orchestrator.Run(context.Background(), orchestrator.Options{
		Project:         proj,
		Graph:           g,
		Out:             outA,
		ConverterBinary: stub,
		Store:           storeA,
		Concurrency:     1, // serialize for stable log output
		Log:             testLog{t},
	})
	if err != nil {
		t.Fatalf("orchestrator A: %v", err)
	}
	// AC may already have warm entries from a previous test run;
	// require either every-element-miss (clean run) OR every-element-hit
	// (cache survived from prior run).
	if !sliceEqual(resA.Converted, wantConv) {
		t.Errorf("A.Converted = %v, want %v", resA.Converted, wantConv)
	}
	if len(resA.Failed) != 0 {
		t.Errorf("A.Failed = %v, want []", resA.Failed)
	}

	// Pass B — clean tmpdir, every element MUST hit AC since A just
	// published. (If A was itself a cache-warm pass, B is also a hit
	// pass; either way B's CacheMisses must be empty.)
	outB := t.TempDir()
	t.Setenv("ORCHESTRATOR_STUB_MODE", "tier2") // any miss explodes
	resB, err := orchestrator.Run(context.Background(), orchestrator.Options{
		Project:         proj,
		Graph:           g,
		Out:             outB,
		ConverterBinary: stub,
		Store:           storeB,
		Concurrency:     1,
		Log:             testLog{t},
	})
	if err != nil {
		t.Fatalf("orchestrator B (cache should hit, converter should not run): %v", err)
	}
	if len(resB.CacheMisses) != 0 {
		t.Errorf("B.CacheMisses = %v, want [] against shared Buildbarn AC", resB.CacheMisses)
	}
	if !sliceEqual(resB.CacheHits, wantConv) {
		t.Errorf("B.CacheHits = %v, want %v", resB.CacheHits, wantConv)
	}

	// Outputs must be byte-identical.
	for _, name := range wantConv {
		assertElementOutputsEqual(t, outA, outB, name)
	}
}

func probeEndpoint(addr string) error {
	c, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		return err
	}
	return c.Close()
}
