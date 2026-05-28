package lower

import "testing"

func TestReanchorProvenanceFile(t *testing.T) {
	const (
		cmakeSrc   = "/tmp/proj/src"
		cmakeBuild = "/tmp/proj/build"
	)
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"already relative passes through",
			"CMakeLists.txt", "CMakeLists.txt"},
		{"inside cmakeSrc — strip",
			"/tmp/proj/src/sub/CMakeLists.txt", "sub/CMakeLists.txt"},
		{"inside cmakeBuild — strip (configure_file shape)",
			"/tmp/proj/build/generated/CMakeLists.txt", "generated/CMakeLists.txt"},
		{"inside cmakeSrc parent (third-party sibling)",
			"/tmp/proj/third-party/foo/CMakeLists.txt", "third-party/foo/CMakeLists.txt"},
		{"completely outside — passes through",
			"/opt/external/foo/CMakeLists.txt", "/opt/external/foo/CMakeLists.txt"},
		{"empty passes through",
			"", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := reanchorProvenanceFile(tc.in, cmakeSrc, cmakeBuild); got != tc.want {
				t.Errorf("reanchorProvenanceFile(%q):\n  got:  %q\n  want: %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestReanchorProvenanceFile_DegenerateRoot(t *testing.T) {
	// Guard: when cmakeSrc is the filesystem root, parent("/") ==
	// "/" and would otherwise match every absolute path. The
	// function should refuse to over-anchor in that case.
	got := reanchorProvenanceFile("/etc/passwd", "/", "/")
	if got != "/etc/passwd" {
		t.Errorf("filesystem-root cmakeSrc: got %q; want unchanged /etc/passwd", got)
	}
}

func TestReanchorProvenanceFile_EmptyAnchors(t *testing.T) {
	// Absolute path + empty anchors: leave alone.
	got := reanchorProvenanceFile("/abs/path/CMakeLists.txt", "", "")
	if got != "/abs/path/CMakeLists.txt" {
		t.Errorf("empty anchors should leave abs path unchanged; got %q", got)
	}
}
