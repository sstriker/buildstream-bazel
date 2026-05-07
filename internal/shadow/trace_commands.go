package shadow

// Extended trace-event extractors.
//
// The base ExtractReadPaths walks cmake's --trace-expand JSON
// stream for read-causing commands (include / configure_file /
// file READ etc.) and returns source-tree paths cmake actually
// consumed. The extractors below pull richer per-command
// records out of the same stream — used by lower's converter
// to surface PUBLIC/PRIVATE visibility on
// target_include_directories, IMPORTED-target deps from
// target_link_libraries (which the codemodel drops on the
// floor for static libs), and configure_file input→output
// pairings (which the codemodel records the input only).
//
// Each extractor filters trace events to those firing inside
// the project's source tree — cmake's own internal calls
// (CMakeSystem.cmake.in's configure_file, TryCompile-XYZ's
// target_link_libraries, etc.) live under /usr/share/cmake-*
// or the build dir's CMakeFiles/CMakeScratch/ scratch space
// and aren't part of the user's project intent.

import (
	"sort"
	"strings"
)

// Decoded is the combined result of one ParseTrace pass dispatched
// through every extractor. Lower's converter (which historically
// called three Extract* functions back-to-back on the same trace)
// uses this to pay the bytes.Split + json.Unmarshal cost once.
type Decoded struct {
	Reads            []string
	Includes         []TargetIncludeCall
	Links            []TargetLinkCall
	ConfigFiles      []ConfigureFileCall
	ExecuteProcesses []ExecuteProcessCall
}

// Decode walks the trace once and dispatches every event to all
// extractors at the same time. Equivalent in result to calling
// ExtractReadPaths + ExtractTargetIncludes + ExtractTargetLinks +
// ExtractConfigureFiles + ExtractExecuteProcess on the same trace,
// but pays the parse cost once rather than per extractor. Reads
// is the deduped slash-style source-tree path list; the four
// call lists (Includes / Links / ConfigFiles / ExecuteProcesses)
// preserve insertion order from the trace.
func Decode(traceRaw []byte, sourceRoot string, knownTargets map[string]bool) Decoded {
	events := ParseTrace(traceRaw)
	reads := map[string]struct{}{}
	var d Decoded
	for _, ev := range events {
		collectReadPath(ev, sourceRoot, reads)
		if call, ok := classifyTargetIncludes(ev, sourceRoot, knownTargets); ok {
			d.Includes = append(d.Includes, call)
		}
		if call, ok := classifyTargetLinks(ev, sourceRoot, knownTargets); ok {
			d.Links = append(d.Links, call)
		}
		if call, ok := classifyConfigureFile(ev, sourceRoot); ok {
			d.ConfigFiles = append(d.ConfigFiles, call)
		}
		if call, ok := classifyExecuteProcess(ev, sourceRoot); ok {
			d.ExecuteProcesses = append(d.ExecuteProcesses, call)
		}
	}
	d.Reads = make([]string, 0, len(reads))
	for k := range reads {
		d.Reads = append(d.Reads, k)
	}
	sort.Strings(d.Reads)
	return d
}

// TargetIncludeCall records one user-written
// target_include_directories(target [SYSTEM] [AFTER|BEFORE]
//
//	<PUBLIC|PRIVATE|INTERFACE> dir1 dir2 ...
//	<PUBLIC|PRIVATE|INTERFACE> dir3 ...)
//
// trace event. Each visibility-keyword group becomes a separate
// entry in Groups so the consumer can tell which dirs came from
// which arm. The codemodel's flat compileGroups[].includes[]
// loses this distinction; the trace preserves it.
type TargetIncludeCall struct {
	Target string
	Groups []TargetIncludeGroup
}

// TargetIncludeGroup is one PUBLIC / PRIVATE / INTERFACE arm of
// a target_include_directories call. SystemFlag carries the
// optional SYSTEM keyword's presence; Order ("BEFORE" / "AFTER"
// / "" for default) reflects the optional ordering keyword.
type TargetIncludeGroup struct {
	Visibility string // "PUBLIC", "PRIVATE", "INTERFACE"
	Dirs       []string
	System     bool
	Order      string
}

