package shadow

import (
	"reflect"
	"testing"
)

// TestExtractPlatformConditionalSources_StreqLinux pins the
// canonical #217 Tier 1 case: a target_sources() call inside
// an `if(CMAKE_SYSTEM_NAME STREQUAL "Linux")` block surfaces
// the source as conditional on @platforms//os:linux.
func TestExtractPlatformConditionalSources_StreqLinux(t *testing.T) {
	trace := `
{"args":["CMAKE_SYSTEM_NAME","STREQUAL","Linux"],"cmd":"if","file":"/src/CMakeLists.txt","line":10}
{"args":["foo","PRIVATE","linux.c"],"cmd":"target_sources","file":"/src/CMakeLists.txt","line":11}
{"args":[],"cmd":"endif","file":"/src/CMakeLists.txt","line":12}
`
	got := ExtractPlatformConditionalSources([]byte(trace), "/src", map[string]bool{"foo": true})
	want := []PlatformConditionalSource{
		{Target: "foo", Source: "linux.c", SelectKey: "@platforms//os:linux"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %#v, want %#v", got, want)
	}
}

// TestExtractPlatformConditionalSources_AddLibraryInline pins
// the add_library(foo STATIC linux.c) shape inside the if
// block — different call from target_sources but same
// conditional-source attribution.
func TestExtractPlatformConditionalSources_AddLibraryInline(t *testing.T) {
	trace := `
{"args":["CMAKE_SYSTEM_NAME","STREQUAL","Darwin"],"cmd":"if","file":"/src/CMakeLists.txt","line":5}
{"args":["foo","STATIC","mac.c","shared.c"],"cmd":"add_library","file":"/src/CMakeLists.txt","line":6}
{"args":[],"cmd":"endif","file":"/src/CMakeLists.txt","line":7}
`
	got := ExtractPlatformConditionalSources([]byte(trace), "/src", map[string]bool{"foo": true})
	want := []PlatformConditionalSource{
		{Target: "foo", Source: "mac.c", SelectKey: "@platforms//os:darwin"},
		{Target: "foo", Source: "shared.c", SelectKey: "@platforms//os:darwin"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %#v, want %#v", got, want)
	}
}

// TestExtractPlatformConditionalSources_AddExecutable pins
// the add_executable shape with the WIN32 GUI-app keyword
// (which must be stripped before the sources are read).
func TestExtractPlatformConditionalSources_AddExecutable(t *testing.T) {
	trace := `
{"args":["CMAKE_SYSTEM_NAME","STREQUAL","Windows"],"cmd":"if","file":"/src/CMakeLists.txt","line":5}
{"args":["bar","WIN32","main_win.c"],"cmd":"add_executable","file":"/src/CMakeLists.txt","line":6}
{"args":[],"cmd":"endif","file":"/src/CMakeLists.txt","line":7}
`
	got := ExtractPlatformConditionalSources([]byte(trace), "/src", map[string]bool{"bar": true})
	want := []PlatformConditionalSource{
		{Target: "bar", Source: "main_win.c", SelectKey: "@platforms//os:windows"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %#v, want %#v", got, want)
	}
}

// TestExtractPlatformConditionalSources_OutsideIfBlock pins
// the negative: sources added at file scope (not inside any
// if) don't surface — pre-#217 behaviour is preserved for
// non-conditional sources.
func TestExtractPlatformConditionalSources_OutsideIfBlock(t *testing.T) {
	trace := `
{"args":["foo","PRIVATE","always.c"],"cmd":"target_sources","file":"/src/CMakeLists.txt","line":5}
`
	got := ExtractPlatformConditionalSources([]byte(trace), "/src", map[string]bool{"foo": true})
	if len(got) != 0 {
		t.Errorf("expected no platform-conditional sources, got %#v", got)
	}
}

// TestExtractPlatformConditionalSources_UnrecognizedPredicate
// pins that if() shapes we don't understand (NOT, UNIX, WIN32
// shorthand, complex boolean expressions) don't surface
// sources — better to leave them in flat srcs (where Bazel
// builds for the configured platform) than emit a select() arm
// against a key we can't be sure about.
func TestExtractPlatformConditionalSources_UnrecognizedPredicate(t *testing.T) {
	cases := []string{
		// UNIX shorthand
		`{"args":["UNIX"],"cmd":"if","file":"/src/CMakeLists.txt","line":5}
{"args":["foo","PRIVATE","src.c"],"cmd":"target_sources","file":"/src/CMakeLists.txt","line":6}
{"args":[],"cmd":"endif","file":"/src/CMakeLists.txt","line":7}`,
		// NOT WIN32
		`{"args":["NOT","WIN32"],"cmd":"if","file":"/src/CMakeLists.txt","line":5}
{"args":["foo","PRIVATE","src.c"],"cmd":"target_sources","file":"/src/CMakeLists.txt","line":6}
{"args":[],"cmd":"endif","file":"/src/CMakeLists.txt","line":7}`,
		// CMAKE_SYSTEM_NAME MATCHES (regex form)
		`{"args":["CMAKE_SYSTEM_NAME","MATCHES","Linux|Android"],"cmd":"if","file":"/src/CMakeLists.txt","line":5}
{"args":["foo","PRIVATE","src.c"],"cmd":"target_sources","file":"/src/CMakeLists.txt","line":6}
{"args":[],"cmd":"endif","file":"/src/CMakeLists.txt","line":7}`,
		// Unrecognized OS name
		`{"args":["CMAKE_SYSTEM_NAME","STREQUAL","HP-UX"],"cmd":"if","file":"/src/CMakeLists.txt","line":5}
{"args":["foo","PRIVATE","src.c"],"cmd":"target_sources","file":"/src/CMakeLists.txt","line":6}
{"args":[],"cmd":"endif","file":"/src/CMakeLists.txt","line":7}`,
	}
	for i, trace := range cases {
		got := ExtractPlatformConditionalSources([]byte(trace), "/src", map[string]bool{"foo": true})
		if len(got) != 0 {
			t.Errorf("case %d: expected no records, got %#v", i, got)
		}
	}
}

// TestExtractPlatformConditionalSources_ElseArmUnrecognized
// pins that the else() arm doesn't surface as a positive
// constraint (since the constraint would be NOT-of-something,
// not expressible as one @platforms//os:* label). Sources in
// the else arm stay in flat srcs.
func TestExtractPlatformConditionalSources_ElseArmUnrecognized(t *testing.T) {
	trace := `
{"args":["CMAKE_SYSTEM_NAME","STREQUAL","Linux"],"cmd":"if","file":"/src/CMakeLists.txt","line":5}
{"args":["foo","PRIVATE","linux.c"],"cmd":"target_sources","file":"/src/CMakeLists.txt","line":6}
{"args":[],"cmd":"else","file":"/src/CMakeLists.txt","line":7}
{"args":["foo","PRIVATE","other.c"],"cmd":"target_sources","file":"/src/CMakeLists.txt","line":8}
{"args":[],"cmd":"endif","file":"/src/CMakeLists.txt","line":9}
`
	got := ExtractPlatformConditionalSources([]byte(trace), "/src", map[string]bool{"foo": true})
	want := []PlatformConditionalSource{
		{Target: "foo", Source: "linux.c", SelectKey: "@platforms//os:linux"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %#v, want %#v", got, want)
	}
}

// TestExtractPlatformConditionalSources_ElseifArmRecognized
// pins that elseif(CMAKE_SYSTEM_NAME STREQUAL "X") is handled
// symmetrically to a fresh if() — the predicate's recognized
// shape surfaces sources under the corresponding constraint.
func TestExtractPlatformConditionalSources_ElseifArmRecognized(t *testing.T) {
	trace := `
{"args":["CMAKE_SYSTEM_NAME","STREQUAL","Linux"],"cmd":"if","file":"/src/CMakeLists.txt","line":5}
{"args":["foo","PRIVATE","linux.c"],"cmd":"target_sources","file":"/src/CMakeLists.txt","line":6}
{"args":["CMAKE_SYSTEM_NAME","STREQUAL","Darwin"],"cmd":"elseif","file":"/src/CMakeLists.txt","line":7}
{"args":["foo","PRIVATE","mac.c"],"cmd":"target_sources","file":"/src/CMakeLists.txt","line":8}
{"args":[],"cmd":"endif","file":"/src/CMakeLists.txt","line":9}
`
	got := ExtractPlatformConditionalSources([]byte(trace), "/src", map[string]bool{"foo": true})
	want := []PlatformConditionalSource{
		{Target: "foo", Source: "linux.c", SelectKey: "@platforms//os:linux"},
		{Target: "foo", Source: "mac.c", SelectKey: "@platforms//os:darwin"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %#v, want %#v", got, want)
	}
}

// TestExtractPlatformConditionalSources_NestedIf pins that an
// inner recognized if() wins over an outer unrecognized one
// (innermost-recognized policy from the comment on
// currentSelectKey).
func TestExtractPlatformConditionalSources_NestedIf(t *testing.T) {
	trace := `
{"args":["BUILD_TESTING"],"cmd":"if","file":"/src/CMakeLists.txt","line":5}
{"args":["CMAKE_SYSTEM_NAME","STREQUAL","Linux"],"cmd":"if","file":"/src/CMakeLists.txt","line":6}
{"args":["foo","PRIVATE","linux_test.c"],"cmd":"target_sources","file":"/src/CMakeLists.txt","line":7}
{"args":[],"cmd":"endif","file":"/src/CMakeLists.txt","line":8}
{"args":[],"cmd":"endif","file":"/src/CMakeLists.txt","line":9}
`
	got := ExtractPlatformConditionalSources([]byte(trace), "/src", map[string]bool{"foo": true})
	want := []PlatformConditionalSource{
		{Target: "foo", Source: "linux_test.c", SelectKey: "@platforms//os:linux"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %#v, want %#v", got, want)
	}
}

// TestExtractPlatformConditionalSources_UnknownTarget pins
// that calls naming a target not in knownTargets are skipped
// (matches the gating ExtractTargetIncludes does — keeps
// producer-side macro noise out of consumer-side IR).
func TestExtractPlatformConditionalSources_UnknownTarget(t *testing.T) {
	trace := `
{"args":["CMAKE_SYSTEM_NAME","STREQUAL","Linux"],"cmd":"if","file":"/src/CMakeLists.txt","line":5}
{"args":["unknown_target","PRIVATE","linux.c"],"cmd":"target_sources","file":"/src/CMakeLists.txt","line":6}
{"args":[],"cmd":"endif","file":"/src/CMakeLists.txt","line":7}
`
	got := ExtractPlatformConditionalSources([]byte(trace), "/src", map[string]bool{"foo": true})
	if len(got) != 0 {
		t.Errorf("expected no records for unknown target, got %#v", got)
	}
}

// TestExtractPlatformConditionalSources_SubdirSources pins
// that sources added in a subdir's CMakeLists.txt resolve to
// the right project-relative path (sourceRoot-anchored, not
// CMakeLists.txt-anchored).
func TestExtractPlatformConditionalSources_SubdirSources(t *testing.T) {
	trace := `
{"args":["CMAKE_SYSTEM_NAME","STREQUAL","Linux"],"cmd":"if","file":"/src/sub/CMakeLists.txt","line":5}
{"args":["foo","PRIVATE","linux.c"],"cmd":"target_sources","file":"/src/sub/CMakeLists.txt","line":6}
{"args":[],"cmd":"endif","file":"/src/sub/CMakeLists.txt","line":7}
`
	got := ExtractPlatformConditionalSources([]byte(trace), "/src", map[string]bool{"foo": true})
	want := []PlatformConditionalSource{
		{Target: "foo", Source: "sub/linux.c", SelectKey: "@platforms//os:linux"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %#v, want %#v", got, want)
	}
}

// TestExtractPlatformConditionalSources_SkipsAlias pins that
// add_library(foo ALIAS bar) and add_library(foo IMPORTED ...)
// shapes don't surface as conditional sources (they have no
// in-codebase sources to attribute).
func TestExtractPlatformConditionalSources_SkipsAlias(t *testing.T) {
	trace := `
{"args":["CMAKE_SYSTEM_NAME","STREQUAL","Linux"],"cmd":"if","file":"/src/CMakeLists.txt","line":5}
{"args":["foo","ALIAS","bar"],"cmd":"add_library","file":"/src/CMakeLists.txt","line":6}
{"args":["baz","SHARED","IMPORTED"],"cmd":"add_library","file":"/src/CMakeLists.txt","line":7}
{"args":[],"cmd":"endif","file":"/src/CMakeLists.txt","line":8}
`
	got := ExtractPlatformConditionalSources([]byte(trace), "/src", map[string]bool{"foo": true, "baz": true})
	if len(got) != 0 {
		t.Errorf("expected no records for alias/imported, got %#v", got)
	}
}

// TestExtractPlatformConditionalSources_SkipsInterfaceLibrary
// pins that add_library(foo INTERFACE) inside a conditional
// doesn't surface — interface libs have no compiled srcs to
// partition.
func TestExtractPlatformConditionalSources_SkipsInterfaceLibrary(t *testing.T) {
	trace := `
{"args":["CMAKE_SYSTEM_NAME","STREQUAL","Linux"],"cmd":"if","file":"/src/CMakeLists.txt","line":5}
{"args":["foo","INTERFACE"],"cmd":"add_library","file":"/src/CMakeLists.txt","line":6}
{"args":[],"cmd":"endif","file":"/src/CMakeLists.txt","line":7}
`
	got := ExtractPlatformConditionalSources([]byte(trace), "/src", map[string]bool{"foo": true})
	if len(got) != 0 {
		t.Errorf("expected no records for INTERFACE library, got %#v", got)
	}
}

// TestDecodeIncludesPlatformConditional pins that
// shadow.Decode (the single-pass entry callers use to get all
// extractions at once) populates PlatformConditionalSources
// alongside the other Decoded fields.
func TestDecodeIncludesPlatformConditional(t *testing.T) {
	trace := `
{"args":["CMAKE_SYSTEM_NAME","STREQUAL","Linux"],"cmd":"if","file":"/src/CMakeLists.txt","line":5}
{"args":["foo","PRIVATE","linux.c"],"cmd":"target_sources","file":"/src/CMakeLists.txt","line":6}
{"args":[],"cmd":"endif","file":"/src/CMakeLists.txt","line":7}
`
	d := Decode([]byte(trace), "/src", map[string]bool{"foo": true})
	want := []PlatformConditionalSource{
		{Target: "foo", Source: "linux.c", SelectKey: "@platforms//os:linux"},
	}
	if !reflect.DeepEqual(d.PlatformConditionalSources, want) {
		t.Errorf("got %#v, want %#v", d.PlatformConditionalSources, want)
	}
}

// TestCmakeSystemNameToConstraint pins the OS→constraint map
// shape so future additions are obvious. Empty string is the
// "unrecognized" sentinel.
func TestCmakeSystemNameToConstraint(t *testing.T) {
	cases := map[string]string{
		"Linux":   "@platforms//os:linux",
		"linux":   "@platforms//os:linux",
		"Darwin":  "@platforms//os:darwin",
		"Windows": "@platforms//os:windows",
		"FreeBSD": "@platforms//os:freebsd",
		"OpenBSD": "@platforms//os:openbsd",
		"NetBSD":  "@platforms//os:netbsd",
		"Android": "@platforms//os:android",
		"iOS":     "@platforms//os:ios",
		"tvOS":    "@platforms//os:tvos",
		"watchOS": "@platforms//os:watchos",
		"QNX":     "@platforms//os:qnx",
		"HP-UX":   "",
		"AIX":     "",
		"":        "",
	}
	for in, want := range cases {
		if got := cmakeSystemNameToConstraint(in); got != want {
			t.Errorf("cmakeSystemNameToConstraint(%q): got %q, want %q", in, got, want)
		}
	}
}
