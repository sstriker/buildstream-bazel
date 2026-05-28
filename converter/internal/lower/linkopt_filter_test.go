package lower

import "testing"

func TestIsCompileOnlyLinkFlag(t *testing.T) {
	cases := []struct {
		tok  string
		want bool
	}{
		// Preprocessor defines — drop.
		{"-DNDEBUG", true},
		{"-DVERSION=\"1.0\"", true},
		{"-UDEBUG", true},

		// Header preprocessor inputs — drop.
		{"-include", true},
		{"-imacros", true},
		{"-include=foo.h", true},

		// Language standard — drop.
		{"-std=c++17", true},
		{"-std=c11", true},

		// Include search — drop.
		{"-Iinclude", true},
		{"-iquote/abs/path", true},
		{"-isystem/usr/include", true},
		{"-idirafter/late", true},

		// Diagnostic controls — drop (but keep -Wl,*).
		{"-Wall", true},
		{"-Wextra", true},
		{"-Werror=format", true},
		{"-pedantic", true},
		{"-pedantic-errors", true},

		// Linker-flag passthrough — keep.
		{"-Wl,--version-script,foo.map", false},
		{"-Wl,-rpath,$ORIGIN", false},
		{"-Wl,-z,now", false},

		// Dual-purpose flags — keep.
		{"-O3", false},
		{"-O0", false},
		{"-g", false},
		{"-g3", false},
		{"-flto", false},
		{"-fno-rtti", false},
		{"-pthread", false},
		{"-march=native", false},
		{"-mtune=generic", false},
		{"-fsanitize=address", false},

		// Link search / library — keep.
		{"-L/usr/lib", false},
		{"-lz", false},
		{"-framework", false},
		{"Foundation", false},

		// Empty token — keep (false), caller already handles.
		{"", false},
	}
	for _, c := range cases {
		if got := isCompileOnlyLinkFlag(c.tok); got != c.want {
			t.Errorf("isCompileOnlyLinkFlag(%q) = %v; want %v", c.tok, got, c.want)
		}
	}
}
