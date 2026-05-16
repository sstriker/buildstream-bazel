package genexeval

import (
	"strings"
	"testing"
)

func TestParse_PlainText(t *testing.T) {
	nodes, err := Parse([]byte("hello, world\n"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(nodes) != 1 {
		t.Fatalf("want 1 chunk node, got %d: %#v", len(nodes), nodes)
	}
	c, ok := nodes[0].(chunkNode)
	if !ok {
		t.Fatalf("want chunkNode, got %T", nodes[0])
	}
	if string(c.Bytes) != "hello, world\n" {
		t.Errorf("chunk bytes = %q", c.Bytes)
	}
}

func TestParse_Parameterless(t *testing.T) {
	nodes, err := Parse([]byte("$<CONFIG>"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(nodes) != 1 {
		t.Fatalf("want 1 genex node, got %d", len(nodes))
	}
	g, ok := nodes[0].(genexNode)
	if !ok {
		t.Fatalf("want genexNode, got %T", nodes[0])
	}
	if g.Op != "CONFIG" {
		t.Errorf("op = %q, want CONFIG", g.Op)
	}
	if g.Args != nil {
		t.Errorf("parameterless genex should have nil Args, got %#v", g.Args)
	}
}

func TestParse_SingleArg(t *testing.T) {
	nodes, err := Parse([]byte("$<CONFIG:Release>"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	g := nodes[0].(genexNode)
	if g.Op != "CONFIG" {
		t.Errorf("op = %q", g.Op)
	}
	if len(g.Args) != 1 {
		t.Fatalf("want 1 arg, got %d", len(g.Args))
	}
	if got := string(g.Args[0][0].(chunkNode).Bytes); got != "Release" {
		t.Errorf("arg[0] = %q", got)
	}
}

func TestParse_MultipleArgs(t *testing.T) {
	nodes, err := Parse([]byte("$<IF:1,yes,no>"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	g := nodes[0].(genexNode)
	if g.Op != "IF" {
		t.Errorf("op = %q", g.Op)
	}
	if len(g.Args) != 3 {
		t.Fatalf("want 3 args, got %d (%#v)", len(g.Args), g.Args)
	}
	wants := []string{"1", "yes", "no"}
	for i, w := range wants {
		got := string(g.Args[i][0].(chunkNode).Bytes)
		if got != w {
			t.Errorf("arg[%d] = %q want %q", i, got, w)
		}
	}
}

func TestParse_Nested(t *testing.T) {
	nodes, err := Parse([]byte("$<IF:$<CONFIG:Release>,debug,release>"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	outer := nodes[0].(genexNode)
	if outer.Op != "IF" {
		t.Errorf("outer op = %q", outer.Op)
	}
	if len(outer.Args) != 3 {
		t.Fatalf("outer wants 3 args, got %d", len(outer.Args))
	}
	// First arg is a single nested genex.
	if len(outer.Args[0]) != 1 {
		t.Fatalf("outer arg[0] should be one node, got %d", len(outer.Args[0]))
	}
	inner, ok := outer.Args[0][0].(genexNode)
	if !ok {
		t.Fatalf("outer arg[0] should be genexNode, got %T", outer.Args[0][0])
	}
	if inner.Op != "CONFIG" {
		t.Errorf("inner op = %q", inner.Op)
	}
	if len(inner.Args) != 1 {
		t.Fatalf("inner wants 1 arg, got %d", len(inner.Args))
	}
}

func TestParse_MixedTextAndGenex(t *testing.T) {
	nodes, err := Parse([]byte("prefix $<CONFIG> suffix\n"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(nodes) != 3 {
		t.Fatalf("want 3 nodes (chunk + genex + chunk), got %d", len(nodes))
	}
	if got := string(nodes[0].(chunkNode).Bytes); got != "prefix " {
		t.Errorf("first chunk = %q", got)
	}
	if nodes[1].(genexNode).Op != "CONFIG" {
		t.Errorf("middle genex op = %q", nodes[1].(genexNode).Op)
	}
	if got := string(nodes[2].(chunkNode).Bytes); got != " suffix\n" {
		t.Errorf("third chunk = %q", got)
	}
}

func TestParse_NestedInArgText(t *testing.T) {
	// `$<IF:$<CONFIG:Release>,foo $<CONFIG> bar,baz>` — the
	// "then" arg is mixed text + genex.
	nodes, err := Parse([]byte("$<IF:$<CONFIG:Release>,foo $<CONFIG> bar,baz>"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	outer := nodes[0].(genexNode)
	thenArg := outer.Args[1]
	if len(thenArg) != 3 {
		t.Fatalf("then-arg should be [chunk genex chunk], got %d nodes", len(thenArg))
	}
	if got := string(thenArg[0].(chunkNode).Bytes); got != "foo " {
		t.Errorf("then-arg[0] = %q", got)
	}
	if thenArg[1].(genexNode).Op != "CONFIG" {
		t.Errorf("then-arg[1] op = %q", thenArg[1].(genexNode).Op)
	}
	if got := string(thenArg[2].(chunkNode).Bytes); got != " bar" {
		t.Errorf("then-arg[2] = %q", got)
	}
}

func TestParse_EmptyArg(t *testing.T) {
	// `$<IF:1,,b>` — empty then-arg is valid cmake syntax.
	nodes, err := Parse([]byte("$<IF:1,,b>"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	g := nodes[0].(genexNode)
	if len(g.Args) != 3 {
		t.Fatalf("want 3 args, got %d", len(g.Args))
	}
	if g.Args[1] != nil && len(g.Args[1]) != 0 {
		t.Errorf("arg[1] should be empty, got %#v", g.Args[1])
	}
}

func TestParse_TrailingComma(t *testing.T) {
	// `$<IF:1,a,>` — trailing comma yields an empty third arg.
	// cmake accepts this.
	nodes, err := Parse([]byte("$<IF:1,a,>"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	g := nodes[0].(genexNode)
	if len(g.Args) != 3 {
		t.Fatalf("want 3 args, got %d", len(g.Args))
	}
}

func TestParse_ErrorCases(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string // substring of error message
	}{
		{"empty op no args", "$<>", "empty genex op"},
		{"empty op with args", "$<:foo>", "empty genex op"},
		{"unterminated opener", "$<CONFIG", "unterminated"},
		{"unterminated args", "$<IF:a,b", "unterminated"},
		{"nested $< inside op name", "$<C$<X>>", "nested"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Parse([]byte(c.in))
			if err == nil {
				t.Fatalf("expected error containing %q; got nil", c.want)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error %q does not contain %q", err.Error(), c.want)
			}
		})
	}
}

func TestParse_NoGenex(t *testing.T) {
	nodes, err := Parse([]byte("a $b $c $$ $\n"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(nodes) != 1 {
		t.Fatalf("want one chunk (no genex), got %d", len(nodes))
	}
}

func TestParse_LiteralCloseBracket(t *testing.T) {
	// `>` at top level is literal text (C++ template usage,
	// HTML, comparison operators). cmake templates commonly
	// carry it.
	nodes, err := Parse([]byte("template<int> + 5 > 3"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(nodes) != 1 {
		t.Fatalf("want one chunk node, got %d: %#v", len(nodes), nodes)
	}
}
