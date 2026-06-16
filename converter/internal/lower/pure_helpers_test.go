package lower

import (
	"strings"
	"testing"
)

// Unit coverage for pure parsing/classification helpers that were exercised only
// transitively (0% in `go test` coverage). These pin parsing edge cases so a
// future tweak can't silently change them.

func TestUnescapeCMakeString(t *testing.T) {
	cases := map[string]string{
		"no escapes here": "no escapes here", // fast path (no backslash)
		`line\nbreak`:     "line\nbreak",     // \n -> newline
		`a\tb`:            "a\tb",            // \t -> tab
		`r\rn`:            "r\rn",            // \r -> CR
		`one\\two`:        `one\two`,         // \\ -> single backslash
		`say \"hi\"`:      `say "hi"`,        // \" -> quote
		`a\;b`:            "a;b",             // \; -> ;
		`x\$y`:            "x$y",             // \$ -> $
		`trailing\`:       `trailing\`,       // lone trailing backslash preserved
	}
	for in, want := range cases {
		if got := unescapeCMakeString(in); got != want {
			t.Errorf("unescapeCMakeString(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSoleProtoBaseInCmd(t *testing.T) {
	cases := map[string]string{
		"protoc --cpp_out=. foo.proto":    "foo",
		"protoc dir/sub/Msg.proto":        "Msg",
		"protoc a.proto b.proto":          "", // two protos -> ambiguous
		"protoc --cpp_out=. --grpc_out=.": "", // none
		"cc -c foo.c":                     "", // no proto
	}
	for in, want := range cases {
		if got := soleProtoBaseInCmd(in); got != want {
			t.Errorf("soleProtoBaseInCmd(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestIsSharedOnlyLinkFlag(t *testing.T) {
	shared := []string{
		"-Wl,--version-script,foo.map", "-Wl,--version-script=foo.map",
		"-Wl,-soname,libfoo.so.1", "-Wl,--soname=libfoo",
		"-Wl,--retain-symbols-file=syms.txt",
	}
	for _, tok := range shared {
		if !isSharedOnlyLinkFlag(tok) {
			t.Errorf("isSharedOnlyLinkFlag(%q) = false, want true", tok)
		}
	}
	for _, tok := range []string{"-lfoo", "-Wl,-z,defs", "-L/usr/lib", "--version-script"} {
		if isSharedOnlyLinkFlag(tok) {
			t.Errorf("isSharedOnlyLinkFlag(%q) = true, want false", tok)
		}
	}
}

func TestSoversionFromTags(t *testing.T) {
	if got := soversionFromTags([]string{"x", "cmake-codegen-soversion=3", "y"}, "1"); got != "3" {
		t.Errorf("present tag: got %q, want 3", got)
	}
	if got := soversionFromTags([]string{"x", "y"}, "1"); got != "1" {
		t.Errorf("absent tag: got %q, want default 1", got)
	}
	if got := soversionFromTags([]string{"cmake-codegen-soversion="}, "1"); got != "1" {
		t.Errorf("empty value: got %q, want default 1", got)
	}
}

func TestIsGeneratedHeaderPath(t *testing.T) {
	for _, p := range []string{"foo.h", "foo.hh", "foo.hpp", "foo.hxx", "foo.ipp", "foo.inc", "foo.def", "foo.gen", "DIR/foo.HXX"} {
		if !isGeneratedHeaderPath(p) {
			t.Errorf("isGeneratedHeaderPath(%q) = false, want true", p)
		}
	}
	for _, p := range []string{"foo.cpp", "foo.c", "foo.cc", "foo", "foo.txt"} {
		if isGeneratedHeaderPath(p) {
			t.Errorf("isGeneratedHeaderPath(%q) = true, want false", p)
		}
	}
}

func TestMakeCIdentifier(t *testing.T) {
	cases := map[string]string{
		"":         "",
		"plain_id": "plain_id",
		"a-b.c/d":  "a_b_c_d", // non-alnum -> _
		"9lives":   "_9lives", // leading digit -> _ prefix (the uncovered branch)
		"Foo.Bar":  "Foo_Bar",
	}
	for in, want := range cases {
		if got := makeCIdentifier(in); got != want {
			t.Errorf("makeCIdentifier(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSupportedCMakeEOpsList(t *testing.T) {
	// Formatting helper — just confirm it lists known ops in "name (desc)" form,
	// sorted/joined, and is non-empty.
	got := supportedCMakeEOpsList()
	if got == "" || !strings.Contains(got, "(") {
		t.Errorf("supportedCMakeEOpsList() = %q, want a non-empty `name (desc)` list", got)
	}
}
