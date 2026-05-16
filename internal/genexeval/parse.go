// Package genexeval parses and evaluates cmake generator
// expressions (genexes — `$<...>`) at convert-element-cmake
// time + at Bazel time. The (a) shape of ROADMAP's
// "Generator-expression evaluation in lifted genrules" entry
// — complement to (b)'s `lower.extractGenexValues` capture
// shape.
//
// Coverage v1: the configure-time-resolvable subset cmake
// can evaluate from the variable namespace plus already-
// captured target properties. Target-evaluator-dependent
// forms (`$<TARGET_FILE:...>`, `$<TARGET_OBJECTS:...>`,
// `$<TARGET_LINKER_FILE:...>`) are typed-refused; the lifter
// falls back to (b) or legacy for those.
//
// Grammar (simplified from
// https://cmake.org/cmake/help/latest/manual/cmake-generator-expressions.7.html):
//
//	expr  := text | genex
//	genex := '$<' op (':' args)? '>'
//	args  := arg (',' arg)*
//	arg   := expr+   (text bytes interleaved with nested genexes)
//
// Where text is any bytes not matching `$<`, `,`, or `>` —
// commas at the top of a nested genex don't split the parent's
// args (the depth counter tracks them); `,` at the top level
// of a genex is the arg separator; `>` at the top level closes
// the genex.
//
// The parser returns a tree of nodes. Static text becomes a
// chunkNode; a genex becomes a genexNode with Op + Args. Args
// themselves are slices of nodes (mixed text and nested
// genexes), which the evaluator concatenates left-to-right.
//
// Unbalanced or malformed genex syntax surfaces as a parser
// error — the caller falls back to (b) / legacy.
package genexeval

import (
	"fmt"
)

// Node is the parser's output: either static text or a genex
// invocation. Implementations are unexported because the
// evaluator is the only consumer.
type Node interface {
	isNode()
}

// chunkNode carries a span of literal bytes from the template.
// The evaluator emits them verbatim.
type chunkNode struct {
	Bytes []byte
}

func (chunkNode) isNode() {}

// genexNode is a parsed `$<Op[:Args]>` invocation. Args is the
// list of comma-separated arg expressions; each arg is itself
// a sequence of nodes (text + nested genexes) that the
// evaluator concatenates before passing as the op's argument.
//
// For parameterless genexes like `$<CONFIG>`, Args is nil.
// For `$<CONFIG:Release>`, Args has one entry containing one
// chunkNode `Release`. For `$<IF:$<CONFIG:Release>,a,b>`, Args
// has three entries: a slice with one genexNode, then two
// single-chunk slices.
type genexNode struct {
	Op   string
	Args [][]Node
}

func (genexNode) isNode() {}

// Parse decomposes template into a flat list of nodes
// (chunkNode interleaved with genexNode). Returns the node
// list and nil on a syntactically-valid template; returns nil
// + an error when a genex is unbalanced or empty
// (`$<>` rejected). The parser is byte-faithful and does NOT
// attempt to validate op names or arg shapes — that's the
// evaluator's job.
//
// Lone `$` bytes (no following `<`) pass through as part of a
// chunkNode. Same for `$<` openers with no matching `>`: the
// parser scans through to end-of-template, fails to balance,
// and surfaces a parser error. Callers that need to tolerate
// unbalanced openers can use lower.topLevelGenexes instead
// (the more permissive scanner used by the (b) capture path).
func Parse(template []byte) ([]Node, error) {
	p := parser{src: template}
	nodes, err := p.parseUntil(0)
	if err != nil {
		return nil, err
	}
	// Top-level `>` and `,` are literal text — cmake templates
	// commonly carry `>` in C++ templates, HTML, comparison
	// operators, etc. The depth-tracking machinery only treats
	// them as terminators inside a genex.
	return nodes, nil
}

// parser is a single-pass cursor over the template. depth is
// the nesting level of `$<...>` openers the caller is currently
// inside; at depth 0 only `$<` is special; at depth > 0 both
// `,` and `>` terminate the current span (the caller decides
// which).
type parser struct {
	src []byte
	pos int
}

