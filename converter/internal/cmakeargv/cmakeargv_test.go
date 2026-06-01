package cmakeargv

import (
	"os"
	"path/filepath"
	"testing"
)

func TestTokenize_SimpleCall(t *testing.T) {
	call, err := tokenizeCall(`add_library(foo STATIC src/a.c src/b.c)`, "add_library")
	if err != nil {
		t.Fatalf("Tokenize: %v", err)
	}
	want := []string{"foo", "STATIC", "src/a.c", "src/b.c"}
	if len(call.Args) != len(want) {
		t.Fatalf("Args: %v, want %v", call.Args, want)
	}
	for i, w := range want {
		if call.Args[i] != w {
			t.Errorf("Args[%d] = %q, want %q", i, call.Args[i], w)
		}
	}
}

func TestTokenize_KeywordsInTargetLinkLibraries(t *testing.T) {
	call, err := tokenizeCall(
		`target_link_libraries(foo PUBLIC zlib openssl PRIVATE jsoncpp INTERFACE iface)`,
		"target_link_libraries")
	if err != nil {
		t.Fatalf("Tokenize: %v", err)
	}
	want := []string{"foo", "PUBLIC", "zlib", "openssl", "PRIVATE", "jsoncpp", "INTERFACE", "iface"}
	for i, w := range want {
		if call.Args[i] != w {
			t.Errorf("Args[%d] = %q, want %q", i, call.Args[i], w)
		}
	}
}

func TestTokenize_QuotedArgs(t *testing.T) {
	call, err := tokenizeCall(
		`add_definitions("-DFOO=1" "-DBAR=\"baz\"")`,
		"add_definitions")
	if err != nil {
		t.Fatalf("Tokenize: %v", err)
	}
	want := []string{`-DFOO=1`, `-DBAR="baz"`}
	for i, w := range want {
		if call.Args[i] != w {
			t.Errorf("Args[%d] = %q, want %q", i, call.Args[i], w)
		}
	}
}

func TestTokenize_BracketArg(t *testing.T) {
	call, err := tokenizeCall(
		`set(CONTENT [=[verbatim "string" with ${vars} and ;semis]=])`,
		"set")
	if err != nil {
		t.Fatalf("Tokenize: %v", err)
	}
	if len(call.Args) != 2 {
		t.Fatalf("Args len: %d (%v)", len(call.Args), call.Args)
	}
	if call.Args[0] != "CONTENT" {
		t.Errorf("Args[0]: %q", call.Args[0])
	}
	want := `verbatim "string" with ${vars} and ;semis`
	if call.Args[1] != want {
		t.Errorf("Args[1]: %q, want %q", call.Args[1], want)
	}
}

func TestTokenize_MultilineCall(t *testing.T) {
	body := `target_link_libraries(foo
    PUBLIC
        zlib
        openssl
    PRIVATE
        jsoncpp
)`
	call, err := tokenizeCall(body, "target_link_libraries")
	if err != nil {
		t.Fatalf("Tokenize: %v", err)
	}
	want := []string{"foo", "PUBLIC", "zlib", "openssl", "PRIVATE", "jsoncpp"}
	if len(call.Args) != len(want) {
		t.Fatalf("Args: %v", call.Args)
	}
	for i, w := range want {
		if call.Args[i] != w {
			t.Errorf("Args[%d] = %q, want %q", i, call.Args[i], w)
		}
	}
}

func TestTokenize_CommentsBetweenArgs(t *testing.T) {
	body := `target_link_libraries(foo
    # Public deps:
    PUBLIC zlib
    # Private deps:
    PRIVATE jsoncpp
)`
	call, err := tokenizeCall(body, "target_link_libraries")
	if err != nil {
		t.Fatalf("Tokenize: %v", err)
	}
	want := []string{"foo", "PUBLIC", "zlib", "PRIVATE", "jsoncpp"}
	for i, w := range want {
		if call.Args[i] != w {
			t.Errorf("Args[%d] = %q, want %q", i, call.Args[i], w)
		}
	}
}

func TestTokenize_VariableRefPreservedAsLiteral(t *testing.T) {
	// We don't expand ${VAR}; the caller decides whether the
	// literal can be matched (used for keyword recovery; the
	// keywords themselves are never inside ${...}).
	call, err := tokenizeCall(
		`target_link_libraries(foo PUBLIC ${SOME_DEP_VAR})`,
		"target_link_libraries")
	if err != nil {
		t.Fatalf("Tokenize: %v", err)
	}
	if call.Args[2] != `${SOME_DEP_VAR}` {
		t.Errorf("var ref should be preserved: %q", call.Args[2])
	}
}

func TestTokenize_CaseInsensitiveCommand(t *testing.T) {
	call, err := tokenizeCall(
		`ADD_LIBRARY(foo STATIC src/a.c)`,
		"add_library")
	if err != nil {
		t.Fatalf("Tokenize: %v", err)
	}
	if call.Command != "add_library" {
		t.Errorf("Command: %q", call.Command)
	}
	if call.Args[0] != "foo" {
		t.Errorf("Args[0]: %q", call.Args[0])
	}
}

func TestTokenize_MismatchedCommandErrors(t *testing.T) {
	_, err := tokenizeCall(
		`add_library(foo)`,
		"target_link_libraries")
	if err == nil {
		t.Fatal("expected mismatch error")
	}
}

func TestTokenize_UnterminatedParen(t *testing.T) {
	_, err := tokenizeCall(`add_library(foo STATIC src/a.c`, "add_library")
	if err == nil {
		t.Fatal("expected unterminated error")
	}
}

func TestReadCall_LiveFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "CMakeLists.txt")
	body := `cmake_minimum_required(VERSION 3.20)
project(test C)
add_library(foo STATIC src/a.c)
target_link_libraries(foo
    PUBLIC zlib
    PRIVATE jsoncpp
)
`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	// Line 4 is `target_link_libraries(foo`.
	call, err := ReadCall(path, 4, "target_link_libraries")
	if err != nil {
		t.Fatalf("ReadCall: %v", err)
	}
	want := []string{"foo", "PUBLIC", "zlib", "PRIVATE", "jsoncpp"}
	for i, w := range want {
		if call.Args[i] != w {
			t.Errorf("Args[%d] = %q, want %q", i, call.Args[i], w)
		}
	}
}

func TestReadCall_LineOutOfRange(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "CMakeLists.txt")
	if err := os.WriteFile(path, []byte("add_library(foo)\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := ReadCall(path, 99, "add_library")
	if err == nil {
		t.Fatal("expected error for out-of-range line")
	}
}

func TestReadCall_MissingFile(t *testing.T) {
	_, err := ReadCall("/nonexistent/path.cmake", 1, "add_library")
	if err == nil {
		t.Fatal("expected error for missing file")
	}
	var ce *Error
	if !errorsAs(err, &ce) {
		t.Errorf("expected *Error wrapping; got %T", err)
	}
}

// tiny errors.As shim to avoid an import dance with the test stdlib.
func errorsAs(err error, target **Error) bool {
	for err != nil {
		if e, ok := err.(*Error); ok {
			*target = e
			return true
		}
		unwrap, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = unwrap.Unwrap()
	}
	return false
}