// TargetLinkCall records one user-written
// target_link_libraries(target
//
//	<PUBLIC|PRIVATE|INTERFACE> lib1 lib2 ...) call. Same shape
//
// as TargetIncludeCall: visibility groups preserve the
// keyword arms.
//
// IMPORTED-target deps that don't surface in the codemodel for
// static libs (because static libs archive rather than link)
// surface here intact — this is how lower closes the find-
// package STATIC delta.
type TargetLinkCall struct {
	Target string
	Groups []TargetLinkGroup
}

// TargetLinkGroup is one PUBLIC / PRIVATE / INTERFACE arm of a
// target_link_libraries call. Visibility "" indicates the legacy
// positional form (no keyword; treated as PUBLIC per cmake semantics).
type TargetLinkGroup struct {
	Visibility string // "PUBLIC", "PRIVATE", "INTERFACE", or "" for the legacy positional shape
	Libs       []string
}

// ConfigureFileCall records one user-written
// configure_file(<input> <output> [...flags...]) call. Args
// are stored as the literal trace-recorded strings; callers
// resolve relative paths against the source root (input) or
// build dir (output) per cmake semantics.
type ConfigureFileCall struct {
	Input   string
	Output  string
	Options []string // any trailing flags: @ONLY, COPYONLY, ESCAPE_QUOTES, NEWLINE_STYLE ..., etc.
}

// ExtractTargetIncludes returns one entry per user-written
// target_include_directories trace event whose `file` is
// inside sourceRoot OR whose target name is in
// knownTargets. The second arm catches calls invoked from
// producer-element cmake macros (e.g. ECM's ecm_add_library)
// that act on consumer-defined targets — those macros live
// outside the consumer source tree, so the file-path filter
// alone would drop them. cmake-internal calls (TryCompile
// scratch in build dir, /usr/share/cmake-* modules, etc.)
// don't act on consumer targets and so are still filtered.
//
// knownTargets may be nil; nil disables the second arm and
// the filter behaves as a strict source-tree check.
func ExtractTargetIncludes(traceRaw []byte, sourceRoot string, knownTargets map[string]bool) []TargetIncludeCall {
	var out []TargetIncludeCall
	for _, ev := range ParseTrace(traceRaw) {
		if call, ok := classifyTargetIncludes(ev, sourceRoot, knownTargets); ok {
			out = append(out, call)
		}
	}
	return out
}

// classifyTargetIncludes returns the TargetIncludeCall corresponding
// to a single trace event, or (_, false) if the event isn't a
// user-scoped target_include_directories. Shared between the legacy
// per-extractor API and Decode's combined pass.
func classifyTargetIncludes(ev TraceEvent, sourceRoot string, knownTargets map[string]bool) (TargetIncludeCall, bool) {
	if !strings.EqualFold(ev.Cmd, "target_include_directories") {
		return TargetIncludeCall{}, false
	}
	if len(ev.Args) < 2 {
		return TargetIncludeCall{}, false
	}
	if !inScopeForTarget(ev.File, sourceRoot, ev.Args[0], knownTargets) {
		return TargetIncludeCall{}, false
	}
	call := TargetIncludeCall{Target: ev.Args[0]}
	// Walk args after the target name; group dirs under
	// their preceding visibility keyword. Optional SYSTEM /
	// AFTER / BEFORE keywords prefix the visibility group.
	// Per cmake docs: SYSTEM applies to all subsequent
	// visibility groups in the same call; we approximate
	// by attaching it to the next group we see.
	var pendingSystem bool
	var pendingOrder string
	var current *TargetIncludeGroup
	for _, a := range ev.Args[1:] {
		switch strings.ToUpper(a) {
		case "SYSTEM":
			pendingSystem = true
			continue
		case "AFTER", "BEFORE":
			pendingOrder = strings.ToUpper(a)
			continue
		case "PUBLIC", "PRIVATE", "INTERFACE":
			if current != nil {
				call.Groups = append(call.Groups, *current)
			}
			current = &TargetIncludeGroup{
				Visibility: strings.ToUpper(a),
				System:     pendingSystem,
				Order:      pendingOrder,
			}
			pendingSystem = false
			pendingOrder = ""
			continue
		}
		if current == nil {
			// Bare positional dirs without a visibility
			// keyword: PRIVATE per cmake's pre-3.0
			// shape. Treat as PRIVATE.
			current = &TargetIncludeGroup{
				Visibility: "PRIVATE",
				System:     pendingSystem,
				Order:      pendingOrder,
			}
			pendingSystem = false
			pendingOrder = ""
		}
		// Unwrap $<BUILD_INTERFACE:X> → X and drop
		// $<INSTALL_INTERFACE:Y> entries (build-time
		// converter context). The codemodel already
		// resolved these generator expressions for the
		// build config; the trace records them
		// pre-resolution. This unwrap brings the trace
		// view into alignment with the codemodel view so
		// downstream consumers can match dir-strings
		// directly.
		resolved, ok := unwrapBuildInterface(a)
		if !ok {
			continue
		}
		current.Dirs = append(current.Dirs, resolved)
	}
	if current != nil {
		call.Groups = append(call.Groups, *current)
	}
	if len(call.Groups) == 0 {
		return TargetIncludeCall{}, false
	}
	return call, true
}

