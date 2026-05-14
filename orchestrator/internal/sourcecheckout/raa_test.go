package sourcecheckout_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"google.golang.org/protobuf/proto"

	"github.com/sstriker/buildstream-bazel/internal/cas"
	"github.com/sstriker/buildstream-bazel/internal/cas/fakecas"
	"github.com/sstriker/buildstream-bazel/internal/element"
	"github.com/sstriker/buildstream-bazel/orchestrator/internal/sourcecheckout"
)

// TestResolve_RemoteAsset_FetchAndMaterialize is the M3d keystone:
//  1. Stage a source tree directly into a fake CAS as a Directory
//     proto (mimicking what `bst source push` would have done).
//  2. Bind it to a uri+qualifiers in the fake Asset server (mimicking
//     `bst source push --remote=...`).
//  3. Have the resolver materialize it given just the spec.
//
// The orchestrator never touches the network or the source's origin —
// the entire flow is digest-resolved against CAS+RAA.
func TestResolve_RemoteAsset_FetchAndMaterialize(t *testing.T) {
	ctx := context.Background()

	// Spin up a fake CAS+RAA endpoint.
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

	// Stage two files as a single Directory in CAS.
	helloC := []byte("// fake hello.c\nint hello(void) { return 0; }\n")
	cmakeLists := []byte("project(hello)\nadd_library(hello hello.c)\n")
	dirProto := &repb.Directory{
		Files: []*repb.FileNode{
			{Name: "CMakeLists.txt", Digest: putBlob(t, store, cmakeLists)},
			{Name: "hello.c", Digest: putBlob(t, store, helloC)},
		},
	}
	cas.SortDirectory(dirProto)
	dirBody, err := cas.MarshalDeterministic(dirProto)
	if err != nil {
		t.Fatalf("marshal dir: %v", err)
	}
	dirDigest := digestOf(dirBody)
	if err := store.PutBlob(ctx, dirDigest, dirBody); err != nil {
		t.Fatalf("upload dir: %v", err)
	}

	// Bind the asset URI to the Directory digest.
	if err := ra.PushDirectory(ctx,
		"bst:source:components/hello@deadbeef",
		dirDigest,
		cas.Qualifier{Name: "bst-source-kind", Value: "git"},
		cas.Qualifier{Name: "bst-source-ref", Value: "deadbeef"},
	); err != nil {
		t.Fatalf("push dir: %v", err)
	}

	// Resolver wired with both Asset + Store.
	r := &sourcecheckout.Resolver{
		CacheDir: t.TempDir(),
		Asset:    ra,
		Store:    store,
	}
	el := &element.Element{
		Name: "components/hello",
		Sources: []element.Source{{
			Kind: "remote-asset",
			Extra: map[string]any{
				"uri": "bst:source:components/hello@deadbeef",
				"qualifiers": map[string]any{
					"bst-source-kind": "git",
					"bst-source-ref":  "deadbeef",
				},
			},
		}},
	}

	// First call: fetch + materialize.
	first, err := r.Resolve(ctx, el)
	if err != nil {
		t.Fatalf("first Resolve: %v", err)
	}
	for name, want := range map[string][]byte{"CMakeLists.txt": cmakeLists, "hello.c": helloC} {
		got, err := os.ReadFile(filepath.Join(first.Path, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if string(got) != string(want) {
			t.Errorf("%s body mismatch", name)
		}
	}
	if first.Digest == nil {
		t.Errorf("kind:remote-asset Resolve returned nil Digest")
	}

	// Second call: cache hit, no extra RAA / CAS round-trip needed.
	second, err := r.Resolve(ctx, el)
	if err != nil {
		t.Fatalf("second Resolve: %v", err)
	}
	if first.Path != second.Path {
		t.Errorf("cache miss on second call: %q vs %q", first.Path, second.Path)
	}
	if second.Digest == nil || (first.Digest != nil && first.Digest.Hash != second.Digest.Hash) {
		t.Errorf("digest changed across cache hits: first=%v second=%v", first.Digest, second.Digest)
	}
}

func TestResolve_RemoteAsset_MissingDigestSurfaces(t *testing.T) {
	ctx := context.Background()
	srv := fakecas.New()
	asset := fakecas.NewAssetServer()
	ep := fakecas.Start(t, srv, fakecas.WithAsset(asset))
	defer ep.Close()

	store, _ := cas.NewGRPCStore(ctx, cas.GRPCConfig{Endpoint: "grpc://" + ep.Addr, Insecure: true})
	defer store.Close()
	ra, _ := cas.NewRemoteAsset(ctx, cas.RemoteAssetConfig{Endpoint: "grpc://" + ep.Addr, Insecure: true})
	defer ra.Close()

	r := &sourcecheckout.Resolver{
		CacheDir: t.TempDir(),
		Asset:    ra,
		Store:    store,
	}
	_, err := r.Resolve(ctx, &element.Element{
		Name: "x",
		Sources: []element.Source{{
			Kind:  "remote-asset",
			Extra: map[string]any{"uri": "bst:source:x@nope"},
		}},
	})
	if err == nil {
		t.Fatal("expected error when uri isn't in the asset server")
	}
}

func TestResolve_RemoteAsset_RequiresAssetAndStore(t *testing.T) {
	r := &sourcecheckout.Resolver{}
	_, err := r.Resolve(context.Background(), &element.Element{
		Name: "x",
		Sources: []element.Source{{
			Kind:  "remote-asset",
			Extra: map[string]any{"uri": "bst:source:x"},
		}},
	})
	if err == nil {
		t.Fatal("expected error when Asset+Store unwired")
	}
}

// helpers

func putBlob(t *testing.T, store cas.Store, body []byte) *repb.Digest {
	t.Helper()
	d := digestOf(body)
	if err := store.PutBlob(context.Background(), d, body); err != nil {
		t.Fatalf("PutBlob: %v", err)
	}
	return d
}

func digestOf(body []byte) *repb.Digest {
	sum := sha256.Sum256(body)
	return &repb.Digest{Hash: hex.EncodeToString(sum[:]), SizeBytes: int64(len(body))}
}

// keep proto imported
var _ = proto.Marshal
