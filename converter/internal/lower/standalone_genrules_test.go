package lower

import (
	"reflect"
	"strings"
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/internal/ninja"
	"github.com/sstriker/buildstream-bazel/converter/internal/todos"
	"github.com/sstriker/buildstream-bazel/converter/ir"
	"github.com/sstriker/buildstream-bazel/internal/shadow"
)

func mustParseNinja(t *testing.T, src string) *ninja.Graph {
	t.Helper()
	g, err := ninja.Parse(strings.NewReader(src), "", nil)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return g
}

func TestLowerStandaloneCustomCommands_StandaloneEdge(t *testing.T) {
	g := mustParseNinja(t, `rule CUSTOM_COMMAND
  command = $COMMAND
rule CXX_COMPILER
  command = c++ -c $in -o $out

build version.txt: CUSTOM_COMMAND
  COMMAND = git rev-parse HEAD
build foo.o: CXX_COMPILER foo.cc
`)
	got := lowerStandaloneCustomCommands(g, nil, "", "/build", "", "", nil, standaloneTraceContext{}, nil, nil)
	if len(got) != 1 {
		t.Fatalf("want 1 standalone; got %d (%v)", len(got), got)
	}
	if got[0].Kind != ir.KindGenrule {
		t.Errorf("Kind: %v", got[0].Kind)
	}
	if got[0].Name != "custom_command_version_txt" {
		t.Errorf("Name: %q", got[0].Name)
	}
	if got[0].GenruleCmd != "git rev-parse HEAD" {
		t.Errorf("Cmd: %q", got[0].GenruleCmd)
	}
	if len(got[0].GenruleOuts) != 1 || got[0].GenruleOuts[0] != "version.txt" {
		t.Errorf("Outs: %v", got[0].GenruleOuts)
	}
}

// TestLowerStandaloneCustomCommands_BreadcrumbsInternalDrop pins that a
// dropped cmake-internal edge (a scripted CDash dashboard target) is NOT
// emitted as a genrule AND is recorded in the filteredInternal sink with its
// category — the audit breadcrumb so the drop isn't silent.
func TestLowerStandaloneCustomCommands_BreadcrumbsInternalDrop(t *testing.T) {
	g := mustParseNinja(t, `rule CUSTOM_COMMAND
  command = $COMMAND

build CMakeFiles/ExperimentalStart: CUSTOM_COMMAND
  COMMAND = /usr/bin/ctest -C Debug -DMODEL=Experimental -DACTIONS=Start -S CMakeFiles/CTestScript.cmake -V
build real.stamp: CUSTOM_COMMAND
  COMMAND = touch real.stamp
`)
	sink := map[string]string{}
	got := lowerStandaloneCustomCommands(g, nil, "", "/build", "", "", nil, standaloneTraceContext{}, sink, nil)
	// Only the real edge survives as a genrule.
	if len(got) != 1 || got[0].GenruleOuts[0] != "real.stamp" {
		t.Fatalf("want only real.stamp genrule; got %v", got)
	}
	// The dashboard edge is dropped but breadcrumbed.
	if sink["CMakeFiles/ExperimentalStart"] != "dashboard" {
		t.Errorf("dashboard drop not breadcrumbed: sink=%v", sink)
	}
}

func TestCmakeInternalCmdKind(t *testing.T) {
	cases := []struct{ cmd, want string }{
		{`/usr/bin/ctest -C Debug -DMODEL=Experimental -DACTIONS=Start -S CMakeFiles/CTestScript.cmake`, "dashboard"},
		{`ctest -D Nightly`, "dashboard"},
		{`cmake -P cmake_install.cmake`, "install"},
		{`cd /tmp/b && /usr/bin/cmake -P cmake_uninstall.cmake`, "uninstall"},
		{`cmake -P cmake_uninstall.cmake`, "uninstall"},
		// Project-specific uninstall script names — the script name is
		// project-chosen, but always carries the word "uninstall" (case-
		// insensitive). eigen ships EigenUninstall.cmake; others use
		// <Proj>Uninstall.cmake / uninstall.cmake; the -P arg may be a relative
		// or absolute path.
		{`/usr/local/opt/cmake-4.3.3/bin/cmake -P cmake/EigenUninstall.cmake`, "uninstall"},
		{`cmake -P FooUninstall.cmake`, "uninstall"},
		{`cmake -P /abs/path/Uninstall.cmake`, "uninstall"},
		{`cmake -P uninstall.cmake`, "uninstall"},
		// Versioned / non-standard install path (the web-session cmake pin)
		// must still match — normalization keys on the basename, not a fixed
		// /usr/bin prefix.
		{`cd /tmp/b && /usr/local/opt/cmake-4.3.3/bin/cmake -P cmake_uninstall.cmake`, "uninstall"},
		{`/usr/local/opt/cmake-4.3.3/bin/cmake -P cmake_install.cmake`, "install"},
		// A user's own -P script is NOT swept up — no "uninstall" in the name.
		{`cmake -P myscript.cmake`, ""},
		// A script merely mentioning install but not an install/uninstall
		// maintenance script also isn't swept up (the install match keys on the
		// exact cmake_install.cmake name, not any *install* substring).
		{`cmake -P generate_installer_config.cmake`, ""},
		{`cmake --regenerate-during-build -S/src -B/build`, "regen"},
		{`cpack --config CPackConfig.cmake`, "cpack"},
		{`ninja clean && rm -rf `, "clean"},
		{`cd /tmp/b && /usr/local/bin/ninja clean`, "clean"},
		{`echo No interactive CMake dialog available.`, "ide-stub"},
		{`python gen.py in out`, ""},
		{`git rev-parse HEAD`, ""},
		// create_symlink is filtered separately (isCreateSymlinkCmd), not here.
		{`cmake -E create_symlink zstd zstdcat`, ""},
	}
	for _, tc := range cases {
		if got := cmakeInternalCmdKind(tc.cmd); got != tc.want {
			t.Errorf("cmakeInternalCmdKind(%q) = %q, want %q", tc.cmd, got, tc.want)
		}
	}
}

