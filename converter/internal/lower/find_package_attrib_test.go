package lower

import (
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/internal/fileapi"
)

func TestBuildFindPackageAttrib_CMakeVarsAuthoritative(t *testing.T) {
	events := []fileapi.Event{{
		Kind: "find_package-v1",
		Found: &fileapi.EventFindPackageFound{
			IsFound: true,
			Package: "ZLIB",
		},
	}}
	cmakeVars := map[string]string{
		"ZLIB_LIBRARIES":   "/usr/lib/x86_64-linux-gnu/libz.so",
		"ZLIB_INCLUDE_DIR": "/usr/include",
	}
	fa := buildFindPackageAttrib(events, cmakeVars)
	if fa == nil {
		t.Fatal("expected non-nil attribution")
	}
	if got := fa.Lookup("/usr/lib/x86_64-linux-gnu/libz.so"); got != "ZLIB" {
		t.Errorf("Lookup: got %q want ZLIB", got)
	}
}

func TestBuildFindPackageAttrib_AllUpperCaseFallback(t *testing.T) {
	// BZip2's find module sets BZIP2_LIBRARIES (all upper), not
	// BZip2_LIBRARIES. The packageVarKeys helper tries both.
	events := []fileapi.Event{{
		Kind: "find_package-v1",
		Found: &fileapi.EventFindPackageFound{
			IsFound: true,
			Package: "BZip2",
		},
	}}
	cmakeVars := map[string]string{
		"BZIP2_LIBRARIES": "/usr/lib/x86_64-linux-gnu/libbz2.so",
	}
	fa := buildFindPackageAttrib(events, cmakeVars)
	if fa == nil {
		t.Fatal("expected non-nil attribution")
	}
	if got := fa.Lookup("/usr/lib/x86_64-linux-gnu/libbz2.so"); got != "BZip2" {
		t.Errorf("Lookup: got %q want BZip2", got)
	}
}

func TestBuildFindPackageAttrib_SemicolonSplit(t *testing.T) {
	// `<Pkg>_LIBRARIES` is often a `;`-joined list (release + debug,
	// or main lib + dependencies). Each abs-path entry should land
	// in byPath.
	events := []fileapi.Event{{
		Kind: "find_package-v1",
		Found: &fileapi.EventFindPackageFound{
			IsFound: true,
			Package: "OpenSSL",
		},
	}}
	cmakeVars := map[string]string{
		"OPENSSL_LIBRARIES": "/usr/lib/libssl.so;/usr/lib/libcrypto.so",
	}
	fa := buildFindPackageAttrib(events, cmakeVars)
	if got := fa.Lookup("/usr/lib/libssl.so"); got != "OpenSSL" {
		t.Errorf("libssl: got %q want OpenSSL", got)
	}
	if got := fa.Lookup("/usr/lib/libcrypto.so"); got != "OpenSSL" {
		t.Errorf("libcrypto: got %q want OpenSSL", got)
	}
}

func TestBuildFindPackageAttrib_BasenameHeuristic_DumpVarsOff(t *testing.T) {
	// DumpVars off: cmakeVars is empty. The basename heuristic
	// still matches `lib<pkg>.<ext>` to the found package.
	events := []fileapi.Event{{
		Kind: "find_package-v1",
		Found: &fileapi.EventFindPackageFound{
			IsFound: true,
			Package: "expat",
		},
	}}
	fa := buildFindPackageAttrib(events, nil)
	if fa == nil {
		t.Fatal("expected non-nil attribution (foundPackages populated)")
	}
	if got := fa.Lookup("/usr/lib/x86_64-linux-gnu/libexpat.so.1"); got != "expat" {
		t.Errorf("heuristic libexpat: got %q want expat", got)
	}
}

func TestBuildFindPackageAttrib_BasenameHeuristic_NonObviousLibName(t *testing.T) {
	// BZip2 → libbz2 is the classic non-obvious-name case.
	// Without cmakeVars, the heuristic can't bridge this gap;
	// returns "" (caller falls through to "no attribution").
	events := []fileapi.Event{{
		Kind: "find_package-v1",
		Found: &fileapi.EventFindPackageFound{
			IsFound: true,
			Package: "BZip2",
		},
	}}
	fa := buildFindPackageAttrib(events, nil)
	if got := fa.Lookup("/usr/lib/x86_64-linux-gnu/libbz2.so"); got != "" {
		t.Errorf("heuristic shouldn't bridge BZip2→libbz2: got %q", got)
	}
}

func TestBuildFindPackageAttrib_SkipsNotFound(t *testing.T) {
	events := []fileapi.Event{
		{
			Kind: "find_package-v1",
			Found: &fileapi.EventFindPackageFound{
				IsFound: false,
				Package: "ZLIB",
			},
		},
		{
			Kind: "find_package-v1",
			// Missing Found → also skipped.
			Found: nil,
		},
	}
	fa := buildFindPackageAttrib(events, nil)
	if fa != nil {
		t.Errorf("expected nil (no IsFound=true events): %+v", fa)
	}
}

func TestBuildFindPackageAttrib_IgnoresNonFindPackageEvents(t *testing.T) {
	events := []fileapi.Event{
		{Kind: "try_compile-v1"},
		{Kind: "message-v1"},
	}
	fa := buildFindPackageAttrib(events, nil)
	if fa != nil {
		t.Errorf("expected nil: %+v", fa)
	}
}

func TestFindPackageAttrib_NilSafe(t *testing.T) {
	var fa *findPackageAttrib
	if got := fa.Lookup("/anything"); got != "" {
		t.Errorf("nil Lookup: got %q want empty", got)
	}
	if got := fa.SortedFoundPackages(); got != nil {
		t.Errorf("nil SortedFoundPackages: got %v want nil", got)
	}
}

func TestFindPackageAttrib_SortedFoundPackages(t *testing.T) {
	events := []fileapi.Event{
		{Kind: "find_package-v1", Found: &fileapi.EventFindPackageFound{IsFound: true, Package: "ZLIB"}},
		{Kind: "find_package-v1", Found: &fileapi.EventFindPackageFound{IsFound: true, Package: "BZip2"}},
		{Kind: "find_package-v1", Found: &fileapi.EventFindPackageFound{IsFound: true, Package: "LibLZMA"}},
	}
	fa := buildFindPackageAttrib(events, nil)
	got := fa.SortedFoundPackages()
	want := []string{"BZip2", "LibLZMA", "ZLIB"}
	if len(got) != len(want) {
		t.Fatalf("got %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("got[%d]=%q want %q", i, got[i], want[i])
		}
	}
}
