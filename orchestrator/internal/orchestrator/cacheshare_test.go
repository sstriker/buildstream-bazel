package orchestrator_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/sstriker/buildstream-bazel/internal/cas"
	"github.com/sstriker/buildstream-bazel/internal/cas/fakecas"
	"github.com/sstriker/buildstream-bazel/orchestrator/internal/orchestrator"
)

// TestRun_CacheShare_TwoOrchestratorsViaSharedAC is the M5 architectural
// keystone test. Orchestrator A converts the FDSDK subset against a
// shared REAPI CAS+ActionCache; orchestrator B, with no local cache and
// a fresh tmpdir, hits AC for every element and produces byte-identical
// outputs without re-running the converter.
//
// The fake CAS+AC server is in-process but speaks real gRPC, so this
// exercises the entire transport path the production GRPCStore takes.
func TestRun_CacheShare_TwoOrchestratorsViaSharedAC(t *testing.T) {
	srv := fakecas.New()
	ep := fakecas.Start(t, srv)
	defer ep.Close()

	storeA, err := cas.NewGRPCStore(context.Background(), cas.GRPCConfig{
		Endpoint: "grpc://" + ep.Addr,
		Insecure: true,
	})
	if err != nil {
		t.Fatalf("dial storeA: %v", err)
	}
	defer storeA.Close()
	storeB, err := cas.NewGRPCStore(context.Background(), cas.GRPCConfig{
		Endpoint: "grpc://" + ep.Addr,
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

	// Orchestrator A: cold AC, every element runs the stub converter
	// and publishes its ActionResult to AC.
	outA := t.TempDir()
	resA, err := orchestrator.Run(context.Background(), orchestrator.Options{
		Project:         proj,
		Graph:           g,
		Out:             outA,
		ConverterBinary: stub,
		Store:           storeA,
		Log:             testLog{t},
	})
	if err != nil {
		t.Fatalf("orchestrator A: %v", err)
	}
	if len(resA.CacheHits) != 0 {
		t.Errorf("A.CacheHits = %v, want []", resA.CacheHits)
	}
	if !sliceEqual(resA.CacheMisses, wantConv) {
		t.Errorf("A.CacheMisses = %v, want %v", resA.CacheMisses, wantConv)
	}

	// AC must have one entry per converted element.
	if got := srv.ActionResultCount(); got != len(wantConv) {
		t.Errorf("AC entries after A: got %d want %d", got, len(wantConv))
	}
	if got := srv.BlobCount(); got == 0 {
		t.Errorf("CAS should have output blobs after A, got 0")
	}

	// Orchestrator B: clean tmpdir, same fixture, same shared CAS+AC.
	// Every element should hit AC; the stub MUST NOT run (its outputs
	// are reproduced from CAS). To prove the converter wasn't invoked
	// for B, force the stub to fail-loud if it runs.
	outB := t.TempDir()
	t.Setenv("ORCHESTRATOR_STUB_MODE", "tier2") // any miss explodes
	resB, err := orchestrator.Run(context.Background(), orchestrator.Options{
		Project:         proj,
		Graph:           g,
		Out:             outB,
		ConverterBinary: stub,
		Store:           storeB,
		Log:             testLog{t},
	})
	if err != nil {
		t.Fatalf("orchestrator B (cache should hit, converter should not run): %v", err)
	}
	if len(resB.CacheMisses) != 0 {
		t.Errorf("B.CacheMisses = %v, want [] (every element should hit AC)", resB.CacheMisses)
	}
	if !sliceEqual(resB.CacheHits, wantConv) {
		t.Errorf("B.CacheHits = %v, want %v", resB.CacheHits, wantConv)
	}

	// Outputs must be byte-identical between A and B for every
	// converted element's published files (BUILD.bazel +
	// cmake-config/* + read_paths.json).
	for _, name := range wantConv {
		assertElementOutputsEqual(t, outA, outB, name)
	}
}

// assertElementOutputsEqual hashes every regular file under
// elements/<name>/ in dirA and dirB and asserts the per-file digests
// match. Failure points at the first divergent path.
func assertElementOutputsEqual(t *testing.T, dirA, dirB, name string) {
	t.Helper()
	pathA := filepath.Join(dirA, "elements", name)
	pathB := filepath.Join(dirB, "elements", name)
	hashesA := hashTree(t, pathA)
	hashesB := hashTree(t, pathB)

	if len(hashesA) != len(hashesB) {
		t.Errorf("%s: file count differs A=%d B=%d", name, len(hashesA), len(hashesB))
	}
	keysA := sortedKeys(hashesA)
	keysB := sortedKeys(hashesB)
	if strings.Join(keysA, ",") != strings.Join(keysB, ",") {
		t.Errorf("%s: file set differs\n  A: %v\n  B: %v", name, keysA, keysB)
		return
	}
	for _, rel := range keysA {
		if hashesA[rel] != hashesB[rel] {
			t.Errorf("%s/%s: digest mismatch\n  A=%s\n  B=%s", name, rel, hashesA[rel], hashesB[rel])
		}
	}
}

func hashTree(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	if err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		body, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(body)
		out[filepath.ToSlash(rel)] = hex.EncodeToString(sum[:])
		return nil
	}); err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	return out
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
