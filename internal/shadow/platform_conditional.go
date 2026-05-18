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
func ExtractPlatformConditionalSources(traceRaw []byte, sourceRoot string, knownTargets map[string]bool) []PlatformConditionalSource {
	events := ParseTrace(traceRaw)
	return extractPlatformConditionalSources(events, sourceRoot, knownTargets)
}

func extractPlatformConditionalSources(events []TraceEvent, sourceRoot string, knownTargets map[string]bool) []PlatformConditionalSource {
	st := newPlatformIfStack()
	var out []PlatformConditionalSource
	for _, ev := range events {
		out = maybeCollectPlatformConditionalSource(ev, st, sourceRoot, knownTargets, out)
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
func maybeCollectPlatformConditionalSource(ev TraceEvent, st *platformIfStack, sourceRoot string, knownTargets map[string]bool, out []PlatformConditionalSource) []PlatformConditionalSource {
	st.observe(ev)
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

// platformIfStack tracks the open `if()` blocks per cmake file
// in trace order. cmake guarantees if/elseif/else/endif balance
// within a single file, so the per-file stack is well-defined.
// include()-introduced files surface as distinct ev.File values
// with their own independent stacks.
type platformIfStack struct {
	perFile map[string][]string
}

func newPlatformIfStack() *platformIfStack {
	return &platformIfStack{perFile: map[string][]string{}}
}

func (p *platformIfStack) observe(ev TraceEvent) {
	switch strings.ToLower(ev.Cmd) {
	case "if":
		p.perFile[ev.File] = append(p.perFile[ev.File], selectKeyFromIfArgs(ev.Args))
	case "elseif":
		st := p.perFile[ev.File]
		if len(st) > 0 {
			st = st[:len(st)-1]
		}
		p.perFile[ev.File] = append(st, selectKeyFromIfArgs(ev.Args))
	case "else":
		st := p.perFile[ev.File]
		if len(st) > 0 {
			st = st[:len(st)-1]
		}
		// We can't express the inverted predicate as a single
		// positive Bazel constraint label, so the else arm is
		// always "unrecognized": sources here fall through to
		// the flat srcs list, matching pre-#217 behaviour.
		p.perFile[ev.File] = append(st, "")
	case "endif":
		st := p.perFile[ev.File]
		if len(st) > 0 {
			p.perFile[ev.File] = st[:len(st)-1]
		}
	}
}

// currentSelectKey returns the innermost recognized select key
// in the open if-stack for the given file. When the open stack
// contains both recognized and unrecognized frames, the
// innermost recognized one wins — sources are conditional on
// every open if, but the innermost positive platform predicate
// is the tightest single constraint we can express. The other
// open frames are guaranteed satisfiable on this platform
// (cmake only traces what runs), so the recognized constraint
// fully characterizes the source's platform-applicability for
// Tier 1's purposes.
func (p *platformIfStack) currentSelectKey(file string) string {
	st := p.perFile[file]
	for i := len(st) - 1; i >= 0; i-- {
		if st[i] != "" {
			return st[i]
		}
	}
	return ""
}

// selectKeyFromIfArgs maps a recognized cmake if() argument
// vector to a Bazel @platforms//os:* constraint label, or ""
// for unrecognized shapes.
//
// Tier 1 only recognizes the canonical direct form:
//
//	if(CMAKE_SYSTEM_NAME STREQUAL "<Name>")
//
// Three-argument shape. The quoting on the third arg is
// optional in cmake source; --trace-expand strips quotes
// before recording.
func selectKeyFromIfArgs(args []string) string {
	if len(args) != 3 {
		return ""
	}
	if !strings.EqualFold(args[0], "CMAKE_SYSTEM_NAME") {
		return ""
	}
	if !strings.EqualFold(args[1], "STREQUAL") {
		return ""
	}
	return cmakeSystemNameToConstraint(args[2])
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
	if rel == "." || strings.HasPrefix(rel, "..") {
		return ""
	}
	return filepath.ToSlash(rel)
}
