package lower

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/sstriker/buildstream-bazel/converter/internal/failure"
	"github.com/sstriker/buildstream-bazel/converter/internal/fileapi"
	"github.com/sstriker/buildstream-bazel/converter/internal/todos"
	"github.com/sstriker/buildstream-bazel/converter/ir"
	"github.com/sstriker/buildstream-bazel/internal/configurefile"
	"github.com/sstriker/buildstream-bazel/internal/shadow"
)

// isExistingDir reports whether p is a directory on disk.
// Used by liftFileProducing to decide whether to wrap an
// argv element in $(location) (file) or leave it as a literal
// path (directory). On stat error (path doesn't exist on the
// recording machine, race, etc.) returns false so the caller
// falls through to the file-shaped $(location) wrap;
// downstream Bazel-time resolution will then either succeed
// or surface a clear "no such target" error.
func isExistingDir(p string) bool {
	info, err := os.Stat(p)
	if err != nil {
		return false
	}
	return info.IsDir()
}

// executeProcessOut is one recovered execute_process output:
// the build-dir-relative path the genrule writes the file at.
// lowerTarget walks this list (sister to configureFileOut) and
// attaches matching outputs to consuming targets via the same
// build-dir-include match that configure_file uses, so a target
// that target_include_directories'd
// `${CMAKE_CURRENT_BINARY_DIR}` and `#include`s a header an
// `execute_process(... OUTPUT_FILE generated.h)` call produces
// gets a real Bazel dep edge to the recovered genrule.
type executeProcessOut struct {
	RelOutput string
}

// recoverExecuteProcess walks the trace's execute_process
// calls, classifies each into a Bucket, and emits a Bazel
// genrule for the liftable buckets (cmake-e, file-producing).
// Unliftable buckets — plus lift attempts that fail their own
// preconditions (e.g. cmake -E copy with an unresolvable
// input path) — surface as a structured refusal slice the
// caller dispositions:
//
//   - Phase A behaviour (UnsupportedExecuteProcessFallback off):
//     ToIR turns the refusals into a single typed
//     `unsupported-execute-process` Tier-1 failure listing
//     every call's location, bucket, reason and argv.
//   - Phase B fallback (UnsupportedExecuteProcessFallback on):
//     ToIR ignores the refusals as errors and emits a
//     placeholder ir.Package whose targets are empty
//     cc_library / cc_binary / cc_library-interface stubs at
//     this stage (Step 2 / PR #97), one per non-UTILITY
//     codemodel target with public visibility, so downstream
//     label references resolve at analysis time. Step 2.5
//     (PR #98) extends the placeholder to wire those stubs
//     to the install root via per-target cc_import / sh_binary
//     reconstructed from Target.Install.Destinations +
//     NameOnDisk.
//
// Returning structured data here keeps the per-call detail
// available for either disposition. Callers that don't care
// about fallback ignore the slice and treat non-empty as an
// error condition (the legacy formatExecuteProcessFailure
// helper renders it the same way the failure message did
// before this refactor).
//
// Liftable buckets append to cc.Genrules (one ir.Target per
// recovered call) and register the output path in
// cc.OutToGenrule. The returned []executeProcessOut slice gives
// lowerTarget the per-call output rels needed for consumer
// attribution (target hdrs / srcs depending on extension), the
// same way configureFileOut does for configure_file. Unliftable
// buckets — and lift attempts that fail their own preconditions
// (e.g. cmake -E copy with an unresolvable input path) — fall
// through to the refusal aggregator returned alongside outs.
//
// Returns (outs, refusals) — never an error. The caller picks
// the disposition: Phase A callers run formatExecuteProcessFailure
// on a non-empty refusal slice to land the typed Tier-1 failure;
// Phase B callers (--unsupported-execute-process-fallback set)
// route the refusal slice into the placeholder ir.Package
// emitter instead.
// configureLogVars projects a configureLog event slice into the
// var → value map the rescue arm consults alongside cmakeVars.
// Each try_compile-v1 / try_run-v1 / find_package-v1 event whose
// payload binds a result variable shows up; the value is the
// exitCode-derived "1"/"0" for try_compile / try_run, or the
// resolved found-package outcome for find_package.
//
// try_compile / try_run record the result variable directly
// (BuildResult.Variable / RunResult.Variable). find_package-v1
// doesn't carry an explicit result-variable field — cmake's
// documented contract is that `find_package(<Pkg> ...)` sets
// `<Pkg>_FOUND` to a truthy / falsey value. We reconstruct that
// variable name from Found.Package and project the resolution as
// cmake's canonical boolean ("1" found / "0" not found), so a
// probe whose OUTPUT_VARIABLE is bound to a `<Pkg>_FOUND` outcome
// is rescued the same way a try_compile-keyed probe is.
//
// Phase 4 of the generator-parity uplift extended the dump-vars
// rescue to cover probes whose results land in cmake's cache via
// Check_* modules; Phase 2 adds the find_package leg here.
func configureLogVars(events []fileapi.Event) map[string]string {
	if len(events) == 0 {
		return nil
	}
	out := map[string]string{}
	for _, e := range events {
		switch e.Kind {
		case "try_compile-v1", "try_run-v1":
			if e.BuildResult != nil && e.BuildResult.Variable != "" {
				if e.BuildResult.ExitCode == 0 {
					out[e.BuildResult.Variable] = "1"
				} else {
					out[e.BuildResult.Variable] = "0"
				}
			}
			if e.RunResult != nil && e.RunResult.Variable != "" {
				if e.RunResult.ExitCode == 0 {
					out[e.RunResult.Variable] = "1"
				} else {
					out[e.RunResult.Variable] = "0"
				}
			}
		case "find_package-v1":
			// find_package(<Pkg>) sets `<Pkg>_FOUND`. The event's
			// Found payload carries the resolved package name and
			// the boolean outcome; bind `<Pkg>_FOUND` to cmake's
			// canonical "1"/"0" so a probe whose OUTPUT_VARIABLE is
			// that variable rescues. We require Found.Package so the
			// synthesised variable name is real; events that recorded
			// no package name (or the cmake 4.3 find-v1 scalar shape,
			// which carries a path rather than a package) contribute
			// nothing here.
			if e.Found != nil && e.Found.Package != "" {
				v := e.Found.Package + "_FOUND"
				if e.Found.IsFound {
					out[v] = "1"
				} else {
					out[v] = "0"
				}
			}
		}
	}
	return out
}

// execAnchors carries the four directory roots the execute_process
// lift relativizes cmake -E / cp / ln operands against. The "host"
// roots are the live trees on the convert host (where templates and
// produced files exist at convert time); the "recorded" roots are
// the trees as the File API reply captured them — equal to the host
// roots in production (recorder and converter run on one machine),
// distinct only for offline fixtures. A source operand resolves
// under a source root; a destination under a build root.
type execAnchors struct {
	hostSrcDir       string
	recordedSrcDir   string
	hostBuildDir     string
	recordedBuildDir string
}

func recoverExecuteProcess(calls []shadow.ExecuteProcessCall, hostSrcDir, recordedSrcDir, hostBuildDir, recordedBuildDir string, liftEnabled bool, cmakeVars map[string]string, forwardedStampVars map[string]bool, cc *codegenContext) ([]executeProcessOut, []executeProcessRefusal) {
	if len(calls) == 0 {
		return nil, nil
	}
	anc := execAnchors{hostSrcDir: hostSrcDir, recordedSrcDir: recordedSrcDir, hostBuildDir: hostBuildDir, recordedBuildDir: recordedBuildDir}
	prescanStampVars(calls, cc)
	// seenProbeFlags maps a lifted build-setting name to the cmake
	// variable that produced it. The same feature probe can recur in the
	// trace (configure re-evaluation) — the same variable lifts once.
	// Two DISTINCT variables that sanitize to the same Bazel name (a
	// case-only HAVE_ZLIB vs have_zlib) collide and refuse below, rather
	// than silently dropping a knob.
	seenProbeFlags := map[string]string{}
	var unsupported []executeProcessRefusal
	var outs []executeProcessOut
	collect := func(rels []string) {
		for _, rel := range rels {
			outs = append(outs, executeProcessOut{RelOutput: rel})
		}
	}
	// Cross-call linkage pre-pass for the unspecified-output lift: the
	// single-claim ambiguity rule needs every eligible call's directory
	// operands and stem claims in view before any per-call emission.
	unspec := planUnspecifiedOutputs(calls, anc, cc)
	for ci, call := range calls {
		// Dead-capture normalization BEFORE classification: a capture
		// keyword whose variable the configure never reads (the
		// silencing idiom) is treated as absent, so every classifier
		// and lifter below sees the liftable shape. The ORIGINAL
		// call's vars still record on any refusal — that's the sink
		// the driver's dead-capture pass feeds on.
		orig := call
		call = clearDeadCaptures(call, cc.DeadCaptureVars)
		v := Classify(call)
		switch v.Bucket {
		case BucketCMakeE:
			rels, reason, ok := liftCMakeE(call, v, anc, liftEnabled, cmakeVars, cc)
			if !ok {
				unsupported = append(unsupported, executeProcessRefusal{
					File:   call.File,
					Line:   call.Line,
					Bucket: v.Bucket,
					Reason: reason,
					Argv:   formatExecuteProcessArgv(call),
				})
				recordCaptureRefusal(orig, cc)
				continue
			}
			collect(rels)
		case BucketFileProducing:
			rels, reason, ok := liftFileProducing(call, anc, cc)
			if !ok {
				unsupported = append(unsupported, executeProcessRefusal{
					File:   call.File,
					Line:   call.Line,
					Bucket: v.Bucket,
					Reason: reason,
					Argv:   formatExecuteProcessArgv(call),
				})
				recordCaptureRefusal(orig, cc)
				continue
			}
			collect(rels)
		case BucketNestedCMake:
			// Nested cmake configure/build (the superbuild-at-configure
			// idiom): record the (src, build) pair for the driver's warm
			// second pass; the lift happens there (lowerNestedBuilds).
			// See nested_cmake.go.
			if ref := recoverNestedCMakeCall(call, anc, cc); ref != nil {
				unsupported = append(unsupported, *ref)
				recordCaptureRefusal(orig, cc)
			}
		default:
			// Argv-declared codegen rescue before the probe/stamp/refusal
			// dispatch: `tool <in…> <out…>` with the files in the argv lifts
			// to a multi-output genrule (see liftArgvFileProducing) — the
			// add_custom_command-equivalent contract recovered from the
			// configure's own on-disk evidence, no convert-time execution.
			if v.Bucket == BucketRefuse {
				if rels, lifted := liftArgvFileProducing(call, anc, cc); lifted {
					collect(rels)
					continue
				}
				// Unspecified-output rescue: outputs absent from the argv,
				// recovered declaratively from File-API demand + ninja
				// exclusion + argv linkage (dir-operand containment or
				// derived-name correlation) — see
				// execute_process_unspecified.go.
				if rels, lifted := liftUnspecifiedOutputs(ci, call, anc, cc, unspec); lifted {
					collect(rels)
					continue
				}
			}
			if ref := recoverProbeOrStampCall(call, v, callCaptureCleared(orig, call), cc, cmakeVars, forwardedStampVars, seenProbeFlags); ref != nil {
				unsupported = append(unsupported, *ref)
				recordCaptureRefusal(orig, cc)
			}
		}
	}
	sort.Slice(outs, func(i, j int) bool { return outs[i].RelOutput < outs[j].RelOutput })
	return outs, unsupported
}

