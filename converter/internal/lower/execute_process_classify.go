package lower

import (
	"sort"
	"strings"

	"github.com/sstriker/buildstream-bazel/internal/shadow"
)

// Bucket is the per-execute_process classification label that
// drives the lifter's per-call decision: which calls translate
// to a Bazel genrule (CMakeE, FileProducing) vs which fail
// Tier-1 with unsupported-execute-process (Stamp, Probe,
// Refuse). The string values match the Reason facet emitted
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

	// BucketRefuse is the typed-refusal bucket: calls whose
	// shape is recognized as unliftable for a specific reason
	// (multi-COMMAND pipeline, malformed argv, unsupported
	// cmake -E op, opaque non-builtin driver without
	// OUTPUT_FILE, etc.). Phase 4 collapsed the historical
	// BucketUnknown fall-through into this typed bucket; every
	// refusal now carries a specific Reason naming the
	// structural feature that prevents lifting, not a catch-all
	// "no recognized lift pattern". The bucket value retains
	// the historical `unknown` string for failure.json triage
	// continuity — orchestrator dedup keys built against the
	// pre-Phase-4 schema continue to match unchanged.
	BucketRefuse Bucket = "unknown"

	// BucketUnknown is the pre-Phase-4 alias for BucketRefuse.
	// Kept as an alias so external orchestrators referencing the
	// constant by name keep building; new call sites prefer
	// BucketRefuse for clarity.
	BucketUnknown = BucketRefuse
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
// name to a one-line description; the description is surfaced
// in the unsupported-op refusal reason so operators see at a
// glance which ops the v1 lifter covers and what they do.
//
// v1 starts intentionally small; widening the set is a
// follow-on commit that adds the per-op translator + a
// fixture. cmake-internal trace events (which can use any op)
// are filtered upstream by the inSourceTree gate, so the
// allow-list only has to cover what real-world projects put in
// their CMakeLists.txt.
var supportedCMakeEOps = map[string]string{
	"copy":                        "copy a single file",
	"copy_if_different":           "copy a single file (no-op if dst is byte-identical)",
	"copy_directory":              "copy a directory's contents recursively",
	"copy_directory_if_different": "copy a directory's contents recursively (skip byte-identical files)",
	"create_symlink":              "create a symlink (lifted as a copy under Bazel's hermetic action model — same path semantics, no symlink/copy distinction at action time)",
	"rename":                      "rename a file/directory (lifted as a copy; the source-side removal has no hermetic analog)",
	"touch":                       "create an empty file",
	"configure_file":              "@VAR@/${VAR}/#cmakedefine substitution from input template",
	"make_directory":              "create a directory (benign no-op — no Bazel output to anchor)",
	"remove":                      "delete files (benign no-op — fresh sandbox per action)",
	"remove_directory":            "delete a directory (benign no-op — fresh sandbox per action)",
}

// noopExecuteProcessOps names the CMakeEOp values that recover to a
// benign no-op: a configure-time filesystem side-effect that produces
// no consumable Bazel output to anchor a genrule on. `make_directory`
// / `mkdir` create a directory, but a bare directory isn't a genrule
// `out` — the files later written into it are recovered by their own
// calls, each of which already `mkdir -p "$(dirname "$@")"`. `remove`
// / `remove_directory` / `rm` / `rmdir` delete files, which has no
// build-time analog (every Bazel action runs in a fresh sandbox). The
// lifter skips these (no genrule, no refusal) rather than dropping the
// whole element into the round-2 fallback over a side-effect that
// can't lose a real compile input.
var noopExecuteProcessOps = map[string]bool{
	"make_directory":   true,
	"remove":           true,
	"remove_directory": true,
	"mkdir":            true,
	"rm":               true,
	"rmdir":            true,
}

// supportedCMakeEOpsList renders the allow-list as a stable,
// human-readable string for the unsupported-op refusal reason:
// `copy (copy a single file), copy_if_different (...), touch
// (...)`. Sort keeps the order stable across runs (Go map
// iteration is randomized).
func supportedCMakeEOpsList() string {
	keys := make([]string, 0, len(supportedCMakeEOps))
	for k := range supportedCMakeEOps {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = k + " (" + supportedCMakeEOps[k] + ")"
	}
	return strings.Join(parts, ", ")
}

