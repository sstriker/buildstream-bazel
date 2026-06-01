// Package fakecas is an in-process REAPI server (CAS + ActionCache +
// ByteStream) used by tests that exercise the gRPC client paths
// without a real Buildbarn endpoint. Production code MUST NOT import
// this package — it lives in internal/ only so test binaries across
// the module can share it.
package fakecas

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"

	raa "github.com/bazelbuild/remote-apis/build/bazel/remote/asset/v1"
	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"google.golang.org/genproto/googleapis/bytestream"
	rpcstatus "google.golang.org/genproto/googleapis/rpc/status"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// Server holds the in-memory blob and ActionCache state. Multiple gRPC
// servers can share one Server to back multiple clients pointing at
// "different endpoints".
type Server struct {
	repb.UnimplementedContentAddressableStorageServer
	repb.UnimplementedActionCacheServer
	bytestream.UnimplementedByteStreamServer

	mu     sync.Mutex
	blobs  map[string][]byte
	action map[string]*repb.ActionResult
}

// New returns an empty fake server.
func New() *Server {
	return &Server{
		blobs:  make(map[string][]byte),
		action: make(map[string]*repb.ActionResult),
	}
}

// BlobCount reports the number of CAS entries currently held.
func (f *Server) BlobCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.blobs)
}

// ActionResultCount reports the number of AC entries currently held.
func (f *Server) ActionResultCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.action)
}

// FindMissingBlobs implements the REAPI service.
func (f *Server) FindMissingBlobs(_ context.Context, req *repb.FindMissingBlobsRequest) (*repb.FindMissingBlobsResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	resp := &repb.FindMissingBlobsResponse{}
	for _, d := range req.BlobDigests {
		if _, ok := f.blobs[d.Hash]; !ok {
			resp.MissingBlobDigests = append(resp.MissingBlobDigests, d)
		}
	}
	return resp, nil
}

// BatchUpdateBlobs implements the REAPI service.
func (f *Server) BatchUpdateBlobs(_ context.Context, req *repb.BatchUpdateBlobsRequest) (*repb.BatchUpdateBlobsResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	resp := &repb.BatchUpdateBlobsResponse{}
	for _, r := range req.Requests {
		f.blobs[r.Digest.Hash] = append([]byte(nil), r.Data...)
		resp.Responses = append(resp.Responses, &repb.BatchUpdateBlobsResponse_Response{
			Digest: r.Digest,
		})
	}
	return resp, nil
}

// BatchReadBlobs implements the REAPI service.
func (f *Server) BatchReadBlobs(_ context.Context, req *repb.BatchReadBlobsRequest) (*repb.BatchReadBlobsResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	resp := &repb.BatchReadBlobsResponse{}
	for _, d := range req.Digests {
		body, ok := f.blobs[d.Hash]
		entry := &repb.BatchReadBlobsResponse_Response{Digest: d}
		if ok {
			entry.Data = body
		} else {
			entry.Status = &rpcstatus.Status{Code: int32(codes.NotFound)}
		}
		resp.Responses = append(resp.Responses, entry)
	}
	return resp, nil
}

// GetActionResult implements the REAPI service.
func (f *Server) GetActionResult(_ context.Context, req *repb.GetActionResultRequest) (*repb.ActionResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ar, ok := f.action[req.ActionDigest.Hash]
	if !ok {
		return nil, status.Error(codes.NotFound, "no entry")
	}
	return proto.Clone(ar).(*repb.ActionResult), nil
}

// UpdateActionResult implements the REAPI service.
func (f *Server) UpdateActionResult(_ context.Context, req *repb.UpdateActionResultRequest) (*repb.ActionResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.action[req.ActionDigest.Hash] = proto.Clone(req.ActionResult).(*repb.ActionResult)
	return req.ActionResult, nil
}

