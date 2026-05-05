//go:build linux

package casfuse

// FUSE adapter — kernel-facing layer that turns the platform-
// agnostic Tree into a mounted filesystem via go-fuse.
//
// Implements the minimum subset of fs.NodeXxx interfaces the
// kernel exercises during typical Bazel repo-rule consumption
// (Lookup, Readdir, Read, Getattr, Readlink). Writes are not
// supported — sources are immutable per design.

import (
	"context"
	"sync"
	"syscall"
	"time"

	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

// MountOptions configure Mount.
type MountOptions struct {
	// AllowOther — if true, processes other than the daemon's
	// owning UID can read the mount. Required when bazel runs
	// as a different user than the daemon. Default: false.
	AllowOther bool
	// ReadOnly — always true today; struct field reserved for
	// future per-mount tuning.
}

// Mount attaches a single Tree at mountPoint and returns the
// running server. The kernel sees the tree's root Directory at
// the mount point. Used by tests; production uses MountRoot
// for the multi-digest namespacing.
func Mount(tree *Tree, mountPoint string, opts MountOptions) (*fuse.Server, error) {
	root := &dirNode{tree: tree}
	server, err := fs.Mount(mountPoint, root, &fs.Options{
		MountOptions: fuse.MountOptions{
			Name:       "cas-fuse",
			FsName:     "cas-fuse",
			AllowOther: opts.AllowOther,
		},
		// Mount contents are content-addressed (digest-keyed), so
		// they don't change underneath the kernel — both directory
		// entries and inode attributes can cache for a long time.
		// Without this, GNU cp's "did this file change while I was
		// copying it?" race check can fire under heavy concurrent
		// access (the kernel re-stats through FUSE between cp's
		// open and its post-read stat; subtle re-evaluation in
		// the FUSE server can produce attributes that look like a
		// post-edit change). The bb_clientd convention is 5 min
		// (300s); we mirror that.
		EntryTimeout: durationPtr(300 * time.Second),
		AttrTimeout:  durationPtr(300 * time.Second),
	})
	if err != nil {
		return nil, err
	}
	return server, nil
}

// durationPtr is a small helper so the Options literal stays
// concise — go-fuse takes EntryTimeout / AttrTimeout as
// *time.Duration so a nil means "kernel default", and we want a
// concrete-finite value here.
func durationPtr(d time.Duration) *time.Duration { return &d }

// MountRoot attaches a Root at mountPoint and returns the
// running server. The kernel sees the bb_clientd-style virtual
// hierarchy:
//
//	<mount>/[<instance>/]blobs/directory/<hash>-<size>/...
//
// where <instance> is omitted for the empty-instance default and
// each <hash>-<size> path component lazily resolves to a Tree
// rooted at that CAS Directory. This is what production daemons
// run: one mount serves every per-source repo a Bazel build
// references.
func MountRoot(root *Root, mountPoint string, opts MountOptions) (*fuse.Server, error) {
	rootNode := &rootNode{root: root}
	server, err := fs.Mount(mountPoint, rootNode, &fs.Options{
		MountOptions: fuse.MountOptions{
			Name:       "cas-fuse",
			FsName:     "cas-fuse",
			AllowOther: opts.AllowOther,
		},
		// See Mount() for why we cache attributes long; same
		// reasoning applies — mount contents are content-addressed
		// and don't change underneath the kernel.
		EntryTimeout: durationPtr(300 * time.Second),
		AttrTimeout:  durationPtr(300 * time.Second),
	})
	if err != nil {
		return nil, err
	}
	return server, nil
}

// dirNode is a virtual directory node backed by a CAS Directory
// digest reachable through Tree. The root dirNode addresses the
// tree's root; subdir lookups follow the same pattern.
type dirNode struct {
	fs.Inode
	tree *Tree
	// dir is the underlying Directory proto. For the root, this
	// is fetched on first Lookup; for subdirs, it's set at
	// construction time from the parent's lookup result
	// (loaded = true).
	dir *repb.Directory

	mu     sync.Mutex
	loaded bool // set only on successful ensureDir; allows retry on transient failure
}

var _ fs.NodeLookuper = (*dirNode)(nil)
var _ fs.NodeReaddirer = (*dirNode)(nil)
var _ fs.NodeGetattrer = (*dirNode)(nil)

func (n *dirNode) ensureDir(ctx context.Context) syscall.Errno {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.loaded {
		return 0
	}
	dir, err := n.tree.Root(ctx)
	if err != nil {
		// Do NOT mark loaded=true on failure so the next Lookup
		// can retry. sync.Once would prevent that, causing the
		// directory to stay nil-forever and subsequent lookups to
		// silently return ENOENT.
		return syscall.EIO
	}
	n.dir = dir
	n.loaded = true
	return 0
}

func (n *dirNode) Getattr(_ context.Context, _ fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	out.Mode = fuse.S_IFDIR | 0o555
	return 0
}

func (n *dirNode) Lookup(ctx context.Context, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	if errno := n.ensureDir(ctx); errno != 0 {
		return nil, errno
	}
	if sub := findDir(n.dir, name); sub != nil {
		subDir, err := n.tree.fetchDir(ctx, Digest{Hash: sub.Digest.Hash, Size: sub.Digest.SizeBytes})
		if err != nil {
			return nil, syscall.EIO
		}
		child := &dirNode{tree: n.tree, dir: subDir, loaded: true}
		out.Mode = fuse.S_IFDIR | 0o555
		return n.NewInode(ctx, child, fs.StableAttr{Mode: fuse.S_IFDIR}), 0
	}
	if file := findFile(n.dir, name); file != nil {
		mode := uint32(0o444)
		if file.IsExecutable {
			mode = 0o555
		}
		child := &fileNode{tree: n.tree, fn: file}
		out.Mode = fuse.S_IFREG | mode
		out.Size = uint64(file.Digest.SizeBytes)
		return n.NewInode(ctx, child, fs.StableAttr{Mode: fuse.S_IFREG}), 0
	}
	if link := findSymlink(n.dir, name); link != nil {
		child := &symlinkNode{target: link.Target}
		out.Mode = fuse.S_IFLNK | 0o777
		return n.NewInode(ctx, child, fs.StableAttr{Mode: fuse.S_IFLNK}), 0
	}
	return nil, syscall.ENOENT
}

func (n *dirNode) Readdir(ctx context.Context) (fs.DirStream, syscall.Errno) {
	if errno := n.ensureDir(ctx); errno != 0 {
		return nil, errno
	}
	entries := make([]fuse.DirEntry, 0, len(n.dir.Files)+len(n.dir.Directories)+len(n.dir.Symlinks))
	for _, d := range n.dir.Directories {
		entries = append(entries, fuse.DirEntry{Mode: fuse.S_IFDIR, Name: d.Name})
	}
	for _, f := range n.dir.Files {
		entries = append(entries, fuse.DirEntry{Mode: fuse.S_IFREG, Name: f.Name})
	}
	for _, s := range n.dir.Symlinks {
		entries = append(entries, fuse.DirEntry{Mode: fuse.S_IFLNK, Name: s.Name})
	}
	return fs.NewListDirStream(entries), 0
}

// fileNode is a virtual file node backed by a CAS blob digest.
// Reads pull bytes from CAS on demand — the kernel page cache
// elides repeats within a short window.
type fileNode struct {
	fs.Inode
	tree *Tree
	fn   *repb.FileNode
}

var _ fs.NodeReader = (*fileNode)(nil)
var _ fs.NodeGetattrer = (*fileNode)(nil)
var _ fs.NodeOpener = (*fileNode)(nil)
var _ fs.NodeGetxattrer = (*fileNode)(nil)
var _ fs.NodeListxattrer = (*fileNode)(nil)

func (n *fileNode) Getattr(_ context.Context, _ fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	mode := uint32(0o444)
	if n.fn.IsExecutable {
		mode = 0o555
	}
	out.Mode = fuse.S_IFREG | mode
	out.Size = uint64(n.fn.Digest.SizeBytes)
	return 0
}

func (n *fileNode) Open(_ context.Context, flags uint32) (fs.FileHandle, uint32, syscall.Errno) {
	// Read-only mount; reject anything but read access.
	if flags&(syscall.O_WRONLY|syscall.O_RDWR) != 0 {
		return nil, 0, syscall.EROFS
	}
	return nil, fuse.FOPEN_KEEP_CACHE, 0
}

func (n *fileNode) Read(ctx context.Context, _ fs.FileHandle, dest []byte, off int64) (fuse.ReadResult, syscall.Errno) {
	body, err := n.tree.ReadFile(ctx, n.fn)
	if err != nil {
		return nil, syscall.EIO
	}
	if off >= int64(len(body)) {
		return fuse.ReadResultData(nil), 0
	}
	end := off + int64(len(dest))
	if end > int64(len(body)) {
		end = int64(len(body))
	}
	return fuse.ReadResultData(body[off:end]), 0
}

// XattrDigestName is the xattr key the file nodes serve with the
// pre-computed CAS digest of each file's bytes (hex of SHA-256).
// Bazel reads this when invoked with
//
//	--unix_digest_hash_attribute_name=user.bazel.cas.digest \
//	--digest_function=SHA256
//
// which lets it skip re-digesting input files served through a
// FUSE mount — meaning bytes don't have to traverse dev's RAM
// just to compute a digest the daemon already knows. Without this,
// BwoB still works but the dev machine reads every input once for
// hashing.
const XattrDigestName = "user.bazel.cas.digest"

// Getxattr serves XattrDigestName with the file's CAS hash. Other
// names return ENODATA so Bazel cleanly falls back to its own
// digesting for unrelated xattr requests.
func (n *fileNode) Getxattr(_ context.Context, attr string, dest []byte) (uint32, syscall.Errno) {
	if attr != XattrDigestName {
		return 0, syscall.ENODATA
	}
	val := []byte(n.fn.Digest.Hash)
	if len(dest) == 0 {
		// "Probe" call: kernel passes a zero-length buffer to ask
		// for the size. Return the length without copying.
		return uint32(len(val)), 0
	}
	if len(dest) < len(val) {
		return uint32(len(val)), syscall.ERANGE
	}
	return uint32(copy(dest, val)), 0
}

// Listxattr advertises the digest xattr so `getfattr -d` style
// listings find it. Bazel itself only Getxattr's the configured
// name, but having Listxattr return the same name keeps tooling
// honest.
func (n *fileNode) Listxattr(_ context.Context, dest []byte) (uint32, syscall.Errno) {
	// Format is NUL-terminated names concatenated.
	val := []byte(XattrDigestName + "\x00")
	if len(dest) == 0 {
		return uint32(len(val)), 0
	}
	if len(dest) < len(val) {
		return uint32(len(val)), syscall.ERANGE
	}
	return uint32(copy(dest, val)), 0
}

// symlinkNode is a virtual symlink. The CAS Directory's
// SymlinkNode carries the target string verbatim; we just hand
// it back on Readlink.
type symlinkNode struct {
	fs.Inode
	target string
}

var _ fs.NodeReadlinker = (*symlinkNode)(nil)
var _ fs.NodeGetattrer = (*symlinkNode)(nil)

func (n *symlinkNode) Getattr(_ context.Context, _ fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	out.Mode = fuse.S_IFLNK | 0o777
	out.Size = uint64(len(n.target))
	return 0
}

func (n *symlinkNode) Readlink(_ context.Context) ([]byte, syscall.Errno) {
	return []byte(n.target), 0
}