// recoverProbeOrStampCall handles the default Classify bucket (configure-time
// probe / stamp / unrecognized) for one execute_process call. It records a
// feature-declaration probe as a bool_flag + config_setting pair and a stamp
// output variable as a workspace-status key (mutating cc / seenProbeFlags),
// then applies the stamp/probe capture gate. Returns a refusal to append when
// the call can't be rescued, or nil when it was lifted, recorded, or
// benign-skipped (every `continue` in the original inline switch default).
func recoverProbeOrStampCall(call shadow.ExecuteProcessCall, v ClassifyResult, captureCleared bool, cc *codegenContext, cmakeVars map[string]string, forwardedStampVars map[string]bool, seenProbeFlags map[string]string) *executeProcessRefusal {
	// Configure-time probes produce no file artifact a consumer
	// #includes, so none lifts to a genrule — and a recognized
	// host/toolchain probe (BucketProbe) is never a build INPUT.
	// Its build-affecting consequence is recovered independently:
	//   - a captured OUTPUT_/RESULT_VARIABLE value feeds a
	//     configure_file (@VAR@) / file(GENERATE) lift via Reply.Vars;
	//   - a host triple (uname, config.guess) lands in a generated
	//     config header (config.h, llvm-config.h) the converter
	//     recovers directly;
	//   - a tool capability (ar/ranlib -D) lands in the recovered
	//     compile flags.
	// So a probe is SKIPPED whether or not the dump-vars hook caught
	// its value — the operator endorsed host/toolchain probes as
	// benign-skippable. The lone probe shape that emits is a
	// feature-declaration probe: it lifts to an operator-overridable
	// bool_flag + config_setting (Bazel targets, still not a file
	// input). A STAMP differs and gates on capture below.
	if v.Bucket == BucketProbe && call.OutputFile == "" {
		// Feature probe -> declared build setting. A probe writing a
		// HAVE_X-style variable is a deferred declaration ("does the
		// host have X?"); the faithful Bazel shape is an
		// operator-overridable bool_flag + a select()-able
		// config_setting, not a refusal or a silent bake. The default
		// is derived per writeback channel (featureProbeDefault): a
		// RESULT_VARIABLE "0" (exit success) and a truthy
		// OUTPUT_VARIABLE stdout both mean "feature present" -> True,
		// else False (including uncaptured).
		if varName, fromResult := featureDeclarationProbeVar(call); varName != "" {
			flag := sanitizeBuildSettingName(varName)
			if prev, ok := seenProbeFlags[flag]; ok {
				if prev == varName {
					return nil // same probe recurred in the trace
				}
				// Distinct cmake variables collide on one Bazel target
				// name (e.g. case-only HAVE_ZLIB vs have_zlib). Refuse so
				// the operator disambiguates rather than silently losing
				// the second knob.
				return &executeProcessRefusal{
					File:   call.File,
					Line:   call.Line,
					Bucket: v.Bucket,
					Reason: fmt.Sprintf("feature probes %q and %q both lift to build setting %q", prev, varName, flag),
					Argv:   formatExecuteProcessArgv(call),
				}
			}
			seenProbeFlags[flag] = varName
			cc.Genrules = append(cc.Genrules,
				ir.Target{
					Name:            flag,
					Kind:            ir.KindBoolFlag,
					BoolFlagDefault: featureProbeDefault(cmakeVars[varName], fromResult),
					Tags:            []string{"cmake-codegen-probe-option"},
					Visibility:      publicVisibility(),
				},
				ir.Target{
					Name:               flag + "_enabled",
					Kind:               ir.KindConfigSetting,
					ConfigSettingFlag:  ":" + flag,
					ConfigSettingValue: "True",
					Visibility:         publicVisibility(),
				},
			)
			return nil
		}
		// Any other recognized probe with no file output: skip. Its
		// result is recovered independently (see above) and is never
		// a Bazel build input, so emitting nothing is faithful.
		return nil
	}
	// Record a stamp output variable for the configure_file lift:
	// a stamp's OUTPUT_VARIABLE (a git/hg/svn revision, a
	// whoami/id/hostid identity, or a `date` timestamp) re-reads from
	// the Bazel workspace status at build time, so a `@GIT_SHA@` /
	// `@BUILD_DATE@` header stays live instead of baking the
	// convert-time value. The key's prefix is driver-aware
	// (stampStatusKey): STABLE_ for identity/revision, VOLATILE_ for
	// `date`. Recorded regardless of the capture gate below — the lift
	// (which runs later over the same cc) consults cc.StampVars; the
	// stamp call itself still skips (captured) or refuses (not) here.
	if v.Bucket == BucketStamp && call.OutputVariable != "" {
		driver := executeProcessDriverBasename(call.Commands[0][0])
		cc.StampVars[call.OutputVariable] = stampStatusKey(call.OutputVariable, driver)
	}
	// Stamp / probe capture gate. A stamp's value (a VCS revision)
	// WOULD bake into the srckey of any configure_file that consumed
	// it — silently pinning the build to one commit — so unlike a probe
	// a stamp isn't skipped blindly. It rescues when that value is
	// reachable by a consuming configure_file: captured at top level by
	// dump-vars (OUTPUT_VARIABLE in cmakeVars), OR forwarded onward by a
	// recovered `set()` copy — including a helper function's
	// `set(${_var} "${out}" PARENT_SCOPE)` return, whose function-local
	// OUTPUT_VARIABLE the dump-vars top-level snapshot can't see but
	// whose value still reaches a captured var the configure_file reads
	// (git_describe()'s shape, as in SDL). A stamp whose OUTPUT_VARIABLE
	// is neither captured nor forwarded has no namespace path to a
	// consumer and stays refused so the operator opts into round-2 (the
	// synthetic single-pass fixtures, with no recovered set() copies,
	// model exactly that). A BucketProbe reaches here only when it set
	// OUTPUT_FILE; it follows the capture gate but not the stamp-only
	// forwarded rescue.
	if v.Bucket == BucketProbe || v.Bucket == BucketStamp {
		// Dead-capture skip: the call HAD capture channels, the
		// analysis proved every one unread, and nothing else (no
		// OUTPUT_FILE) is consumable — a silenced stamp writes a value
		// nowhere, so there is nothing to bake AND nothing to refuse.
		// Genuinely capture-less stamp calls (git fetch side effects)
		// keep refusing: captureCleared distinguishes them.
		if captureCleared && call.OutputVariable == "" && call.ResultVariable == "" && call.OutputFile == "" {
			return nil
		}
		if call.OutputVariable != "" {
			if _, ok := cmakeVars[call.OutputVariable]; ok {
				return nil
			}
			if v.Bucket == BucketStamp && forwardedStampVars[call.OutputVariable] {
				return nil
			}
		}
		if call.ResultVariable != "" {
			if _, ok := cmakeVars[call.ResultVariable]; ok {
				return nil
			}
		}
	}
	return &executeProcessRefusal{
		File:   call.File,
		Line:   call.Line,
		Bucket: v.Bucket,
		Reason: v.Reason,
		Argv:   formatExecuteProcessArgv(call),
	}
}

// prescanStampVars records every BucketStamp OUTPUT_VARIABLE into
// cc.StampVars BEFORE the main lift loop, so a `cmake -E configure_file`
// appearing EARLIER in the trace than its stamp call still wires
// stamp_values (the configure_file recovery gets trace-order independence
// for free by running after the whole execute_process walk; the -E lift
// runs inside it). The later in-loop write (recoverProbeOrStampCall) is
// idempotent — same key, same value. Classify runs twice per call;
// execute_process call counts are tiny. Known v1 limitation, shared with
// the in-loop recording: stamps reached only through a recovered set()
// copy (propagateStampVars, which runs after this recovery) don't wire
// into -E configure_file lifts.
func prescanStampVars(calls []shadow.ExecuteProcessCall, cc *codegenContext) {
	for _, call := range calls {
		// Dead-capture view, mirroring the main loop: a silenced
		// stamp's variable must not register (nothing consumes it).
		call = clearDeadCaptures(call, cc.DeadCaptureVars)
		v := Classify(call)
		if v.Bucket == BucketStamp && call.OutputVariable != "" && len(call.Commands) > 0 && len(call.Commands[0]) > 0 {
			driver := executeProcessDriverBasename(call.Commands[0][0])
			cc.StampVars[call.OutputVariable] = stampStatusKey(call.OutputVariable, driver)
		}
	}
}

// stampStatusKey derives the Bazel workspace-status key a stamp cmake
// variable reads from at build time. The remainder is the upper-cased
// variable name with any non-[A-Z0-9_] run folded to '_' so the key is a
// valid status identifier (GIT_SHA -> STABLE_GIT_SHA). The operator's
// --workspace_status_command emits this key; predictable derivation from
// the cmake var name keeps that contract self-documenting.
//
// The prefix is driver-aware:
//   - identity / revision drivers (git/hg/svn/whoami/id/hostid) -> STABLE_,
//     routing the key into stable-status.txt (ctx.info_file) — cache-keyed,
//     so a change correctly re-renders the consuming configure_file.
//   - `date` -> VOLATILE_, routing into volatile-status.txt
//     (ctx.version_file). A wall-clock timestamp must NOT be cache-keyed: a
//     STABLE_ key would bust the action cache every build. Bazel reads
//     volatile-status but doesn't cache-key it, so the timestamp changes per
//     build without forcing rebuilds.
func stampStatusKey(varName, driver string) string {
	prefix := "STABLE_"
	if driver == "date" {
		prefix = "VOLATILE_"
	}
	return statusKeyWithPrefix(prefix, varName)
}

