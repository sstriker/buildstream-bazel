package lower

import "testing"

func TestIsCompileOnlyLinkFlag(t *testing.T) {
	cases := []struct {
		tok  string
		want bool
	}{
		// Warnings — compile only.
		{"-Wall", true},
		{"-Wextra", true},
		{"-Wno-unused", true},
		{"-Werror", true},
		{"-Werror=date-time", true},
		{"-Wpedantic", true},
		// `-Wl,` IS the linker driver passthrough — keep.
		{"-Wl,--gc-sections", false},
		{"-Wl,-z,now", false},
		// `-W` alone isn't a compile-only flag.
		{"-W", false},

		// Defines.
		{"-DFOO", true},
		{"-DFOO=1", true},
		// Bare `-D` is malformed; don't filter.
		{"-D", false},

		// Includes.
		{"-Iinclude", true},
		{"-I/abs/path", true},
		{"-I", false},
		{"-isystem", true},
		{"-isystem/usr/include", true},

		// Language standard.
		{"-std=c++17", true},
		{"-std=gnu99", true},

		// Pedantic.
		{"-pedantic", true},
		{"-pedantic-errors", true},

		// -f compile-only forms.
		{"-fno-semantic-interposition", true},
		{"-fno-lifetime-dse", true},
		{"-fno-rtti", true},
		{"-fno-exceptions", true},
		{"-fvisibility=hidden", true},
		{"-fvisibility-inlines-hidden", true},

		// -f link-affecting forms — keep.
		{"-flto", false},
		{"-flto=thin", false},
		{"-fuse-ld=lld", false},
		{"-fno-pic", false},

		// Linker stuff — keep.
		{"-lz", false},
		{"-Llibs", false},
		{"-shared", false},
		{"-static", false},
		{"-pie", false},
		{"-rdynamic", false},

		// Empty / unrelated.
		{"", false},
		{"foo.o", false},
		{"-O3", false},
	}
	for _, tc := range cases {
		t.Run(tc.tok, func(t *testing.T) {
			if got := isCompileOnlyLinkFlag(tc.tok); got != tc.want {
				t.Errorf("isCompileOnlyLinkFlag(%q) = %v; want %v", tc.tok, got, tc.want)
			}
		})
	}
}