func TestIsCreateSymlinkCmd(t *testing.T) {
	cases := []struct {
		cmd  string
		want bool
	}{
		// Raw ninja form (what the recovery sees, before the ln -sfn rewrite),
		// across cmake paths — zstd's zstdcat/unzstd/zstdmt tool aliases and
		// the .1 manpage aliases.
		{`/usr/local/opt/cmake-4.3.3/bin/cmake -E create_symlink zstd zstdcat`, true},
		{`cd /tmp/b && /usr/bin/cmake -E create_symlink zstd.1 zstdcat.1`, true},
		{`cmake -E create_symlink libfoo.so.1 libfoo.so`, true},
		// Not create_symlink.
		{`ln -sfn zstd zstdcat`, false}, // post-rewrite form; the filter runs pre-rewrite
		{`cp build/cmake/../../programs/zstdless.1 .`, false},
		{`cmake -E copy a b`, false},
		{`python gen.py`, false},
	}
	for _, tc := range cases {
		if got := isCreateSymlinkCmd(tc.cmd); got != tc.want {
			t.Errorf("isCreateSymlinkCmd(%q) = %v, want %v", tc.cmd, got, tc.want)
		}
	}
}

func TestIsCopyCmd(t *testing.T) {
	cases := []struct {
		cmd  string
		want bool
	}{
		// Raw ninja form (pre-rewrite), across cmake paths + copy variants.
		{`/usr/local/opt/cmake-4.3.3/bin/cmake -E copy build/cmake/../../programs/zstd.1 .`, true},
		{`cmake -E copy_if_different a b`, true},
		{`cmake -E copy_directory src dst`, true},
		// Not a copy.
		{`cmake -E create_symlink zstd zstdcat`, false},
		{`cp a b`, false}, // post-rewrite form; the drop check runs on the raw `-E copy`
		{`python gen.py`, false},
	}
	for _, tc := range cases {
		if got := isCopyCmd(tc.cmd); got != tc.want {
			t.Errorf("isCopyCmd(%q) = %v, want %v", tc.cmd, got, tc.want)
		}
	}
}

func TestLowerStandaloneCustomCommands_SkipsCoveredEdge(t *testing.T) {
	g := mustParseNinja(t, `rule CUSTOM_COMMAND
  command = $COMMAND
rule CXX_COMPILER
  command = c++ -c $in -o $out

build generated.h: CUSTOM_COMMAND tmpl
  COMMAND = python gen.py $in $out
build foo.o: CXX_COMPILER foo.cc
`)
	existing := []ir.Target{{
		Name:        "generated_h",
		Kind:        ir.KindGenrule,
		GenruleOuts: []string{"generated.h"},
	}}
	got := lowerStandaloneCustomCommands(g, existing, "", "/build", "", "", nil, standaloneTraceContext{}, nil, nil)
	if len(got) != 0 {
		t.Errorf("expected dedup; got %v", got)
	}
}

func TestLowerStandaloneCustomCommands_HandlesMultipleStandalone(t *testing.T) {
	g := mustParseNinja(t, `rule CUSTOM_COMMAND
  command = $COMMAND

build alpha.stamp: CUSTOM_COMMAND
  COMMAND = touch alpha.stamp
build beta.stamp: CUSTOM_COMMAND
  COMMAND = touch beta.stamp
`)
	got := lowerStandaloneCustomCommands(g, nil, "", "/build", "", "", nil, standaloneTraceContext{}, nil, nil)
	if len(got) != 2 {
		t.Fatalf("want 2 standalone; got %d", len(got))
	}
	// Ninja-source order preserved.
	if got[0].Name != "custom_command_alpha_stamp" {
		t.Errorf("got[0].Name = %q", got[0].Name)
	}
	if got[1].Name != "custom_command_beta_stamp" {
		t.Errorf("got[1].Name = %q", got[1].Name)
	}
}

func TestLowerStandaloneCustomCommands_HandlesNameCollision(t *testing.T) {
	// Two outputs that sanitize to the same name (different
	// extensions wouldn't normally collide; this synthetic edge
	// pair exercises the suffix path).
	g := mustParseNinja(t, `rule CUSTOM_COMMAND
  command = $COMMAND

build foo: CUSTOM_COMMAND
  COMMAND = touch foo
build foo_: CUSTOM_COMMAND
  COMMAND = touch foo_
`)
	got := lowerStandaloneCustomCommands(g, nil, "", "/build", "", "", nil, standaloneTraceContext{}, nil, nil)
	if len(got) != 2 {
		t.Fatalf("want 2; got %d", len(got))
	}
	if got[0].Name == got[1].Name {
		t.Errorf("names should disambiguate; got both %q", got[0].Name)
	}
}