// statusKeyWithPrefix builds a workspace-status key from a given prefix and a
// cmake variable name (the name upper-cased with any non-[A-Z0-9_] run folded
// to '_'). Shared by stampStatusKey (driver -> prefix) and the
// function-forward re-key (which preserves a known stamp's STABLE_/VOLATILE_
// prefix while restemming to the consumer variable's name).
func statusKeyWithPrefix(prefix, varName string) string {
	var b strings.Builder
	b.WriteString(prefix)
	for _, r := range strings.ToUpper(varName) {
		switch {
		case r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

// formatExecuteProcessFailure converts a non-empty refusal
// slice into the typed `unsupported-execute-process` Tier-1
// error. Used by ToIR when Phase B fallback is off (the
// default). Returns nil for an empty slice so callers can
// chain the result idiomatically.
func formatExecuteProcessFailure(refusals []executeProcessRefusal) error {
	if len(refusals) == 0 {
		return nil
	}
	return failure.New(failure.UnsupportedExecuteProcess,
		"%s", formatExecuteProcessRefusal(refusals))
}

// liftCMakeE translates a recognized cmake -E builtin call
// into a Bazel genrule and appends it to cc.Genrules. Returns
// (reason, false) when the lift can't proceed (e.g. an
// unresolvable input path) so the caller can fall back to
// refusal with a precise diagnostic instead of silently
// dropping the call.
//
// The genrule's cmd is intentionally written in plain shell
// rather than re-invoking cmake at action time: cmake itself
// isn't on the executor in a Bazel-9 + bb_clientd flow, and
// the -E builtins map cleanly to portable shell tools (touch,
// cp, mkdir, ln). cmake-codegen-cmake-e tag mirrors the
// existing add_custom_command lifter so audit queries can
// split the two cleanly even though they take different
// trace-vs-ninja paths to recover.
func liftCMakeE(call shadow.ExecuteProcessCall, v ClassifyResult, anc execAnchors, liftEnabled bool, cmakeVars map[string]string, cc *codegenContext) ([]string, string, bool) {
	argv := call.Commands[0] // single-COMMAND guaranteed by Classify
	// cmake -E <op> <args...>; argv[0]=cmake, argv[1]=-E, argv[2]=op.
	// Guard the slice: the raw-command buckets (cp / touch_raw / ln)
	// have no `-E <op>` prefix, so their argv can be shorter than 3
	// (e.g. `touch marker` is len 2). Those cases re-slice from
	// argv[1:] below; computing argv[3:] unconditionally would panic
	// on them. cmake -E ops always have len >= 3 (Classify refuses
	// `cmake -E` with no op), so args stays correct for them.
	var args []string
	if len(argv) >= 3 {
		args = argv[3:]
	}
	// Benign no-ops (cmake -E make_directory / remove / remove_directory
	// and the raw mkdir / rm / rmdir analogs): a filesystem side-effect
	// with no consumable Bazel output to anchor a genrule on. Skip
	// without emitting anything and without refusing — dropping the
	// element into the round-2 fallback over a side-effect that can't
	// lose a real compile input would be wrong. Checked before the
	// switch so both the cmake -E and raw spellings share one arm.
	if noopExecuteProcessOps[v.CMakeEOp] {
		return nil, "", true
	}
	switch v.CMakeEOp {
	case "touch":
		return liftCMakeETouch(args, anc, cc)
	case "copy", "copy_if_different":
		// cmake -E copy / copy_if_different copy a single FILE (the
		// directory form is copy_directory, handled below). Bazel
		// actions run hermetically; the action's output is the file
		// at <dst>.
		return liftCMakeECopy(v.CMakeEOp, args, anc, cc)
	case "create_symlink":
		// create_symlink shares the (src, dst) two-arg shape with
		// copy, but its target can be a FILE or a DIRECTORY (e.g.
		// `cmake -E create_symlink include build/include`). Bazel
		// actions run hermetically; the action's output is the
		// file(s) at <dst>. Whether the cmake-side path creates
		// that via cp or ln -sf is irrelevant to downstream
		// consumers — they read bytes by path. Lifting symlink as
		// copy preserves the dst-anchored path semantics and avoids
		// the subtle issues genrule outputs have with symlinks
		// (action-cache hashing, sandbox cleanup, cross-fs
		// handling). Routes through the file-or-dir dispatch so a
		// directory target recursively copies its contents rather
		// than emitting a broken `cp <dir>` on a single $(location).
		return liftCreateSymlinkLike("cmake -E create_symlink", "create_symlink", args, anc, cc)
	case "copy_directory", "copy_directory_if_different":
		// cmake -E copy_directory <src> <dst> copies the CONTENTS of
		// <src> into <dst> (no source-basename insertion, unlike
		// `cp -R`), so the dir-copy emit runs with an empty
		// sub-prefix. _if_different differs only in rerun-skip
		// semantics, which Bazel's fresh-sandbox actions make moot.
		return liftCMakeECopyDirectory(v.CMakeEOp, args, anc, cc)
	case "rename":
		// cmake -E rename <src> <dst>: dst holds src's bytes; the
		// source-side removal has no hermetic analog, so lift as a
		// copy (file or directory). Shared with raw `mv`.
		return liftRenameLike("cmake -E rename", "rename", args, anc, cc)
	case "mv":
		// Raw POSIX `mv` — the analog of `cmake -E rename`. argv is
		// `mv <flags...> <src> <dst>`, operands start at argv[1].
		return liftRenameLike("mv", "mv", argv[1:], anc, cc)
	case "configure_file":
		return liftCMakeEConfigureFile(args, anc, liftEnabled, cmakeVars, cc)
	case "cp":
		// Raw POSIX `cp` (issue #312). argv is `cp <flags...>
		// <src> <dst>` (no `-E <op>` prefix), so the operands
		// start at argv[1].
		return liftCp(argv[1:], anc, cc)
	case "install":
		// Raw POSIX `install` (issue #376): copy-with-attributes,
		// or a directory create with -d. argv is `install <flags...>
		// <src...> <dst>`, operands start at argv[1].
		return liftInstall(argv[1:], anc, cc)
	case "touch_raw":
		// Raw POSIX `touch` — the analog of `cmake -E touch`, reusing
		// liftCMakeETouch after flag-stripping. argv is `touch
		// <flags...> <path...>`, operands start at argv[1].
		return liftTouch(argv[1:], anc, cc)
	case "ln":
		// Raw POSIX `ln [-s]` — the analog of `cmake -E
		// create_symlink`, reusing the create_symlink copy path.
		// argv is `ln <flags...> <target> <linkname>`, operands
		// start at argv[1].
		return liftLn(argv[1:], anc, cc)
	}
	return nil, "internal: classified as cmake-e " + v.CMakeEOp + " but no lifter wired", false
}

// liftCMakeETouch translates `cmake -E touch <path> ...` into
// a genrule per output path. cmake's touch accepts multiple
// paths and emits each as a separate genrule (one Bazel rule
// per file output); a single genrule with multi-out would
// require the consumer to reference outputs by index, which
// downstream attribution doesn't model.
//
// touch with no args is rejected (refused with a diagnostic);
// a path outside the build dir is also refused — the converter
// can't anchor it as a Bazel output.
func liftCMakeETouch(paths []string, anc execAnchors, cc *codegenContext) ([]string, string, bool) {
	if len(paths) == 0 {
		return nil, "cmake -E touch with no arguments", false
	}
	rels := make([]string, 0, len(paths))
	for _, p := range paths {
		rel, ok := executeProcessAnchorOutput(p, anc)
		if !ok {
			return nil, fmt.Sprintf("cmake -E touch path %q is not under the build dir", p), false
		}
		rels = append(rels, rel)
		if _, exists := cc.OutToGenrule[rel]; exists {
			// Already recovered (e.g., the same call appears
			// multiple times in the trace from re-evaluation).
			continue
		}
		name := executeProcessGenruleName(rel)
		cc.Genrules = append(cc.Genrules, ir.Target{
			Name:        name,
			Kind:        ir.KindGenrule,
			GenruleCmd:  `mkdir -p "$$(dirname "$@")" && touch "$@"`,
			GenruleOuts: []string{rel},
			Tags:        cmakeETags("touch"),
			Visibility:  []string{"//visibility:private"},
		})
		cc.OutToGenrule[rel] = name
	}
	return rels, "", true
}

// liftCMakeECopy translates `cmake -E copy <src> <dst>` (and
// the byte-equal copy_if_different form, which differs only in
// rerun-skip semantics that don't apply to Bazel actions —
// every action gets a fresh sandbox dir).
//
// v1 supports the 2-arg form only. The N-src + 1-dst-dir form
// (`cmake -E copy a b c dst/`) is more involved (would emit one
// genrule per src-to-dst mapping) and rare in practice; refused
// with a diagnostic until a real fixture forces it.
//
// The src must resolve under the source root (so it's a real
// Bazel-tracked input) and the dst must resolve under the build
// dir (so it's a real Bazel output). Either anchor failure ends
// the lift with a descriptive reason — the caller falls back to
// refusal so the operator sees exactly which path didn't
// resolve.
func liftCMakeECopy(op string, args []string, anc execAnchors, cc *codegenContext) ([]string, string, bool) {
	if len(args) != 2 {
		return nil, fmt.Sprintf("cmake -E %s: v1 supports the 2-arg form only (got %d args)", op, len(args)), false
	}
	src, dst := args[0], args[1]
	// Fast path: a real source-tree input copied to a build-dir output is a
	// `cp` genrule (the file is a declared Bazel input).
	if _, ok := executeProcessAnchorSource(src, anc); ok {
		return emitCopyGenrule("cmake -E "+op, op, src, dst, anc, cc)
	}
	// The source isn't under the source root — it's a CONFIGURE-TIME build-dir
	// intermediate (LLVM's `Extension.def.tmp`, written by an earlier
	// file(WRITE)/configure step, then `copy_if_different`'d to the final
	// `Extension.def`). There's no Bazel input to `cp` from, but the final
	// OUTPUT exists on disk in the build dir — bake its bytes as a write_file,
	// exactly like a configure_file COPYONLY / file(RENAME) recovery. This is
	// the config.h-class generated-header materialization, generalized to the
	// `cmake -E copy[_if_different]` spelling.
	if baked, ok := bakeBuildDirCopyOutput(op, dst, anc, cc); ok {
		return baked, "", true
	}
	return nil, fmt.Sprintf("cmake -E %s: source %q is not under the source root", op, src), false
}

// bakeBuildDirCopyOutput recovers a `cmake -E copy[_if_different] <tmp> <dst>`
// whose SOURCE is a build-dir configure-time intermediate (not a source-tree
// input) by baking the on-disk bytes of <dst> as a write_file — the same move
// classifyFileRename makes for `file(RENAME)` and liftCMakeEConfigureFile makes
// for `cmake -E configure_file`. Returns (outs, true) when the dst anchors
// under the build dir and its bytes are readable; (nil, false) otherwise (no
// build-dir stash, dst outside the build dir, or output not on disk) so the
// caller falls back to the original source-not-under-source-root refusal.
func bakeBuildDirCopyOutput(op, dst string, anc execAnchors, cc *codegenContext) ([]string, bool) {
	if anc.hostBuildDir == "" {
		return nil, false
	}
	dstRel, ok := executeProcessAnchorOutput(dst, anc)
	if !ok {
		return nil, false
	}
	if _, exists := cc.OutToGenrule[dstRel]; exists {
		return []string{dstRel}, true
	}
	rendered, err := os.ReadFile(filepath.Join(anc.hostBuildDir, dstRel))
	if err != nil {
		return nil, false
	}
	name := executeProcessGenruleName(dstRel)
	cc.Genrules = append(cc.Genrules, bakeFileTarget(name, dstRel, rendered, cmakeETags(op)))
	cc.OutToGenrule[dstRel] = name
	return []string{dstRel}, true
}

// liftCreateSymlinkLike translates `cmake -E create_symlink <target>
// <linkname>` (args already past the `-E op` prefix) into a copy
// genrule, dispatching file-vs-directory on the on-disk target via
// emitCopyFileOrDir. Distinct from liftCMakeECopy because a symlink
// target — unlike `cmake -E copy`'s single-file contract — can be a
// directory (e.g. LLVM/VTK symlinking an `include` tree into the build
// dir), which must copy recursively.
func liftCreateSymlinkLike(what, op string, args []string, anc execAnchors, cc *codegenContext) ([]string, string, bool) {
	if len(args) != 2 {
		return nil, fmt.Sprintf("%s: v1 supports the 2-arg form only (got %d args)", what, len(args)), false
	}
	return emitSymlinkCopy(what, op, args[0], args[1], anc, cc)
}

// emitSymlinkCopy handles the symlink ops (cmake -E create_symlink /
// raw ln). It reproduces the link as a copy of the target's bytes via
// emitCopyFileOrDir — EXCEPT for the install-compat-alias shape, which
// it skips benignly.
//
// The alias shape: a symlink whose source can't anchor under the
// source root AND whose link can't anchor under the build dir. These
// are versioned install-compat aliases over build-generated files —
// libpng's `cmake -E create_symlink libpng16-config libpng-config` and
// `libpng16.pc -> libpng.pc`, the canonical `.so.N -> .so` /
// `-config` / `.pc` symlinks projects create at install time. With
// nothing anchorable on either side there is nothing for Bazel to
// track, the same "nothing to anchor" character as the make_directory
// / remove no-op family — so skip (no genrule, no refusal) rather than
// dropping the whole element into the round-2 fallback over a link with
// zero effect on the built artifact.
//
// The narrowness: a link that DOES anchor under the build dir is a
// potential real output (a build-generated header alias a later step
// #includes), so it still flows to emitCopyFileOrDir — which refuses if
// the source is unrecoverable. We don't silently drop a
// possibly-load-bearing symlink; only the anchors-nowhere alias skips.
func emitSymlinkCopy(what, op, src, dst string, anc execAnchors, cc *codegenContext) ([]string, string, bool) {
	if _, ok := executeProcessAnchorSource(src, anc); !ok {
		if _, ok := executeProcessAnchorOutput(dst, anc); !ok {
			// Install-compat alias (build-generated source, link not a
			// tracked build output): nothing to anchor — benign skip.
			return nil, "", true
		}
	}
	return emitCopyFileOrDir(what, op, src, dst, anc, cc)
}

// emitCopyGenrule anchors a (src, dst) pair and appends a single
// copy genrule. Shared by `cmake -E copy` / `copy_if_different` /
// `create_symlink` (via liftCMakeECopy) and raw `ln` (via liftLn) so
// the create_symlink-as-copy lift lives in exactly one place.
//
// `what` is the diagnostic prefix naming the original call shape
// (e.g. "cmake -E copy" or "ln") so a refusal points at what the
// operator actually wrote; `tagOp` is the op label that lands in the
// genrule's cmake-codegen-execute-process-op tag (the cmake -E op
// name for builtins, the raw driver basename for raw commands —
// matching the raw-`cp` precedent).
//
// src must resolve under the source root (so it's a real Bazel input)
// and dst under the build dir (so it's a real Bazel output). Either
// anchor failure ends the lift with a descriptive reason so the
// caller falls back to refusal naming the path that didn't resolve.
func emitCopyGenrule(what, tagOp, src, dst string, anc execAnchors, cc *codegenContext) ([]string, string, bool) {
	srcRel, ok := executeProcessAnchorSource(src, anc)
	if !ok {
		return nil, fmt.Sprintf("%s: source %q is not under the source root", what, src), false
	}
	dstRel, ok := executeProcessAnchorOutput(dst, anc)
	if !ok {
		return nil, fmt.Sprintf("%s: destination %q is not under the build dir", what, dst), false
	}
	if srcRel == dstRel {
		// Single-file copy/link whose source and destination resolve to the SAME
		// package-relative path — mbedtls's link_to_source(X) copies a generated
		// build file back to its own committed source path (error.c,
		// version_features.c). Emitting it would make X both an input and an
		// output, which Bazel forbids; and the copy is redundant — X already
		// exists as the committed source. Drop it AND surface no output: the
		// aliased path IS a committed source, so every consumer (the codemodel
		// target that compiles it included) already resolves to it — there is
		// nothing for the build-dir-include attribution to attach. Returning the
		// path here used to broadcast it into the srcs of EVERY target whose
		// includes cover the build root (mbedtls's three libs each gained all
		// four link_to_source files, over-exporting their symbols — the
		// symbol-fidelity lens's first true positive). Only fires on the genuine
		// in==out collision (which Bazel rejects regardless), so it can't
		// regress a building target. Scoped to the single-file path: a recursive
		// DIR copy (emitDirCopyGenrule) at the same relative root is a real
		// staging copy, not an identity.
		return nil, "", true
	}
	if _, exists := cc.OutToGenrule[dstRel]; exists {
		return []string{dstRel}, "", true
	}
	name := executeProcessGenruleName(dstRel)
	cc.Genrules = append(cc.Genrules, ir.Target{
		Name:        name,
		Kind:        ir.KindGenrule,
		Srcs:        []string{srcRel},
		GenruleCmd:  fmt.Sprintf(`mkdir -p "$$(dirname "$@")" && cp "$(location %s)" "$@"`, srcRel),
		GenruleOuts: []string{dstRel},
		Tags:        cmakeETags(tagOp),
		Visibility:  []string{"//visibility:private"},
	})
	cc.OutToGenrule[dstRel] = name
	return []string{dstRel}, "", true
}

// liftTouch translates a raw POSIX `touch <path> ...` execute_process
// call into one genrule per path, mirroring liftCMakeECopy's reuse of
// the cmake -E lifter for raw `cp`: raw `touch` is the POSIX
// equivalent of `cmake -E touch`, which we already lift, so after
// flag-stripping it routes straight to liftCMakeETouch (and shares its
// cmake-codegen-execute-process-op=touch tag — the operation is
// genuinely identical).
//
// args is everything after `touch` (flags + operands). touch flags
// change create / timestamp semantics — `-c` skips creating a missing
// file, `-r`/`-t`/`-d` only stamp times on an existing file — none of
// which map to "produce this file" under Bazel's hermetic action
// model. Any flag therefore refuses with a precise diagnostic rather
// than silently emitting a genrule whose output the original call
// might not have created. The flagless `touch <path...>` form (the
// overwhelming majority of configure-time marker writes) lifts.
func liftTouch(args []string, anc execAnchors, cc *codegenContext) ([]string, string, bool) {
	paths := make([]string, 0, len(args))
	for _, a := range args {
		if strings.HasPrefix(a, "-") && a != "-" {
			return nil, fmt.Sprintf("touch: flag %q changes create/timestamp semantics that don't map to a Bazel output; only the flagless `touch <path>` form lifts", a), false
		}
		paths = append(paths, a)
	}
	if len(paths) == 0 {
		return nil, "touch: no path operands", false
	}
	return liftCMakeETouch(paths, anc, cc)
}

// liftLn translates a raw POSIX `ln [-s] <target> <linkname>`
// execute_process call into a copy genrule, mirroring how
// `cmake -E create_symlink` is already lifted. Under Bazel's hermetic
// action model the link-vs-copy distinction is meaningless (consumers
// read bytes by path), so a symlink and a hardlink alike reproduce as
// a copy that materialises <linkname> with the target's bytes — the
// same reasoning liftCMakeECopy applies to create_symlink.
//
// args is everything after `ln` (flags + operands). Flags (`-s`,
// `-f`, `-n`, ...) are accepted and ignored: none of them change the
// "linkname holds target's bytes by path" outcome the copy
// reproduces. v1 supports exactly the 2-operand `<target> <linkname>`
// form; the 1-operand form (`ln -s target`, linkname defaults to the
// configure-time cwd basename — unanchorable) and the 3+-operand form
// (multiple links into a dir) refuse with a precise diagnostic.
func liftLn(args []string, anc execAnchors, cc *codegenContext) ([]string, string, bool) {
	operands := make([]string, 0, len(args))
	for _, a := range args {
		if strings.HasPrefix(a, "-") && a != "-" {
			continue
		}
		operands = append(operands, a)
	}
	if len(operands) != 2 {
		return nil, fmt.Sprintf("ln: v1 supports the 2-operand <target> <linkname> form only (got %d operands)", len(operands)), false
	}
	// op label "ln" tags the genrule with the raw driver name (the
	// raw-`cp` precedent), keeping raw-ln lifts distinguishable from
	// `cmake -E create_symlink` in audit queries. Routes through
	// emitSymlinkCopy: shares the create_symlink file-or-dir dispatch
	// (`ln -s` can target a directory) AND the install-compat-alias
	// benign skip (a versioned `.so.N` / `-config` alias over a
	// build-generated file that anchors nowhere).
	return emitSymlinkCopy("ln", "ln", operands[0], operands[1], anc, cc)
}

// liftCp translates a raw POSIX `cp` execute_process call into
// one or more copy genrules, mirroring liftCMakeECopy's shape.
// Unlike cmake -E copy, raw cp can copy a directory recursively,
// and its source may be a symlink — so liftCp consults the
// on-disk source root (hostSrcDir) to decide file-vs-directory
// and to dereference symlinks, work that the argv-only
// classifier intentionally leaves to the lifter.
//
// args is everything after `cp` (flags + operands). v1 supports
// exactly the 2-operand `<src> <dst>` form; 3+ operands
// (multi-src into a dir) refuse with a precise diagnostic so the
// caller never mis-lifts. recursive is inferred from -R / -r /
// -a (archive implies recursive); other flags are ignored.
//
// Anchoring: src must resolve under the source root (real Bazel
// input), dst under the build dir (real Bazel output). The
// on-disk source is stat'd through filepath.EvalSymlinks so the
// emitted Srcs point at the REAL files, not through a symlink
// (e.g. tests/types/data -> ../data resolves to tests/data/...).
//
// Returns (rels, "", true) on success; (nil, reason, false) on
// any refusal.
func liftCp(args []string, anc execAnchors, cc *codegenContext) ([]string, string, bool) {
	// Split flags (tokens starting with '-') from operands.
	var operands []string
	recursive := false
	for _, a := range args {
		if strings.HasPrefix(a, "-") && a != "-" {
			// -RauL, -r, -a, --recursive, etc. Recursion is
			// implied by R / r anywhere in a short-flag cluster,
			// or by archive mode (a).
			body := strings.TrimLeft(a, "-")
			if strings.ContainsAny(body, "Rra") || a == "--recursive" || a == "--archive" {
				recursive = true
			}
			continue
		}
		operands = append(operands, a)
	}
	if len(operands) != 2 {
		return nil, fmt.Sprintf("cp: v1 supports the 2-operand <src> <dst> form only (got %d operands)", len(operands)), false
	}
	src, dst := operands[0], operands[1]

	srcRel, ok := executeProcessAnchorSource(src, anc)
	if !ok {
		return nil, fmt.Sprintf("cp: source %q is not under the source root", src), false
	}
	dstRel, ok := executeProcessAnchorOutput(dst, anc)
	if !ok {
		return nil, fmt.Sprintf("cp: destination %q is not under the build dir", dst), false
	}
	// relativeIfInsideRelaxed maps the build dir itself to ".";
	// normalise that to "" so the file/dir branches treat the
	// build-dir root uniformly (outputs land directly under it).
	if dstRel == "." {
		dstRel = ""
	}

	// Resolve the on-disk source to decide file-vs-directory and
	// to dereference symlinks. EvalSymlinks failure (broken link,
	// race) falls back to the raw abs path so the os.Stat below
	// produces the authoritative refusal.
	abs := filepath.Join(anc.hostSrcDir, srcRel)
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		real = abs
	}
	info, err := os.Stat(real)
	if err != nil {
		return nil, fmt.Sprintf("cp: source %q does not exist on disk", src), false
	}

	// Re-anchor the deref'd real path back under the source root
	// so emitted Srcs point at the real files, not through the
	// symlink. Fall back to srcRel when the real path escapes the
	// root (defensive — a symlink pointing outside the tree).
	realSrcRel, ok := executeProcessAnchorSource(real, anc)
	if !ok {
		realSrcRel = srcRel
	}

	if !info.IsDir() {
		return liftCpFile(realSrcRel, dst, dstRel, cc)
	}
	// Directory source. POSIX `cp` without -R/-r/-a fails on a
	// directory; reproducing it would emit a genrule that copies
	// nothing useful, so refuse with a diagnostic that names the
	// missing flag rather than silently emitting an empty rule.
	if !recursive {
		return nil, fmt.Sprintf("cp: source %q is a directory but no recursive flag (-R/-r/-a) was given", src), false
	}
	return liftCpDir(real, srcRel, realSrcRel, dstRel, anc, cc)
}

// liftInstall reproduces a POSIX `install` call (issue #376). `install` is a
// copy-with-attributes — and, with -d, a directory create. Forms:
//
//	install -d   [opts] DIR...        directory creation (benign no-op)
//	install      [opts] SRC DST       copy SRC -> DST (a file)
//	install      [opts] SRC... DIR    copy each SRC into DIR
//	install -t DIR [opts] SRC...      copy each SRC into DIR
//
// The DESTINATION decides whether the copy is load-bearing: a destination
// under the build dir lifts to a copy genrule (a real Bazel artifact); a
// destination outside it — the common `${CMAKE_INSTALL_PREFIX}/bin` staging
// — is a benign skip, since install-prefix output is not a Bazel-tracked
// input and a genrule for it would only dangle. A source that can't be
// anchored under the source root refuses (the input can't be named).
// Mode/ownership flags (-m/-o/-g, -p, ...) are dropped — permissions don't
// affect the bytes a Bazel consumer reads. `-s`/`--strip`/`--strip-program`
// rewrite the bytes (a stripped binary != the source), so they refuse; and
// any unrecognized flag refuses rather than risk mis-splitting SRC/DEST
// (safe round-2 fallback).
// installOpts holds the install(1) flag state parseInstallArgs recovers and
// liftInstall acts on (directory mode, byte-mutating strip, the -t target
// directory, and the -T force-file override).
type installOpts struct {
	dirMode      bool
	strip        bool
	forceFile    bool // -T / --no-target-directory: DEST is always a file
	targetDir    string
	hasTargetDir bool
}

// parseInstallArgs parses an install(1) command line (GNU coreutils + the
// common BSD subset) into its operands and the option flags liftInstall acts
// on. It returns ok=false with a human-readable reason when an option is
// unrecognized or malformed — install refuses rather than risk mis-splitting
// SRC/DEST.
//
// Option model:
//   - short value-taking (joined `-m755` or next-arg `-m 755`):
//     -m MODE, -o OWNER, -g GROUP, -t DIR, -S SUFFIX;
//   - short boolean: -d (dir), -s (strip), -D/-p/-v/-b/-c/-C/-T/-Z/-P;
//   - long value-taking (--opt VALUE or --opt=VALUE) + long boolean (see maps).
func parseInstallArgs(args []string) ([]string, installOpts, string, bool) {
	var operands []string
	var o installOpts
	const valueShort = "mogtS"
	const boolShort = "dsDpvbcCTZP"
	longValue := map[string]bool{
		"--mode": true, "--owner": true, "--group": true,
		"--suffix": true, "--target-directory": true, "--strip-program": true,
	}
	longBool := map[string]bool{
		"--directory": true, "--compare": true, "--preserve-timestamps": true,
		"--strip": true, "--verbose": true,
		"--backup": true, "--context": true, "--debug": true,
		"--help": true, "--version": true,
	}
	i := 0
	for i < len(args) {
		a := args[i]
		switch {
		case a == "--":
			operands = append(operands, args[i+1:]...)
			i = len(args)
		case strings.HasPrefix(a, "--"):
			ni, reason, ok := parseInstallLongOption(args, i, longValue, longBool, &o)
			if !ok {
				return nil, installOpts{}, reason, false
			}
			i = ni
		case strings.HasPrefix(a, "-") && len(a) > 1:
			ni, reason, ok := parseInstallShortCluster(args, i, valueShort, boolShort, &o)
			if !ok {
				return nil, installOpts{}, reason, false
			}
			i = ni
		default:
			operands = append(operands, args[i:]...)
			i = len(args)
		}
	}
	return operands, o, "", true
}

// parseInstallLongOption handles one `--foo[=val]` argv element at args[i],
// updating o and returning the next index to scan from. ok=false + a reason on
// an unrecognized option, or on a value-taking option (`--target-directory`,
// and the `longValue` set) given without a value. `--strip-program` is the one
// value-taking option NOT value-validated here: it sets o.strip, and liftInstall
// refuses ANY strip ("modifies the copied bytes") regardless of the value, so a
// value-less `--strip-program` already gets a clear refusal — validating it
// would be dead code.
func parseInstallLongOption(args []string, i int, longValue, longBool map[string]bool, o *installOpts) (int, string, bool) {
	a := args[i]
	name, val := a, ""
	hasEq := false
	if eq := strings.IndexByte(a, '='); eq >= 0 {
		name, val, hasEq = a[:eq], a[eq+1:], true
	}
	switch {
	case name == "--directory":
		o.dirMode = true
		return i + 1, "", true
	case name == "--no-target-directory":
		o.forceFile = true
		return i + 1, "", true
	case name == "--strip":
		o.strip = true
		return i + 1, "", true
	case name == "--strip-program":
		o.strip = true
		if hasEq {
			return i + 1, "", true
		}
		return i + 2, "", true // consume the program-path value
	case name == "--target-directory":
		if hasEq {
			o.targetDir, o.hasTargetDir = val, true
			return i + 1, "", true
		}
		if i+1 >= len(args) {
			return i, "install: --target-directory without a value", false
		}
		o.targetDir, o.hasTargetDir = args[i+1], true
		return i + 2, "", true
	case longValue[name]:
		if hasEq {
			return i + 1, "", true
		}
		if i+1 >= len(args) {
			return i, fmt.Sprintf("install: %s without a value", name), false
		}
		return i + 2, "", true // flag + its separate value
	case longBool[name]:
		return i + 1, "", true
	default:
		return i, fmt.Sprintf("install: unrecognized option %q (refusing rather than risk mis-splitting SRC/DEST)", a), false
	}
}

// parseInstallShortCluster handles one `-xyz` short-flag cluster at args[i],
// updating o and returning the next index to scan from. A value-taking flag
// (mogtS) swallows the rest of the cluster as its value, or — when it's the
// last char — the next argv element. Unknown chars refuse (don't guess whether
// they consume a value and shift the operands).
func parseInstallShortCluster(args []string, i int, valueShort, boolShort string, o *installOpts) (int, string, bool) {
	a := args[i]
	body := a[1:]
	consumedNext := false
	recognized := true
	for j := 0; j < len(body); j++ {
		c := body[j]
		if c == 'd' {
			o.dirMode = true
			continue
		}
		if c == 's' {
			o.strip = true
			continue
		}
		if c == 'T' {
			o.forceFile = true // DEST is always a file, never a dir
			continue
		}
		if strings.IndexByte(valueShort, c) >= 0 {
			rest := body[j+1:]
			if rest != "" {
				if c == 't' {
					o.targetDir, o.hasTargetDir = rest, true
				}
			} else {
				// Value is the next argv element — required for EVERY
				// value-taking short flag (-m/-o/-g/-t/-S), not just -t.
				// Validate it exists so a malformed trailing flag (`install
				// -m`) refuses with a clear diagnostic instead of mis-counting
				// operands.
				if i+1 >= len(args) {
					return i, fmt.Sprintf("install: -%c given without a value", c), false
				}
				if c == 't' {
					o.targetDir, o.hasTargetDir = args[i+1], true
				}
				consumedNext = true
			}
			break // value-taking flag swallows the cluster remainder
		}
		if strings.IndexByte(boolShort, c) < 0 {
			recognized = false
			break
		}
	}
	if !recognized {
		return i, fmt.Sprintf("install: unrecognized short option in %q (refusing rather than risk mis-splitting SRC/DEST)", a), false
	}
	if consumedNext {
		return i + 2, "", true
	}
	return i + 1, "", true
}

func liftInstall(args []string, anc execAnchors, cc *codegenContext) ([]string, string, bool) {
	operands, o, reason, ok := parseInstallArgs(args)
	if !ok {
		return nil, reason, false
	}

	// -s / --strip / --strip-program rewrite the copied bytes (a stripped
	// binary differs from the source), so the call can't be reproduced as a
	// plain byte copy. Refuse → round-2 fallback rather than emit a wrong
	// artifact.
	if o.strip {
		return nil, "install: -s/--strip(-program) modifies the copied bytes; can't reproduce as a plain copy", false
	}

	// `install -d DIR...`: directory creation. No Bazel artifact (fresh
	// sandbox per action), so skip benignly — same as mkdir / cmake -E
	// make_directory.
	if o.dirMode {
		return nil, "", true
	}

	// -T / --no-target-directory forces DEST to be a file and is mutually
	// exclusive with -t / --target-directory (which forces a directory).
	if o.forceFile && o.hasTargetDir {
		return nil, "install: -T/--no-target-directory and -t are mutually exclusive", false
	}

	var sources []string
	var dest string
	destIsDir := false
	switch {
	case o.hasTargetDir:
		if len(operands) == 0 {
			return nil, "install: -t given but no source operands", false
		}
		sources, dest, destIsDir = operands, o.targetDir, true
	case len(operands) >= 2:
		dest = operands[len(operands)-1]
		sources = operands[:len(operands)-1]
		// dest is a directory when: multiple sources, a trailing slash, or
		// it exists on disk as a directory — the `install foo /build/include`
		// form where /build/include is an existing dir, in which case install
		// copies to /build/include/foo (not to a file named include). The
		// on-disk check resolves the otherwise-ambiguous single-source,
		// no-trailing-slash case; it fires only for live runs whose recorded
		// paths exist on this host (reply-dir paths won't, falling back to
		// the syntactic signals). -T/--no-target-directory overrides ALL of
		// these: DEST is then always the file path.
		destIsDir = !o.forceFile &&
			(len(sources) > 1 || strings.HasSuffix(dest, "/") || isExistingDir(dest))
	default:
		return nil, fmt.Sprintf("install: expected SRC... DEST (got %d operand(s))", len(operands)), false
	}

	// install needs an absolute destination to resolve. A RELATIVE dest
	// (e.g. `install foo include/`) can't be anchored —
	// executeProcessAnchorOutput rejects relative paths the same as
	// outside-the-tree ones, so treating it as a benign skip would silently
	// drop a copy that may land under the build tree. Refuse instead.
	if !filepath.IsAbs(dest) {
		return nil, fmt.Sprintf("install: destination %q is relative; can't anchor it (refusing rather than risk dropping a build-tree copy)", dest), false
	}

	// The directory the file(s) land in decides build-tree vs install-prefix.
	destDir := dest
	if !destIsDir {
		destDir = filepath.Dir(dest)
	}
	if _, ok := executeProcessAnchorOutput(destDir, anc); !ok {
		// Not under the build tree. A dest under the SOURCE tree (e.g.
		// `install foo /src/include/foo.h`) can be a real, load-bearing
		// compile input — silently skipping it would drop a file Bazel
		// needs, so refuse. Only a dest outside BOTH trees is the
		// install-prefix staging form that's a safe benign skip.
		if _, underSrc := executeProcessAnchorSource(destDir, anc); underSrc {
			return nil, fmt.Sprintf("install: destination %q is under the source tree (a potential build input); refusing rather than silently dropping it", dest), false
		}
		return nil, "", true
	}

	// Under the build tree: reproduce each copy. emitCopyGenrule anchors the
	// source (refuses if not under the source root) and the destination
	// (under build, checked above) and emits one cp genrule per file.
	var outs []string
	for _, s := range sources {
		dstPath := dest
		if destIsDir {
			dstPath = filepath.Join(dest, filepath.Base(s))
		}
		o, reason, ok := emitCopyGenrule("install", "install", s, dstPath, anc, cc)
		if !ok {
			return nil, reason, false
		}
		outs = append(outs, o...)
	}
	return outs, "", true
}

// liftCpFile handles `cp <file> <dst>`. When dst names a
// directory (the recorded dst is the build dir, ends in '/', or
// has no file extension while src has a basename), cp drops the
// source basename into it — the output is dstRel/<basename>;
// otherwise dst is the literal output path. The dir heuristic is
// best-effort (a real on-disk stat of dst isn't available — it
// doesn't exist yet at convert time), documented here so the
// limitation is visible: an extensionless destination FILE
// (e.g. `cp x README`) is mis-treated as a directory.
func liftCpFile(realSrcRel, dst, dstRel string, cc *codegenContext) ([]string, string, bool) {
	outRel := dstRel
	// POSIX `cp <file> <dst>`: when <dst> doesn't already exist as a
	// directory, cp creates a file AT <dst>; it only drops the source
	// basename into <dst> when <dst> is an existing directory. At
	// convert time the build-dir destination never pre-exists, so the
	// only reliable "this is a directory" signals are a trailing slash
	// or the build-dir root itself (dstRel==""). An extensionless dst
	// like `${BINARY}/script` is therefore treated as a FILE — so
	// `cp src/script ${BINARY}/script` yields `script`, not
	// `script/script`.
	dstIsDir := dstRel == "" || strings.HasSuffix(dst, "/")
	if dstIsDir {
		base := filepath.Base(realSrcRel)
		if dstRel == "" {
			outRel = base
		} else {
			outRel = dstRel + "/" + base
		}
	}
	if _, exists := cc.OutToGenrule[outRel]; exists {
		return []string{outRel}, "", true
	}
	name := executeProcessGenruleName(outRel)
	cc.Genrules = append(cc.Genrules, ir.Target{
		Name:        name,
		Kind:        ir.KindGenrule,
		Srcs:        []string{realSrcRel},
		GenruleCmd:  fmt.Sprintf(`mkdir -p "$$(dirname "$@")" && cp "$(location %s)" "$@"`, realSrcRel),
		GenruleOuts: []string{outRel},
		Tags:        cmakeETags("cp"),
		Visibility:  []string{"//visibility:private"},
	})
	cc.OutToGenrule[outRel] = name
	return []string{outRel}, "", true
}

// liftCpDir handles `cp -R <srcdir> <dstdir>`. POSIX cp -R copies
// srcdir INTO dstdir, landing files at
// dstdir/<basename(srcdir)>/<rel>. The basename used is the
// ORIGINAL srcRel basename (e.g. `data`), NOT the deref'd target
// name, matching what cp does with a symlinked source argument — so
// the sub-prefix passed to emitDirCopyGenrule is filepath.Base(srcRel).
func liftCpDir(real, srcRel, realSrcRel, dstRel string, anc execAnchors, cc *codegenContext) ([]string, string, bool) {
	return emitDirCopyGenrule(real, realSrcRel, dstRel, filepath.Base(srcRel), "cp", anc, cc)
}

// dirCopyOutRel joins the destination-relative dir, an optional
// sub-prefix, and the per-file path into the output rel, skipping
// empty components so a build-dir-root destination (dstRel=="") or an
// empty sub-prefix (copy_directory / rename, which don't insert the
// source basename) don't produce leading-slash or double-slash rels.
func dirCopyOutRel(dstRel, subPrefix, fileUnder string) string {
	parts := make([]string, 0, 3)
	if dstRel != "" {
		parts = append(parts, dstRel)
	}
	if subPrefix != "" {
		parts = append(parts, subPrefix)
	}
	parts = append(parts, fileUnder)
	return strings.Join(parts, "/")
}

// emitDirCopyGenrule walks the (deref'd) source directory `real` and
// emits ONE multi-output genrule copying every regular file under it
// to <dstRel>/<subPrefix>/<fileUnder>. subPrefix is the source
// argument's basename for `cp -R` (which copies srcdir INTO dstdir)
// and empty for `cmake -E copy_directory` / `rename` / `mv` (which
// copy the directory's CONTENTS). Multi-out genrules reference
// $(RULEDIR) rather than $@ (single-output only). Srcs/outs are sorted
// for determinism. An empty source dir succeeds with no rels (an empty
// copy is a no-op — failing conversion over it would be wrong). `op`
// is the audit op label for the genrule's execute-process-op tag.
func emitDirCopyGenrule(real, realSrcRel, dstRel, subPrefix, op string, anc execAnchors, cc *codegenContext) ([]string, string, bool) {
	type pair struct{ src, out string }
	var pairs []pair
	err := filepath.WalkDir(real, func(p string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() || !d.Type().IsRegular() {
			return nil
		}
		fileUnder, rerr := filepath.Rel(real, p)
		if rerr != nil {
			return rerr
		}
		fileUnder = filepath.ToSlash(fileUnder)
		// Re-anchor each file against the source root (robust to
		// nested symlinks); fall back to joining the deref'd
		// dir's source-root rel with the walked sub-path.
		srcFileRel, ok := executeProcessAnchorSource(p, anc)
		if !ok {
			srcFileRel = realSrcRel + "/" + fileUnder
		}
		outRel := dirCopyOutRel(dstRel, subPrefix, fileUnder)
		if _, exists := cc.OutToGenrule[outRel]; exists {
			return nil
		}
		pairs = append(pairs, pair{src: srcFileRel, out: outRel})
		return nil
	})
	if err != nil {
		return nil, fmt.Sprintf("%s: failed to walk source dir %q: %v", op, realSrcRel, err), false
	}
	if len(pairs) == 0 {
		// Empty dir (or every file already recovered): no-op copy,
		// succeed with no rels rather than failing conversion.
		return nil, "", true
	}
	sort.Slice(pairs, func(i, j int) bool { return pairs[i].out < pairs[j].out })

	srcs := make([]string, 0, len(pairs))
	outs := make([]string, 0, len(pairs))
	cmds := make([]string, 0, len(pairs))
	for _, pr := range pairs {
		srcs = append(srcs, pr.src)
		outs = append(outs, pr.out)
		cmds = append(cmds, fmt.Sprintf(
			`mkdir -p "$(RULEDIR)/%s" && cp -L "$(location %s)" "$(RULEDIR)/%s"`,
			filepath.ToSlash(filepath.Dir(pr.out)), pr.src, pr.out))
	}
	name := executeProcessGenruleName(outs[0])
	cc.Genrules = append(cc.Genrules, ir.Target{
		Name:        name,
		Kind:        ir.KindGenrule,
		Srcs:        srcs,
		GenruleCmd:  strings.Join(cmds, " && "),
		GenruleOuts: outs,
		Tags:        cmakeETags(op),
		Visibility:  []string{"//visibility:private"},
	})
	for _, o := range outs {
		cc.OutToGenrule[o] = name
	}
	return outs, "", true
}

// liftCMakeECopyDirectory translates `cmake -E copy_directory <src>
// <dst>` (and the copy_directory_if_different form) into a recursive
// copy genrule. cmake copies the CONTENTS of <src> into <dst> (no
// source-basename insertion, unlike `cp -R`), so the emit runs with an
// empty sub-prefix. The on-disk source is consulted to enumerate the
// files and dereference symlinks — work the argv-only classifier
// leaves to the lifter.
func liftCMakeECopyDirectory(op string, args []string, anc execAnchors, cc *codegenContext) ([]string, string, bool) {
	if len(args) != 2 {
		return nil, fmt.Sprintf("cmake -E %s: v1 supports the 2-arg form only (got %d args)", op, len(args)), false
	}
	return liftDirCopyAnchored("cmake -E "+op, op, args[0], args[1], anc, cc)
}

// liftDirCopyAnchored anchors a (src, dst) directory pair and emits a
// recursive contents-copy genrule (empty sub-prefix). src must resolve
// under the source root and be a directory on disk; dst must resolve
// under the build dir. Shared by `cmake -E copy_directory` and the
// directory arm of `rename` / `mv`.
func liftDirCopyAnchored(what, op, src, dst string, anc execAnchors, cc *codegenContext) ([]string, string, bool) {
	srcRel, ok := executeProcessAnchorSource(src, anc)
	if !ok {
		return nil, fmt.Sprintf("%s: source %q is not under the source root", what, src), false
	}
	dstRel, ok := executeProcessAnchorOutput(dst, anc)
	if !ok {
		return nil, fmt.Sprintf("%s: destination %q is not under the build dir", what, dst), false
	}
	if dstRel == "." {
		dstRel = ""
	}
	abs := filepath.Join(anc.hostSrcDir, srcRel)
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		real = abs
	}
	info, err := os.Stat(real)
	if err != nil {
		return nil, fmt.Sprintf("%s: source %q does not exist on disk", what, src), false
	}
	if !info.IsDir() {
		return nil, fmt.Sprintf("%s: source %q is not a directory", what, src), false
	}
	realSrcRel, ok := executeProcessAnchorSource(real, anc)
	if !ok {
		realSrcRel = srcRel
	}
	return emitDirCopyGenrule(real, realSrcRel, dstRel, "", op, anc, cc)
}

// liftRenameLike translates `cmake -E rename <src> <dst>` and raw
// `mv <src> <dst>` into a copy genrule. Under Bazel's hermetic action
// model a rename has no "remove the source" analog (the source bytes
// stay), so the lift reproduces only the destination side — sound,
// because a renamed file/dir that feeds a later build step is
// load-bearing, while the source's post-rename absence isn't
// modelable (and isn't a compile input). The on-disk source decides
// file-vs-directory: a file routes to emitCopyGenrule, a directory to
// liftDirCopyAnchored (contents copy).
//
// The common in-build-tree shape `mv build/x.tmp build/x` refuses
// cleanly: the source isn't under the source root, so the anchor fails
// and the call falls through to the round-2 fallback rather than
// mis-lifting a non-existent-at-convert-time temp file.
//
// args is everything after the driver (rename: argv[3:]; mv: argv[1:]).
// Leading-dash flags are ignored (mv's -f / -n / -T don't change the
// "dst holds src's bytes by path" outcome); exactly 2 operands are
// required.
func liftRenameLike(what, op string, args []string, anc execAnchors, cc *codegenContext) ([]string, string, bool) {
	operands := make([]string, 0, len(args))
	for _, a := range args {
		if strings.HasPrefix(a, "-") && a != "-" {
			continue
		}
		operands = append(operands, a)
	}
	if len(operands) != 2 {
		return nil, fmt.Sprintf("%s: v1 supports the 2-operand <src> <dst> form only (got %d operands)", what, len(operands)), false
	}
	return emitCopyFileOrDir(what, op, operands[0], operands[1], anc, cc)
}

// emitCopyFileOrDir anchors a (src, dst) pair and emits a copy genrule,
// dispatching on the on-disk source: a regular file routes to
// emitCopyGenrule (single-output copy), a directory to
// liftDirCopyAnchored (recursive contents copy). Shared by the
// rename / mv lift and the create_symlink / ln lift, both of which
// reproduce "dst holds src's bytes by path" and so must handle a
// directory source identically — a `$(location <dir>)` + plain `cp`
// emit would fail at build time on a directory.
//
// src must resolve under the source root (a real Bazel input); dst is
// anchored under the build dir by the file/dir emitter. The on-disk
// source is stat'd (through symlinks) ONLY to decide file-vs-dir: when
// it can't be stat'd (offline / synthetic-path conversions, broken
// link, race) the lift falls back to the single-file copy shape — the
// dominant case and the behaviour the create_symlink lift had before
// it grew directory support — rather than refusing. A genuinely
// missing source then surfaces as a clear "no such input" at Bazel
// build time, the same as any other anchored src that isn't on disk.
func emitCopyFileOrDir(what, op, src, dst string, anc execAnchors, cc *codegenContext) ([]string, string, bool) {
	srcRel, ok := executeProcessAnchorSource(src, anc)
	if !ok {
		return nil, fmt.Sprintf("%s: source %q is not under the source root", what, src), false
	}
	abs := filepath.Join(anc.hostSrcDir, srcRel)
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		real = abs
	}
	if info, err := os.Stat(real); err == nil && info.IsDir() {
		return liftDirCopyAnchored(what, op, src, dst, anc, cc)
	}
	return emitCopyGenrule(what, op, src, dst, anc, cc)
}