// copyDrivers names argv[0] basenames the lifter reproduces as
// a copy genrule. v1 covers POSIX `cp`; the lifter (liftCp)
// decides file-vs-directory and symlink-deref from the on-disk
// source. Kept a map (not a bare ==) so widening to `install -m`
// / `rsync` shapes later is a one-line addition.
var copyDrivers = map[string]bool{
	"cp": true,
}

// touchDrivers names argv[0] basenames the lifter reproduces as a
// touch genrule. Raw POSIX `touch` is the exact analog of
// `cmake -E touch`, which we already lift (liftCMakeETouch) — so a
// configure-time `execute_process(COMMAND touch <marker>)` recovers
// to the same empty-file genrule instead of hard-failing the element,
// the same move the raw-`cp` lift makes for `cmake -E copy`. Kept a
// map (like copyDrivers) so a sibling driver is a one-line addition.
var touchDrivers = map[string]bool{
	"touch": true,
}

// symlinkDrivers names argv[0] basenames the lifter reproduces as a
// copy genrule. Raw POSIX `ln` (with or without -s) is the analog of
// `cmake -E create_symlink`, which we already lift as a copy under
// Bazel's hermetic action model — consumers read bytes by path, so
// the link-vs-copy distinction is meaningless at action time. The
// lifter (liftLn) reuses that same create_symlink copy path.
var symlinkDrivers = map[string]bool{
	"ln": true,
}

// renameDrivers names argv[0] basenames the lifter reproduces as a
// copy genrule. Raw POSIX `mv` is the analog of `cmake -E rename`:
// the destination ends up holding the source's bytes, and the
// source-side removal is a configure-time side-effect with no hermetic
// analog (so we copy rather than move). The lifter (liftRenameLike)
// is shared with `cmake -E rename`.
var renameDrivers = map[string]bool{
	"mv": true,
}

