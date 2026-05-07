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
			// python3 is in dualUseProbeDrivers: it CAN be
			// a probe (`python3 -c "import sys; print(...)"`)
			// or a code generator (`python3 gen.py > out.h`).
			// OUTPUT_FILE alone disambiguates as code
			// generation; falls through to FileProducing for
			// the lifter to hoist as a build-time genrule.
			bucket: BucketFileProducing,
		},
		{
			name: "python3 -c with OUTPUT_VARIABLE only → probe",
			call: shadow.ExecuteProcessCall{
				Commands:       [][]string{{"python3", "-c", "import sys; print(sys.version_info[0])"}},
				OutputVariable: "PYMAJOR",
			},
			// Dual-use probe drivers classify as Probe only
			// when OUTPUT_VARIABLE is set without OUTPUT_FILE
			// (the unambiguous probe shape).
			bucket: BucketProbe,
		},
		{
			name: "git + OUTPUT_FILE → stamp (driver-first; stamp drivers can't hoist)",
			call: shadow.ExecuteProcessCall{
				Commands:   [][]string{{"git", "describe", "--tags"}},
				OutputFile: "version.txt",
			},
			// VCS drivers have no legitimate code-generation
			// use; hoisting `git describe > out.txt` into a
			// build-time genrule would run git on the
			// executor — re-introduces the non-hermeticity
			// the refusal is meant to prevent. Stamp wins
			// over FileProducing regardless of OUTPUT_FILE.
			bucket: BucketStamp,
		},
		{
			name: "git + OUTPUT_VARIABLE + OUTPUT_FILE → stamp (still driver-first)",
			call: shadow.ExecuteProcessCall{
				Commands:       [][]string{{"git", "describe", "--tags"}},
				OutputVariable: "VER",
				OutputFile:     "version.txt",
			},
			// Both writeback channels populated; stamp
			// driver classification is unaffected. Both
			// OutputVariable and OutputFile thread into
			// the refusal reason via outputContext for
			// triage but neither flips the bucket.
			bucket: BucketStamp,
		},
		{
			name: "uname + OUTPUT_FILE → probe (strong probe driver, driver-first)",
			call: shadow.ExecuteProcessCall{
				Commands:   [][]string{{"uname", "-m"}},
				OutputFile: "arch.txt",
			},
			// uname is in strongProbeDrivers (unambiguously
			// a host probe — no legitimate code-generation
			// use). Same driver-first rule as stamp: a
			// build-time genrule running `uname -m` would
			// re-introduce host-environment leakage.
			bucket: BucketProbe,
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