// ExtractTargetLinks returns one entry per user-written
// target_link_libraries trace event whose `file` is inside
// sourceRoot OR whose target name is in knownTargets. The
// macro-from-import case (a producer element's cmake module
// calls target_link_libraries on a consumer target) needs
// the second arm — the macro lives outside the consumer
// source tree so the file-path filter alone would drop the
// call.
//
// knownTargets may be nil; nil disables the second arm and
// the filter behaves as a strict source-tree check.
//
// The legacy positional shape `target_link_libraries(target
// libA libB)` (no visibility keyword) groups all libs under
// Visibility="" so consumers can match on Visibility==""
// without writing a special case.
func ExtractTargetLinks(traceRaw []byte, sourceRoot string, knownTargets map[string]bool) []TargetLinkCall {
	var out []TargetLinkCall
	for _, ev := range ParseTrace(traceRaw) {
		if call, ok := classifyTargetLinks(ev, sourceRoot, knownTargets); ok {
			out = append(out, call)
		}
	}
	return out
}

func classifyTargetLinks(ev TraceEvent, sourceRoot string, knownTargets map[string]bool) (TargetLinkCall, bool) {
	if !strings.EqualFold(ev.Cmd, "target_link_libraries") {
		return TargetLinkCall{}, false
	}
	if len(ev.Args) < 2 {
		return TargetLinkCall{}, false
	}
	if !inScopeForTarget(ev.File, sourceRoot, ev.Args[0], knownTargets) {
		return TargetLinkCall{}, false
	}
	call := TargetLinkCall{Target: ev.Args[0]}
	var current *TargetLinkGroup
	for _, a := range ev.Args[1:] {
		switch strings.ToUpper(a) {
		case "PUBLIC", "PRIVATE", "INTERFACE":
			if current != nil {
				call.Groups = append(call.Groups, *current)
			}
			current = &TargetLinkGroup{Visibility: strings.ToUpper(a)}
			continue
		}
		if current == nil {
			// Legacy positional shape — start an unkeyed group.
			current = &TargetLinkGroup{Visibility: ""}
		}
		current.Libs = append(current.Libs, a)
	}
	if current != nil {
		call.Groups = append(call.Groups, *current)
	}
	if len(call.Groups) == 0 {
		return TargetLinkCall{}, false
	}
	return call, true
}

// ExtractConfigureFiles returns one entry per user-written
// configure_file call in the source tree. The trace records
// args as resolved strings (variables already expanded);
// callers resolve relative paths against the source dir
// (input) or build dir (output) per cmake's conventions.
func ExtractConfigureFiles(traceRaw []byte, sourceRoot string) []ConfigureFileCall {
	var out []ConfigureFileCall
	for _, ev := range ParseTrace(traceRaw) {
		if call, ok := classifyConfigureFile(ev, sourceRoot); ok {
			out = append(out, call)
		}
	}
	return out
}

func classifyConfigureFile(ev TraceEvent, sourceRoot string) (ConfigureFileCall, bool) {
	if !strings.EqualFold(ev.Cmd, "configure_file") {
		return ConfigureFileCall{}, false
	}
	if !inSourceTree(ev.File, sourceRoot) {
		return ConfigureFileCall{}, false
	}
	if len(ev.Args) < 2 {
		return ConfigureFileCall{}, false
	}
	return ConfigureFileCall{
		Input:   ev.Args[0],
		Output:  ev.Args[1],
		Options: append([]string(nil), ev.Args[2:]...),
	}, true
}

