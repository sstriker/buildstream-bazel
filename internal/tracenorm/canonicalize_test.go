package tracenorm

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCanonicalize_StripsPidPrefix verifies the pid prefix is
// removed regardless of pid width.
func TestCanonicalize_StripsPidPrefix(t *testing.T) {
	in := `1234  execve("/usr/bin/cc", ["cc", "-c", "x.c"], 0x0) = 0
99  execve("/usr/bin/ar", ["ar", "rcs", "libfoo.a", "x.o"], 0x0) = 0
1048576  execve("/usr/bin/ld", ["ld", "-o", "app", "x.o"], 0x0) = 0
`
	want := `execve("/usr/bin/cc", ["cc", "-c", "x.c"], 0x0) = 0
execve("/usr/bin/ar", ["ar", "rcs", "libfoo.a", "x.o"], 0x0) = 0
execve("/usr/bin/ld", ["ld", "-o", "app", "x.o"], 0x0) = 0
`
	if got := canonicalizeString(t, in, nil); got != want {
		t.Errorf("pid stripping mismatch\nwant:\n%s\ngot:\n%s", want, got)
	}
}

// TestCanonicalize_StableTempPaths verifies cross-event correlation
// (compile output → as input) survives canonicalization.
func TestCanonicalize_StableTempPaths(t *testing.T) {
	in := `1  execve("/usr/libexec/cc1", ["cc1", "-o", "/tmp/ccABC123.s"], 0x0) = 0
2  execve("/usr/bin/as", ["as", "-o", "/tmp/ccDEF456.o", "/tmp/ccABC123.s"], 0x0) = 0
3  execve("/usr/bin/ld", ["ld", "-o", "app", "/tmp/ccDEF456.o", "-plugin-opt=-fresolution=/tmp/ccGHI789.res"], 0x0) = 0
`
	got := canonicalizeString(t, in, nil)
	for _, want := range []string{
		`"-o", "/tmp/cc1.s"`,
		`"-o", "/tmp/cc2.o", "/tmp/cc1.s"`,
		`"-o", "app", "/tmp/cc2.o"`,
		`"-plugin-opt=-fresolution=/tmp/cc3.res"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q\n--got--\n%s", want, got)
		}
	}
	for _, banned := range []string{"ccABC123", "ccDEF456", "ccGHI789"} {
		if strings.Contains(got, banned) {
			t.Errorf("raw random token %q leaked through canonicalization\n%s", banned, got)
		}
	}
}

// TestCanonicalize_PrefixSubs verifies longest-first ordering.
func TestCanonicalize_PrefixSubs(t *testing.T) {
	in := `1  execve("/usr/bin/install", ["install", "greet", "/tmp/sandbox/install_root/usr/bin/greet"], 0x0) = 0
2  execve("/usr/bin/cc", ["cc", "-I/tmp/sandbox/dep_prefix/usr/include", "-c", "x.c"], 0x0) = 0
`
	subs := []PrefixSub{
		{From: "/tmp/sandbox", To: "/SANDBOX"},
		{From: "/tmp/sandbox/install_root", To: "/INSTALL_ROOT"},
		{From: "/tmp/sandbox/dep_prefix", To: "/DEP_PREFIX"},
	}
	got := canonicalizeString(t, in, subs)
	for _, want := range []string{
		`"/INSTALL_ROOT/usr/bin/greet"`,
		`"-I/DEP_PREFIX/usr/include"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q\n--got--\n%s", want, got)
		}
	}
	if strings.Contains(got, "/SANDBOX/install_root") {
		t.Errorf("shorter prefix sub fired before longer; got:\n%s", got)
	}
}

// TestCanonicalize_DeterministicAcrossRuns guards against
// nondeterminism in the canonicalizer (map iteration order, etc.).
func TestCanonicalize_DeterministicAcrossRuns(t *testing.T) {
	in := `1  execve("/usr/libexec/cc1", ["cc1", "-o", "/tmp/ccAAA.s"], 0x0) = 0
2  execve("/usr/libexec/cc1", ["cc1", "-o", "/tmp/ccBBB.s"], 0x0) = 0
3  execve("/usr/bin/as", ["as", "-o", "/tmp/ccCCC.o", "/tmp/ccAAA.s"], 0x0) = 0
4  execve("/usr/bin/as", ["as", "-o", "/tmp/ccDDD.o", "/tmp/ccBBB.s"], 0x0) = 0
`
	first := canonicalizeString(t, in, nil)
	for i := 0; i < 5; i++ {
		again := canonicalizeString(t, in, nil)
		if again != first {
			t.Fatalf("run %d differed\nfirst:\n%s\nagain:\n%s", i, first, again)
		}
	}
}

// TestCanonicalizeBytes_MatchesFileVariant verifies the byte-slice
// helper produces the same output as the file-based form. trace-
// publish uses CanonicalizeBytes; cmd/build-tracer uses the file
// form. Same canonicalized output is the contract.
func TestCanonicalizeBytes_MatchesFileVariant(t *testing.T) {
	in := `1234  execve("/usr/bin/cc", ["cc", "-c", "x.c"], 0x0) = 0
99  execve("/usr/bin/cc", ["cc", "-o", "/tmp/ccZZZ123.s"], 0x0) = 0
`
	subs := []PrefixSub{{From: "/tmp", To: "/T"}}
	fileForm := canonicalizeString(t, in, subs)
	bytesForm := string(CanonicalizeBytes([]byte(in), subs))
	if fileForm != bytesForm {
		t.Errorf("file/bytes variants diverged\nfile:\n%s\nbytes:\n%s", fileForm, bytesForm)
	}
}

func canonicalizeString(t *testing.T, in string, subs []PrefixSub) string {
	t.Helper()
	tmp := t.TempDir()
	rawPath := filepath.Join(tmp, "raw.log")
	outPath := filepath.Join(tmp, "out.log")
	if err := os.WriteFile(rawPath, []byte(in), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Canonicalize(rawPath, outPath, subs); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}
