package shadow

import (
	"path/filepath"
	"strings"
)

// PlatformConditionalSource records one source file that the
// trace shows being added to a target inside a recognized
// platform-conditional cmake `if()` block.
//
// Recognized today (Tier 1 of #217): direct
// `if(CMAKE_SYSTEM_NAME STREQUAL "<Name>")` shapes, where
// <Name> is one of cmake's well-known os values that maps to
// a Bazel @platforms//os:* constraint. The `elseif()` arm of
// such an if is also recognized when its predicate is the
// same STREQUAL shape. `else()` arms (where the constraint
// would be a NOT-of-something not natively expressible as a
// single positive constraint label) are NOT recognized;
// sources inside them fall through unpartitioned, same as
// pre-#217 behaviour.
//
// Target is the cmake target the source was attached to
// (target_sources / add_library / add_executable). Source is
// the source file path made project-source-root-relative
// using slash form, matching the codemodel
// `TargetSource.Path` shape so the lower path can join the
// two by string equality. SelectKey is the constraint label
// the lower path should attach the source under in
// `ir.Target.PerPlatform["srcs"][selectKey]`.
type PlatformConditionalSource struct {
	Target    string
	Source    string
	SelectKey string
}

// ExtractPlatformConditionalSources walks the trace once and
// returns every source that was attached to a known target
// inside a recognized platform-conditional `if()` block. Order
// of returned records is insertion order from the trace.
//
// knownTargets gates which target_sources / add_library /
// add_executable calls we trust to be addressing in-codebase
// targets (vs producer-side cmake macros that target imported
// libs we don't model). Matches the gating ExtractTargetIncludes
// uses for the same reason.
//
// sourceRoot is the project source root; sources outside it
// are dropped (their package-relative path would escape with
// `..`, which the lower path doesn't accept).
//
// Scope tracking: for each (file, line) event, looks up the
// active if-stack by parsing the CMakeLists.txt source bytes
// via cmakeparse. This replaces the previous trace-event-driven
// stack which relied on `endif` events that cmake's
// `--trace-format=json-v1` does NOT emit (endif is a structural
// delimiter, not a command). The cmakeparse-based lookup is
// stack-balance-correct by construction regardless of trace
// event ordering.
//
// File reads go through defaultFS (os.ReadFile). For tests +
// offline-replay use ExtractPlatformConditionalSourcesWithFS.
func ExtractPlatformConditionalSources(traceRaw []byte, sourceRoot string, knownTargets map[string]bool) []PlatformConditionalSource {
	return ExtractPlatformConditionalSourcesWithFS(traceRaw, sourceRoot, sourceRoot, knownTargets, defaultFS{})
}

// ExtractPlatformConditionalSourcesWithFS is the file-system-
// abstracted variant. Required for offline-replay tests where
// trace events record paths under traceSourceRoot but the
// CMakeLists files live under hostSourceRoot. Production
// callers (the converter's Decode) thread hostSourceRoot in;
// the file-less standalone signature delegates here with
// traceSourceRoot == hostSourceRoot which matches the
// in-process recording case.
func ExtractPlatformConditionalSourcesWithFS(traceRaw []byte, traceSourceRoot, hostSourceRoot string, knownTargets map[string]bool, fs fsReader) []PlatformConditionalSource {
	events := ParseTrace(traceRaw)
	return extractPlatformConditionalSources(events, traceSourceRoot, hostSourceRoot, knownTargets, fs)
}