// noopDrivers names argv[0] basenames whose raw form is a filesystem
// side-effect with no consumable Bazel output — the raw analogs of the
// no-op `cmake -E` ops (see noopExecuteProcessOps). Classified as
// cmake-e so the lifter can skip them benignly instead of refusing and
// dropping the element into the round-2 fallback.
var noopDrivers = map[string]bool{
	"mkdir": true,
	"rm":    true,
	"rmdir": true,
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
	"cc":         true,
	"c++":        true,
	"ld":         true,
	"ar":         true,
	"ranlib":     true,
	"ninja":      true,
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
//  1. No / multi-COMMAND clauses → Refuse with a specific
//     structural reason (no-COMMAND / multi-COMMAND pipeline /
//     empty-argv).
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
//  5. Otherwise → Refuse with a reason that names exactly which
//     lift signal is missing (no OUTPUT_FILE, opaque driver,
//     no captured output channel). Phase 4 collapsed the
//     historical catch-all "no recognized lift pattern" string
//     into per-shape diagnoses so operators see the structural
//     feature blocking the lift, not a black-box refusal.
func Classify(call shadow.ExecuteProcessCall) ClassifyResult {
	if len(call.Commands) == 0 {
		return ClassifyResult{
			Bucket: BucketRefuse,
			Reason: "no COMMAND clause",
		}
	}
	if len(call.Commands) > 1 {
		return ClassifyResult{
			Bucket: BucketRefuse,
			Reason: "multi-COMMAND pipeline (concurrent stages with stdout chaining)",
		}
	}

	argv := call.Commands[0]
	if len(argv) == 0 {
		return ClassifyResult{
			Bucket: BucketRefuse,
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
				Bucket: BucketRefuse,
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
			Bucket: BucketRefuse,
			Reason: "cmake -E " + op + " is not in the v1 supported-op set (supported: " + supportedCMakeEOpsList() + ")",
		}
	}

	// Raw `cp` (POSIX copy) is lifted as a copy, mirroring how
	// `cmake -E copy` is already lifted. The classifier can't
	// prove a cp is build-irrelevant from argv alone — `cp
	// generated.h ${BINARY}/include/` is load-bearing — so
	// reproducing the copy as a genrule is SOUND, whereas
	// skipping it would risk dropping a real compile input.
	//
	// argv-only here: file-vs-directory and symlink-deref
	// decisions need the on-disk source and belong in the lifter
	// (liftCp), which HAS filesystem access. OUTPUT_VARIABLE /
	// RESULT_VARIABLE-bearing cp calls still classify as copy —
	// the copy happens regardless; any captured exit/var flows
	// through the existing dump-vars rescue.
	if copyDrivers[driver] {
		return ClassifyResult{
			Bucket:   BucketCMakeE,
			Reason:   "cp (POSIX copy)",
			CMakeEOp: "cp",
		}
	}

	// Raw `touch` is the POSIX analog of `cmake -E touch` (already
	// lifted). A configure-time marker-file write recovers to the
	// same empty-file genrule rather than refusing. argv-only here;
	// refusing semantics-changing touch flags (`-c`, `-r`, ...) is
	// the lifter's job (liftTouch).
	if touchDrivers[driver] {
		return ClassifyResult{
			Bucket:   BucketCMakeE,
			Reason:   "touch (POSIX file touch)",
			CMakeEOp: "touch_raw",
		}
	}

	// Raw `ln` is the POSIX analog of `cmake -E create_symlink`
	// (already lifted as a copy). Reproducing the link as a copy of
	// the target's bytes at the link path is sound under Bazel's
	// hermetic action model. argv-only here; flag/operand parsing is
	// the lifter's job (liftLn).
	if symlinkDrivers[driver] {
		return ClassifyResult{
			Bucket:   BucketCMakeE,
			Reason:   "ln (POSIX link)",
			CMakeEOp: "ln",
		}
	}

	// Raw `mv` is the POSIX analog of `cmake -E rename` (lifted as a
	// copy). argv-only here; operand parsing + file-vs-directory is
	// the lifter's job (liftRenameLike).
	if renameDrivers[driver] {
		return ClassifyResult{
			Bucket:   BucketCMakeE,
			Reason:   "mv (POSIX rename)",
			CMakeEOp: "mv",
		}
	}

	// Raw `mkdir` / `rm` / `rmdir` are filesystem side-effects with no
	// consumable Bazel output — the raw analogs of the no-op cmake -E
	// ops. Classify as cmake-e so the lifter skips them benignly
	// (no genrule, no refusal) rather than dropping the element into
	// the round-2 fallback over a side-effect that can't lose a real
	// compile input.
	if noopDrivers[driver] {
		return ClassifyResult{
			Bucket:   BucketCMakeE,
			Reason:   driver + " (POSIX filesystem side-effect with no Bazel output)",
			CMakeEOp: driver,
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
	// (OUTPUT_VARIABLE or RESULT_VARIABLE writeback, without
	// OUTPUT_FILE). Calls with OUTPUT_FILE fall through to the
	// FileProducing case below — `python3 gen.py spec.txt
	// OUTPUT_FILE generated.h` is code generation, not a probe.
	//
	// RESULT_VARIABLE-only shape is the canonical "does this
	// thing exist?" probe (e.g. `execute_process(COMMAND python3
	// -c "import pygments" RESULT_VARIABLE _r OUTPUT_QUIET
	// ERROR_QUIET)` — exit status is the answer). Without
	// RESULT_VARIABLE in the gate, those fall through to Unknown
	// and refuse Tier-1 unnecessarily.
	if dualUseProbeDrivers[driver] && call.OutputFile == "" &&
		(call.OutputVariable != "" || call.ResultVariable != "") {
		channel := "OUTPUT_VARIABLE"
		if call.OutputVariable == "" {
			channel = "RESULT_VARIABLE"
		}
		return ClassifyResult{
			Bucket: BucketProbe,
			Reason: driver + " writing " + channel + " looks like a host/toolchain probe",
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

	// Fall-through: no recognized driver, no cmake -E builtin,
	// no OUTPUT_FILE. Phase 4 collapsed the historical
	// "no recognized lift pattern" catch-all into per-shape
	// diagnoses so the refusal reason names the structural
	// feature missing from the call — what operators need to
	// either change in the CMakeLists.txt to make the call
	// liftable, or what tells them the call genuinely can't
	// be expressed as a Bazel rule.
	return ClassifyResult{
		Bucket: BucketRefuse,
		Reason: unliftableShapeReason(driver, call),
	}
}

// unliftableShapeReason builds a structural diagnosis for a
// call that fell through every classifier arm: argv[0] isn't a
// cmake -E builtin, isn't in any stamp/probe driver set, and the
// call doesn't carry an OUTPUT_FILE redirect. The reason names
// exactly which lift signal is missing — operators see the
// structural feature they'd need to add (an OUTPUT_FILE redirect
// to hoist to a build-time genrule, an OUTPUT_VARIABLE to route
// through dump-vars, etc.) rather than a black-box "no
// recognized lift pattern" string.
//
// Driver is the canonical lower-case basename from
// executeProcessDriverBasename; empty when argv[0] itself was
// empty (defensive — Classify rejects len(argv)==0 above so this
// shouldn't fire, but the helper stays defined for it).
func unliftableShapeReason(driver string, call shadow.ExecuteProcessCall) string {
	// Build up the diagnosis from the call shape. The variants
	// distinguished:
	//   - Driver was empty (argv[0] was ""). Already screened above
	//     but threaded into the message defensively.
	//   - OutputVariable / ResultVariable / ResultsVariable /
	//     ErrorVariable: the call writes back to cmake-side state
	//     but isn't a recognized probe/stamp shape. The dump-vars
	//     path can sometimes rescue this (Phase 4); the refusal
	//     reason names the variable for operator triage.
	//   - InputFile: stdin redirect set but no output side — likely
	//     a configure-time consumer with no liftable byproducts.
	//   - Anything else: an opaque side-effect call (creates files
	//     ninja doesn't track, prints to stdout that the call
	//     discards, etc.). The most common shape under this arm.
	if driver == "" {
		return "argv[0] empty after normalisation; no driver to classify"
	}
	suffix := outputContext(call)
	switch {
	case call.OutputVariable != "":
		return driver + " writes OUTPUT_VARIABLE " + call.OutputVariable +
			" but isn't a recognized stamp/probe driver; lift requires either an OUTPUT_FILE redirect (for build-time hoist) or a dump-vars capture of the value"
	case call.ResultsVariable != "":
		return driver + " writes RESULTS_VARIABLE " + call.ResultsVariable +
			" (per-COMMAND exit codes); pipeline status capture has no Bazel analog"
	case call.ResultVariable != "":
		return driver + " writes RESULT_VARIABLE " + call.ResultVariable +
			" but isn't a recognized probe driver (exit-status-as-answer shape only covers known toolchain probes)"
	case call.ErrorVariable != "":
		return driver + " captures stderr into ERROR_VARIABLE " + call.ErrorVariable +
			" with no output channel; configure-time-only diagnostic with no build-time analog"
	case call.InputFile != "":
		return driver + " reads INPUT_FILE " + call.InputFile +
			" with no captured output channel; configure-time side-effect with no liftable signature"
	default:
		return driver + " has no captured output channel (no OUTPUT_FILE, no OUTPUT_VARIABLE, no RESULT_VARIABLE); opaque side-effect call cannot be lifted to a Bazel rule" + suffix
	}
}

// outputContext renders the call's writeback channels
// (OutputVariable, OutputFile) as a leading-space suffix for
// classifier reason messages — e.g. ` writing OUTPUT_VARIABLE
// GIT_SHA`, concatenated onto the bucket label so the final
// reason reads `git is a version-control driver writing
// OUTPUT_VARIABLE GIT_SHA`. Empty when neither channel is set.
// Threads diagnostic context into stamp / strong-probe refusal
// reasons without re-implementing the formatting at each call
// site.
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
// of an argv[0] for driver-pattern matching, normalised so
// the stamp / probe / cmake-driver maps don't have to carry
// per-platform variants:
//
//   - Strips any path component (`/usr/bin/cmake` →
//     `cmake`, `C:\Program Files\CMake\bin\cmake.exe` →
//     `cmake.exe`).
//   - Strips a trailing `.exe` (case-insensitive) so
//     Windows-style absolute paths classify the same as
//     POSIX bare names.
//   - Lowercases the result so case-insensitive filesystems
//     (Windows, macOS HFS+) classify identically to the
//     canonical lower-case driver-map keys.
//
// Pure string ops; no filesystem access.
func executeProcessDriverBasename(arg0 string) string {
	if arg0 == "" {
		return ""
	}
	// Handle Windows-style backslash separators in addition
	// to POSIX forward slashes. filepath.Base on a host
	// where filepath.Separator is '/' won't strip
	// `C:\foo\bar` — handle the cross-platform shape
	// explicitly.
	base := arg0
	if i := strings.LastIndexAny(base, `/\`); i >= 0 {
		base = base[i+1:]
	}
	if strings.HasSuffix(strings.ToLower(base), ".exe") {
		base = base[:len(base)-len(".exe")]
	}
	return strings.ToLower(base)
}
