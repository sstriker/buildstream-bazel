package cmakeparse

import (
	"reflect"
	"testing"
)

// kinds drops the line numbers from a token stream so the
// per-test fixture stays compact when shape, not position,
// is what's being asserted.
func kinds(ts []token) []tokenKind {
	out := make([]tokenKind, len(ts))
	for i, t := range ts {
		out[i] = t.kind
	}
	return out
}

// TestLex_SimpleCommand pins the minimal command-invocation
// shape: identifier, paren, args, paren, EOF.
func TestLex_SimpleCommand(t *testing.T) {
	toks, err := lex(`add_library(foo STATIC foo.c)`)
	if err != nil {
		t.Fatalf("lex: %v", err)
	}
	wantKinds := []tokenKind{tokWord, tokLParen, tokWord, tokWord, tokWord, tokRParen, tokEOF}
	if !reflect.DeepEqual(kinds(toks), wantKinds) {
		t.Errorf("kinds = %v, want %v", kinds(toks), wantKinds)
	}
	if toks[0].text != "add_library" || toks[2].text != "foo" || toks[3].text != "STATIC" || toks[4].text != "foo.c" {
		t.Errorf("text mismatch: %#v", toks)
	}
}

// TestLex_QuotedArgument pins that quoted arguments stay
// atomic — internal spaces don't split them.
func TestLex_QuotedArgument(t *testing.T) {
	toks, err := lex(`if("foo bar" STREQUAL baz)`)
	if err != nil {
		t.Fatalf("lex: %v", err)
	}
	if toks[2].kind != tokWord || toks[2].text != `"foo bar"` {
		t.Errorf("quoted arg = %#v", toks[2])
	}
}

// TestLex_BracketArgument pins the bracket-argument syntax
// [=[ ... ]=] — bracketed text stays atomic, parens inside
// don't terminate, newlines inside count for line tracking.
func TestLex_BracketArgument(t *testing.T) {
	src := "message([=[hello (world)\nline2]=])"
	toks, err := lex(src)
	if err != nil {
		t.Fatalf("lex: %v", err)
	}
	// Expect: ident `message`, `(`, arg `[=[...]=]`, `)`, EOF.
	if len(toks) != 5 {
		t.Fatalf("len = %d, want 5; toks = %#v", len(toks), toks)
	}
	if toks[2].kind != tokWord {
		t.Errorf("token 2 kind = %v, want tokWord", toks[2].kind)
	}
	if toks[2].text != "[=[hello (world)\nline2]=]" {
		t.Errorf("bracket arg text = %q", toks[2].text)
	}
}

// TestLex_BracketComment pins #[[ ... ]] bracket comments are
// skipped wholesale — parens / newlines inside don't matter.
func TestLex_BracketComment(t *testing.T) {
	src := "add_library(foo #[[ comment\nwith (parens) ]] STATIC foo.c)"
	toks, err := lex(src)
	if err != nil {
		t.Fatalf("lex: %v", err)
	}
	// Expect tokens `add_library`, `foo`, `STATIC`, `foo.c` —
	// the comment is gone.
	words := []string{}
	for _, tk := range toks {
		if tk.kind == tokWord {
			words = append(words, tk.text)
		}
	}
	want := []string{"add_library", "foo", "STATIC", "foo.c"}
	if !reflect.DeepEqual(words, want) {
		t.Errorf("words = %v, want %v", words, want)
	}
}

// TestLex_LineComment pins `# ...` comments are skipped to
// end-of-line.
func TestLex_LineComment(t *testing.T) {
	src := "add_library(foo STATIC foo.c) # a comment\nadd_library(bar STATIC bar.c)"
	toks, err := lex(src)
	if err != nil {
		t.Fatalf("lex: %v", err)
	}
	calls := 0
	for _, tk := range toks {
		if tk.kind == tokWord && tk.text == "add_library" {
			calls++
		}
	}
	if calls != 2 {
		t.Errorf("add_library tokens = %d, want 2", calls)
	}
}

// TestLex_VarRef pins ${var} and $<genex> are preserved as
// opaque substrings inside the argument they're part of.
func TestLex_VarRef(t *testing.T) {
	src := `target_sources(foo PRIVATE ${CMAKE_CURRENT_SOURCE_DIR}/src.c)`
	toks, err := lex(src)
	if err != nil {
		t.Fatalf("lex: %v", err)
	}
	// The `${...}` stays atomic inside the source-path arg.
	var words []string
	for _, tk := range toks {
		if tk.kind == tokWord {
			words = append(words, tk.text)
		}
	}
	want := []string{"target_sources", "foo", "PRIVATE", "${CMAKE_CURRENT_SOURCE_DIR}/src.c"}
	if !reflect.DeepEqual(words, want) {
		t.Errorf("words = %#v, want %#v", words, want)
	}
}

// TestLex_NestedVarRef pins nested ${${var}} stays atomic.
func TestLex_NestedVarRef(t *testing.T) {
	src := `foo(${${INNER}})`
	toks, err := lex(src)
	if err != nil {
		t.Fatalf("lex: %v", err)
	}
	var words []string
	for _, tk := range toks {
		if tk.kind == tokWord {
			words = append(words, tk.text)
		}
	}
	want := []string{"foo", "${${INNER}}"}
	if !reflect.DeepEqual(words, want) {
		t.Errorf("words = %v, want %v", words, want)
	}
}

// TestLex_GenexParens pins generator expressions $<...> are
// preserved even when they contain parens-like chars (`<` and
// `>` are balanced as if they were parens for this scan).
func TestLex_GenexParens(t *testing.T) {
	src := `target_sources(foo PRIVATE $<BUILD_INTERFACE:src.c>)`
	toks, err := lex(src)
	if err != nil {
		t.Fatalf("lex: %v", err)
	}
	var words []string
	for _, tk := range toks {
		if tk.kind == tokWord {
			words = append(words, tk.text)
		}
	}
	want := []string{"target_sources", "foo", "PRIVATE", "$<BUILD_INTERFACE:src.c>"}
	if !reflect.DeepEqual(words, want) {
		t.Errorf("words = %v, want %v", words, want)
	}
}

// TestLex_UnterminatedQuote pins that an unterminated quoted
// arg surfaces as an error (not silent best-effort).
func TestLex_UnterminatedQuote(t *testing.T) {
	_, err := lex(`add_library(foo "unterm`)
	if err == nil {
		t.Errorf("expected error for unterminated quoted arg")
	}
}

// TestLex_LineTracking pins the line numbers attached to each
// token reflect the source-line origin of the token's first
// character.
func TestLex_LineTracking(t *testing.T) {
	src := "add_library(foo\n  STATIC\n  foo.c)\nadd_library(bar STATIC bar.c)"
	toks, err := lex(src)
	if err != nil {
		t.Fatalf("lex: %v", err)
	}
	// Find the second add_library — should be on line 4.
	count := 0
	for _, tk := range toks {
		if tk.kind == tokWord && tk.text == "add_library" {
			count++
			if count == 2 && tk.line != 4 {
				t.Errorf("second add_library line = %d, want 4 (saw token %#v)", tk.line, tk)
			}
		}
	}
	if count != 2 {
		t.Errorf("expected 2 add_library tokens, got %d", count)
	}
}
