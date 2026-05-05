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

func canonicalize(rawPath, outPath string, prefixSubs []prefixSub) error {
	return tracenorm.Canonicalize(rawPath, outPath, prefixSubs)
}