// Read serves ByteStream Read against the in-memory blob store.
// resource_name format: [instance/]blobs/{hash}/{size}
func (f *Server) Read(req *bytestream.ReadRequest, stream bytestream.ByteStream_ReadServer) error {
	hash, err := parseReadResource(req.ResourceName)
	if err != nil {
		return status.Errorf(codes.InvalidArgument, "%v", err)
	}
	f.mu.Lock()
	body, ok := f.blobs[hash]
	f.mu.Unlock()
	if !ok {
		return status.Errorf(codes.NotFound, "blob %s missing", hash)
	}
	const chunk = 64 * 1024
	for off := int64(0); off < int64(len(body)); off += chunk {
		end := off + chunk
		if end > int64(len(body)) {
			end = int64(len(body))
		}
		if err := stream.Send(&bytestream.ReadResponse{Data: body[off:end]}); err != nil {
			return err
		}
	}
	return nil
}

// Write serves ByteStream Write. resource_name format:
// [instance/]uploads/{uuid}/blobs/{hash}/{size}
func (f *Server) Write(stream bytestream.ByteStream_WriteServer) error {
	var (
		hash string
		buf  bytes.Buffer
	)
	for {
		req, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		if hash == "" && req.ResourceName != "" {
			h, err := parseWriteResource(req.ResourceName)
			if err != nil {
				return status.Errorf(codes.InvalidArgument, "%v", err)
			}
			hash = h
		}
		buf.Write(req.Data)
		if req.FinishWrite {
			break
		}
	}
	if hash == "" {
		return status.Error(codes.InvalidArgument, "resource_name never set")
	}
	body := buf.Bytes()
	f.mu.Lock()
	f.blobs[hash] = body
	f.mu.Unlock()
	return stream.SendAndClose(&bytestream.WriteResponse{CommittedSize: int64(len(body))})
}

func parseReadResource(rn string) (string, error) {
	parts := strings.Split(rn, "/")
	for i, p := range parts {
		if p == "blobs" && i+2 < len(parts) {
			if _, err := strconv.ParseInt(parts[i+2], 10, 64); err != nil {
				return "", err
			}
			return parts[i+1], nil
		}
	}
	return "", errors.New("missing blobs/ segment in resource_name")
}

func parseWriteResource(rn string) (string, error) {
	parts := strings.Split(rn, "/")
	for i, p := range parts {
		if p == "uploads" && i+1 < len(parts) {
			rest := parts[i+2:]
			for j, q := range rest {
				if q == "blobs" && j+2 < len(rest) {
					return rest[j+1], nil
				}
			}
		}
	}
	return "", errors.New("missing uploads/.../blobs/ segment in resource_name")
}

// Endpoint represents a running fake CAS gRPC server.
type Endpoint struct {
	Server   *Server
	Addr     string
	teardown func()
}

// Close shuts down the gRPC server and releases the listener.
func (e *Endpoint) Close() { e.teardown() }

// Option configures Start.
type Option func(*startConfig)

type startConfig struct {
	asset *AssetServer
}

// WithAsset registers an AssetServer (Fetch + Push) alongside the
// standard services. Tests that exercise the M3d source-CAS resolver
// must enable this.
func WithAsset(asset *AssetServer) Option {
	return func(c *startConfig) { c.asset = asset }
}

// Start spins up an in-process gRPC server backed by srv on a random
// localhost port and returns an Endpoint. Test callers are responsible
// for closing it.
func Start(t testing.TB, srv *Server, opts ...Option) *Endpoint {
	t.Helper()
	cfg := &startConfig{}
	for _, o := range opts {
		o(cfg)
	}
	gs := grpc.NewServer()
	repb.RegisterContentAddressableStorageServer(gs, srv)
	repb.RegisterActionCacheServer(gs, srv)
	bytestream.RegisterByteStreamServer(gs, srv)
	if cfg.asset != nil {
		raa.RegisterFetchServer(gs, cfg.asset)
		raa.RegisterPushServer(gs, cfg.asset)
	}

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() { _ = gs.Serve(lis) }()
	return &Endpoint{
		Server: srv,
		Addr:   lis.Addr().String(),
		teardown: func() {
			gs.Stop()
			_ = lis.Close()
		},
	}
}

// Dial returns a plain insecure client connection to the endpoint;
// callers wrap it in cas.NewGRPCStore-equivalent shapes.
func (e *Endpoint) Dial(t testing.TB) *grpc.ClientConn {
	t.Helper()
	conn, err := grpc.NewClient(e.Addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	return conn
}