func TestLowerStandaloneCustomCommands_SkipsRuleWithoutCommand(t *testing.T) {
	g := mustParseNinja(t, `rule CUSTOM_COMMAND
  description = no-op

build phony: CUSTOM_COMMAND
`)
	got := lowerStandaloneCustomCommands(g, nil, "", "/build", "", "", nil, standaloneTraceContext{}, nil, nil)
	if len(got) != 0 {
		t.Errorf("expected skip when rule has no command; got %v", got)
	}
}

func TestLowerStandaloneCustomCommands_PreservesImplicitOuts(t *testing.T) {
	g := mustParseNinja(t, `rule CUSTOM_COMMAND
  command = $COMMAND

build main.txt | side.txt: CUSTOM_COMMAND in
  COMMAND = gen $in
`)
	got := lowerStandaloneCustomCommands(g, nil, "", "/build", "", "", nil, standaloneTraceContext{}, nil, nil)
	if len(got) != 1 {
		t.Fatalf("want 1; got %d", len(got))
	}
	// Sorted outs include both main and implicit side.
	if len(got[0].GenruleOuts) != 2 {
		t.Errorf("outs len: %d", len(got[0].GenruleOuts))
	}
}

func TestLowerStandaloneCustomCommands_NilGraph(t *testing.T) {
	if got := lowerStandaloneCustomCommands(nil, nil, "", "/build", "", "", nil, standaloneTraceContext{}, nil, nil); got != nil {
		t.Errorf("nil graph should return nil; got %v", got)
	}
}

func TestLowerStandaloneCustomCommands_FiltersCMakeBookkeepingEdges(t *testing.T) {
	// Mirrors the bookkeeping edges cmake's Ninja generator always
	// emits: edit_cache / rebuild_cache utility outputs under
	// CMakeFiles/<name>.util. Plus a real user-declared standalone
	// edge so the test can confirm the filter only skips the
	// bookkeeping shape, not anything else.
	g := mustParseNinja(t, `rule CUSTOM_COMMAND
  command = $COMMAND

build CMakeFiles/edit_cache.util: CUSTOM_COMMAND
  COMMAND = /usr/bin/cmake -E echo No interactive CMake dialog available.
build CMakeFiles/rebuild_cache.util: CUSTOM_COMMAND
  COMMAND = /usr/bin/cmake --regenerate-during-build -S/src -B/build
build version.txt: CUSTOM_COMMAND
  COMMAND = git rev-parse HEAD
`)
	got := lowerStandaloneCustomCommands(g, nil, "", "/build", "", "", nil, standaloneTraceContext{}, nil, nil)
	if len(got) != 1 {
		t.Fatalf("want 1 standalone (bookkeeping edges filtered); got %d (%v)", len(got), got)
	}
	if got[0].Name != "custom_command_version_txt" {
		t.Errorf("got[0].Name = %q; want custom_command_version_txt (the non-bookkeeping edge)", got[0].Name)
	}
}

func TestLowerStandaloneCustomCommands_FiltersNinjaVarOutputs(t *testing.T) {
	// cmake's Ninja generator pairs every real custom-command
	// output with a `${cmake_ninja_workdir}<basename>` implicit
	// output for restat tracking. The walker must drop those
	// from the genrule's outs (Bazel can't declare an outs entry
	// whose path is a ninja-time variable) but keep the real
	// output so the genrule lands with usable outs + name.
	g := mustParseNinja(t, `rule CUSTOM_COMMAND
  command = $COMMAND

build version.txt | ${cmake_ninja_workdir}version.txt: CUSTOM_COMMAND
  COMMAND = touch version.txt
`)
	got := lowerStandaloneCustomCommands(g, nil, "", "/build", "", "", nil, standaloneTraceContext{}, nil, nil)
	if len(got) != 1 {
		t.Fatalf("want 1 standalone; got %d (%v)", len(got), got)
	}
	if want := []string{"version.txt"}; !equalStringSlices(got[0].GenruleOuts, want) {
		t.Errorf("outs = %v, want %v (${cmake_ninja_workdir}-shadow stripped)", got[0].GenruleOuts, want)
	}
	if got[0].Name != "custom_command_version_txt" {
		t.Errorf("name = %q, want custom_command_version_txt (first non-var output)", got[0].Name)
	}
}

