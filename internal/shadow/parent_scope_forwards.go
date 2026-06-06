package shadow

import (
	"path/filepath"
	"strings"
)

// ParentScopeForward is one resolved function-parameter-forwarded variable
// write recovered from a NON-EXPANDED cmake trace (`cmake --trace`). A helper
// function hands a value back to its caller under a name passed in as an
// argument:
//
//	function(get_git_sha _var)
//	  execute_process(... OUTPUT_VARIABLE out ...)
//	  set(${_var} "${out}" PARENT_SCOPE)
//	endfunction()
//	get_git_sha(GIT_SHA)            # _var <- GIT_SHA
//
// This is the GetGitRevisionDescription.cmake / git_describe() shape SDL (and
// the hundreds of projects that vendor that module) use. Dst is the ACTUAL
// caller argument the parameter was bound to at a given call site (GIT_SHA),
// SrcVar is the function-local source variable forwarded (out).
//
// Unlike SetAssignment — a verbatim `set(Dst ${SrcVar})` copy whose Dst is a
// literal name — the Dst here lives behind the function boundary: the set's
// literal first argument is `${_var}`, a reference to the function parameter,
// and only the call site binds it to a concrete name. Recovering that binding
// lets a VCS-stamp value flowing through the function-local OUTPUT_VARIABLE be
// marked on the real consumer variable the caller's configure_file references,
// so `@GIT_SHA@` lifts to stamp_values instead of baking the convert-time
// revision.
//
// Only the non-expanded trace surfaces this: `--trace-expand` substitutes
// `${_var}` / `${out}` to their values before logging, erasing the parameter
// reference the binding resolution keys on.
type ParentScopeForward struct {
	Dst    string // resolved caller-scope variable (the call argument)
	SrcVar string // the function-local source variable forwarded
	File   string // call-site file
	Line   int    // call-site line
}

// funcDef is one recovered `function(<name> <params...>)` definition with the
// line range its body spans in its defining file. Body command events fire at
// the definition's file:line (not the call site's), so a `set(...)` inside the
// body is attributed to its function by File + an exclusive line range between
// the `function` and matching `endfunction` events.
type funcDef struct {
	name      string
	params    []string
	file      string
	startLine int // the `function(...)` line
	endLine   int // the matching `endfunction()` line
}

