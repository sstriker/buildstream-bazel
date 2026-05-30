package lower

import (
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/internal/cmakerun"
)

func TestLiteralProbeSink_NilSafe(t *testing.T) {
	var s *LiteralProbeSink
	// Want on a nil sink returns the hash and records nothing.
	h := s.Want("$<TARGET_PROPERTY:foo,P>", "foo")
	want := cmakerun.LiteralProbeRequest{Literal: "$<TARGET_PROPERTY:foo,P>", Target: "foo"}.Hash()
	if h != want {
		t.Fatalf("nil-sink Want hash = %q, want %q", h, want)
	}
	if s.Len() != 0 {
		t.Fatalf("nil-sink Len = %d, want 0", s.Len())
	}
	if s.Requests() != nil {
		t.Fatalf("nil-sink Requests = %v, want nil", s.Requests())
	}
}

func TestLiteralProbeSink_RecordsAndDedupes(t *testing.T) {
	s := &LiteralProbeSink{}
	h1 := s.Want("$<TARGET_PROPERTY:foo,P>", "foo")
	h2 := s.Want("$<TARGET_PROPERTY:foo,P>", "foo") // dup
	h3 := s.Want("$<TARGET_PROPERTY:bar,Q>", "bar")

	if h1 != h2 {
		t.Fatalf("same request hashed differently: %q vs %q", h1, h2)
	}
	if h1 == h3 {
		t.Fatalf("distinct requests shared a hash: %q", h1)
	}
	if s.Len() != 2 {
		t.Fatalf("Len = %d, want 2 (dedupe)", s.Len())
	}

	reqs := s.Requests()
	if len(reqs) != 2 {
		t.Fatalf("Requests len = %d, want 2", len(reqs))
	}
	// Deterministic: sorted by hash.
	if reqs[0].Hash() > reqs[1].Hash() {
		t.Fatalf("Requests not sorted by hash: %q then %q", reqs[0].Hash(), reqs[1].Hash())
	}
}

func TestLiteralProbeSink_EmptyRequests(t *testing.T) {
	s := &LiteralProbeSink{}
	if s.Requests() != nil {
		t.Fatalf("fresh sink Requests = %v, want nil", s.Requests())
	}
	if s.Len() != 0 {
		t.Fatalf("fresh sink Len = %d, want 0", s.Len())
	}
}