// liftCMakeEConfigureFile translates `cmake -E configure_file
// <input> <output>` into a Bazel-time genrule that invokes
// //tools:cmake-configure-file (not cmake at action time), so
// the lift removes cmake from the executor's dependency surface
// for projects that use this shape from execute_process.
//
// v1 accepts only the 2-arg form. cmake -E configure_file's
// flag list (--copy-only / --escape-quotes / --at-only /
// -D<KEY>=<value>) is rejected for now — supporting -D would
// require harvesting the supplied key/value pairs into a values
// dict, which the existing dump-vars namespace doesn't supply.
// A real fixture with flags can lift this restriction.
//
// Substitution behaviour: when invoked from execute_process at
// configure time, cmake -E configure_file evaluates against the
// parent cmake's process environment (not the parent's
// CMakeLists.txt variable namespace). configurefile.Extract's
// reverse-engineering from (template, rendered) bytes recovers
// whatever substitutions cmake actually applied, which is what
// the verify-pass needs to reproduce the rendering. cmakeVars
// is consulted first (parity with configure_file's lifter) for
// the case where the operator's CMakeLists.txt also surfaced
// matching env-shaped variables.
//
// Tags: the cmake-codegen-cmake-e family plus
// execute-process-op=configure_file, with cmake-codegen-lifted
// added when the verify-pass succeeds. Reuses
// configureFileLegacyCmd for the bytes-embedded fallback and
// configureFileLiftedCmd for the lifted shape so the recovered
// genrule looks structurally like a configure_file lift.
func liftCMakeEConfigureFile(args []string, anc execAnchors, liftEnabled bool, cmakeVars map[string]string, cc *codegenContext) ([]string, string, bool) {
	if len(args) != 2 {
		return nil, fmt.Sprintf("cmake -E configure_file: v1 supports the 2-arg form only (got %d args)", len(args)), false
	}
	if anc.hostBuildDir == "" {
		// Trace-only / offline path with no build-dir stash:
		// we can't read the rendered bytes, so skip the call
		// gracefully. Matches recoverConfigureFiles's and
		// recoverFileGenerate's no-build-dir behavior — every
		// recovery surface that needs the live build dir
		// degrades to "do nothing" rather than refusing,
		// because ToIR is expected to work in trace-only
		// contexts.
		return nil, "", true
	}
	src, dst := args[0], args[1]
	srcRel, ok := executeProcessAnchorSource(src, anc)
	if !ok {
		return nil, fmt.Sprintf("cmake -E configure_file: source %q is not under the source root", src), false
	}
	dstRel, ok := executeProcessAnchorOutput(dst, anc)
	if !ok {
		return nil, fmt.Sprintf("cmake -E configure_file: destination %q is not under the build dir", dst), false
	}
	if _, exists := cc.OutToGenrule[dstRel]; exists {
		return []string{dstRel}, "", true
	}

	// Resolve template + rendered bytes. The recording-machine
	// source path is reconstructed via hostSrcDir; the rendered
	// output lives under hostBuildDir (parity with
	// recoverConfigureFiles's path-resolution shape). Either
	// missing means the offline fixture / live tree is
	// incomplete; soft-skip rather than refusing, parity with
	// recoverConfigureFiles's read-error treatment.
	// Reaching here means hostBuildDir is set (the offline no-build-dir
	// degradation returned above), so a read failure is a LIVE anomaly: the
	// configure ran but the template / rendered output we need to recover the
	// configured file isn't readable. That's an uncertain skip — we'd be
	// silently dropping an output a consumer needs — so refuse (loud) rather
	// than the old "do nothing". (The offline trace-only case stays the
	// silent degradation above.)
	templatePath := filepath.Join(anc.hostSrcDir, srcRel)
	template, terr := os.ReadFile(templatePath)
	if terr != nil {
		return nil, fmt.Sprintf("cmake -E configure_file: template %q is unreadable — the configured output can't be recovered", srcRel), false
	}
	rendered, rerr := os.ReadFile(filepath.Join(anc.hostBuildDir, dstRel))
	if rerr != nil {
		return nil, fmt.Sprintf("cmake -E configure_file: rendered output %q is unreadable — the configured bytes can't be recovered", dstRel), false
	}

	// Same-path mirror — parity with configure_file's copyOnlySourceMirror.
	// srcRel (source-relative) == dstRel (build-relative): with identical
	// bytes the committed source IS the output, so emit NO rule at all (the
	// consumer attribution attaches the rel path, which resolves to the
	// source file; Bazel's includes attr covers the source root and its
	// genfiles mirror alike). No COPYONLY gate, unlike configure_file: byte
	// equality is the entire (and strictly sound) condition since cmake -E
	// configure_file always substitutes. With DIFFERING bytes a lifted spec
	// would carry Template == Out — an input/output collision Bazel rejects
	// at analysis time — so force the bake shape, which deliberately
	// references no template input.
	if srcRel == dstRel {
		if bytes.Equal(template, rendered) {
			return []string{dstRel}, "", true
		}
		liftEnabled = false
	}

	name := executeProcessGenruleName(dstRel)
	target := buildCMakeEConfigureFileGenrule(name, srcRel, dstRel, template, rendered, liftEnabled, cmakeVars, cc.StampVars)
	cc.Genrules = append(cc.Genrules, target)
	cc.OutToGenrule[dstRel] = name
	return []string{dstRel}, "", true
}

