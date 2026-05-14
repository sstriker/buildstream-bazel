package main

// Source-cache lookup for non-kind:local sources.
//
// write-a stays "small and dumb" — it doesn't fetch from the
// network. But real bazel-build of project B needs actual source
// bytes for elements with kind:git_repo / kind:tar / etc., not
// just metadata records on resolvedSource. The --source-cache
// flag closes the gap: callers pre-populate
// <cache>/<source-key>/ trees (via the `internal/sourcecheckout`
// layer or by hand for tests), and write-a's loader treats those
// entries as if they were kind:local sources at staging time.
//
// sourceKey is the deterministic content key write-a derives per
// non-kind:local source — SHA-256 of the source's canonical
// identity (kind, url, ref). Identical sources across elements
// share the same key (and the same cache directory), so a single
// fetched tree backs every reference to it.

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// sourceKey returns the cache-directory name a fetched source
// tree is expected to live under. For kind:local sources (which
// don't need fetching) returns the empty string.
//
// The key composes the source kind + url + ref string form. For
// scalar refs (kind:git_repo / kind:tar) the ref's Value is
// canonical. For non-scalar refs (kind:cargo2 / kind:go_module
// vendored ref lists) the YAML-encoded node is canonical — same
// shape input produces the same key. Track is intentionally not
// part of the key: BuildStream uses it as a fetch-time hint, not
// part of the source identity.
func sourceKey(rs resolvedSource) string {
	if rs.Kind == "" || rs.Kind == "local" {
		return ""
	}
	h := sha256.New()
	h.Write([]byte(rs.Kind))
	h.Write([]byte{0})
	h.Write([]byte(rs.URL))
	h.Write([]byte{0})
	switch rs.Ref.Kind {
	case 0:
		// Zero-valued node — no ref declared.
	case yaml.ScalarNode:
		h.Write([]byte(rs.Ref.Value))
	default:
		// Non-scalar (vendored ref lists). Marshal back to YAML
		// for a stable canonical form. Errors are unlikely; on
		// error we still want a well-defined key so we fall
		// through to the empty marshaling.
		body, err := yaml.Marshal(&rs.Ref)
		if err == nil {
			h.Write(body)
		}
	}
	return hex.EncodeToString(h.Sum(nil))
}

// resolveFromCache populates rs.AbsPath from the source cache when
// the source-key matches an existing directory. Returns true when
// a cache hit was found (AbsPath populated) so the staging step
// treats this entry as kind:local-equivalent. Empty cacheDir or
// kind:local sources return false (caller takes the existing path).
func resolveFromCache(cacheDir string, rs *resolvedSource) bool {
	if cacheDir == "" {
		return false
	}
	key := sourceKey(*rs)
	if key == "" {
		return false
	}
	candidate := filepath.Join(cacheDir, key)
	info, err := os.Stat(candidate)
	if err != nil || !info.IsDir() {
		return false
	}
	rs.AbsPath = candidate
	return true
}
