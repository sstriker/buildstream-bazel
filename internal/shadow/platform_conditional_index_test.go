package shadow

import (
	"reflect"
	"testing"
)

// memFS is a tiny in-memory fsReader for the cmakeparse-path
// tests so the tests don't depend on filesystem layout.
type memFS map[string][]byte

func (m memFS) ReadFile(path string) ([]byte, error) {
	if b, ok := m[path]; ok {
		return b, nil
	}
	return nil, errMemFSNotFound{path: path}
}

type errMemFSNotFound struct{ path string }

func (e errMemFSNotFound) Error() string { return "memFS: not found: " + e.path }

// TestExtractPlatformConditionalSources_CmakeparsePath_HappyPath
// exercises the cmakeparse-based scope tracker that activates
// when the trace omits endif events (cmake's real JSON-v1
// format). Synthesises an in-tree CMakeLists.txt that places
// add_executable(foo bar.c) inside an if(WIN32) block and
// confirms the source surfaces as @platforms//os:windows.
func TestExtractPlatformConditionalSources_CmakeparsePath_HappyPath(t *testing.T) {
	// Trace with NO endif events — triggers the cmakeparse
	// path. Sources at line 7 (inside the if block per
	// CMakeLists.txt below).
	trace := `
{"args":["WIN32"],"cmd":"if","file":"/src/CMakeLists.txt","line":5}
{"args":["foo","bar.c"],"cmd":"add_executable","file":"/src/CMakeLists.txt","line":7}
`
	cmakeLists := `# 1
# 2
# 3
# 4
if(WIN32)
# 6
add_executable(foo bar.c)
# 8
endif()
`
	fs := memFS{"/src/CMakeLists.txt": []byte(cmakeLists)}
	got := ExtractPlatformConditionalSourcesWithFS([]byte(trace), "/src", "/src", map[string]bool{"foo": true}, fs)
	want := []PlatformConditionalSource{
		{Target: "foo", Source: "bar.c", SelectKey: "@platforms//os:windows"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %#v, want %#v", got, want)
	}
}

// TestExtractPlatformConditionalSources_CmakeparsePath_OutsideIfBlock
// pins the case that motivated this commit: a source at a line
// OUTSIDE any if-block must NOT inherit the constraint of an
// earlier if-block in the same file. The legacy trace-event
// stack would have left WIN32 on the stack (no endif to pop)
// and mis-attributed bar.c to @platforms//os:windows.
func TestExtractPlatformConditionalSources_CmakeparsePath_OutsideIfBlock(t *testing.T) {
	trace := `
{"args":["WIN32"],"cmd":"if","file":"/src/CMakeLists.txt","line":5}
{"args":["foo","bar.c"],"cmd":"add_executable","file":"/src/CMakeLists.txt","line":12}
`
	cmakeLists := `# 1
# 2
# 3
# 4
if(WIN32)
# 6
# 7
endif()
# 9
# 10
# 11
add_executable(foo bar.c)
`
	fs := memFS{"/src/CMakeLists.txt": []byte(cmakeLists)}
	got := ExtractPlatformConditionalSourcesWithFS([]byte(trace), "/src", "/src", map[string]bool{"foo": true}, fs)
	if len(got) != 0 {
		t.Errorf("expected no attributions (line 12 sits after endif at line 8); got %#v", got)
	}
}

// TestExtractPlatformConditionalSources_CmakeparsePath_NoFileFound
// pins that the cmakeparse path degrades gracefully when the
// CMakeLists.txt source bytes can't be read — no panic, no
// attributions, no error surfaced to the caller. Matches the
// pre-PR-#268 Tier-0 behaviour for projects without platform
// conditionals.
func TestExtractPlatformConditionalSources_CmakeparsePath_NoFileFound(t *testing.T) {
	trace := `
{"args":["WIN32"],"cmd":"if","file":"/missing/CMakeLists.txt","line":5}
{"args":["foo","bar.c"],"cmd":"add_executable","file":"/missing/CMakeLists.txt","line":7}
`
	fs := memFS{}
	got := ExtractPlatformConditionalSourcesWithFS([]byte(trace), "/missing", "/missing", map[string]bool{"foo": true}, fs)
	if len(got) != 0 {
		t.Errorf("expected graceful fallthrough; got %#v", got)
	}
}

// TestExtractPlatformConditionalSources_CmakeparsePath_NestedIf
// confirms innermost-recognized-key wins in the cmakeparse path,
// matching the legacy stack's policy.
func TestExtractPlatformConditionalSources_CmakeparsePath_NestedIf(t *testing.T) {
	trace := `
{"args":["WIN32"],"cmd":"if","file":"/src/CMakeLists.txt","line":3}
{"args":["LINUX"],"cmd":"if","file":"/src/CMakeLists.txt","line":5}
{"args":["foo","both.c"],"cmd":"add_executable","file":"/src/CMakeLists.txt","line":7}
`
	cmakeLists := `# 1
# 2
if(WIN32)
# 4
if(LINUX)
# 6
add_executable(foo both.c)
# 8
endif()
endif()
`
	fs := memFS{"/src/CMakeLists.txt": []byte(cmakeLists)}
	got := ExtractPlatformConditionalSourcesWithFS([]byte(trace), "/src", "/src", map[string]bool{"foo": true}, fs)
	want := []PlatformConditionalSource{
		// Innermost recognized wins.
		{Target: "foo", Source: "both.c", SelectKey: "@platforms//os:linux"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %#v, want %#v", got, want)
	}
}

// TestHasEndifEvent_FormatDetection pins the trace-shape detector
// that dispatches between the cmakeparse-path (no endif) and the
// legacy stack-path (with endif). Synthetic test traces emit
// endif explicitly; real cmake JSON-v1 doesn't.
func TestHasEndifEvent_FormatDetection(t *testing.T) {
	withEndif := []TraceEvent{
		{Cmd: "if"}, {Cmd: "endif"},
	}
	if !hasEndifEvent(withEndif) {
		t.Error("expected hasEndifEvent=true for trace with endif")
	}
	withoutEndif := []TraceEvent{
		{Cmd: "if"}, {Cmd: "if"}, {Cmd: "elseif"},
	}
	if hasEndifEvent(withoutEndif) {
		t.Error("expected hasEndifEvent=false for trace without endif")
	}
	// Case-insensitive comparison matches the existing observe() rules.
	withMixedCase := []TraceEvent{
		{Cmd: "ENDIF"},
	}
	if !hasEndifEvent(withMixedCase) {
		t.Error("expected hasEndifEvent=true for ENDIF (case-insensitive)")
	}
}
