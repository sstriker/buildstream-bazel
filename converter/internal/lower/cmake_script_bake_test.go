package lower

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/internal/ninja"
)

// TestBakeCmakeScriptGenrule_RunsCmakeAndEmbedsOutput pins the
// convert-time bake contract: cmake -P runs, declared outputs
// get captured, genrule cmds materialize the bytes via
// base64-decode. We synthesize a minimal cmake script that
// writes its declared output file to demonstrate the full
// round-trip.
func TestBakeCmakeScriptGenrule_RunsCmakeAndEmbedsOutput(t *testing.T) {
	cmakeBin, err := execLookPath("cmake")
	if err != nil {
		t.Skip("cmake not on PATH; bake test requires convert-host cmake")
	}

	// Workspace: source root holds the script; build dir is
	// where cmake would have placed the declared output.
	src := t.TempDir()
	build := t.TempDir()
	scriptPath := filepath.Join(src, "gen.cmake")
	// The script writes a static byte sequence to a fixed
	// path under the *test workdir* (cmake -P runs with its
	// CWD as the workdir). Our bake path runs cmake in a
	// fresh tmpdir and reads <tmpdir>/<out>.
	if err := os.WriteFile(scriptPath, []byte(`file(WRITE "out.txt" "hello\n")`), 0o644); err != nil {
		t.Fatal(err)
	}

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
	b := &ninja.Build{
		Outputs: []string{"out.txt"},
		Rule:    "CUSTOM_COMMAND",
		Bindings: map[string]string{
			"COMMAND": cmakeBin + " -P " + scriptPath,
		},
		BindingOrder: []string{"COMMAND"},
	}
	g.Builds = append(g.Builds, b)

	cc := newCodegenContext()
	cc.CMakeBinary = cmakeBin

	cmd := cmakeBin + " -P " + scriptPath
	rel, name, reason, ok := bakeCmakeScriptGenrule(cc, b, cmd, scriptPath, build)
	if !ok {
		t.Fatalf("bake failed: reason=%q", reason)
	}
	if rel != "out.txt" {
		t.Errorf("rel = %q, want out.txt", rel)
	}
	if name == "" {
		t.Fatal("name empty")
	}
	if len(cc.Genrules) != 1 {
		t.Fatalf("Genrules len = %d, want 1", len(cc.Genrules))
	}
	gen := cc.Genrules[0]
	// The bake tag funnels into warnConvertTimeBaking.
	foundBakeTag := false
	for _, tag := range gen.Tags {
		if tag == "cmake-codegen-cmake-script-bake" {
			foundBakeTag = true
		}
	}
	if !foundBakeTag {
		t.Errorf("missing cmake-codegen-cmake-script-bake tag; got %v", gen.Tags)
	}
	// The genrule cmd should base64-decode the literal "hello\n".
	wantBase64 := base64.StdEncoding.EncodeToString([]byte("hello\n"))
	if !strings.Contains(gen.GenruleCmd, wantBase64) {
		t.Errorf("cmd doesn't carry base64-encoded payload; got %q want substring %q",
			gen.GenruleCmd, wantBase64)
	}
}

// TestBakeCmakeScriptGenrule_ForwardsPositionalArgs covers the
// libpng `cmake -P gensrc.cmake <output-name>` dispatch shape:
// the script reads ${CMAKE_ARGV3} as a switch and writes one of
// several declared outputs per invocation. Bake must forward
// positional args so the script's dispatch logic sees the right
// value.
func TestBakeCmakeScriptGenrule_ForwardsPositionalArgs(t *testing.T) {
	cmakeBin, err := execLookPath("cmake")
	if err != nil {
		t.Skip("cmake not on PATH; bake test requires convert-host cmake")
	}

	src := t.TempDir()
	build := t.TempDir()
	scriptPath := filepath.Join(src, "dispatch.cmake")
	// Script: dispatch on the first positional arg
	// (${CMAKE_ARGV3} — argv[0..2] are cmake/-P/script-path).
	// Writes a different byte sequence depending on the arg.
	script := `
if(NOT DEFINED CMAKE_ARGV3)
  message(FATAL_ERROR "missing positional arg")
endif()
if("${CMAKE_ARGV3}" STREQUAL "a.txt")
  file(WRITE "a.txt" "alpha\n")
elseif("${CMAKE_ARGV3}" STREQUAL "b.txt")
  file(WRITE "b.txt" "beta\n")
else()
  message(FATAL_ERROR "unknown arg: ${CMAKE_ARGV3}")
endif()
`
	if err := os.WriteFile(scriptPath, []byte(script), 0o644); err != nil {
		t.Fatal(err)
	}

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
	b := &ninja.Build{
		Outputs: []string{"a.txt"},
		Rule:    "CUSTOM_COMMAND",
		Bindings: map[string]string{
			"COMMAND": cmakeBin + " -P " + scriptPath + " a.txt",
		},
		BindingOrder: []string{"COMMAND"},
	}
	g.Builds = append(g.Builds, b)

	cc := newCodegenContext()
	cc.CMakeBinary = cmakeBin
	cmd := cmakeBin + " -P " + scriptPath + " a.txt"
	rel, _, reason, ok := bakeCmakeScriptGenrule(cc, b, cmd, scriptPath, build)
	if !ok {
		t.Fatalf("bake failed (positional arg not forwarded?): reason=%q", reason)
	}
	if rel != "a.txt" {
		t.Errorf("rel = %q, want a.txt", rel)
	}
	if len(cc.Genrules) != 1 {
		t.Fatalf("Genrules len = %d, want 1", len(cc.Genrules))
	}
	wantBase64 := base64.StdEncoding.EncodeToString([]byte("alpha\n"))
	if !strings.Contains(cc.Genrules[0].GenruleCmd, wantBase64) {
		t.Errorf("cmd doesn't carry expected payload; got %q want substring %q",
			cc.Genrules[0].GenruleCmd, wantBase64)
	}
}

