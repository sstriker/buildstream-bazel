package main

// sources.json emission.
//
// Per docs/sources-design.md, project A and project B both load a
// shared module_extension that declares one external repo per
// non-kind:local source identity in the graph. The user-facing
// MODULE.bazel must enumerate those repo names statically (Bazel's
// use_repo() can't be dynamic), but the *backing data* for each
// declared repo — kind, url, ref, eventually CAS Directory digest —
// lives in tools/sources.json so MODULE.bazel itself stays small
// and stable as the underlying source metadata bumps.
//
// v1 (this PR): the extension declares stub repos with an empty
// :tree filegroup. Subsequent PRs replace the stub with a
// ctx.symlink into the cmd/cas-fuse mount point.

import (
	"encoding/json"

	"github.com/sstriker/buildstream-bazel/internal/sliceutil"
)

// sourceEntry is one record per unique source identity in the
// graph. Digest is left empty in v1 — the CAS Directory digest is
// populated once `bst source push` runs (PR #59) and write-a is
// taught to read the resulting key→digest map.
type sourceEntry struct {
	Key   string `json:"key"`
	Kind  string `json:"kind"`
	URL   string `json:"url"`
	Ref   string `json:"ref,omitempty"`
	Track string `json:"track,omitempty"`
	// Digest is the CAS Directory digest in "<hash>-<size>" form.
	// Empty in v1 (stub repos). Populated in PR #58 when we wire
	// real Directory paths.
	Digest string `json:"digest,omitempty"`
}

// sourcesJSON is the on-disk shape (top-level "sources" array
// keeps the door open for future schema additions like a CAS
// instance-name without a v1→v2 migration).
type sourcesJSON struct {
	Sources []sourceEntry `json:"sources"`
}

// collectSources walks the graph, derives sourceKey for every
// non-kind:local source, and returns one entry per unique key.
// Two elements referencing the same git_repo at the same ref
// share an entry. Output ordering is by key for deterministic
// rendering.
func collectSources(g *graph) sourcesJSON {
	seen := map[string]sourceEntry{}
	for _, elem := range g.Elements {
		for _, rs := range elem.Sources {
			key := sourceKey(rs)
			if key == "" {
				continue
			}
			if _, dup := seen[key]; dup {
				continue
			}
			ref := ""
			if rs.Ref.Kind != 0 {
				ref = rs.Ref.Value
			}
			seen[key] = sourceEntry{
				Key:   key,
				Kind:  rs.Kind,
				URL:   rs.URL,
				Ref:   ref,
				Track: rs.Track,
			}
		}
	}
	keys := sliceutil.SortedKeys(seen)
	out := sourcesJSON{Sources: make([]sourceEntry, 0, len(keys))}
	for _, k := range keys {
		out.Sources = append(out.Sources, seen[k])
	}
	return out
}

// marshalSourcesJSON returns the canonical on-disk bytes — pretty
// printed (so PR review can read it), trailing newline.
func marshalSourcesJSON(s sourcesJSON) ([]byte, error) {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}
