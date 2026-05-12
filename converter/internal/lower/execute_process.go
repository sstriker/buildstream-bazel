package lower

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/sstriker/cmake-to-bazel/converter/internal/failure"
	"github.com/sstriker/cmake-to-bazel/converter/ir"
	"github.com/sstriker/cmake-to-bazel/internal/configurefile"
	"github.com/sstriker/cmake-to-bazel/internal/shadow"
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
//     to install_tree.tar via per-target cc_import / sh_binary
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
	case "copy", "copy_if_different":
		return liftCMakeECopy(v.CMakeEOp, args, hostSrcDir, recordedSrcDir, recordedBuildDir, cc)
	case "configure_file":
		return liftCMakeEConfigureFile(args, hostSrcDir, recordedSrcDir, hostBuildDir, recordedBuildDir, liftEnabled, cmakeVars, cc)
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
	// recoverConfigureFiles's path-resolution shape).
	templatePath := filepath.Join(hostSrcDir, srcRel)
	template, terr := os.ReadFile(templatePath)
	if terr != nil {
		return nil, fmt.Sprintf("cmake -E configure_file: can't read template %q: %v", srcRel, terr), false
	}
	rendered, rerr := os.ReadFile(filepath.Join(hostBuildDir, dstRel))
	if rerr != nil {
		return nil, fmt.Sprintf("cmake -E configure_file: can't read rendered output %q: %v", dstRel, rerr), false
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
	legacy := ir.Target{
		Name:        name,
		Kind:        ir.KindGenrule,
		Srcs:        []string{srcRel},
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
