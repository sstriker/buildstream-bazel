package lower

import (
	"errors"
	"strings"
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/internal/failure"
	"github.com/sstriker/buildstream-bazel/converter/internal/ninja"
)

func TestUsesCmakeScriptMode(t *testing.T) {
	cases := []struct {
		name string
		cmd  string
		want bool
	}{
		{
			name: "bare /usr/bin/cmake -P",
			cmd:  "/usr/bin/cmake -P /src/scripts/gen.cmake /build/x.h",
			want: true,
		},
		{
			name: "${CMAKE_COMMAND} -P",
			cmd:  "${CMAKE_COMMAND} -P /src/scripts/gen.cmake",
			want: true,
		},
		{
			name: "cd prefix + /usr/bin/cmake -P",
			cmd:  "cd /build && /usr/bin/cmake -P /src/scripts/gen.cmake",
			want: true,
		},
		{
			name: "cd prefix + cmake -D... -P (libpng pnglibconf shape)",
			cmd:  "cd /build && /usr/bin/cmake -DOUTPUT=pnglibconf.h -P /build/scripts/gensrc.cmake",
			want: true,
		},
		{
			name: "cmake -D + -P with bare cmake name",
			cmd:  "cmake -DFOO=bar -DBAZ=qux -P /tmp/build/script.cmake",
			want: true,
		},
		{
			name: "env wrapper + cmake -P",
			cmd:  "env SOURCE_DATE_EPOCH=0 /usr/bin/cmake -P /src/scripts/gen.cmake",
			want: true,
		},
		{
			name: "cmake -E (not script mode)",
			cmd:  "/usr/bin/cmake -E touch /build/marker",
			want: false,
		},
		{
			name: "non-cmake driver",
			cmd:  "/usr/bin/python3 scripts/gen.py /build/x.h",
			want: false,
		},
		{
			name: "cd-prefixed non-cmake driver",
			cmd:  "cd /build && /usr/bin/python3 scripts/gen.py",
			want: false,
		},
		{
			name: "cmake without -P",
			cmd:  "/usr/bin/cmake --build /build --target foo",
			want: false,
		},
		{
			name: "cmake with -P-like flag value (not the script-mode flag)",
			cmd:  "/usr/bin/cmake -DOPTION=-P /src/scripts/gen.cmake",
			want: false,
		},
		{
			// Locks that iteration past the `-P` token doesn't matter once
			// the script-mode signal is observed — additional flags after
			// the script path (cmake honours `--debug-output` as a
			// cache-affecting flag in script mode) must still trip the
			// refusal.
			name: "cmake -P with trailing flag",
			cmd:  "/usr/bin/cmake -P /build/scripts/gen.cmake --debug-output",
			want: true,
		},
		{
			name: "empty",
			cmd:  "",
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := usesCmakeScriptMode(tc.cmd)
			if got != tc.want {
				t.Errorf("usesCmakeScriptMode(%q) = %v, want %v", tc.cmd, got, tc.want)
			}
		})
	}
}

// TestGenruleNameFor_StripsBuildDir is the regression test for
// issue #192: when cmake writes an absolute path into build.ninja
// (`<buildDir>/pkg/gen/output.cpp`), the genrule name derivation
// must relativize against buildDir BEFORE sanitization. Otherwise
// the buildDir's per-run temp suffix (e.g.
// `/tmp/convert-element-build-1806770363/`) leaks into the rule
// name and makes BUILD.bazel non-deterministic across runs of
// convert-element-cmake on the same package.
//
// The bug shape pre-fix: two runs of the converter against the
// same package produced different rule names like
// `gen__tmp_convert_element_build_1806770363_pkg_gen_output_cpp`
// vs `gen__tmp_convert_element_build_999_pkg_gen_output_cpp` —
// breaking srckey stability and downstream consumer references.
func TestGenruleNameFor_StripsBuildDir(t *testing.T) {
	const buildDir = "/tmp/convert-element-build-1806770363"
	cases := []struct {
		name    string
		outputs []string
		want    string
		// Substrings the result must NOT contain — pins the bug
		// is no longer reachable (buildDir suffix doesn't appear
		// in the rule name).
		notSubstring []string
	}{
		{
			name:    "absolute path under buildDir",
			outputs: []string{buildDir + "/pkg/gen/output.cpp"},
			want:    "gen_pkg_gen_output_cpp",
			// Tmp-suffix proof: the random digit run from
			// buildDir must NOT appear in the rule name.
			notSubstring: []string{"1806770363", "convert_element_build", "tmp"},
		},
		{
			name:    "relative path stays as-is",
			outputs: []string{"pkg/gen/output.cpp"},
			want:    "gen_pkg_gen_output_cpp",
		},
		{
			name:    "absolute outside buildDir falls through verbatim",
			outputs: []string{"/some/other/dir/foo.h"},
			// Outside-buildDir paths can't be safely
			// relativized; sanitization runs on the raw absolute
			// path. Stable across runs as long as the path
			// itself is.
			want: "gen__some_other_dir_foo_h",
		},
		{
			name:    "no outputs uses fallback",
			outputs: nil,
			want:    "gen_out",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := &ninja.Build{Outputs: tc.outputs}
			got := genruleNameFor(b, buildDir)
			if got != tc.want {
				t.Errorf("genruleNameFor(%v, %q) = %q, want %q", tc.outputs, buildDir, got, tc.want)
			}
			for _, banned := range tc.notSubstring {
				if strings.Contains(got, banned) {
					t.Errorf("genruleNameFor result %q must NOT contain %q (buildDir leakage — issue #192)", got, banned)
				}
			}
		})
	}
}

