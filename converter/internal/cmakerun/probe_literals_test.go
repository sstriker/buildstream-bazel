package cmakerun

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLiteralProbeRequest_Hash(t *testing.T) {
	a := LiteralProbeRequest{Literal: "$<TARGET_FILE:foo>"}
	b := LiteralProbeRequest{Literal: "$<TARGET_FILE:foo>"}
	c := LiteralProbeRequest{Literal: "$<TARGET_FILE:bar>"}
	d := LiteralProbeRequest{Literal: "$<TARGET_FILE:foo>", Target: "ctx"}

	if a.Hash() != b.Hash() {
		t.Fatalf("identical requests hashed differently: %s vs %s", a.Hash(), b.Hash())
	}
	if a.Hash() == c.Hash() {
		t.Fatalf("distinct literals shared a hash: %s", a.Hash())
	}
	if a.Hash() == d.Hash() {
		t.Fatalf("same literal with different Target shared a hash: %s", a.Hash())
	}
	if len(a.Hash()) != 16 {
		t.Fatalf("hash length = %d, want 16", len(a.Hash()))
	}
}

func TestRenderLiteralProbeHook_Empty(t *testing.T) {
	if got := RenderLiteralProbeHook(nil); got != nil {
		t.Fatalf("empty request set rendered non-nil: %q", got)
	}
}

func TestRenderLiteralProbeHook_Shape(t *testing.T) {
	reqs := []LiteralProbeRequest{
		{Literal: `$<TARGET_PROPERTY:foo,CUSTOM_PROP>`, Target: "foo"},
		{Literal: `say "hi" \ bye`},
	}
	body := string(RenderLiteralProbeHook(reqs))

	for _, want := range []string{
		`cmake_language(DEFER DIRECTORY "${CMAKE_SOURCE_DIR}" CALL _cmtb_litprobe)`,
		`function(_cmtb_litprobe)`,
		LiteralProbeDirname,
		`.$<CONFIG>.txt`,
		`CONTENT "$<TARGET_PROPERTY:foo,CUSTOM_PROP>"`,
		`TARGET foo`,
		`CONTENT "say \"hi\" \\ bye"`, // backslash + quote escaped
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("rendered hook missing %q\n---\n%s", want, body)
		}
	}

	// The no-Target request must not emit a TARGET clause for itself.
	// Crude but sufficient: only one TARGET line total (for foo).
	if n := strings.Count(body, "TARGET "); n != 1 {
		t.Fatalf("TARGET clause count = %d, want 1\n%s", n, body)
	}
}

func TestRenderLiteralProbeHook_Deterministic(t *testing.T) {
	reqs := []LiteralProbeRequest{
		{Literal: "z"}, {Literal: "a"}, {Literal: "m"},
	}
	first := RenderLiteralProbeHook(reqs)
	// Shuffle the input order; output must be byte-identical (sorted by hash).
	shuffled := []LiteralProbeRequest{reqs[2], reqs[0], reqs[1]}
	second := RenderLiteralProbeHook(shuffled)
	if string(first) != string(second) {
		t.Fatalf("render not order-independent:\n--- first ---\n%s\n--- second ---\n%s", first, second)
	}
}

func TestSplitLiteralProbeFilename(t *testing.T) {
	cases := []struct {
		fname        string
		hash, config string
		ok           bool
	}{
		{"abc123.Release.txt", "abc123", "Release", true},
		{"abc123..txt", "abc123", "", true}, // empty $<CONFIG>
		{"abc123.Release.1.txt", "abc123", "Release.1", true},
		{"abc123.txt", "", "", false},     // no config separator
		{".Release.txt", "", "", false},   // no hash
		{"abc123.Release", "", "", false}, // no .txt
	}
	for _, c := range cases {
		h, cfg, ok := splitLiteralProbeFilename(c.fname)
		if ok != c.ok || h != c.hash || cfg != c.config {
			t.Errorf("split(%q) = (%q,%q,%v), want (%q,%q,%v)",
				c.fname, h, cfg, ok, c.hash, c.config, c.ok)
		}
	}
}

func TestReadLiteralProbe_RoundTrip(t *testing.T) {
	buildDir := t.TempDir()
	dir := filepath.Join(buildDir, LiteralProbeDirname)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(name, content string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// h1: agrees across two configs → Unified. h2: diverges → select shape.
	write("h1.Release.txt", "/lib/libfoo.a")
	write("h1.Debug.txt", "/lib/libfoo.a")
	write("h2.Release.txt", "/build/Release/app")
	write("h2.Debug.txt", "/build/Debug/app")
	write("stray.dat", "ignored") // non-.txt skipped

	got, err := ReadLiteralProbe(buildDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d resolutions, want 2: %v", len(got), got)
	}
	if v, ok := got["h1"].Unified(); !ok || v != "/lib/libfoo.a" {
		t.Fatalf("h1 Unified = (%q,%v), want (/lib/libfoo.a,true)", v, ok)
	}
	if _, ok := got["h2"].Unified(); ok {
		t.Fatalf("h2 should diverge across configs (no Unified value)")
	}
	if got["h2"].PerConfig["Release"] != "/build/Release/app" {
		t.Fatalf("h2 Release = %q, want /build/Release/app", got["h2"].PerConfig["Release"])
	}
}

func TestReadLiteralProbe_AbsentDir(t *testing.T) {
	got, err := ReadLiteralProbe(t.TempDir())
	if err != nil {
		t.Fatalf("absent probe dir should be (nil,nil), got err %v", err)
	}
	if got != nil {
		t.Fatalf("absent probe dir should return nil, got %v", got)
	}
}

func TestBuildCmakeArgv_WiresLiteralProbe(t *testing.T) {
	argv, err := buildCmakeArgv(Options{
		SourceRoot: "/src", BuildDir: "/build", BuildType: "Release",
	}, "/build/dump-vars.cmake", "", "/build/probe-genex.cmake", "/build/probe-literals.cmake")
	if err != nil {
		t.Fatal(err)
	}
	var tli string
	for _, a := range argv {
		if strings.HasPrefix(a, "-DCMAKE_PROJECT_TOP_LEVEL_INCLUDES=") {
			tli = a
		}
	}
	if tli == "" {
		t.Fatal("no TOP_LEVEL_INCLUDES arg emitted")
	}
	if !strings.Contains(tli, "/build/probe-literals.cmake") {
		t.Fatalf("literal-probe hook not in TOP_LEVEL_INCLUDES: %s", tli)
	}
	// The literal probe must come AFTER the structural probe so its
	// DEFER fires later.
	if strings.Index(tli, "probe-genex.cmake") > strings.Index(tli, "probe-literals.cmake") {
		t.Fatalf("literal probe must follow structural probe: %s", tli)
	}
}
