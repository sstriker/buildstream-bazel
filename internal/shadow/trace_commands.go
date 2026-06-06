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
	Reads                      []string
	Includes                   []TargetIncludeCall
	Links                      []TargetLinkCall
	CompileDefinitions         []TargetCompileCall
	CompileOptions             []TargetCompileCall
	ConfigFiles                []ConfigureFileCall
	FileGenerates              []FileGenerateCall
	ExecuteProcesses           []ExecuteProcessCall
	PlatformConditionalSources []PlatformConditionalSource
	SourceFileProperties       []SourceFilePropertiesCall
	AddCustomCommands          []AddCustomCommandCall
	AddCustomTargets           []AddCustomTargetCall
	AddDependencies            []AddDependenciesCall
	AddLibraries               []AddLibraryCall
	InstallExports             []InstallExportCall
	FileGlobs                  []FileGlobCall
}

// Decode walks the trace once and dispatches every event to all
// extractors at the same time. Equivalent in result to calling
// ExtractReadPaths + ExtractTargetIncludes + ExtractTargetLinks +
// ExtractConfigureFiles + ExtractFileGenerate +
// ExtractExecuteProcess + ExtractPlatformConditionalSources on
// the same trace, but pays the parse cost once rather than per
// extractor. Reads is the deduped slash-style source-tree path
// list; the call lists (Includes / Links / ConfigFiles /
// FileGenerates / ExecuteProcesses / PlatformConditionalSources)
// preserve insertion order from the trace.
func Decode(traceRaw []byte, sourceRoot string, knownTargets map[string]bool) Decoded {
	return DecodeWithFS(traceRaw, sourceRoot, sourceRoot, knownTargets, defaultFS{})
}

