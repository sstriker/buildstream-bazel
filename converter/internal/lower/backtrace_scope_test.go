package lower

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/internal/fileapi"
)

// stageCMakeListsForTest writes a synthetic CMakeLists.txt and
// returns its abs path. The backtrace graph's File entries store
// the abs path verbatim, so the test must thread that same path
// through.
func stageCMakeListsForTest(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "CMakeLists.txt")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestBacktraceRecoverLinkScope_BasicKeywords(t *testing.T) {
	path := stageCMakeListsForTest(t, `cmake_minimum_required(VERSION 3.20)
project(test C)
add_library(foo STATIC src/a.c)
target_link_libraries(foo PUBLIC zlib PRIVATE jsoncpp)
`)
	// Backtrace points line 4 (the target_link_libraries call).
	r := &fileapi.Reply{
		Targets: map[string]fileapi.Target{
			"foo::@": {
				Name: "foo",
				Type: "STATIC_LIBRARY",
				Dependencies: []fileapi.TargetDependency{
					{Id: "zlib::@1", Backtrace: 1},
					{Id: "jsoncpp::@2", Backtrace: 1},
				},
				BacktraceGraph: fileapi.BacktraceGraph{
					Commands: []string{"target_link_libraries"},
					Files:    []string{path},
					Nodes: []fileapi.BacktraceNode{
						{},                             // root sentinel
						{File: 0, Line: 4, Command: 0}, // tll call site
					},
				},
			},
		},
	}
	scope := backtraceRecoverLinkScope(r)
	if scope["foo"]["zlib"] != "PUBLIC" {
		t.Errorf("zlib scope: %q", scope["foo"]["zlib"])
	}
	if scope["foo"]["jsoncpp"] != "PRIVATE" {
		t.Errorf("jsoncpp scope: %q", scope["foo"]["jsoncpp"])
	}
}

func TestBacktraceRecoverLinkScope_LegacyPositional(t *testing.T) {
	// target_link_libraries(foo zlib jsoncpp) — no keyword;
	// defaults to PUBLIC for every dep.
	path := stageCMakeListsForTest(t, `add_library(foo STATIC src/a.c)
target_link_libraries(foo zlib jsoncpp)
`)
	r := &fileapi.Reply{
		Targets: map[string]fileapi.Target{
			"foo::@": {
				Name: "foo",
				Dependencies: []fileapi.TargetDependency{
					{Id: "zlib::@", Backtrace: 1},
					{Id: "jsoncpp::@", Backtrace: 1},
				},
				BacktraceGraph: fileapi.BacktraceGraph{
					Commands: []string{"target_link_libraries"},
					Files:    []string{path},
					Nodes: []fileapi.BacktraceNode{
						{},
						{File: 0, Line: 2, Command: 0},
					},
				},
			},
		},
	}
	scope := backtraceRecoverLinkScope(r)
	if scope["foo"]["zlib"] != "PUBLIC" {
		t.Errorf("zlib scope (positional): %q", scope["foo"]["zlib"])
	}
	if scope["foo"]["jsoncpp"] != "PUBLIC" {
		t.Errorf("jsoncpp scope (positional): %q", scope["foo"]["jsoncpp"])
	}
}

func TestBacktraceRecoverLinkScope_InterfaceKeyword(t *testing.T) {
	path := stageCMakeListsForTest(t, `add_library(foo INTERFACE)
target_link_libraries(foo INTERFACE iface_dep)
`)
	r := &fileapi.Reply{
		Targets: map[string]fileapi.Target{
			"foo::@": {
				Name: "foo",
				Dependencies: []fileapi.TargetDependency{
					{Id: "iface_dep::@", Backtrace: 1},
				},
				BacktraceGraph: fileapi.BacktraceGraph{
					Commands: []string{"target_link_libraries"},
					Files:    []string{path},
					Nodes: []fileapi.BacktraceNode{
						{},
						{File: 0, Line: 2, Command: 0},
					},
				},
			},
		},
	}
	scope := backtraceRecoverLinkScope(r)
	if scope["foo"]["iface_dep"] != "INTERFACE" {
		t.Errorf("iface_dep scope: %q", scope["foo"]["iface_dep"])
	}
}

