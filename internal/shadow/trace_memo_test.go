package shadow

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// ParseTrace returns identical results on repeated calls (cache hit path) and
// keeps distinct traces distinct (no fingerprint collision for our inputs).
func TestParseTrace_MemoizedAndDistinct(t *testing.T) {
	a := `{"args":["x","PUBLIC","inc"],"cmd":"target_include_directories","file":"/src/CMakeLists.txt","line":4}
{"args":["COMMAND","git","describe","OUTPUT_VARIABLE","V"],"cmd":"execute_process","file":"/src/CMakeLists.txt","line":7}
`
	b := `{"args":["y","PRIVATE","other"],"cmd":"target_include_directories","file":"/src/sub/CMakeLists.txt","line":1}
`
	a1 := ParseTrace([]byte(a))
	a2 := ParseTrace([]byte(a)) // cache hit
	if !reflect.DeepEqual(a1, a2) {
		t.Fatalf("repeated ParseTrace differ:\n%v\n%v", a1, a2)
	}
	if len(a1) != 2 || a1[1].Cmd != "execute_process" {
		t.Fatalf("unexpected parse of a: %+v", a1)
	}
	bRes := ParseTrace([]byte(b))
	if len(bRes) != 1 || bRes[0].Args[0] != "y" {
		t.Fatalf("trace b mis-parsed (collision?): %+v", bRes)
	}
	// a still parses to its own content after b (distinct keys).
	if a3 := ParseTrace([]byte(a)); !reflect.DeepEqual(a1, a3) {
		t.Fatalf("trace a changed after b: %+v vs %+v", a1, a3)
	}
	if ParseTrace(nil) != nil {
		t.Errorf("empty trace should be nil")
	}
}

// A large trace (above the 3-window fingerprint threshold) round-trips and
// differs from a sibling that shares head+tail but diverges in the middle.
func TestParseTrace_LargeFingerprintMiddleSensitive(t *testing.T) {
	head := `{"args":["a"],"cmd":"message","file":"/s/CMakeLists.txt","line":1}` + "\n"
	tail := `{"args":["z"],"cmd":"message","file":"/s/CMakeLists.txt","line":9}` + "\n"
	filler := strings.Repeat(`{"args":["m"],"cmd":"message","file":"/s/CMakeLists.txt","line":5}`+"\n", 400)
	fillerAlt := strings.Repeat(`{"args":["n"],"cmd":"message","file":"/s/CMakeLists.txt","line":5}`+"\n", 400)
	t1 := head + filler + tail
	t2 := head + fillerAlt + tail // same head/tail, different middle
	r1 := ParseTrace([]byte(t1))
	r2 := ParseTrace([]byte(t2))
	if r1[1].Args[0] == r2[1].Args[0] {
		t.Fatalf("middle-divergent traces collided: %q == %q", r1[1].Args[0], r2[1].Args[0])
	}
	if r1[1].Args[0] != "m" || r2[1].Args[0] != "n" {
		t.Fatalf("mis-parse: r1=%q r2=%q", r1[1].Args[0], r2[1].Args[0])
	}
}

// TestParseTrace_ParallelMatchesSerial crosses the parallelLineThreshold so the
// concurrent decode path runs, and asserts it yields exactly what a serial
// reference parse would: same events, same order, same drop of non-JSON lines.
// Locks the byte-identity of the parallel path in regression form (the inline
// path is covered by the small-fixture tests above).
func TestParseTrace_ParallelMatchesSerial(t *testing.T) {
	var b strings.Builder
	const n = 5000 // > parallelLineThreshold (2048) → exercises the worker pool
	for i := 0; i < n; i++ {
		switch i % 4 {
		case 0:
			fmt.Fprintf(&b, `{"args":["t%d","PUBLIC","inc%d"],"cmd":"target_include_directories","file":"/src/CMakeLists.txt","line":%d}`, i, i, i)
		case 1:
			fmt.Fprintf(&b, `{"args":["L%d","NDEBUG"],"cmd":"add_definitions","file":"/src/CMakeLists.txt","line":%d,"frame":2}`, i, i)
		case 2:
			// A non-JSON / banner-style line that must be dropped (same as cmake's
			// leading banner) — verifies the drop rule under parallelism.
			b.WriteString("not-a-json-line " + strings.Repeat("x", i%17))
		case 3:
			b.WriteString(`{"args":["s"],"cmd":"set","file":"/src/CMakeLists.txt","line":1,"defer":"__0"}`)
		}
		b.WriteByte('\n')
	}
	raw := []byte(b.String())

	// Serial reference: replicate parseTraceUncached's drop + order without the
	// worker pool, independent of the production code path.
	var want []TraceEvent
	for _, line := range strings.Split(b.String(), "\n") {
		tl := strings.TrimSpace(line)
		if len(tl) == 0 || tl[0] != '{' {
			continue
		}
		var ev TraceEvent
		if json.Unmarshal([]byte(tl), &ev) != nil {
			continue
		}
		want = append(want, ev)
	}

	got := parseTraceUncached(raw)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parallel parse != serial reference: got %d events, want %d", len(got), len(want))
	}
	if len(got) != n*3/4 {
		t.Errorf("expected %d kept events (3 of every 4 lines), got %d", n*3/4, len(got))
	}
}