func extractPlatformConditionalSources(events []TraceEvent, traceSourceRoot, hostSourceRoot string, knownTargets map[string]bool, fs fsReader) []PlatformConditionalSource {
	// Two implementations, selected on the trace's shape:
	//
	//   - cmake's `--trace-format=json-v1` (the real cmake
	//     mode the converter sees in production) doesn't emit
	//     `endif` events, so the trace-event-driven stack
	//     can't pop. Use the cmakeparse-based per-file scope
	//     index instead, which is stack-balance-correct by
	//     construction (reads if/endif structure from the
	//     source bytes).
	//
	//   - Synthetic test traces (and any hypothetical future
	//     cmake-output mode that does emit endif) preserve
	//     the trace-event stack: the tests pass synthetic if
	//     + endif events without providing CMakeLists.txt
	//     source bytes on disk, so the cmakeparse path can't
	//     resolve the file and would attribute nothing.
	//
	// Detection: any `endif` event in the trace → use
	// trace-event stack. Otherwise (typical of cmake's real
	// JSON-v1 trace) → cmakeparse-based.
	if hasEndifEvent(events) {
		st := newPlatformIfStack()
		var out []PlatformConditionalSource
		for _, ev := range events {
			out = maybeCollectPlatformConditionalSourceTraceStack(ev, st, traceSourceRoot, knownTargets, out)
		}
		return out
	}
	idx := newCmakeFileIfIndex()
	var out []PlatformConditionalSource
	for _, ev := range events {
		out = maybeCollectPlatformConditionalSource(ev, idx, traceSourceRoot, hostSourceRoot, fs, knownTargets, out)
	}
	return out
}

func hasEndifEvent(events []TraceEvent) bool {
	for _, ev := range events {
		if strings.EqualFold(ev.Cmd, "endif") {
			return true
		}
	}
	return false
}

// maybeCollectPlatformConditionalSourceTraceStack is the
// pre-cmakeparse code path: maintains the global
// platformIfStack via trace-event observation and attributes
// sources using the current stack at each event. Kept for the
// trace-with-endif case (synthetic tests + any future cmake
// output mode that emits endif events). The body is the
// original maybeCollect implementation before the cmakeparse
// rewrite.
func maybeCollectPlatformConditionalSourceTraceStack(ev TraceEvent, st *platformIfStack, sourceRoot string, knownTargets map[string]bool, out []PlatformConditionalSource) []PlatformConditionalSource {
	if inSourceTree(ev.File, sourceRoot) {
		st.observe(ev)
	}
	key := st.currentSelectKey(ev.File)
	if key == "" {
		return out
	}
	target, srcs, ok := sourcesFromAddOrTargetCall(ev)
	if !ok {
		return out
	}
	if !knownTargets[target] {
		return out
	}
	for _, src := range srcs {
		rel := resolveSourceRelative(src, ev.File, sourceRoot)
		if rel == "" {
			continue
		}
		out = append(out, PlatformConditionalSource{
			Target:    target,
			Source:    rel,
			SelectKey: key,
		})
	}
	return out
}

// maybeCollectPlatformConditionalSource is the per-event step
// of the platform-conditional source attribution pass. Updates
// the if-stack for the event (so it stays in sync across the
// stream) and appends any PlatformConditionalSource records
// the event yields. Returns the (possibly grown) out slice.
//
// Single source of truth shared between Decode (single-pass
// multi-extractor) and extractPlatformConditionalSources
// (standalone) so the two can't drift as Tier 2/3 extensions
// land — addresses the duplication Copilot flagged on #223.
//
// In-tree filter: if/endif events are observed only when their
// ev.File lives inside the project source tree. cmake's own
// internal modules under /usr/share/cmake-* fire many
// if(WIN32) / if(APPLE) / if(CMAKE_HOST_SYSTEM_NAME ...) events
// during compiler detection / Find* / sysinfo probes — those
// would otherwise push spurious select keys onto the global
// stack and mis-attribute later in-tree target_sources calls.
// Source-attaching calls (target_sources / add_library /
// add_executable) stay observed regardless of file scope
// because they're additionally gated by knownTargets[target] —
// cmake-internal calls don't act on user-defined targets.
//
// cmake guarantees if/elseif/else/endif balance within a single
// file, so the per-file filter preserves stack balance for
// whichever subset of files we choose to observe.
func maybeCollectPlatformConditionalSource(ev TraceEvent, idx *cmakeFileIfIndex, traceSourceRoot, hostSourceRoot string, fs fsReader, knownTargets map[string]bool, out []PlatformConditionalSource) []PlatformConditionalSource {
	// Only in-tree events can produce attributions — cmake's
	// internal modules under /usr/share/cmake-* fire if(WIN32)
	// / if(APPLE) etc. during compiler detection probes that
	// have nothing to do with the user's project intent.
	if !inSourceTree(ev.File, traceSourceRoot) {
		return out
	}
	target, srcs, ok := sourcesFromAddOrTargetCall(ev)
	if !ok {
		return out
	}
	if !knownTargets[target] {
		return out
	}
	// Look up the active if-stack for (file, line) via cmakeparse.
	// Returns "" when no recognized platform constraint sits in
	// the active stack — sources fall through to flat srcs.
	hostFile := remapHostPath(ev.File, traceSourceRoot, hostSourceRoot)
	key := idx.currentSelectKey(hostFile, ev.Line, fs)
	if key == "" {
		return out
	}
	for _, src := range srcs {
		rel := resolveSourceRelative(src, ev.File, traceSourceRoot)
		if rel == "" {
			continue
		}
		out = append(out, PlatformConditionalSource{
			Target:    target,
			Source:    rel,
			SelectKey: key,
		})
	}
	return out
}

