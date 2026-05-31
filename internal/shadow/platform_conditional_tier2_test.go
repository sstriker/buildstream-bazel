package shadow

import (
	"errors"
	"reflect"
	"sort"
	"testing"
)

// mapFS is a stub filesystem that returns pre-canned bytes
// keyed by path. Unknown paths return os.ErrNotExist.
type mapFS map[string][]byte

func (m mapFS) ReadFile(path string) ([]byte, error) {
	if b, ok := m[path]; ok {
		return b, nil
	}
	return nil, errors.New("not found: " + path)
}

// TestTier2_RecoversSkippedArm pins the canonical Tier-2 case:
// cmake configures for Linux, the trace records the `if(LINUX)`
// event and the body command for linux.c (entered), and the
// `elseif(WIN32)` predicate event with no body events
// (skipped). Tier 2 reopens CMakeLists.txt, parses the elseif
// arm, and emits a record for win.c under the windows
// constraint.
func TestTier2_RecoversSkippedArm(t *testing.T) {
	cmake := `add_library(app STATIC base.c)
if(LINUX)
  target_sources(app PRIVATE linux.c)
elseif(WIN32)
  target_sources(app PRIVATE win.c)
endif()
`
	// Trace: the LINUX arm fires; the WIN32 arm's predicate is
	// recorded (cmake evaluates each elseif to decide) but no
	// command body events fire inside it.
	trace := `
{"args":["LINUX"],"cmd":"if","file":"/src/CMakeLists.txt","line":2}
{"args":["app","PRIVATE","linux.c"],"cmd":"target_sources","file":"/src/CMakeLists.txt","line":3}
{"args":["WIN32"],"cmd":"elseif","file":"/src/CMakeLists.txt","line":4}
{"args":[],"cmd":"endif","file":"/src/CMakeLists.txt","line":6}
`
	fs := mapFS{"/src/CMakeLists.txt": []byte(cmake)}
	tier1 := ExtractPlatformConditionalSources([]byte(trace), "/src", map[string]bool{"app": true})
	want1 := []PlatformConditionalSource{
		{Target: "app", Source: "linux.c", SelectKey: "@platforms//os:linux"},
	}
	if !reflect.DeepEqual(tier1, want1) {
		t.Errorf("tier 1 got %#v, want %#v", tier1, want1)
	}
	tier2, _ := extractPlatformConditionalSourcesTier2(
		[]byte(trace), "/src", "", map[string]bool{"app": true}, tier1, fs,
	)
	want2 := []PlatformConditionalSource{
		{Target: "app", Source: "win.c", SelectKey: "@platforms//os:windows"},
	}
	if !reflect.DeepEqual(tier2, want2) {
		t.Errorf("tier 2 got %#v, want %#v", tier2, want2)
	}
}

// TestTier2_RecoversBothSidesOfIfElse pins the if/else shape where
// the else arm is the ENTERED one (project configured for non-Windows):
// `if(WIN32) win.c else() posix.c`. Since if(WIN32) is a recognized
// platform predicate, the #217 else-arm fix maps the entered else's
// posix.c to //conditions:default (Tier 1), and Tier 2 recovers the
// skipped win.c under the windows constraint. Together they fully
// reconstruct the select() from a single non-Windows configure.
func TestTier2_RecoversBothSidesOfIfElse(t *testing.T) {
	cmake := `add_library(app STATIC)
if(WIN32)
  target_sources(app PRIVATE win.c)
else()
  target_sources(app PRIVATE posix.c)
endif()
`
	trace := `
{"args":["WIN32"],"cmd":"if","file":"/src/CMakeLists.txt","line":2}
{"args":[],"cmd":"else","file":"/src/CMakeLists.txt","line":4}
{"args":["app","PRIVATE","posix.c"],"cmd":"target_sources","file":"/src/CMakeLists.txt","line":5}
{"args":[],"cmd":"endif","file":"/src/CMakeLists.txt","line":6}
`
	fs := mapFS{"/src/CMakeLists.txt": []byte(cmake)}
	tier1 := ExtractPlatformConditionalSources([]byte(trace), "/src", map[string]bool{"app": true})
	want1 := []PlatformConditionalSource{
		{Target: "app", Source: "posix.c", SelectKey: "//conditions:default"},
	}
	if !reflect.DeepEqual(tier1, want1) {
		t.Errorf("tier 1 got %#v, want %#v", tier1, want1)
	}
	tier2, _ := extractPlatformConditionalSourcesTier2(
		[]byte(trace), "/src", "", map[string]bool{"app": true}, tier1, fs,
	)
	want := []PlatformConditionalSource{
		{Target: "app", Source: "win.c", SelectKey: "@platforms//os:windows"},
	}
	if !reflect.DeepEqual(tier2, want) {
		t.Errorf("tier 2 got %#v, want %#v", tier2, want)
	}
}

