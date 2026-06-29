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
		nil,   // dirScopes
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
	got := buildConfigureFileGenrule("gen_cfg_h", "cfg.h", rendered, call, "/src/project", "/src/project", nil, false, nil, nil)
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
		nil,  // dirScopes
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
// while the baked value stays in values as the non-stamped fallback. A
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
		nil,  // dirScopes
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
	// The baked value stays in values as the non-stamped fallback.
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
		nil,  // dirScopes
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
	out, err := recoverConfigureFilesFromCalls(calls, hostSrc, hostSrc, hostBuild, hostBuild, nil, false, nil, cc)
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

// TestRecoverConfigureFilesFromCalls_DefersToClaimedOutput pins the duplicate-
// producer guard ([1]): when another lifter already claimed the output path —
// most often execute_process's regenerating producer, which runs BEFORE
// configure_file recovery — configure_file must DEFER: emit no second producer
// and leave the existing OutToGenrule mapping intact, so the BUILD never declares
// the same output twice (Bazel rejects "generated by both") and the consumer
// keeps wiring to the regenerating producer over this frozen bake.
func TestRecoverConfigureFilesFromCalls_DefersToClaimedOutput(t *testing.T) {
	hostSrc := t.TempDir()
	hostBuild := t.TempDir()
	if err := os.WriteFile(filepath.Join(hostBuild, "cfg.h"), []byte("body\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	calls := []shadow.ConfigureFileCall{{
		Input:  filepath.Join(hostSrc, "cfg.h.in"),
		Output: filepath.Join(hostBuild, "cfg.h"),
	}}
	cc := newCodegenContext()
	cc.OutToGenrule["cfg.h"] = "exec_cfg_h" // an earlier producer already owns it
	out, err := recoverConfigureFilesFromCalls(calls, hostSrc, hostSrc, hostBuild, hostBuild, nil, false, nil, cc)
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if len(cc.Genrules) != 0 {
		t.Errorf("Genrules = %d; want 0 (deferred to the existing producer)", len(cc.Genrules))
	}
	if cc.OutToGenrule["cfg.h"] != "exec_cfg_h" {
		t.Errorf("OutToGenrule[cfg.h] = %q; want it left as the existing producer exec_cfg_h", cc.OutToGenrule["cfg.h"])
	}
	if len(out) != 0 {
		t.Errorf("out = %v; want 0 entries (configure_file defers the output entirely)", out)
	}
}

// TestRecoverConfigureFilesFromCalls_IncludedModuleRelativeOutput pins the
// theme-6 vtk proj_config.h fix: a relative configure_file output whose call
// lives in an include()d .cmake module (CallFile under a `cmake/` subdir with
// no directory scope of its own) must anchor to the INCLUDER's directory scope
// — its CMAKE_CURRENT_BINARY_DIR — not to dir(CallFile). The module sits at
// sub/cmake/Mod.cmake but the scope is `sub`, so `configure_file(... src/out.h)`
// writes to sub/src/out.h. Pre-fix the recovery looked under sub/cmake/src and
// silently dropped the output.
func TestRecoverConfigureFilesFromCalls_IncludedModuleRelativeOutput(t *testing.T) {
	hostSrc := t.TempDir()
	hostBuild := t.TempDir()
	// The configured output lands in the INCLUDER's binary dir (sub/src), not
	// the module's (sub/cmake/src).
	outDir := filepath.Join(hostBuild, "sub", "src")
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outDir, "proj_config.h"), []byte("#define HAVE_X 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	calls := []shadow.ConfigureFileCall{{
		// Call made inside an include()d module under sub/cmake/.
		CallFile: filepath.Join(hostSrc, "sub", "cmake", "Mod.cmake"),
		Input:    "cmake/proj_config.cmake.in",
		Output:   "src/proj_config.h", // relative → anchored to the includer scope
	}}
	// Directory scopes: root + "sub" (the includer). "sub/cmake" is NOT a scope.
	dirScopes := []dirScope{{Source: "", Build: ""}, {Source: "sub", Build: "sub"}}
	cc := newCodegenContext()
	out, err := recoverConfigureFilesFromCalls(calls, hostSrc, hostSrc, hostBuild, hostBuild, dirScopes, false, nil, cc)
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("out = %v; want 1 entry (the included-module output recovered)", out)
	}
	if out[0].RelOutput != "sub/src/proj_config.h" {
		t.Errorf("RelOutput = %q, want sub/src/proj_config.h (includer scope, not sub/cmake)", out[0].RelOutput)
	}
	if _, ok := cc.OutToGenrule["sub/src/proj_config.h"]; !ok {
		t.Errorf("no genrule recovered for sub/src/proj_config.h; OutToGenrule=%v", cc.OutToGenrule)
	}

	// Control: with no dirScopes (offline fallback), it anchors to dir(CallFile)
	// = sub/cmake and looks under sub/cmake/src — which doesn't exist — so the
	// output is dropped (the pre-fix behavior, exercised by the fallback path).
	cc2 := newCodegenContext()
	out2, err := recoverConfigureFilesFromCalls(calls, hostSrc, hostSrc, hostBuild, hostBuild, nil, false, nil, cc2)
	if err != nil {
		t.Fatalf("recover (no scopes): %v", err)
	}
	if len(out2) != 0 {
		t.Errorf("without dirScopes the includer-scoped output should NOT resolve (fallback to dir(CallFile)); got %v", out2)
	}
}

