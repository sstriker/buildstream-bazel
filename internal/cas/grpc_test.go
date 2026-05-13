package cas

import (
	"bytes"
	"context"
	"errors"
	"testing"

	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"google.golang.org/genproto/googleapis/bytestream"

	"github.com/sstriker/buildstream-bazel/internal/cas/fakecas"
)

// startFakeCAS spins up an in-process gRPC server and returns a connected
// GRPCStore plus a teardown.
func startFakeCAS(t *testing.T) (*GRPCStore, *fakecas.Server, func()) {
	t.Helper()
	srv := fakecas.New()
	ep := fakecas.Start(t, srv)
	conn := ep.Dial(t)
	s := &GRPCStore{
		conn:             conn,
		cas:              repb.NewContentAddressableStorageClient(conn),
		ac:               repb.NewActionCacheClient(conn),
		bs:               bytestream.NewByteStreamClient(conn),
		MaxBatchBlobSize: 64, // force ByteStream for blobs > 64 bytes
	}
	teardown := func() {
		conn.Close()
		ep.Close()
	}
	return s, srv, teardown
}

func TestGRPCStore_BatchRoundTrip(t *testing.T) {
	s, _, teardown := startFakeCAS(t)
	defer teardown()
	ctx := context.Background()
	body := []byte("small")
	d := DigestOf(body)

	if err := s.PutBlob(ctx, d, body); err != nil {
		t.Fatalf("PutBlob: %v", err)
	}
	got, err := s.GetBlob(ctx, d)
	if err != nil {
		t.Fatalf("GetBlob: %v", err)
	}
	if string(got) != string(body) {
		t.Errorf("body mismatch: got %q want %q", got, body)
	}
}

func TestGRPCStore_ByteStreamRoundTrip(t *testing.T) {
	s, _, teardown := startFakeCAS(t)
	defer teardown()
	ctx := context.Background()
	body := bytes.Repeat([]byte("X"), 1024) // > MaxBatchBlobSize=64
	d := DigestOf(body)

	if err := s.PutBlob(ctx, d, body); err != nil {
		t.Fatalf("PutBlob (stream): %v", err)
	}
	got, err := s.GetBlob(ctx, d)
	if err != nil {
		t.Fatalf("GetBlob (stream): %v", err)
	}
	if !bytes.Equal(got, body) {
		t.Errorf("stream body mismatch: got %d bytes want %d", len(got), len(body))
	}
}

func TestGRPCStore_FindMissing(t *testing.T) {
	s, _, teardown := startFakeCAS(t)
	defer teardown()
	ctx := context.Background()

	present := []byte("here")
	pd := DigestOf(present)
	if err := s.PutBlob(ctx, pd, present); err != nil {
		t.Fatalf("PutBlob: %v", err)
	}
	absent := DigestOf([]byte("not stored"))

	missing, err := s.FindMissing(ctx, []*Digest{pd, absent})
	if err != nil {
		t.Fatalf("FindMissing: %v", err)
	}
	if len(missing) != 1 || !DigestEqual(missing[0], absent) {
		t.Errorf("expected [%s], got %v", DigestString(absent), missing)
	}
}

func TestGRPCStore_GetBlob_NotFound(t *testing.T) {
	s, _, teardown := startFakeCAS(t)
	defer teardown()
	_, err := s.GetBlob(context.Background(), DigestOf([]byte("never put")))
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
}

func TestGRPCStore_ActionResultRoundTrip(t *testing.T) {
	s, _, teardown := startFakeCAS(t)
	defer teardown()
	ctx := context.Background()

	ad := DigestOf([]byte("action"))
	want := &repb.ActionResult{
		ExitCode: 0,
		OutputFiles: []*repb.OutputFile{
			{Path: "BUILD.bazel", Digest: DigestOf([]byte("BUILD"))},
		},
	}
	if err := s.UpdateActionResult(ctx, ad, want); err != nil {
		t.Fatalf("UpdateActionResult: %v", err)
	}
	got, err := s.GetActionResult(ctx, ad)
	if err != nil {
		t.Fatalf("GetActionResult: %v", err)
	}
	if got.ExitCode != want.ExitCode {
		t.Errorf("exit_code: got %d want %d", got.ExitCode, want.ExitCode)
	}
	if len(got.OutputFiles) != 1 || got.OutputFiles[0].Path != "BUILD.bazel" {
		t.Errorf("output_files: %+v", got.OutputFiles)
	}
}

func TestGRPCStore_GetActionResult_NotFound(t *testing.T) {
	s, _, teardown := startFakeCAS(t)
	defer teardown()
	_, err := s.GetActionResult(context.Background(), DigestOf([]byte("none")))
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
}