// buildCMakeEConfigureFileGenrule mirrors buildConfigureFileGenrule's
// lifted-vs-legacy decision tree, scoped to cmake -E configure_file
// (no @ONLY / COPYONLY / ESCAPE_QUOTES options in v1; future
// fixtures can lift those restrictions). Genex in the template
// short-circuits to legacy — same exit shape as the
// file(GENERATE) lifter.
func buildCMakeEConfigureFileGenrule(name, srcRel, dstRel string, template, rendered []byte, liftEnabled bool, cmakeVars, stampVars map[string]string) ir.Target {
	opts := configurefile.Options{}
	// Bake the resolved bytes via the shared bakeFileTarget chooser:
	// readable skylib write_file for \n-only text, byte-exact base64
	// genrule for binary / control-byte / CRLF bodies. Same de-base64
	// maintainability win as the configure_file / file(GENERATE) bakes.
	// The bake shape intentionally references no template input: the
	// bytes are already resolved, so staging the template would create
	// confusing rebuild semantics — template edits would invalidate the
	// rule via Bazel's source graph but the re-run would just re-emit the
	// same baked bytes. The lifted branch below points a cmake_configure_file
	// rule at the template (via its `template` label) because it re-renders
	// at Bazel time.
	legacy := bakeFileTarget(name, dstRel, rendered, cmakeEConfigureFileTags(false))
	if !liftEnabled {
		return legacy
	}
	if hasGenex(template) {
		return legacy
	}
	values, ok := pickValues(template, rendered, opts, cmakeVars)
	if !ok {
		return legacy
	}
	spec := newConfigureFileSpec(dstRel, opts)
	spec.Template = srcRel
	spec.Values = values
	// VCS-stamp lift parity with the configure_file recovery: a template
	// var written by a stamp execute_process re-reads its value from the
	// Bazel workspace status at build time; the baked value stays in
	// `values` as the non-stamped fallback. Limitation (documented at the
	// prescan): set()-copy-forwarded stamps (propagateStampVars runs after
	// this recovery) don't wire here.
	spec.StampValues = stampValuesForTemplate(template, opts, stampVars)
	return cmakeConfigureFileTarget(name, spec, cmakeEConfigureFileTags(true))
}

