package lower

import (
	"sort"
	"sync"

	"github.com/sstriker/buildstream-bazel/converter/internal/cmakerun"
)

// LiteralProbeSink is the side-channel collector for arbitrary
// genex literals the lifter could not resolve via the Go-side
// evaluator + structural probe (cmakerun.GenexProbe). It mirrors
// the Rejections collector pattern: a single ToIR pass records
// every unresolved literal here instead of dropping it silently,
// so the orchestrator (convert-element-cmake) can run the warm
// second configure pass that resolves them via cmake's own
// evaluator (cmakerun.RenderLiteralProbeHook / ReadLiteralProbe).
//
// Lifecycle across the two passes:
//
//   - Pass 1: Options.LiteralProbeSink is non-nil and
//     Options.LiteralResolutions is empty. Lift sites that hit a
//     genexeval.UnsupportedError on an arbitrary literal call
//     Want() to record a probe request instead of falling back to
//     legacy. The orchestrator drains Requests(), feeds them to
//     cmakerun.Configure as Options.LiteralProbes (the warm second
//     pass), reads the resolved bytes back with ReadLiteralProbe,
//     and keys them by request hash.
//   - Pass 2: the orchestrator re-runs ToIR with
//     Options.LiteralResolutions populated. The same lift sites now
//     find the resolution in hand and emit the resolved value
//     (flat, or a select() over //config:<name> when the literal
//     diverged per config) rather than recording or refusing.
//
// A nil sink disables collection (callers that don't run the two-
// pass loop, e.g. unit-test goldens and the offline --reply-dir
// path), so the literal probe is strictly opt-in and changes no
// existing behavior when unused.
//
// The sink is safe for concurrent Want() calls; ToIR's per-target
// walk is sequential today but lift helpers may fan out later, and
// the mutex keeps the dedup map race-free regardless.
type LiteralProbeSink struct {
	mu   sync.Mutex
	seen map[string]cmakerun.LiteralProbeRequest // keyed by request hash
}

// Want records that the lifter needs literal resolved in the
// context of cmake target (which may be ""). Returns the request's
// stable hash — the same key ReadLiteralProbe / LiteralResolutions
// use — so the caller can look up the resolution on the second
// pass without reconstructing the request. Dedupes by hash, so
// recording the same (literal, target) twice is a no-op beyond the
// first.
//
// Recording on a nil sink is a no-op that still returns the hash,
// so call sites don't need a nil guard before computing the key.
func (s *LiteralProbeSink) Want(literal, target string) string {
	req := cmakerun.LiteralProbeRequest{Literal: literal, Target: target}
	h := req.Hash()
	if s == nil {
		return h
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.seen == nil {
		s.seen = map[string]cmakerun.LiteralProbeRequest{}
	}
	s.seen[h] = req
	return h
}

// Requests returns the collected probe requests in a deterministic
// order (sorted by hash) so the generated hook and any golden stay
// byte-stable. Returns nil for a nil or empty sink.
func (s *LiteralProbeSink) Requests() []cmakerun.LiteralProbeRequest {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.seen) == 0 {
		return nil
	}
	out := make([]cmakerun.LiteralProbeRequest, 0, len(s.seen))
	for _, r := range s.seen {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Hash() < out[j].Hash() })
	return out
}

// Len reports how many distinct literals have been recorded. The
// orchestrator gates the second pass on Len() > 0 — an empty set
// means every genex resolved in pass 1, so no warm reconfigure is
// needed (the common case, zero overhead).
func (s *LiteralProbeSink) Len() int {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.seen)
}
