package tracenorm

import (
	"reflect"
	"testing"
)

// TestCanonicalize_OpenatDroppedWithoutSourceRoot verifies the
// legacy AC byte schema: traces that happen to carry openat lines
// (e.g., a build-tracer ran with read capture but the publisher
// didn't opt into the oracle) get those lines stripped so the
// canonical bytes match a no-openat trace.
func TestCanonicalize_OpenatDroppedWithoutSourceRoot(t *testing.T) {
	in := `1234  execve("/usr/bin/cc", ["cc", "-c", "x.c"], 0x0) = 0
1234  openat(AT_FDCWD, "/work/src/x.c", O_RDONLY|O_CLOEXEC) = 3
1234  execve("/usr/bin/ar", ["ar", "rcs", "libfoo.a", "x.o"], 0x0) = 0
`
	got := string(CanonicalizeBytes([]byte(in), nil))
	want := `execve("/usr/bin/cc", ["cc", "-c", "x.c"], 0x0) = 0
execve("/usr/bin/ar", ["ar", "rcs", "libfoo.a", "x.o"], 0x0) = 0
`
	if got != want {
		t.Errorf("openat passthrough without source-root\nwant:\n%s\ngot:\n%s", want, got)
	}
}

// TestCanonicalize_OpenatFilteredAndStabilized verifies the
// oracle path: with SourceRoot configured, openat lines for
// paths inside the root pass through with the path rewritten
// source-relative and the `= <fd>` return value stripped.
// Paths outside the root drop.
func TestCanonicalize_OpenatFilteredAndStabilized(t *testing.T) {
	in := `1234  execve("/usr/bin/cc", ["cc", "-c", "x.c"], 0x0) = 0
1234  openat(AT_FDCWD, "/work/src/x.c", O_RDONLY|O_CLOEXEC) = 3
1234  openat(AT_FDCWD, "/work/include/foo.h", O_RDONLY|O_CLOEXEC) = 4
1234  openat(AT_FDCWD, "/lib/x86_64-linux-gnu/libc.so.6", O_RDONLY|O_CLOEXEC) = 5
1234  openat(AT_FDCWD, "/etc/ld.so.cache", O_RDONLY|O_CLOEXEC) = 6
`
	got := string(CanonicalizeBytesWith([]byte(in), Options{SourceRoot: "/work"}))
	want := `execve("/usr/bin/cc", ["cc", "-c", "x.c"], 0x0) = 0
openat(AT_FDCWD, "src/x.c", O_RDONLY|O_CLOEXEC) = ?
openat(AT_FDCWD, "include/foo.h", O_RDONLY|O_CLOEXEC) = ?
`
	if got != want {
		t.Errorf("openat oracle pass\nwant:\n%s\ngot:\n%s", want, got)
	}
}

// TestCanonicalize_OpenatStableAcrossFdValues verifies the fd-
// suffix strip: two trace runs that recorded different fd values
// (run-volatile) produce identical canonical bytes.
func TestCanonicalize_OpenatStableAcrossFdValues(t *testing.T) {
	a := `7  openat(AT_FDCWD, "/work/x.c", O_RDONLY|O_CLOEXEC) = 3
8  openat(AT_FDCWD, "/work/y.c", O_RDONLY|O_CLOEXEC) = 11
`
	b := `7  openat(AT_FDCWD, "/work/x.c", O_RDONLY|O_CLOEXEC) = 99
8  openat(AT_FDCWD, "/work/y.c", O_RDONLY|O_CLOEXEC) = 4
`
	gotA := string(CanonicalizeBytesWith([]byte(a), Options{SourceRoot: "/work"}))
	gotB := string(CanonicalizeBytesWith([]byte(b), Options{SourceRoot: "/work"}))
	if gotA != gotB {
		t.Errorf("fd values leaked into canonical bytes\na:\n%s\nb:\n%s", gotA, gotB)
	}
}

