package lower

import (
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/internal/fileapi"
	"github.com/sstriker/buildstream-bazel/internal/shadow"
)

// TestParseTraceFactsCached pins the cache-reuse contract that eliminates the
// per-pass re-parsing of the (invariant) trace: once populated, a subsequent
// call returns the cached traceFacts WITHOUT re-running parseTraceFacts. The
// nil reply proves no parse happens on the hit — parseTraceFacts would panic on
// a nil *fileapi.Reply, so a clean return is the "reused, not re-parsed" signal.
func TestParseTraceFactsCached(t *testing.T) {
	marker := &shadow.Decoded{}
	c := NewTraceFactsCache()
	c.tf = &traceFacts{decodedTrace: marker, traceDecoded: true}

	got := parseTraceFactsCached(nil, fileapi.Configuration{}, Options{TraceFactsCache: c})
	if got.decodedTrace != marker {
		t.Fatal("a populated cache must return its traceFacts without re-parsing")
	}

	// A cache pointer shared across passes reuses the SAME parse: the second
	// call still sees the marker (the memo isn't cleared between passes).
	if got2 := parseTraceFactsCached(nil, fileapi.Configuration{}, Options{TraceFactsCache: c}); got2.decodedTrace != marker {
		t.Fatal("the shared cache must keep reusing the first parse across passes")
	}
}
