package lower

import (
	"reflect"
	"testing"
)

func TestChainToolToken(t *testing.T) {
	cases := []struct{ in, want string }{
		{"python3", "python3"},
		{"/usr/bin/python3", "python3"},
		{"perl", "perl"},
		{"/opt/tools/gen", "gen"},
	}
	for _, tc := range cases {
		if got := chainToolToken(tc.in); got != tc.want {
			t.Errorf("chainToolToken(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestSplitArgvPathPrefix(t *testing.T) {
	cases := []struct{ in, wantPre, wantPath string }{
		{"/abs/path", "", "/abs/path"},
		{"out=/abs/p", "out=", "/abs/p"},
		{"--out=/abs/p", "--out=", "/abs/p"},
		{"/has=eq/in/path", "", "/has=eq/in/path"}, // '=' after a slash is part of the path
		{"plain", "", "plain"},
	}
	for _, tc := range cases {
		pre, p := splitArgvPathPrefix(tc.in)
		if pre != tc.wantPre || p != tc.wantPath {
			t.Errorf("splitArgvPathPrefix(%q) = (%q, %q), want (%q, %q)", tc.in, pre, p, tc.wantPre, tc.wantPath)
		}
	}
}

// TestChainCwdNames pins the intermediate -> cwd-transient-name assignment:
// basenames, with a collision declining (don't guess).
func TestChainCwdNames(t *testing.T) {
	got, ok := chainCwdNames(map[string]bool{"/tmp/a/int.tmp": true, "/tmp/b/other.h": true})
	if !ok {
		t.Fatal("expected ok for distinct basenames")
	}
	want := map[string]string{"/tmp/a/int.tmp": "int.tmp", "/tmp/b/other.h": "other.h"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("chainCwdNames = %v, want %v", got, want)
	}
	// Colliding basenames decline.
	if _, ok := chainCwdNames(map[string]bool{"/tmp/a/int.tmp": true, "/tmp/b/int.tmp": true}); ok {
		t.Error("colliding basenames must decline (ok=false)")
	}
}

// TestLiveChainStages pins that a pure side-effect stage (no declared-output /
// intermediate operand, e.g. `mktemp -d`) drops out while the contributing
// stages stay.
func TestLiveChainStages(t *testing.T) {
	anc := execAnchors{hostSrcDir: "/src", recordedSrcDir: "/src", hostBuildDir: "/b", recordedBuildDir: "/b"}
	declaredSet := map[string]bool{"gen/value.c": true}
	intermediates := map[string]bool{"/tmp/x/int.tmp": true}
	stages := [][]string{
		{"mktemp", "-d"},
		{"python3", "/src/stageA.py", "/src/input.txt", "/tmp/x/int.tmp"},
		{"python3", "/src/stageB.py", "/tmp/x/int.tmp", "/b/gen/value.c"},
	}
	live := liveChainStages(stages, anc, declaredSet, intermediates)
	if len(live) != 2 {
		t.Fatalf("live stages = %d, want 2 (mktemp dropped)", len(live))
	}
	if live[0][0] != "python3" || live[1][0] != "python3" {
		t.Errorf("unexpected live stages: %v", live)
	}
}