func equalStringSlices(a, b []string) bool {
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

func TestFilterOutVarRefs(t *testing.T) {
	cases := []struct {
		in   []string
		want []string
	}{
		{
			in:   []string{"version.txt", "${cmake_ninja_workdir}version.txt"},
			want: []string{"version.txt"},
		},
		{
			in:   []string{"${cmake_ninja_workdir}foo", "${cmake_ninja_workdir}bar"},
			want: []string{},
		},
		{
			in:   []string{"a", "b"},
			want: []string{"a", "b"},
		},
		{
			in:   nil,
			want: nil,
		},
	}
	for i, tc := range cases {
		got := filterOutVarRefs(append([]string(nil), tc.in...))
		if len(got) == 0 && len(tc.want) == 0 {
			continue
		}
		if !equalStringSlices(got, tc.want) {
			t.Errorf("case %d: filterOutVarRefs(%v) = %v, want %v", i, tc.in, got, tc.want)
		}
	}
}

func TestIsCMakeBookkeepingOutput(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		// Single-config (Ninja) shape: CMakeFiles/<name>.util at
		// the build-dir root.
		{"CMakeFiles/edit_cache.util", true},
		{"CMakeFiles/rebuild_cache.util", true},
		{"CMakeFiles/install.util", true},
		{"CMakeFiles/package_source.util", true},
		{"CMakeFiles/list_install_components.util", true},
		// Multi-config (Ninja Multi-Config) shape: per-subdir
		// per-config CMakeFiles dir holding the .util bookkeeping
		// output. Surveyed against zlib/spdlog/Catch2/libpng/VTK/
		// LLVM under -G "Ninja Multi-Config" Release;Debug — these
		// surface as `<subdir>/CMakeFiles/<Config>/<name>.util`
		// and previously slipped through the filter, lifting
		// cpack / ctest / install / uninstall genrules with
		// convert-time absolute paths in their cmd.
		{"test/CMakeFiles/Release/package.util", true},
		{"contrib/CMakeFiles/Debug/install.util", true},
		{"src/CMakeFiles/Release/test.util", true},
		// User-declared add_custom_command outputs never land
		// under CMakeFiles/ with the .util suffix.
		{"version.txt", false},
		{"gen/version.h", false},
		{"CMakeFiles/stub.dir/src/stub.c.o", false}, // compile artefact, not bookkeeping
		{"CMakeFiles/foo.txt", false},               // wrong suffix
		{"some/notCMakeFiles/foo.util", false},      // CMakeFiles not a path component

		// cmake's add_custom_target(check-<name> COMMAND llvm-lit ...)
		// shape — test-runner edges that don't end in .util. cmake
		// uses these for `ninja check-all` etc; Bazel users emit
		// cc_test rules instead, so the converted genrule would be
		// dead code with a broken cmd (llvm-lit is configure_file-
		// generated and not a build artifact).
		{"CMakeFiles/check-all", true},
		{"test/CMakeFiles/check-llvm", true},
		{"test/CMakeFiles/check-llvm-analysis-aliasset", true},
		{"utils/mlgo-utils/CMakeFiles/check-mlgo-utils", true},
		// User-declared `check-config` outputs (not under
		// CMakeFiles/) pass through.
		{"check-config", false},
		{"some/notCMakeFiles/check-foo", false},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			if got := isCMakeBookkeepingOutput(tc.in); got != tc.want {
				t.Errorf("isCMakeBookkeepingOutput(%q) = %v; want %v", tc.in, got, tc.want)
			}
		})
	}
}

// TestLowerStandaloneCustomCommands_TraceNamesFromAddCustomTarget
// covers the trace cross-reference: when an in-trace
// add_custom_target wraps the OUTPUT (via DEPENDS), the emitted
// genrule takes the target's name instead of the
// `custom_command_<sanitized-output>` fallback. Matches the
// CMakeLists.txt shape:
//
//	add_custom_command(OUTPUT version.h COMMAND gen)
//	add_custom_target(gen_headers DEPENDS version.h)
//
// → genrule name `gen_headers`, not `custom_command_version_h`.
func TestLowerStandaloneCustomCommands_TraceNamesFromAddCustomTarget(t *testing.T) {
	g := mustParseNinja(t, `rule CUSTOM_COMMAND
  command = $COMMAND

build version.h: CUSTOM_COMMAND
  COMMAND = gen
`)
	ctx := standaloneTraceContext{
		CustomCommands: []shadow.AddCustomCommandCall{
			{Outputs: []string{"version.h"}, Commands: [][]string{{"gen"}}},
		},
		CustomTargets: []shadow.AddCustomTargetCall{
			{Name: "gen_headers", Depends: []string{"version.h"}},
		},
	}
	got := lowerStandaloneCustomCommands(g, nil, "", "/build", "", "", nil, ctx, nil, nil)
	if len(got) != 1 {
		t.Fatalf("want 1 standalone; got %d", len(got))
	}
	if got[0].Name != "gen_headers" {
		t.Errorf("Name: %q want gen_headers (taken from add_custom_target)", got[0].Name)
	}
}

// TestLowerStandaloneCustomCommands_TraceVisibilityFromAddDependencies
// covers the visibility leg of the cross-reference: when an
// in-trace add_dependencies call references the wrapping target
// (or the output directly), the genrule's visibility opens from
// `//visibility:private` to `:__pkg__` so the downstream consumer
// can reference it.
//
//	add_custom_command(OUTPUT generated.h COMMAND gen)
//	add_custom_target(gen_target DEPENDS generated.h)
//	add_dependencies(mylib gen_target)  # ← consumer
//
// → genrule named `gen_target` with `:__pkg__` visibility.
func TestLowerStandaloneCustomCommands_TraceVisibilityFromAddDependencies(t *testing.T) {
	g := mustParseNinja(t, `rule CUSTOM_COMMAND
  command = $COMMAND

build generated.h: CUSTOM_COMMAND
  COMMAND = gen
`)
	ctx := standaloneTraceContext{
		CustomCommands: []shadow.AddCustomCommandCall{
			{Outputs: []string{"generated.h"}, Commands: [][]string{{"gen"}}},
		},
		CustomTargets: []shadow.AddCustomTargetCall{
			{Name: "gen_target", Depends: []string{"generated.h"}},
		},
		AddDependencies: []shadow.AddDependenciesCall{
			{Target: "mylib", Depends: []string{"gen_target"}},
		},
	}
	got := lowerStandaloneCustomCommands(g, nil, "", "/build", "", "", nil, ctx, nil, nil)
	if len(got) != 1 {
		t.Fatalf("want 1 standalone; got %d", len(got))
	}
	if got[0].Name != "gen_target" {
		t.Errorf("Name: %q want gen_target", got[0].Name)
	}
	if len(got[0].Visibility) != 1 || got[0].Visibility[0] != ":__pkg__" {
		t.Errorf("Visibility: %v want [:__pkg__] (downstream consumer signals package visibility)", got[0].Visibility)
	}
}

