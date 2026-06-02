package shadow

import "strings"

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
// followed by scope/cache keywords (PARENT_SCOPE / CACHE … / INTERNAL …).
// Anything richer — concatenation (`v${X}`), multiple refs (`${X}${Y}`),
// list building (`set(X ${Y} Z)`) — does not forward a single value
// verbatim and is skipped, so the recovered relation stays a clean
// value-forwarding edge.
//
// sourceRoot, when non-empty, restricts to set() calls in the project's
// own tree (cmake's modules set thousands of internal variables); pass ""
// to take all events.
func ExtractSetAssignments(traceRaw []byte, sourceRoot string) []SetAssignment {
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

// isSetScopeTail reports whether a trailing set() token is a scope/cache
// keyword (so it doesn't make the call a list-builder). The value tokens
// of a CACHE entry (type, docstring) follow the CACHE keyword, so once we
// see CACHE the call is still a single-value copy with cache semantics.
func isSetScopeTail(tok string) bool {
	switch strings.ToUpper(tok) {
	case "PARENT_SCOPE", "CACHE", "INTERNAL", "FORCE":
		return true
	}
	return false
}
