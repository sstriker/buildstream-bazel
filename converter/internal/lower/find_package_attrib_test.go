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

// TestBuildFindPackageAttrib_CMakeVarsFoundFallback verifies the
// no-configureLog-events path: cmakes below 3.32 don't emit
// find_package-v1 events. cmake's convention <Pkg>_FOUND=TRUE
// is the reliable signal that find_package(Pkg) succeeded, set
// by every find module since cmake 1.x. The attribution must
// pick up packages discovered this way, otherwise the
// variable-form fix is inert on cmake 3.20-3.31 (including the
// orchestrator's 3.28 pin).
func TestBuildFindPackageAttrib_CMakeVarsFoundFallback(t *testing.T) {
	// No configureLog events (mimics cmake 3.28-3.31 reality);
	// cmakeVars carries the <Pkg>_FOUND + <Pkg>_LIBRARIES the
	// FindZLIB module set.
	cmakeVars := map[string]string{
		"ZLIB_FOUND":     "TRUE",
		"ZLIB_LIBRARIES": "/usr/lib/x86_64-linux-gnu/libz.so",
	}
	fa := buildFindPackageAttrib(nil, cmakeVars)
	if fa == nil {
		t.Fatal("expected non-nil attribution from cmakeVars <PKG>_FOUND path")
	}
	if got := fa.Lookup("/usr/lib/x86_64-linux-gnu/libz.so"); got != "ZLIB" {
		t.Errorf("Lookup: got %q want ZLIB", got)
	}
}

// TestBuildFindPackageAttrib_CMakeVarsFoundFallback_TruthyForms
// pins isTruthyCMakeBool's coverage: cmake's documented truthy
// constants (1, ON, YES, TRUE, Y, case-insensitive) all trigger
// attribution; the falsy set (OFF, NO, FALSE, N, empty,
// NOTFOUND, ...) does not.
func TestBuildFindPackageAttrib_CMakeVarsFoundFallback_TruthyForms(t *testing.T) {
	truthy := []string{"TRUE", "true", "True", "ON", "on", "YES", "1", "Y"}
	for _, v := range truthy {
		fa := buildFindPackageAttrib(nil, map[string]string{
			"ZLIB_FOUND":     v,
			"ZLIB_LIBRARIES": "/usr/lib/libz.so",
		})
		if fa == nil || fa.Lookup("/usr/lib/libz.so") != "ZLIB" {
			t.Errorf("ZLIB_FOUND=%q should be treated as truthy", v)
		}
	}
	falsy := []string{"FALSE", "OFF", "NO", "0", "N", "", "NOTFOUND", "ZLIB-NOTFOUND"}
	for _, v := range falsy {
		fa := buildFindPackageAttrib(nil, map[string]string{
			"ZLIB_FOUND":     v,
			"ZLIB_LIBRARIES": "/usr/lib/libz.so",
		})
		if fa != nil && fa.Lookup("/usr/lib/libz.so") != "" {
			t.Errorf("ZLIB_FOUND=%q should be treated as falsy; got attribution", v)
		}
	}
}

// TestBuildFindPackageAttrib_EventsAndVars_NoDuplicates pins the
// dedup contract: when a package is sourced from BOTH the
// configureLog event AND the cmakeVars <PKG>_FOUND fallback,
// it appears in foundPackages exactly once.
func TestBuildFindPackageAttrib_EventsAndVars_NoDuplicates(t *testing.T) {
	events := []fileapi.Event{{
		Kind: "find_package-v1",
		Found: &fileapi.EventFindPackageFound{
			IsFound: true, Package: "ZLIB",
		},
	}}
	cmakeVars := map[string]string{
		"ZLIB_FOUND":     "TRUE",
		"ZLIB_LIBRARIES": "/usr/lib/libz.so",
	}
	fa := buildFindPackageAttrib(events, cmakeVars)
	if fa == nil {
		t.Fatal("expected non-nil")
	}
	count := 0
	for _, p := range fa.foundPackages {
		if p == "ZLIB" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("ZLIB should appear exactly once in foundPackages; got %d (foundPackages=%v)", count, fa.foundPackages)
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
}