func TestBacktraceRecoverLinkScope_OutermostUserFrame(t *testing.T) {
	// A macro wrap: the leaf backtrace points to a cmake-internal
	// file (would-be macro); the parent is the user's call. The
	// outermost-user-frame walk should return the user's frame.
	userPath := stageCMakeListsForTest(t, `add_library(foo STATIC src/a.c)
my_link_helper(foo zlib)
`)
	r := &fileapi.Reply{
		Targets: map[string]fileapi.Target{
			"foo::@": {
				Name: "foo",
				Dependencies: []fileapi.TargetDependency{
					// Backtrace 2 = leaf inside fake macro; parent = 1 user call.
					{Id: "zlib::@", Backtrace: 2},
				},
				BacktraceGraph: fileapi.BacktraceGraph{
					Commands: []string{"my_link_helper", "target_link_libraries"},
					Files:    []string{userPath, "/usr/share/cmake-3.28/Modules/MyMacro.cmake"},
					Nodes: []fileapi.BacktraceNode{
						{},                             // root
						{File: 0, Line: 2, Command: 0}, // user macro call
						{File: 1, Line: 5, Command: 1, Parent: intPtr(1)}, // internal tll
					},
				},
			},
		},
	}
	// Note: this test's outermost-user-frame returns the
	// my_link_helper call, but `my_link_helper` isn't a TLL-like
	// command — so recovery silently skips. Confirms the
	// command-filter gate.
	scope := backtraceRecoverLinkScope(r)
	if scope["foo"]["zlib"] != "" {
		t.Errorf("expected no recovery for non-TLL outermost frame; got %q",
			scope["foo"]["zlib"])
	}
}

func TestBacktraceRecoverLinkScope_BuildInterfaceGenexUnwrap(t *testing.T) {
	path := stageCMakeListsForTest(t, `add_library(foo STATIC src/a.c)
target_link_libraries(foo PUBLIC $<BUILD_INTERFACE:zlib>)
`)
	r := &fileapi.Reply{
		Targets: map[string]fileapi.Target{
			"foo::@": {
				Name: "foo",
				Dependencies: []fileapi.TargetDependency{
					{Id: "zlib::@", Backtrace: 1},
				},
				BacktraceGraph: fileapi.BacktraceGraph{
					Commands: []string{"target_link_libraries"},
					Files:    []string{path},
					Nodes: []fileapi.BacktraceNode{
						{},
						{File: 0, Line: 2, Command: 0},
					},
				},
			},
		},
	}
	scope := backtraceRecoverLinkScope(r)
	if scope["foo"]["zlib"] != "PUBLIC" {
		t.Errorf("genex-wrapped dep should recover PUBLIC; got %q",
			scope["foo"]["zlib"])
	}
}

func TestBacktraceRecoverLinkScope_NilOrEmptyReply(t *testing.T) {
	if got := backtraceRecoverLinkScope(nil); got != nil {
		t.Errorf("nil reply should return nil; got %v", got)
	}
	if got := backtraceRecoverLinkScope(&fileapi.Reply{}); got != nil {
		t.Errorf("empty reply should return nil; got %v", got)
	}
}

func TestBacktraceRecoverLinkScope_SkipsGeneratorProvided(t *testing.T) {
	r := &fileapi.Reply{
		Targets: map[string]fileapi.Target{
			"ZERO_CHECK::@": {
				Name:                "ZERO_CHECK",
				IsGeneratorProvided: true,
				Dependencies:        []fileapi.TargetDependency{{Id: "some::@", Backtrace: 1}},
			},
		},
	}
	if got := backtraceRecoverLinkScope(r); got != nil {
		t.Errorf("ZERO_CHECK should be skipped; got %v", got)
	}
}

func TestDepLibNameFromId(t *testing.T) {
	cases := []struct{ in, want string }{
		{"foo::@", "foo"},
		{"foo::@abc123", "foo"},
		{"zlib", "zlib"},
		{"", ""},
	}
	for _, c := range cases {
		if got := depLibNameFromId(c.in); got != c.want {
			t.Errorf("depLibNameFromId(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestStripGenexWrapper(t *testing.T) {
	cases := []struct{ in, want string }{
		{"$<BUILD_INTERFACE:zlib>", "zlib"},
		{"$<INSTALL_INTERFACE:foo>", "foo"},
		{"plain", "plain"},
		{"$<UPPER_CASE:foo>", "$<UPPER_CASE:foo>"},
	}
	for _, c := range cases {
		if got := stripGenexWrapper(c.in); got != c.want {
			t.Errorf("stripGenexWrapper(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestIsCMakeInternalPath(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		{"/usr/share/cmake-3.28/Modules/CMakeFindDependencyMacro.cmake", true},
		{"/opt/cmake/Modules/x.cmake", true},
		{"/home/user/Cellar/cmake/3.28/Modules/y.cmake", true},
		{"/proj/CMakeLists.txt", false},
		{"/proj/cmake/CustomHelpers.cmake", false},
	}
	for _, c := range cases {
		if got := isCMakeInternalPath(c.path); got != c.want {
			t.Errorf("isCMakeInternalPath(%q) = %v, want %v", c.path, got, c.want)
		}
	}
}

func intPtr(n int) *int { return &n }
