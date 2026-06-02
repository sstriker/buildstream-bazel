package lower

import (
	"strings"
	"testing"

	"github.com/sstriker/buildstream-bazel/internal/shadow"
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
			// Raw `cp` (issue #312): classified as a copy and
			// lifted, not refused. The classifier doesn't touch
			// the filesystem — file-vs-dir / symlink-deref is the
			// lifter's job.
			name: "raw cp → cmake-e cp",
			call: shadow.ExecuteProcessCall{
				Commands: [][]string{{"cp", "-RauL", "/src/data", "/build"}},
			},
			bucket: BucketCMakeE,
			op:     "cp",
		},
		{
			// cp with a captured exit/var still classifies as a
			// copy — the copy happens; the captured value flows
			// through the dump-vars rescue.
			name: "cp with RESULT_VARIABLE → cmake-e cp",
			call: shadow.ExecuteProcessCall{
				Commands:       [][]string{{"/bin/cp", "a.txt", "/build/a.txt"}},
				ResultVariable: "_R",
			},
			bucket: BucketCMakeE,
			op:     "cp",
		},
		{
			// Raw `touch`: the POSIX analog of `cmake -E touch`
			// (already lifted). Classified as cmake-e with the
			// touch_raw sentinel op so the dispatcher re-slices argv
			// from argv[1:] (no `-E <op>` prefix) and routes to
			// liftTouch. Flag-handling is the lifter's job.
			name: "raw touch → cmake-e touch_raw",
			call: shadow.ExecuteProcessCall{
				Commands: [][]string{{"touch", "/build/marker.stamp"}},
			},
			bucket: BucketCMakeE,
			op:     "touch_raw",
		},
		{
			// Raw `ln -s`: the POSIX analog of `cmake -E
			// create_symlink` (already lifted as a copy). Classified
			// as cmake-e with op "ln"; the lifter reproduces the link
			// as a copy of the target's bytes.
			name: "raw ln -s → cmake-e ln",
			call: shadow.ExecuteProcessCall{
				Commands: [][]string{{"ln", "-s", "/src/bin/clang-18", "/build/bin/clang"}},
			},
			bucket: BucketCMakeE,
			op:     "ln",
		},
		{
			// cmake -E copy_directory: recursive contents-copy,
			// recognized as a supported cmake -E op.
			name: "cmake -E copy_directory",
			call: shadow.ExecuteProcessCall{
				Commands: [][]string{{"cmake", "-E", "copy_directory", "src", "dst"}},
			},
			bucket: BucketCMakeE,
			op:     "copy_directory",
		},
		{
			// cmake -E rename: lifted as a copy.
			name: "cmake -E rename",
			call: shadow.ExecuteProcessCall{
				Commands: [][]string{{"cmake", "-E", "rename", "a", "b"}},
			},
			bucket: BucketCMakeE,
			op:     "rename",
		},
		{
			// Raw `mv`: the POSIX analog of cmake -E rename.
			name: "raw mv → cmake-e mv",
			call: shadow.ExecuteProcessCall{
				Commands: [][]string{{"mv", "/src/a.txt", "/build/b.txt"}},
			},
			bucket: BucketCMakeE,
			op:     "mv",
		},
		{
			// cmake -E make_directory: benign no-op (recognized, not
			// refused; the lifter skips it).
			name: "cmake -E make_directory → no-op",
			call: shadow.ExecuteProcessCall{
				Commands: [][]string{{"cmake", "-E", "make_directory", "/build/d"}},
			},
			bucket: BucketCMakeE,
			op:     "make_directory",
		},
		{
			// Raw `mkdir`: benign no-op analog.
			name: "raw mkdir → no-op",
			call: shadow.ExecuteProcessCall{
				Commands: [][]string{{"mkdir", "-p", "/build/d"}},
			},
			bucket: BucketCMakeE,
			op:     "mkdir",
		},
		{
			// Raw `rm`: benign no-op analog.
			name: "raw rm → no-op",
			call: shadow.ExecuteProcessCall{
				Commands: [][]string{{"rm", "-rf", "/build/stale"}},
			},
			bucket: BucketCMakeE,
			op:     "rm",
		},
		{
			// A genuinely opaque copy-shaped driver (rsync) is NOT
			// in copyDrivers, so it still refuses — the cp lift is
			// scoped to drivers whose semantics the lifter can
			// reproduce exactly.
			name: "rsync (not cp) still refuses",
			call: shadow.ExecuteProcessCall{
				Commands: [][]string{{"rsync", "-a", "/src/data", "/build"}},
			},
			bucket: BucketUnknown,
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
			// hg (Mercurial) is a stampDriver alongside git —
			// `hg id -i` captured to a variable is a VCS revision
			// stamp, classified driver-first exactly like git.
			name: "hg id + OUTPUT_VARIABLE → stamp",
			call: shadow.ExecuteProcessCall{
				Commands:       [][]string{{"hg", "id", "-i"}},
				OutputVariable: "HG_ID",
			},
			bucket: BucketStamp,
		},
		{
			// svn is a stampDriver — `svn info` / `svnversion`
			// output is a VCS revision stamp.
			name: "svn info + OUTPUT_VARIABLE → stamp",
			call: shadow.ExecuteProcessCall{
				Commands:       [][]string{{"svn", "info", "--show-item", "revision"}},
				OutputVariable: "SVN_REV",
			},
			bucket: BucketStamp,
		},
		{
			// Subcommand-agnostic: classification keys on the
			// driver, not the git subcommand. `git log -1 --format`
			// is a stamp just like rev-parse / describe.
			name: "git log + OUTPUT_VARIABLE → stamp (subcommand-agnostic)",
			call: shadow.ExecuteProcessCall{
				Commands:       [][]string{{"git", "log", "-1", "--format=%H"}},
				OutputVariable: "GIT_COMMIT",
			},
			bucket: BucketStamp,
		},
		{
			name: "uname -m + OUTPUT_FILE → probe (strong probe driver, driver-first)",
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
		{
			name: "python3 -c with RESULT_VARIABLE only → probe (import-check pattern)",
			call: shadow.ExecuteProcessCall{
				Commands:       [][]string{{"python3", "-c", "import pygments"}},
				ResultVariable: "_R",
			},
			// Canonical "is this module available?" probe: exit
			// status is the answer, no captured stdout. Without
			// the RESULT_VARIABLE branch in the dual-use gate,
			// this falls through to Unknown.
			bucket: BucketProbe,
		},
		{
			name: "ninja --version with OUTPUT_VARIABLE → probe",
			call: shadow.ExecuteProcessCall{
				Commands:       [][]string{{"ninja", "--version"}},
				OutputVariable: "NINJA_VER",
			},
			bucket: BucketProbe,
		},
		{
			name: "cc -Wl,--version with OUTPUT_VARIABLE → probe (linker capability)",
			call: shadow.ExecuteProcessCall{
				Commands:       [][]string{{"cc", "-Wl,--version", "-o", "/dev/null"}},
				OutputVariable: "LD_VER",
			},
			bucket: BucketProbe,
		},
		{
			name: "ar -D probe → probe (deterministic-mode capability check)",
			call: shadow.ExecuteProcessCall{
				Commands:       [][]string{{"ar", "rD", "t.a"}},
				ResultVariable: "_AR_D",
			},
			bucket: BucketProbe,
		},
		{
			name: "ranlib -D probe → probe (deterministic-mode capability check)",
			call: shadow.ExecuteProcessCall{
				Commands:       [][]string{{"ranlib", "-D", "t.a"}},
				ResultVariable: "_RL_D",
			},
			bucket: BucketProbe,
		},
		{
			name: "sh config.guess → probe (host-triple detection script)",
			call: shadow.ExecuteProcessCall{
				Commands:       [][]string{{"/bin/sh", "/llvm/cmake/config.guess"}},
				OutputVariable: "TT_OUT",
			},
			bucket: BucketProbe,
		},
		{
			name: "direct ./config.sub → probe (host-triple detection script)",
			call: shadow.ExecuteProcessCall{
				Commands:       [][]string{{"./config.sub", "x86_64-pc-linux-gnu"}},
				OutputVariable: "CANON",
			},
			bucket: BucketProbe,
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

// TestExecuteProcessRunsHostDetectionScript guards the host-triple
// detection recognition: a shell-run or direct config.guess/config.sub
// matches, but a script named only as a data argument (cp config.guess)
// or an unrelated tool does not.
func TestExecuteProcessRunsHostDetectionScript(t *testing.T) {
	cases := []struct {
		name string
		argv []string
		want bool
	}{
		{"sh config.guess", []string{"/bin/sh", "/llvm/cmake/config.guess"}, true},
		{"bash config.sub", []string{"bash", "config.sub", "x86_64"}, true},
		{"direct ./config.guess", []string{"./config.guess"}, true},
		{"cp names script as data arg", []string{"cp", "config.guess", "/tmp/x"}, false},
		{"unrelated tool", []string{"gcc", "-c", "foo.c"}, false},
		{"empty argv", nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := executeProcessRunsHostDetectionScript(c.argv); got != c.want {
				t.Errorf("got %v want %v", got, c.want)
			}
		})
	}
}

// TestClassify_DriverBasenameNormalisation locks in the
// argv[0] normalisation rules: path stripping (POSIX +
// Windows), case-fold to lower, .exe suffix removal. These
// keep the stamp / probe / cmake-driver maps as plain
// lower-case canonical names and avoid duplicating
// per-platform variants in each map.
func TestClassify_DriverBasenameNormalisation(t *testing.T) {
	cases := []struct {
		name   string
		argv0  string
		bucket Bucket
	}{
		{
			// `C:\Program Files\CMake\bin\cmake.exe -E touch x` —
			// Windows-style absolute path with .exe suffix;
			// should classify as cmake-e regardless of platform.
			name:   "Windows cmake.exe path",
			argv0:  `C:\Program Files\CMake\bin\cmake.exe`,
			bucket: BucketCMakeE,
		},
		{
			// `/usr/bin/Cmake -E touch x` — POSIX path with
			// non-canonical case (case-insensitive
			// filesystems can surface mixed case).
			name:   "POSIX Cmake mixed case",
			argv0:  "/usr/bin/Cmake",
			bucket: BucketCMakeE,
		},
		{
			// `git.exe rev-parse HEAD` — Windows git
			// executable; should classify as Stamp.
			name:   "git.exe Windows",
			argv0:  "git.exe",
			bucket: BucketStamp,
		},
		{
			// `C:\Tools\HOSTNAME.EXE` — uppercase .EXE plus
			// Windows path; should classify as Probe (strong
			// driver: hostname).
			name:   "uppercase HOSTNAME.EXE Windows",
			argv0:  `C:\Tools\HOSTNAME.EXE`,
			bucket: BucketProbe,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			call := shadow.ExecuteProcessCall{}
			switch c.bucket {
			case BucketCMakeE:
				call.Commands = [][]string{{c.argv0, "-E", "touch", "marker"}}
			case BucketStamp:
				call.Commands = [][]string{{c.argv0, "rev-parse", "HEAD"}}
				call.OutputVariable = "GIT_SHA"
			case BucketProbe:
				call.Commands = [][]string{{c.argv0}}
				call.OutputVariable = "H"
			}
			got := Classify(call)
			if got.Bucket != c.bucket {
				t.Errorf("bucket: got %q want %q (reason: %q)", got.Bucket, c.bucket, got.Reason)
			}
		})
	}
}

