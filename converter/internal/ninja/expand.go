package ninja

import (
	"strings"
)

// Scope is a layered variable lookup. Each layer is a map; lookup walks the
// slice index 0 -> N-1 and returns the first hit. Callers stack layers
// outermost-first so the leftmost wins, matching ninja's "innermost binding
// wins" rule (we just hand callers the layers in the right order).
type Scope []map[string]string

// Get returns the value for name, or "" if absent. Empty string and absent
// are not distinguished — same as ninja.
func (s Scope) Get(name string) string {
	for _, layer := range s {
		if v, ok := layer[name]; ok {
			return v
		}
	}
	return ""
}

// Expand resolves $VAR / ${VAR} references in s using scope, recursively.
//
// Recursion is bounded: a binding that references itself, or a cycle of
// bindings, is detected via the seen set and the offending name is left as
// the literal "$NAME" in the output.
//
// Escapes:
//
//	$$  -> literal $
//	$ \ -> literal space (ninja uses $-space to escape spaces in paths,
//	       not relevant inside command strings but cheap to support)
//	$:  -> literal :
//	$\n -> empty (line continuation; expansion is post-line-join, so
//	       these are already gone in practice)
//
// Identifier rule: a name after $ is `[A-Za-z0-9_]+`. Inside `${...}` we
// allow `[A-Za-z0-9_.-]+` (ninja allows dots and hyphens in braced refs).
func Expand(s string, scope Scope) string {
	return expand(s, scope, map[string]bool{})
}

func expand(s string, scope Scope, seen map[string]bool) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		c := s[i]
		if c != '$' {
			b.WriteByte(c)
			i++
			continue
		}
		if i+1 >= len(s) {
			// trailing '$' — write as-is
			b.WriteByte('$')
			i++
			continue
		}
		next := s[i+1]
		switch {
		case next == '$':
			b.WriteByte('$')
			i += 2
		case next == ' ':
			b.WriteByte(' ')
			i += 2
		case next == ':':
			b.WriteByte(':')
			i += 2
		case next == '\n':
			i += 2
		case next == '{':
			end := strings.IndexByte(s[i+2:], '}')
			if end < 0 {
				// malformed; treat as literal
				b.WriteString(s[i:])
				i = len(s)
				continue
			}
			name := s[i+2 : i+2+end]
			b.WriteString(lookup(name, scope, seen))
			i += 2 + end + 1
		case isIdentStart(next):
			// Unbraced refs allow [A-Za-z0-9_] only — `-` and `.` end the
			// identifier (they're literal). Use the wider charset only
			// inside ${...}.
			j := i + 1
			for j < len(s) && isUnbracedIdent(s[j]) {
				j++
			}
			name := s[i+1 : j]
			b.WriteString(lookup(name, scope, seen))
			i = j
		default:
			// Unknown escape: keep the $ and the next byte literal.
			b.WriteByte(c)
			b.WriteByte(next)
			i += 2
		}
	}
	return b.String()
}

func lookup(name string, scope Scope, seen map[string]bool) string {
	if seen[name] {
		return "$" + name
	}
	v := scope.Get(name)
	if v == "" {
		return ""
	}
	if !strings.ContainsRune(v, '$') {
		return v
	}
	seen[name] = true
	defer delete(seen, name)
	return expand(v, scope, seen)
}

func isIdentStart(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || b == '_'
}

// isUnbracedIdent: identifier rule for $NAME (no dots / hyphens).
func isUnbracedIdent(b byte) bool {
	return isIdentStart(b) || (b >= '0' && b <= '9')
}

// CommandFor renders the fully-expanded command string for a build statement.
//
// Scope chain (highest precedence first):
//  1. $in / $out built-ins computed from the build statement.
//  2. The build statement's own bindings.
//  3. The rule's bindings.
//  4. The graph's top-level vars.
//
// If the rule isn't in the graph, returns "" and false.
func CommandFor(g *Graph, b *Build) (string, bool) {
	rule, ok := g.Rules[b.Rule]
	if !ok {
		return "", false
	}
	cmd, ok := rule.Bindings["command"]
	if !ok {
		return "", false
	}
	scope := Scope{
		builtins(b),
		b.Bindings,
		rule.Bindings,
		g.Vars,
	}
	return Expand(cmd, scope), true
}

// builtins computes the $in/$out replacements for one build statement.
// $in is the explicit input list joined by spaces; $out is the explicit
// output list joined by spaces. Implicit inputs/outputs do NOT appear in
// $in/$out — they're declared so ninja tracks them but commands don't see
// them, matching ninja semantics.
func builtins(b *Build) map[string]string {
	return map[string]string{
		"in":  strings.Join(b.Inputs, " "),
		"out": strings.Join(b.Outputs, " "),
	}
}

// CustomCommandEdges returns every build statement using rule
// "CUSTOM_COMMAND" in g, preserving build.ninja declaration order.
// cmake's Ninja generator emits this rule for every
// add_custom_command and add_custom_target — Phase 4 of the
// generator-parity uplift (ROADMAP.md) walks the returned list to
// lift each edge into a genrule with depfile-derived srcs.
//
// Returns nil when the graph has no CUSTOM_COMMAND builds (e.g., a
// trivial CMakeLists with only add_executable / add_library targets).
func CustomCommandEdges(g *Graph) []*Build {
	var out []*Build
	for _, b := range g.Builds {
		if b.Rule == "CUSTOM_COMMAND" {
			out = append(out, b)
		}
	}
	return out
}

// DepfileFor returns the resolved depfile path for a build statement.
// Resolved means $-variables expand through the same scope chain
// CommandFor uses (builtins → build bindings → rule bindings → graph
// vars). Returns ("", false) when neither the build nor its rule
// declares a depfile binding — most CUSTOM_COMMAND edges don't,
// because cmake's add_custom_command(... DEPFILE ...) is the opt-in
// shape (cmake 3.20+).
//
// Callers that want absolute paths join the result against the
// build directory; ninja depfile paths are build-dir-relative by
// convention.
func DepfileFor(g *Graph, b *Build) (string, bool) {
	rule, ok := g.Rules[b.Rule]
	if !ok {
		return "", false
	}
	df := b.Bindings["depfile"]
	if df == "" {
		df = rule.Bindings["depfile"]
	}
	if df == "" {
		return "", false
	}
	scope := Scope{
		builtins(b),
		b.Bindings,
		rule.Bindings,
		g.Vars,
	}
	return Expand(df, scope), true
}

// DescriptionFor returns the resolved $description binding for a
// build statement (the human-readable line ninja prints when the
// rule fires). Same scope chain as CommandFor / DepfileFor.
// Returns ("", false) when neither build nor rule declares one.
//
// cmake's add_custom_command(... COMMENT "...") lands here, so the
// description doubles as source-level naming for the lifted genrule.
func DescriptionFor(g *Graph, b *Build) (string, bool) {
	rule, ok := g.Rules[b.Rule]
	if !ok {
		return "", false
	}
	desc := b.Bindings["description"]
	if desc == "" {
		desc = rule.Bindings["description"]
	}
	if desc == "" {
		return "", false
	}
	scope := Scope{
		builtins(b),
		b.Bindings,
		rule.Bindings,
		g.Vars,
	}
	return Expand(desc, scope), true
}