// TestTier2_AddLibraryAndAddExecutable pins that the same
// recognition works for add_library / add_executable, not just
// target_sources.
func TestTier2_AddLibraryAndAddExecutable(t *testing.T) {
	cmake := `if(LINUX)
  add_library(liblinux STATIC linux1.c linux2.c)
elseif(WIN32)
  add_library(libwin STATIC win1.c)
  add_executable(appwin WIN32 winmain.c)
endif()
`
	trace := `
{"args":["LINUX"],"cmd":"if","file":"/src/CMakeLists.txt","line":1}
{"args":["liblinux","STATIC","linux1.c","linux2.c"],"cmd":"add_library","file":"/src/CMakeLists.txt","line":2}
{"args":["WIN32"],"cmd":"elseif","file":"/src/CMakeLists.txt","line":3}
{"args":[],"cmd":"endif","file":"/src/CMakeLists.txt","line":6}
`
	fs := mapFS{"/src/CMakeLists.txt": []byte(cmake)}
	known := map[string]bool{"liblinux": true, "libwin": true, "appwin": true}
	tier1 := ExtractPlatformConditionalSources([]byte(trace), "/src", known)
	tier2, _ := extractPlatformConditionalSourcesTier2(
		[]byte(trace), "/src", "", known, tier1, fs,
	)
	wantTier2 := []PlatformConditionalSource{
		{Target: "libwin", Source: "win1.c", SelectKey: "@platforms//os:windows"},
		{Target: "appwin", Source: "winmain.c", SelectKey: "@platforms//os:windows"},
	}
	if !reflect.DeepEqual(tier2, wantTier2) {
		t.Errorf("tier 2 got %#v, want %#v", tier2, wantTier2)
	}
}

// TestTier2_VarRefInSourceRefused pins the conservative-shape
// refusal: a source path containing `${...}` doesn't surface
// as a record (we don't have the cmake variable namespace at
// parse time). The refusal lands in the unsupported-reasons
// slice for diagnostics.
func TestTier2_VarRefInSourceRefused(t *testing.T) {
	cmake := `if(WIN32)
  target_sources(app PRIVATE ${CMAKE_CURRENT_SOURCE_DIR}/win.c)
endif()
`
	trace := `
{"args":["WIN32"],"cmd":"if","file":"/src/CMakeLists.txt","line":1}
{"args":[],"cmd":"endif","file":"/src/CMakeLists.txt","line":3}
`
	fs := mapFS{"/src/CMakeLists.txt": []byte(cmake)}
	tier2, unsupp := extractPlatformConditionalSourcesTier2(
		[]byte(trace), "/src", "", map[string]bool{"app": true}, nil, fs,
	)
	if len(tier2) != 0 {
		t.Errorf("expected no records (var-ref refused), got %#v", tier2)
	}
	if len(unsupp) != 1 || unsupp[0].Reason != ErrTier2UnknownVarRef.Error() {
		t.Errorf("expected one var-ref refusal, got %#v", unsupp)
	}
}