// TestClassify_RefuseReasonsAreSpecific asserts that the Phase
// 4 fall-through cases (calls that don't match any positive
// classifier arm) carry a per-shape refusal reason rather than
// the legacy catch-all "no recognized lift pattern" string.
// Operators reading failure.json should see the structural
// feature blocking the lift, not a black-box refusal — that's
// what tells them whether to rework the CMakeLists.txt or accept
// the round-2 fallback for the call.
func TestClassify_RefuseReasonsAreSpecific(t *testing.T) {
	cases := []struct {
		name            string
		call            shadow.ExecuteProcessCall
		reasonSubstring string
	}{
		{
			// Opaque driver, no OutputFile, no captured channel —
			// the most common fall-through. The reason names the
			// driver and lists the channels that ARE missing so
			// operators know which knob to add.
			name: "opaque driver with no captured output",
			call: shadow.ExecuteProcessCall{
				Commands: [][]string{{"random_tool", "--mode=opaque"}},
			},
			reasonSubstring: "no captured output channel",
		},
		{
			// OUTPUT_VARIABLE present but driver unknown — the
			// reason names the variable so operators can trace
			// it through the CMakeLists.txt downstream consumers.
			name: "opaque driver writing OUTPUT_VARIABLE",
			call: shadow.ExecuteProcessCall{
				Commands:       [][]string{{"random_tool", "--mode=opaque"}},
				OutputVariable: "RESULT",
			},
			reasonSubstring: "writes OUTPUT_VARIABLE RESULT",
		},
		{
			// RESULTS_VARIABLE (per-COMMAND status capture) is
			// pipeline-shaped state with no Bazel analog; the
			// refusal says so explicitly.
			name: "opaque driver writing RESULTS_VARIABLE",
			call: shadow.ExecuteProcessCall{
				Commands:        [][]string{{"random_tool"}},
				ResultsVariable: "STATUSES",
			},
			reasonSubstring: "RESULTS_VARIABLE STATUSES",
		},
		{
			// ERROR_VARIABLE-only shape: configure-time diagnostic
			// with no build-time analog.
			name: "opaque driver writing ERROR_VARIABLE",
			call: shadow.ExecuteProcessCall{
				Commands:      [][]string{{"random_tool"}},
				ErrorVariable: "STDERR",
			},
			reasonSubstring: "ERROR_VARIABLE STDERR",
		},
		{
			// INPUT_FILE with no output side: configure-time
			// consumer with no liftable signature.
			name: "opaque driver reading INPUT_FILE",
			call: shadow.ExecuteProcessCall{
				Commands:  [][]string{{"random_tool"}},
				InputFile: "input.txt",
			},
			reasonSubstring: "INPUT_FILE input.txt",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Classify(c.call)
			if got.Bucket != BucketRefuse {
				t.Errorf("bucket: got %q want %q", got.Bucket, BucketRefuse)
			}
			if got.Reason == "" {
				t.Errorf("reason: empty (every refusal should carry one)")
			}
			if !strings.Contains(got.Reason, c.reasonSubstring) {
				t.Errorf("reason %q does not contain %q", got.Reason, c.reasonSubstring)
			}
			if strings.Contains(got.Reason, "no recognized lift pattern") {
				t.Errorf("Phase 4 retired the catch-all 'no recognized lift pattern' string; got %q", got.Reason)
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