// platformIfStack tracks the open `if()` blocks in cmake
// execution order — a single global stack regardless of which
// file each event came from. cmake guarantees if/elseif/else/
// endif balance within each file AND that include() and
// function/macro calls preserve the open-if context across
// file boundaries (the body of an include()d file or a called
// function executes in the caller's if-scope, so an `if()`
// opened by the caller is still in effect for calls inside the
// includee/callee).
//
// An earlier per-file design (one stack keyed on ev.File) was
// reported to miss the common include()-wrapping-conditional
// pattern:
//
//	# caller/CMakeLists.txt
//	if(CMAKE_SYSTEM_NAME STREQUAL "Linux")
//	    include("subdir/inner.cmake")
//	endif()
//
//	# subdir/inner.cmake
//	target_sources(foo PRIVATE linux.c)
//
// The trace's target_sources event records ev.File =
// inner.cmake, so the per-file stack saw no open if and left
// linux.c unconditional. The global stack sees the caller's
// open if at the moment inner.cmake's target_sources runs and
// correctly attributes the source.
// defaultSelectKey is the sentinel SelectKey for a platform
// if-chain's else() arm. In a Bazel select(), "none of the sibling
// arms matched" is exactly `//conditions:default`, so an else() whose
// every prior arm (if/elseif) is a recognized platform constraint maps
// here — the source is conditional on "not any of the recognized
// siblings", which the emitted select()'s default arm expresses. When
// a sibling arm is an UNRECOGNIZED predicate (a non-platform condition
// like if(BUILD_TESTING)), else() does NOT mean "default platform" and
// the arm stays "" (flat) — see ifFrame.allSiblingsRecognized.
const defaultSelectKey = "//conditions:default"

// ifFrame is one open `if()` block on the stack. key is the current
// arm's select key (a platform constraint, defaultSelectKey for a
// qualifying else, or "" when unrecognized). allSiblingsRecognized
// tracks whether EVERY arm seen so far in this block (if + each
// elseif) mapped to a recognized platform constraint — the guard that
// lets a trailing else() become defaultSelectKey only when the whole
// chain is platform-pure.
type ifFrame struct {
	key                   string
	allSiblingsRecognized bool
}

type platformIfStack struct {
	stack []ifFrame
}

func newPlatformIfStack() *platformIfStack {
	return &platformIfStack{}
}

