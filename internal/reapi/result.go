package reapi

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"google.golang.org/protobuf/proto"

	"github.com/sstriker/buildstream-bazel/internal/cas"
)

// SynthesizeResult walks a local output directory after a successful
// converter run and builds an ActionResult that references every
// produced blob in CAS. As a side effect it uploads each output file
// (and Tree protos for output directories) via store.PutBlob.
//
// outputPaths must be the same list passed in Command.output_paths
// (relative to outDir's parent — i.e. the converter's working dir).
// Files that don't exist on disk (e.g. failure.json on a successful
// run) are skipped silently — REAPI semantics tolerate missing
// declared outputs.
//
// stdout / stderr are uploaded as separate CAS blobs and referenced by
// digest, matching what bazel + Buildbarn emit. Pass nil to skip.
func SynthesizeResult(
	ctx context.Context,
	store cas.Store,
	rootDir string,
	outputPaths []string,
	exitCode int32,
	stdout, stderr []byte,
) (*repb.ActionResult, error) {
	ar := &repb.ActionResult{ExitCode: exitCode}

	for _, rel := range outputPaths {
		host := filepath.Join(rootDir, rel)
		info, err := os.Stat(host)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("synth: stat %s: %w", host, err)
		}
		switch {
		case info.Mode().IsRegular():
			if err := addOutputFile(ctx, store, ar, host, rel); err != nil {
				return nil, err
			}
		case info.IsDir():
			if err := addOutputDir(ctx, store, ar, host, rel); err != nil {
				return nil, err
			}
		default:
			return nil, fmt.Errorf("synth: %s has unsupported mode %s", host, info.Mode())
		}
	}

	if len(stdout) > 0 {
		d := cas.DigestOf(stdout)
		if err := store.PutBlob(ctx, d, stdout); err != nil {
			return nil, fmt.Errorf("synth: upload stdout: %w", err)
		}
		ar.StdoutDigest = d
	}
	if len(stderr) > 0 {
		d := cas.DigestOf(stderr)
		if err := store.PutBlob(ctx, d, stderr); err != nil {
			return nil, fmt.Errorf("synth: upload stderr: %w", err)
		}
		ar.StderrDigest = d
	}

	return ar, nil
}

func addOutputFile(ctx context.Context, store cas.Store, ar *repb.ActionResult, host, rel string) error {
	body, err := os.ReadFile(host)
	if err != nil {
		return fmt.Errorf("synth: read %s: %w", host, err)
	}
	d := cas.DigestOf(body)
	if err := store.PutBlob(ctx, d, body); err != nil {
		return fmt.Errorf("synth: upload %s: %w", rel, err)
	}
	info, err := os.Stat(host)
	if err != nil {
		return err
	}
	ar.OutputFiles = append(ar.OutputFiles, &repb.OutputFile{
		Path:         filepath.ToSlash(rel),
		Digest:       d,
		IsExecutable: info.Mode()&0o111 != 0,
	})
	return nil
}

func addOutputDir(ctx context.Context, store cas.Store, ar *repb.ActionResult, host, rel string) error {
	tree, err := cas.PackDir(host)
	if err != nil {
		return fmt.Errorf("synth: pack %s: %w", host, err)
	}
	// Upload every Directory proto referenced by this output dir.
	for h, d := range tree.Directories {
		body, err := cas.MarshalDeterministic(d)
		if err != nil {
			return fmt.Errorf("synth: marshal directory %s: %w", h, err)
		}
		if err := store.PutBlob(ctx, &cas.Digest{Hash: h, SizeBytes: int64(len(body))}, body); err != nil {
			return fmt.Errorf("synth: upload directory %s: %w", h, err)
		}
	}
	// Upload every leaf file blob.
	for h, p := range tree.Files {
		body, err := os.ReadFile(p)
		if err != nil {
			return fmt.Errorf("synth: read %s: %w", p, err)
		}
		if err := store.PutBlob(ctx, &cas.Digest{Hash: h, SizeBytes: int64(len(body))}, body); err != nil {
			return fmt.Errorf("synth: upload file %s: %w", p, err)
		}
	}
	// Upload the canonical Tree proto and reference it from the
	// OutputDirectory entry.
	treeProto := tree.AsReapiTree()
	treeDigest, treeBlob, err := cas.DigestProto(treeProto)
	if err != nil {
		return fmt.Errorf("synth: digest tree %s: %w", rel, err)
	}
	if err := store.PutBlob(ctx, treeDigest, treeBlob); err != nil {
		return fmt.Errorf("synth: upload tree %s: %w", rel, err)
	}
	ar.OutputDirectories = append(ar.OutputDirectories, &repb.OutputDirectory{
		Path:       filepath.ToSlash(rel),
		TreeDigest: treeDigest,
	})
	return nil
}

