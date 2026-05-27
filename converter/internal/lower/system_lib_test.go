package lower

import "testing"

func TestSystemLibName(t *testing.T) {
	cases := []struct {
		path string
		want string
	}{
		// Standard multi-arch shapes.
		{"/usr/lib/x86_64-linux-gnu/libtinfo.so", "tinfo"},
		{"/usr/lib/x86_64-linux-gnu/libz.so", "z"},
		{"/usr/lib/aarch64-linux-gnu/libpthread.so.0", "pthread"},
		// Versioned shared libs.
		{"/usr/lib/libfoo.so.1.2.3", "foo"},
		// Static libs.
		{"/usr/lib64/libm.a", "m"},
		// /lib root.
		{"/lib/libc.so.6", "c"},
		// /usr/local.
		{"/usr/local/lib/libfoo.so", "foo"},
		// macOS .dylib.
		{"/usr/lib/libSystem.dylib", "System"},

		// Rejections:
		// Non-system path → stay elided.
		{"/opt/vendor/lib/libmystery.so", ""},
		// Unrecognized suffix.
		{"/usr/lib/libfoo.dll", ""},
		// Missing `lib` prefix.
		{"/usr/lib/foo.so", ""},
		// Empty name (`lib.so` is weird).
		{"/usr/lib/lib.so", ""},
		// Relative path (not under any system prefix).
		{"libfoo.so", ""},
		// Path that just happens to contain `/usr/lib/` mid-string.
		{"/opt/build/usr/lib/libfoo.so", ""},
	}
	for _, c := range cases {
		if got := systemLibName(c.path); got != c.want {
			t.Errorf("systemLibName(%q) = %q, want %q", c.path, got, c.want)
		}
	}
}