func TestDirScopeRel(t *testing.T) {
	src := "/src/proj"
	scopes := []dirScope{
		{Source: "", Build: ""},
		{Source: "sub", Build: "sub"},
		// A custom-binary-dir add_subdirectory: outputs land at the BUILD
		// path, not the source-relative one.
		{Source: "sub/deep", Build: "custom/deepbuild"},
		{Source: "other", Build: "other"},
	}
	cases := []struct {
		callFile string
		want     string
		ok       bool
	}{
		// Call from a CMakeLists.txt in a scope → that scope.
		{"/src/proj/sub/CMakeLists.txt", "sub", true},
		// Call from an include()d module under sub/cmake (no own scope) → sub.
		{"/src/proj/sub/cmake/Mod.cmake", "sub", true},
		// Deepest wins, and the scope's BUILD path is returned: a module
		// under sub/deep/x anchors at the custom binary dir, not sub/deep.
		{"/src/proj/sub/deep/x/Mod.cmake", "custom/deepbuild", true},
		// Root-level call → "".
		{"/src/proj/CMakeLists.txt", "", true},
		// Outside the source tree → no scope.
		{"/elsewhere/Mod.cmake", "", false},
	}
	for _, c := range cases {
		got, ok := dirScopeRel(c.callFile, src, scopes)
		if got != c.want || ok != c.ok {
			t.Errorf("dirScopeRel(%q) = (%q, %v), want (%q, %v)", c.callFile, got, ok, c.want, c.ok)
		}
	}

	// A trailing-slash recordedSrcDir must still match (normalized internally).
	if got, ok := dirScopeRel("/src/proj/sub/cmake/Mod.cmake", "/src/proj/", scopes); got != "sub" || !ok {
		t.Errorf("dirScopeRel with trailing-slash src = (%q, %v), want (\"sub\", true)", got, ok)
	}
}

func TestReanchorConvertTimePaths(t *testing.T) {
	cases := []struct {
		name                      string
		content, src, build, want string
	}{
		{
			name:    "curl configurehelp $Cpreprocessor",
			content: `$Cpreprocessor = '"/usr/bin/cc" -E -I/tmp/curl/include -I/tmp/convert-element-build-1910530120/lib -I/tmp/curl/lib';`,
			src:     "/tmp/curl",
			build:   "/tmp/convert-element-build-1910530120",
			want:    `$Cpreprocessor = '"/usr/bin/cc" -E -Iinclude -Ilib -Ilib';`,
		},
		{
			name:    "no convert-time paths is a no-op",
			content: "#define VER \"1.2.3\"\n",
			src:     "/tmp/curl",
			build:   "/tmp/convert-element-build-1910530120",
			want:    "#define VER \"1.2.3\"\n",
		},
		{
			name:    "in-source build strips the longer (build) prefix first",
			content: "-I/tmp/proj/build/gen -I/tmp/proj/include",
			src:     "/tmp/proj",
			build:   "/tmp/proj/build",
			want:    "-Igen -Iinclude",
		},
		{
			name:    "empty prefixes do not eat slashes",
			content: "a/b/c",
			src:     "",
			build:   "",
			want:    "a/b/c",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := reanchorConvertTimePaths(c.content, c.src, c.build); got != c.want {
				t.Errorf("reanchorConvertTimePaths() = %q, want %q", got, c.want)
			}
		})
	}
}

