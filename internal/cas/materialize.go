package cas

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"google.golang.org/protobuf/proto"
)

// validSegment rejects names that would let a malformed Directory escape
// the materialization root: empty, "."/"..", or anything containing a
// path separator. REAPI defines FileNode.name / SymlinkNode.name /
// DirectoryNode.name as a single path segment; this enforces that
// instead of trusting upstream.
func validSegment(name string) bool {
	if name == "" || name == "." || name == ".." {
		return false
	}
	if strings.ContainsRune(name, '/') || strings.ContainsRune(name, filepath.Separator) {
		return false
	}
	return true
}

// symlinkTargetEscapes reports whether sl.Target would resolve outside
// linkDir (the directory the symlink lives in). Absolute targets and
// targets that climb above linkDir via ".." both fail. CAS Directory
// trees may legitimately point at sibling files inside the same root,
// so this is a containment check, not a flat reject.
func symlinkTargetEscapes(linkDir, target, root string) bool {
	if filepath.IsAbs(target) {
		return true
	}
	resolved := filepath.Clean(filepath.Join(linkDir, target))
	rel, err := filepath.Rel(root, resolved)
	if err != nil {
		return true
	}
	return rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// MaterializeDirectory walks the Directory tree rooted at d (in CAS)
// and writes every entry to dst on the local filesystem. The directory
// is created (and its parents) if absent; existing entries at the same
// paths are overwritten.
//
// Used by M3d's source-CAS resolver to drop a Buildbarn-resident
// source tree into a per-element checkout dir, and by anything else
// that needs to project a CAS Directory back to disk for a tool that
// reads files (cmake, the converter, ...).
//
// Returns ErrMissingBlob (wrapping ErrNotFound) when a referenced
// blob is absent from the store, so callers can distinguish "stale
// digest" from real I/O failures.
func MaterializeDirectory(ctx context.Context, store Store, d *Digest, dst string) error {
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return fmt.Errorf("materialize: mkdir %s: %w", dst, err)
	}
	root, err := filepath.Abs(dst)
	if err != nil {
		return fmt.Errorf("materialize: abs %s: %w", dst, err)
	}
	return materializeDirRecurse(ctx, store, d, root, root)
}

// writeFile writes a materialized file to disk. For executable files it
// holds syscall.ForkLock for read while the FD is open so no concurrent
// goroutine's fork+exec can inherit the in-flight write FD: a child that
// inherits a write FD to the same inode (post-rename) would later
// trigger ETXTBSY when its sibling tries to exec the materialized
// binary. Non-executable files skip the lock — only exec'd binaries
// matter for ETXTBSY.
func writeFile(path string, body []byte, mode os.FileMode, isExecutable bool) error {
	if !isExecutable {
		return os.WriteFile(path, body, mode)
	}
	syscall.ForkLock.RLock()
	defer syscall.ForkLock.RUnlock()
	return os.WriteFile(path, body, mode)
}

// ErrMissingBlob wraps ErrNotFound with the digest of the missing
// blob so callers can report a precise error.
type ErrMissingBlob struct {
	Digest *Digest
	Err    error
}

func (e *ErrMissingBlob) Error() string {
	return fmt.Sprintf("missing blob %s: %v", DigestString(e.Digest), e.Err)
}
func (e *ErrMissingBlob) Unwrap() error { return e.Err }

func materializeDirRecurse(ctx context.Context, store Store, d *Digest, dst, root string) error {
	body, err := store.GetBlob(ctx, d)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return &ErrMissingBlob{Digest: d, Err: err}
		}
		return err
	}
	dir := &repb.Directory{}
	if err := proto.Unmarshal(body, dir); err != nil {
		return fmt.Errorf("materialize: unmarshal directory %s: %w", DigestString(d), err)
	}

	for _, f := range dir.Files {
		if !validSegment(f.Name) {
			return fmt.Errorf("materialize: rejecting file name %q (must be a single path segment)", f.Name)
		}
		body, err := store.GetBlob(ctx, f.Digest)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				return &ErrMissingBlob{Digest: f.Digest, Err: err}
			}
			return err
		}
		mode := os.FileMode(0o644)
		if f.IsExecutable {
			mode = 0o755
		}
		if err := writeFile(filepath.Join(dst, f.Name), body, mode, f.IsExecutable); err != nil {
			return fmt.Errorf("materialize: write %s: %w", filepath.Join(dst, f.Name), err)
		}
	}
	for _, sl := range dir.Symlinks {
		if !validSegment(sl.Name) {
			return fmt.Errorf("materialize: rejecting symlink name %q (must be a single path segment)", sl.Name)
		}
		if symlinkTargetEscapes(dst, sl.Target, root) {
			return fmt.Errorf("materialize: rejecting symlink %q -> %q (target escapes root)", sl.Name, sl.Target)
		}
		path := filepath.Join(dst, sl.Name)
		if err := os.Symlink(sl.Target, path); err != nil {
			return fmt.Errorf("materialize: symlink %s -> %s: %w", path, sl.Target, err)
		}
	}
	for _, sub := range dir.Directories {
		if !validSegment(sub.Name) {
			return fmt.Errorf("materialize: rejecting directory name %q (must be a single path segment)", sub.Name)
		}
		next := filepath.Join(dst, sub.Name)
		if err := os.MkdirAll(next, 0o755); err != nil {
			return fmt.Errorf("materialize: mkdir %s: %w", next, err)
		}
		if err := materializeDirRecurse(ctx, store, sub.Digest, next, root); err != nil {
			return err
		}
	}
	return nil
}
