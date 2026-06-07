package cmakeargv

import (
	"os"
	"path/filepath"
	"testing"
)

func writeTemp(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "CMakeLists.txt")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func eq(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %d lines %q, want %d %q", len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("line[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestLeadingComment(t *testing.T) {
	// add_library is on line 5; lines 3-4 are its leading comment block,
	// line 2 is blank (stops the scan), line 1 is an unrelated comment.
	body := "# unrelated top comment\n" + // 1
		"\n" + // 2 (blank — stops upward scan)
		"# This library wraps the vendored zlib.\n" + // 3
		"#   built static for the embedded target\n" + // 4
		"add_library(foo STATIC src/foo.c)\n" // 5
	p := writeTemp(t, body)
	got, err := LeadingComment(p, 5)
	if err != nil {
		t.Fatal(err)
	}
	eq(t, got, []string{
		"# This library wraps the vendored zlib.",
		"#   built static for the embedded target",
	})
}

func TestLeadingComment_StopsAtCode(t *testing.T) {
	// A comment trailing code on the line above is NOT a leading comment.
	body := "add_executable(prev main.c)  # trailing on code\n" + // 1
		"add_library(foo STATIC foo.c)\n" // 2
	p := writeTemp(t, body)
	got, err := LeadingComment(p, 2)
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Errorf("expected nil (line above is code), got %q", got)
	}
}

func TestLeadingComment_None(t *testing.T) {
	p := writeTemp(t, "add_library(foo STATIC foo.c)\n")
	got, err := LeadingComment(p, 1)
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Errorf("expected nil, got %q", got)
	}
}

func TestLeadingComment_BracketSkipped(t *testing.T) {
	// A bracket comment opener is out of scope: the scan stops at it rather
	// than capturing a malformed token.
	body := "#[[ bracket note ]]\n" + // 1
		"# real leading comment\n" + // 2
		"add_library(foo STATIC foo.c)\n" // 3
	p := writeTemp(t, body)
	got, err := LeadingComment(p, 3)
	if err != nil {
		t.Fatal(err)
	}
	eq(t, got, []string{"# real leading comment"})
}

func TestFileHeaderComment(t *testing.T) {
	body := "# Copyright 2026 the project authors.\n" + // 1
		"# SPDX-License-Identifier: Apache-2.0\n" + // 2
		"\n" + // 3 (blank ends header)
		"cmake_minimum_required(VERSION 3.20)\n" // 4
	p := writeTemp(t, body)
	got, err := FileHeaderComment(p)
	if err != nil {
		t.Fatal(err)
	}
	eq(t, got, []string{
		"# Copyright 2026 the project authors.",
		"# SPDX-License-Identifier: Apache-2.0",
	})
}

func TestFileHeaderComment_LeadingBlanksTolerated(t *testing.T) {
	body := "\n\n# header after blanks\ncmake_minimum_required(VERSION 3.20)\n"
	p := writeTemp(t, body)
	got, err := FileHeaderComment(p)
	if err != nil {
		t.Fatal(err)
	}
	eq(t, got, []string{"# header after blanks"})
}

func TestFileHeaderComment_NoneWhenOpensWithCommand(t *testing.T) {
	p := writeTemp(t, "cmake_minimum_required(VERSION 3.20)\n# not a header\n")
	got, err := FileHeaderComment(p)
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Errorf("expected nil header, got %q", got)
	}
}

func TestComment_MissingFile(t *testing.T) {
	if _, err := LeadingComment("/no/such/file", 1); err == nil {
		t.Error("expected error for missing file")
	}
}

func TestTrailingCommentLines(t *testing.T) {
	cases := []struct {
		name  string
		lines []string
		line  int
		want  string
	}{
		{"single line", []string{`add_library(foo STATIC foo.c)  # core lib`}, 1, "# core lib"},
		{"no trailing", []string{`add_library(foo STATIC foo.c)`}, 1, ""},
		{
			"multi-line call",
			[]string{`add_custom_command(`, `  OUTPUT x`, `  COMMAND gen)   # makes x`},
			1, "# makes x",
		},
		{
			"paren inside quoted arg",
			[]string{`add_custom_command(OUTPUT x COMMAND sh -c "echo (hi)")  # quoted`},
			1, "# quoted",
		},
		{"trailing after close on its own line", []string{`add_library(`, `  foo`, `)  # lib`}, 1, "# lib"},
		{"comment inside args is not trailing", []string{`add_library(foo  # not this`, `  bar)`}, 1, ""},
	}
	for _, c := range cases {
		if got := TrailingCommentLines(c.lines, c.line); got != c.want {
			t.Errorf("%s: TrailingCommentLines = %q, want %q", c.name, got, c.want)
		}
	}
}
