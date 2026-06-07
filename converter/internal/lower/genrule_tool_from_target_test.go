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
	execArtifacts := map[string]bool{
		"bin/llvm-min-tblgen":     true,
		"bin/clang-tblgen":        true,
		"obj/CustomGen/CustomGen": true,
		// lib/libfoo.so is intentionally NOT an executable.
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
			// VAR=<executable-artifact> form: a custom command passes a
			// built tool as a cmake -D arg (VTK's
			// -DEXE_SQLITE3=bin/Debug/sqlitebin-9.4). The embedded path is
			// lifted, keeping the `VAR=` prefix.
			name:      "executable inside VAR= arg lifts",
			in:        "echo --tool=bin/llvm-min-tblgen",
			wantCmd:   "echo --tool=$(location :llvm-min-tblgen)",
			wantTools: []string{":llvm-min-tblgen"},
		},
		{
			// A LIBRARY artifact embedded in an arg must NOT be lifted — it's
			// not a runnable tool (a linker flag / data path), so the gate
			// keys on execArtifacts.
			name:      "library inside VAR= arg does not lift",
			in:        "echo -DFOO=lib/libfoo.so",
			wantCmd:   "echo -DFOO=lib/libfoo.so",
			wantTools: nil,
		},
		{
			name:      "no map → passthrough",
			in:        "bin/llvm-min-tblgen -gen",
			wantCmd:   "bin/llvm-min-tblgen -gen",
			wantTools: nil,
		},
		{
			name:      "leading ./ rewrites — cmake-Ninja shape",
			in:        "python3 ./bin/llvm-min-tblgen -gen-attrs",
			wantCmd:   "python3 $(location :llvm-min-tblgen) -gen-attrs",
			wantTools: []string{":llvm-min-tblgen"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := artifacts
			e := execArtifacts
			if tc.name == "no map → passthrough" {
				m = nil
				e = nil
			}
			gotCmd, gotTools := rewriteToolFromTarget(tc.in, m, e)
			if gotCmd != tc.wantCmd {
				t.Errorf("cmd:\n  in:   %q\n  got:  %q\n  want: %q", tc.in, gotCmd, tc.wantCmd)
			}
			if !reflect.DeepEqual(gotTools, tc.wantTools) {
				t.Errorf("tools: got %v; want %v", gotTools, tc.wantTools)
			}
		})
	}
}
