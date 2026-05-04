// Package srckeyregistry persists trace + make-db artifacts
// keyed by per-element srckey. Used by the trace-driven
// autotools translator (see docs/trace-driven-autotools.md
// "Roadmap" → 2-phase srckey design):
//
//   - **Round 1** (cache miss): write-a renders the standard
//     install genrule; bazel build runs it, producing trace +
//     make-db + install_tree.tar + BUILD.bazel.out. After the
//     build, a wrapper registers (srckey, trace, make-db) into
//     this store.
//   - **Round 2** (cache hit): write-a's render checks the
//     store for the element's srckey. If hit, project A's BUILD
//     becomes a converter-only genrule whose srcs are the
//     registered trace + make-db (no expensive autotools build).
//
// The implementation is intentionally small and filesystem-
// backed: one directory per srckey, one file per artifact name.
// Production deployments can swap the storage backend (CAS via
// REAPI / buildbarn) without changing the contract: callers see
// (srckey, name) -> bytes lookups, register pushes (srckey,
// name, bytes).
//
// Atomicity: Register writes through a tempfile + rename to
// ensure a Lookup never sees a half-written artifact. The
// per-srckey directory is created lazily on first Register;
// concurrent registers of the same (srckey, name) tolerate
// the last-writer-wins outcome (deterministic upstream input
// + deterministic build means same bytes, so the race is
// benign).
package srckeyregistry

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// FS is a filesystem-backed registry. All artifacts live under
// dir/<srckey>/<name>. Callers create one Registry per
// physical location (per-machine cache directory, per-CI shared
// volume, etc.).
type FS struct {
	dir string
}

// New constructs a Registry rooted at dir. Creates the directory
// if missing. Returns an error if dir exists but isn't a
// directory.
func New(dir string) (*FS, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create registry dir %s: %w", dir, err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("registry path %s exists but is not a directory", dir)
	}
	return &FS{dir: dir}, nil
}

// Register stores `content` under (srckey, name). Atomic:
// writes to a tempfile in the same directory then renames into
// place. Subsequent Lookup calls see the new bytes; in-flight
// Lookups see either the previous content or the new — never
// truncated bytes.
func (r *FS) Register(srckey, name string, content []byte) error {
	if err := validateKey(srckey); err != nil {
		return fmt.Errorf("register: %w", err)
	}
	if err := validateName(name); err != nil {
		return fmt.Errorf("register: %w", err)
	}
	keyDir := filepath.Join(r.dir, srckey)
	if err := os.MkdirAll(keyDir, 0o755); err != nil {
		return fmt.Errorf("register: create %s: %w", keyDir, err)
	}
	tmp, err := os.CreateTemp(keyDir, name+".tmp-*")
	if err != nil {
		return fmt.Errorf("register: tempfile in %s: %w", keyDir, err)
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(content); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("register: write %s: %w", tmpPath, err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("register: close %s: %w", tmpPath, err)
	}
	final := filepath.Join(keyDir, name)
	if err := os.Rename(tmpPath, final); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("register: rename %s -> %s: %w", tmpPath, final, err)
	}
	return nil
}

// Lookup returns the artifact bytes for (srckey, name). The ok
// flag distinguishes "not registered" from "I/O error": false
// + nil error means the registry has no entry; false + non-nil
// means the lookup itself failed.
func (r *FS) Lookup(srckey, name string) (content []byte, ok bool, err error) {
	if err := validateKey(srckey); err != nil {
		return nil, false, fmt.Errorf("lookup: %w", err)
	}
	if err := validateName(name); err != nil {
		return nil, false, fmt.Errorf("lookup: %w", err)
	}
	path := filepath.Join(r.dir, srckey, name)
	body, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("lookup: read %s: %w", path, err)
	}
	return body, true, nil
}

// Has reports whether (srckey, name) is registered. Cheaper than
// Lookup when the caller just wants to know presence (e.g.,
// write-a deciding which render shape to emit).
func (r *FS) Has(srckey, name string) (bool, error) {
	if err := validateKey(srckey); err != nil {
		return false, fmt.Errorf("has: %w", err)
	}
	if err := validateName(name); err != nil {
		return false, fmt.Errorf("has: %w", err)
	}
	_, err := os.Stat(filepath.Join(r.dir, srckey, name))
	if err == nil {
		return true, nil
	}
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return false, fmt.Errorf("has: stat: %w", err)
}

// Dir returns the registry's root directory. Useful for tools
// that want to log or share the location.
func (r *FS) Dir() string { return r.dir }

// validateKey rejects srckeys that would let the storage path
// escape the registry directory (`..` / leading slash) or hit
// filesystem case-collision oddities. Per-element srckey is
// already a 64-char hex sha256 — the validator just enforces
// that shape so unrelated identifiers can't be passed in.
func validateKey(srckey string) error {
	if srckey == "" {
		return fmt.Errorf("srckey is empty")
	}
	if len(srckey) > 256 {
		return fmt.Errorf("srckey too long (%d chars)", len(srckey))
	}
	for _, c := range srckey {
		switch {
		case c >= '0' && c <= '9':
		case c >= 'a' && c <= 'z':
		case c >= 'A' && c <= 'Z':
		case c == '-' || c == '_':
		default:
			return fmt.Errorf("srckey contains illegal character %q", c)
		}
	}
	return nil
}

// validateName rejects artifact names that contain path
// separators or start with a dot (avoids collisions with
// hidden files / parent-dir traversal).
func validateName(name string) error {
	if name == "" {
		return fmt.Errorf("artifact name is empty")
	}
	if strings.ContainsAny(name, `/\`) {
		return fmt.Errorf("artifact name %q contains path separator", name)
	}
	if strings.HasPrefix(name, ".") {
		return fmt.Errorf("artifact name %q must not start with dot", name)
	}
	return nil
}