// TestTier2_GenexInSourceRefused pins the parallel refusal for
// generator expressions in source paths.
func TestTier2_GenexInSourceRefused(t *testing.T) {
	cmake := `if(WIN32)
  target_sources(app PRIVATE $<BUILD_INTERFACE:src.c>)
endif()
`
	trace := `
{"args":["WIN32"],"cmd":"if","file":"/src/CMakeLists.txt","line":1}
{"args":[],"cmd":"endif","file":"/src/CMakeLists.txt","line":3}
`
	fs := mapFS{"/src/CMakeLists.txt": []byte(cmake)}
	tier2, unsupp := extractPlatformConditionalSourcesTier2(
		[]byte(trace), "/src", "", map[string]bool{"app": true}, nil, fs,
	)
	if len(tier2) != 0 {
		t.Errorf("expected no records (genex refused), got %#v", tier2)
	}
	if len(unsupp) != 1 || unsupp[0].Reason != ErrTier2GenexInSource.Error() {
		t.Errorf("expected one genex refusal, got %#v", unsupp)
	}
}

// TestTier2_UnknownTargetSkipped pins that arm bodies naming a
// target outside knownTargets get dropped — same gating Tier 1
// applies. Producer-side cmake macros that target imported libs
// (where we have no in-codebase target) shouldn't pollute the
// IR with phantom records.
func TestTier2_UnknownTargetSkipped(t *testing.T) {
	cmake := `if(WIN32)
  target_sources(unknown_target PRIVATE win.c)
endif()
`
	trace := `
{"args":["WIN32"],"cmd":"if","file":"/src/CMakeLists.txt","line":1}
{"args":[],"cmd":"endif","file":"/src/CMakeLists.txt","line":3}
`
	fs := mapFS{"/src/CMakeLists.txt": []byte(cmake)}
	tier2, _ := extractPlatformConditionalSourcesTier2(
		[]byte(trace), "/src", "", map[string]bool{"app": true}, nil, fs,
	)
	if len(tier2) != 0 {
		t.Errorf("expected no records for unknown target, got %#v", tier2)
	}
}

// TestTier2_DedupsAgainstTier1 pins that a (target, source) pair
// Tier 1 already attributed doesn't get re-emitted by Tier 2.
// The trace-entered arm and the parsed skipped arm don't
// overlap by construction, but this defensive check matters
// when the cmake file changes between configure and parse
// (e.g. fixture drift) — Tier 2 won't second-guess Tier 1.
func TestTier2_DedupsAgainstTier1(t *testing.T) {
	cmake := `if(LINUX)
  target_sources(app PRIVATE shared.c)
endif()
`
	// Suppose Tier 1 already recorded shared.c under linux. The
	// (synthetic) trace below also has it inside the LINUX arm
	// so Tier 2 would otherwise re-walk and re-emit. The dedup
	// pass against `existing` suppresses the duplicate.
	trace := `
{"args":["LINUX"],"cmd":"if","file":"/src/CMakeLists.txt","line":1}
{"args":[],"cmd":"endif","file":"/src/CMakeLists.txt","line":3}
`
	fs := mapFS{"/src/CMakeLists.txt": []byte(cmake)}
	existing := []PlatformConditionalSource{
		{Target: "app", Source: "shared.c", SelectKey: "@platforms//os:linux"},
	}
	tier2, _ := extractPlatformConditionalSourcesTier2(
		[]byte(trace), "/src", "", map[string]bool{"app": true}, existing, fs,
	)
	if len(tier2) != 0 {
		t.Errorf("expected no dedup-skipped records, got %#v", tier2)
	}
}

// TestTier2_FileNotFoundSwallowedSilently pins that an
// unreadable CMakeLists.txt doesn't error — the Tier-2 driver
// silently drops the file and continues. This keeps live-cmake
// vs offline-replay running symmetrically when the
// remap-host-path resolution fails for some events but not
// others (rare but legal in cross-package fixtures).
func TestTier2_FileNotFoundSwallowedSilently(t *testing.T) {
	trace := `
{"args":["WIN32"],"cmd":"if","file":"/src/CMakeLists.txt","line":1}
{"args":[],"cmd":"endif","file":"/src/CMakeLists.txt","line":3}
`
	tier2, _ := extractPlatformConditionalSourcesTier2(
		[]byte(trace), "/src", "", map[string]bool{"app": true}, nil, mapFS{},
	)
	if len(tier2) != 0 {
		t.Errorf("expected no records (file not found), got %#v", tier2)
	}
}

