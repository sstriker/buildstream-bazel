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

// stampDrivers names argv[0] basenames whose presence
// classifies the call as Stamp regardless of how the output
// is captured. VCS query tools have no legitimate
// code-generation use; hoisting them to a build-time genrule
// would run the VCS tool on the executor and re-introduce
// the same non-hermeticity the refusal is meant to prevent.
// Driver name is the gate, not OutputVariable / OutputFile
// presence.
var stampDrivers = map[string]bool{
	"git": true,
	"hg":  true,
	"svn": true,
}

// strongProbeDrivers names argv[0] basenames whose presence
// classifies the call as Probe regardless of how the output
// is captured. These tools are unambiguously host/toolchain
// probes (uname / hostname / sw_vers / lsb_release etc.);
// they don't generate files. Hoisting them to a build-time
// genrule would re-introduce host-environment leakage.
var strongProbeDrivers = map[string]bool{
	"uname":       true,
	"hostname":    true,
	"sw_vers":     true,
	"lsb_release": true,
}

// dualUseProbeDrivers names argv[0] basenames that CAN be
// host probes but are also legitimate code-generation tools.
// `python3 gen.py ... > out.h` is code generation;
// `python3 -c "import sys; print(sys.version_info[0])"` is a
// probe. Compilers and pkg-config are similar: probe shape
// when capturing toolchain info via OUTPUT_VARIABLE; not
// probe when the call's purpose is to invoke them as build
// tools.
//
// These drivers classify as Probe only when the call shape
// disambiguates (OUTPUT_VARIABLE set without OUTPUT_FILE);
// shapes with OUTPUT_FILE fall through to FileProducing
// (the lifter then hoists the call to a build-time genrule).
var dualUseProbeDrivers = map[string]bool{
	"gcc":        true,
	"g++":        true,
	"clang":      true,
	"clang++":    true,
	"ld":         true,
	"pkg-config": true,
	"python":     true,
	"python3":    true,
	"perl":       true,
	"ruby":       true,
	"node":       true,
}

// Classify maps one execute_process call to a Bucket using
// argv-only heuristics — no subprocess execution, no
// filesystem access. Order of checks:
//
//  1. No / multi-COMMAND clauses → Unknown.
//  2. cmake -E builtin recognition wins over everything else,
//     even when the call also sets OutputFile (e.g. `cmake -E
//     touch <path>`).
//  3. Stamp / probe DRIVER recognition wins over file-producing
//     classification. A `git rev-parse > out.txt` shape
//     classifies as Stamp, not FileProducing, because hoisting
//     it to a build-time genrule would run the VCS tool on the
//     executor and re-introduce the same non-hermeticity the
//     refusal is meant to prevent. Driver-first classification
//     closes the hole the earlier OutputFile-gated shape left.
//  4. OUTPUT_FILE alone → FileProducing (the call's purpose is
//     "produce this file at configure time"; hoist to a
//     build-time genrule).
//  5. Otherwise → Unknown.
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

	// Stamp / strong-probe drivers: classification is
	// driver-first regardless of how the call captures
	// output. These tools have no legitimate code-generation
	// use; hoisting them to a build-time genrule would
	// re-introduce the non-hermeticity the refusal is meant to
	// prevent. Diagnostic context (OutputVariable / OutputFile)
	// threads into the reason so operators see the full
	// shape; classification doesn't pivot on it.
	if stampDrivers[driver] {
		return ClassifyResult{
			Bucket: BucketStamp,
			Reason: driver + " is a version-control driver" + outputContext(call),
		}
	}
	if strongProbeDrivers[driver] {
		return ClassifyResult{
			Bucket: BucketProbe,
			Reason: driver + " is a host probe driver" + outputContext(call),
		}
	}
	// Dual-use probe drivers: classify as Probe only when the
	// call shape unambiguously matches the probe pattern
	// (OUTPUT_VARIABLE without OUTPUT_FILE). Calls with
	// OUTPUT_FILE fall through to the FileProducing case
	// below — `python3 gen.py spec.txt OUTPUT_FILE generated.h`
	// is code generation, not a probe, and should hoist to a
	// build-time genrule.
	if dualUseProbeDrivers[driver] && call.OutputVariable != "" && call.OutputFile == "" {
		return ClassifyResult{
			Bucket: BucketProbe,
			Reason: driver + " writing OUTPUT_VARIABLE looks like a host/toolchain probe",
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

// outputContext renders the call's writeback channels (
// OutputVariable, OutputFile) as a parenthesised suffix for
// classifier reason messages. Threads diagnostic context
// into stamp / strong-probe refusal reasons without
// re-implementing the formatting at each call site.
func outputContext(call shadow.ExecuteProcessCall) string {
	switch {
	case call.OutputVariable != "" && call.OutputFile != "":
		return " writing OUTPUT_VARIABLE " + call.OutputVariable + " with OUTPUT_FILE " + call.OutputFile
	case call.OutputVariable != "":
		return " writing OUTPUT_VARIABLE " + call.OutputVariable
	case call.OutputFile != "":
		return " with OUTPUT_FILE " + call.OutputFile
	}
	return ""
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