// inScopeForTarget combines two checks for whether a trace
// event is part of the user's project intent:
//
//  1. The call's `file` lives inside the project source tree
//     (the typical CMakeLists case).
//  2. The call's first argument names a target the consumer
//     defined (the macro-from-import case: a producer
//     element's .cmake module, staged outside the consumer
//     source tree, modifies a consumer-defined target).
//
// Returns true when either check passes. Used by the
// target_include_directories / target_link_libraries
// extractors to keep producer-macro calls that the strict
// file-path filter alone would drop.
func inScopeForTarget(file, sourceRoot, target string, knownTargets map[string]bool) bool {
	if inSourceTree(file, sourceRoot) {
		return true
	}
	return target != "" && knownTargets[target]
}

// inSourceTree reports whether the trace event's `file` (the
// CMakeLists / .cmake module that issued the call) lives inside
// the project's source root. Filters out cmake's bundled
// modules under /usr/share/cmake-* and TryCompile-* scratch
// CMakeLists in the build dir.
func inSourceTree(file, sourceRoot string) bool {
	if file == "" || sourceRoot == "" {
		return false
	}
	// cmake records absolute paths in the trace's "file" field.
	// Use a string-prefix check rather than filepath.Rel because
	// we're comparing host-absolute paths, not symlink-resolved
	// canonical paths.
	if !strings.HasPrefix(file, sourceRoot) {
		return false
	}
	tail := file[len(sourceRoot):]
	return tail == "" || tail[0] == '/' || tail[0] == '\\'
}

// ExecuteProcessCall records one user-written
// execute_process(...) trace event. cmake's
// `execute_process` runs an arbitrary subprocess at configure
// time — a hermeticity violation by Bazel's analysis-then-action
// model. Surfacing each call (with its argv pipeline, redirect
// targets and writeback variables) lets the converter classify
// it into liftable buckets (cmake -E builtins, file-producing
// commands with declared OUTPUT_FILE) vs unliftable buckets
// (probes, version stamps, opaque pipelines) and emit a typed
// Tier-1 failure for the latter.
//
// v1 captures only the fields the classifier and lifter
// consume:
//   - Commands: one argv list per COMMAND clause; multi-COMMAND
//     forms a pipeline (cmake runs the stages concurrently with
//     stdout chained).
//   - WorkingDirectory / Environment / Timeout: needed when
//     hoisting a command to a build-time genrule.
//   - OutputVariable / ResultVariable / ResultsVariable /
//     ErrorVariable: writeback variables; their presence signals
//     "this call shapes the cmake variable namespace" (the
//     hard-to-lift case for non-stamp uses).
//   - InputFile / OutputFile / ErrorFile: stdio redirect
//     targets; OutputFile is the file-producing bucket's signal.
//   - RawArgs: original token list, for failure-report context
//     when classification refuses.
//
// Out-of-v1 fields (OUTPUT_QUIET, ERROR_QUIET, ENCODING,
// COMMAND_ECHO, COMMAND_ERROR_IS_FATAL, ENVIRONMENT_MODIFICATION,
// *STRIP_TRAILING_WHITESPACE, etc.) are intentionally not
// modeled — none of them affect bucket selection or the lift's
// shape. Add them when a classifier rule needs them.
type ExecuteProcessCall struct {
	File             string
	Line             int
	Commands         [][]string
	WorkingDirectory string
	Timeout          string
	OutputVariable   string
	ResultVariable   string
	ResultsVariable  string
	ErrorVariable    string
	InputFile        string
	OutputFile       string
	ErrorFile        string
	Environment      []string
	RawArgs          []string
}

// ExtractExecuteProcess returns one entry per user-written
// execute_process trace event whose `file` is inside
// sourceRoot. cmake-internal calls (try_compile scratch in the
// build dir, /usr/share/cmake-* probes during project()
// language enabling, etc.) are filtered out — they aren't part
// of the user's project intent and the converter isn't trying
// to "lift" them.
func ExtractExecuteProcess(traceRaw []byte, sourceRoot string) []ExecuteProcessCall {
	var out []ExecuteProcessCall
	for _, ev := range ParseTrace(traceRaw) {
		if call, ok := classifyExecuteProcess(ev, sourceRoot); ok {
			out = append(out, call)
		}
	}
	return out
}