// cmakeEConfigureFileTags returns the cmake-codegen tag set for
// a recovered `cmake -E configure_file` call. Mirrors
// cmakeETags("configure_file") and adds cmake-codegen-lifted
// when the verify-pass succeeded.
func cmakeEConfigureFileTags(lifted bool) []string {
	tags := []string{
		"cmake-codegen",
		"cmake-codegen-cmake-e",
		"cmake-codegen-driver=cmake_e",
		"cmake-codegen-execute-process",
		"cmake-codegen-execute-process-op=configure_file",
	}
	if lifted {
		tags = append(tags, "cmake-codegen-lifted")
	}
	sort.Strings(tags)
	return tags
}

// liftFileProducing translates an execute_process call with a
// declared OUTPUT_FILE redirect into a build-time genrule.
// This hoist moves the work from configure-time to build-time,
// which is a behaviour change — the genrule carries the
// cmake-codegen-execute-process-hoisted tag so audits flag the
// move for reviewer attention.
//
// v1 is conservative about hoist preconditions:
//   - OUTPUT_FILE must anchor under the build dir.
//   - argv elements that look like absolute paths under the
//     source root are surfaced as Bazel srcs with $(location)
//     substitution in the cmd; argv[0] is preserved as-is so
//     PATH-resolved tools (python3, gen-script-on-PATH) keep
//     working under Bazel's standard executor sandbox.
//   - WORKING_DIRECTORY and ENVIRONMENT are not yet modeled —
//     refuse the lift if either is set so a hoisted genrule
//     never silently drops them (follow-on). TIMEOUT (a
//     configure-time watchdog), INPUT_FILE (stdin from a
//     source-tree file), and ERROR_FILE (stderr to a build-dir
//     path, or merged via the same-file idiom) ARE modeled —
//     see the gate block below.
//
// Driver tag: cmake-codegen-driver=<basename(argv[0])> mirrors
// the genrule.go custom-command recovery so existing audit
// queries that filter on driver= pick up hoisted rules.
func liftFileProducing(call shadow.ExecuteProcessCall, anc execAnchors, cc *codegenContext) ([]string, string, bool) {
	if call.WorkingDirectory != "" {
		return nil, "WORKING_DIRECTORY is not yet modeled by the file-producing lifter", false
	}
	if len(call.Environment) > 0 {
		return nil, "ENVIRONMENT is not yet modeled by the file-producing lifter", false
	}
	// TIMEOUT is a configure-time watchdog (kill the child if slow); it
	// never shapes the OUTPUT bytes, and Bazel actions carry their own
	// execution timeouts. Dropping it changes a failure MODE, not a
	// produced artifact — so it no longer refuses the lift.
	//
	// INPUT_FILE is a plain stdin redirect: a source-tree input lifts
	// as a declared src with `< $(location …)`. A build-dir INPUT_FILE
	// (stdin from another recovery's output) stays refused for now —
	// the producer-ordering wiring is its own follow-on.
	stdinRel := ""
	if call.InputFile != "" {
		rel, ok := executeProcessAnchorSource(call.InputFile, anc)
		if !ok {
			return nil, fmt.Sprintf("INPUT_FILE %q is not under the source tree (build-dir stdin producer wiring not modeled)", call.InputFile), false
		}
		stdinRel = rel
	}

	dstRel, ok := executeProcessAnchorOutput(call.OutputFile, anc)
	if !ok {
		return nil, fmt.Sprintf("OUTPUT_FILE %q is not under the build dir", call.OutputFile), false
	}

	// ERROR_FILE is a stderr redirect: the same file as OUTPUT_FILE is
	// cmake's documented stream-merge (`2>&1`); a distinct build-dir
	// path becomes a second declared out. Outside the build dir there
	// is nothing to anchor it to — refuse as before.
	errRel := ""
	mergeStderr := false
	if call.ErrorFile != "" {
		if call.ErrorFile == call.OutputFile {
			mergeStderr = true
		} else if rel, ok := executeProcessAnchorOutput(call.ErrorFile, anc); ok {
			errRel = rel
		} else {
			return nil, fmt.Sprintf("ERROR_FILE %q is not under the build dir", call.ErrorFile), false
		}
	}
	if _, exists := cc.OutToGenrule[dstRel]; exists {
		return []string{dstRel}, "", true
	}

	argv := call.Commands[0]
	if len(argv) == 0 {
		return nil, "empty argv", false
	}

	// Walk argv: rewrite elements that anchor under the source
	// root as $(location <rel>) so Bazel's source graph tracks
	// them. argv[0] is the tool name; absolute host paths
	// (cmake's resolution of ${Python3_EXECUTABLE}, etc.) are
	// stripped to basename so the recovered cmd is portable
	// across executor environments — host-resolved /usr/local/bin/
	// paths are recording-machine artefacts, not declared
	// dependencies. If argv[0] resolves under the source root
	// it's an in-tree tool; surface as $(location) so the
	// dependency is explicit.
	//
	// Directories under the source root (cmake's
	// ${CMAKE_CURRENT_SOURCE_DIR} expanding to a dir path) are
	// preserved as literal strings rather than $(location)
	// references — Bazel's $(location <label>) expects a single
	// file, and a dir-shaped src would either fail or expand
	// to surprising basenames. The hostSrcDir-relative path is
	// still emitted (so the cmd is portable across recording
	// machines), just without the $(location) wrap.
	srcs := make([]string, 0)
	srcSet := map[string]bool{}
	rewritten := make([]string, 0, len(argv))
	for i, a := range argv {
		if rel, ok := executeProcessAnchorSource(a, anc); ok {
			// relativeIfInside maps the source root itself
			// to "" (empty relative path). For an argv
			// element that points AT the source root
			// (e.g. cmake's ${CMAKE_CURRENT_SOURCE_DIR}
			// resolving to the project root), the only
			// valid rendering is the literal `.` —
			// shellQuoteArg("") would emit `''` (changing
			// the cmd's semantics from "source root" to
			// "empty argument") and $(location <empty>) /
			// $(location .) wouldn't resolve to a Bazel
			// label. Treat empty rel as the source-root
			// directory so the directory branch fires
			// regardless of the isExistingDir filesystem
			// probe (which can fail on offline tests where
			// hostSrcDir is synthetic).
			isDir := rel == "" || isExistingDir(filepath.Join(anc.hostSrcDir, rel))
			if rel == "" {
				rel = "."
			}
			if isDir {
				// Directory: preserve as literal relative path.
				rewritten = append(rewritten, shellQuoteArg(rel))
				continue
			}
			if !srcSet[rel] {
				srcSet[rel] = true
				srcs = append(srcs, rel)
			}
			rewritten = append(rewritten, fmt.Sprintf("$(location %s)", rel))
			continue
		}
		if i == 0 && filepath.IsAbs(a) {
			// Host-resolved tool path: strip to basename so
			// the cmd is portable. The driver tag below
			// captures the same basename for audit queries.
			rewritten = append(rewritten, shellQuoteArg(filepath.Base(a)))
			continue
		}
		rewritten = append(rewritten, shellQuoteArg(a))
	}

	tool := strings.Join(rewritten, " ")
	if stdinRel != "" {
		if !srcSet[stdinRel] {
			srcSet[stdinRel] = true
			srcs = append(srcs, stdinRel)
		}
		tool += fmt.Sprintf(` < "$(location %s)"`, stdinRel)
	}
	outs := []string{dstRel}
	var cmd string
	switch {
	case errRel != "":
		// Two declared outs: $@ is single-out-only, so address both
		// through $(location <out>) — Bazel resolves a rule's own outs.
		outs = append(outs, errRel)
		cmd = fmt.Sprintf(`mkdir -p "$$(dirname "$(location %[1]s)")" "$$(dirname "$(location %[2]s)")" && %[3]s > "$(location %[1]s)" 2> "$(location %[2]s)"`, dstRel, errRel, tool)
	case mergeStderr:
		cmd = fmt.Sprintf(`mkdir -p "$$(dirname "$@")" && %s > "$@" 2>&1`, tool)
	default:
		cmd = fmt.Sprintf(`mkdir -p "$$(dirname "$@")" && %s > "$@"`, tool)
	}
	driver := executeProcessDriverBasename(argv[0])
	if driver == "" {
		driver = "unknown"
	}

	name := executeProcessGenruleName(dstRel)
	cc.Genrules = append(cc.Genrules, ir.Target{
		Name:        name,
		Kind:        ir.KindGenrule,
		Srcs:        srcs,
		GenruleCmd:  cmd,
		GenruleOuts: outs,
		Tags:        fileProducingTags(driver),
		Visibility:  []string{"//visibility:private"},
	})
	cc.OutToGenrule[dstRel] = name
	if errRel != "" {
		// The stderr out is registered (no other channel may claim it)
		// but NOT returned for consumer attribution — a diagnostics
		// dump shouldn't attach to compile inputs by include coverage.
		cc.OutToGenrule[errRel] = name
	}
	// Per-site conversion-todos override: a hoisted VCS/identity/date stamp
	// bakes a non-hermetic value (wrong on rebuild) — more than an
	// "improvement", an author should re-express it (workspace-status / stamp).
	// A baked check-probe under the same tag stays the default Improvement.
	if cc.bakeTodoDisposition != nil && stampDrivers[driver] {
		cc.bakeTodoDisposition[name] = todos.Actionable
	}
	return []string{dstRel}, "", true
}