// TestLowerStandaloneCustomCommands_TraceVisibilityFromDirectOutputConsumer
// covers the edge case where add_dependencies references the
// OUTPUT path directly (legal in cmake; uncommon but rendered):
//
//	add_custom_command(OUTPUT foo.txt COMMAND ...)
//	add_dependencies(consumer foo.txt)
//
// → emitted genrule's visibility opens to `:__pkg__` even though
// no add_custom_target wraps the output.
func TestLowerStandaloneCustomCommands_TraceVisibilityFromDirectOutputConsumer(t *testing.T) {
	g := mustParseNinja(t, `rule CUSTOM_COMMAND
  command = $COMMAND

build foo.txt: CUSTOM_COMMAND
  COMMAND = touch foo.txt
`)
	ctx := standaloneTraceContext{
		CustomCommands: []shadow.AddCustomCommandCall{
			{Outputs: []string{"foo.txt"}, Commands: [][]string{{"touch", "foo.txt"}}},
		},
		AddDependencies: []shadow.AddDependenciesCall{
			{Target: "consumer", Depends: []string{"foo.txt"}},
		},
	}
	got := lowerStandaloneCustomCommands(g, nil, "", "/build", "", "", nil, ctx, nil, nil)
	if len(got) != 1 {
		t.Fatalf("want 1 standalone; got %d", len(got))
	}
	// No add_custom_target → falls back to the output-derived
	// name (cross-reference doesn't have a target name to use).
	if got[0].Name != "custom_command_foo_txt" {
		t.Errorf("Name: %q want custom_command_foo_txt", got[0].Name)
	}
	// But the add_dependencies(consumer foo.txt) opens visibility.
	if len(got[0].Visibility) != 1 || got[0].Visibility[0] != ":__pkg__" {
		t.Errorf("Visibility: %v want [:__pkg__]", got[0].Visibility)
	}
}

// TestLowerStandaloneCustomCommands_TraceEmptyKeepsLegacyBehavior
// confirms the offline-replay-no-trace path (zero-valued
// standaloneTraceContext) keeps the legacy naming + private
// visibility — important for byte-stability of projects converted
// without trace capture.
func TestLowerStandaloneCustomCommands_TraceEmptyKeepsLegacyBehavior(t *testing.T) {
	g := mustParseNinja(t, `rule CUSTOM_COMMAND
  command = $COMMAND

build version.txt: CUSTOM_COMMAND
  COMMAND = touch version.txt
`)
	got := lowerStandaloneCustomCommands(g, nil, "", "/build", "", "", nil, standaloneTraceContext{}, nil, nil)
	if len(got) != 1 {
		t.Fatalf("want 1 standalone; got %d", len(got))
	}
	if got[0].Name != "custom_command_version_txt" {
		t.Errorf("Name: %q (legacy shape)", got[0].Name)
	}
	if len(got[0].Visibility) != 1 || got[0].Visibility[0] != "//visibility:private" {
		t.Errorf("Visibility: %v want [//visibility:private]", got[0].Visibility)
	}
}