// TestTier2_RemapHostPath pins the cmake-recorded-vs-on-disk
// path remap. The trace records absolute paths under the
// recording host's source root; offline-replay fixtures live
// at a different on-disk location. The remap translates them
// so the file reader finds the right bytes.
func TestTier2_RemapHostPath(t *testing.T) {
	cases := []struct {
		tracePath, traceRoot, hostRoot, want string
	}{
		{"/recording/src/CMakeLists.txt", "/recording/src", "/local/fixture", "/local/fixture/CMakeLists.txt"},
		{"/recording/src/sub/CMakeLists.txt", "/recording/src", "/local/fixture", "/local/fixture/sub/CMakeLists.txt"},
		// Same root → unchanged.
		{"/src/CMakeLists.txt", "/src", "/src", "/src/CMakeLists.txt"},
		// Empty hostRoot → unchanged.
		{"/src/CMakeLists.txt", "/src", "", "/src/CMakeLists.txt"},
		// Trace path outside trace root → unchanged (let
		// reader fail loudly).
		{"/usr/share/cmake/Modules/X.cmake", "/recording/src", "/local/fixture", "/usr/share/cmake/Modules/X.cmake"},
	}
	for _, tc := range cases {
		if got := remapHostPath(tc.tracePath, tc.traceRoot, tc.hostRoot); got != tc.want {
			t.Errorf("remapHostPath(%q, %q, %q) = %q, want %q",
				tc.tracePath, tc.traceRoot, tc.hostRoot, got, tc.want)
		}
	}
}

// TestTier2_UnrecognizedPredicateSkipped pins that an if-event
// with an unrecognized predicate (NOT, UNIX, AND-conjunctions,
// regex MATCHES) doesn't trigger a parse — we wouldn't have a
// constraint to attribute against. Matches Tier 1's policy.
func TestTier2_UnrecognizedPredicateSkipped(t *testing.T) {
	cmake := `if(UNIX)
  target_sources(app PRIVATE u.c)
endif()
`
	trace := `
{"args":["UNIX"],"cmd":"if","file":"/src/CMakeLists.txt","line":1}
{"args":[],"cmd":"endif","file":"/src/CMakeLists.txt","line":3}
`
	fs := mapFS{"/src/CMakeLists.txt": []byte(cmake)}
	tier2, _ := extractPlatformConditionalSourcesTier2(
		[]byte(trace), "/src", "", map[string]bool{"app": true}, nil, fs,
	)
	if len(tier2) != 0 {
		t.Errorf("expected no records for unrecognized predicate, got %#v", tier2)
	}
}

// TestTier2_CmakeInternalIgnored pins that an if-event firing
// from cmake's own internal modules (under /usr/share/cmake-*)
// doesn't drive a parse — those files aren't in the user's
// source tree.
func TestTier2_CmakeInternalIgnored(t *testing.T) {
	trace := `
{"args":["WIN32"],"cmd":"if","file":"/usr/share/cmake-3.28/Modules/SomeCmakeInternal.cmake","line":50}
{"args":[],"cmd":"endif","file":"/usr/share/cmake-3.28/Modules/SomeCmakeInternal.cmake","line":60}
`
	tier2, _ := extractPlatformConditionalSourcesTier2(
		[]byte(trace), "/src", "", map[string]bool{"app": true}, nil, mapFS{},
	)
	if len(tier2) != 0 {
		t.Errorf("expected no records for cmake-internal events, got %#v", tier2)
	}
}