// TestCanonicalize_OpenatNonAtFdcwdDirfd verifies that openat
// calls using a non-AT_FDCWD dirfd (e.g. an O_PATH-opened
// directory) still get captured when the pathname is
// absolute. The kernel ignores dirfd for absolute paths;
// dropping these would silently lose source-tree reads from
// callers that don't use the AT_FDCWD form. The canonicalized
// line rewrites the dirfd back to AT_FDCWD so the byte
// schema stays stable.
func TestCanonicalize_OpenatNonAtFdcwdDirfd(t *testing.T) {
	in := `42  openat(7, "/work/include/foo.h", O_RDONLY|O_CLOEXEC) = 9
43  openat(-1, "/work/src/x.c", O_RDONLY) = 10
`
	got := string(CanonicalizeBytesWith([]byte(in), Options{SourceRoot: "/work"}))
	want := `openat(AT_FDCWD, "include/foo.h", O_RDONLY|O_CLOEXEC) = ?
openat(AT_FDCWD, "src/x.c", O_RDONLY) = ?
`
	if got != want {
		t.Errorf("non-AT_FDCWD dirfd handling\nwant:\n%s\ngot:\n%s", want, got)
	}
}

// TestCanonicalize_OpenatRelativePathsDropped verifies that
// relative openat paths (which require per-call cwd context to
// resolve) are dropped rather than mis-rooted.
func TestCanonicalize_OpenatRelativePathsDropped(t *testing.T) {
	in := `1  openat(AT_FDCWD, "Makefile", O_RDONLY) = 3
2  openat(AT_FDCWD, "/work/configure.ac", O_RDONLY) = 4
`
	got := string(CanonicalizeBytesWith([]byte(in), Options{SourceRoot: "/work"}))
	want := `openat(AT_FDCWD, "configure.ac", O_RDONLY) = ?
`
	if got != want {
		t.Errorf("relative-path drop\nwant:\n%s\ngot:\n%s", want, got)
	}
}

func TestExtractReads_BasicShape(t *testing.T) {
	canonical := []byte(`execve("/usr/bin/cc", ["cc", "-c", "x.c"], 0x0) = 0
openat(AT_FDCWD, "src/x.c", O_RDONLY|O_CLOEXEC) = ?
openat(AT_FDCWD, "include/foo.h", O_RDONLY|O_CLOEXEC) = ?
openat(AT_FDCWD, "src/x.c", O_RDONLY|O_CLOEXEC) = ?
`)
	got := ExtractReads(canonical)
	want := []string{"include/foo.h", "src/x.c"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ExtractReads = %v, want %v", got, want)
	}
}

func TestExtractReads_EmptyOrNoOpenat(t *testing.T) {
	cases := map[string][]byte{
		"empty": nil,
		"no openat": []byte(`execve("/usr/bin/cc", ["cc"], 0x0) = 0
execve("/usr/bin/ar", ["ar"], 0x0) = 0
`),
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			if got := ExtractReads(in); got != nil {
				t.Errorf("ExtractReads = %v, want nil", got)
			}
		})
	}
}

// TestExtractReads_RoundTrip verifies the end-to-end shape: raw
// trace → CanonicalizeBytesWith(SourceRoot=...) → ExtractReads
// returns the expected source-relative read set.
func TestExtractReads_RoundTrip(t *testing.T) {
	raw := []byte(`123  execve("/usr/bin/cc", ["cc", "-c", "x.c"], 0x0) = 0
456  openat(AT_FDCWD, "/work/src/x.c", O_RDONLY|O_CLOEXEC) = 3
789  openat(AT_FDCWD, "/work/include/foo.h", O_RDONLY|O_CLOEXEC) = 4
12  openat(AT_FDCWD, "/usr/include/stdio.h", O_RDONLY|O_CLOEXEC) = 5
`)
	canonical := CanonicalizeBytesWith(raw, Options{SourceRoot: "/work"})
	got := ExtractReads(canonical)
	want := []string{"include/foo.h", "src/x.c"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("round trip\ncanonical:\n%s\nreads = %v, want %v", canonical, got, want)
	}
}
