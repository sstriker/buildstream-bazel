package cas

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"

	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
)

// Tree is the in-memory result of packing a local directory into REAPI
// Directory protos. It carries enough information for a caller to
// upload every blob (file content + Directory proto) to CAS without
// re-walking the source tree.
//
// All Digests inside Tree refer to either:
//   - File contents: look up Files[digest.Hash] for the local path.
//   - Directory protos: look up Directories[digest.Hash] for the proto;
//     marshal with MarshalDeterministic to get the bytes whose sha256
//     matches digest.Hash.
type Tree struct {
	// Root is the top-level Directory of the packed tree.
	Root *repb.Directory
	// RootDigest is the digest of MarshalDeterministic(Root).
	RootDigest *Digest

	// Directories contains every Directory proto in the tree, including
	// Root, keyed by its digest hash.
	Directories map[string]*repb.Directory

	// Files maps every file digest hash to the local filesystem path
	// from which the blob can be re-read for upload.
	Files map[string]string
}

// PackDir walks a local directory and produces a Tree.
//
// Within each Directory the children are sorted alphabetically by name,
// matching REAPI's canonical encoding. Empty directories produce empty
// Directory protos with the same EmptyDigest()-style digest of their
// serialized empty form (which is itself stable).
//
// Symlinks are recorded as SymlinkNode entries with the link target
// stored verbatim. Non-regular, non-dir, non-symlink entries (sockets,
// devices, fifos) are skipped with a returned error — those have no
// REAPI representation.
func PackDir(root string) (*Tree, error) {
	info, err := os.Stat(root)
	if err != nil {
		return nil, fmt.Errorf("packdir stat %s: %w", root, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("packdir: %s is not a directory", root)
	}

	t := &Tree{
		Directories: make(map[string]*repb.Directory),
		Files:       make(map[string]string),
	}

	rootDir, rootDigest, err := t.packSubtree(root)
	if err != nil {
		return nil, err
	}
	t.Root = rootDir
	t.RootDigest = rootDigest
	return t, nil
}

// packSubtree builds the Directory proto for absPath, recursing into
// children. Returns the Directory proto and its digest.
func (t *Tree) packSubtree(absPath string) (*repb.Directory, *Digest, error) {
	entries, err := os.ReadDir(absPath)
	if err != nil {
		return nil, nil, fmt.Errorf("readdir %s: %w", absPath, err)
	}

	dir := &repb.Directory{}

	for _, e := range entries {
		name := e.Name()
		child := filepath.Join(absPath, name)

		info, err := e.Info()
		if err != nil {
			return nil, nil, fmt.Errorf("stat %s: %w", child, err)
		}
		mode := info.Mode()
		switch {
		case mode.IsRegular():
			d, err := DigestFile(child)
			if err != nil {
				return nil, nil, err
			}
			t.Files[d.Hash] = child
			dir.Files = append(dir.Files, &repb.FileNode{
				Name:         name,
				Digest:       d,
				IsExecutable: mode&0o111 != 0,
			})
		case mode.IsDir():
			_, subDigest, err := t.packSubtree(child)
			if err != nil {
				return nil, nil, err
			}
			dir.Directories = append(dir.Directories, &repb.DirectoryNode{
				Name:   name,
				Digest: subDigest,
			})
		case mode&fs.ModeSymlink != 0:
			target, err := os.Readlink(child)
			if err != nil {
				return nil, nil, fmt.Errorf("readlink %s: %w", child, err)
			}
			dir.Symlinks = append(dir.Symlinks, &repb.SymlinkNode{
				Name:   name,
				Target: target,
			})
		default:
			return nil, nil, fmt.Errorf("packdir: %s has unsupported mode %s", child, mode)
		}
	}

	SortDirectory(dir)

	digest, _, err := DigestProto(dir)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal directory %s: %w", absPath, err)
	}
	t.Directories[digest.Hash] = dir
	return dir, digest, nil
}

// SortDirectory sorts each child slice by name. REAPI requires this for
// canonical encoding; callers building Directory protos by hand (e.g.
// composing an input root from several subtrees) MUST call this before
// digesting.
func SortDirectory(d *repb.Directory) {
	sort.Slice(d.Files, func(i, j int) bool { return d.Files[i].Name < d.Files[j].Name })
	sort.Slice(d.Directories, func(i, j int) bool { return d.Directories[i].Name < d.Directories[j].Name })
	sort.Slice(d.Symlinks, func(i, j int) bool { return d.Symlinks[i].Name < d.Symlinks[j].Name })
}

// AsReapiTree returns a *repb.Tree containing Root and every other
// Directory in the tree as `children`. The Tree message is what's
// referenced from an ActionResult.OutputDirectory; uploading the Tree
// proto to CAS gives a downstream consumer everything it needs to walk
// the subtree without further round trips.
//
// The children list is sorted by digest hash so two equivalent Trees
// produce byte-identical Tree blobs (and therefore identical digests).
func (t *Tree) AsReapiTree() *repb.Tree {
	tree := &repb.Tree{Root: t.Root}
	hashes := make([]string, 0, len(t.Directories))
	for h := range t.Directories {
		if h == t.RootDigest.Hash {
			continue
		}
		hashes = append(hashes, h)
	}
	sort.Strings(hashes)
	for _, h := range hashes {
		tree.Children = append(tree.Children, t.Directories[h])
	}
	return tree
}
