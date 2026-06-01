package lower

import (
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/internal/cmakerun"
)

// TestProbeGenexValuesForBody exercises tier (b′) — the per-literal
// two-pass probe that resolves each top-level genex in a
// file(GENERATE) body individually, for the case (b)'s positional
// anchoring can't handle: adjacent genexes with no static separator.
func TestProbeGenexValuesForBody(t *testing.T) {
	// Two adjacent genexes — extractGenexValues (tier b) refuses
	// this shape ("adjacent to the next genex with no static
	// separator"), so it's exactly what (b′) is for.
	body := []byte("$<TARGET_PROPERTY:app,P1>$<TARGET_PROPERTY:app,P2>\n")

	// Sanity: confirm tier (b) genuinely can't recover this, so the
	// test isn't vacuously exercising a case (b) already handles.
	if _, err := extractGenexValues(body, []byte("AABB\n")); err == nil {
		t.Fatalf("extractGenexValues unexpectedly succeeded on adjacent genexes; tier (b′) test would be vacuous")
	}

	lit1 := "$<TARGET_PROPERTY:app,P1>"
	lit2 := "$<TARGET_PROPERTY:app,P2>"
	hash := func(lit string) string {
		return cmakerun.LiteralProbeRequest{Literal: lit}.Hash()
	}

	t.Run("nil cc is inert", func(t *testing.T) {
		if _, ok := probeGenexValuesForBody(nil, body); ok {
			t.Fatal("nil cc should return false")
		}
	})

	t.Run("no genex", func(t *testing.T) {
		cc := newCodegenContext()
		if _, ok := probeGenexValuesForBody(cc, []byte("plain text\n")); ok {
			t.Fatal("genex-free body should return false")
		}
	})

	t.Run("pass 1 records all literals and drops", func(t *testing.T) {
		sink := &LiteralProbeSink{}
		cc := newCodegenContext()
		cc.LiteralProbeSink = sink
		if _, ok := probeGenexValuesForBody(cc, body); ok {
			t.Fatal("pass 1 should return false (resolutions not in hand yet)")
		}
		// Both literals must be recorded in one pass so the single
		// warm reconfigure probes them together (not one per literal).
		reqs := sink.Requests()
		if len(reqs) != 2 {
			t.Fatalf("pass 1 should record 2 distinct literals, got %d: %+v", len(reqs), reqs)
		}
	})

	t.Run("pass 2 resolves adjacent genexes", func(t *testing.T) {
		cc := newCodegenContext()
		cc.LiteralResolutions = map[string]cmakerun.LiteralResolution{
			hash(lit1): {PerConfig: map[string]string{"": "AA"}},
			hash(lit2): {PerConfig: map[string]string{"": "BB"}},
		}
		vals, ok := probeGenexValuesForBody(cc, body)
		if !ok {
			t.Fatal("pass 2 should resolve both literals")
		}
		if vals[lit1] != "AA" || vals[lit2] != "BB" {
			t.Fatalf("resolved values = %+v, want %s→AA %s→BB", vals, lit1, lit2)
		}
	})

	t.Run("partial resolution drops", func(t *testing.T) {
		cc := newCodegenContext()
		cc.LiteralResolutions = map[string]cmakerun.LiteralResolution{
			hash(lit1): {PerConfig: map[string]string{"": "AA"}},
			// lit2 unresolved
		}
		if _, ok := probeGenexValuesForBody(cc, body); ok {
			t.Fatal("a partially-resolved body should drop to legacy")
		}
	})

	t.Run("per-config divergence drops", func(t *testing.T) {
		cc := newCodegenContext()
		cc.LiteralResolutions = map[string]cmakerun.LiteralResolution{
			hash(lit1): {PerConfig: map[string]string{"": "AA"}},
			hash(lit2): {PerConfig: map[string]string{"Release": "BB", "Debug": "CC"}},
		}
		// Divergent literal has no single value for a flat literal-
		// replace map — that's the future select()-capable consumer's
		// job, not this one.
		if _, ok := probeGenexValuesForBody(cc, body); ok {
			t.Fatal("per-config-divergent literal should drop")
		}
	})

	t.Run("resolved value still carrying a genex drops", func(t *testing.T) {
		cc := newCodegenContext()
		cc.LiteralResolutions = map[string]cmakerun.LiteralResolution{
			hash(lit1): {PerConfig: map[string]string{"": "AA"}},
			hash(lit2): {PerConfig: map[string]string{"": "$<CONFIG>"}},
		}
		if _, ok := probeGenexValuesForBody(cc, body); ok {
			t.Fatal("a resolved value that still carries a genex can't be a literal replacement")
		}
	})
}
