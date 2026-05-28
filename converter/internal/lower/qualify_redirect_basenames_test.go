package lower

import "testing"

func TestQualifyRedirectBasenames(t *testing.T) {
	const subdir = "lib/Transforms/Hello"
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"basename redirect → qualified",
			"python3 -c '...' < in.exports > LLVMHello.exports",
			"python3 -c '...' < lib/Transforms/Hello/in.exports > lib/Transforms/Hello/LLVMHello.exports"},
		{"already-qualified passes through",
			"python3 -c '...' > lib/Transforms/Hello/already.txt",
			"python3 -c '...' > lib/Transforms/Hello/already.txt"},
		{"absolute path passes through",
			"echo hi > /tmp/abs.out",
			"echo hi > /tmp/abs.out"},
		{"bazel substitution passes through",
			"echo hi > $@",
			"echo hi > $@"},
		{"fd-numbered redirect ignored",
			"echo hi 2> err.log",
			"echo hi 2> err.log"},
		{"&> ignored",
			"echo hi &> all.log",
			"echo hi &> all.log"},
		{"append >>",
			"echo hi >> out",
			"echo hi >> lib/Transforms/Hello/out"},
		{"empty subdir → no-op (caller's responsibility, defensive)",
			"echo hi > out", "echo hi > out"},
		{"no redirects → passthrough",
			"echo hi", "echo hi"},
		{"redirect with no whitespace",
			"echo hi >out",
			"echo hi >lib/Transforms/Hello/out"},
		{"empty cmd",
			"", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sd := subdir
			if tc.name == "empty subdir → no-op (caller's responsibility, defensive)" {
				sd = ""
			}
			if got := qualifyRedirectBasenames(tc.in, sd); got != tc.want {
				t.Errorf("qualifyRedirectBasenames:\n  in:   %q\n  got:  %q\n  want: %q", tc.in, got, tc.want)
			}
		})
	}
}