// parseUntil consumes nodes from p.src starting at p.pos until
// it hits one of the terminator runes (',' or '>' at the
// caller's nesting depth). The caller is responsible for
// consuming the terminator. Returns the parsed nodes plus any
// error encountered.
//
// terminators is a bitmask of which terminator runes stop the
// scan: 0 = scan to end-of-template (the top-level call);
// terminatorComma | terminatorClose = scan inside a genex arg
// (either separator ends this arg).
func (p *parser) parseUntil(terminators int) ([]Node, error) {
	var nodes []Node
	chunkStart := p.pos
	flushChunk := func() {
		if p.pos > chunkStart {
			nodes = append(nodes, chunkNode{Bytes: p.src[chunkStart:p.pos]})
		}
	}
	for p.pos < len(p.src) {
		c := p.src[p.pos]
		// Top-level terminators only fire when called from
		// inside a genex (terminators != 0).
		if terminators != 0 {
			if c == ',' && terminators&terminatorComma != 0 {
				flushChunk()
				return nodes, nil
			}
			if c == '>' && terminators&terminatorClose != 0 {
				flushChunk()
				return nodes, nil
			}
		}
		if c == '$' && p.pos+1 < len(p.src) && p.src[p.pos+1] == '<' {
			flushChunk()
			g, err := p.parseGenex()
			if err != nil {
				return nil, err
			}
			nodes = append(nodes, g)
			chunkStart = p.pos
			continue
		}
		p.pos++
	}
	flushChunk()
	if terminators != 0 {
		// Hit EOF while inside a genex — unbalanced opener.
		return nil, fmt.Errorf("unterminated `$<` opener (expected `>` before end of template)")
	}
	return nodes, nil
}

const (
	terminatorComma = 1 << iota
	terminatorClose
)

// parseGenex consumes a single `$<...>` starting at p.pos (the
// `$`). Returns the parsed genexNode + advances p.pos past the
// closing `>`. Errors on:
//
//   - Empty op (`$<>` or `$<:...>`)
//   - Unbalanced opener (no matching `>`)
//
// Args are parsed via recursive parseUntil calls — each arg
// scans until a comma or close-bracket at the current depth.
func (p *parser) parseGenex() (genexNode, error) {
	// Skip the `$<` opener.
	p.pos += 2

	// Op: read identifier bytes up to `:` or `>`. cmake treats
	// the op as "everything before the first `:` or `>`". For
	// parameterless ops like $<CONFIG>, the op is the whole
	// thing before `>`.
	opStart := p.pos
	for p.pos < len(p.src) {
		c := p.src[p.pos]
		if c == ':' || c == '>' {
			break
		}
		// Nested `$<` inside an op-name region is malformed;
		// cmake would reject. We surface as a parser error
		// rather than silently absorb.
		if c == '$' && p.pos+1 < len(p.src) && p.src[p.pos+1] == '<' {
			return genexNode{}, fmt.Errorf("unexpected nested `$<` inside genex op name at offset %d", p.pos)
		}
		p.pos++
	}
	if p.pos >= len(p.src) {
		return genexNode{}, fmt.Errorf("unterminated `$<` at offset %d", opStart-2)
	}
	op := string(p.src[opStart:p.pos])
	if op == "" {
		return genexNode{}, fmt.Errorf("empty genex op at offset %d", opStart-2)
	}

	// Parameterless: `$<OP>` closes here.
	if p.src[p.pos] == '>' {
		p.pos++ // consume `>`
		return genexNode{Op: op}, nil
	}

	// Parameterized: `$<OP:arg(,arg)*>`. Consume the `:` and
	// parse args until the matching `>`.
	p.pos++ // consume `:`
	var args [][]Node
	for {
		argNodes, err := p.parseUntil(terminatorComma | terminatorClose)
		if err != nil {
			return genexNode{}, err
		}
		args = append(args, argNodes)
		if p.pos >= len(p.src) {
			return genexNode{}, fmt.Errorf("unterminated `$<%s:...`", op)
		}
		switch p.src[p.pos] {
		case ',':
			p.pos++ // consume comma, continue with next arg
		case '>':
			p.pos++ // consume close, done
			return genexNode{Op: op, Args: args}, nil
		default:
			// parseUntil only returns at one of the terminators
			// it was told about; unreachable in practice.
			return genexNode{}, fmt.Errorf("internal: parseUntil stopped at non-terminator %q at offset %d", p.src[p.pos], p.pos)
		}
	}
}