// classifyExecuteProcess parses one trace event into an
// ExecuteProcessCall, or returns (_, false) when the event
// isn't an in-source-tree execute_process. Shared between the
// legacy single-pass API and Decode's combined pass.
//
// cmake's execute_process arg syntax interleaves keywords with
// values; COMMAND clauses form pipelines and consume tokens
// until the next recognized keyword. ENVIRONMENT is variadic:
// it consumes "KEY=value" tokens until another keyword
// appears. Single-value keywords (WORKING_DIRECTORY, TIMEOUT,
// *_VARIABLE, *_FILE) consume exactly one following token.
// Flag-only keywords (OUTPUT_QUIET, ERROR_QUIET,
// *_STRIP_TRAILING_WHITESPACE) take no value.
//
// Unknown tokens at the top level (i.e., not part of an open
// COMMAND or ENVIRONMENT list) are dropped silently — cmake
// adds new options across versions and a stricter rejection
// would force the classifier to refuse otherwise-liftable calls
// just because a v1 hasn't taught the parser about a benign
// recent option.
func classifyExecuteProcess(ev TraceEvent, sourceRoot string) (ExecuteProcessCall, bool) {
	if !strings.EqualFold(ev.Cmd, "execute_process") {
		return ExecuteProcessCall{}, false
	}
	if !inSourceTree(ev.File, sourceRoot) {
		return ExecuteProcessCall{}, false
	}
	if len(ev.Args) == 0 {
		return ExecuteProcessCall{}, false
	}

	call := ExecuteProcessCall{
		File:    ev.File,
		Line:    ev.Line,
		RawArgs: append([]string(nil), ev.Args...),
	}

	// Iteration state: which "open list" the current token
	// extends. cmake's syntax has two variadic clauses
	// (COMMAND and ENVIRONMENT) — bare values append to whichever
	// is currently open; encountering a known keyword closes
	// the open list.
	const (
		listNone = iota
		listCommand
		listEnvironment
	)
	open := listNone
	var currentCmd []string

	flushCommand := func() {
		if len(currentCmd) > 0 {
			call.Commands = append(call.Commands, currentCmd)
		}
		currentCmd = nil
	}

	for i := 0; i < len(ev.Args); i++ {
		a := ev.Args[i]
		switch strings.ToUpper(a) {
		case "COMMAND":
			flushCommand()
			open = listCommand
			continue
		case "WORKING_DIRECTORY":
			flushCommand()
			open = listNone
			if i+1 < len(ev.Args) {
				i++
				call.WorkingDirectory = ev.Args[i]
			}
			continue
		case "TIMEOUT":
			flushCommand()
			open = listNone
			if i+1 < len(ev.Args) {
				i++
				call.Timeout = ev.Args[i]
			}
			continue
		case "RESULT_VARIABLE":
			flushCommand()
			open = listNone
			if i+1 < len(ev.Args) {
				i++
				call.ResultVariable = ev.Args[i]
			}
			continue
		case "RESULTS_VARIABLE":
			flushCommand()
			open = listNone
			if i+1 < len(ev.Args) {
				i++
				call.ResultsVariable = ev.Args[i]
			}
			continue
		case "OUTPUT_VARIABLE":
			flushCommand()
			open = listNone
			if i+1 < len(ev.Args) {
				i++
				call.OutputVariable = ev.Args[i]
			}
			continue
		case "ERROR_VARIABLE":
			flushCommand()
			open = listNone
			if i+1 < len(ev.Args) {
				i++
				call.ErrorVariable = ev.Args[i]
			}
			continue
		case "INPUT_FILE":
			flushCommand()
			open = listNone
			if i+1 < len(ev.Args) {
				i++
				call.InputFile = ev.Args[i]
			}
			continue
		case "OUTPUT_FILE":
			flushCommand()
			open = listNone
			if i+1 < len(ev.Args) {
				i++
				call.OutputFile = ev.Args[i]
			}
			continue
		case "ERROR_FILE":
			flushCommand()
			open = listNone
			if i+1 < len(ev.Args) {
				i++
				call.ErrorFile = ev.Args[i]
			}
			continue
		case "ENVIRONMENT":
			flushCommand()
			open = listEnvironment
			continue
		case "OUTPUT_QUIET",
			"ERROR_QUIET",
			"OUTPUT_STRIP_TRAILING_WHITESPACE",
			"ERROR_STRIP_TRAILING_WHITESPACE":
			flushCommand()
			open = listNone
			continue
		case "COMMAND_ERROR_IS_FATAL",
			"COMMAND_ECHO",
			"ENCODING":
			// Single-value diagnostic options we don't model
			// in v1 but must consume the value so subsequent
			// keyword scanning doesn't trip on it.
			flushCommand()
			open = listNone
			if i+1 < len(ev.Args) {
				i++
			}
			continue
		case "ENVIRONMENT_MODIFICATION":
			// Variadic ops list. Out-of-v1; close any open
			// list and skip until the next recognized keyword.
			flushCommand()
			open = listNone
			for i+1 < len(ev.Args) && !isExecuteProcessKeyword(ev.Args[i+1]) {
				i++
			}
			continue
		}

		// Bare value: append to whichever list is open.
		switch open {
		case listCommand:
			currentCmd = append(currentCmd, a)
		case listEnvironment:
			call.Environment = append(call.Environment, a)
		default:
			// Top-level bare value with no open list — cmake
			// itself would error on this; drop silently here so
			// the converter doesn't mis-read its meaning.
		}
	}
	flushCommand()

	if len(call.Commands) == 0 {
		// No COMMAND clause — defensively skip. cmake itself
		// rejects this shape, so we shouldn't see it in
		// practice, but the parser stays honest about what it
		// captured.
		return ExecuteProcessCall{}, false
	}
	return call, true
}

