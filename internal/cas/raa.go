package cas

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"

	raa "github.com/bazelbuild/remote-apis/build/bazel/remote/asset/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// RemoteAsset wraps the REAPI Remote Asset Fetch service. It maps a
// (uri, qualifier...) tuple to a Directory digest already resident in
// the same CAS the orchestrator's Store reads — the standard flow
// BuildStream's `bst source push` populates.
//
// M3d uses this to look up an FDSDK element's source by digest,
// skipping orchestrator-side git/tar/bst checkouts entirely.
type RemoteAsset struct {
	conn         *grpc.ClientConn
	fetch        raa.FetchClient
	push         raa.PushClient
	instanceName string
	token        string
}

// RemoteAssetConfig configures a RemoteAsset client. Endpoint
// conventions match GRPCConfig — `grpc://host:port` (insecure) or
// `grpcs://host:port` (TLS) — so the same flag plumbing the CAS
// already uses applies here.
type RemoteAssetConfig struct {
	Endpoint     string
	InstanceName string

	Insecure    bool
	TLSCertFile string
	TLSKeyFile  string
	CAFile      string
	TokenFile   string
}

// NewRemoteAsset dials the Remote Asset endpoint and constructs a
// client. The connection is independent of the CAS connection; in
// production both typically point at the same gRPC endpoint
// (Buildbarn collocates services).
func NewRemoteAsset(ctx context.Context, cfg RemoteAssetConfig) (*RemoteAsset, error) {
	endpoint, scheme := normalizeEndpoint(cfg.Endpoint)
	useTLS := !cfg.Insecure
	switch scheme {
	case "grpc":
		useTLS = false
	case "grpcs":
		useTLS = true
	}

	var dialOpts []grpc.DialOption
	if useTLS {
		tc, err := buildTLS(GRPCConfig{
			TLSCertFile: cfg.TLSCertFile,
			TLSKeyFile:  cfg.TLSKeyFile,
			CAFile:      cfg.CAFile,
		})
		if err != nil {
			return nil, fmt.Errorf("raa tls: %w", err)
		}
		dialOpts = append(dialOpts, grpc.WithTransportCredentials(credentials.NewTLS(tc)))
	} else {
		dialOpts = append(dialOpts, grpc.WithTransportCredentials(insecure.NewCredentials()))
	}

	conn, err := grpc.NewClient(endpoint, dialOpts...)
	if err != nil {
		return nil, fmt.Errorf("raa dial %s: %w", endpoint, err)
	}
	r := &RemoteAsset{
		conn:         conn,
		fetch:        raa.NewFetchClient(conn),
		push:         raa.NewPushClient(conn),
		instanceName: cfg.InstanceName,
	}
	if cfg.TokenFile != "" {
		// Reuse cas.GRPCStore's token-loading by reading once here.
		// The Store handles auth; for RAA we replicate the small bit.
		t, err := readToken(cfg.TokenFile)
		if err != nil {
			conn.Close()
			return nil, err
		}
		r.token = t
	}
	return r, nil
}

// Close releases the underlying gRPC connection.
func (r *RemoteAsset) Close() error { return r.conn.Close() }

func (r *RemoteAsset) withAuth(ctx context.Context) context.Context {
	if r.token == "" {
		return ctx
	}
	return metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+r.token)
}

// FetchDirectory maps a (uri, qualifiers...) tuple to a CAS Directory
// digest. Returns ErrNotFound when the asset isn't known to the
// server. The returned digest can be passed to MaterializeDirectory
// (or referenced from an Action's input root).
func (r *RemoteAsset) FetchDirectory(ctx context.Context, uri string, qualifiers ...Qualifier) (*Digest, error) {
	if uri == "" {
		return nil, errors.New("raa: uri required")
	}
	q := make([]*raa.Qualifier, 0, len(qualifiers))
	for _, p := range qualifiers {
		q = append(q, &raa.Qualifier{Name: p.Name, Value: p.Value})
	}
	resp, err := r.fetch.FetchDirectory(r.withAuth(ctx), &raa.FetchDirectoryRequest{
		InstanceName: r.instanceName,
		Uris:         []string{uri},
		Qualifiers:   q,
	})
	if err != nil {
		return nil, fmt.Errorf("raa FetchDirectory %s: %w", uri, err)
	}
	if resp.Status != nil && resp.Status.Code != 0 {
		// The proto carries grpc/codes via Code int32. NOT_FOUND = 5.
		if resp.Status.Code == 5 {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("raa FetchDirectory %s: code=%d msg=%s",
			uri, resp.Status.Code, resp.Status.Message)
	}
	if resp.RootDirectoryDigest == nil {
		return nil, fmt.Errorf("raa FetchDirectory %s: server returned OK but no digest", uri)
	}
	return resp.RootDirectoryDigest, nil
}

// PushDirectory binds (uri, qualifiers) to a CAS Directory digest the
// caller has already uploaded. Used by tests and by tooling that
// pre-populates an Asset cache (the production-side equivalent is
// `bst source push`).
func (r *RemoteAsset) PushDirectory(ctx context.Context, uri string, digest *Digest, qualifiers ...Qualifier) error {
	q := make([]*raa.Qualifier, 0, len(qualifiers))
	for _, p := range qualifiers {
		q = append(q, &raa.Qualifier{Name: p.Name, Value: p.Value})
	}
	_, err := r.push.PushDirectory(r.withAuth(ctx), &raa.PushDirectoryRequest{
		InstanceName:        r.instanceName,
		Uris:                []string{uri},
		Qualifiers:          q,
		RootDirectoryDigest: digest,
	})
	if err != nil {
		if status.Code(err) == 5 {
			return ErrNotFound
		}
		return fmt.Errorf("raa PushDirectory %s: %w", uri, err)
	}
	return nil
}

// Qualifier is a cas-package-friendly mirror of the raa.Qualifier
// proto so callers don't have to import raa directly.
type Qualifier struct {
	Name  string
	Value string
}

// QualifiersFromMap converts a name->value map to a sorted Qualifier
// slice. Sort by name for stable wire ordering across runs (the spec
// requires unique names; ordering is otherwise unconstrained).
func QualifiersFromMap(m map[string]string) []Qualifier {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]Qualifier, 0, len(keys))
	for _, k := range keys {
		out = append(out, Qualifier{Name: k, Value: m[k]})
	}
	return out
}

// readToken loads a bearer token from disk. Whitespace is trimmed.
func readToken(path string) (string, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("raa token: %w", err)
	}
	return strings.TrimSpace(string(body)), nil
}
