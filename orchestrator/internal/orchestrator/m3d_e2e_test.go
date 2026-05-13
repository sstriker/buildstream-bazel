package orchestrator_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"

	"github.com/sstriker/buildstream-bazel/internal/cas"
	"github.com/sstriker/buildstream-bazel/internal/cas/fakecas"
	"github.com/sstriker/buildstream-bazel/orchestrator/internal/element"
	"github.com/sstriker/buildstream-bazel/orchestrator/internal/orchestrator"
)

// TestRun_M3D_RemoteAssetSourcesEndToEnd is the M3d-step-1 keystone
// at the orchestrator level: a synthetic fdsdk-style fixture declares
// every cmake element with `kind: remote-asset` sources, the test
// pre-populates a fake CAS+RAA with the source trees (mimicking what
// `bst source push` produces), and the orchestrator converts cleanly
// without touching the filesystem origin.
func TestRun_M3D_RemoteAssetSourcesEndToEnd(t *testing.T) {
	ctx := context.Background()

	// Stand up fake CAS+RAA on a single endpoint.
	srv := fakecas.New()
	asset := fakecas.NewAssetServer()
	ep := fakecas.Start(t, srv, fakecas.WithAsset(asset))
	defer ep.Close()

	store, err := cas.NewGRPCStore(ctx, cas.GRPCConfig{
		Endpoint: "grpc://" + ep.Addr,
		Insecure: true,
	})
	if err != nil {
		t.Fatalf("dial cas: %v", err)
	}
	defer store.Close()
	ra, err := cas.NewRemoteAsset(ctx, cas.RemoteAssetConfig{
		Endpoint: "grpc://" + ep.Addr,
		Insecure: true,
	})
	if err != nil {
		t.Fatalf("dial raa: %v", err)
	}
	defer ra.Close()

	// Build a synthetic fixture in a tmpdir. One cmake element with a
	// kind:remote-asset source — no kind:local paths, the orchestrator
	// must go through the RAA path or fail.
	fixture := t.TempDir()
	if err := os.MkdirAll(filepath.Join(fixture, "elements", "components"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	bstBody := []byte(`kind: cmake
sources:
- kind: remote-asset
  uri: bst:source:components/hello@deadbeef
  qualifiers:
    bst-source-kind: git
    bst-source-ref: deadbeef
`)
	if err := os.WriteFile(filepath.Join(fixture, "elements", "components", "hello.bst"), bstBody, 0o644); err != nil {
		t.Fatalf("write hello.bst: %v", err)
	}

	// Stage the source tree in CAS, bind it to the asset URI.
	uploadSource(t, ctx, store, ra, "bst:source:components/hello@deadbeef",
		map[string][]byte{
			"CMakeLists.txt": []byte("cmake_minimum_required(VERSION 3.10)\nproject(hello)\nadd_library(hello hello.c)\n"),
			"hello.c":        []byte("int hello(void) { return 0; }\n"),
		},
		[]cas.Qualifier{
			{Name: "bst-source-kind", Value: "git"},
			{Name: "bst-source-ref", Value: "deadbeef"},
		},
	)

	proj, err := element.ReadProject(fixture, "elements")
	if err != nil {
		t.Fatalf("ReadProject: %v", err)
	}
	g, err := element.BuildGraph(proj)
	if err != nil {
		t.Fatalf("BuildGraph: %v", err)
	}

	stub := os.Args[0]
	t.Setenv("ORCHESTRATOR_STUB_CONVERTER", "1")
	t.Setenv("ORCHESTRATOR_STUB_MODE", "success")

	out := t.TempDir()
	res, err := orchestrator.Run(ctx, orchestrator.Options{
		Project:         proj,
		Graph:           g,
		Out:             out,
		ConverterBinary: stub,
		Store:           store,
		SourceAsset:     ra,
		Concurrency:     1,
		Log:             testLog{t},
	})
	if err != nil {
		t.Fatalf("orchestrator: %v", err)
	}
	if !sliceEqual(res.Converted, []string{"components/hello"}) {
		t.Errorf("Converted = %v, want [components/hello]", res.Converted)
	}
	if len(res.Failed) != 0 {
		t.Errorf("Failed = %v, want []", res.Failed)
	}

	// Source must have materialized into the per-element source cache
	// (<out>/sources/<key>/checkout/). Existence of CMakeLists.txt at
	// the cached path proves RAA->CAS->disk round trip succeeded.
	matches, _ := filepath.Glob(filepath.Join(out, "sources", "*", "checkout", "CMakeLists.txt"))
	if len(matches) != 1 {
		t.Errorf("expected exactly one materialized CMakeLists.txt under <out>/sources/, got %v", matches)
	}
}

// uploadSource packs files into a CAS Directory, uploads the Directory
// + every file blob, and binds the URI in the asset server via the
// gRPC Push client. Mimics `bst source push` for one tree.
func uploadSource(
	t *testing.T,
	ctx context.Context,
	store cas.Store,
	ra *cas.RemoteAsset,
	uri string,
	files map[string][]byte,
	qualifiers []cas.Qualifier,
) {
	t.Helper()

	dir := &repb.Directory{}
	for name, body := range files {
		d := cas.DigestOf(body)
		if err := store.PutBlob(ctx, d, body); err != nil {
			t.Fatalf("PutBlob %s: %v", name, err)
		}
		dir.Files = append(dir.Files, &repb.FileNode{Name: name, Digest: d})
	}
	cas.SortDirectory(dir)
	dirBody, err := cas.MarshalDeterministic(dir)
	if err != nil {
		t.Fatalf("marshal dir: %v", err)
	}
	dirDigest := cas.DigestOf(dirBody)
	if err := store.PutBlob(ctx, dirDigest, dirBody); err != nil {
		t.Fatalf("upload dir: %v", err)
	}
	if err := ra.PushDirectory(ctx, uri, dirDigest, qualifiers...); err != nil {
		t.Fatalf("push asset %s: %v", uri, err)
	}
}
