package lower

import (
	"path/filepath"
	"strings"

	"github.com/sstriker/cmake-to-bazel/internal/shadow"
)

// Bucket is the per-execute_process classification label that
// drives the lifter's per-call decision: which calls translate
// to a Bazel genrule (CMakeE, FileProducing) vs which fail
// Tier-1 with unsupported-execute-process (Stamp, Probe,
// Unknown). The string values match the Reason facet emitted
// in failure.json so orchestrator triage can dedupe by bucket.
type Bucket string

const (
	// BucketCMakeE marks calls whose first COMMAND clause
	// invokes one of cmake's portable -E builtins from a
	// recognized op set. cmake's -E ops are deterministic file
	// operations (copy, touch, make_directory, etc.) with known
	// IO contracts, so the lifter can translate them to native
	// Bazel idioms without losing semantics.
	BucketCMakeE Bucket = "cmake-e"

	// BucketFileProducing marks calls that declare an
	// OUTPUT_FILE redirect — a strong signal the call's purpose
	// is "produce this file at configure time." The lifter
	// hoists these to build-time genrules with the original
	// argv as cmd, OutputFile as $@. The hoist moves work from
	// configure-time to build-time, which is a behaviour
	// change — the lifter tags the genrule
	// cmake-codegen-execute-process-hoisted so downstream
	// audits can flag the move for reviewer attention.
	BucketFileProducing Bucket = "file-producing"

	// BucketStamp marks calls that look like a version-stamp
	// query (git/hg/svn rev-parse / describe etc.) writing
	// back to OutputVariable. The textbook Bazel analog is a
	// repository_ctx.execute() repo rule producing a generated
	// .bzl table — that infrastructure isn't built yet, so
	// v1 refuses these with a typed Tier-1 failure pointing
	// owners at the design recipe.
	BucketStamp Bucket = "stamp"

	// BucketProbe marks calls that look like a host/toolchain
	// probe (uname, gcc --version, pkg-config --modversion etc.)
	// writing back to OutputVariable. These should fold into
	// select() over @platforms config_settings; v1 refuses
	// them rather than guessing at the right platform mapping.
	BucketProbe Bucket = "probe"

	// BucketUnknown is the fall-through bucket for calls that
	// don't match any recognized pattern: multi-COMMAND
	// pipelines, opaque shell scripts, side-effect-rich
	// invocations. v1 refuses them with a typed Tier-1
	// failure carrying the captured argv so owners can decide
	// per-call whether to rework the CMakeLists.txt or accept
	// the round-2 fallback (Phase B).
	BucketUnknown Bucket = "unknown"
)

// ClassifyResult is the typed verdict returned by Classify for
// one execute_process call. Reason is a short human-readable
// phrase paired with Bucket for failure.json triage and
// genrule tag suffixes; CMakeEOp is set only when
// Bucket==BucketCMakeE and carries the canonical lowercase op
// name (copy / touch / etc.) the lifter will translate.
type ClassifyResult struct {
	Bucket   Bucket
	Reason   string
	CMakeEOp string
}

// supportedCMakeEOps is the v1 allow-list of cmake -E
// operations the lifter knows how to translate to a Bazel
// genrule with declared inputs and outputs. Entries map the op
// name to a one-line description used in the genrule comment
// (and for failure-report context when an op isn't supported).
//
// v1 starts intentionally small; widening the set is a
// follow-on commit that adds the per-op translator + a
// fixture. cmake-internal trace events (which can use any op)
// are filtered upstream by the inSourceTree gate, so the
// allow-list only has to cover what real-world projects put in
// their CMakeLists.txt.
var supportedCMakeEOps = map[string]string{
	"copy":              "copy a single file",
	"copy_if_different": "copy a single file (no-op if dst is byte-identical)",
	"touch":             "create an empty file",
}

// stampDrivers names argv[0] basenames whose presence with an
// OutputVariable writeback (and no OutputFile redirect)
// signals a version-stamp probe. Version stamps need a repo
// rule (loading-time) to be sound under Bazel's
// analysis-then-action model; v1 refuses them rather than
// emitting a non-hermetic build-time genrule that runs git on
// the executor.
var stampDrivers = map[string]bool{
	"git": true,
	"hg":  true,
	"svn": true,
}