// TestRecoverConfigureFilesFromCalls_ScrubsConvertTimePaths is the
// end-to-end guard: a configure_file output whose rendered bytes bake the
// ephemeral convert-time build dir (and source dir) is recovered with
// those prefixes stripped, so the emitted genrule content is deterministic
// and free of the dangling /tmp/convert-element-build-XXXX path.
func TestRecoverConfigureFilesFromCalls_ScrubsConvertTimePaths(t *testing.T) {
	hostSrc := t.TempDir()
	hostBuild := t.TempDir()
	rendered := []byte("preproc = '-I" + hostSrc + "/include -I" + hostBuild + "/lib';\n")
	if err := os.WriteFile(filepath.Join(hostBuild, "configurehelp.pm"), rendered, 0o644); err != nil {
		t.Fatal(err)
	}
	calls := []shadow.ConfigureFileCall{{
		Input:  filepath.Join(hostSrc, "configurehelp.pm.in"),
		Output: filepath.Join(hostBuild, "configurehelp.pm"),
	}}
	cc := newCodegenContext()
	// recorded == host here (online-convert shape), so the baked prefixes
	// match what the scrub strips.
	if _, err := recoverConfigureFilesFromCalls(calls, hostSrc, hostSrc, hostBuild, hostBuild, nil, false, nil, cc); err != nil {
		t.Fatalf("recover: %v", err)
	}
	if len(cc.Genrules) != 1 {
		t.Fatalf("Genrules = %d; want 1", len(cc.Genrules))
	}
	content := strings.Join(cc.Genrules[0].WriteFileContent, "\n")
	if strings.Contains(content, hostBuild) || strings.Contains(content, hostSrc) {
		t.Errorf("emitted content still carries a convert-time abs path:\n%s", content)
	}
	if want := "preproc = '-Iinclude -Ilib';"; !strings.Contains(content, want) {
		t.Errorf("emitted content = %q, want it to contain %q", content, want)
	}
}