// DecodeWithFS is the file-system-abstracted variant. Required
// for offline-replay tests where trace-event paths sit under
// traceSourceRoot but the actual CMakeLists files live under
// hostSourceRoot (cmakeparse needs the host-side bytes for Tier
// 1 platform-conditional scope tracking).
//
// In-process callers (the converter) pass traceSourceRoot ==
// hostSourceRoot since cmake just ran on the same machine.
func DecodeWithFS(traceRaw []byte, traceSourceRoot, hostSourceRoot string, knownTargets map[string]bool, fs fsReader) Decoded {
	events := ParseTrace(traceRaw)
	reads := map[string]struct{}{}
	var d Decoded
	// Tier 1 platform-conditional scope tracking is dispatched
	// on the trace's shape — see extractPlatformConditionalSources
	// in platform_conditional.go. Production cmake's
	// --trace-format=json-v1 omits endif events so the
	// cmakeparse path activates; synthetic test traces that
	// emit endif use the legacy trace-event stack.
	traceHasEndif := hasEndifEvent(events)
	tier1Stack := newPlatformIfStack()
	tier1Idx := newCmakeFileIfIndex()
	for _, ev := range events {
		collectReadPath(ev, traceSourceRoot, reads)
		if call, ok := classifyTargetIncludes(ev, traceSourceRoot, knownTargets); ok {
			d.Includes = append(d.Includes, call)
		}
		if call, ok := classifyTargetLinks(ev, traceSourceRoot, knownTargets); ok {
			d.Links = append(d.Links, call)
		}
		if call, ok := classifyTargetCompile(ev, traceSourceRoot, knownTargets, "target_compile_definitions"); ok {
			d.CompileDefinitions = append(d.CompileDefinitions, call)
		}
		if call, ok := classifyTargetCompile(ev, traceSourceRoot, knownTargets, "target_compile_options"); ok {
			d.CompileOptions = append(d.CompileOptions, call)
		}
		if call, ok := classifyConfigureFile(ev, traceSourceRoot); ok {
			d.ConfigFiles = append(d.ConfigFiles, call)
		}
		if call, ok := classifyFileRename(ev, traceSourceRoot); ok {
			d.ConfigFiles = append(d.ConfigFiles, call)
		}
		if call, ok := classifyFileGenerate(ev, traceSourceRoot); ok {
			d.FileGenerates = append(d.FileGenerates, call)
		}
		if call, ok := classifyFileGlob(ev, traceSourceRoot); ok {
			d.FileGlobs = append(d.FileGlobs, call)
		}
		if call, ok := classifyExecuteProcess(ev, traceSourceRoot); ok {
			d.ExecuteProcesses = append(d.ExecuteProcesses, call)
		}
		if call, ok := classifySourceFileProperties(ev, traceSourceRoot); ok {
			d.SourceFileProperties = append(d.SourceFileProperties, call)
		}
		if call, ok := classifyAddCustomCommand(ev, traceSourceRoot); ok {
			d.AddCustomCommands = append(d.AddCustomCommands, call)
		}
		if call, ok := classifyAddCustomTarget(ev, traceSourceRoot); ok {
			d.AddCustomTargets = append(d.AddCustomTargets, call)
		}
		if call, ok := classifyAddDependencies(ev, traceSourceRoot); ok {
			d.AddDependencies = append(d.AddDependencies, call)
		}
		if call, ok := classifyAddLibrary(ev, traceSourceRoot); ok {
			d.AddLibraries = append(d.AddLibraries, call)
		}
		if call, ok := classifyInstallExport(ev); ok {
			d.InstallExports = append(d.InstallExports, call)
		}
		if traceHasEndif {
			d.PlatformConditionalSources = maybeCollectPlatformConditionalSourceTraceStack(ev, tier1Stack, traceSourceRoot, knownTargets, d.PlatformConditionalSources)
		} else {
			d.PlatformConditionalSources = maybeCollectPlatformConditionalSource(ev, tier1Idx, traceSourceRoot, hostSourceRoot, fs, knownTargets, d.PlatformConditionalSources)
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

// TargetCompileCall records one user-written
// target_compile_definitions / target_compile_options call:
//
//	<cmd>(target
//	  <PUBLIC|PRIVATE|INTERFACE> item1 item2 ...
//	  <PUBLIC|PRIVATE|INTERFACE> item3 ...)
//
// Shape mirrors TargetLinkCall — each visibility-keyword group
// is one entry in Groups. Used by the INTERFACE_* aggregation
// pipeline in lower/buildGenexTargets to recover the
// per-target direct INTERFACE_COMPILE_DEFINITIONS /
// INTERFACE_COMPILE_OPTIONS contribution (PUBLIC + INTERFACE
// arms), then walk codemodel Dependencies[] transitively.
//
// The codemodel's CompileGroups[].Defines /
// CompileCommandFragments record the POST-aggregation set
// (i.e. what's actually fed to the compiler for this target's
// own sources), with no per-visibility distinction; the trace
// preserves the keyword arms so we can pick out just the
// contributions that propagate to consumers.
type TargetCompileCall struct {
	Cmd    string // lowercase command name ("target_compile_definitions" / "target_compile_options")
	Target string
	Groups []TargetCompileGroup
}

// TargetCompileGroup is one PUBLIC / PRIVATE / INTERFACE arm
// of a target_compile_definitions / target_compile_options
// call. Visibility "" indicates the legacy positional form
// (no keyword); cmake treats those as PRIVATE-equivalent for
// the compile-time effect — they don't propagate to consumers.
type TargetCompileGroup struct {
	Visibility string   // "PUBLIC", "PRIVATE", "INTERFACE", or "" for the legacy positional shape
	Items      []string // raw trace-recorded items (defines: "FOO=1" or "FOO"; options: "-Wall" etc.)
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
	// CallFile is the absolute path of the CMakeLists that made the call
	// (the trace event's `file`). cmake resolves a RELATIVE Output against
	// CMAKE_CURRENT_BINARY_DIR — the build-dir mirror of this file's
	// directory — so the recovery needs it to anchor relative outputs (the
	// ubiquitous `configure_file(config.h.in config.h)` autotools idiom).
	CallFile string
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

// ExtractTargetCompile returns one entry per user-written
// target_compile_definitions / target_compile_options trace
// event whose `file` is inside sourceRoot OR whose target
// name is in knownTargets (the same scoping rule as
// ExtractTargetIncludes / ExtractTargetLinks). cmd selects
// which command family to extract — "target_compile_definitions"
// or "target_compile_options". Other commands return empty.
//
// Used by lower/buildGenexTargets to recover the per-target
// direct INTERFACE_COMPILE_DEFINITIONS / INTERFACE_COMPILE_OPTIONS
// contribution (PUBLIC + INTERFACE arms) before walking
// codemodel Dependencies[] for the transitive aggregate.
func ExtractTargetCompile(traceRaw []byte, sourceRoot string, knownTargets map[string]bool, cmd string) []TargetCompileCall {
	var out []TargetCompileCall
	for _, ev := range ParseTrace(traceRaw) {
		if call, ok := classifyTargetCompile(ev, sourceRoot, knownTargets, cmd); ok {
			out = append(out, call)
		}
	}
	return out
}

// classifyTargetCompile is the per-event arm of
// ExtractTargetCompile + Decode. The cmd argument selects the
// command name to match — "target_compile_definitions" or
// "target_compile_options". The two commands share the exact
// shape `<cmd>(target [keyword] item item ...)` and differ
// only in which property they set, so one classifier covers
// both with a name filter.
//
// Same scoping rule as classifyTargetLinks: the event must
// either originate in the source tree OR the target name
// must be in knownTargets (catches calls from producer-element
// macros acting on consumer targets).
func classifyTargetCompile(ev TraceEvent, sourceRoot string, knownTargets map[string]bool, cmd string) (TargetCompileCall, bool) {
	if !strings.EqualFold(ev.Cmd, cmd) {
		return TargetCompileCall{}, false
	}
	if len(ev.Args) < 2 {
		return TargetCompileCall{}, false
	}
	if !inScopeForTarget(ev.File, sourceRoot, ev.Args[0], knownTargets) {
		return TargetCompileCall{}, false
	}
	call := TargetCompileCall{Cmd: strings.ToLower(cmd), Target: ev.Args[0]}
	var current *TargetCompileGroup
	for _, a := range ev.Args[1:] {
		switch strings.ToUpper(a) {
		case "PUBLIC", "PRIVATE", "INTERFACE":
			if current != nil {
				call.Groups = append(call.Groups, *current)
			}
			current = &TargetCompileGroup{Visibility: strings.ToUpper(a)}
			continue
		}
		if current == nil {
			// Legacy positional shape — cmake treats these as
			// PRIVATE-equivalent (no propagation to consumers).
			// Emit them under Visibility="" so callers can
			// distinguish from an explicit PRIVATE if they need
			// to.
			current = &TargetCompileGroup{Visibility: ""}
		}
		current.Items = append(current.Items, a)
	}
	if current != nil {
		call.Groups = append(call.Groups, *current)
	}
	if len(call.Groups) == 0 {
		return TargetCompileCall{}, false
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
	if len(ev.Args) < 2 {
		return ConfigureFileCall{}, false
	}
	// Normally a configure_file is only interesting when its CALL SITE is
	// inside the project tree — that filters out cmake's own internal
	// configure_file calls (try_compile scratch, package-config helpers,
	// etc.) whose outputs aren't project compile inputs.
	//
	// The one universal exception is generate_export_header (CMake's
	// GenerateExportHeader module): it calls configure_file from cmake's
	// own module dir against the fixed template "exportheader.cmake.in",
	// but its output <name>_export.h is a per-target compile header every
	// consumer #includes. Recognizing it by that stable template basename
	// recovers the (baked) header instead of dropping it, while keeping
	// every other out-of-tree configure_file filtered — the same
	// recognize-a-known-idiom move as the cc_embed encoder list.
	if !inSourceTree(ev.File, sourceRoot) && !isGenerateExportHeaderTemplate(ev.Args[0]) {
		return ConfigureFileCall{}, false
	}
	return ConfigureFileCall{
		Input:    ev.Args[0],
		Output:   ev.Args[1],
		Options:  append([]string(nil), ev.Args[2:]...),
		CallFile: ev.File,
	}, true
}

// classifyFileRename models cmake's `file(RENAME <src> <dest>)` — the
// "atomically materialize a generated file" idiom — as a synthetic
// COPYONLY configure_file whose <dest> bytes are baked verbatim (the
// configure_file recovery reads them from the build dir). OpenBLAS's
// deterministic-arch (cross-compile) branch writes config.h this way
// (cmake/prebuild.cmake: file(WRITE ...tmp) + APPENDs, then
// file(RENAME tmp config.h)), whereas its non-cross branch uses
// configure_file(... COPYONLY) — so the existing configure_file recovery
// only ever saw config.h on the path the build lens doesn't take. Treating
// RENAME as COPYONLY routes config.h through the identical bake + consumer
// attribution. Only the call site being in the source tree qualifies, so
// cmake-internal renames (try_compile scratch, etc.) are filtered; renames
// whose dest lands outside the build dir are dropped later by the recovery
// (relativeIfInsideRelaxed), so a source-tree-dest rename can't bake.
func classifyFileRename(ev TraceEvent, sourceRoot string) (ConfigureFileCall, bool) {
	if !strings.EqualFold(ev.Cmd, "file") {
		return ConfigureFileCall{}, false
	}
	if len(ev.Args) != 3 || !strings.EqualFold(ev.Args[0], "RENAME") {
		return ConfigureFileCall{}, false
	}
	if !inSourceTree(ev.File, sourceRoot) {
		return ConfigureFileCall{}, false
	}
	return ConfigureFileCall{
		Input:    ev.Args[1],
		Output:   ev.Args[2],
		Options:  []string{"COPYONLY"},
		CallFile: ev.File,
	}, true
}

// isGenerateExportHeaderTemplate reports whether the configure_file input is
// CMake's GenerateExportHeader template (exportheader.cmake.in). cmake always
// passes it as an absolute, module-dir-prefixed path, so a "/"-anchored suffix
// match is precise. Backslashes are normalized first so a trace recorded with
// Windows separators still matches (cmake can emit either).
func isGenerateExportHeaderTemplate(input string) bool {
	return strings.HasSuffix(strings.ReplaceAll(input, "\\", "/"), "/exportheader.cmake.in")
}

// FileGenerateCall records one user-written
// file(GENERATE OUTPUT <out> [INPUT <in> | CONTENT <bytes>]
//
//	[CONDITION <cond>] [TARGET <tgt>] [NEWLINE_STYLE <style>]
//	[NO_SOURCE_PERMISSIONS | USE_SOURCE_PERMISSIONS]
//	[FILE_PERMISSIONS <perms>...] [PERMISSIONS <perms>...]) call.
//
// cmake evaluates file(GENERATE) at generate-time — after
// --trace-expand's variable expansion but before the build
// runs — so the trace records the call with any `$<...>`
// generator expressions still literal in OUTPUT / INPUT /
// CONTENT / CONDITION. The lifter pairs each call with the
// rendered bytes cmake wrote to the build dir at end-of-
// generate (analogous to how configure_file pairs with its
// rendered output), then either lifts to a template+values
// genrule when the call is genex-free and the verify-pass
// reproduces the bytes, or falls back to the legacy bytes-
// embedded shape.
//
// Mutual exclusion: cmake itself rejects a call that
// declares both INPUT and CONTENT, so exactly one is set
// on a well-formed call. The classifier mirrors that by
// dropping both-keywords-present calls as malformed, so
// downstream consumers can rely on HasInput XOR HasContent
// after a successful classify.
//
// HasInput / HasContent track whether the keyword was
// PRESENT in the trace args, independent of whether its
// value was the empty string. cmake accepts and emits
// `file(GENERATE OUTPUT ... CONTENT "")` (a legitimate
// empty-file emission) and the lifter has to distinguish
// that from `no CONTENT keyword supplied at all`. String-
// emptiness as the discriminator would collapse the two
// shapes and route the empty-body case to legacy fallback.
//
// v1 captures only the fields the lifter consumes. PERMISSIONS
// / FILE_PERMISSIONS / USE_SOURCE_PERMISSIONS /
// NO_SOURCE_PERMISSIONS affect mode bits, not content bytes
// — Bazel's genrule sets its own mode and downstream
// compilers don't care about config.h-shape header modes,
// so they're parsed-and-dropped like configure_file's
// permission tokens. NewlineStyle is captured because cmake
// re-emits line terminators per the style choice even when
// CONTENT is verbatim, so the lifter has to feed it to
// configurefile.Substitute for byte-equal verify-pass.
type FileGenerateCall struct {
	File         string
	Line         int
	Output       string
	Input        string // populated when HasInput == true
	HasInput     bool   // true iff the INPUT keyword was present in the trace args
	Content      string // populated when HasContent == true; may be the empty string
	HasContent   bool   // true iff the CONTENT keyword was present in the trace args
	Condition    string // empty if no CONDITION clause
	Target       string
	NewlineStyle string // "" / "UNIX" / "LF" / "DOS" / "WIN32" / "CRLF" — passed through verbatim
	RawArgs      []string
}

// ExtractFileGenerate returns one entry per user-written
// file(GENERATE ...) trace event whose `file` is inside
// sourceRoot. cmake-internal file(GENERATE) usage is rare
// (the command is user-facing by design), but the
// source-tree filter keeps the extractor honest with the
// configure_file / execute_process siblings.
func ExtractFileGenerate(traceRaw []byte, sourceRoot string) []FileGenerateCall {
	var out []FileGenerateCall
	for _, ev := range ParseTrace(traceRaw) {
		if call, ok := classifyFileGenerate(ev, sourceRoot); ok {
			out = append(out, call)
		}
	}
	return out
}

// classifyFileGenerate parses one trace event into a
// FileGenerateCall, or returns (_, false) when the event
// isn't an in-source-tree file(GENERATE ...). The classifier
// walks the keyword sequence cmake's documentation specifies:
// OUTPUT / INPUT / CONTENT each consume one value;
// CONDITION / TARGET / NEWLINE_STYLE each consume one value;
// PERMISSIONS / FILE_PERMISSIONS are variadic and consume
// tokens until the next recognized keyword; USE_/NO_SOURCE_
// PERMISSIONS are flag-only.
//
// Unknown tokens at the top level are dropped silently —
// matches the execute_process classifier's tolerance for
// cmake-version-added options that don't affect the lift.
func classifyFileGenerate(ev TraceEvent, sourceRoot string) (FileGenerateCall, bool) {
	if !strings.EqualFold(ev.Cmd, "file") {
		return FileGenerateCall{}, false
	}
	if !inSourceTree(ev.File, sourceRoot) {
		return FileGenerateCall{}, false
	}
	if len(ev.Args) == 0 || !strings.EqualFold(ev.Args[0], "GENERATE") {
		return FileGenerateCall{}, false
	}

	call := FileGenerateCall{
		File:    ev.File,
		Line:    ev.Line,
		RawArgs: append([]string(nil), ev.Args...),
	}

	// Skip the "GENERATE" subcommand at index 0; walk keywords.
	for i := 1; i < len(ev.Args); i++ {
		a := ev.Args[i]
		switch strings.ToUpper(a) {
		case "OUTPUT":
			if i+1 < len(ev.Args) {
				i++
				call.Output = ev.Args[i]
			}
			continue
		case "INPUT":
			if i+1 < len(ev.Args) {
				i++
				call.Input = ev.Args[i]
				call.HasInput = true
			}
			continue
		case "CONTENT":
			if i+1 < len(ev.Args) {
				i++
				call.Content = ev.Args[i]
				call.HasContent = true
			}
			continue
		case "CONDITION":
			if i+1 < len(ev.Args) {
				i++
				call.Condition = ev.Args[i]
			}
			continue
		case "TARGET":
			if i+1 < len(ev.Args) {
				i++
				call.Target = ev.Args[i]
			}
			continue
		case "NEWLINE_STYLE":
			if i+1 < len(ev.Args) {
				i++
				call.NewlineStyle = ev.Args[i]
			}
			continue
		case "PERMISSIONS", "FILE_PERMISSIONS":
			// Variadic permission list — consume tokens until
			// the next recognized keyword. v1 ignores the
			// values (mode bits don't affect lift shape).
			for i+1 < len(ev.Args) && !isFileGenerateKeyword(ev.Args[i+1]) {
				i++
			}
			continue
		case "USE_SOURCE_PERMISSIONS",
			"NO_SOURCE_PERMISSIONS":
			// Flag-only — no value to consume.
			continue
		}
		// Unknown top-level token: drop silently (parity with
		// the execute_process classifier's tolerance).
	}

	if call.Output == "" {
		// Malformed call (cmake itself rejects a GENERATE
		// without OUTPUT). Defensive: drop.
		return FileGenerateCall{}, false
	}
	if !call.HasInput && !call.HasContent {
		// cmake requires exactly one of INPUT / CONTENT. Note
		// the keyword-presence check, not the value-emptiness
		// check: `CONTENT ""` is a legitimate empty-file
		// emission and the extractor preserves it.
		return FileGenerateCall{}, false
	}
	if call.HasInput && call.HasContent {
		// cmake rejects both-keywords-present as malformed
		// too. Mirror that here so the lifter's "exactly one
		// of HasInput / HasContent" invariant actually holds.
		return FileGenerateCall{}, false
	}
	return call, true
}

// isFileGenerateKeyword reports whether s is one of the
// documented file(GENERATE) keyword tokens. Used to bound the
// variadic PERMISSIONS / FILE_PERMISSIONS lists. Case-
// insensitive to match cmake's keyword matching.
func isFileGenerateKeyword(s string) bool {
	switch strings.ToUpper(s) {
	case "OUTPUT",
		"INPUT",
		"CONTENT",
		"CONDITION",
		"TARGET",
		"NEWLINE_STYLE",
		"PERMISSIONS",
		"FILE_PERMISSIONS",
		"USE_SOURCE_PERMISSIONS",
		"NO_SOURCE_PERMISSIONS":
		return true
	}
	return false
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

// FileGlobCall records one user-written file(GLOB <var> ...) or
// file(GLOB_RECURSE <var> ...) call. Under cmake --trace-expand the
// globbing expressions arrive already expanded to absolute, wildcarded
// paths (e.g. "/src/data/*.txt"); the matched file list is NOT in the
// trace (it's assigned to <var> internally and only surfaces, expanded,
// where ${var} is later used). Recurse distinguishes GLOB_RECURSE (matches
// beneath the pattern dir) from GLOB (the pattern dir only). Relative is
// set when the RELATIVE option is present — results are then relative to a
// base dir, which the standalone-genrule threader can't match against
// absolute srcs, so those calls are recorded but skipped downstream.
type FileGlobCall struct {
	File     string
	Line     int
	Var      string
	Patterns []string
	Recurse  bool
	Relative bool
	RawArgs  []string
}

// ExtractFileGlobs returns one entry per user-written file(GLOB ...) /
// file(GLOB_RECURSE ...) call whose `file` is inside sourceRoot. cmake's
// own bundled modules glob constantly during project()/compiler probing;
// inSourceTree drops those so only the project's own globs remain.
func ExtractFileGlobs(traceRaw []byte, sourceRoot string) []FileGlobCall {
	var out []FileGlobCall
	for _, ev := range ParseTrace(traceRaw) {
		if call, ok := classifyFileGlob(ev, sourceRoot); ok {
			out = append(out, call)
		}
	}
	return out
}

// classifyFileGlob parses one trace event into a FileGlobCall, or returns
// (_, false) when it isn't an in-source-tree file(GLOB|GLOB_RECURSE ...).
// Argument shape (cmake docs):
//
//	file(GLOB <var> [LIST_DIRECTORIES true|false] [RELATIVE <path>]
//	     [CONFIGURE_DEPENDS] <globbing-expressions>...)
//
// Option keywords are consumed (RELATIVE/LIST_DIRECTORIES each take a
// value; CONFIGURE_DEPENDS/FOLLOW_SYMLINKS are flags); every remaining
// token is a globbing expression. Unknown keywords fall through to the
// pattern list — harmless, since the threader only acts when a pattern's
// match set actually coincides with a genrule's srcs.
func classifyFileGlob(ev TraceEvent, sourceRoot string) (FileGlobCall, bool) {
	if !strings.EqualFold(ev.Cmd, "file") {
		return FileGlobCall{}, false
	}
	if !inSourceTree(ev.File, sourceRoot) {
		return FileGlobCall{}, false
	}
	if len(ev.Args) < 3 {
		return FileGlobCall{}, false
	}
	recurse := strings.EqualFold(ev.Args[0], "GLOB_RECURSE")
	if !recurse && !strings.EqualFold(ev.Args[0], "GLOB") {
		return FileGlobCall{}, false
	}
	call := FileGlobCall{
		File:    ev.File,
		Line:    ev.Line,
		Var:     ev.Args[1],
		Recurse: recurse,
		RawArgs: append([]string(nil), ev.Args...),
	}
	for i := 2; i < len(ev.Args); i++ {
		switch strings.ToUpper(ev.Args[i]) {
		case "LIST_DIRECTORIES":
			i++ // skip the true/false value
		case "RELATIVE":
			call.Relative = true
			i++ // skip the base path
		case "CONFIGURE_DEPENDS", "FOLLOW_SYMLINKS":
			// flag-only
		default:
			call.Patterns = append(call.Patterns, ev.Args[i])
		}
	}
	if len(call.Patterns) == 0 {
		return FileGlobCall{}, false
	}
	return call, true
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

// SourceFilePropertiesCall records one user-written
// set_source_files_properties(<files...>
//
//	[DIRECTORY <dirs...>]
//	[TARGET_DIRECTORY <targets...>]
//	PROPERTIES <prop1> <val1> [<prop2> <val2> ...]) call.
//
// The codemodel surfaces per-source IsGenerated and FileSetIndex but
// has no slot for arbitrary source-file properties (COMPILE_DEFINITIONS,
// COMPILE_OPTIONS, LANGUAGE, HEADER_FILE_ONLY, etc.). The trace
// preserves the full call shape so the lift can route per-file
// COMPILE_DEFINITIONS into a per-file cc_library split (Bazel has no
// per-source-file local_defines), HEADER_FILE_ONLY into hdrs vs srcs
// classification, etc.
//
// Phase 1 of the generator-parity uplift (ROADMAP.md) introduces the
// decoder; consumers in lower/ land separately as each property gets
// a Bazel-side lowering recipe.
type SourceFilePropertiesCall struct {
	// File and Line are the trace-recorded call site (the
	// CMakeLists.txt / .cmake file invoking
	// set_source_files_properties).
	File string
	Line int

	// Files lists the source-file arguments preceding the
	// DIRECTORY / TARGET_DIRECTORY / PROPERTIES keyword. Paths are
	// stored as the trace recorded them — typically project-source-
	// relative for the common idiom but absolute when the caller
	// passed an absolute path. Resolution against the source root
	// is the consumer's responsibility.
	Files []string

	// Directories carries the optional DIRECTORY <dirs...> arm
	// (cmake 3.18+). When non-empty the properties apply to the
	// source files as seen from those directories' scope, not the
	// current call site's directory.
	Directories []string

	// TargetDirectories carries the optional TARGET_DIRECTORY
	// <targets...> arm (cmake 3.18+). The properties apply to the
	// source files in each named target's directory scope.
	TargetDirectories []string

	// Properties is the ordered list of (name, value) pairs from
	// the PROPERTIES section. cmake itself stores each one in the
	// source file's directory-scoped property bag; consumers
	// decide which properties they care about.
	Properties []SourceFileProperty
}

// SourceFileProperty is one (name, value) pair on a
// set_source_files_properties call's PROPERTIES section.
type SourceFileProperty struct {
	Name  string
	Value string
}

// ExtractSourceFileProperties returns one entry per
// set_source_files_properties call whose trace event fires inside
// sourceRoot. Calls with no source files or no properties (malformed
// or interface-only) are skipped.
func ExtractSourceFileProperties(traceRaw []byte, sourceRoot string) []SourceFilePropertiesCall {
	var out []SourceFilePropertiesCall
	for _, ev := range ParseTrace(traceRaw) {
		if call, ok := classifySourceFileProperties(ev, sourceRoot); ok {
			out = append(out, call)
		}
	}
	return out
}

func classifySourceFileProperties(ev TraceEvent, sourceRoot string) (SourceFilePropertiesCall, bool) {
	if !strings.EqualFold(ev.Cmd, "set_source_files_properties") {
		return SourceFilePropertiesCall{}, false
	}
	if !inSourceTree(ev.File, sourceRoot) {
		return SourceFilePropertiesCall{}, false
	}

	call := SourceFilePropertiesCall{
		File: ev.File,
		Line: ev.Line,
	}

	// Walk argv: <files...> [DIRECTORY <d...>] [TARGET_DIRECTORY <t...>] PROPERTIES <n> <v> ...
	// The keyword ordering is loose in cmake — DIRECTORY and
	// TARGET_DIRECTORY can appear in either order before
	// PROPERTIES — but each appears at most once.
	section := "files"
	for i := 0; i < len(ev.Args); i++ {
		a := ev.Args[i]
		switch strings.ToUpper(a) {
		case "DIRECTORY":
			section = "directories"
			continue
		case "TARGET_DIRECTORY":
			section = "target_directories"
			continue
		case "PROPERTIES":
			section = "properties"
			continue
		}
		switch section {
		case "files":
			call.Files = append(call.Files, a)
		case "directories":
			call.Directories = append(call.Directories, a)
		case "target_directories":
			call.TargetDirectories = append(call.TargetDirectories, a)
		case "properties":
			// Pair up (name, value). Drop a dangling name with no
			// value rather than recording an empty-string value
			// that could be confused with an explicit "".
			if i+1 < len(ev.Args) {
				call.Properties = append(call.Properties, SourceFileProperty{
					Name:  a,
					Value: ev.Args[i+1],
				})
				i++
			}
		}
	}

	if len(call.Files) == 0 || len(call.Properties) == 0 {
		// Malformed call (PROPERTIES with no pairs) or
		// directive-only (DIRECTORY/TARGET_DIRECTORY with no
		// source files) — both are unusable.
		return SourceFilePropertiesCall{}, false
	}

	return call, true
}

// AddCustomCommandCall records one user-written
// add_custom_command(OUTPUT <outs...> COMMAND <cmd1...>
//
//	[COMMAND <cmd2...> ...] [DEPENDS <deps...>] [BYPRODUCTS <bps...>]
//	[MAIN_DEPENDENCY <dep>] [WORKING_DIRECTORY <dir>]
//	[COMMENT <comment>] [VERBATIM] [USES_TERMINAL] ...) call.
//
// Bazel-side consumers (the Phase 4 standalone-genrule emitter)
// use the OUTPUT list to cross-reference build.ninja CUSTOM_COMMAND
// edges back to the user's source-level call site — that lets the
// emitted genrule pick up a name derived from the matching
// add_custom_target (when one wraps the OUTPUT) and a visibility
// derived from downstream consumers in the same trace, instead of
// the synthetic `custom_command_<output>` name and the hardcoded
// private visibility the output-only path produces.
//
// v1 captures the fields the cross-reference consumes:
//   - File, Line: source-level call site for error/audit context.
//   - Outputs / ByProducts: identify which build.ninja edge the
//     event corresponds to.
//   - Commands: per-COMMAND argv lists, in declaration order
//     (mirrors execute_process's pipeline shape).
//   - Depends / MainDependency: source-side dependency declarations
//     for completeness.
//   - WorkingDirectory: per cmake semantics, relative paths in
//     Outputs/Commands resolve against this.
//   - RawArgs: original token list for audit context.
//
// Out-of-v1 keyword payloads (VERBATIM, USES_TERMINAL,
// COMMAND_EXPAND_LISTS, JOB_POOL, IMPLICIT_DEPENDS, APPEND, ...)
// don't affect cross-reference matching and aren't modeled. Their
// keyword tokens are still consumed by the parser so subsequent
// keyword scanning stays correct.
type AddCustomCommandCall struct {
	File             string
	Line             int
	Outputs          []string
	ByProducts       []string
	Depends          []string
	MainDependency   string
	Commands         [][]string
	WorkingDirectory string
	Comment          string
	RawArgs          []string
}

// AddCustomTargetCall records one user-written
// add_custom_target(<name> [ALL] [COMMAND <cmd1...>]
//
//	[COMMAND <cmd2...> ...] [DEPENDS <deps...>]
//	[BYPRODUCTS <bps...>] [WORKING_DIRECTORY <dir>]
//	[COMMENT <comment>] [SOURCES <srcs...>] [VERBATIM]
//	[USES_TERMINAL] ...) call.
//
// The Bazel-side consumer pairs this against
// AddCustomCommandCall records via the BYPRODUCTS / DEPENDS
// chain: when an add_custom_command(OUTPUT out) is followed by
// add_custom_target(name DEPENDS out), the target name becomes
// the standalone-genrule name (replacing the synthetic
// `custom_command_<output>` shape).
//
// All marks ALL-targets (built by default); ignored by the
// standalone-genrule cross-reference but recorded for symmetry.
type AddCustomTargetCall struct {
	File             string
	Line             int
	Name             string
	All              bool
	Commands         [][]string
	Depends          []string
	ByProducts       []string
	Sources          []string
	WorkingDirectory string
	Comment          string
	RawArgs          []string
}

// AddDependenciesCall records one user-written
// add_dependencies(<target> <dep1> [<dep2> ...]) call. The
// Bazel-side cross-reference uses this to discover downstream
// consumers of a custom-command output: when target T calls
// add_dependencies(T producer-name) and producer-name names an
// add_custom_target whose OUTPUTs include foo.txt, foo.txt has
// a downstream consumer in the same package and the emitted
// standalone genrule's visibility opens from
// `//visibility:private` to `:__pkg__`.
type AddDependenciesCall struct {
	File    string
	Line    int
	Target  string
	Depends []string
	RawArgs []string
}

// ExtractAddCustomCommands returns one entry per user-written
// add_custom_command call whose trace event fires inside
// sourceRoot. cmake-internal scratch-CMakeLists from try_compile
// etc. are filtered out by the source-tree gate.
func ExtractAddCustomCommands(traceRaw []byte, sourceRoot string) []AddCustomCommandCall {
	var out []AddCustomCommandCall
	for _, ev := range ParseTrace(traceRaw) {
		if call, ok := classifyAddCustomCommand(ev, sourceRoot); ok {
			out = append(out, call)
		}
	}
	return out
}

// classifyAddCustomCommand parses one trace event into an
// AddCustomCommandCall, or returns (_, false) when the event
// isn't an in-source-tree add_custom_command with an OUTPUT
// arm. The TARGET-form (add_custom_command(TARGET ...
// PRE_BUILD|POST_BUILD|PRE_LINK ...)) is a distinct shape —
// it attaches a command to an existing target rather than
// declaring a new file-producing rule — and is filtered out
// here; the standalone-genrule cross-reference only consumes
// the OUTPUT form.
func classifyAddCustomCommand(ev TraceEvent, sourceRoot string) (AddCustomCommandCall, bool) {
	if !strings.EqualFold(ev.Cmd, "add_custom_command") {
		return AddCustomCommandCall{}, false
	}
	if !inSourceTree(ev.File, sourceRoot) {
		return AddCustomCommandCall{}, false
	}
	if len(ev.Args) == 0 {
		return AddCustomCommandCall{}, false
	}
	// TARGET-form starts with the literal "TARGET" keyword; the
	// OUTPUT-form starts with "OUTPUT" (or another section
	// keyword). The cross-reference only cares about the
	// OUTPUT-form — TARGET-form attaches a hook to an existing
	// target and doesn't declare a standalone genrule.
	if strings.EqualFold(ev.Args[0], "TARGET") {
		return AddCustomCommandCall{}, false
	}

	call := AddCustomCommandCall{
		File:    ev.File,
		Line:    ev.Line,
		RawArgs: append([]string(nil), ev.Args...),
	}

	const (
		secNone = iota
		secOutput
		secCommand
		secDepends
		secByProducts
		secImplicitDeps // IMPLICIT_DEPENDS — variadic <lang> <file> pairs; we just sink tokens
	)
	sec := secNone
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
		case "OUTPUT":
			flushCommand()
			sec = secOutput
			continue
		case "COMMAND":
			flushCommand()
			sec = secCommand
			continue
		case "DEPENDS":
			flushCommand()
			sec = secDepends
			continue
		case "BYPRODUCTS":
			flushCommand()
			sec = secByProducts
			continue
		case "IMPLICIT_DEPENDS":
			flushCommand()
			sec = secImplicitDeps
			continue
		case "MAIN_DEPENDENCY":
			flushCommand()
			sec = secNone
			if i+1 < len(ev.Args) {
				i++
				call.MainDependency = ev.Args[i]
			}
			continue
		case "WORKING_DIRECTORY":
			flushCommand()
			sec = secNone
			if i+1 < len(ev.Args) {
				i++
				call.WorkingDirectory = ev.Args[i]
			}
			continue
		case "COMMENT":
			flushCommand()
			sec = secNone
			if i+1 < len(ev.Args) {
				i++
				call.Comment = ev.Args[i]
			}
			continue
		case "DEPFILE",
			"JOB_POOL",
			"JOB_SERVER_AWARE":
			// Single-value keywords we don't model; consume the value.
			flushCommand()
			sec = secNone
			if i+1 < len(ev.Args) {
				i++
			}
			continue
		case "VERBATIM",
			"APPEND",
			"USES_TERMINAL",
			"COMMAND_EXPAND_LISTS":
			// Flag-only keywords; no value to consume.
			flushCommand()
			sec = secNone
			continue
		}
		// Bare value: append to the open section.
		switch sec {
		case secOutput:
			call.Outputs = append(call.Outputs, a)
		case secCommand:
			currentCmd = append(currentCmd, a)
		case secDepends:
			call.Depends = append(call.Depends, a)
		case secByProducts:
			call.ByProducts = append(call.ByProducts, a)
		case secImplicitDeps:
			// IMPLICIT_DEPENDS shape: <LANG> <file> [<LANG> <file> ...].
			// Out of v1 — drop tokens silently.
		default:
			// Top-level bare value with no open section — cmake
			// itself would reject; drop silently here.
		}
	}
	flushCommand()

	if len(call.Outputs) == 0 {
		// add_custom_command(OUTPUT ...) requires at least one
		// OUTPUT; defensive guard against malformed events.
		return AddCustomCommandCall{}, false
	}
	return call, true
}

// ExtractAddCustomTargets returns one entry per user-written
// add_custom_target call whose trace event fires inside
// sourceRoot. cmake-internal scratch-CMakeLists are filtered out
// by the source-tree gate.
func ExtractAddCustomTargets(traceRaw []byte, sourceRoot string) []AddCustomTargetCall {
	var out []AddCustomTargetCall
	for _, ev := range ParseTrace(traceRaw) {
		if call, ok := classifyAddCustomTarget(ev, sourceRoot); ok {
			out = append(out, call)
		}
	}
	return out
}

// classifyAddCustomTarget parses one trace event into an
// AddCustomTargetCall, or returns (_, false) when the event isn't
// an in-source-tree add_custom_target. Requires a non-empty Name
// (the first positional arg).
func classifyAddCustomTarget(ev TraceEvent, sourceRoot string) (AddCustomTargetCall, bool) {
	if !strings.EqualFold(ev.Cmd, "add_custom_target") {
		return AddCustomTargetCall{}, false
	}
	if !inSourceTree(ev.File, sourceRoot) {
		return AddCustomTargetCall{}, false
	}
	if len(ev.Args) == 0 {
		return AddCustomTargetCall{}, false
	}

	call := AddCustomTargetCall{
		File:    ev.File,
		Line:    ev.Line,
		Name:    ev.Args[0],
		RawArgs: append([]string(nil), ev.Args...),
	}

	const (
		secNone = iota
		secCommand
		secDepends
		secByProducts
		secSources
	)
	sec := secNone
	var currentCmd []string
	flushCommand := func() {
		if len(currentCmd) > 0 {
			call.Commands = append(call.Commands, currentCmd)
		}
		currentCmd = nil
	}

	// Skip arg[0] (the target name); walk keywords starting at
	// arg[1].
	for i := 1; i < len(ev.Args); i++ {
		a := ev.Args[i]
		switch strings.ToUpper(a) {
		case "ALL":
			flushCommand()
			sec = secNone
			call.All = true
			continue
		case "COMMAND":
			flushCommand()
			sec = secCommand
			continue
		case "DEPENDS":
			flushCommand()
			sec = secDepends
			continue
		case "BYPRODUCTS":
			flushCommand()
			sec = secByProducts
			continue
		case "SOURCES":
			flushCommand()
			sec = secSources
			continue
		case "WORKING_DIRECTORY":
			flushCommand()
			sec = secNone
			if i+1 < len(ev.Args) {
				i++
				call.WorkingDirectory = ev.Args[i]
			}
			continue
		case "COMMENT":
			flushCommand()
			sec = secNone
			if i+1 < len(ev.Args) {
				i++
				call.Comment = ev.Args[i]
			}
			continue
		case "JOB_POOL",
			"JOB_SERVER_AWARE":
			flushCommand()
			sec = secNone
			if i+1 < len(ev.Args) {
				i++
			}
			continue
		case "VERBATIM",
			"USES_TERMINAL",
			"COMMAND_EXPAND_LISTS":
			flushCommand()
			sec = secNone
			continue
		}
		switch sec {
		case secCommand:
			currentCmd = append(currentCmd, a)
		case secDepends:
			call.Depends = append(call.Depends, a)
		case secByProducts:
			call.ByProducts = append(call.ByProducts, a)
		case secSources:
			call.Sources = append(call.Sources, a)
		default:
			// Bare top-level value with no open section: cmake
			// itself would reject; drop silently here.
		}
	}
	flushCommand()

	if call.Name == "" {
		return AddCustomTargetCall{}, false
	}
	return call, true
}

// ExtractAddDependencies returns one entry per user-written
// add_dependencies call whose trace event fires inside sourceRoot.
// The cross-reference uses these to discover downstream consumers
// of a custom-command's outputs.
func ExtractAddDependencies(traceRaw []byte, sourceRoot string) []AddDependenciesCall {
	var out []AddDependenciesCall
	for _, ev := range ParseTrace(traceRaw) {
		if call, ok := classifyAddDependencies(ev, sourceRoot); ok {
			out = append(out, call)
		}
	}
	return out
}

// AddLibraryCall records one user-written `add_library(<name>
// [<type>] ...)` call. Surfaces INTERFACE-only library
// declarations that cmake's File API codemodel doesn't expose
// (its `targets[]` array is keyed on EXECUTABLE / STATIC /
// SHARED / MODULE / OBJECT targets only — INTERFACE-only nodes
// like nlohmann_json's main library are absent), so the
// converter's main lift would skip them without trace-side
// recovery.
type AddLibraryCall struct {
	File string
	Line int

	// Name is the target name (first positional argument).
	Name string

	// Type is the explicit library type keyword (STATIC / SHARED
	// / MODULE / OBJECT / INTERFACE / IMPORTED / ALIAS). When the
	// caller omits the keyword, cmake defaults based on the
	// BUILD_SHARED_LIBS variable; for trace replay we leave Type
	// empty and let the consumer decide.
	Type string

	// Aliases is the list of ALIAS-form names cmake recorded.
	// Filled when the call is `add_library(<alias> ALIAS <target>)`;
	// in that case Name is <alias> and Aliases is [<target>] so
	// callers can resolve alias-to-actual mappings.
	Aliases []string

	// RawArgs preserves the verbatim argv for callers that need
	// extra tokens (e.g. EXCLUDE_FROM_ALL / GLOBAL keywords).
	RawArgs []string
}

// ExtractAddLibrary walks the cmake trace for user-written
// add_library() calls. Filters to in-source-tree call sites so
// cmake's own internal libraries don't pollute the result.
func ExtractAddLibrary(traceRaw []byte, sourceRoot string) []AddLibraryCall {
	var out []AddLibraryCall
	for _, ev := range ParseTrace(traceRaw) {
		if call, ok := classifyAddLibrary(ev, sourceRoot); ok {
			out = append(out, call)
		}
	}
	return out
}

func classifyAddLibrary(ev TraceEvent, sourceRoot string) (AddLibraryCall, bool) {
	if !strings.EqualFold(ev.Cmd, "add_library") {
		return AddLibraryCall{}, false
	}
	if !inSourceTree(ev.File, sourceRoot) {
		return AddLibraryCall{}, false
	}
	if len(ev.Args) == 0 {
		return AddLibraryCall{}, false
	}
	call := AddLibraryCall{
		File:    ev.File,
		Line:    ev.Line,
		Name:    ev.Args[0],
		RawArgs: append([]string(nil), ev.Args...),
	}
	if len(ev.Args) >= 2 {
		switch strings.ToUpper(ev.Args[1]) {
		case "STATIC", "SHARED", "MODULE", "OBJECT", "INTERFACE", "IMPORTED":
			call.Type = strings.ToUpper(ev.Args[1])
		case "ALIAS":
			// add_library(<alias> ALIAS <target>) — alias-form.
			// The third arg is the underlying target.
			call.Type = "ALIAS"
			if len(ev.Args) >= 3 {
				call.Aliases = []string{ev.Args[2]}
			}
		}
	}
	return call, true
}

// InstallExportCall records one install(EXPORT <name> [FILE <file>]
// [NAMESPACE <ns>] DESTINATION <dest> ...) trace event. The cmake
// File API codemodel exposes the export's member targets
// (DirectoryInstaller.ExportTargets) but drops the NAMESPACE prefix
// the generated <name>Targets.cmake file actually uses at consumer
// time — ExportTarget.Name is the bare target ("foo"), not the
// consumer-facing "NS::foo". The namespace lives only here in the
// trace; recovering it lets the converter emit the real export name
// (e.g. ZLIB::ZLIB) instead of guessing "<project>::<project>".
type InstallExportCall struct {
	ExportName  string // the EXPORT <name> arg (e.g. "usepkgTargets")
	Namespace   string // the NAMESPACE <ns> arg incl. trailing "::" (e.g. "usepkg::"); empty if omitted
	File        string // the FILE <file> arg (e.g. "usepkgTargets.cmake"); empty if omitted
	Destination string // the DESTINATION <dest> arg (e.g. "lib/cmake/usepkg")
}

// classifyInstallExport recognizes the install(EXPORT ...) form
// (args[0] == "EXPORT"). This is distinct from the
// install(TARGETS ... EXPORT <name> ...) form, which associates
// targets with an export name but carries no NAMESPACE. No
// source-tree filter: install(EXPORT) is always project-authored
// (cmake's own modules never call it), so over-collection isn't a
// concern, and the call can legitimately live in an included
// .cmake under the source tree.
func classifyInstallExport(ev TraceEvent) (InstallExportCall, bool) {
	if !strings.EqualFold(ev.Cmd, "install") {
		return InstallExportCall{}, false
	}
	if len(ev.Args) < 2 || !strings.EqualFold(ev.Args[0], "EXPORT") {
		return InstallExportCall{}, false
	}
	call := InstallExportCall{ExportName: ev.Args[1]}
	for i := 2; i < len(ev.Args); i++ {
		key := strings.ToUpper(ev.Args[i])
		if key != "NAMESPACE" && key != "FILE" && key != "DESTINATION" {
			continue
		}
		if i+1 >= len(ev.Args) {
			break
		}
		switch key {
		case "NAMESPACE":
			call.Namespace = ev.Args[i+1]
		case "FILE":
			call.File = ev.Args[i+1]
		case "DESTINATION":
			call.Destination = ev.Args[i+1]
		}
	}
	return call, true
}

func classifyAddDependencies(ev TraceEvent, sourceRoot string) (AddDependenciesCall, bool) {
	if !strings.EqualFold(ev.Cmd, "add_dependencies") {
		return AddDependenciesCall{}, false
	}
	if !inSourceTree(ev.File, sourceRoot) {
		return AddDependenciesCall{}, false
	}
	if len(ev.Args) < 2 {
		return AddDependenciesCall{}, false
	}
	return AddDependenciesCall{
		File:    ev.File,
		Line:    ev.Line,
		Target:  ev.Args[0],
		Depends: append([]string(nil), ev.Args[1:]...),
		RawArgs: append([]string(nil), ev.Args...),
	}, true
}
