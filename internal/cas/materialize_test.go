package cas

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
)

func putBlob(ctx context.Context, s *LocalStore, body []byte) (*Digest, error) {
	d := DigestOf(body)
	if err := s.PutBlob(ctx, d, body); err != nil {
		return nil, err
	}
	return d, nil
}

func putProto(ctx context.Context, s *LocalStore, m *repb.Directory) (*Digest, error) {
	d, body, err := DigestProto(m)
	if err != nil {
		return nil, err
	}
	if err := s.PutBlob(ctx, d, body); err != nil {
		return nil, err
	}
	return d, nil
}

// TestMaterializeDirectory_RejectsTraversalFileName covers the
// defense-in-depth case where a malformed Directory proto carries a
// FileNode.name that's not a single path segment. Per REAPI spec,
// names must be single segments; an unsanitized "../escape" would
// otherwise let the materializer write outside dst.
func TestMaterializeDirectory_RejectsTraversalFileName(t *testing.T) {
	store, err := NewLocalStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	bodyDigest, err := putBlob(ctx, store, []byte("payload"))
	if err != nil {
		t.Fatal(err)
	}
	dir := &repb.Directory{
		Files: []*repb.FileNode{{Name: "../escape", Digest: bodyDigest}},
	}
	dirDigest, err := putProto(ctx, store, dir)
	if err != nil {
		t.Fatal(err)
	}

	dst := t.TempDir()
	err = MaterializeDirectory(ctx, store, dirDigest, dst)
	if err == nil {
		t.Fatalf("MaterializeDirectory accepted traversal file name")
	}
	if !strings.Contains(err.Error(), "single path segment") {
		t.Errorf("expected single-segment rejection, got: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(filepath.Dir(dst), "escape")); statErr == nil {
		t.Errorf("file written outside dst — guard failed")
	}
}

// TestMaterializeDirectory_RejectsAbsoluteSymlinkTarget covers
// SymlinkNode.target containing an absolute path. Even though the
// link's name is constrained to dst, an absolute target lets a
// downstream tool (cmake / bazel) read or write outside the
// materialized tree.
func TestMaterializeDirectory_RejectsAbsoluteSymlinkTarget(t *testing.T) {
	store, err := NewLocalStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	dir := &repb.Directory{
		Symlinks: []*repb.SymlinkNode{{Name: "link", Target: "/etc/passwd"}},
	}
	dirDigest, err := putProto(ctx, store, dir)
	if err != nil {
		t.Fatal(err)
	}

	err = MaterializeDirectory(ctx, store, dirDigest, t.TempDir())
	if err == nil {
		t.Fatalf("MaterializeDirectory accepted absolute symlink target")
	}
	if !strings.Contains(err.Error(), "escapes root") {
		t.Errorf("expected target-escapes-root rejection, got: %v", err)
	}
}

// TestMaterializeDirectory_RejectsClimbingSymlinkTarget covers
// SymlinkNode.target that uses ".." to climb out of dst.
func TestMaterializeDirectory_RejectsClimbingSymlinkTarget(t *testing.T) {
	store, err := NewLocalStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	dir := &repb.Directory{
		Symlinks: []*repb.SymlinkNode{{Name: "link", Target: "../../outside"}},
	}
	dirDigest, err := putProto(ctx, store, dir)
	if err != nil {
		t.Fatal(err)
	}

	err = MaterializeDirectory(ctx, store, dirDigest, t.TempDir())
	if err == nil {
		t.Fatalf("MaterializeDirectory accepted climbing symlink target")
	}
	if !strings.Contains(err.Error(), "escapes root") {
		t.Errorf("expected target-escapes-root rejection, got: %v", err)
	}
}

// TestMaterializeDirectory_AllowsInternalSymlink confirms the guard
// doesn't reject legitimate within-tree symlinks.
func TestMaterializeDirectory_AllowsInternalSymlink(t *testing.T) {
	store, err := NewLocalStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	bodyDigest, err := putBlob(ctx, store, []byte("hi"))
	if err != nil {
		t.Fatal(err)
	}
	dir := &repb.Directory{
		Files:    []*repb.FileNode{{Name: "real", Digest: bodyDigest}},
		Symlinks: []*repb.SymlinkNode{{Name: "alias", Target: "real"}},
	}
	dirDigest, err := putProto(ctx, store, dir)
	if err != nil {
		t.Fatal(err)
	}

	dst := t.TempDir()
	if err := MaterializeDirectory(ctx, store, dirDigest, dst); err != nil {
		t.Fatalf("MaterializeDirectory rejected legitimate sibling symlink: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(dst, "alias")); err != nil {
		t.Errorf("symlink not created: %v", err)
	}
}
