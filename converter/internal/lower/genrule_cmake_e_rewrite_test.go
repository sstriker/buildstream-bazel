package lower

import "testing"

func TestRewriteCMakeEInvocations(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"passthrough — no cmake -E",
			"echo hello", "echo hello"},
		{"make_directory → mkdir -p",
			"cmake -E make_directory ./lib/ocaml/llvm",
			"mkdir -p ./lib/ocaml/llvm"},
		{"create_symlink → ln -sfn",
			"cmake -E create_symlink target link",
			"ln -sfn target link"},
		{"copy → cp",
			"cmake -E copy src dst",
			"cp src dst"},
		{"copy_if_different → cp",
			"cmake -E copy_if_different src dst",
			"cp src dst"},
		{"copy_directory → cp -r",
			"cmake -E copy_directory src dst",
			"cp -r src dst"},
		{"remove → rm -f",
			"cmake -E remove foo bar",
			"rm -f foo bar"},
		{"remove_directory → rm -rf",
			"cmake -E remove_directory foo",
			"rm -rf foo"},
		{"rename → mv",
			"cmake -E rename a b",
			"mv a b"},
		{"touch → touch",
			"cmake -E touch foo.stamp",
			"touch foo.stamp"},
		{"true → true",
			"cmake -E true",
			"true"},
		{"echo → echo",
			"cmake -E echo hi",
			"echo hi"},
		{"unsupported op passes through",
			"cmake -E env FOO=1 cmd",
			"cmake -E env FOO=1 cmd"},
		{"chained with &&",
			"cmake -E make_directory a && cmake -E touch a/x",
			"mkdir -p a && touch a/x"},
		{"chained with ; separator",
			"cmake -E touch a;cmake -E touch b",
			"touch a;touch b"},
		{"prefixed cmake (handled by host-bin first, but defensive)",
			"/foo/cmake -E touch f",
			"touch f"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := rewriteCMakeEInvocations(tc.in); got != tc.want {
				t.Errorf("rewriteCMakeEInvocations:\n  in:   %q\n  got:  %q\n  want: %q", tc.in, got, tc.want)
			}
		})
	}
}
