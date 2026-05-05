//go:build linux

package casfuse

// FUSE adapter nodes that lazily resolve the
// bb_clientd-style multi-digest virtual hierarchy:
//
//   /<mount>/[<instance>/]blobs/directory/<hash>-<size>/...
//
// Implemented as four chained directory shells:
//
//   rootNode → instanceNode (per REAPI instance, including "")
//             ↳ blobsNode    (the literal "blobs" segment)
//             ↳ directoryNode (the literal "directory" segment)
//             ↳ digestNode   (the "<hash>-<size>" segment;
//                             resolves to a Tree rooted at that
//                             digest, then dirNode handles the
//                             rest of the walk)
//
// Each shell-level Lookup is a constant-time string compare
// against the next expected segment; only the digestNode does
// CAS work (fetching the named Directory). Names that don't
// match the expected layout return ENOENT, matching what a
// curious shell `ls` would expect.

import (
	"context"
	"syscall"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

// rootNode is the top of the multi-digest virtual hierarchy.
// Its first lookup segment can be either "blobs" (default
// instance) or any other string interpreted as a REAPI instance
// name. The kernel doesn't pre-Readdir the root in normal
// usage, so listing returns the well-known "blobs" entry.
type rootNode struct {
	fs.Inode
	root *Root
}

var _ fs.NodeLookuper = (*rootNode)(nil)
var _ fs.NodeReaddirer = (*rootNode)(nil)
var _ fs.NodeGetattrer = (*rootNode)(nil)

func (n *rootNode) Getattr(_ context.Context, _ fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	out.Mode = fuse.S_IFDIR | 0o555
	return 0
}

func (n *rootNode) Lookup(ctx context.Context, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	out.Mode = fuse.S_IFDIR | 0o555
	if name == "blobs" {
		child := &blobsNode{root: n.root, instance: ""}
		return n.NewInode(ctx, child, fs.StableAttr{Mode: fuse.S_IFDIR}), 0
	}
	// Any other segment is a REAPI instance name. We can't
	// validate it without a CAS round-trip; eagerly accept and
	// let downstream lookups error if the instance doesn't exist.
	child := &instanceNode{root: n.root, instance: name}
	return n.NewInode(ctx, child, fs.StableAttr{Mode: fuse.S_IFDIR}), 0
}

func (n *rootNode) Readdir(_ context.Context) (fs.DirStream, syscall.Errno) {
	return fs.NewListDirStream([]fuse.DirEntry{
		{Mode: fuse.S_IFDIR, Name: "blobs"},
	}), 0
}

// instanceNode wraps a REAPI instance namespace; only "blobs"
// is a valid child.
type instanceNode struct {
	fs.Inode
	root     *Root
	instance string
}

var _ fs.NodeLookuper = (*instanceNode)(nil)
var _ fs.NodeGetattrer = (*instanceNode)(nil)

func (n *instanceNode) Getattr(_ context.Context, _ fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	out.Mode = fuse.S_IFDIR | 0o555
	return 0
}

func (n *instanceNode) Lookup(ctx context.Context, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	if name != "blobs" {
		return nil, syscall.ENOENT
	}
	out.Mode = fuse.S_IFDIR | 0o555
	child := &blobsNode{root: n.root, instance: n.instance}
	return n.NewInode(ctx, child, fs.StableAttr{Mode: fuse.S_IFDIR}), 0
}

// blobsNode wraps the "blobs" literal; only "directory" is
// implemented today (file blobs aren't a typical lookup target
// for the FUSE consumer; PushBlob / pre-fetched files come in
// via the directoryNode walk).
type blobsNode struct {
	fs.Inode
	root     *Root
	instance string
}

var _ fs.NodeLookuper = (*blobsNode)(nil)
var _ fs.NodeGetattrer = (*blobsNode)(nil)

func (n *blobsNode) Getattr(_ context.Context, _ fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	out.Mode = fuse.S_IFDIR | 0o555
	return 0
}

func (n *blobsNode) Lookup(ctx context.Context, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	if name != "directory" {
		return nil, syscall.ENOENT
	}
	out.Mode = fuse.S_IFDIR | 0o555
	child := &directoryNode{root: n.root, instance: n.instance}
	return n.NewInode(ctx, child, fs.StableAttr{Mode: fuse.S_IFDIR}), 0
}

// directoryNode wraps the "directory" literal; lookups on it
// are interpreted as "<hash>-<size>" digest references and
// resolved into a dirNode for the named CAS Directory.
type directoryNode struct {
	fs.Inode
	root     *Root
	instance string
}

var _ fs.NodeLookuper = (*directoryNode)(nil)
var _ fs.NodeGetattrer = (*directoryNode)(nil)

func (n *directoryNode) Getattr(_ context.Context, _ fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	out.Mode = fuse.S_IFDIR | 0o555
	return 0
}

func (n *directoryNode) Lookup(ctx context.Context, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	d, err := ParseDigest(name)
	if err != nil {
		return nil, syscall.ENOENT
	}
	tree := n.root.TreeFor(n.instance, d)
	dir, err := tree.Root(ctx)
	if err != nil {
		// A bad digest (CAS NotFound) surfaces as I/O error,
		// matching how the kernel reports unreachable storage.
		return nil, syscall.EIO
	}
	child := &dirNode{tree: tree, dir: dir, loaded: true}
	out.Mode = fuse.S_IFDIR | 0o555
	return n.NewInode(ctx, child, fs.StableAttr{Mode: fuse.S_IFDIR}), 0
}
