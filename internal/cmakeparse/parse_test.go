package cmakeparse

import (
	"reflect"
	"testing"
)

// TestParse_TopLevelCommands pins that a sequence of top-level
// command calls parses to a flat slice of Command nodes.
func TestParse_TopLevelCommands(t *testing.T) {
	src := `
project(myproj)
add_library(foo STATIC foo.c)
add_executable(bar bar.c)
`
	nodes, err := Parse(src)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(nodes) != 3 {
		t.Fatalf("nodes = %d, want 3", len(nodes))
	}
	wantNames := []string{"project", "add_library", "add_executable"}
	for i, want := range wantNames {
		if nodes[i].Command == nil {
			t.Fatalf("nodes[%d].Command = nil", i)
		}
		if nodes[i].Command.Name != want {
			t.Errorf("nodes[%d].Name = %q, want %q", i, nodes[i].Command.Name, want)
		}
	}
}

// TestParse_IfBlock pins the basic if-block shape: one if-arm
// holding a body of commands; StartLine/EndLine span the
// if-block.
func TestParse_IfBlock(t *testing.T) {
	src := `
if(WIN32)
  target_sources(foo PRIVATE win.c)
endif()
`
	nodes, err := Parse(src)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(nodes) != 1 || nodes[0].If == nil {
		t.Fatalf("expected 1 if-block; got %#v", nodes)
	}
	blk := nodes[0].If
	if len(blk.Arms) != 1 {
		t.Fatalf("arms = %d, want 1", len(blk.Arms))
	}
	if blk.Arms[0].Kind != "if" || !reflect.DeepEqual(blk.Arms[0].PredicateArgs, []string{"WIN32"}) {
		t.Errorf("arm predicate = %#v", blk.Arms[0])
	}
	if len(blk.Arms[0].Body) != 1 || blk.Arms[0].Body[0].Command == nil {
		t.Fatalf("arm body = %#v", blk.Arms[0].Body)
	}
	if blk.Arms[0].Body[0].Command.Name != "target_sources" {
		t.Errorf("body cmd = %q", blk.Arms[0].Body[0].Command.Name)
	}
}

// TestParse_IfElseifElse pins multi-arm if/elseif/else/endif
// parses into separate arms with their own predicates +
// bodies.
func TestParse_IfElseifElse(t *testing.T) {
	src := `
if(LINUX)
  target_sources(app PRIVATE linux.c)
elseif(WIN32)
  target_sources(app PRIVATE win.c)
else()
  target_sources(app PRIVATE other.c)
endif()
`
	nodes, err := Parse(src)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(nodes) != 1 || nodes[0].If == nil {
		t.Fatalf("expected 1 if-block")
	}
	blk := nodes[0].If
	if len(blk.Arms) != 3 {
		t.Fatalf("arms = %d, want 3", len(blk.Arms))
	}
	if blk.Arms[0].Kind != "if" || blk.Arms[0].PredicateArgs[0] != "LINUX" {
		t.Errorf("arm 0 = %#v", blk.Arms[0])
	}
	if blk.Arms[1].Kind != "elseif" || blk.Arms[1].PredicateArgs[0] != "WIN32" {
		t.Errorf("arm 1 = %#v", blk.Arms[1])
	}
	if blk.Arms[2].Kind != "else" || len(blk.Arms[2].PredicateArgs) != 0 {
		t.Errorf("arm 2 = %#v", blk.Arms[2])
	}
	for i, arm := range blk.Arms {
		if len(arm.Body) != 1 {
			t.Errorf("arm %d body = %d cmds, want 1", i, len(arm.Body))
		}
	}
}

// TestParse_NestedIf pins nested if-blocks parse recursively
// into IfBlock nodes inside the outer arm's body.
func TestParse_NestedIf(t *testing.T) {
	src := `
if(WIN32)
  if(MSVC)
    target_sources(foo PRIVATE msvc.c)
  endif()
endif()
`
	nodes, err := Parse(src)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	outer := nodes[0].If
	if outer == nil || len(outer.Arms) != 1 {
		t.Fatalf("outer if = %#v", nodes[0])
	}
	body := outer.Arms[0].Body
	if len(body) != 1 || body[0].If == nil {
		t.Fatalf("nested if body = %#v", body)
	}
	inner := body[0].If
	if inner.Arms[0].PredicateArgs[0] != "MSVC" {
		t.Errorf("inner predicate = %#v", inner.Arms[0].PredicateArgs)
	}
}