// isExecuteProcessKeyword reports whether s is one of the
// documented execute_process keyword tokens. Used to bound the
// variadic ENVIRONMENT_MODIFICATION list (and is conservative
// about case to match cmake's case-insensitive keyword
// matching).
func isExecuteProcessKeyword(s string) bool {
	switch strings.ToUpper(s) {
	case "COMMAND",
		"WORKING_DIRECTORY",
		"TIMEOUT",
		"RESULT_VARIABLE",
		"RESULTS_VARIABLE",
		"OUTPUT_VARIABLE",
		"ERROR_VARIABLE",
		"INPUT_FILE",
		"OUTPUT_FILE",
		"ERROR_FILE",
		"OUTPUT_QUIET",
		"ERROR_QUIET",
		"OUTPUT_STRIP_TRAILING_WHITESPACE",
		"ERROR_STRIP_TRAILING_WHITESPACE",
		"COMMAND_ERROR_IS_FATAL",
		"COMMAND_ECHO",
		"ENCODING",
		"ENVIRONMENT",
		"ENVIRONMENT_MODIFICATION":
		return true
	}
	return false
}

// unwrapBuildInterface resolves the build-time view of a
// generator-expression-wrapped argument. Returns:
//   - ($<BUILD_INTERFACE:X>, true) → ("X", true): use X
//   - ($<INSTALL_INTERFACE:Y>, true) → ("", false): drop
//   - any other input → returns the input unchanged + true
//
// Limited to BUILD_INTERFACE / INSTALL_INTERFACE — the only
// genex forms cmake records pre-resolution in trace args for
// target_include_directories. Other genex shapes
// ($<CONFIG:...>, $<COMPILE_LANGUAGE:...>, ...) cmake
// already evaluates against the trace's invocation context
// before recording, so they don't surface here.
func unwrapBuildInterface(arg string) (string, bool) {
	const buildPrefix = "$<BUILD_INTERFACE:"
	const installPrefix = "$<INSTALL_INTERFACE:"
	if strings.HasPrefix(arg, buildPrefix) && strings.HasSuffix(arg, ">") {
		inner := arg[len(buildPrefix) : len(arg)-1]
		return inner, true
	}
	if strings.HasPrefix(arg, installPrefix) && strings.HasSuffix(arg, ">") {
		// Build-time consumer doesn't see install-interface args.
		return "", false
	}
	return arg, true
}
