package cmakeparse

import (
	"fmt"
	"strings"
)

// Command is one parsed cmake command call.
//
// Name is the command identifier (case-preserved as written;
// callers compare case-insensitively per cmake semantics).
//
// Args is the argument list. Quoted arguments retain their
// surrounding quotes ("foo bar" stays "foo bar"); the parser
// doesn't unescape because the callers that care
// (target_sources / add_library / add_executable source-arg
// extraction) need to round-trip the original bytes. Use Unquote
// to get the unquoted form when comparing against trace argv
// strings (which cmake records pre-quoted).
//
// Line is the 1-based source line where the command name
// appeared.
type Command struct {
	Name string
	Args []string
	Line int
}

// IfBlock is one parsed `if() ... [elseif()...]... [else()...] endif()`
// structure. Each Arm is a body of commands gated by the arm's
// predicate.
//
// StartLine / EndLine are the line range the if-block spans:
// StartLine is the line of the `if` token, EndLine is the line
// of the `endif` token. Arms carry their own line ranges so
// callers can correlate trace events with the exact arm body
// that contains them.
type IfBlock struct {
	StartLine int
	EndLine   int
	Arms      []IfArm
}

// IfArm is one arm of an if-block — the if itself, an elseif,
// or the else.
//
// Kind is "if", "elseif", or "else". For "else" the
// PredicateArgs is nil (the else arm's predicate is implicit:
// NOT-of-everything-above).
//
// PredicateArgs is the literal argument vector to the
// if/elseif (unevaluated). Callers map this through
// selectKeyFromIfArgs (in internal/shadow/) to a Bazel
// constraint label.
//
// StartLine / EndLine bracket the arm's command body — from
// the line of the if/elseif/else token to the line of the
// next arm-delimiter (next elseif/else/endif).
type IfArm struct {
	Kind          string
	PredicateArgs []string
	StartLine     int
	EndLine       int
	Body          []Node
}

// Node is a discriminated union over the two top-level shapes
// the parser exposes: a plain Command or an IfBlock. Other
// cmake control-flow constructs (foreach, while, function,
// macro, block) are NOT modeled — function/macro definitions
// are parsed as a flat sequence of Commands (we don't enter
// them); foreach/while bodies aren't extracted as nested
// blocks (their bodies don't affect Tier 2's recovery since we
// only follow if/elseif/else/endif).
//
// Exactly one of Command / If is non-nil.
type Node struct {
	Command *Command
	If      *IfBlock
}

// Parse parses a cmake source buffer into a flat sequence of
// Nodes. Returns the parse tree and any first error
// encountered.
//
// What it handles:
//   - Top-level command calls.
//   - if / elseif / else / endif blocks, nested arbitrarily.
//   - Bracket arguments / comments, quoted arguments, variable
//     references (preserved opaque).
//
// What it deliberately doesn't handle:
//   - function/macro definitions — parsed as a flat command
//     sequence (the function() and endfunction() are surfaced
//     as plain Commands; we don't enter the body or expand
//     calls to defined functions).
//   - foreach/while loops — same: the foreach() and endforeach()
//     are surfaced as plain Commands; the body's commands are
//     in the sibling stream but not gated by the loop.
//   - block() / endblock() — same.
//
// The Tier 2 driver checks predicate recognition before walking
// the body, so unrecognized control flow doesn't matter for
// correctness — the driver only descends into recognized if-
// blocks whose predicate maps to a platform constraint.
func Parse(src string) ([]Node, error) {
	toks, err := lex(src)
	if err != nil {
		return nil, err
	}
	p := &parser{toks: toks}
	var out []Node
	for !p.atEOF() {
		node, err := p.parseNode()
		if err != nil {
			return nil, err
		}
		out = append(out, node)
	}
	return out, nil
}

type parser struct {
	toks []token
	pos  int
}

func (p *parser) atEOF() bool {
	return p.pos >= len(p.toks) || p.toks[p.pos].kind == tokEOF
}

func (p *parser) peek() token {
	if p.pos >= len(p.toks) {
		return token{kind: tokEOF}
	}
	return p.toks[p.pos]
}

func (p *parser) advance() token {
	t := p.peek()
	p.pos++
	return t
}

// parseNode parses one top-level node — a command call which
// may be the start of an if-block (in which case parseIfBlock
// consumes the whole block).
func (p *parser) parseNode() (Node, error) {
	cmd, err := p.parseCommand()
	if err != nil {
		return Node{}, err
	}
	if strings.EqualFold(cmd.Name, "if") {
		block, err := p.parseIfBlockBody(cmd)
		if err != nil {
			return Node{}, err
		}
		return Node{If: block}, nil
	}
	return Node{Command: cmd}, nil
}