// fileProducingTags builds the cmake-codegen tag set for a
// hoisted file-producing genrule. The -hoisted facet is the
// audit-visible signal that this rule represents work moved
// from configure-time to build-time, distinct from genuine
// build-time genrules that the user authored as
// add_custom_command.
func fileProducingTags(driver string) []string {
	tags := []string{
		"cmake-codegen",
		"cmake-codegen-driver=" + driver,
		"cmake-codegen-execute-process",
		"cmake-codegen-execute-process-hoisted",
	}
	sort.Strings(tags)
	return tags
}

// shellQuoteArg returns a shell-safe representation of an argv
// element. Shell-special chars get single-quoted; literal
// single quotes in the input are escaped as the standard
// `'\”` sequence (close-quote, escaped-quote, re-open-quote).
// Used by liftFileProducing to defend the hoisted cmd
// against argv elements containing spaces, $, etc.
// — paths from cmake's expanded trace are usually plain but
// this keeps the lifter honest about adversarial inputs.
func shellQuoteArg(a string) string {
	if a == "" {
		return "''"
	}
	safe := true
	for i := 0; i < len(a); i++ {
		c := a[i]
		switch {
		case c >= 'a' && c <= 'z',
			c >= 'A' && c <= 'Z',
			c >= '0' && c <= '9',
			c == '_', c == '-', c == '/', c == '.', c == ':', c == '+', c == '=', c == ',':
			// safe
		default:
			safe = false
		}
		if !safe {
			break
		}
	}
	if safe {
		return a
	}
	return "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
}