// ExtractParentScopeForwards walks a NON-EXPANDED cmake trace and returns each
// function-parameter-forwarded variable write, resolved to the caller-scope
// variable name at every in-tree call site of the enclosing function.
//
// It matches only the narrow, unambiguous shape the stamp lift needs:
// `set(${param} ${src} PARENT_SCOPE)` inside a recovered function body, where
// `param` is one of that function's declared parameters. PARENT_SCOPE is
// required — without it the write never escapes to the caller, so it can't be
// the value a caller's configure_file reads. A call argument that is itself a
// `${...}` reference is skipped (it would need a further binding level).
//
// sourceRoot, when non-empty, restricts both the function definitions and the
// call sites to the project's own tree (cmake's bundled modules define
// thousands of helper functions); pass "" to take all events.
func ExtractParentScopeForwards(traceRaw []byte, sourceRoot string) []ParentScopeForward {
	if sourceRoot != "" {
		sourceRoot = filepath.Clean(sourceRoot)
	}
	events := ParseTrace(traceRaw)
	defs := recoverFunctionDefs(events, sourceRoot)
	if len(defs) == 0 {
		return nil
	}
	// Collect in-tree call sites for every recovered function name. A call is
	// any command event whose cmd matches a defined function's name (cmake
	// commands are case-insensitive).
	callsByName := map[string][]TraceEvent{}
	for _, def := range defs {
		callsByName[strings.ToLower(def.name)] = nil
	}
	for _, ev := range events {
		key := strings.ToLower(ev.Cmd)
		if _, isFunc := callsByName[key]; !isFunc {
			continue
		}
		if sourceRoot != "" && !inSourceTree(ev.File, sourceRoot) {
			continue
		}
		callsByName[key] = append(callsByName[key], ev)
	}

	var out []ParentScopeForward
	for _, ev := range events {
		if !strings.EqualFold(ev.Cmd, "set") {
			continue
		}
		if sourceRoot != "" && !inSourceTree(ev.File, sourceRoot) {
			continue
		}
		// `set(${param} ${src} PARENT_SCOPE)` — exactly three args, the third
		// the PARENT_SCOPE keyword that makes the write escape to the caller.
		if len(ev.Args) != 3 || !strings.EqualFold(ev.Args[2], "PARENT_SCOPE") {
			continue
		}
		param, ok := soleVarRef(ev.Args[0])
		if !ok {
			continue
		}
		src, ok := soleVarRef(ev.Args[1])
		if !ok {
			continue
		}
		def := enclosingDef(defs, ev.File, ev.Line)
		if def == nil {
			continue
		}
		idx := paramIndex(def.params, param)
		if idx < 0 {
			continue
		}
		for _, call := range callsByName[strings.ToLower(def.name)] {
			if idx >= len(call.Args) {
				continue
			}
			arg := call.Args[idx]
			// A literal variable name resolves directly; a `${...}` actual arg
			// would need a further binding level, so skip it conservatively
			// rather than record a name that can't match a template marker.
			if arg == "" {
				continue
			}
			if _, isRef := soleVarRef(arg); isRef {
				continue
			}
			out = append(out, ParentScopeForward{Dst: arg, SrcVar: src, File: call.File, Line: call.Line})
		}
	}
	return out
}

// recoverFunctionDefs pairs `function` / `endfunction` trace events into body
// line ranges. At definition time cmake records a function's body WITHOUT
// executing it, so the two bracketing events are properly nested and a simple
// stack pairs them. Definitions are filtered to sourceRoot in the result; the
// stack still tracks out-of-tree defs so pairing stays balanced across cmake's
// own modules.
func recoverFunctionDefs(events []TraceEvent, sourceRoot string) []funcDef {
	type open struct {
		name   string
		params []string
		file   string
		line   int
	}
	var stack []open
	var defs []funcDef
	for _, ev := range events {
		switch {
		case strings.EqualFold(ev.Cmd, "function"):
			if len(ev.Args) == 0 {
				continue
			}
			stack = append(stack, open{
				name:   ev.Args[0],
				params: append([]string(nil), ev.Args[1:]...),
				file:   ev.File,
				line:   ev.Line,
			})
		case strings.EqualFold(ev.Cmd, "endfunction"):
			if len(stack) == 0 {
				continue
			}
			top := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if sourceRoot != "" && !inSourceTree(top.file, sourceRoot) {
				continue
			}
			// A valid body range needs the endfunction in the same file as its
			// function; a mismatch means the trace is malformed for our purpose.
			if ev.File != top.file || ev.Line <= top.line {
				continue
			}
			defs = append(defs, funcDef{
				name:      top.name,
				params:    top.params,
				file:      top.file,
				startLine: top.line,
				endLine:   ev.Line,
			})
		}
	}
	return defs
}

// enclosingDef returns the function definition whose body strictly contains
// (file, line), or nil. The range is exclusive of the bracketing
// function/endfunction lines so those commands themselves aren't attributed.
func enclosingDef(defs []funcDef, file string, line int) *funcDef {
	for i := range defs {
		d := &defs[i]
		if d.file == file && line > d.startLine && line < d.endLine {
			return d
		}
	}
	return nil
}

// paramIndex returns the position of name in params, or -1. cmake's implicit
// parameters (ARGN/ARGC/ARGV*) aren't declared names, so a `${ARGV0}` forward
// correctly returns -1 and is skipped.
func paramIndex(params []string, name string) int {
	for i, p := range params {
		if p == name {
			return i
		}
	}
	return -1
}