func (p *platformIfStack) observe(ev TraceEvent) {
	switch strings.ToLower(ev.Cmd) {
	case "if":
		k := selectKeyFromIfArgs(ev.Args)
		p.stack = append(p.stack, ifFrame{key: k, allSiblingsRecognized: k != ""})
	case "elseif":
		prior := true
		if len(p.stack) > 0 {
			prior = p.stack[len(p.stack)-1].allSiblingsRecognized
			p.stack = p.stack[:len(p.stack)-1]
		}
		k := selectKeyFromIfArgs(ev.Args)
		// This arm contributes to the block's recognized-ness: the
		// chain stays "all recognized" only if every prior arm AND
		// this elseif map to a platform constraint.
		p.stack = append(p.stack, ifFrame{key: k, allSiblingsRecognized: prior && k != ""})
	case "else":
		prior := true
		if len(p.stack) > 0 {
			prior = p.stack[len(p.stack)-1].allSiblingsRecognized
			p.stack = p.stack[:len(p.stack)-1]
		}
		// An else() arm maps to //conditions:default ONLY when every
		// sibling (if + all elseif) was a recognized platform
		// constraint — then "not any of them" is exactly the select's
		// default arm. Otherwise the inverted predicate isn't
		// expressible and the source falls through to flat srcs
		// (pre-#217 behaviour).
		key := ""
		if prior {
			key = defaultSelectKey
		}
		p.stack = append(p.stack, ifFrame{key: key, allSiblingsRecognized: prior})
	case "endif":
		if len(p.stack) > 0 {
			p.stack = p.stack[:len(p.stack)-1]
		}
	}
}

// currentSelectKey returns the innermost recognized select key
// in the open if-stack. When the open stack contains both
// recognized and unrecognized frames, the innermost recognized
// one wins — sources are conditional on every open if, but the
// innermost positive platform predicate is the tightest single
// constraint we can express. The other open frames are
// guaranteed satisfiable on this platform (cmake only traces
// what runs), so the recognized constraint fully characterizes
// the source's platform-applicability for Tier 1's purposes.
//
// File-scoping isn't applied here (the stack is global — see
// the platformIfStack docstring); the unused file parameter is
// retained for caller symmetry and so a future per-file
// refinement (e.g. for shapes where a global stack misattributes)
// can drop in without touching callers.
func (p *platformIfStack) currentSelectKey(file string) string {
	_ = file
	for i := len(p.stack) - 1; i >= 0; i-- {
		if p.stack[i].key != "" {
			return p.stack[i].key
		}
	}
	return ""
}

// selectKeyFromIfArgs maps a recognized cmake if() argument
// vector to a Bazel @platforms//os:* constraint label, or ""
// for unrecognized shapes. See shorthandPlatformVarToConstraint
// for the single-identifier mapping table.
//
// Recognized:
//
//	if(CMAKE_SYSTEM_NAME STREQUAL "<Name>")  — canonical three-arg form
//	if(<NAME>)                               — single-arg shorthand:
//	                                           WIN32 / LINUX / APPLE /
//	                                           MSVC / MINGW / CYGWIN
//
// Deliberately NOT recognized (no clean single-constraint
// mapping):
//
//	if(UNIX)                — true for Linux + macOS + BSDs; multi-OS aggregate
//	if(BSD)                 — multi-OS aggregate
//	if(NOT <X>)             — inverted predicate; would need a select default arm
//	if(<A> AND <B>)         — multi-condition; would need a config_setting
//	if(CMAKE_SYSTEM_NAME MATCHES <re>)  — regex form
//
// Sources inside any unrecognized shape fall through to flat
// srcs, matching pre-#217 behaviour.
func selectKeyFromIfArgs(args []string) string {
	switch len(args) {
	case 1:
		return shorthandPlatformVarToConstraint(args[0])
	case 3:
		if !strings.EqualFold(args[0], "CMAKE_SYSTEM_NAME") {
			return ""
		}
		if !strings.EqualFold(args[1], "STREQUAL") {
			return ""
		}
		return cmakeSystemNameToConstraint(args[2])
	}
	return ""
}

