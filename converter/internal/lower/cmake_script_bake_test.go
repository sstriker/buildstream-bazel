package lower

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/internal/ninja"
	"github.com/sstriker/buildstream-bazel/converter/ir"
)

// assertBakedBody checks a baked target reproduces wantBody, whether it
// lowered to a readable skylib write_file (\n-only text) or the
// byte-exact base64 genrule fallback (binary / control-byte / CRLF).
func assertBakedBody(t *testing.T, tgt ir.Target, wantBody string) {
	t.Helper()
	switch tgt.Kind {
	case ir.KindWriteFile:
		if got := strings.Join(tgt.WriteFileContent, "\n"); got != wantBody {
			t.Errorf("write_file content round-trip = %q, want %q", got, wantBody)
		}
	case ir.KindGenrule:
		want := base64.StdEncoding.EncodeToString([]byte(wantBody))
		if !strings.Contains(tgt.GenruleCmd, want) {
			t.Errorf("genrule cmd doesn't carry base64 payload; got %q want substr %q", tgt.GenruleCmd, want)
		}
	default:
		t.Errorf("unexpected bake kind %v", tgt.Kind)
	}
}

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
	name, reason, ok := bakeCmakeScriptGenrule(cc, b, cmd, scriptPath, build, g)
	if !ok {
		t.Fatalf("bake failed: reason=%q", reason)
	}
	if name == "" {
		t.Fatal("name empty")
	}
	if cc.OutToGenrule["out.txt"] == "" {
		t.Error("out.txt not registered in OutToGenrule")
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
	assertBakedBody(t, gen, "hello\n")
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
	_, reason, ok := bakeCmakeScriptGenrule(cc, b, cmd, scriptPath, build, g)
	if !ok {
		t.Fatalf("bake failed (positional arg not forwarded?): reason=%q", reason)
	}
	if cc.OutToGenrule["a.txt"] == "" {
		t.Error("a.txt not registered in OutToGenrule")
	}
	if len(cc.Genrules) != 1 {
		t.Fatalf("Genrules len = %d, want 1", len(cc.Genrules))
	}
	assertBakedBody(t, cc.Genrules[0], "alpha\n")
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
	_, reason, ok := bakeCmakeScriptGenrule(cc, b, cmd, scriptPath, build, g)
	if !ok {
		t.Fatalf("bake failed (-D arg ordering bug?): reason=%q", reason)
	}
	if cc.OutToGenrule["a.txt"] == "" {
		t.Error("a.txt not registered in OutToGenrule")
	}
	if len(cc.Genrules) != 1 {
		t.Fatalf("Genrules len = %d, want 1", len(cc.Genrules))
	}
	assertBakedBody(t, cc.Genrules[0], "alpha\n")
}