// TestRecoverConfigureFilesFromCalls_SkipsAndDedupes locks the
// silent-skip paths:
//   - empty calls → nil, no error.
//   - relative output WITHOUT a call site → skipped (can't anchor).
//     (A relative output WITH a CallFile is anchored + recovered — see
//     the sibling TestRecoverConfigureFilesFromCalls_RelativeOutputAnchored.)
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
		out, err := recoverConfigureFilesFromCalls(nil, hostSrc, hostSrc, hostBuild, hostBuild, nil, false, nil, cc)
		if err != nil || out != nil {
			t.Errorf("got (%v, %v); want (nil, nil)", out, err)
		}
	})

	t.Run("empty hostBuildDir returns nil, nil", func(t *testing.T) {
		cc := newCodegenContext()
		calls := []shadow.ConfigureFileCall{{Output: "/tmp/build/cfg.h"}}
		out, err := recoverConfigureFilesFromCalls(calls, hostSrc, hostSrc, "", "", nil, false, nil, cc)
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
		out, err := recoverConfigureFilesFromCalls(calls, hostSrc, hostSrc, hostBuild, hostBuild, nil, false, nil, cc)
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

// TestRecoverConfigureFilesFromCalls_RelativeOutputAnchored pins the fix for
// the ubiquitous `configure_file(config.h.in config.h)` autotools idiom: a
// RELATIVE output is anchored against CMAKE_CURRENT_BINARY_DIR — the build-dir
// mirror of the directory of the CMakeLists that made the call (CallFile) —
// and recovered, instead of being skipped for "lack of per-call binary-dir
// context".
func TestRecoverConfigureFilesFromCalls_RelativeOutputAnchored(t *testing.T) {
	hostSrc := t.TempDir()
	hostBuild := t.TempDir()
	// The call is made from a sub-directory's CMakeLists, so the binary-dir
	// mirror is hostBuild/sub — exercises the relDir anchoring, not just root.
	if err := os.MkdirAll(filepath.Join(hostBuild, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hostBuild, "sub", "config.h"), []byte("#define X 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	calls := []shadow.ConfigureFileCall{{
		Input:    "config.h.cmake.in", // relative input (resolved against the source dir)
		Output:   "config.h",          // RELATIVE output — the autotools idiom
		CallFile: filepath.Join(hostSrc, "sub", "CMakeLists.txt"),
	}}
	cc := newCodegenContext()
	out, err := recoverConfigureFilesFromCalls(calls, hostSrc, hostSrc, hostBuild, hostBuild, nil, false, nil, cc)
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("out = %v; want 1 (relative output anchored + recovered)", out)
	}
	if out[0].RelOutput != "sub/config.h" {
		t.Errorf("RelOutput = %q, want sub/config.h", out[0].RelOutput)
	}
	if _, ok := cc.OutToGenrule["sub/config.h"]; !ok {
		t.Errorf("OutToGenrule missing sub/config.h: %v", cc.OutToGenrule)
	}
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

// TestRecoverConfigureFilesFromCalls_DeferDirectoryAnchor pins the
// cmake_language(DEFER DIRECTORY <dir> CALL configure_file …) anchoring: the
// deferred call executes at <dir>'s scope end with <dir>'s
// CMAKE_CURRENT_BINARY_DIR, so its RELATIVE output lands in <dir>'s build
// mirror — NOT the registration site's scope (what CallFile-based anchoring
// computes). sub/CMakeLists.txt defers to the root, so `cfg.h` is written at
// <build>/cfg.h; pre-fix the recovery looked under <build>/sub/ and silently
// dropped a generated header consumers #include.
func TestRecoverConfigureFilesFromCalls_DeferDirectoryAnchor(t *testing.T) {
	hostSrc := t.TempDir()
	hostBuild := t.TempDir()
	// cmake wrote the deferred output at the ROOT build dir.
	if err := os.WriteFile(filepath.Join(hostBuild, "cfg.h"), []byte("#define CFG_VALUE 7\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	calls := []shadow.ConfigureFileCall{{
		CallFile: filepath.Join(hostSrc, "sub", "CMakeLists.txt"),
		Input:    filepath.Join(hostSrc, "sub", "cfg.h.in"),
		Output:   "cfg.h", // relative → resolves in the DEFERRED-TO scope
		DeferDir: hostSrc, // cmake_language(DEFER DIRECTORY ${CMAKE_SOURCE_DIR} …)
	}}
	dirScopes := []dirScope{{Source: "", Build: ""}, {Source: "sub", Build: "sub"}}
	cc := newCodegenContext()
	out, err := recoverConfigureFilesFromCalls(calls, hostSrc, hostSrc, hostBuild, hostBuild, dirScopes, false, nil, cc)
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("out = %v; want 1 entry (the deferred output recovered at the root scope)", out)
	}
	if out[0].RelOutput != "cfg.h" {
		t.Errorf("RelOutput = %q, want cfg.h (deferred-to root scope, not sub/)", out[0].RelOutput)
	}
	if _, ok := cc.OutToGenrule["cfg.h"]; !ok {
		t.Errorf("no genrule recovered for cfg.h; OutToGenrule=%v", cc.OutToGenrule)
	}

	// Control: the same call WITHOUT DeferDir anchors to the registration
	// scope (sub/), where no file exists — dropped. That is correct for a
	// plain DEFER (own-scope execution) and was the pre-fix behavior for
	// DEFER DIRECTORY.
	calls[0].DeferDir = ""
	cc2 := newCodegenContext()
	out2, err := recoverConfigureFilesFromCalls(calls, hostSrc, hostSrc, hostBuild, hostBuild, dirScopes, false, nil, cc2)
	if err != nil {
		t.Fatalf("recover (no DeferDir): %v", err)
	}
	if len(out2) != 0 {
		t.Errorf("without DeferDir the output should anchor to sub/ and drop; got %v", out2)
	}
}

// TestRecoverConfigureFiles_SamePathCopyOnlyMirror_NoRule pins the no-rule
// rewire: a COPYONLY configure_file whose template's source-relative path
// equals the output's build-relative path (the "stage a header into the
// binary dir" idiom) emits NO rule — consumers resolve the rel path to the
// committed source file via the includes attr's source-root coverage. A
// RENAMED copy (cfg.h.in → cfg.h) keeps its rule.
func TestRecoverConfigureFiles_SamePathCopyOnlyMirror_NoRule(t *testing.T) {
	hostSrc := t.TempDir()
	hostBuild := t.TempDir()
	write := func(root, rel, body string) {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(hostSrc, "sub/config.h", "#define X 1\n")
	write(hostBuild, "sub/config.h", "#define X 1\n")
	write(hostSrc, "sub/cfg.h.in", "#define Y 2\n")
	write(hostBuild, "sub/cfg.h", "#define Y 2\n")
	calls := []shadow.ConfigureFileCall{
		{
			CallFile: filepath.Join(hostSrc, "sub", "CMakeLists.txt"),
			Input:    filepath.Join(hostSrc, "sub", "config.h"),
			Output:   filepath.Join(hostBuild, "sub", "config.h"),
			Options:  []string{"COPYONLY"},
		},
		{
			CallFile: filepath.Join(hostSrc, "sub", "CMakeLists.txt"),
			Input:    filepath.Join(hostSrc, "sub", "cfg.h.in"),
			Output:   filepath.Join(hostBuild, "sub", "cfg.h"),
			Options:  []string{"COPYONLY"},
		},
	}
	cc := newCodegenContext()
	out, err := recoverConfigureFilesFromCalls(calls, hostSrc, hostSrc, hostBuild, hostBuild, nil, false, nil, cc)
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("out = %+v; want both outputs wired for consumer attribution", out)
	}
	// Exactly ONE rule: the renamed copy. The same-path mirror emits none.
	if len(cc.Genrules) != 1 {
		t.Fatalf("Genrules = %+v; want exactly one (the renamed cfg.h copy)", cc.Genrules)
	}
	if got := cc.Genrules[0].GenruleOuts; len(got) != 1 || got[0] != "sub/cfg.h" {
		// Bake tier may emit write_file instead of genrule for text.
		if cc.Genrules[0].WriteFileOut != "sub/cfg.h" {
			t.Errorf("the surviving rule should produce sub/cfg.h; got %+v", cc.Genrules[0])
		}
	}
	if _, registered := cc.OutToGenrule["sub/config.h"]; registered {
		t.Errorf("same-path mirror must not register a producer; consumers resolve to the source file")
	}
}

// TestRecoverConfigureFiles_OutOfProjectRecipeGate: a configure_file issued from
// OUTSIDE the project (a cmake module in the install prefix) is recovered only
// when its output is an include()d .cmake recipe — the OUTPUT->include tie. A
// non-included out-of-project .cmake (a package/version config) is dropped (it
// would be a dead genrule); an IN-project .cmake is recovered regardless of
// include() (unchanged behavior).
func TestRecoverConfigureFiles_OutOfProjectRecipeGate(t *testing.T) {
	hostSrc := t.TempDir()
	hostBuild := t.TempDir()
	for _, f := range []string{"recipe.cmake", "inproj.cmake", "config.cmake"} {
		if err := os.WriteFile(filepath.Join(hostBuild, f), []byte("# "+f+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	const prefix = "/usr/share/cmake-4.3/Modules"
	calls := []shadow.ConfigureFileCall{
		// out-of-project, .cmake, INCLUDED -> recovered (the prefix-recipe win).
		{Input: prefix + "/Mod.cmake.in", Output: filepath.Join(hostBuild, "recipe.cmake"), CallFile: prefix + "/Mod.cmake"},
		// out-of-project, .cmake, NOT included -> dropped (dead genrule otherwise).
		{Input: prefix + "/Cfg.cmake.in", Output: filepath.Join(hostBuild, "config.cmake"), CallFile: prefix + "/Cfg.cmake"},
		// in-project, .cmake, NOT included -> recovered (in-project unaffected).
		{Input: filepath.Join(hostSrc, "x.cmake.in"), Output: filepath.Join(hostBuild, "inproj.cmake"), CallFile: filepath.Join(hostSrc, "CMakeLists.txt")},
	}
	cc := newCodegenContext()
	cc.IncludeCalls = []shadow.IncludeCall{{Path: filepath.Join(hostBuild, "recipe.cmake")}}
	out, err := recoverConfigureFilesFromCalls(calls, hostSrc, hostSrc, hostBuild, hostBuild, nil, false, nil, cc)
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	got := map[string]bool{}
	for _, o := range out {
		got[o.RelOutput] = true
	}
	if !got["recipe.cmake"] {
		t.Error("included out-of-project recipe .cmake should be recovered")
	}
	if got["config.cmake"] {
		t.Error("non-included out-of-project .cmake should be DROPPED (dead genrule)")
	}
	if !got["inproj.cmake"] {
		t.Error("in-project .cmake should be recovered regardless of include()")
	}
}
