package lower

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/internal/ninja"
)

// TestRecoverGenrule_CmakeScriptLift covers the opt-in
// cmake -P → genrule lift. When CMakeScriptRunner is set on the
// codegenContext, a `cmake -P <script>` custom command lowers
// to a genrule that invokes the runner at Bazel build time,
// preserving any -D args.
func TestRecoverGenrule_CmakeScriptLift(t *testing.T) {
	const (
		buildDir = "/tmp/build"
		cmakeSrc = "/src/project"
	)
	g := &ninja.Graph{
		Vars:  map[string]string{},
		Rules: map[string]*ninja.Rule{},
		Pools: map[string]*ninja.Pool{},
	}
	g.Rules["CUSTOM_COMMAND"] = &ninja.Rule{
		Name: "CUSTOM_COMMAND",
		Bindings: map[string]string{
			"command": "$COMMAND",
		},
		BindingOrder: []string{"command"},
	}
	g.Builds = []*ninja.Build{{
		Outputs: []string{"gen/hash.h"},
		Rule:    "CUSTOM_COMMAND",
		Bindings: map[string]string{
			"COMMAND": "/usr/bin/cmake -DINPUT=/src/project/data.txt -DOUTPUT=/tmp/build/gen/hash.h -P /src/project/scripts/hash.cmake",
		},
		BindingOrder: []string{"COMMAND"},
	}}

	cc := newCodegenContext()
	cc.CMakeScriptRunner = "//tools:cmake-script-runner"

	relOut, name, err := cc.recoverGenrule(buildDir+"/gen/hash.h", cmakeSrc, buildDir, g)
	if err != nil {
		t.Fatalf("lift returned error: %v", err)
	}
	if relOut != "gen/hash.h" {
		t.Errorf("relOut = %q, want %q", relOut, "gen/hash.h")
	}
	if name == "" {
		t.Fatal("genrule name is empty")
	}
	if len(cc.Genrules) != 1 {
		t.Fatalf("Genrules: got %d, want 1", len(cc.Genrules))
	}
	gen := cc.Genrules[0]
	if !strings.Contains(gen.GenruleCmd, "$(execpath //tools:cmake-script-runner)") {
		t.Errorf("cmd missing runner execpath; got %q", gen.GenruleCmd)
	}
	if !strings.Contains(gen.GenruleCmd, "-P $(execpath scripts/hash.cmake)") {
		t.Errorf("cmd missing -P script execpath; got %q", gen.GenruleCmd)
	}
	// -D args preserved (input + output paths threaded through).
	if !strings.Contains(gen.GenruleCmd, "-DINPUT=/src/project/data.txt") {
		t.Errorf("cmd missing -DINPUT arg; got %q", gen.GenruleCmd)
	}
	if !strings.Contains(gen.GenruleCmd, "-DOUTPUT=/tmp/build/gen/hash.h") {
		t.Errorf("cmd missing -DOUTPUT arg; got %q", gen.GenruleCmd)
	}
	// Script is in srcs.
	foundScript := false
	for _, s := range gen.Srcs {
		if s == "scripts/hash.cmake" {
			foundScript = true
		}
	}
	if !foundScript {
		t.Errorf("script not in srcs; got %v", gen.Srcs)
	}
	// Runner is in tools.
	if len(gen.GenruleTools) != 1 || gen.GenruleTools[0] != "//tools:cmake-script-runner" {
		t.Errorf("GenruleTools = %v, want [//tools:cmake-script-runner]", gen.GenruleTools)
	}
	// Tag for audit/triage.
	foundTag := false
	for _, tag := range gen.Tags {
		if tag == "cmake-codegen-cmake-script-lift" {
			foundTag = true
		}
	}
	if !foundTag {
		t.Errorf("missing cmake-codegen-cmake-script-lift tag; got %v", gen.Tags)
	}
}

// TestRecoverGenrule_CmakeScriptLift_DisabledFallsBackToRefusal
// pins the off-by-default contract: with CMakeScriptRunner
// empty, the recoverGenrule path falls through to the existing
// UnsupportedCustomCommandScript refusal — unchanged from
// pre-lift behaviour.
func TestRecoverGenrule_CmakeScriptLift_DisabledFallsBackToRefusal(t *testing.T) {
	const (
		buildDir = "/tmp/build"
		cmakeSrc = "/src/project"
	)
	g := &ninja.Graph{
		Vars:  map[string]string{},
		Rules: map[string]*ninja.Rule{},
		Pools: map[string]*ninja.Pool{},
	}
	g.Rules["CUSTOM_COMMAND"] = &ninja.Rule{
		Name: "CUSTOM_COMMAND",
		Bindings: map[string]string{
			"command": "$COMMAND",
		},
		BindingOrder: []string{"command"},
	}
	g.Builds = []*ninja.Build{{
		Outputs: []string{"gen/hash.h"},
		Rule:    "CUSTOM_COMMAND",
		Bindings: map[string]string{
			"COMMAND": "/usr/bin/cmake -P /src/project/scripts/hash.cmake",
		},
		BindingOrder: []string{"COMMAND"},
	}}

	cc := newCodegenContext()
	// CMakeScriptRunner left empty.

	_, _, err := cc.recoverGenrule(buildDir+"/gen/hash.h", cmakeSrc, buildDir, g)
	if err == nil {
		t.Fatal("expected refusal with empty CMakeScriptRunner; got nil")
	}
	if !strings.Contains(err.Error(), "cmake -P") {
		t.Errorf("error doesn't mention cmake -P; got %v", err)
	}
}

