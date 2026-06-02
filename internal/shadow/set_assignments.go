package shadow

import (
	"path/filepath"
	"strings"
)

// SetAssignment is one `set(<Dst> ${<SrcVar>})` verbatim variable copy
// recovered from a NON-EXPANDED cmake trace (`cmake --trace`, not
// `--trace-expand`). Only the single-source-variable copy shape is
// recorded — the shape that forwards a value unchanged from one variable
// to another — because that is what carries a VCS-stamp value onward to a
// `configure_file` marker (e.g. `set(VERSION ${GIT_SHA})` then
// `@VERSION@`). The expanded trace cannot surface this: `--trace-expand`
// substitutes `${SrcVar}` to its value before logging, so the link is
// visible only in the non-expanded trace.
type SetAssignment struct {
	Dst    string // the variable being assigned
	SrcVar string // the single ${...} variable its value is copied from
	File   string
	Line   int
}

// ExtractSetAssignments walks a NON-EXPANDED cmake trace and returns each
// `set(<Dst> ${<SrcVar>})` verbatim variable copy. It matches only the
// pure-copy shape: the value argument must be exactly one `${VAR}`
// reference (no surrounding text, no second value token), optionally
// followed by `PARENT_SCOPE` or the `CACHE` keyword (whose own trailing
// type/docstring/FORCE tokens are then cache metadata, not values).
// Anything richer — concatenation (`v${X}`), multiple refs (`${X}${Y}`),
// or a bare extra value (`set(X ${Y} Z)`, including `set(X ${Y} FORCE)` /
// `set(X ${Y} INTERNAL)` where the trailing token is a plain list element
// without a preceding `CACHE`) — does not forward a single value verbatim
// and is skipped, so the recovered relation stays a clean value-forwarding
// edge.
//
// sourceRoot, when non-empty, restricts to set() calls in the project's
// own tree (cmake's modules set thousands of internal variables); pass ""
// to take all events.
func ExtractSetAssignments(traceRaw []byte, sourceRoot string) []SetAssignment {
	// inSourceTree treats a trailing separator as outside-tree (it
	// requires the next char to be a separator), and sourceRoot can
	// arrive from CLI --source-root with one — normalize so a
	// `/path/to/src/` doesn't silently drop every in-tree set() copy.
	if sourceRoot != "" {
		sourceRoot = filepath.Clean(sourceRoot)
	}
	var out []SetAssignment
	for _, ev := range ParseTrace(traceRaw) {
		if !strings.EqualFold(ev.Cmd, "set") {
			continue
		}
		if sourceRoot != "" && !inSourceTree(ev.File, sourceRoot) {
			continue
		}
		// args: [Dst, value, <scope/cache keywords...>]. Need Dst plus a
		// value that is a sole ${VAR} reference.
		if len(ev.Args) < 2 {
			continue
		}
		src, ok := soleVarRef(ev.Args[1])
		if !ok {
			continue
		}
		// A third arg that isn't a scope/cache keyword is an additional
		// value — `set(X ${Y} Z)` builds a list, not a verbatim copy — so
		// skip. The CACHE / PARENT_SCOPE tails carry only keyword tokens
		// after the value, which are fine.
		if len(ev.Args) > 2 && !isSetScopeTail(ev.Args[2]) {
			continue
		}
		out = append(out, SetAssignment{Dst: ev.Args[0], SrcVar: src, File: ev.File, Line: ev.Line})
	}
	return out
}

// soleVarRef returns the variable name when s is exactly a single cmake
// variable reference `${NAME}` — no surrounding text, no nested or
// adjacent references. `${GIT_SHA}` -> ("GIT_SHA", true); `v${X}`,
// `${X}${Y}`, `${X}-suffix`, `${${Y}}` -> ("", false) (not a verbatim
// copy of one variable).
func soleVarRef(s string) (string, bool) {
	if !strings.HasPrefix(s, "${") || !strings.HasSuffix(s, "}") || len(s) <= 3 {
		return "", false
	}
	name := s[2 : len(s)-1]
	// A clean single-variable name has no further `$`, `{`, or `}` — those
	// would mean concatenation, nesting, or a second reference.
	if strings.ContainsAny(name, "${}") {
		return "", false
	}
	return name, true
}

// isSetScopeTail reports whether the token immediately after the value is
// a keyword that keeps the call a single-value copy rather than a
// list-builder. Only PARENT_SCOPE and CACHE qualify: PARENT_SCOPE writes
// the same value to the parent scope, and CACHE introduces cache metadata
// (type, docstring, FORCE) that follows it — all non-value tokens. A bare
// INTERNAL or FORCE in this position (no preceding CACHE) is just a plain
// list element, so `set(X ${Y} FORCE)` / `set(X ${Y} INTERNAL)` are NOT
// verbatim copies and must not match here.
func isSetScopeTail(tok string) bool {
	switch strings.ToUpper(tok) {
	case "PARENT_SCOPE", "CACHE":
		return true
	}
	return false
}
