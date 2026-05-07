package main

// canonicalize / prefixSub delegate to internal/tracenorm.
//
// The shaping logic moved out of this command so trace-publish
// (the round-2 publisher binary) can re-apply the same canonical
// form defensively before computing the CAS Directory digest. See
// internal/tracenorm for the actual transforms.

import (
	"github.com/sstriker/cmake-to-bazel/internal/tracenorm"
)

// prefixSub is the local alias build-tracer's flag layer wraps.
// Conversions to/from tracenorm.PrefixSub keep main.go's
// flag.Value implementation unchanged.
type prefixSub = tracenorm.PrefixSub

// canonicalizeWith threads --source-root through to
// tracenorm.CanonicalizeWith so openat lines captured by the
// strace-fallback or native backend get filtered + stabilized
// (or dropped, when sourceRoot is empty). Mirrors the same
// Options surface trace-publish uses for defensive
// re-canonicalization.
func canonicalizeWith(rawPath, outPath string, prefixSubs []prefixSub, sourceRoot string) error {
	return tracenorm.CanonicalizeWith(rawPath, outPath, tracenorm.Options{
		PrefixSubs: prefixSubs,
		SourceRoot: sourceRoot,
	})
}
