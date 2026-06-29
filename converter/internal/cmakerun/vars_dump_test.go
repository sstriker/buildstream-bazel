package cmakerun

import (
	"encoding/hex"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// TestReadVarsDumpFromBuildDir: the nested-vars capture reads the dump
// dump-vars.cmake wrote into the build dir (CMAKE_BINARY_DIR). Present →
// parsed map; absent → (nil, nil) so a nested build with no dump degrades
// to "no nested vars" rather than erroring.
func TestReadVarsDumpFromBuildDir(t *testing.T) {
	buildDir := t.TempDir()
	body := hexLine("SUB_VALUE", "7") + hexLine("PROJECT_NAME", "sub")
	if err := os.WriteFile(filepath.Join(buildDir, VarsDumpFilename), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := ReadVarsDumpFromBuildDir(buildDir)
	if err != nil {
		t.Fatalf("ReadVarsDumpFromBuildDir: %v", err)
	}
	if want := (map[string]string{"SUB_VALUE": "7", "PROJECT_NAME": "sub"}); !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}

	// A build dir with no dump → (nil, nil), the degrade-to-no-vars path.
	got, err = ReadVarsDumpFromBuildDir(t.TempDir())
	if err != nil || got != nil {
		t.Errorf("absent dump should be (nil, nil); got (%v, %v)", got, err)
	}
}

// hexLine renders a "<NAME>=<HEX>\n"-style dump line; used by the
// parse tests so they don't have to spell out hex by hand.
func hexLine(name, value string) string {
	return name + "=" + hex.EncodeToString([]byte(value)) + "\n"
}

func TestParseVarsDump_HappyPath(t *testing.T) {
	body := []byte(hexLine("PROJECT_VERSION", "1.2.3") +
		hexLine("CFGLIB_VERSION_MAJOR", "1") +
		hexLine("HAVE_FOO", "1"))
	got, err := parseVarsDump(body)
	if err != nil {
		t.Fatalf("parseVarsDump: %v", err)
	}
	want := map[string]string{
		"PROJECT_VERSION":      "1.2.3",
		"CFGLIB_VERSION_MAJOR": "1",
		"HAVE_FOO":             "1",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestParseVarsDump_EmptyInput(t *testing.T) {
	got, err := parseVarsDump(nil)
	if err != nil {
		t.Fatalf("parseVarsDump(nil): %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected empty map; got %v", got)
	}
}

// TestParseVarsDump_ValuesRoundTripBytes verifies hex decoding
// preserves the exact bytes — values containing newlines, quotes,
// semicolons, NULs all survive.
func TestParseVarsDump_ValuesRoundTripBytes(t *testing.T) {
	body := []byte(
		hexLine("MULTILINE", "first\nsecond") +
			hexLine("QUOTED", `"with quotes"`) +
			hexLine("SEMI", "a;b;c") +
			hexLine("NUL_INSIDE", "before\x00after"))
	got, err := parseVarsDump(body)
	if err != nil {
		t.Fatalf("parseVarsDump: %v", err)
	}
	want := map[string]string{
		"MULTILINE":  "first\nsecond",
		"QUOTED":     `"with quotes"`,
		"SEMI":       "a;b;c",
		"NUL_INSIDE": "before\x00after",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestParseVarsDump_RejectsMalformed(t *testing.T) {
	cases := map[string]struct {
		in       string
		wantSubs string // substring expected in the error
	}{
		"missing-equals": {"PROJECT_VERSION_NOEQUAL\n", "missing '='"},
		"empty-name":     {"=" + hex.EncodeToString([]byte("oops")) + "\n", "empty variable name"},
		"non-hex-value":  {"PROJECT=not-hex-zzz\n", "decode hex"},
		"odd-length-hex": {"PROJECT=abc\n", "decode hex"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := parseVarsDump([]byte(tc.in))
			if err == nil {
				t.Fatalf("expected error for %q", tc.in)
			}
			if !strings.Contains(err.Error(), tc.wantSubs) {
				t.Errorf("error %q missing substring %q", err, tc.wantSubs)
			}
		})
	}
}

func TestParseVarsDump_BlankLinesIgnored(t *testing.T) {
	body := []byte("\n" + hexLine("FOO", "bar") + "\n\n" + hexLine("BAZ", "qux") + "\n")
	got, err := parseVarsDump(body)
	if err != nil {
		t.Fatalf("parseVarsDump: %v", err)
	}
	want := map[string]string{"FOO": "bar", "BAZ": "qux"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// TestParseVarsDump_FiltersVolatileViaIntegration verifies the
// post-parse filterVolatilePaths pass runs and drops *_BINARY_DIR
// entries plus values that contain the build-dir prefix as a
// substring.
func TestParseVarsDump_FiltersVolatileViaIntegration(t *testing.T) {
	body := []byte(
		hexLine("CMAKE_BINARY_DIR", "/tmp/build-XXXX") +
			hexLine("PROJECT_BINARY_DIR", "/tmp/build-XXXX") +
			hexLine("PROJECT_SOURCE_DIR", "/work/src") +
			hexLine("CMAKE_CACHEFILE_DIR", "/tmp/build-XXXX") +
			hexLine("HAS_BUILD_PATH", "build-tree-prefix=/tmp/build-XXXX/cache.txt") +
			hexLine("STABLE_VAR", "1") +
			hexLine("PROJECT_VERSION", "1.2.3"))
	got, err := parseVarsDump(body)
	if err != nil {
		t.Fatalf("parseVarsDump: %v", err)
	}
	// Volatile names dropped.
	for _, dropped := range []string{
		"CMAKE_BINARY_DIR",
		"PROJECT_BINARY_DIR",
		"PROJECT_SOURCE_DIR",
		"CMAKE_CACHEFILE_DIR",
		"HAS_BUILD_PATH",
	} {
		if _, ok := got[dropped]; ok {
			t.Errorf("expected %s dropped; got %v", dropped, got)
		}
	}
	// Stable vars kept.
	for k, want := range map[string]string{"STABLE_VAR": "1", "PROJECT_VERSION": "1.2.3"} {
		if got[k] != want {
			t.Errorf("expected %s=%q, got %q", k, want, got[k])
		}
	}
}

// TestFilterVolatilePaths_NameSuffixes covers the name-based
// drops directly so the volatility predicate's surface is locked
// in regardless of value-side build-dir scanning.
func TestFilterVolatilePaths_NameSuffixes(t *testing.T) {
	in := map[string]string{
		"CMAKE_BINARY_DIR":      "/tmp/build",
		"PROJECT_BINARY_DIR":    "/tmp/build",
		"FOO_BINARY_DIR":        "/tmp/build/sub",
		"PROJECT_SOURCE_DIR":    "/work/src",
		"FOO_SOURCE_DIR":        "/work/src/sub",
		"CMAKE_HOME_DIRECTORY":  "/work",
		"CMAKE_CACHEFILE_DIR":   "/tmp/build",
		"CMAKE_BUILD_TOOL":      "/usr/bin/ninja",
		"CMAKE_COMMAND":         "/usr/bin/cmake",
		"CMAKE_ROOT":            "/usr/share/cmake",
		"PROJECT_VERSION":       "1.0",
		"PROJECT_VERSION_MAJOR": "1",
	}
	got := filterVolatilePaths(in)
	wantKept := map[string]bool{
		"PROJECT_VERSION":       true,
		"PROJECT_VERSION_MAJOR": true,
	}
	for name := range got {
		if !wantKept[name] {
			t.Errorf("unexpected kept variable: %s = %q", name, got[name])
		}
	}
	for want := range wantKept {
		if _, ok := got[want]; !ok {
			t.Errorf("expected %s kept; got %v", want, got)
		}
	}
}

// TestFilterVolatilePaths_BuildDirSubstring verifies the value-
// side filter: any value whose bytes contain a build-dir prefix
// drops, even if the variable name doesn't match the
// *_BINARY_DIR / *_SOURCE_DIR suffix list.
func TestFilterVolatilePaths_BuildDirSubstring(t *testing.T) {
	in := map[string]string{
		"CMAKE_BINARY_DIR": "/tmp/run-2026/build",
		// HAS_PATH's name is benign but its value points into
		// the build dir; the volatility filter must drop it.
		"HAS_PATH": "log lives at /tmp/run-2026/build/cmake-config.log",
		// Stable: doesn't reference the build dir.
		"PROJECT_VERSION": "1.0",
	}
	got := filterVolatilePaths(in)
	if _, ok := got["HAS_PATH"]; ok {
		t.Errorf("expected HAS_PATH dropped; got %q", got["HAS_PATH"])
	}
	if got["PROJECT_VERSION"] != "1.0" {
		t.Errorf("expected PROJECT_VERSION=1.0 kept; got %q", got["PROJECT_VERSION"])
	}
}

// TestFilterVolatilePaths_NoBuildDirVarMissing covers the corner
// case where neither CMAKE_BINARY_DIR nor any *_BINARY_DIR
// variable is in the dump (atypical — but parsing a hand-
// crafted partial dump shouldn't blow up, just skip the
// substring filter pass).
func TestFilterVolatilePaths_NoBuildDirVarMissing(t *testing.T) {
	in := map[string]string{
		"PROJECT_VERSION": "1.0",
		"HAS_PATH":        "/tmp/anywhere/log.txt",
	}
	got := filterVolatilePaths(in)
	// PROJECT_VERSION kept.
	if got["PROJECT_VERSION"] != "1.0" {
		t.Errorf("expected PROJECT_VERSION kept; got %v", got)
	}
	// HAS_PATH kept too (no build-dir substring filter without a
	// known build-dir prefix).
	if got["HAS_PATH"] != "/tmp/anywhere/log.txt" {
		t.Errorf("expected HAS_PATH kept; got %v", got)
	}
}

func TestBuildDirPrefixes_CollectsDistinct(t *testing.T) {
	in := map[string]string{
		"CMAKE_BINARY_DIR":   "/tmp/build",
		"PROJECT_BINARY_DIR": "/tmp/build",     // dupe of CMAKE_BINARY_DIR
		"FOO_BINARY_DIR":     "/tmp/build/foo", // distinct subproject
		"BAR_BINARY_DIR":     "/tmp/build/bar", // distinct subproject
		"PROJECT_SOURCE_DIR": "/work",          // not a binary dir, ignored
		"PROJECT_VERSION":    "1.0",            // not a dir at all
	}
	got := buildDirPrefixes(in)
	wantSet := map[string]bool{
		"/tmp/build":     true,
		"/tmp/build/foo": true,
		"/tmp/build/bar": true,
	}
	if len(got) != len(wantSet) {
		t.Errorf("got %d prefixes %v, want %d (%v)", len(got), got, len(wantSet), wantSet)
	}
	for _, p := range got {
		if !wantSet[p] {
			t.Errorf("unexpected prefix %q in %v", p, got)
		}
	}
}

func TestBuildDirPrefixes_EmptyWhenNoBinaryDir(t *testing.T) {
	in := map[string]string{
		"PROJECT_VERSION":    "1.0",
		"PROJECT_SOURCE_DIR": "/work/src",
	}
	if got := buildDirPrefixes(in); got != nil {
		t.Errorf("expected nil; got %v", got)
	}
}