// shorthandPlatformVarToConstraint maps cmake's
// single-identifier platform-shorthand variables to the
// corresponding @platforms//os:* constraint label. Returns ""
// for anything without a clean single-positive mapping
// (notably UNIX and BSD — true on multiple Bazel OSes — and
// NOT-of-something predicates handled at the if-args level).
//
// Strictly-OS predicates:
//
//	WIN32   → @platforms//os:windows
//	LINUX   → @platforms//os:linux   (cmake 3.25+)
//	APPLE   → @platforms//os:darwin  (broader than :darwin in
//	          cmake — true for iOS/tvOS/watchOS too — but :darwin
//	          is the closest single positive constraint and matches
//	          the codebase convention; projects needing the finer
//	          distinction use CMAKE_SYSTEM_NAME STREQUAL "iOS" etc.)
//
// Compiler predicates that ALSO carry an implicit OS constraint:
//
//	MSVC    → @platforms//os:windows  (MSVC only runs on Windows)
//	MINGW   → @platforms//os:windows  (MinGW targets Windows)
//	CYGWIN  → @platforms//os:windows  (Cygwin runs on Windows)
//
// The compiler-predicate mappings are lossy in the
// other-compiler-on-same-OS direction (e.g. `if(MSVC)` would
// also gate-in code on a MinGW Windows build under the
// :windows arm) but lossless in the OS-implication direction:
// any compile that runs under MSVC/MinGW/Cygwin is by
// definition a Windows build. Without these mappings the
// sources would fall through to flat srcs and attempt to compile
// on Linux — a strictly worse failure mode than the slight
// imprecision.
//
// Case-insensitive: cmake's `if(Win32)` parses the same as
// `if(WIN32)`.
func shorthandPlatformVarToConstraint(name string) string {
	switch strings.ToUpper(name) {
	case "WIN32", "MSVC", "MINGW", "CYGWIN":
		return "@platforms//os:windows"
	case "LINUX":
		return "@platforms//os:linux"
	case "APPLE":
		return "@platforms//os:darwin"
	}
	return ""
}

// cmakeSystemNameToConstraint maps cmake's CMAKE_SYSTEM_NAME
// values to the @platforms//os:* constraint label most projects
// use. Returns "" for values without a canonical mapping; the
// caller treats that as "unrecognized" and skips partitioning.
//
// Coverage matches the os values @platforms ships constants
// for as of bazel 7. Missing values (BlueGeneL, CrayLinuxEnv,
// HP-UX, ...) won't surface as platform-conditional sources
// today; adding them is a one-line entry here when a
// downstream needs it.
func cmakeSystemNameToConstraint(name string) string {
	switch strings.ToLower(name) {
	case "linux":
		return "@platforms//os:linux"
	case "darwin":
		return "@platforms//os:darwin"
	case "windows":
		return "@platforms//os:windows"
	case "freebsd":
		return "@platforms//os:freebsd"
	case "openbsd":
		return "@platforms//os:openbsd"
	case "netbsd":
		return "@platforms//os:netbsd"
	case "android":
		return "@platforms//os:android"
	case "ios":
		return "@platforms//os:ios"
	case "tvos":
		return "@platforms//os:tvos"
	case "watchos":
		return "@platforms//os:watchos"
	case "qnx":
		return "@platforms//os:qnx"
	}
	return ""
}

// sourcesFromAddOrTargetCall pulls out (target, sources) from
// a single trace event when the event is one of the calls that
// attach source files to a target. Returns ("", nil, false)
// for any other event.
//
// Recognized shapes (Tier 1):
//
//	add_library(<name> [STATIC|SHARED|MODULE|OBJECT|INTERFACE]
//	    [EXCLUDE_FROM_ALL] <src>...)
//	add_executable(<name> [WIN32] [MACOSX_BUNDLE]
//	    [EXCLUDE_FROM_ALL] <src>...)
//	target_sources(<name> <PRIVATE|PUBLIC|INTERFACE> <src>...
//	    [<PRIVATE|PUBLIC|INTERFACE> <src>...]...)
//
// Skipped: alias / imported add_library forms (no real
// sources), target_sources FILE_SET form (more complex parse,
// rare in conditional blocks). For target_sources we
// concatenate sources across all visibility groups — the
// platform-conditionality applies regardless of visibility.
func sourcesFromAddOrTargetCall(ev TraceEvent) (target string, sources []string, ok bool) {
	switch strings.ToLower(ev.Cmd) {
	case "add_library":
		return parseAddLibraryArgs(ev.Args)
	case "add_executable":
		return parseAddExecutableArgs(ev.Args)
	case "target_sources":
		return parseTargetSourcesArgs(ev.Args)
	}
	return "", nil, false
}