// TestRecoverGenrule_CmakeScriptLift_RefusesScriptOutsideSource
// pins the conservative half: if the cmake -P script's path
// isn't under the source root (e.g. it lives under the build dir
// as a configure_file output), the lift declines and the
// existing refusal stands. The hardcoded-paths-in-script case
// would fail at Bazel build time anyway; better to refuse at
// convert time with a clear diagnostic.
func TestRecoverGenrule_CmakeScriptLift_RefusesScriptOutsideSource(t *testing.T) {
	const (
		buildDir = "/tmp/build"
		cmakeSrc = "/src/project"
	)
	g := &ninja.Graph{
		Vars:  map[string]string{},
		Rules: map[string]*ninja.Rule{},
		Pools: map[string]*ninja.Pool{},
	}
	g.Rules["CUSTOM_COMMAND"] = &ninja.Rule{
		Name: "CUSTOM_COMMAND",
		Bindings: map[string]string{
			"command": "$COMMAND",
		},
		BindingOrder: []string{"command"},
	}
	g.Builds = []*ninja.Build{{
		Outputs: []string{"gen/out.h"},
		Rule:    "CUSTOM_COMMAND",
		Bindings: map[string]string{
			// Script lives under buildDir (the configure_file
			// shape — libpng's gensrc.cmake.in -> gensrc.cmake).
			"COMMAND": "/usr/bin/cmake -P /tmp/build/scripts/configured.cmake",
		},
		BindingOrder: []string{"COMMAND"},
	}}
	cc := newCodegenContext()
	cc.CMakeScriptRunner = "//tools:cmake-script-runner"

	_, _, err := cc.recoverGenrule(buildDir+"/gen/out.h", cmakeSrc, buildDir, g)
	if err == nil {
		t.Fatal("expected refusal when script isn't under source root; got nil")
	}
	if len(cc.Genrules) != 0 {
		t.Errorf("script-outside-source lift attempt synthesized %d Genrules; want 0",
			len(cc.Genrules))
	}
}

// TestExtractCmakePDashArgs covers the -D arg extraction edge
// cases that the lift relies on for VTK-shape parameter-driven
// scripts.
func TestExtractCmakePDashArgs(t *testing.T) {
	cases := []struct {
		name string
		cmd  string
		want []string
	}{
		{
			name: "space-separated -D",
			cmd:  "cmake -D INPUT=foo -D OUTPUT=bar -P script.cmake",
			want: []string{"-D", "INPUT=foo", "-D", "OUTPUT=bar"},
		},
		{
			name: "glued -D",
			cmd:  "cmake -DFOO=a -DBAR=b -P script.cmake",
			want: []string{"-DFOO=a", "-DBAR=b"},
		},
		{
			name: "leading cd-and prefix stripped",
			cmd:  "cd /build && cmake -DFOO=a -P script.cmake",
			want: []string{"-DFOO=a"},
		},
		{
			name: "no -D args",
			cmd:  "cmake -P script.cmake",
			want: nil,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := extractCmakePDashArgs(c.cmd)
			if len(got) != len(c.want) {
				t.Fatalf("got %v (len %d), want %v (len %d)", got, len(got), c.want, len(c.want))
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Errorf("got[%d] = %q, want %q", i, got[i], c.want[i])
				}
			}
		})
	}
}

// TestExtractCmakePScriptPositionalArgs covers the libpng
// `cmake -P gensrc.cmake <output-name>` dispatch shape: the
// switch arg is positional, lives after the script path, and
// must round-trip through to the bake invocation so the
// script's ${CMAKE_ARGV3} dispatch sees the right value.
func TestExtractCmakePScriptPositionalArgs(t *testing.T) {
	cases := []struct {
		name string
		cmd  string
		want []string
	}{
		{
			name: "single positional after script",
			cmd:  "cmake -P script.cmake pnglibconf.h",
			want: []string{"pnglibconf.h"},
		},
		{
			name: "multiple positionals",
			cmd:  "cmake -P script.cmake out1 out2",
			want: []string{"out1", "out2"},
		},
		{
			name: "no positionals",
			cmd:  "cmake -P script.cmake",
			want: nil,
		},
		{
			name: "positional mixed with -D, only positional returned",
			cmd:  "cmake -DFOO=1 -P script.cmake -DBAR=2 pnglibconf.h",
			want: []string{"pnglibconf.h"},
		},
		{
			name: "leading cd-and prefix stripped",
			cmd:  "cd /build && cmake -P script.cmake out1",
			want: []string{"out1"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := extractCmakePScriptPositionalArgs(c.cmd)
			if len(got) != len(c.want) {
				t.Fatalf("got %v (len %d), want %v (len %d)", got, len(got), c.want, len(c.want))
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Errorf("got[%d] = %q, want %q", i, got[i], c.want[i])
				}
			}
		})
	}
}