// MaterializeResult writes every blob referenced by ar to rootDir at
// the recorded relative paths. Existing content under rootDir at those
// paths is overwritten.
//
// Returns ErrMissingBlob (wrapping cas.ErrNotFound) if any referenced
// blob is absent from the store — the caller should treat that as a
// stale ActionCache entry and re-execute, per the M5 plan's
// resilience case.
func MaterializeResult(
	ctx context.Context,
	store cas.Store,
	ar *repb.ActionResult,
	rootDir string,
) error {
	for _, of := range ar.OutputFiles {
		host := filepath.Join(rootDir, filepath.FromSlash(of.Path))
		if err := os.MkdirAll(filepath.Dir(host), 0o755); err != nil {
			return fmt.Errorf("materialize: mkdir %s: %w", filepath.Dir(host), err)
		}
		body, err := store.GetBlob(ctx, of.Digest)
		if err != nil {
			return wrapMissing(err, of.Path)
		}
		mode := os.FileMode(0o644)
		if of.IsExecutable {
			mode = 0o755
		}
		if err := os.WriteFile(host, body, mode); err != nil {
			return fmt.Errorf("materialize: write %s: %w", host, err)
		}
	}
	for _, od := range ar.OutputDirectories {
		host := filepath.Join(rootDir, filepath.FromSlash(od.Path))
		if err := materializeOutputDir(ctx, store, od.TreeDigest, host); err != nil {
			return wrapMissing(err, od.Path)
		}
	}
	return nil
}

// ErrMissingBlob wraps cas.ErrNotFound with the path of the referenced
// output that's missing. Callers can distinguish "AC entry stale"
// from other materialization failures by checking errors.Is(err, cas.ErrNotFound).
type ErrMissingBlob struct {
	Path string
	Err  error
}

func (e *ErrMissingBlob) Error() string { return fmt.Sprintf("missing blob for %s: %v", e.Path, e.Err) }
func (e *ErrMissingBlob) Unwrap() error { return e.Err }

func wrapMissing(err error, path string) error {
	if errors.Is(err, cas.ErrNotFound) {
		return &ErrMissingBlob{Path: path, Err: err}
	}
	return err
}

func materializeOutputDir(ctx context.Context, store cas.Store, treeDigest *cas.Digest, host string) error {
	body, err := store.GetBlob(ctx, treeDigest)
	if err != nil {
		return err
	}
	tree := &repb.Tree{}
	if err := proto.Unmarshal(body, tree); err != nil {
		return fmt.Errorf("materialize: unmarshal tree at %s: %w", host, err)
	}

	dirByHash := map[string]*repb.Directory{}
	for _, c := range tree.Children {
		body, err := cas.MarshalDeterministic(c)
		if err != nil {
			return fmt.Errorf("materialize: marshal child: %w", err)
		}
		d := cas.DigestOf(body)
		dirByHash[d.Hash] = c
	}

	if err := os.MkdirAll(host, 0o755); err != nil {
		return err
	}
	return walkDirectory(ctx, store, tree.Root, dirByHash, host)
}

func walkDirectory(ctx context.Context, store cas.Store, dir *repb.Directory, dirByHash map[string]*repb.Directory, host string) error {
	for _, f := range dir.Files {
		body, err := store.GetBlob(ctx, f.Digest)
		if err != nil {
			return err
		}
		mode := os.FileMode(0o644)
		if f.IsExecutable {
			mode = 0o755
		}
		path := filepath.Join(host, f.Name)
		if err := os.WriteFile(path, body, mode); err != nil {
			return fmt.Errorf("materialize: write %s: %w", path, err)
		}
	}
	for _, sub := range dir.Directories {
		child, ok := dirByHash[sub.Digest.Hash]
		if !ok {
			return fmt.Errorf("materialize: directory %s/%s digest %s not in tree.children",
				host, sub.Name, cas.DigestString(sub.Digest))
		}
		next := filepath.Join(host, sub.Name)
		if err := os.MkdirAll(next, 0o755); err != nil {
			return err
		}
		if err := walkDirectory(ctx, store, child, dirByHash, next); err != nil {
			return err
		}
	}
	for _, sl := range dir.Symlinks {
		path := filepath.Join(host, sl.Name)
		if err := os.Symlink(sl.Target, path); err != nil {
			return fmt.Errorf("materialize: symlink %s -> %s: %w", path, sl.Target, err)
		}
	}
	return nil
}
