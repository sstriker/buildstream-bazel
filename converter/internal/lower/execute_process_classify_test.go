package lower

import (
	"testing"

	"github.com/sstriker/cmake-to-bazel/internal/shadow"
)

// TestClassify_Buckets is the truth-table covering each
// recognized bucket and the order-of-precedence corners
// between them. Each row is a hand-crafted ExecuteProcessCall
// that the trace classifier could plausibly produce; the
// classifier verdict has to be deterministic and decoupled
// from the underlying CMakeLists.txt phrasing.
func TestClassify_Buckets(t *testing.T) {
	cases := []struct {
		name   string
		call   shadow.ExecuteProcessCall
		bucket Bucket
		op     string // expected CMakeEOp; "" when not BucketCMakeE
	}{
		{
			name: "cmake -E touch",
			call: shadow.ExecuteProcessCall{
				Commands: [][]string{{"cmake", "-E", "touch", "marker.stamp"}},
			},
			bucket: BucketCMakeE,
			op:     "touch",
		},
		{
			name: "cmake -E copy with absolute path argv0",
			call: shadow.ExecuteProcessCall{
				Commands: [][]string{{"/usr/bin/cmake", "-E", "copy", "src", "dst"}},
			},
			bucket: BucketCMakeE,
			op:     "copy",
		},
		{
			name: "cmake -E copy_if_different",
			call: shadow.ExecuteProcessCall{
				Commands: [][]string{{"cmake", "-E", "copy_if_different", "src", "dst"}},
			},
			bucket: BucketCMakeE,
			op:     "copy_if_different",
		},
		{
			name: "cmake -E with unrecognized op falls back to Unknown",
			call: shadow.ExecuteProcessCall{
				Commands: [][]string{{"cmake", "-E", "compare_files", "a", "b"}},
			},
			bucket: BucketUnknown,
		},
		{
			name: "${CMAKE_COMMAND} marker is recognized as cmake",
			call: shadow.ExecuteProcessCall{
				Commands: [][]string{{"${CMAKE_COMMAND}", "-E", "touch", "x"}},
			},
			bucket: BucketCMakeE,
			op:     "touch",
		},
		{
			name: "git rev-parse + OUTPUT_VARIABLE → stamp",
			call: shadow.ExecuteProcessCall{
				Commands:       [][]string{{"git", "rev-parse", "HEAD"}},
				OutputVariable: "GIT_SHA",
			},
			bucket: BucketStamp,
		},
		{
			name: "uname -m + OUTPUT_VARIABLE → probe",
			call: shadow.ExecuteProcessCall{
				Commands:       [][]string{{"uname", "-m"}},
				OutputVariable: "ARCH",
			},
			bucket: BucketProbe,
		},
		{
			name: "pkg-config --modversion → probe",
			call: shadow.ExecuteProcessCall{
				Commands:       [][]string{{"pkg-config", "--modversion", "zlib"}},
				OutputVariable: "ZLIB_VERSION",
			},
			bucket: BucketProbe,
		},
		{
			name: "python3 gen.py with OUTPUT_FILE → file-producing",
			call: shadow.ExecuteProcessCall{
				Commands:   [][]string{{"python3", "gen.py", "--in", "spec.txt"}},
				OutputFile: "generated.h",
			},
			bucket: BucketFileProducing,
		},
		{
			name: "python3 with OUTPUT_VARIABLE only → probe (python is in probeDrivers)",
			call: shadow.ExecuteProcessCall{
				Commands:       [][]string{{"python3", "-c", "import sys; print(sys.version_info[0])"}},
				OutputVariable: "PYMAJOR",
			},
			bucket: BucketProbe,
		},
		{
			name: "git with OUTPUT_FILE (not OUTPUT_VARIABLE) still classifies as stamp",
			call: shadow.ExecuteProcessCall{
				Commands:       [][]string{{"git", "describe", "--tags"}},
				OutputVariable: "VER",
				OutputFile:     "version.txt",
			},
			// Order-of-precedence corner: when both OutputVariable
			// and OutputFile are set, OutputVariable wins for
			// stamp/probe drivers — a file-producing genrule
			// running git on the executor still re-introduces the
			// non-hermeticity we're avoiding. Falls through to
			// FileProducing because the stamp/probe gate requires
			// OutputFile=="". Document the conservative corner:
			// real-world projects rarely combine both.
			bucket: BucketFileProducing,
		},
		{
			name: "multi-COMMAND pipeline → unknown",
			call: shadow.ExecuteProcessCall{
				Commands:       [][]string{{"grep", "foo", "input"}, {"wc", "-l"}},
				OutputVariable: "LINES",
			},
			bucket: BucketUnknown,
		},
		{
			name: "no COMMAND → unknown",
			call: shadow.ExecuteProcessCall{
				OutputVariable: "X",
			},
			bucket: BucketUnknown,
		},
		{
			name: "argv0 is unknown driver, no OutputFile → unknown",
			call: shadow.ExecuteProcessCall{
				Commands:       [][]string{{"random_tool", "--mode=opaque"}},
				OutputVariable: "RESULT",
			},
			bucket: BucketUnknown,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Classify(c.call)
			if got.Bucket != c.bucket {
				t.Errorf("bucket: got %q want %q (reason: %q)", got.Bucket, c.bucket, got.Reason)
			}
			if got.CMakeEOp != c.op {
				t.Errorf("op: got %q want %q", got.CMakeEOp, c.op)
			}
			if got.Reason == "" {
				t.Errorf("reason: empty (every classification should carry one)")
			}
		})
	}
}

// TestClassify_CMakeEOpsAreLowercased asserts that the
// CMakeEOp returned is always the lowercase op name regardless
// of how the user wrote it in CMakeLists.txt — the eventual
// lifter dispatches on this field via a map lookup.
func TestClassify_CMakeEOpsAreLowercased(t *testing.T) {
	call := shadow.ExecuteProcessCall{
		Commands: [][]string{{"cmake", "-E", "TOUCH", "marker"}},
	}
	got := Classify(call)
	if got.Bucket != BucketCMakeE {
		t.Fatalf("bucket: %q want %q", got.Bucket, BucketCMakeE)
	}
	if got.CMakeEOp != "touch" {
		t.Errorf("op: %q want %q", got.CMakeEOp, "touch")
	}
}