// TestGenruleNameFor_DeterministicAcrossBuildDirs is the
// stability proof for issue #192: the SAME ninja output produced
// by two different buildDirs (simulating two runs of the
// converter where cmake picks a different tmpdir each time)
// must yield the SAME rule name. Without the fix, the two
// names diverge — the bug's user-facing symptom.
func TestGenruleNameFor_DeterministicAcrossBuildDirs(t *testing.T) {
	// Same relative output under two different buildDirs.
	b1 := &ninja.Build{Outputs: []string{"/tmp/convert-element-build-AAA/pkg/foo.cpp"}}
	b2 := &ninja.Build{Outputs: []string{"/tmp/convert-element-build-BBB/pkg/foo.cpp"}}

	n1 := genruleNameFor(b1, "/tmp/convert-element-build-AAA")
	n2 := genruleNameFor(b2, "/tmp/convert-element-build-BBB")

	if n1 != n2 {
		t.Fatalf("rule names diverge across buildDir runs (issue #192):\n  run-A: %q\n  run-B: %q", n1, n2)
	}
}

// TestRecoverGenrule_RefusesEmptyCmd is the regression test for
// issue #193: when ninja.CommandFor resolves a CUSTOM_COMMAND's
// command binding to an empty string (rule's `command` is
// literally empty, or expands to nothing), recoverGenrule must
// refuse with a typed Tier-1 failure — NOT emit a genrule with
// `cmd = ""`, which Bazel would reject at build time with
// "declared output was not created by genrule".
//
// Both source-only outputs (the issue's reproduction) and
// non-source-only outputs hit the same Bazel-side rejection;
// the gate is on the empty cmd alone, not narrowed by
// isSourceOnly. Two sub-tests pin both shapes.
func TestRecoverGenrule_RefusesEmptyCmd(t *testing.T) {
	cases := []struct {
		name     string
		ninjaSrc string
	}{
		{
			name: "empty cmd with source-only output (issue #193 repro)",
			ninjaSrc: `rule CUSTOM_COMMAND
  command = $COMMAND

build /build/pkg/gen/output.cpp: CUSTOM_COMMAND
  COMMAND =
`,
		},
		{
			name: "empty cmd with non-source output (same Bazel rejection)",
			ninjaSrc: `rule CUSTOM_COMMAND
  command = $COMMAND

build /build/version.h: CUSTOM_COMMAND
  COMMAND =
`,
		},
		{
			name: "whitespace-only cmd (same effect as empty after TrimSpace)",
			ninjaSrc: `rule CUSTOM_COMMAND
  command = $COMMAND

build /build/x.cpp: CUSTOM_COMMAND
  COMMAND =
`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g, err := ninja.Parse(strings.NewReader(tc.ninjaSrc), "", nil)
			if err != nil {
				t.Fatalf("ninja.Parse: %v", err)
			}
			cc := newCodegenContext()
			// recoverGenrule's first positional is the generated
			// source path the consumer references; pull it from
			// the parsed ninja so the test stays in lockstep
			// with the fixture above.
			var srcPath string
			for _, b := range g.Builds {
				if len(b.Outputs) > 0 {
					srcPath = b.Outputs[0]
					break
				}
			}
			if srcPath == "" {
				t.Fatal("no build statement parsed; fixture malformed")
			}
			_, _, err = cc.recoverGenrule(srcPath, "/src", "/build", g)
			if err == nil {
				t.Fatal("expected typed Tier-1 error on empty-cmd CUSTOM_COMMAND; got nil")
			}
			// Pin that it's the UnsupportedCustomCommand typed
			// failure shape — not some other error class that
			// would mean we're tripping on a different gate.
			var f *failure.Error
			if !errors.As(err, &f) {
				t.Fatalf("expected *failure.Error, got %T: %v", err, err)
			}
			if f.Code != failure.UnsupportedCustomCommand {
				t.Errorf("failure.Code = %v, want UnsupportedCustomCommand", f.Code)
			}
			// And — the user-facing-critical assertion — NO
			// genrule was synthesized into cc.Genrules. The bug
			// was that one WAS appended, with the empty-cmd
			// body, and would then land in BUILD.bazel.
			if len(cc.Genrules) != 0 {
				for _, gen := range cc.Genrules {
					t.Errorf("refusal should NOT synthesize a genrule; got %+v (issue #193)", gen)
				}
			}
		})
	}
}
