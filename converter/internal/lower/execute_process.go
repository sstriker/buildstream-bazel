package lower

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/sstriker/buildstream-bazel/converter/internal/failure"
	"github.com/sstriker/buildstream-bazel/converter/internal/fileapi"
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
// resolved found-package metadata for find_package.
//
// Phase 4 of the generator-parity uplift: extends the dump-vars
// rescue to also cover probes whose results land in cmake's cache
// via Check_* modules rather than directly in user variables.
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
		}
	}
	return out
}

func recoverExecuteProcess(calls []shadow.ExecuteProcessCall, hostSrcDir, recordedSrcDir, hostBuildDir, recordedBuildDir string, liftEnabled bool, cmakeVars map[string]string, cc *codegenContext) ([]executeProcessOut, []executeProcessRefusal) {
	if len(calls) == 0 {
		return nil, nil
	}
	var unsupported []executeProcessRefusal
	var outs []executeProcessOut
	collect := func(rels []string) {
		for _, rel := range rels {
			outs = append(outs, executeProcessOut{RelOutput: rel})
		}
	}
	for _, call := range calls {
		v := Classify(call)
		switch v.Bucket {
		case BucketCMakeE:
			rels, reason, ok := liftCMakeE(call, v, hostSrcDir, recordedSrcDir, hostBuildDir, recordedBuildDir, liftEnabled, cmakeVars, cc)
			if !ok {
				unsupported = append(unsupported, executeProcessRefusal{
					File:   call.File,
					Line:   call.Line,
					Bucket: v.Bucket,
					Reason: reason,
					Argv:   formatExecuteProcessArgv(call),
				})
				continue
			}
			collect(rels)
		case BucketFileProducing:
			rels, reason, ok := liftFileProducing(call, hostSrcDir, recordedSrcDir, recordedBuildDir, cc)
			if !ok {
				unsupported = append(unsupported, executeProcessRefusal{
					File:   call.File,
					Line:   call.Line,
					Bucket: v.Bucket,
					Reason: reason,
					Argv:   formatExecuteProcessArgv(call),
				})
				continue
			}
			collect(rels)
		default:
			// Phase 4 rescue: when a BucketProbe call writes an
			// OUTPUT_VARIABLE that's already in cmakeVars (captured
			// by the dump-vars hook at end-of-configure), the probe's
			// value flows through to downstream configure_file /
			// file(GENERATE) lifts via Reply.Vars — no Bazel-side
			// emission needed for the probe call itself. The probe
			// IS rescued; nothing fails.
			//
			// Same logic for BucketStamp: a version-stamp probe
			// (`git rev-parse HEAD`) sets OUTPUT_VARIABLE; if cmake
			// captured the value, downstream consumers see it via
			// cmakeVars. The downside is that the stamp value bakes
			// into srckey (which is the round-2 fallback's
			// per-build-tree trade-off); operators who need a
			// non-baked stamp opt into the round-2 path.
			if v.Bucket == BucketProbe || v.Bucket == BucketStamp {
				if call.OutputVariable != "" {
					if _, ok := cmakeVars[call.OutputVariable]; ok {
						// Captured via dump-vars; consumer reads
						// through cmakeVars. Skip the refusal.
						continue
					}
				}
			}
			unsupported = append(unsupported, executeProcessRefusal{
				File:   call.File,
				Line:   call.Line,
				Bucket: v.Bucket,
				Reason: v.Reason,
				Argv:   formatExecuteProcessArgv(call),
			})
		}
	}
	sort.Slice(outs, func(i, j int) bool { return outs[i].RelOutput < outs[j].RelOutput })
	return outs, unsupported
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
func liftCMakeE(call shadow.ExecuteProcessCall, v ClassifyResult, hostSrcDir, recordedSrcDir, hostBuildDir, recordedBuildDir string, liftEnabled bool, cmakeVars map[string]string, cc *codegenContext) ([]string, string, bool) {
	argv := call.Commands[0] // single-COMMAND guaranteed by Classify
	// cmake -E <op> <args...>; argv[0]=cmake, argv[1]=-E, argv[2]=op
	args := argv[3:]
	switch v.CMakeEOp {
	case "touch":
		return liftCMakeETouch(args, recordedBuildDir, cc)
	case "copy", "copy_if_different", "create_symlink":
		// create_symlink shares the (src, dst) two-arg shape with
		// copy / copy_if_different. Bazel actions run hermetically;
		// the action's output is the file at <dst>. Whether the
		// cmake-side path creates that file via cp or ln -sf is
		// irrelevant to downstream consumers — they read bytes by
		// path. Lifting symlink as copy preserves the dst-anchored
		// path semantics and avoids the subtle issues genrule
		// outputs have with symlinks (action-cache hashing,
		// sandbox cleanup, cross-fs handling).
		return liftCMakeECopy(v.CMakeEOp, args, hostSrcDir, recordedSrcDir, recordedBuildDir, cc)
	case "configure_file":
		return liftCMakeEConfigureFile(args, hostSrcDir, recordedSrcDir, hostBuildDir, recordedBuildDir, liftEnabled, cmakeVars, cc)
	case "cp":
		// Raw POSIX `cp` (issue #312). argv is `cp <flags...>
		// <src> <dst>` (no `-E <op>` prefix), so the operands
		// start at argv[1].
		return liftCp(argv[1:], hostSrcDir, recordedSrcDir, recordedBuildDir, cc)
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
func liftCMakeETouch(paths []string, recordedBuildDir string, cc *codegenContext) ([]string, string, bool) {
	if len(paths) == 0 {
		return nil, "cmake -E touch with no arguments", false
	}
	rels := make([]string, 0, len(paths))
	for _, p := range paths {
		rel, ok := executeProcessAnchorOutput(p, recordedBuildDir)
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
			GenruleCmd:  fmt.Sprintf(`mkdir -p "$$(dirname "$@")" && touch "$@"`),
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
func liftCMakeECopy(op string, args []string, hostSrcDir, recordedSrcDir, recordedBuildDir string, cc *codegenContext) ([]string, string, bool) {
	if len(args) != 2 {
		return nil, fmt.Sprintf("cmake -E %s: v1 supports the 2-arg form only (got %d args)", op, len(args)), false
	}
	src, dst := args[0], args[1]
	srcRel, ok := executeProcessAnchorSource(src, hostSrcDir, recordedSrcDir)
	if !ok {
		return nil, fmt.Sprintf("cmake -E %s: source %q is not under the source root", op, src), false
	}
	dstRel, ok := executeProcessAnchorOutput(dst, recordedBuildDir)
	if !ok {
		return nil, fmt.Sprintf("cmake -E %s: destination %q is not under the build dir", op, dst), false
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
		Tags:        cmakeETags(op),
		Visibility:  []string{"//visibility:private"},
	})
	cc.OutToGenrule[dstRel] = name
	return []string{dstRel}, "", true
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
func liftCp(args []string, hostSrcDir, recordedSrcDir, recordedBuildDir string, cc *codegenContext) ([]string, string, bool) {
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

	srcRel, ok := executeProcessAnchorSource(src, hostSrcDir, recordedSrcDir)
	if !ok {
		return nil, fmt.Sprintf("cp: source %q is not under the source root", src), false
	}
	dstRel, ok := executeProcessAnchorOutput(dst, recordedBuildDir)
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
	abs := filepath.Join(hostSrcDir, srcRel)
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
	realSrcRel, ok := executeProcessAnchorSource(real, hostSrcDir, recordedSrcDir)
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
	return liftCpDir(real, srcRel, realSrcRel, dstRel, hostSrcDir, recordedSrcDir, cc)
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
// name, matching what cp does with a symlinked source argument.
//
// One multi-output genrule is emitted covering every regular
// file under the (deref'd) source dir. Multi-out genrules
// reference $(RULEDIR) rather than $@ (which is single-output
// only). Srcs/outs are sorted for determinism. An empty source
// dir succeeds with no rels (an empty copy is a no-op — failing
// conversion over it would be wrong).
func liftCpDir(real, srcRel, realSrcRel, dstRel, hostSrcDir, recordedSrcDir string, cc *codegenContext) ([]string, string, bool) {
	type pair struct{ src, out string }
	var pairs []pair
	base := filepath.Base(srcRel)
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
		srcFileRel, ok := executeProcessAnchorSource(p, hostSrcDir, recordedSrcDir)
		if !ok {
			srcFileRel = realSrcRel + "/" + fileUnder
		}
		outRel := dstRel + "/" + base + "/" + fileUnder
		if dstRel == "" {
			outRel = base + "/" + fileUnder
		}
		if _, exists := cc.OutToGenrule[outRel]; exists {
			return nil
		}
		pairs = append(pairs, pair{src: srcFileRel, out: outRel})
		return nil
	})
	if err != nil {
		return nil, fmt.Sprintf("cp -R: failed to walk source dir %q: %v", srcRel, err), false
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
		Tags:        cmakeETags("cp"),
		Visibility:  []string{"//visibility:private"},
	})
	for _, o := range outs {
		cc.OutToGenrule[o] = name
	}
	return outs, "", true
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
func liftCMakeEConfigureFile(args []string, hostSrcDir, recordedSrcDir, hostBuildDir, recordedBuildDir string, liftEnabled bool, cmakeVars map[string]string, cc *codegenContext) ([]string, string, bool) {
	if len(args) != 2 {
		return nil, fmt.Sprintf("cmake -E configure_file: v1 supports the 2-arg form only (got %d args)", len(args)), false
	}
	if hostBuildDir == "" {
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
	srcRel, ok := executeProcessAnchorSource(src, hostSrcDir, recordedSrcDir)
	if !ok {
		return nil, fmt.Sprintf("cmake -E configure_file: source %q is not under the source root", src), false
	}
	dstRel, ok := executeProcessAnchorOutput(dst, recordedBuildDir)
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
	templatePath := filepath.Join(hostSrcDir, srcRel)
	template, terr := os.ReadFile(templatePath)
	if terr != nil {
		return nil, "", true
	}
	rendered, rerr := os.ReadFile(filepath.Join(hostBuildDir, dstRel))
	if rerr != nil {
		return nil, "", true
	}

	name := executeProcessGenruleName(dstRel)
	target := buildCMakeEConfigureFileGenrule(name, srcRel, dstRel, template, rendered, liftEnabled, cmakeVars)
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
func buildCMakeEConfigureFileGenrule(name, srcRel, dstRel string, template, rendered []byte, liftEnabled bool, cmakeVars map[string]string) ir.Target {
	opts := configurefile.Options{}
	// Legacy shape intentionally omits Srcs: the cmd is just
	// `echo <base64> | base64 -d > $@`, so the template isn't
	// consumed at action time. Staging it as Srcs would create
	// confusing rebuild semantics — template edits would
	// invalidate the genrule via Bazel's source graph but the
	// re-run would just re-emit the same baked-in bytes. The
	// configure_file / file(GENERATE) legacy shapes follow the
	// same rule; this branch matches them. Lifted branch below
	// sets Srcs because the lifted cmd references it via
	// $(location srcRel).
	legacy := ir.Target{
		Name:        name,
		Kind:        ir.KindGenrule,
		GenruleCmd:  configureFileLegacyCmd(dstRel, rendered),
		GenruleOuts: []string{dstRel},
		Tags:        cmakeEConfigureFileTags(false),
		Visibility:  []string{"//visibility:private"},
	}
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
	cmd, err := configureFileLiftedCmd(srcRel, dstRel, values, opts)
	if err != nil {
		return legacy
	}
	return ir.Target{
		Name:         name,
		Kind:         ir.KindGenrule,
		Srcs:         []string{srcRel},
		GenruleCmd:   cmd,
		GenruleOuts:  []string{dstRel},
		GenruleTools: []string{"//tools:cmake-configure-file"},
		Tags:         cmakeEConfigureFileTags(true),
		Visibility:   []string{"//visibility:private"},
	}
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
//   - WORKING_DIRECTORY, ENVIRONMENT, TIMEOUT are not yet
//     modeled — refuse the lift if any are set so a hoisted
//     genrule never silently drops them. Adding support is a
//     follow-on once a real fixture exercises the shape.
//
// Driver tag: cmake-codegen-driver=<basename(argv[0])> mirrors
// the genrule.go custom-command recovery so existing audit
// queries that filter on driver= pick up hoisted rules.
func liftFileProducing(call shadow.ExecuteProcessCall, hostSrcDir, recordedSrcDir, recordedBuildDir string, cc *codegenContext) ([]string, string, bool) {
	if call.WorkingDirectory != "" {
		return nil, "WORKING_DIRECTORY is not yet modeled by the file-producing lifter", false
	}
	if len(call.Environment) > 0 {
		return nil, "ENVIRONMENT is not yet modeled by the file-producing lifter", false
	}
	if call.Timeout != "" {
		return nil, "TIMEOUT is not yet modeled by the file-producing lifter", false
	}
	if call.InputFile != "" || call.ErrorFile != "" {
		return nil, "INPUT_FILE / ERROR_FILE are not yet modeled by the file-producing lifter", false
	}

	dstRel, ok := executeProcessAnchorOutput(call.OutputFile, recordedBuildDir)
	if !ok {
		return nil, fmt.Sprintf("OUTPUT_FILE %q is not under the build dir", call.OutputFile), false
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
		if rel, ok := executeProcessAnchorSource(a, hostSrcDir, recordedSrcDir); ok {
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
			isDir := rel == "" || isExistingDir(filepath.Join(hostSrcDir, rel))
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

	cmd := fmt.Sprintf(`mkdir -p "$$(dirname "$@")" && %s > "$@"`, strings.Join(rewritten, " "))
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
		GenruleOuts: []string{dstRel},
		Tags:        fileProducingTags(driver),
		Visibility:  []string{"//visibility:private"},
	})
	cc.OutToGenrule[dstRel] = name
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
func executeProcessAnchorOutput(p, recordedBuildDir string) (string, bool) {
	if !filepath.IsAbs(p) {
		return "", false
	}
	return relativeIfInsideRelaxed(recordedBuildDir, p)
}

// executeProcessAnchorSource tries to resolve a recorded
// absolute path as a source-root-relative slash path. Returns
// ("", false) when the path is relative or resolves outside
// the source root. We accept either the host-real source path
// (the recording machine's view) OR the recorded source path
// — offline fixtures keep both consistent, but production runs
// the recorder and the converter on the same machine so
// recordedSrcDir == hostSrcDir.
func executeProcessAnchorSource(p, hostSrcDir, recordedSrcDir string) (string, bool) {
	if !filepath.IsAbs(p) {
		return "", false
	}
	if rel, ok := relativeIfInside(recordedSrcDir, p); ok {
		return rel, true
	}
	if hostSrcDir != "" && hostSrcDir != recordedSrcDir {
		if rel, ok := relativeIfInside(hostSrcDir, p); ok {
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
	rel = filepath.ToSlash(rel)
	rel = strings.TrimPrefix(rel, "./")
	var sb strings.Builder
	sb.WriteString("exec_")
	for i := 0; i < len(rel); i++ {
		c := rel[i]
		switch {
		case (c >= 'a' && c <= 'z'),
			(c >= 'A' && c <= 'Z'),
			(c >= '0' && c <= '9'),
			c == '_':
			sb.WriteByte(c)
		default:
			sb.WriteByte('_')
		}
	}
	return sb.String()
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
