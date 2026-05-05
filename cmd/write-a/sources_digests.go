package main

// Compute CAS Directory digests for graph sources by packing
// the on-disk source-cache trees (the same trees --source-cache
// already resolves at staging time). The packed digest lands in
// each sourceEntry's Digest field, so the rendered sources.json
// carries real CAS coordinates the FUSE-symlink repo rule
// (rules/sources.bzl) can address.
//
// Population strategy is intentionally write-a-side rather than
// inside the module extension: write-a already has the
// --source-cache tree and an absolute path per source identity.
// The extension stays small (read JSON, declare repos), and CAS
// upload (out-of-band step) remains separate — `bst source push`
// in production, `cmd/source-push` for tests / dev.

import (
	"fmt"

	"github.com/sstriker/cmake-to-bazel/internal/cas"
)

// populateDigests packs the on-disk source-cache tree for each
// sourceEntry that has one and stamps the resulting Directory
// digest into entry.Digest. Returns the entries with digests
// filled in for sources that resolved.
//
// Sources without a source-cache hit (AbsPath empty) get
// Digest left empty — the sources.json carries enough metadata
// to resolve them later, but the FUSE-symlink repo rule will
// fail at evaluation time if asked to resolve such a key.
// That's the right behaviour: it surfaces "I forgot to populate
// the cache for source X" instead of silently producing a broken
// build.
func populateDigests(g *graph, entries []sourceEntry) ([]sourceEntry, error) {
	// Index resolvedSource by sourceKey so we can find AbsPath
	// per entry without re-walking the graph.
	keyToPath := map[string]string{}
	for _, elem := range g.Elements {
		for _, rs := range elem.Sources {
			k := sourceKey(rs)
			if k == "" {
				continue
			}
			if _, dup := keyToPath[k]; dup {
				continue
			}
			keyToPath[k] = rs.AbsPath
		}
	}

	out := make([]sourceEntry, len(entries))
	for i, e := range entries {
		out[i] = e
		path, ok := keyToPath[e.Key]
		if !ok || path == "" {
			continue
		}
		tree, err := cas.PackDir(path)
		if err != nil {
			return nil, fmt.Errorf("pack source %s (%s): %w", e.Key, path, err)
		}
		// `<hash>-<size>` is the path-component format
		// rules/sources.bzl uses to build the symlink target
		// (`<mount>/<prefix>/directory/<digest>`); cas.DigestString
		// returns `<hash>/<size>` (slash) for log lines / browser
		// URLs, which is the wrong shape here.
		out[i].Digest = fmt.Sprintf("%s-%d", tree.RootDigest.Hash, tree.RootDigest.SizeBytes)
	}
	return out, nil
}