// TestParse_MultipleCommandsInArm pins multiple commands in
// one arm all surface in arm.Body in order.
func TestParse_MultipleCommandsInArm(t *testing.T) {
	src := `
if(LINUX)
  target_sources(foo PRIVATE linux1.c)
  target_sources(foo PRIVATE linux2.c)
  set(SOMEVAR ON)
endif()
`
	nodes, err := Parse(src)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	body := nodes[0].If.Arms[0].Body
	if len(body) != 3 {
		t.Fatalf("body cmds = %d, want 3", len(body))
	}
	wantNames := []string{"target_sources", "target_sources", "set"}
	for i, want := range wantNames {
		if body[i].Command == nil || body[i].Command.Name != want {
			t.Errorf("body[%d].Name = %v, want %s", i, body[i].Command, want)
		}
	}
}

// TestParse_UnterminatedIf pins that a missing endif() is an
// error, not silent best-effort recovery.
func TestParse_UnterminatedIf(t *testing.T) {
	src := `if(LINUX)
target_sources(foo PRIVATE linux.c)
`
	_, err := Parse(src)
	if err == nil {
		t.Errorf("expected error for missing endif")
	}
}

// TestParse_UnbalancedParens pins that a missing close paren
// on a command is an error.
func TestParse_UnbalancedParens(t *testing.T) {
	_, err := Parse(`add_library(foo STATIC foo.c`)
	if err == nil {
		t.Errorf("expected error for unbalanced parens")
	}
}

// TestParse_CommentsBetweenCommands pins that comments at any
// position (between, before, after commands) are ignored.
func TestParse_CommentsBetweenCommands(t *testing.T) {
	src := `
# Top comment.
project(myproj) # trailing comment
# Between.
add_library(foo STATIC foo.c)
# Bracket comment:
#[[ multi
line ]]
add_executable(bar bar.c)
`
	nodes, err := Parse(src)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(nodes) != 3 {
		t.Errorf("nodes = %d, want 3", len(nodes))
	}
}

// TestParse_BracketArgumentInCommand pins a bracket arg gets
// preserved as a single argument string in the parsed Command.
func TestParse_BracketArgumentInCommand(t *testing.T) {
	src := "message([==[hello\nworld]==])"
	nodes, err := Parse(src)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	cmd := nodes[0].Command
	if cmd == nil || cmd.Name != "message" {
		t.Fatalf("got %#v", nodes[0])
	}
	if len(cmd.Args) != 1 || cmd.Args[0] != "[==[hello\nworld]==]" {
		t.Errorf("args = %#v", cmd.Args)
	}
}

// TestParse_LineNumbers pins the .Line attribute on Commands
// + IfBlock arms reflects the source line.
func TestParse_LineNumbers(t *testing.T) {
	src := "project(foo)\n\nif(LINUX)\n  target_sources(foo PRIVATE l.c)\nendif()\n"
	nodes, err := Parse(src)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if nodes[0].Command.Line != 1 {
		t.Errorf("project line = %d, want 1", nodes[0].Command.Line)
	}
	blk := nodes[1].If
	if blk == nil {
		t.Fatalf("expected if-block at nodes[1], got %#v", nodes[1])
	}
	if blk.StartLine != 3 {
		t.Errorf("if start line = %d, want 3", blk.StartLine)
	}
	if blk.EndLine != 5 {
		t.Errorf("endif line = %d, want 5", blk.EndLine)
	}
	if blk.Arms[0].Body[0].Command.Line != 4 {
		t.Errorf("body cmd line = %d, want 4", blk.Arms[0].Body[0].Command.Line)
	}
}

// TestUnquote pins the quote-stripping helper rounds quoted
// trace-style args to bare strings.
func TestUnquote(t *testing.T) {
	cases := map[string]string{
		`"foo"`:         "foo",
		`foo`:           "foo",
		`""`:            "",
		`"foo\nbar"`:    "foo\nbar",
		`"with \" esc"`: `with " esc`,
		`"back\\slash"`: `back\slash`,
	}
	for in, want := range cases {
		if got := Unquote(in); got != want {
			t.Errorf("Unquote(%q) = %q, want %q", in, got, want)
		}
	}
}