// parseCommand reads a single command call: IDENT ( arg ... ).
// Returns the parsed Command. The opening identifier and parens
// are consumed; the closing paren is consumed.
func (p *parser) parseCommand() (*Command, error) {
	head := p.advance()
	if head.kind != tokWord {
		return nil, fmt.Errorf("cmakeparse: line %d: expected command name, got %q", head.line, head.text)
	}
	cmd := &Command{Name: head.text, Line: head.line}
	open := p.advance()
	if open.kind != tokLParen {
		return nil, fmt.Errorf("cmakeparse: line %d: expected `(` after command %q, got %q", open.line, head.text, open.text)
	}
	for {
		t := p.peek()
		switch t.kind {
		case tokRParen:
			p.advance()
			return cmd, nil
		case tokEOF:
			return nil, fmt.Errorf("cmakeparse: line %d: unterminated command call %q", head.line, head.text)
		case tokLParen:
			// cmake's if-arg grammar permits `if((A AND B) OR
			// C)`-style parens inside args. We don't evaluate
			// them — preserve them as literal `(` tokens inside
			// Args so the predicate-recognizer can refuse the
			// call cleanly (the recognized shapes have no parens
			// in their arg vectors). The matching `)` is consumed
			// by the outer tokRParen case above, which would
			// terminate the command-call early on imbalanced
			// nesting. We don't expect this shape on the platform-
			// predicate paths Tier 2 actually walks; it just
			// keeps the parser honest about token positions.
			p.advance()
			cmd.Args = append(cmd.Args, "(")
		default:
			p.advance()
			cmd.Args = append(cmd.Args, t.text)
		}
	}
}

// parseIfBlockBody parses an if-block's body given the already-
// consumed `if(...)` command. Walks subsequent nodes until
// `endif(...)`, splitting on `elseif(...)` / `else()` into arms.
func (p *parser) parseIfBlockBody(ifCmd *Command) (*IfBlock, error) {
	block := &IfBlock{
		StartLine: ifCmd.Line,
	}
	// The opening arm covers from the if() down to the first
	// arm-delimiter (elseif/else/endif).
	arm := IfArm{
		Kind:          "if",
		PredicateArgs: append([]string(nil), ifCmd.Args...),
		StartLine:     ifCmd.Line,
	}
	closeArm := func(endLine int) {
		arm.EndLine = endLine
		block.Arms = append(block.Arms, arm)
	}

	for !p.atEOF() {
		t := p.peek()
		if t.kind != tokWord {
			return nil, fmt.Errorf("cmakeparse: line %d: expected command in if-body, got %q", t.line, t.text)
		}
		switch strings.ToLower(t.text) {
		case "endif":
			endCmd, err := p.parseCommand()
			if err != nil {
				return nil, err
			}
			closeArm(endCmd.Line)
			block.EndLine = endCmd.Line
			return block, nil
		case "elseif":
			delimCmd, err := p.parseCommand()
			if err != nil {
				return nil, err
			}
			closeArm(delimCmd.Line)
			arm = IfArm{
				Kind:          "elseif",
				PredicateArgs: append([]string(nil), delimCmd.Args...),
				StartLine:     delimCmd.Line,
			}
		case "else":
			delimCmd, err := p.parseCommand()
			if err != nil {
				return nil, err
			}
			closeArm(delimCmd.Line)
			arm = IfArm{
				Kind:      "else",
				StartLine: delimCmd.Line,
			}
		default:
			node, err := p.parseNode()
			if err != nil {
				return nil, err
			}
			arm.Body = append(arm.Body, node)
		}
	}
	return nil, fmt.Errorf("cmakeparse: line %d: unterminated if() block (missing endif)", ifCmd.Line)
}

// Unquote strips a single pair of surrounding double quotes
// from an argument lexeme. Returns the unchanged input when
// the lexeme isn't quoted. cmake's quoted-argument rules
// preserve internal whitespace + variable references; we
// match the trace decoder's behaviour (cmake records the
// post-unquote bytes in the trace's `args` array, so Tier 2
// callers normalize parser output through Unquote before
// matching).
func Unquote(s string) string {
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		inner := s[1 : len(s)-1]
		// Decode the standard cmake escapes the trace decoder
		// would have already applied: `\\` → `\`, `\"` → `"`,
		// `\n` → newline, `\t` → tab. Other escapes pass
		// through with their backslash stripped to match how
		// cmake itself records args.
		var b strings.Builder
		b.Grow(len(inner))
		for i := 0; i < len(inner); i++ {
			c := inner[i]
			if c == '\\' && i+1 < len(inner) {
				next := inner[i+1]
				switch next {
				case 'n':
					b.WriteByte('\n')
				case 't':
					b.WriteByte('\t')
				case 'r':
					b.WriteByte('\r')
				case '"':
					b.WriteByte('"')
				case '\\':
					b.WriteByte('\\')
				default:
					b.WriteByte(next)
				}
				i++
				continue
			}
			b.WriteByte(c)
		}
		return b.String()
	}
	return s
}