// TestBakeCmakeScriptGenrule_ForwardsDashDArgsBeforeScript covers
// libpng's gensrc.cmake-style dispatch where the build invokes
// `cmake -DOUTPUT=name -P script.cmake` and the script reads
// ${OUTPUT} as a switch (`if(OUTPUT STREQUAL "pnglibconf.h") ...`).
// cmake requires -D vars BEFORE the -P arg or it treats them as
// positional ${CMAKE_ARGV*} entries instead of setting the
// variable. Verify bake preserves that ordering so dispatch
// scripts pick the right branch.
func TestBakeCmakeScriptGenrule_ForwardsDashDArgsBeforeScript(t *testing.T) {
	cmakeBin, err := execLookPath("cmake")
	if err != nil {
		t.Skip("cmake not on PATH; bake test requires convert-host cmake")
	}

	src := t.TempDir()
	build := t.TempDir()
	scriptPath := filepath.Join(src, "dispatch_d.cmake")
	// Script reads ${OUTPUT} (set via -DOUTPUT=...) and writes
	// different bytes per arm.
	script := `
if(NOT DEFINED OUTPUT)
  message(FATAL_ERROR "OUTPUT not set — -D arg landed after -P script?")
endif()
if(OUTPUT STREQUAL "a.txt")
  file(WRITE "a.txt" "alpha\n")
elseif(OUTPUT STREQUAL "b.txt")
  file(WRITE "b.txt" "beta\n")
else()
  message(FATAL_ERROR "unknown OUTPUT: ${OUTPUT}")
endif()
`
	if err := os.WriteFile(scriptPath, []byte(script), 0o644); err != nil {
		t.Fatal(err)
	}

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
	b := &ninja.Build{
		Outputs: []string{"a.txt"},
		Rule:    "CUSTOM_COMMAND",
		Bindings: map[string]string{
			"COMMAND": cmakeBin + " -DOUTPUT=a.txt -P " + scriptPath,
		},
		BindingOrder: []string{"COMMAND"},
	}
	g.Builds = append(g.Builds, b)

	cc := newCodegenContext()
	cc.CMakeBinary = cmakeBin
	cmd := cmakeBin + " -DOUTPUT=a.txt -P " + scriptPath
	rel, _, reason, ok := bakeCmakeScriptGenrule(cc, b, cmd, scriptPath, build)
	if !ok {
		t.Fatalf("bake failed (-D arg ordering bug?): reason=%q", reason)
	}
	if rel != "a.txt" {
		t.Errorf("rel = %q, want a.txt", rel)
	}
	if len(cc.Genrules) != 1 {
		t.Fatalf("Genrules len = %d, want 1", len(cc.Genrules))
	}
	wantBase64 := base64.StdEncoding.EncodeToString([]byte("alpha\n"))
	if !strings.Contains(cc.Genrules[0].GenruleCmd, wantBase64) {
		t.Errorf("cmd doesn't carry expected payload; got %q want substring %q",
			cc.Genrules[0].GenruleCmd, wantBase64)
	}
}

func TestBakeCmakeScriptGenrule_NoCmakeRefuses(t *testing.T) {
	cc := newCodegenContext()
	cc.CMakeBinary = "" // not available
	b := &ninja.Build{Outputs: []string{"foo"}}
	_, _, reason, ok := bakeCmakeScriptGenrule(cc, b, "cmake -P /x/y.cmake", "/x/y.cmake", "/build")
	if ok {
		t.Fatal("expected refusal when CMakeBinary empty; got ok")
	}
	if !strings.Contains(reason, "cmake binary not on PATH") {
		t.Errorf("reason missing expected substring; got %q", reason)
	}
}

func TestSanitizeForName(t *testing.T) {
	cases := []struct{ in, want string }{
		{"foo.h", "foo_h"},
		{"sub-dir/bar.c", "sub_dir_bar_c"},
		{"already_clean", "already_clean"},
		{"with spaces", "with_spaces"},
	}
	for _, c := range cases {
		if got := sanitizeForName(c.in); got != c.want {
			t.Errorf("sanitizeForName(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