// TestTier2_SubdirSources pins that sources in an arm of a
// subdir's CMakeLists.txt resolve to the right package-relative
// path (sourceRoot-anchored, not CMakeLists-anchored).
func TestTier2_SubdirSources(t *testing.T) {
	cmake := `if(LINUX)
  target_sources(app PRIVATE linux.c)
elseif(WIN32)
  target_sources(app PRIVATE win.c)
endif()
`
	trace := `
{"args":["LINUX"],"cmd":"if","file":"/src/sub/CMakeLists.txt","line":1}
{"args":["app","PRIVATE","linux.c"],"cmd":"target_sources","file":"/src/sub/CMakeLists.txt","line":2}
{"args":["WIN32"],"cmd":"elseif","file":"/src/sub/CMakeLists.txt","line":3}
{"args":[],"cmd":"endif","file":"/src/sub/CMakeLists.txt","line":5}
`
	fs := mapFS{"/src/sub/CMakeLists.txt": []byte(cmake)}
	tier2, _ := extractPlatformConditionalSourcesTier2(
		[]byte(trace), "/src", "", map[string]bool{"app": true}, nil, fs,
	)
	want := []PlatformConditionalSource{
		{Target: "app", Source: "sub/win.c", SelectKey: "@platforms//os:windows"},
	}
	if !reflect.DeepEqual(tier2, want) {
		t.Errorf("tier 2 got %#v, want %#v", tier2, want)
	}
}

// TestTier2_MultipleSourcesInArm pins multiple sources in one
// skipped arm all surface as separate records, sorted by the
// driver's emission order (insertion = parse order).
func TestTier2_MultipleSourcesInArm(t *testing.T) {
	cmake := `if(LINUX)
  target_sources(app PRIVATE linux.c)
elseif(WIN32)
  target_sources(app PRIVATE win1.c win2.c)
  target_sources(app PRIVATE win3.c)
endif()
`
	trace := `
{"args":["LINUX"],"cmd":"if","file":"/src/CMakeLists.txt","line":1}
{"args":["app","PRIVATE","linux.c"],"cmd":"target_sources","file":"/src/CMakeLists.txt","line":2}
{"args":["WIN32"],"cmd":"elseif","file":"/src/CMakeLists.txt","line":3}
{"args":[],"cmd":"endif","file":"/src/CMakeLists.txt","line":6}
`
	fs := mapFS{"/src/CMakeLists.txt": []byte(cmake)}
	tier2, _ := extractPlatformConditionalSourcesTier2(
		[]byte(trace), "/src", "", map[string]bool{"app": true}, nil, fs,
	)
	got := []string{}
	for _, r := range tier2 {
		if r.SelectKey != "@platforms//os:windows" {
			t.Errorf("unexpected key %q", r.SelectKey)
		}
		got = append(got, r.Source)
	}
	sort.Strings(got)
	want := []string{"win1.c", "win2.c", "win3.c"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// TestTier2_OffsetRemap pins the full host-source-root remap
// path: trace records `/recording/src/CMakeLists.txt`, the
// actual file lives at `/local/fixture/CMakeLists.txt`. The
// driver reads the right bytes after the remap.
func TestTier2_OffsetRemap(t *testing.T) {
	cmake := `if(LINUX)
  target_sources(app PRIVATE linux.c)
elseif(WIN32)
  target_sources(app PRIVATE win.c)
endif()
`
	trace := `
{"args":["LINUX"],"cmd":"if","file":"/recording/src/CMakeLists.txt","line":1}
{"args":["app","PRIVATE","linux.c"],"cmd":"target_sources","file":"/recording/src/CMakeLists.txt","line":2}
{"args":["WIN32"],"cmd":"elseif","file":"/recording/src/CMakeLists.txt","line":3}
{"args":[],"cmd":"endif","file":"/recording/src/CMakeLists.txt","line":5}
`
	fs := mapFS{"/local/fixture/CMakeLists.txt": []byte(cmake)}
	tier2, _ := extractPlatformConditionalSourcesTier2(
		[]byte(trace), "/recording/src", "/local/fixture",
		map[string]bool{"app": true}, nil, fs,
	)
	want := []PlatformConditionalSource{
		{Target: "app", Source: "win.c", SelectKey: "@platforms//os:windows"},
	}
	if !reflect.DeepEqual(tier2, want) {
		t.Errorf("tier 2 got %#v, want %#v", tier2, want)
	}
}
