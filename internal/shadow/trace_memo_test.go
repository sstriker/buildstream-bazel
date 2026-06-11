package shadow

import (
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
