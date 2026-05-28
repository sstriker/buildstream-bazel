package lower

import (
	"reflect"
	"testing"
)

func TestRewriteToolFromTarget(t *testing.T) {
	artifacts := map[string]string{
		"bin/llvm-min-tblgen":     "llvm-min-tblgen",
		"bin/clang-tblgen":        "clang-tblgen",
		"lib/libfoo.so":           "foo",
		"obj/CustomGen/CustomGen": "CustomGen",
	}

	cases := []struct {
		name      string
		in        string
		wantCmd   string
		wantTools []string
	}{
		{
			name:      "passthrough — no artifact ref",
			in:        "echo hello",
			wantCmd:   "echo hello",
			wantTools: nil,
		},
		{
			name:      "leading tblgen ref",
			in:        "bin/llvm-min-tblgen -gen-attrs include/llvm/IR/Attributes.td -o out.inc",
			wantCmd:   "$(location :llvm-min-tblgen) -gen-attrs include/llvm/IR/Attributes.td -o out.inc",
			wantTools: []string{":llvm-min-tblgen"},
		},
		{
			name:      "two distinct tool refs",
			in:        "bin/llvm-min-tblgen -gen-foo && bin/clang-tblgen -gen-bar",
			wantCmd:   "$(location :llvm-min-tblgen) -gen-foo && $(location :clang-tblgen) -gen-bar",
			wantTools: []string{":llvm-min-tblgen", ":clang-tblgen"},
		},
		{
			name:      "repeated tool dedups",
			in:        "bin/llvm-min-tblgen a && bin/llvm-min-tblgen b",
			wantCmd:   "$(location :llvm-min-tblgen) a && $(location :llvm-min-tblgen) b",
			wantTools: []string{":llvm-min-tblgen"},
		},
		{
			name:      "tool inside arg does not rewrite",
			in:        "echo --tool=bin/llvm-min-tblgen",
			wantCmd:   "echo --tool=bin/llvm-min-tblgen",
			wantTools: nil,
		},
		{
			name:      "no map → passthrough",
			in:        "bin/llvm-min-tblgen -gen",
			wantCmd:   "bin/llvm-min-tblgen -gen",
			wantTools: nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := artifacts
			if tc.name == "no map → passthrough" {
				m = nil
			}
			gotCmd, gotTools := rewriteToolFromTarget(tc.in, m)
			if gotCmd != tc.wantCmd {
				t.Errorf("cmd:\n  in:   %q\n  got:  %q\n  want: %q", tc.in, gotCmd, tc.wantCmd)
			}
			if !reflect.DeepEqual(gotTools, tc.wantTools) {
				t.Errorf("tools: got %v; want %v", gotTools, tc.wantTools)
			}
		})
	}
}