// TestRecoverGenrule_CmakeScriptLift_RequestedOutput pins the
// requested-output contract on the RUNNER-LIFT arm (the bake arm has
// its own pin in cmake_script_bake_test.go): a multi-output edge is
// consumed once per output, and each consumer must get back the
// output it asked for — not the edge's first out.
func TestRecoverGenrule_CmakeScriptLift_RequestedOutput(t *testing.T) {
	const (
		buildDir = "/tmp/build"
		cmakeSrc = "/src/project"
	)
	g := &ninja.Graph{
		Vars:  map[string]string{},
		Rules: map[string]*ninja.Rule{},
		Pools: map[string]*ninja.Pool{},
	}
	g.Rules["CUSTOM_COMMAND"] = &ninja.Rule{
		Name:         "CUSTOM_COMMAND",
		Bindings:     map[string]string{"command": "$COMMAND"},
		BindingOrder: []string{"command"},
	}
	g.Builds = []*ninja.Build{{
		Outputs: []string{"gen/shader.h", "gen/shader.cxx"},
		Rule:    "CUSTOM_COMMAND",
		Bindings: map[string]string{
			"COMMAND": "/usr/bin/cmake -P /src/project/scripts/encode.cmake",
		},
		BindingOrder: []string{"COMMAND"},
	}}

	cc := newCodegenContext()
	cc.CMakeScriptRunner = "//tools:cmake-script-runner"

	// First consumer asks for the SECOND output — must get it back.
	relOut, name, err := cc.recoverGenrule(buildDir+"/gen/shader.cxx", cmakeSrc, buildDir, g)
	if err != nil {
		t.Fatalf("lift returned error: %v", err)
	}
	if relOut != "gen/shader.cxx" {
		t.Errorf("requested gen/shader.cxx, got %q", relOut)
	}
	// Second consumer (SeenBuilds reuse path) asks for the first.
	relOut2, name2, err := cc.recoverGenrule(buildDir+"/gen/shader.h", cmakeSrc, buildDir, g)
	if err != nil {
		t.Fatalf("lift (reuse) returned error: %v", err)
	}
	if relOut2 != "gen/shader.h" {
		t.Errorf("requested gen/shader.h, got %q", relOut2)
	}
	if name != name2 {
		t.Errorf("consumers of one edge got different genrules: %q vs %q", name, name2)
	}
	// Both outputs declared on the single emitted genrule.
	if len(cc.Genrules) != 1 {
		t.Fatalf("Genrules: got %d, want 1", len(cc.Genrules))
	}
	if got := cc.Genrules[0].GenruleOuts; len(got) != 2 {
		t.Errorf("GenruleOuts = %v, want both outputs", got)
	}
}

// TestDiscoverCmakeScriptOutputs: a `cmake -P` script's outputs are recovered
// from its configure_file / file(WRITE|GENERATE) / execute_process(OUTPUT_FILE)
// statements, with ${VAR} resolved from the command's -D args (the VTK
// -DSCRIPT_OUT=<path> shape) and the standard CMAKE_*_DIR locations. This is the
// set fed as discovered_outputs when the ninja edge declared no outputs.
func TestDiscoverCmakeScriptOutputs(t *testing.T) {
	buildDir := t.TempDir()
	srcDir := t.TempDir()
	script := filepath.Join(srcDir, "gen.cmake")
	body := `# generated recipe
configure_file("${CMAKE_CURRENT_SOURCE_DIR}/in.h.in" "${CMAKE_CURRENT_BINARY_DIR}/configured.h" @ONLY)
file(WRITE "${SCRIPT_OUT}" "contents")
file(GENERATE OUTPUT ${CMAKE_BINARY_DIR}/generated.cpp CONTENT "x")
execute_process(COMMAND foo OUTPUT_FILE ${CMAKE_BINARY_DIR}/captured.txt)
file(WRITE ${UNRESOLVED_VAR}/skip.h "y")
`
	if err := os.WriteFile(script, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	dArgs := []string{"-D", "SCRIPT_OUT=" + filepath.Join(buildDir, "out", "hashed.h"), "-DUNUSED:STRING=z"}

	got := discoverCmakeScriptOutputs(script, dArgs, buildDir, srcDir)
	want := []string{"configured.h", "out/hashed.h", "generated.cpp", "captured.txt"}
	// Order follows the collect order (configure_file, file WRITE, file GENERATE,
	// execute_process); compare as sets to stay robust to that.
	if !sameStringSet(got, want) {
		t.Errorf("discoverCmakeScriptOutputs = %v, want set %v", got, want)
	}
	// The ${UNRESOLVED_VAR} write must be dropped (no -D for it).
	for _, g := range got {
		if strings.Contains(g, "skip.h") {
			t.Errorf("unresolved ${VAR} output leaked: %v", got)
		}
	}
}

func sameStringSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	m := map[string]int{}
	for _, x := range a {
		m[x]++
	}
	for _, x := range b {
		m[x]--
	}
	for _, v := range m {
		if v != 0 {
			return false
		}
	}
	return true
}
