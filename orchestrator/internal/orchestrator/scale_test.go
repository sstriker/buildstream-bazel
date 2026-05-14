package orchestrator_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/sstriker/buildstream-bazel/internal/cas"
	"github.com/sstriker/buildstream-bazel/internal/cas/fakecas"
	"github.com/sstriker/buildstream-bazel/internal/element"
	"github.com/sstriker/buildstream-bazel/orchestrator/internal/orchestrator"
)

// TestRun_Scale_DeterministicAcrossLevels: drive the orchestrator at three
// concurrency levels (1, 8, 32) against the 50-element fdsdk-scale fixture
// and assert all elements convert successfully and per-element output
// trees are byte-identical across all three levels. Wall-clock per level
// is logged for visibility but not asserted (CI noise variance kills
// numeric thresholds).
//
// This is what surfaces real-world concurrency issues at a non-trivial
// graph size: AC eviction races, memory pressure under high parallelism,
// queue-depth imbalances. The fdsdk-subset fixture is too small (3
// elements) to exercise any of that.
func TestRun_Scale_DeterministicAcrossLevels(t *testing.T) {
	stub := os.Args[0]
	t.Setenv("ORCHESTRATOR_STUB_CONVERTER", "1")
	t.Setenv("ORCHESTRATOR_STUB_MODE", "success")

	proj, g := mustLoadScaleFixture(t)
	if got := len(proj.Elements); got != 50 {
		t.Fatalf("scale fixture: expected 50 elements, got %d", got)
	}

	levels := []int{1, 8, 32}
	outs := make(map[int]string, len(levels))
	results := make(map[int]*orchestrator.Result, len(levels))
	for _, c := range levels {
		// Per-level fakecas instance so each pass is cold — apples-to-
		// apples timing. Sharing a CAS across passes makes pass 2+
		// all-hit and the timing comparison meaningless.
		srv := fakecas.New()
		ep := fakecas.Start(t, srv)
		store, err := cas.NewGRPCStore(context.Background(), cas.GRPCConfig{
			Endpoint: "grpc://" + ep.Addr,
			Insecure: true,
		})
		if err != nil {
			t.Fatalf("dial cas (c=%d): %v", c, err)
		}

		out := t.TempDir()
		start := time.Now()
		res, err := orchestrator.Run(context.Background(), orchestrator.Options{
			Project:         proj,
			Graph:           g,
			Out:             out,
			ConverterBinary: stub,
			Store:           store,
			Concurrency:     c,
			Log:             testLog{t},
		})
		elapsed := time.Since(start)
		store.Close()
		ep.Close()
		if err != nil {
			t.Fatalf("Run (concurrency=%d): %v", c, err)
		}
		if len(res.Failed) != 0 {
			t.Errorf("Run (concurrency=%d): %d failures: %v", c, len(res.Failed), res.Failed)
		}
		if len(res.Converted) != 50 {
			t.Errorf("Run (concurrency=%d): converted %d elements, want 50", c, len(res.Converted))
		}
		t.Logf("scale c=%d: elapsed=%s converted=%d miss=%d hit=%d",
			c, elapsed.Round(time.Millisecond), len(res.Converted), len(res.CacheMisses), len(res.CacheHits))
		outs[c] = out
		results[c] = res
	}

	// All three passes are cold (per-level fakecas reset above), so all
	// three should report 50 misses + 0 hits. Drift here means the
	// fakecas reset didn't take effect or AC bled across.
	for _, c := range levels {
		if len(results[c].CacheMisses) != 50 || len(results[c].CacheHits) != 0 {
			t.Errorf("c=%d: expected 50 miss / 0 hit (cold pass), got %d/%d",
				c, len(results[c].CacheMisses), len(results[c].CacheHits))
		}
	}

	// Byte-identity across all three concurrency levels.
	if !sliceEqual(results[1].Converted, results[8].Converted) ||
		!sliceEqual(results[1].Converted, results[32].Converted) {
		t.Errorf("Converted lists differ:\n  c=1:  %v\n  c=8:  %v\n  c=32: %v",
			results[1].Converted, results[8].Converted, results[32].Converted)
	}
	dirs := []string{outs[1], outs[8], outs[32]}
	for _, name := range results[1].Converted {
		scaleAssertBytewise(t, dirs, name)
	}
}

func mustLoadScaleFixture(t *testing.T) (*element.Project, *element.Graph) {
	t.Helper()
	root, err := filepath.Abs("../../testdata/fdsdk-scale")
	if err != nil {
		t.Fatal(err)
	}
	proj, err := element.ReadProject(root, "elements")
	if err != nil {
		t.Fatalf("ReadProject: %v", err)
	}
	g, err := element.BuildGraph(proj)
	if err != nil {
		t.Fatalf("BuildGraph: %v", err)
	}
	return proj, g
}

// scaleAssertBytewise compares per-file sha256 digests of the named
// element's output tree across all dirs. Reports specific file-level
// drift so a regression points operators at the offending output.
func scaleAssertBytewise(t *testing.T, dirs []string, name string) {
	t.Helper()
	hashes := make([]map[string]string, len(dirs))
	for i, d := range dirs {
		hashes[i] = scaleHashTree(t, filepath.Join(d, "elements", name))
	}
	keys := func(h map[string]string) []string {
		out := make([]string, 0, len(h))
		for k := range h {
			out = append(out, k)
		}
		sort.Strings(out)
		return out
	}
	ref := keys(hashes[0])
	for i := 1; i < len(hashes); i++ {
		got := keys(hashes[i])
		if strings.Join(ref, ",") != strings.Join(got, ",") {
			t.Errorf("%s: file set differs at dir[%d]:\n  ref: %v\n  got: %v", name, i, ref, got)
			return
		}
	}
	for _, rel := range ref {
		want := hashes[0][rel]
		for i := 1; i < len(hashes); i++ {
			if hashes[i][rel] != want {
				t.Errorf("%s/%s: digest mismatch (dir[0]=%s dir[%d]=%s)", name, rel, want, i, hashes[i][rel])
			}
		}
	}
}

func scaleHashTree(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		// Skip directories AND non-regular files (notably symlinks).
		// The orchestrator's source/ subdirectory is populated with
		// symlinks to host-absolute paths; their byte content varies
		// per host and they're irrelevant to converted-output byte-
		// identity (which is what this hash-tree comparison is for).
		if !info.Mode().IsRegular() {
			return nil
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		h := sha256.New()
		if _, err := io.Copy(h, f); err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		out[rel] = hex.EncodeToString(h.Sum(nil))
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	return out
}