// TestBakeCmakeScriptGenrule_TopologicalChain pins the producer-
// chain bake: when build B reads input X produced by build A
// (both cmake -P), baking B triggers A first so the
// configure-substituted ${BINDIR}-absolute read in B succeeds.
// Surfaced by libpng's gensrc.cmake → genout.cmake → pnglibconf.h
// chain where each step reads ${BINDIR}/<output-of-prior-step>.
func TestBakeCmakeScriptGenrule_TopologicalChain(t *testing.T) {
	cmakeBin, err := execLookPath("cmake")
	if err != nil {
		t.Skip("cmake not on PATH; bake test requires convert-host cmake")
	}

	src := t.TempDir()
	build := t.TempDir()
	// Producer script: writes "step1\n" to step1.txt in $CWD.
	producerPath := filepath.Join(src, "producer.cmake")
	if err := os.WriteFile(producerPath, []byte(`file(WRITE "step1.txt" "step1\n")`), 0o644); err != nil {
		t.Fatal(err)
	}
	// Consumer script: reads step1.txt (from $CWD == buildDir
	// post-producer-bake) and writes step2.txt with the contents
	// + "step2".
	consumerPath := filepath.Join(src, "consumer.cmake")
	consumerSrc := `
file(READ "step1.txt" STEP1)
file(WRITE "step2.txt" "${STEP1}step2\n")
`
	if err := os.WriteFile(consumerPath, []byte(consumerSrc), 0o644); err != nil {
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
	producerBuild := &ninja.Build{
		Outputs:      []string{"step1.txt"},
		Rule:         "CUSTOM_COMMAND",
		Bindings:     map[string]string{"COMMAND": cmakeBin + " -P " + producerPath},
		BindingOrder: []string{"COMMAND"},
	}
	consumerBuild := &ninja.Build{
		Outputs:      []string{"step2.txt"},
		Inputs:       []string{"step1.txt"}, // chain input
		Rule:         "CUSTOM_COMMAND",
		Bindings:     map[string]string{"COMMAND": cmakeBin + " -P " + consumerPath},
		BindingOrder: []string{"COMMAND"},
	}
	g.Builds = append(g.Builds, producerBuild, consumerBuild)

	cc := newCodegenContext()
	cc.CMakeBinary = cmakeBin
	// Bake the consumer; bakeProducerChain should pre-bake the
	// producer so step1.txt exists in buildDir when consumer runs.
	cmd := cmakeBin + " -P " + consumerPath
	_, reason, ok := bakeCmakeScriptGenrule(cc, consumerBuild, cmd, consumerPath, build, g)
	if !ok {
		t.Fatalf("chain bake failed: reason=%q", reason)
	}
	if cc.OutToGenrule["step2.txt"] == "" {
		t.Error("step2.txt not registered in OutToGenrule")
	}
	// Two genrules emitted: producer + consumer (chain).
	if len(cc.Genrules) != 2 {
		t.Errorf("Genrules len = %d, want 2 (producer + consumer)", len(cc.Genrules))
	}
	// Consumer's cmd should carry the chained payload "step1\nstep2\n".
	assertBakedBody(t, cc.Genrules[len(cc.Genrules)-1], "step1\nstep2\n")
}

func TestBakeCmakeScriptGenrule_NoCmakeRefuses(t *testing.T) {
	cc := newCodegenContext()
	cc.CMakeBinary = "" // not available
	b := &ninja.Build{Outputs: []string{"foo"}}
	_, reason, ok := bakeCmakeScriptGenrule(cc, b, "cmake -P /x/y.cmake", "/x/y.cmake", "/build", nil)
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

// TestRecoverCmakeScriptGenrule_RequestedOutput pins the multi-output
// contract: a script writing a .h + the symbol-defining .cxx (the
// vtkEncodeString shape) is consumed once per output, and the
// recovery must return the REQUESTED output, not the bake's primary
// out — otherwise the .cxx consumer is handed the .h,
// attachGeneratedSource routes it to hdrs by extension, and the
// definition compiles nowhere (vtkProbeOpenGLVersion's undefined
// shader-string symbols).
func TestRecoverCmakeScriptGenrule_RequestedOutput(t *testing.T) {
	cmakeBin, err := execLookPath("cmake")
	if err != nil {
		t.Skip("cmake not on PATH; bake test requires convert-host cmake")
	}
	src := t.TempDir()
	build := t.TempDir()
	scriptPath := filepath.Join(src, "encode.cmake")
	if err := os.WriteFile(scriptPath, []byte(
		"file(WRITE \"shader.h\" \"extern const char *s;\\n\")\n"+
			"file(WRITE \"shader.cxx\" \"const char *s = \\\"x\\\";\\n\")\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	g := &ninja.Graph{Vars: map[string]string{}, Rules: map[string]*ninja.Rule{}, Pools: map[string]*ninja.Pool{}}
	g.Rules["CUSTOM_COMMAND"] = &ninja.Rule{
		Name:         "CUSTOM_COMMAND",
		Bindings:     map[string]string{"command": "$COMMAND"},
		BindingOrder: []string{"command"},
	}
	b := &ninja.Build{
		Outputs: []string{"shader.h", "shader.cxx"},
		Rule:    "CUSTOM_COMMAND",
		Bindings: map[string]string{
			"COMMAND": cmakeBin + " -P " + scriptPath,
		},
		BindingOrder: []string{"COMMAND"},
	}
	g.Builds = append(g.Builds, b)
	cc := newCodegenContext()
	cc.CMakeBinary = cmakeBin
	cc.CMakeScriptBake = true

	// First consumer asks for the .cxx — must get the .cxx back even
	// though the bake's primary out is the .h.
	rel, _, err := cc.recoverGenrule(filepath.Join(build, "shader.cxx"), src, build, g)
	if err != nil {
		t.Fatalf("recoverGenrule(.cxx): %v", err)
	}
	if rel != "shader.cxx" {
		t.Errorf("requested .cxx, got %q", rel)
	}
	// Second consumer (SeenBuilds reuse path) asks for the .h.
	rel2, _, err := cc.recoverGenrule(filepath.Join(build, "shader.h"), src, build, g)
	if err != nil {
		t.Fatalf("recoverGenrule(.h): %v", err)
	}
	if rel2 != "shader.h" {
		t.Errorf("requested .h, got %q", rel2)
	}
}

// TestRecoverCmakeScriptGenrule_ImplicitOutConsumer pins the
// BYPRODUCTS shape: a consumer can reference a file the ninja edge
// declares as an IMPLICIT out (`build out | byproduct :`), because
// BuildFor indexes implicit outs too. The bake must materialize and
// register that file like any explicit out — otherwise the consumer
// is handed a path no target produces (a dangling reference the
// build only catches as a missing-input error much later).
func TestRecoverCmakeScriptGenrule_ImplicitOutConsumer(t *testing.T) {
	cmakeBin, err := execLookPath("cmake")
	if err != nil {
		t.Skip("cmake not on PATH; bake test requires convert-host cmake")
	}
	src := t.TempDir()
	build := t.TempDir()
	scriptPath := filepath.Join(src, "gen.cmake")
	if err := os.WriteFile(scriptPath, []byte(
		"file(WRITE \"main.txt\" \"main\\n\")\n"+
			"file(WRITE \"side.txt\" \"side\\n\")\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	g := &ninja.Graph{Vars: map[string]string{}, Rules: map[string]*ninja.Rule{}, Pools: map[string]*ninja.Pool{}}
	g.Rules["CUSTOM_COMMAND"] = &ninja.Rule{
		Name:         "CUSTOM_COMMAND",
		Bindings:     map[string]string{"command": "$COMMAND"},
		BindingOrder: []string{"command"},
	}
	b := &ninja.Build{
		Outputs:      []string{"main.txt"},
		ImplicitOuts: []string{"side.txt", "${cmake_ninja_workdir}main.txt"},
		Rule:         "CUSTOM_COMMAND",
		Bindings: map[string]string{
			"COMMAND": cmakeBin + " -P " + scriptPath,
		},
		BindingOrder: []string{"COMMAND"},
	}
	g.Builds = append(g.Builds, b)
	cc := newCodegenContext()
	cc.CMakeBinary = cmakeBin
	cc.CMakeScriptBake = true

	rel, _, err := cc.recoverGenrule(filepath.Join(build, "side.txt"), src, build, g)
	if err != nil {
		t.Fatalf("recoverGenrule(implicit out): %v", err)
	}
	if rel != "side.txt" {
		t.Errorf("requested side.txt, got %q", rel)
	}
	if cc.OutToGenrule["side.txt"] == "" {
		t.Error("implicit out side.txt not registered in OutToGenrule")
	}
	// The ${cmake_ninja_workdir} shadow must NOT surface as an out.
	for o := range cc.OutToGenrule {
		if strings.Contains(o, "${") {
			t.Errorf("ninja-var shadow leaked into OutToGenrule: %q", o)
		}
	}
}
