package lower

import "testing"

func TestRewriteCmakeECommand(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		// Single-op rewrites.
		{"create_symlink", "cmake -E create_symlink target link", "ln -sf target link"},
		{"make_directory single", "cmake -E make_directory build", "mkdir -p build"},
		{"make_directory multi", "cmake -E make_directory a b c", "mkdir -p a b c"},
		{"copy", "cmake -E copy src.h dst.h", "cp src.h dst.h"},
		{"copy_if_different", "cmake -E copy_if_different a.h b.h", "cp a.h b.h"},
		{"copy_directory", "cmake -E copy_directory src dst", "cp -r src dst"},
		{"copy_directory_if_different", "cmake -E copy_directory_if_different src dst", "cp -r src dst"},
		{"remove_directory", "cmake -E remove_directory tmp", "rm -rf tmp"},
		{"remove", "cmake -E remove a b", "rm -f a b"},
		{"rename", "cmake -E rename old new", "mv old new"},
		{"touch", "cmake -E touch stamp.txt", "touch stamp.txt"},
		{"true", "cmake -E true", "true"},
		{"echo", "cmake -E echo hello world", "echo hello world"},

		// Chain handling — each && stays intact, each cmake -E
		// invocation rewrites independently.
		{
			name: "chain && of multiple ops",
			in:   "cmake -E remove_directory dst && cmake -E copy_directory src dst",
			want: "rm -rf dst && cp -r src dst",
		},
		{
			name: "chain mixed with non-cmake-E",
			in:   "cmake -E touch foo && echo done",
			want: "touch foo && echo done",
		},

		// Absolute / relative cmake driver path.
		{
			name: "absolute cmake driver",
			in:   "/usr/bin/cmake -E touch foo",
			want: "touch foo",
		},

		// Pass-through cases.
		{"no cmake -E", "echo hello world", "echo hello world"},
		{"cmake -P (not -E)", "cmake -P script.cmake", "cmake -P script.cmake"},
		{"empty cmd", "", ""},
		{"unknown -E op", "cmake -E unknown_op arg", "cmake -E unknown_op arg"},
		{"create_symlink wrong arity", "cmake -E create_symlink only_one_arg", "cmake -E create_symlink only_one_arg"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := rewriteCmakeECommand(c.in); got != c.want {
				t.Errorf("rewriteCmakeECommand(%q) = %q; want %q", c.in, got, c.want)
			}
		})
	}
}