func parseAddLibraryArgs(args []string) (string, []string, bool) {
	if len(args) < 2 {
		return "", nil, false
	}
	target := args[0]
	rest := args[1:]
	// Skip alias / imported forms — no in-codebase sources.
	for _, a := range rest {
		switch strings.ToUpper(a) {
		case "ALIAS", "IMPORTED", "INTERFACE":
			// INTERFACE libraries have no compiled sources by
			// definition; skip them here so we don't end up
			// reporting INTERFACE header attachments as
			// platform-conditional srcs.
			return "", nil, false
		}
	}
	rest = stripLeadingKeywords(rest, map[string]bool{
		"STATIC": true, "SHARED": true, "MODULE": true,
		"OBJECT": true, "EXCLUDE_FROM_ALL": true,
	})
	if len(rest) == 0 {
		return "", nil, false
	}
	return target, rest, true
}

func parseAddExecutableArgs(args []string) (string, []string, bool) {
	if len(args) < 2 {
		return "", nil, false
	}
	target := args[0]
	rest := args[1:]
	// Skip alias / imported forms.
	for _, a := range rest {
		switch strings.ToUpper(a) {
		case "ALIAS", "IMPORTED":
			return "", nil, false
		}
	}
	rest = stripLeadingKeywords(rest, map[string]bool{
		"WIN32": true, "MACOSX_BUNDLE": true, "EXCLUDE_FROM_ALL": true,
	})
	if len(rest) == 0 {
		return "", nil, false
	}
	return target, rest, true
}

func parseTargetSourcesArgs(args []string) (string, []string, bool) {
	if len(args) < 2 {
		return "", nil, false
	}
	target := args[0]
	var srcs []string
	for _, a := range args[1:] {
		u := strings.ToUpper(a)
		if u == "PRIVATE" || u == "PUBLIC" || u == "INTERFACE" {
			continue
		}
		if u == "FILE_SET" || u == "TYPE" || u == "BASE_DIRS" || u == "FILES" {
			// FILE_SET form — bail out; the arg structure is
			// header-set-shaped, not a flat src list.
			return "", nil, false
		}
		srcs = append(srcs, a)
	}
	if len(srcs) == 0 {
		return "", nil, false
	}
	return target, srcs, true
}

// stripLeadingKeywords drops args from the front of the list
// while they match one of the recognized cmake keywords. cmake
// requires these keywords (when present) to precede the
// positional source-file arguments, so a non-keyword arg ends
// the strip.
func stripLeadingKeywords(args []string, keywords map[string]bool) []string {
	for len(args) > 0 && keywords[strings.ToUpper(args[0])] {
		args = args[1:]
	}
	return args
}

// resolveSourceRelative converts a cmake-source-as-written
// (possibly relative to the CMakeLists.txt that named it) into
// a slash-form project-source-root-relative path matching the
// codemodel TargetSource.Path shape. Returns "" when the source
// resolves outside sourceRoot.
func resolveSourceRelative(src, file, sourceRoot string) string {
	if src == "" {
		return ""
	}
	abs := src
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(filepath.Dir(file), abs)
	}
	rel, err := filepath.Rel(sourceRoot, abs)
	if err != nil {
		return ""
	}
	rel = filepath.ToSlash(rel)
	if rel == "." {
		return ""
	}
	// Reject parent-directory escapes only. The earlier
	// `strings.HasPrefix(rel, "..")` check was too broad — it
	// would also reject in-tree filenames whose first segment
	// literally starts with `..` (e.g. `..foo/bar.c`, a legal
	// if unusual filename). The narrow segment-aware check
	// drops only true `..` path segments.
	if rel == ".." || strings.HasPrefix(rel, "../") {
		return ""
	}
	return rel
}