func TestSanitizeOutputName(t *testing.T) {
	cases := []struct{ in, want string }{
		{"version.txt", "version_txt"},
		{"gen/version.h", "gen_version_h"},
		{"foo-bar.cc", "foo_bar_cc"},
		{"./relative", "relative"},
		{"a..b//c", "a_b_c"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			if got := sanitizeOutputName(tc.in); got != tc.want {
				t.Errorf("sanitizeOutputName(%q) = %q; want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestCustomTargetStampIsNonAll(t *testing.T) {
	allByName := map[string]bool{
		"test-ci":  false, // add_custom_target(test-ci ...)  — no ALL
		"docs-all": true,  // add_custom_target(docs-all ALL ...)
	}
	cases := []struct {
		name string
		outs []string
		want bool
	}{
		{"non-all stamp", []string{"tests/CMakeFiles/test-ci"}, true},
		{"non-all stamp multi-config suffix", []string{"tests/CMakeFiles/test-ci-Debug"}, true},
		{"ALL target stays in default build", []string{"docs/CMakeFiles/docs-all-Release"}, false},
		{"unknown name under CMakeFiles (real genrule)", []string{"x/CMakeFiles/some_codegen.inc"}, false},
		{"deeper file under CMakeFiles is not a stamp", []string{"a/CMakeFiles/test-ci/extra.o"}, false},
		{"not under CMakeFiles", []string{"gen/test-ci"}, false},
		{"empty map (no trace)", []string{"tests/CMakeFiles/test-ci"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := allByName
			if c.name == "empty map (no trace)" {
				m = nil
			}
			if got := customTargetStampIsNonAll(c.outs, m); got != c.want {
				t.Errorf("customTargetStampIsNonAll(%v) = %v; want %v", c.outs, got, c.want)
			}
		})
	}
}

// TestTraceWrapperRealArgv: a cmake-generated `cmake -P`/`cmake -E` wrapper edge
// recovers the real command from the matching single-COMMAND add_custom_command
// trace record; multi-COMMAND, a user's own cmake -P, and non-cmake edges don't
// substitute.
func TestTraceWrapperRealArgv(t *testing.T) {
	idx := buildOutputToCustomCommand([]shadow.AddCustomCommandCall{
		{Outputs: []string{"foo.pb.cc", "foo.pb.h"}, Commands: [][]string{{"protoc", "--cpp_out=.", "foo.proto"}}},
		{Outputs: []string{"multi.out"}, Commands: [][]string{{"protoc", "--cpp_out=.", "m.proto"}, {"cp", "a", "b"}}},
		{Outputs: []string{"userp.out"}, Commands: [][]string{{"cmake", "-P", "user.cmake"}}},
	}, "")
	if got := traceWrapperRealArgv("cmake -P CMakeFiles/x.dir/foo.cmake", []string{"foo.pb.cc", "foo.pb.h"}, idx); len(got) != 3 || got[0] != "protoc" {
		t.Errorf("a cmake -P wrapper over foo.pb.* should yield the real protoc argv; got %v", got)
	}
	if got := traceWrapperRealArgv("cmake -P x.cmake", []string{"multi.out"}, idx); got != nil {
		t.Errorf("a multi-COMMAND record must not substitute (would drop bundled steps); got %v", got)
	}
	if got := traceWrapperRealArgv("cmake -P x.cmake", []string{"userp.out"}, idx); got != nil {
		t.Errorf("a user's own cmake -P must stay on the script path; got %v", got)
	}
	if got := traceWrapperRealArgv("protoc --cpp_out=. foo.proto", []string{"foo.pb.cc"}, idx); got != nil {
		t.Errorf("a non-cmake (real-tool) edge must not substitute; got %v", got)
	}
	if got := traceWrapperRealArgv("cmake -P x.cmake", []string{"foo.pb.cc"}, nil); got != nil {
		t.Errorf("no trace index → no substitution; got %v", got)
	}
}

// TestWrapperRealCodegenCmd_GuardsCleanRecognition: the substitution only fires
// when the wrapper's real argv recognizes CLEANLY. A wrapped protoc whose edge
// srcs surface the .proto substitutes; one whose srcs DON'T (Match succeeds on
// --cpp_out but Lower fails on the missing input) must NOT substitute, so the
// caller keeps the generic genrule instead of a strict refusal stub — the
// "degrades to today" guarantee. Recognition-off never substitutes.
func TestWrapperRealCodegenCmd_GuardsCleanRecognition(t *testing.T) {
	argv := []string{"protoc", "--cpp_out=.", "foo.proto"}
	outs := []string{"foo.pb.cc", "foo.pb.h"}

	cc := newCodegenContext()
	cc.RecognizeCodegen = true
	// srcs surface the .proto → clean recognition → substitute.
	if cmd, ok := wrapperRealCodegenCmd(cc, argv, []string{"foo.proto"}, outs, "pkg", "", nil); !ok || cmd.Driver != "protoc" {
		t.Errorf("clean recognition should substitute the real protoc argv; got ok=%v driver=%q", ok, cmd.Driver)
	}
	// srcs DON'T surface the .proto → Match-but-Lower-fails → keep the genrule.
	if _, ok := wrapperRealCodegenCmd(cc, argv, nil, outs, "pkg", "", nil); ok {
		t.Errorf("a real argv that matches but cannot Lower must NOT substitute (no regression to a strict refusal stub)")
	}

	// Recognition off: nothing to gain from substituting; keep the edge path.
	off := newCodegenContext()
	off.RecognizeCodegen = false
	if _, ok := wrapperRealCodegenCmd(off, argv, []string{"foo.proto"}, outs, "pkg", "", nil); ok {
		t.Errorf("recognition-off must not substitute")
	}
}

// TestRecognizeViaTraceWrapperArgv: the real argv recovered from a wrapper edge
// recognizes to the native rule (the point of P1) — a wrapped protoc lowers to
// proto_library + cc_proto_library instead of a generic genrule.
func TestRecognizeViaTraceWrapperArgv(t *testing.T) {
	cc := newCodegenContext()
	cc.RecognizeCodegen = true
	cmd := codegenCommandFromArgv([]string{"protoc", "--cpp_out=.", "foo.proto"},
		[]string{"foo.proto"}, []string{"foo.pb.cc", "foo.pb.h"}, "pkg")
	fallback := ir.Target{Name: "exec_foo", Kind: ir.KindGenrule, GenruleOuts: []string{"foo.pb.cc", "foo.pb.h"}}
	tgts, ok := recognizeOrGenrule(cc, cmd, fallback)
	if !ok || len(tgts) != 2 || tgts[0].NativeRule == nil || tgts[0].NativeRule.Kind != "proto_library" {
		t.Fatalf("a wrapped protoc should recognize to proto_library + cc_proto_library; got ok=%v %+v", ok, tgts)
	}
}

// TestStandaloneWrapperRecognizes gates the --cmake-script-bake fallback: a
// dispatch-wrapped protoc (real argv recovered from the trace) recognizes, so
// the bake must defer to the recognizer; a user's own cmake -P script does not,
// so the bake still applies; recognition-off never defers.
func TestStandaloneWrapperRecognizes(t *testing.T) {
	idx := buildOutputToCustomCommand([]shadow.AddCustomCommandCall{
		{Outputs: []string{"foo.pb.cc", "foo.pb.h"}, Commands: [][]string{{"protoc", "--cpp_out=.", "foo.proto"}}},
		{Outputs: []string{"userp.out"}, Commands: [][]string{{"cmake", "-P", "user.cmake"}}},
	}, "")

	cc := newCodegenContext()
	cc.RecognizeCodegen = true
	// Dispatch wrapper over protoc, srcs surface the .proto → recognizes → defer bake.
	if !cc.standaloneWrapperRecognizes("cmake -P CMakeFiles/x.dir/foo.cmake",
		[]string{"foo.proto"}, []string{"foo.pb.cc", "foo.pb.h"}, "", "pkg", idx, nil) {
		t.Error("a dispatch-wrapped protoc should be recognizable → bake must defer to the recognizer")
	}
	// A user's own cmake -P script (not a dispatch over a tool) → not recognizable → bake applies.
	if cc.standaloneWrapperRecognizes("cmake -P user.cmake",
		nil, []string{"userp.out"}, "", "pkg", idx, nil) {
		t.Error("a user cmake -P script is not a recognizable tool wrapper; bake should still apply")
	}
	// Recognition off → never defer (bake as before).
	off := newCodegenContext()
	off.RecognizeCodegen = false
	if off.standaloneWrapperRecognizes("cmake -P CMakeFiles/x.dir/foo.cmake",
		[]string{"foo.proto"}, []string{"foo.pb.cc", "foo.pb.h"}, "", "pkg", idx, nil) {
		t.Error("recognition-off must not defer the bake")
	}
}

// TestDropLiftedToolSrcs pins the shared step that keeps a tool the swap lifted
// to $(execpath <label>) + tools out of srcs (else it'd be both a src and a
// tool). Now called by all three genrule-emission paths (emitRecoveredGenrule,
// the standalone path, workdir-buildout) — direct coverage for the pure helper.
func TestDropLiftedToolSrcs(t *testing.T) {
	artifactToName := map[string]string{"tools/gen.sh": "gen_sh", "in.txt": "in_txt"}
	// gen.sh was lifted (its name is in tools as ":gen_sh") → dropped; in.txt is
	// a plain src (not in tools) → kept; helper.h has no artifactToName → kept.
	got := dropLiftedToolSrcs(
		[]string{"tools/gen.sh", "in.txt", "helper.h"},
		[]string{":gen_sh"},
		artifactToName,
	)
	want := []string{"in.txt", "helper.h"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("dropLiftedToolSrcs = %v, want %v", got, want)
	}
	// No-op guards: empty tools / srcs / artifactToName return srcs unchanged.
	srcs := []string{"tools/gen.sh"}
	if got := dropLiftedToolSrcs(srcs, nil, artifactToName); !reflect.DeepEqual(got, srcs) {
		t.Errorf("empty tools should be a no-op; got %v", got)
	}
	if got := dropLiftedToolSrcs(srcs, []string{":gen_sh"}, nil); !reflect.DeepEqual(got, srcs) {
		t.Errorf("empty artifactToName should be a no-op; got %v", got)
	}
}

// TestCodegenCommandFrom_UnwrapsWrappers: the recognizer's command view sees
// through cmake -E env/chdir and bare shell wrappers to the real tool, so a
// wrapped protoc still lowers to a native rule instead of a genrule of the
// wrapper. Only the driver/args VIEW is unwrapped; the genrule fallback keeps
// the original command.
func TestCodegenCommandFrom_UnwrapsWrappers(t *testing.T) {
	for _, tc := range []struct {
		name, cmd, wantDriver string
		wantArg0              string
	}{
		{"bare", "protoc --cpp_out=. foo.proto", "protoc", "--cpp_out=."},
		{"env", "env GEN=1 protoc --cpp_out=. foo.proto", "protoc", "--cpp_out=."},
		{"cmake -E env", "/usr/bin/cmake -E env GEN_FAST=1 protoc --cpp_out=. foo.proto", "protoc", "--cpp_out=."},
		{"cmake -E chdir", "cmake -E chdir /b protoc --cpp_out=. foo.proto", "protoc", "--cpp_out=."},
		{"cmake -E env then env", "cmake -E env A=1 env B=2 protoc --cpp_out=. foo.proto", "protoc", "--cpp_out=."},
		// A real cmake -E op (copy) is not a wrapper: driver stays cmake.
		{"cmake -E copy not unwrapped", "cmake -E copy a b", "cmake", "-E"},
	} {
		got := codegenCommandFrom(tc.cmd, nil, nil, "")
		if got.Driver != tc.wantDriver {
			t.Errorf("%s: Driver = %q, want %q", tc.name, got.Driver, tc.wantDriver)
		}
		if len(got.Args) == 0 || got.Args[0] != tc.wantArg0 {
			t.Errorf("%s: Args[0] = %v, want %q", tc.name, got.Args, tc.wantArg0)
		}
	}
}

// TestTryStandaloneCmakeScriptCodegen_Gating: the recognize-through-script entry
// declines (no convert-time cmake re-trace) unless RecognizeCodegen +
// CMakeScriptTrace + a cmake binary are all present and the cmd is cmake -P.
func TestTryStandaloneCmakeScriptCodegen_Gating(t *testing.T) {
	base := func() *codegenContext {
		cc := newCodegenContext()
		cc.RecognizeCodegen = true
		cc.CMakeScriptTrace = true
		cc.CMakeBinary = "/usr/bin/cmake"
		return cc
	}
	const scriptCmd = "cmake -P gen.cmake"
	outs := []string{"foo.pb.cc"}

	// nil b / g are safe: every case here short-circuits at the gate checks (or
	// the empty-outs guard) before recoverCmakeScriptCodegen / SeenBuilds[b].
	if base().tryStandaloneCmakeScriptCodegen(nil, "protoc --cpp_out=. foo.proto", "/s", "/b", outs, nil) {
		t.Error("non-cmake-script cmd must decline (handled by the recognizer chokepoint, not the script path)")
	}
	off := base()
	off.CMakeScriptTrace = false
	if off.tryStandaloneCmakeScriptCodegen(nil, scriptCmd, "/s", "/b", outs, nil) {
		t.Error("CMakeScriptTrace off must decline")
	}
	// RecognizeCodegen is deliberately NOT a gate here: with --cmake-script-trace
	// alone the recovery still fires (emitting a genrule; RecognizeCodegen only
	// upgrades it to a native rule via recognizeOrGenrule). So there is no
	// "RecognizeCodegen off must decline" case — that path now re-traces the
	// script, which needs a real edge/graph and is covered by the render gates.
	nocmake := base()
	nocmake.CMakeBinary = ""
	if nocmake.tryStandaloneCmakeScriptCodegen(nil, scriptCmd, "/s", "/b", outs, nil) {
		t.Error("no cmake binary must decline")
	}
	if base().tryStandaloneCmakeScriptCodegen(nil, scriptCmd, "/s", "/b", nil, nil) {
		t.Error("no declared outs must decline")
	}
}

// TestCoveredOuts_KindNativeRule pins the duplicate-producer guard ([19]): a
// KindNativeRule producer's declared out/outs (the codegen-recognizer substrate —
// pkg_tar from cmake -E tar, proto rules) must be covered, so when the same output
// is also a ninja CUSTOM_COMMAND edge the standalone pass does NOT re-emit a second
// genrule for it (Bazel rejects the duplicate generated file).
func TestCoveredOuts_KindNativeRule(t *testing.T) {
	existing := []ir.Target{{
		Name: "archive",
		Kind: ir.KindNativeRule,
		NativeRule: &ir.NativeRuleSpec{
			Attrs: []ir.NativeAttr{
				{Name: "out", Str: "dist/archive.tar"},
				{Name: "outs", List: []string{"gen/a.h", "gen/b.h"}},
			},
		},
	}}
	covered := coveredOuts(existing)
	for _, want := range []string{"dist/archive.tar", "gen/a.h", "gen/b.h"} {
		if !covered[want] {
			t.Errorf("coveredOuts missing KindNativeRule output %q; got %v", want, covered)
		}
	}
}

// TestCodegenCheckpointRestore pins the all-or-nothing rollback: a checkpoint
// captures the codegen consumer-wiring registries + Genrules length, and
// restore undoes every mutation made after it (the contract a partial
// recognize-through-script recovery relies on).
func TestCodegenCheckpointRestore(t *testing.T) {
	cc := newCodegenContext()
	cc.OutToGenrule["pre.h"] = "pre_gen"
	cc.Genrules = append(cc.Genrules, ir.Target{Name: "pre_gen", Kind: ir.KindGenrule})

	cp := cc.checkpointCodegen()

	// Mutate every checkpointed registry, as a recovery would.
	cc.OutToGenrule["new.pb.cc"] = "new_gen"
	cc.OutToNativeConsumerDep["new.pb.cc"] = ":new_proto"
	cc.OutToNativeConsumerPkg["new.pb.cc"] = "a"
	cc.NativeRuleSubPackage["new_proto"] = "sub"
	cc.recognizedConsumerByInput["foo.proto"] = "new_proto"
	cc.recognizedNameOwner["new_proto"] = "foo.proto"
	// Stamp / bake state the shared recovery's prescan also writes — must roll
	// back too, else a leaked StampVars entry mis-wires a later configure_file.
	cc.StampVars["GIT_SHA"] = "STABLE_GIT_SHA"
	cc.StampCommands["STABLE_GIT_SHA"] = "git rev-parse HEAD"
	cc.StampKeyCollisions["STABLE_GIT_SHA"] = true
	cc.bakeTodoDisposition["new_gen"] = todos.Actionable
	cc.Genrules = append(cc.Genrules, ir.Target{Name: "new_gen", Kind: ir.KindGenrule})

	cc.restoreCodegen(cp)

	if len(cc.Genrules) != 1 || cc.Genrules[0].Name != "pre_gen" {
		t.Errorf("Genrules not restored to the checkpoint: %+v", cc.Genrules)
	}
	if _, leaked := cc.OutToGenrule["new.pb.cc"]; leaked {
		t.Error("OutToGenrule leaked a post-checkpoint entry after restore")
	}
	if cc.OutToGenrule["pre.h"] != "pre_gen" {
		t.Error("restore dropped a pre-checkpoint entry")
	}
	for name, m := range map[string]map[string]string{
		"OutToNativeConsumerDep":    cc.OutToNativeConsumerDep,
		"OutToNativeConsumerPkg":    cc.OutToNativeConsumerPkg,
		"NativeRuleSubPackage":      cc.NativeRuleSubPackage,
		"recognizedConsumerByInput": cc.recognizedConsumerByInput,
		"recognizedNameOwner":       cc.recognizedNameOwner,
		"StampVars":                 cc.StampVars,
		"StampCommands":             cc.StampCommands,
	} {
		if len(m) != 0 {
			t.Errorf("%s not restored to empty: %v", name, m)
		}
	}
	if len(cc.StampKeyCollisions) != 0 {
		t.Errorf("StampKeyCollisions not restored to empty: %v", cc.StampKeyCollisions)
	}
	if len(cc.bakeTodoDisposition) != 0 {
		t.Errorf("bakeTodoDisposition not restored to empty: %v", cc.bakeTodoDisposition)
	}
}