// probeDrivers names argv[0] basenames whose presence with an
// OutputVariable writeback (and no OutputFile redirect)
// signals a host/toolchain probe. The right Bazel translation
// is a select() over config_setting (or in extreme cases a
// repo rule for one-shot toolchain detection); v1 refuses
// them rather than guessing.
var probeDrivers = map[string]bool{
	"uname":       true,
	"hostname":    true,
	"gcc":         true,
	"g++":         true,
	"clang":       true,
	"clang++":     true,
	"ld":          true,
	"pkg-config":  true,
	"python":      true,
	"python3":     true,
	"perl":        true,
	"ruby":        true,
	"node":        true,
	"sw_vers":     true,
	"lsb_release": true,
}

// Classify maps one execute_process call to a Bucket using
// argv-only heuristics — no subprocess execution, no
// filesystem access. The order of checks matters: pipelines
// short-circuit to Unknown before any single-COMMAND shape
// recognition; CMakeE wins over FileProducing for `cmake -E
// touch` even though touch declares an output file; stamp /
// probe pattern detection precedes the generic
// FileProducing fallback so a `git rev-parse > out` pattern
// (stamp via OutputFile) classifies as Stamp rather than
// FileProducing — those still need the repo-rule analog and
// a Bazel-time genrule running git would re-introduce the
// hermeticity violation we're trying to remove.
func Classify(call shadow.ExecuteProcessCall) ClassifyResult {
	if len(call.Commands) == 0 {
		return ClassifyResult{
			Bucket: BucketUnknown,
			Reason: "no COMMAND clause",
		}
	}
	if len(call.Commands) > 1 {
		return ClassifyResult{
			Bucket: BucketUnknown,
			Reason: "multi-COMMAND pipeline (concurrent stages with stdout chaining)",
		}
	}

	argv := call.Commands[0]
	if len(argv) == 0 {
		return ClassifyResult{
			Bucket: BucketUnknown,
			Reason: "empty COMMAND argv",
		}
	}

	driver := executeProcessDriverBasename(argv[0])

	// cmake -E builtin recognition first — overrides stamp /
	// probe / file-producing patterns even if the call also
	// happens to set OutputFile.
	if isCMakeDriver(argv[0]) && len(argv) >= 2 && argv[1] == "-E" {
		if len(argv) < 3 {
			return ClassifyResult{
				Bucket: BucketUnknown,
				Reason: "cmake -E without an operation",
			}
		}
		op := strings.ToLower(argv[2])
		if _, ok := supportedCMakeEOps[op]; ok {
			return ClassifyResult{
				Bucket:   BucketCMakeE,
				Reason:   "cmake -E " + op,
				CMakeEOp: op,
			}
		}
		return ClassifyResult{
			Bucket: BucketUnknown,
			Reason: "cmake -E " + op + " is not in the v1 supported-op set",
		}
	}

	// Version-stamp / probe detection. Both require a writeback
	// variable (OutputVariable) and no OutputFile redirect —
	// the file-producing form of `git describe > out.txt` is
	// rare and still needs the repo-rule analog, so it falls
	// through to Stamp here rather than FileProducing.
	if call.OutputVariable != "" && call.OutputFile == "" {
		if stampDrivers[driver] {
			return ClassifyResult{
				Bucket: BucketStamp,
				Reason: driver + " writing OUTPUT_VARIABLE looks like a version stamp",
			}
		}
		if probeDrivers[driver] {
			return ClassifyResult{
				Bucket: BucketProbe,
				Reason: driver + " writing OUTPUT_VARIABLE looks like a host/toolchain probe",
			}
		}
	}

	// File-producing fallback. OUTPUT_FILE is the strong
	// signal; without it we have no idea what the call did and
	// can't lift it.
	if call.OutputFile != "" {
		return ClassifyResult{
			Bucket: BucketFileProducing,
			Reason: "OUTPUT_FILE declared (" + call.OutputFile + ")",
		}
	}

	return ClassifyResult{
		Bucket: BucketUnknown,
		Reason: "no recognized lift pattern",
	}
}

// isCMakeDriver reports whether argv[0] names cmake itself —
// the literal string "cmake", a path ending in /cmake, or the
// trace-recorded ${CMAKE_COMMAND} marker that occasionally
// survives variable expansion verbatim.
func isCMakeDriver(arg0 string) bool {
	if arg0 == "${CMAKE_COMMAND}" || arg0 == "$(CMAKE_COMMAND)" {
		return true
	}
	base := executeProcessDriverBasename(arg0)
	return base == "cmake"
}

// executeProcessDriverBasename returns the canonical basename
// of an argv[0] for driver-pattern matching. Strips any path
// component, leaves the basename in its original case (cmake's
// portable filename casing). Pure string ops; no filesystem
// access.
func executeProcessDriverBasename(arg0 string) string {
	if arg0 == "" {
		return ""
	}
	return filepath.Base(arg0)
}