// executeProcessAnchorOutput tries to resolve a recorded
// absolute path as a build-dir-relative slash path. Returns
// ("", false) when the path is relative (no anchor context) or
// resolves outside the build dir.
func executeProcessAnchorOutput(p string, anc execAnchors) (string, bool) {
	if !filepath.IsAbs(p) {
		return "", false
	}
	return relativeIfInsideRelaxed(anc.recordedBuildDir, p)
}

// executeProcessAnchorSource tries to resolve a recorded
// absolute path as a source-root-relative slash path. Returns
// ("", false) when the path is relative or resolves outside
// the source root. We accept either the host-real source path
// (the recording machine's view) OR the recorded source path
// — offline fixtures keep both consistent, but production runs
// the recorder and the converter on the same machine so
// recordedSrcDir == hostSrcDir.
func executeProcessAnchorSource(p string, anc execAnchors) (string, bool) {
	if !filepath.IsAbs(p) {
		return "", false
	}
	if rel, ok := relativeIfInside(anc.recordedSrcDir, p); ok {
		// Umbrella re-anchor, mirroring lc.umbrellaReanchor for the
		// exec recovery's anchors: when the label root sits ABOVE the
		// cmake source dir (workspace promotion, --element-source-root
		// overlays, the nested-cmake recursive lowering), a bare
		// cmakeSrc-relative rel mis-anchors — the emitted genrule
		// srcs/$(location) must carry the labelRoot-relative form
		// ("sub/sub_extra.c.in", not "sub_extra.c.in"). Offline
		// replays where hostSrcDir is a different machine's path
		// don't relativize and keep the recorded-relative form.
		if anc.hostSrcDir != "" && anc.hostSrcDir != anc.recordedSrcDir {
			if prefix, inside := relativeIfInside(anc.hostSrcDir, anc.recordedSrcDir); inside && prefix != "" && prefix != "." {
				return filepath.ToSlash(filepath.Join(prefix, rel)), true
			}
		}
		return rel, true
	}
	if anc.hostSrcDir != "" && anc.hostSrcDir != anc.recordedSrcDir {
		if rel, ok := relativeIfInside(anc.hostSrcDir, p); ok {
			return rel, true
		}
	}
	return "", false
}

// executeProcessGenruleName turns a build-dir-relative output
// path into a Bazel-rule-name-safe identifier with the
// "exec_" prefix: "marker.stamp" -> "exec_marker_stamp",
// "subdir/foo.h" -> "exec_subdir_foo_h". Mirrors
// configureFileGenruleName's gen_-prefixed shape but uses a
// distinct prefix so the two recoveries can land in the same
// package without name collisions.
func executeProcessGenruleName(rel string) string {
	return "exec_" + sanitizePathToNameStem(rel)
}

// cmakeETags returns the cmake-codegen tag set for a recovered
// cmake -E execute_process call. cmake-codegen-driver=cmake_e
// is the audit-query handle; cmake-codegen-execute-process is
// the source-of-recovery facet (distinguishes from
// add_custom_command-driven cmake -E recoveries which carry
// the same cmake_e driver but originate in build.ninja, not
// the trace). cmake-codegen-cmake-e mirrors the existing
// genrule.go tag so existing audit queries that filter on it
// pick up execute_process-derived rules without rewording.
func cmakeETags(op string) []string {
	tags := []string{
		"cmake-codegen",
		"cmake-codegen-cmake-e",
		"cmake-codegen-driver=cmake_e",
		"cmake-codegen-execute-process",
		"cmake-codegen-execute-process-op=" + op,
	}
	sort.Strings(tags)
	return tags
}

// executeProcessRefusal is the per-call refusal record used
// inside recoverExecuteProcess to assemble the aggregated
// failure message. File / Line are stored as raw fields so
// the sort comparator can order numerically by line within
// each file (lexicographic ordering of "file:line" strings
// would put `:10` before `:2`); Bucket and Reason mirror the
// classifier output (or, for failed lifts, the lifter's
// specific diagnostic). Argv carries the joined COMMAND argv
// (or "<n> stages" for pipelines) for at-a-glance triage.
type executeProcessRefusal struct {
	File   string
	Line   int
	Bucket Bucket
	Reason string
	Argv   string
}

// loc returns "<file>:<line>" for the refusal's source
// location, or just "<file>" when the trace didn't record a
// line number (defensive — cmake's JSON-v1 trace always
// carries one in practice). Used by the failure-message
// renderer; sort uses File / Line directly for numeric
// ordering.
func (r executeProcessRefusal) loc() string {
	if r.Line > 0 {
		return r.File + ":" + itoa(r.Line)
	}
	return r.File
}

// formatExecuteProcessArgv compresses the call's COMMAND
// pipeline into a one-line string suitable for the failure
// report. Single-stage pipelines render as the joined argv;
// multi-stage pipelines render as "<n> stages: stage0 | stage1
// | ..." so the failure reader can spot them at a glance.
// Argv elements aren't shell-escaped — the report's purpose is
// triage, not re-execution; the original CMakeLists.txt is the
// re-execution source of truth.
func formatExecuteProcessArgv(call shadow.ExecuteProcessCall) string {
	if len(call.Commands) == 0 {
		return "(no COMMAND clause)"
	}
	if len(call.Commands) == 1 {
		return strings.Join(call.Commands[0], " ")
	}
	parts := make([]string, len(call.Commands))
	for i, stage := range call.Commands {
		parts[i] = strings.Join(stage, " ")
	}
	return itoa(len(call.Commands)) + " stages: " + strings.Join(parts, " | ")
}

// formatExecuteProcessRefusal renders the aggregated refusal
// list into a single message string, sorted by source
// location so the output is stable across runs (the trace's
// call order is configure-time-evaluation order which can
// shift with cmake version drift; sorting by file then
// numeric line is the stable presentation).
//
// Sort precedence: File ascending, then Line ascending. The
// secondary numeric sort matters when one file declares
// multiple unliftable calls — lexicographic sort on the
// rendered "file:line" string would put `:10` before `:2`.
func formatExecuteProcessRefusal(refusals []executeProcessRefusal) string {
	sort.Slice(refusals, func(i, j int) bool {
		if refusals[i].File != refusals[j].File {
			return refusals[i].File < refusals[j].File
		}
		return refusals[i].Line < refusals[j].Line
	})
	var sb strings.Builder
	sb.WriteString(itoa(len(refusals)))
	if len(refusals) == 1 {
		sb.WriteString(" execute_process call cannot be lifted natively:\n")
	} else {
		sb.WriteString(" execute_process calls cannot be lifted natively:\n")
	}
	for _, r := range refusals {
		sb.WriteString("  - ")
		sb.WriteString(r.loc())
		sb.WriteString(" [")
		sb.WriteString(string(r.Bucket))
		sb.WriteString("] ")
		sb.WriteString(r.Reason)
		sb.WriteString("\n      argv: ")
		sb.WriteString(r.Argv)
		sb.WriteString("\n")
	}
	sb.WriteString(
		"see docs/research/cmake_analysis.md §9 for the lift-or-refuse decision tree; " +
			"unliftable elements are intended to fall through to the round-2 fallback (Phase B, not yet wired)")
	return sb.String()
}

// itoa is a tiny strconv-free integer formatter. Avoids
// pulling strconv just for the line numbers in the refusal
// report; line numbers are always small positive ints in
// cmake's JSON-v1 trace.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := false
	if n < 0 {
		neg = true
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
