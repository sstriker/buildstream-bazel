package lower

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/ir"
	"github.com/sstriker/buildstream-bazel/internal/configurefile"
	"github.com/sstriker/buildstream-bazel/internal/shadow"
)

// Batch C of the test-coverage plan in PR #199: configure_file
// lift symmetry with file(GENERATE). Each test pins one of the
// helpers configure_file.go currently has at 0–50% coverage,
// plus the configureFileTagSet-style refactor lifted from
// fileGenerateTagSet (the plan's small-refactor-while-we're-here).

// ---------------------------------------------------------------------------
// configureFileTags / configureFileTagSet
// ---------------------------------------------------------------------------

// TestConfigureFileTags covers the zero-value and Lifted-true
// facet of the new configureFileTagSet (mirrors
// fileGenerateTags(s)/fileGenerateTagSet from file_generate.go).
// Pins:
//   - base tags always present and sorted,
//   - cmake-codegen-driver=configure_file is the driver facet,
//   - Lifted appends cmake-codegen-lifted (and only when true).
func TestConfigureFileTags(t *testing.T) {
	baseTags := []string{
		"cmake-codegen",
		"cmake-codegen-configure-file",
		"cmake-codegen-driver=configure_file",
	}
	cases := []struct {
		name string
		set  configureFileTagSet
		want []string
	}{
		{name: "zero value emits base tags only", set: configureFileTagSet{}, want: baseTags},
		{
			name: "Lifted appends cmake-codegen-lifted",
			set:  configureFileTagSet{Lifted: true},
			want: append(append([]string{}, baseTags...), "cmake-codegen-lifted"),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := configureFileTags(tc.set)
			// Sort copies so the comparison is order-independent;
			// configureFileTags also sorts internally — we check
			// both the membership AND the sorted-order contract.
			wantSorted := append([]string{}, tc.want...)
			sort.Strings(wantSorted)
			if !equalStringsForCF(got, wantSorted) {
				t.Errorf("configureFileTags(%+v) = %v, want %v", tc.set, got, wantSorted)
			}
			// Sorted-order contract: result equals its sorted self.
			gotSorted := append([]string{}, got...)
			sort.Strings(gotSorted)
			if !equalStringsForCF(got, gotSorted) {
				t.Errorf("configureFileTags(%+v) result %v is not sorted", tc.set, got)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// isConfigureFileKeyword
// ---------------------------------------------------------------------------

func TestIsConfigureFileKeyword(t *testing.T) {
	keywords := []string{
		"@ONLY",
		"COPYONLY",
		"ESCAPE_QUOTES",
		"NEWLINE_STYLE",
		"FILE_PERMISSIONS",
		"USE_SOURCE_PERMISSIONS",
		"NO_SOURCE_PERMISSIONS",
	}
	for _, kw := range keywords {
		t.Run("matches "+kw, func(t *testing.T) {
			if !isConfigureFileKeyword(kw) {
				t.Errorf("isConfigureFileKeyword(%q) = false, want true", kw)
			}
		})
		t.Run("case-insensitive "+kw, func(t *testing.T) {
			if !isConfigureFileKeyword(strings.ToLower(kw)) {
				t.Errorf("isConfigureFileKeyword(%q) = false, want true (case-insensitive)", strings.ToLower(kw))
			}
		})
	}
	for _, nonKw := range []string{"", "OWNER_READ", "1.2.3", "WORLD_WRITE"} {
		t.Run("rejects "+nonKw, func(t *testing.T) {
			if isConfigureFileKeyword(nonKw) {
				t.Errorf("isConfigureFileKeyword(%q) = true, want false", nonKw)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// configureFileOptionsFromCall
// ---------------------------------------------------------------------------

// TestConfigureFileOptionsFromCall covers the option-list
// parser. The hazard is wrong option mapping shipping wrong
// bytes through the lifted shape — the verify-pass catches
// most divergence, but a mis-decoded NEWLINE_STYLE could
// silently confuse the Substitute round-trip. Locks the parser
// against the four documented option families plus the
// malformed-input failure mode.
func TestConfigureFileOptionsFromCall(t *testing.T) {
	cases := []struct {
		name    string
		in      []string
		want    configurefile.Options
		wantErr string // substring; empty means no error
	}{
		{name: "empty list", in: nil, want: configurefile.Options{}},
		{name: "@ONLY", in: []string{"@ONLY"}, want: configurefile.Options{AtOnly: true}},
		{name: "COPYONLY", in: []string{"COPYONLY"}, want: configurefile.Options{CopyOnly: true}},
		{name: "ESCAPE_QUOTES", in: []string{"ESCAPE_QUOTES"}, want: configurefile.Options{EscapeQuotes: true}},
		{
			name: "NEWLINE_STYLE UNIX (LF)",
			in:   []string{"NEWLINE_STYLE", "UNIX"},
			want: configurefile.Options{NewlineStyle: configurefile.NewlineLF},
		},
		{
			name: "NEWLINE_STYLE LF",
			in:   []string{"NEWLINE_STYLE", "LF"},
			want: configurefile.Options{NewlineStyle: configurefile.NewlineLF},
		},
		{
			name: "NEWLINE_STYLE DOS (CRLF)",
			in:   []string{"NEWLINE_STYLE", "DOS"},
			want: configurefile.Options{NewlineStyle: configurefile.NewlineCRLF},
		},
		{
			name: "NEWLINE_STYLE WIN32 (CRLF)",
			in:   []string{"NEWLINE_STYLE", "WIN32"},
			want: configurefile.Options{NewlineStyle: configurefile.NewlineCRLF},
		},
		{
			name: "NEWLINE_STYLE CRLF",
			in:   []string{"NEWLINE_STYLE", "CRLF"},
			want: configurefile.Options{NewlineStyle: configurefile.NewlineCRLF},
		},
		{
			name:    "NEWLINE_STYLE without value",
			in:      []string{"NEWLINE_STYLE"},
			wantErr: "without value",
		},
		{
			name:    "NEWLINE_STYLE with unknown value",
			in:      []string{"NEWLINE_STYLE", "EBCDIC"},
			wantErr: "unknown value",
		},
		{
			name: "case-insensitive keyword matches",
			in:   []string{"@only", "escape_quotes"},
			want: configurefile.Options{AtOnly: true, EscapeQuotes: true},
		},
		{
			name: "USE_SOURCE_PERMISSIONS is accepted-and-ignored",
			in:   []string{"USE_SOURCE_PERMISSIONS", "@ONLY"},
			want: configurefile.Options{AtOnly: true},
		},
		{
			name: "NO_SOURCE_PERMISSIONS is accepted-and-ignored",
			in:   []string{"NO_SOURCE_PERMISSIONS"},
			want: configurefile.Options{},
		},
		{
			name: "FILE_PERMISSIONS consumes its variadic value list until next keyword",
			in:   []string{"FILE_PERMISSIONS", "OWNER_READ", "OWNER_WRITE", "@ONLY"},
			want: configurefile.Options{AtOnly: true},
		},
		{
			name: "FILE_PERMISSIONS at end of list consumes through end",
			in:   []string{"@ONLY", "FILE_PERMISSIONS", "OWNER_READ", "OWNER_WRITE"},
			want: configurefile.Options{AtOnly: true},
		},
		{
			name: "unknown option is accepted (verify-pass decides)",
			in:   []string{"SOMETHING_NEW", "@ONLY"},
			want: configurefile.Options{AtOnly: true},
		},
		{
			name: "multiple flags combine",
			in:   []string{"@ONLY", "ESCAPE_QUOTES", "NEWLINE_STYLE", "LF"},
			want: configurefile.Options{
				AtOnly:       true,
				EscapeQuotes: true,
				NewlineStyle: configurefile.NewlineLF,
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := configureFileOptionsFromCall(tc.in)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("configureFileOptionsFromCall(%v) = %+v, nil; want error containing %q", tc.in, got, tc.wantErr)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("error %q does not contain %q", err.Error(), tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("configureFileOptionsFromCall(%v) returned unexpected err: %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("configureFileOptionsFromCall(%v) = %+v, want %+v", tc.in, got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// pickValues
// ---------------------------------------------------------------------------

// TestPickValues covers the lifter's values-source priority:
// (1) cmakeVars succeeds when Substitute round-trips, (2) Extract
// fallback when cmakeVars verify-pass diverges, (3) (nil, false)
// when both refuse.
func TestPickValues(t *testing.T) {
	template := []byte("#define VER \"@VERSION@\"\n")
	rendered := []byte("#define VER \"1.2.3\"\n")
	opts := configurefile.Options{AtOnly: true}

	t.Run("cmakeVars succeeds (full namespace wins)", func(t *testing.T) {
		vars := map[string]string{"VERSION": "1.2.3", "EXTRA": "unused"}
		got, ok := pickValues(template, rendered, opts, vars)
		if !ok {
			t.Fatal("pickValues returned !ok with sufficient cmakeVars")
		}
		// Identity check: when cmakeVars round-trips, the function
		// returns the cmakeVars map verbatim (incl. the EXTRA noise).
		if got["VERSION"] != "1.2.3" || got["EXTRA"] != "unused" {
			t.Errorf("returned values %v don't carry cmakeVars verbatim", got)
		}
	})

	t.Run("cmakeVars wrong-value falls back to Extract", func(t *testing.T) {
		// Verify-pass divergence: cmakeVars says VERSION=4.5.6
		// but rendered says 1.2.3. Substitute(...) != rendered,
		// so pickValues falls through to Extract — which recovers
		// only the variables the template references.
		vars := map[string]string{"VERSION": "4.5.6"}
		got, ok := pickValues(template, rendered, opts, vars)
		if !ok {
			t.Fatal("pickValues returned !ok; expected Extract fallback")
		}
		if got["VERSION"] != "1.2.3" {
			t.Errorf("Extract fallback recovered VERSION=%q, want 1.2.3", got["VERSION"])
		}
		if _, present := got["EXTRA"]; present {
			t.Errorf("Extract fallback returned cmakeVars-like map: %v", got)
		}
	})

	t.Run("Extract failure returns (nil, false)", func(t *testing.T) {
		// Template + rendered with no recoverable shape (mismatched
		// line counts beyond Extract's tolerance).
		badTmpl := []byte("line1\nline2\n@MISSING@\n")
		badRendered := []byte("totally different output\n")
		got, ok := pickValues(badTmpl, badRendered, configurefile.Options{}, nil)
		if ok {
			t.Errorf("pickValues = %v, true; want (nil, false) on Extract failure", got)
		}
	})
}

// ---------------------------------------------------------------------------
// buildConfigureFileGenrule
// ---------------------------------------------------------------------------

// TestBuildConfigureFileGenrule_BakeShape locks the bake (non-lifted)
// shape. The lift-disabled flag forces the bake; for \n-only-text
// rendered bytes that's the readable skylib write_file (content
// round-trips the bytes), and the tags must NOT include
// cmake-codegen-lifted. (The binary base64-genrule fallback is pinned
// separately by TestBuildConfigureFileGenrule_BinaryBakeStaysBase64.)
func TestBuildConfigureFileGenrule_BakeShape(t *testing.T) {
	rendered := []byte("#define VER \"1.2.3\"\n")
	call := shadow.ConfigureFileCall{
		Input:  "/src/project/cfg.h.in",
		Output: "/tmp/build/cfg.h",
	}
	got := buildConfigureFileGenrule(
		"gen_cfg_h", "cfg.h", rendered, call,
		"/src/project", "/src/project",
		false, // liftEnabled = false → legacy
		nil,
		nil, // stampVars
	)
	// The rendered bytes are \n-only text, so the non-lifted bake now
	// lowers to the readable skylib write_file (shared bakeFileTarget)
	// rather than the legacy base64 genrule.
	if got.Kind != ir.KindWriteFile {
		t.Errorf("kind = %v, want KindWriteFile", got.Kind)
	}
	if got.WriteFileOut != "cfg.h" {
		t.Errorf("write_file out = %q, want cfg.h", got.WriteFileOut)
	}
	if got.WriteFileNewline != "unix" {
		t.Errorf("write_file newline = %q, want unix", got.WriteFileNewline)
	}
	if join := strings.Join(got.WriteFileContent, "\n"); join != string(rendered) {
		t.Errorf("write_file content round-trip = %q, want %q", join, string(rendered))
	}
	if got.Srcs != nil {
		t.Errorf("bake shape must not declare srcs; got %v", got.Srcs)
	}
	if got.GenruleTools != nil {
		t.Errorf("bake shape must not declare tools; got %v", got.GenruleTools)
	}
	if hasTag(got.Tags, "cmake-codegen-lifted") {
		t.Errorf("bake shape carries cmake-codegen-lifted tag: %v", got.Tags)
	}
	if !hasTag(got.Tags, "cmake-codegen-driver=configure_file") {
		t.Errorf("driver tag missing: %v", got.Tags)
	}
}

// TestBuildConfigureFileGenrule_BinaryBakeStaysBase64 pins the
// byte-exact fallback for the configure_file bake too: a body with a
// NUL control byte stays on the base64 genrule rather than write_file.
func TestBuildConfigureFileGenrule_BinaryBakeStaysBase64(t *testing.T) {
	rendered := []byte("a\x00b\n")
	call := shadow.ConfigureFileCall{Input: "/src/project/cfg.h.in", Output: "/tmp/build/cfg.h"}
	got := buildConfigureFileGenrule("gen_cfg_h", "cfg.h", rendered, call, "/src/project", "/src/project", false, nil, nil)
	if got.Kind != ir.KindGenrule {
		t.Fatalf("binary bake should stay on the base64 genrule; got kind %v", got.Kind)
	}
	if want := base64.StdEncoding.EncodeToString(rendered); !strings.Contains(got.GenruleCmd, want) {
		t.Errorf("base64 cmd missing exact rendered bytes; cmd=%q want substr %q", got.GenruleCmd, want)
	}
}

// TestBuildConfigureFileGenrule_LiftedShape covers the lifted
// shape on a recoverable template: the spec references the
// template via its `template` field, the tool is set, and the
// rendered bytes are never carried structurally (the spec holds
// the template + values, not the output bytes). Cleanly matches
// the file(GENERATE) lifted-shape test's symmetry the plan calls
// out as overdue.
func TestBuildConfigureFileGenrule_LiftedShape(t *testing.T) {
	template := "#define VER \"@VERSION@\"\n"
	rendered := []byte("#define VER \"1.2.3\"\n")

	hostSrc := t.TempDir()
	templateRel := "src/cfg.h.in"
	if err := os.MkdirAll(filepath.Join(hostSrc, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hostSrc, templateRel), []byte(template), 0o644); err != nil {
		t.Fatal(err)
	}
	call := shadow.ConfigureFileCall{
		Input:   filepath.Join(hostSrc, templateRel),
		Output:  "/tmp/build/cfg.h",
		Options: []string{"@ONLY"},
	}

	got := buildConfigureFileGenrule(
		"gen_cfg_h", "cfg.h", rendered, call,
		hostSrc, hostSrc,
		true, // liftEnabled
		map[string]string{"VERSION": "1.2.3"},
		nil, // stampVars
	)
	if got.Kind != ir.KindCMakeConfigureFile || got.CMakeConfigureFile == nil {
		t.Fatalf("lifted kind = %v (spec nil? %v); want KindCMakeConfigureFile", got.Kind, got.CMakeConfigureFile == nil)
	}
	if got.CMakeConfigureFile.Template != templateRel {
		t.Errorf("lifted template = %q, want %q", got.CMakeConfigureFile.Template, templateRel)
	}
	if got.CMakeConfigureFile.Tool != "//tools:cmake-configure-file" {
		t.Errorf("lifted tool = %q, want //tools:cmake-configure-file", got.CMakeConfigureFile.Tool)
	}
	if !hasTag(got.Tags, "cmake-codegen-lifted") {
		t.Errorf("lifted shape missing cmake-codegen-lifted tag: %v", got.Tags)
	}
	// Soundness: the rendered output bytes are never carried in the
	// spec — the lift holds the template + values map and re-renders
	// at Bazel time. (No structured field holds the output bytes, so
	// there's nothing to assert their absence against.)
	if got.CMakeConfigureFile.Content != "" {
		t.Errorf("INPUT-form lift must not carry inline Content; got %q", got.CMakeConfigureFile.Content)
	}
}

// TestBuildConfigureFileGenrule_StampValues covers the VCS-stamp lift: a
// template referencing a stamp-sourced var (@GIT_SHA@) gets a stamp_values
// entry so the rule re-reads the live revision from the workspace status,
// while the baked value stays in values as the no---stamp fallback. A
// stamp var the template does NOT reference is excluded (no spurious
// status dependency).
func TestBuildConfigureFileGenrule_StampValues(t *testing.T) {
	template := "#define REV \"@GIT_SHA@\"\n#define VER \"@VERSION@\"\n"
	rendered := []byte("#define REV \"abc123\"\n#define VER \"1.2.3\"\n")

	hostSrc := t.TempDir()
	templateRel := "src/version.h.in"
	if err := os.MkdirAll(filepath.Join(hostSrc, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hostSrc, templateRel), []byte(template), 0o644); err != nil {
		t.Fatal(err)
	}
	call := shadow.ConfigureFileCall{
		Input:   filepath.Join(hostSrc, templateRel),
		Output:  "/tmp/build/version.h",
		Options: []string{"@ONLY"},
	}
	got := buildConfigureFileGenrule(
		"gen_version_h", "version.h", rendered, call,
		hostSrc, hostSrc,
		true, // liftEnabled
		map[string]string{"GIT_SHA": "abc123", "VERSION": "1.2.3"},
		map[string]string{"GIT_SHA": "STABLE_GIT_SHA", "UNUSED_STAMP": "STABLE_UNUSED_STAMP"},
	)
	if got.Kind != ir.KindCMakeConfigureFile || got.CMakeConfigureFile == nil {
		t.Fatalf("kind = %v (spec nil? %v); want lifted KindCMakeConfigureFile", got.Kind, got.CMakeConfigureFile == nil)
	}
	sv := got.CMakeConfigureFile.StampValues
	if len(sv) != 1 || sv["GIT_SHA"] != "STABLE_GIT_SHA" {
		t.Errorf("stamp_values = %v, want {GIT_SHA: STABLE_GIT_SHA} (UNUSED_STAMP not referenced, must be excluded)", sv)
	}
	// The baked value stays in values as the no---stamp fallback.
	if got.CMakeConfigureFile.Values["GIT_SHA"] != "abc123" {
		t.Errorf("values[GIT_SHA] = %q, want the baked fallback abc123", got.CMakeConfigureFile.Values["GIT_SHA"])
	}
}

// TestBuildConfigureFileGenrule_FallsBackOnMissingTemplate
// covers the "lift-eligible but template not readable" branch:
// the template path resolves under recordedSrcDir but the file
// doesn't exist on the host. Must fall back to the legacy shape
// silently — same soundness contract as the other fallback rows.
func TestBuildConfigureFileGenrule_FallsBackOnMissingTemplate(t *testing.T) {
	rendered := []byte("body\n")
	hostSrc := t.TempDir()
	call := shadow.ConfigureFileCall{
		Input:  filepath.Join(hostSrc, "cfg.h.in"), // not actually created
		Output: "/tmp/build/cfg.h",
	}
	got := buildConfigureFileGenrule(
		"gen_cfg_h", "cfg.h", rendered, call,
		hostSrc, hostSrc,
		true, // liftEnabled, but template ReadFile fails
		map[string]string{"VAR": "v"},
		nil, // stampVars
	)
	if got.Srcs != nil || got.GenruleTools != nil {
		t.Errorf("expected legacy fallback; got srcs=%v tools=%v", got.Srcs, got.GenruleTools)
	}
	if hasTag(got.Tags, "cmake-codegen-lifted") {
		t.Errorf("fallback shape carries cmake-codegen-lifted tag: %v", got.Tags)
	}
}

// ---------------------------------------------------------------------------
// recoverConfigureFilesFromCalls
// ---------------------------------------------------------------------------

// TestRecoverConfigureFilesFromCalls_HappyPath drives the top-
// level entry on a single configure_file call that the host
// build dir actually contains. Pins:
//   - one genrule synthesized on cc.Genrules,
//   - cc.OutToGenrule populated with the rel-output → name map,
//   - the returned []configureFileOut carries the recording-
//     machine absolute path and the build-rel path the consumer
//     attribution uses.
func TestRecoverConfigureFilesFromCalls_HappyPath(t *testing.T) {
	hostSrc := t.TempDir()
	hostBuild := t.TempDir()
	rendered := []byte("body\n")
	if err := os.WriteFile(filepath.Join(hostBuild, "cfg.h"), rendered, 0o644); err != nil {
		t.Fatal(err)
	}
	calls := []shadow.ConfigureFileCall{{
		Input:  filepath.Join(hostSrc, "cfg.h.in"),
		Output: filepath.Join(hostBuild, "cfg.h"),
	}}
	cc := newCodegenContext()
	out, err := recoverConfigureFilesFromCalls(calls, hostSrc, hostSrc, hostBuild, hostBuild, false, nil, cc)
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("out = %v; want 1 entry", out)
	}
	if out[0].RelOutput != "cfg.h" {
		t.Errorf("RelOutput = %q, want cfg.h", out[0].RelOutput)
	}
	if len(cc.Genrules) != 1 {
		t.Fatalf("Genrules = %d; want 1", len(cc.Genrules))
	}
	if name, ok := cc.OutToGenrule["cfg.h"]; !ok || name != cc.Genrules[0].Name {
		t.Errorf("OutToGenrule[cfg.h] = %q, want %q", name, cc.Genrules[0].Name)
	}
}

// TestRecoverConfigureFilesFromCalls_SkipsAndDedupes locks the
// silent-skip paths:
//   - empty calls → nil, no error.
//   - relative output → skipped (no per-call binary-dir context).
//   - output outside build dir → skipped.
//   - duplicate call (cmake re-evaluating the same configure_file
//     across frames) → only one genrule.
//   - missing file on disk → skipped silently (offline-fixture
//     graceful-degrade contract).
func TestRecoverConfigureFilesFromCalls_SkipsAndDedupes(t *testing.T) {
	hostSrc := t.TempDir()
	hostBuild := t.TempDir()
	rendered := []byte("body\n")
	if err := os.WriteFile(filepath.Join(hostBuild, "cfg.h"), rendered, 0o644); err != nil {
		t.Fatal(err)
	}

	t.Run("nil calls returns nil, nil", func(t *testing.T) {
		cc := newCodegenContext()
		out, err := recoverConfigureFilesFromCalls(nil, hostSrc, hostSrc, hostBuild, hostBuild, false, nil, cc)
		if err != nil || out != nil {
			t.Errorf("got (%v, %v); want (nil, nil)", out, err)
		}
	})

	t.Run("empty hostBuildDir returns nil, nil", func(t *testing.T) {
		cc := newCodegenContext()
		calls := []shadow.ConfigureFileCall{{Output: "/tmp/build/cfg.h"}}
		out, err := recoverConfigureFilesFromCalls(calls, hostSrc, hostSrc, "", "", false, nil, cc)
		if err != nil || out != nil {
			t.Errorf("got (%v, %v); want (nil, nil)", out, err)
		}
	})

	t.Run("dedupes duplicate calls and skips relative/outside outputs", func(t *testing.T) {
		calls := []shadow.ConfigureFileCall{
			{Output: filepath.Join(hostBuild, "cfg.h")}, // happy
			{Output: filepath.Join(hostBuild, "cfg.h")}, // dup — should dedupe
			{Output: "cfg.h"},                               // relative — skip
			{Output: "/etc/passwd"},                         // outside build dir — skip
			{Output: filepath.Join(hostBuild, "missing.h")}, // not on disk — skip silently
		}
		cc := newCodegenContext()
		out, err := recoverConfigureFilesFromCalls(calls, hostSrc, hostSrc, hostBuild, hostBuild, false, nil, cc)
		if err != nil {
			t.Fatalf("recover: %v", err)
		}
		if len(out) != 1 {
			t.Errorf("out = %v; want 1 (dedup + skip filters)", out)
		}
		if len(cc.Genrules) != 1 {
			t.Errorf("Genrules = %d; want 1", len(cc.Genrules))
		}
	})
}

// equalStringsForCF is a local string-slice equality helper.
// (genrule_internal_test.go has equalStrings; the linter merges
// the two if we move to one common helper later.)
func equalStringsForCF(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
