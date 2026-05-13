package reapi

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"

	"github.com/sstriker/buildstream-bazel/internal/cas"
)

// Executor submits a BuiltAction for execution and returns the
// resulting ActionResult. M5 ships a noop implementation (the
// orchestrator runs the converter locally); M3b's GRPCExecutor talks
// to a Buildbarn-style Execution service.
//
// Implementations are responsible for ensuring every input blob the
// Action references is in CAS before the Action runs — workers
// materialize the input root from the same CAS the client reads.
type Executor interface {
	Execute(ctx context.Context, store cas.Store, built *BuiltAction) (*repb.ActionResult, error)
}

// GRPCExecutor submits an Action to a REAPI Execution service over
// gRPC. The same connection / store / instance_name conventions as
// cas.GRPCStore apply.
type GRPCExecutor struct {
	client       repb.ExecutionClient
	instanceName string
}

// NewGRPCExecutor wires an Execution client onto an existing gRPC
// connection. Callers pass the same connection used for CAS / AC so
// blob uploads and Execute share the same auth + transport.
func NewGRPCExecutor(conn *grpc.ClientConn, instanceName string) *GRPCExecutor {
	return &GRPCExecutor{
		client:       repb.NewExecutionClient(conn),
		instanceName: instanceName,
	}
}

// Execute uploads the action's CAS-resident inputs (Action proto +
// Command proto + every Directory and file blob in InputRoot), then
// submits an ExecuteRequest. It blocks streaming the resulting
// long-running Operation until done, returning the embedded
// ActionResult.
//
// Cache lookup is the caller's responsibility — Execute always
// schedules an action regardless of AC state (skip_cache_lookup is
// not set; a Buildbarn that has the entry will short-circuit).
func (e *GRPCExecutor) Execute(ctx context.Context, store cas.Store, built *BuiltAction) (*repb.ActionResult, error) {
	if err := UploadInputs(ctx, store, built); err != nil {
		return nil, fmt.Errorf("execute: upload inputs: %w", err)
	}

	stream, err := e.client.Execute(ctx, &repb.ExecuteRequest{
		InstanceName: e.instanceName,
		ActionDigest: built.ActionDigest,
	})
	if err != nil {
		return nil, fmt.Errorf("execute: submit: %w", err)
	}
	for {
		op, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return nil, errors.New("execute: stream ended without Done operation")
		}
		if err != nil {
			return nil, fmt.Errorf("execute: recv: %w", err)
		}
		if !op.Done {
			continue
		}
		switch r := op.Result.(type) {
		case nil:
			return nil, errors.New("execute: Done operation has no Result")
		case interface{ GetError() any }:
			return nil, fmt.Errorf("execute: %v", r.GetError())
		}
		// Operation_Response is the success path — Any-wrapped ExecuteResponse.
		if errResult := op.GetError(); errResult != nil {
			return nil, fmt.Errorf("execute: server error code=%d msg=%s",
				errResult.Code, errResult.Message)
		}
		respAny := op.GetResponse()
		if respAny == nil {
			return nil, errors.New("execute: Done operation has neither Response nor Error")
		}
		execResp := &repb.ExecuteResponse{}
		if err := respAny.UnmarshalTo(execResp); err != nil {
			return nil, fmt.Errorf("execute: unmarshal ExecuteResponse: %w", err)
		}
		if execResp.Status != nil && execResp.Status.Code != 0 {
			return nil, fmt.Errorf("execute: server status code=%d msg=%s",
				execResp.Status.Code, execResp.Status.Message)
		}
		if execResp.Result == nil {
			return nil, errors.New("execute: ExecuteResponse has no Result")
		}
		return execResp.Result, nil
	}
}

// UploadInputs ensures every blob the BuiltAction depends on is in
// CAS. Skips already-present blobs by FindMissing-first. Order:
// Directory protos, file blobs, then the Command and Action protos
// themselves. Workers expect Action & Command resolvable by digest.
func UploadInputs(ctx context.Context, store cas.Store, built *BuiltAction) error {
	// Collect digests for every Directory proto + file blob.
	dirDigests := make(map[string]*cas.Digest, len(built.InputRoot.Directories))
	dirBodies := make(map[string][]byte, len(built.InputRoot.Directories))
	for h, d := range built.InputRoot.Directories {
		body, err := cas.MarshalDeterministic(d)
		if err != nil {
			return fmt.Errorf("marshal directory %s: %w", h, err)
		}
		dirDigests[h] = &cas.Digest{Hash: h, SizeBytes: int64(len(body))}
		dirBodies[h] = body
	}

	fileDigests := make(map[string]*cas.Digest, len(built.InputRoot.Files))
	for h, p := range built.InputRoot.Files {
		info, err := os.Stat(p)
		if err != nil {
			return fmt.Errorf("stat input %s: %w", p, err)
		}
		fileDigests[h] = &cas.Digest{Hash: h, SizeBytes: info.Size()}
	}

	// Single FindMissing batch over the union; skips network round-trips
	// for every blob already in CAS (cross-element dep dirs reuse).
	allDigests := make([]*cas.Digest, 0, len(dirDigests)+len(fileDigests)+2)
	for _, d := range dirDigests {
		allDigests = append(allDigests, d)
	}
	for _, d := range fileDigests {
		allDigests = append(allDigests, d)
	}
	allDigests = append(allDigests,
		built.CommandDigest,
		built.ActionDigest,
	)
	missing, err := store.FindMissing(ctx, allDigests)
	if err != nil {
		return fmt.Errorf("findmissing: %w", err)
	}
	missingSet := make(map[string]bool, len(missing))
	for _, d := range missing {
		missingSet[d.Hash] = true
	}

	// Upload missing Directory protos.
	for h, d := range dirDigests {
		if !missingSet[h] {
			continue
		}
		if err := store.PutBlob(ctx, d, dirBodies[h]); err != nil {
			return fmt.Errorf("upload directory %s: %w", h, err)
		}
	}
	// Upload missing file blobs.
	for h, d := range fileDigests {
		if !missingSet[h] {
			continue
		}
		body, err := os.ReadFile(built.InputRoot.Files[h])
		if err != nil {
			return fmt.Errorf("read input %s: %w", built.InputRoot.Files[h], err)
		}
		if err := store.PutBlob(ctx, d, body); err != nil {
			return fmt.Errorf("upload file %s: %w", h, err)
		}
	}
	// Always upload Command + Action if missing.
	if missingSet[built.CommandDigest.Hash] {
		if err := store.PutBlob(ctx, built.CommandDigest, built.CommandBlob); err != nil {
			return fmt.Errorf("upload command: %w", err)
		}
	}
	if missingSet[built.ActionDigest.Hash] {
		if err := store.PutBlob(ctx, built.ActionDigest, built.ActionBlob); err != nil {
			return fmt.Errorf("upload action: %w", err)
		}
	}
	return nil
}

// ensure proto package linkage is resolved (used via repb above; the
// compiler is happy without an explicit reference, but a no-op
// reference here makes a future refactor that removes the only use of
// repb obvious instead of mysterious).
var _ proto.Message = (*repb.Action)(nil)
