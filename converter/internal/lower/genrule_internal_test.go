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

// TestSplitShellTokens covers the small shell-style tokenizer
// used by extractDriver / usesCmakeScriptMode. Not POSIX-complete
// by design — only the shapes CMake's CUSTOM_COMMAND emits. The
// hazard is wrong splits silently corrupting the genrule cmd:
// an unbalanced quote or unescaped space changes argv0 and
// reroutes audit-tag classification (cmake-codegen-driver=...).
func TestSplitShellTokens(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{name: "empty", in: "", want: nil},
		{name: "single bare token", in: "cmake", want: []string{"cmake"}},
		{
			name: "multiple bare tokens with mixed whitespace",
			in:   "cmake  -E\ttouch /build/marker",
			want: []string{"cmake", "-E", "touch", "/build/marker"},
		},
		{
			name: "double-quoted argument with embedded space",
			in:   `cmake -DMSG="hello world" foo`,
			want: []string{"cmake", "-DMSG=hello world", "foo"},
		},
		{
			name: "single-quoted argument with embedded space",
			in:   `sh -c 'echo hi there'`,
			want: []string{"sh", "-c", "echo hi there"},
		},
		{
			name: "backslash-escape of space outside quotes",
			in:   `cp /tmp/a\ b /dest`,
			want: []string{"cp", "/tmp/a b", "/dest"},
		},
		{
			name: "backslash-escape inside double quotes",
			in:   `echo "a\"b"`,
			want: []string{"echo", `a"b`},
		},
		{
			name: "trailing backslash with no follower is dropped silently",
			in:   `cmd \`,
			want: []string{"cmd"},
		},
		{
			name: "consecutive quoted segments concatenate (no separating space)",
			in:   `"a""b"`,
			want: []string{"ab"},
		},
		{
			name: "leading and trailing whitespace",
			in:   "  cmake   ",
			want: []string{"cmake"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := splitShellTokens(tc.in)
			if !equalStrings(got, tc.want) {
				t.Errorf("splitShellTokens(%q) = %#v, want %#v", tc.in, got, tc.want)
			}
		})
	}
}

// TestExtractDriver covers the cmake-codegen-driver=X audit-tag
// derivation. Wrong classification means narrowing-audit allowlists
// don't match — operator-facing hazard. Exercises:
//   - bare argv0 with and without `cd <dir> &&` prefix,
//   - env-wrapper stripping (env KEY=V -u FLAG ... real_cmd),
//   - sh/bash wrappers retain their name (M2-audit hook),
//   - fallback to "unknown" when nothing resolves.
func TestExtractDriver(t *testing.T) {
	cases := []struct {
		name string
		cmd  string
		want string
	}{
		{name: "empty falls back to unknown", cmd: "", want: "unknown"},
		{name: "bare cmake", cmd: "cmake -E touch /build/x", want: "cmake"},
		{
			name: "absolute path argv0 uses basename",
			cmd:  "/usr/bin/python3 /src/scripts/gen.py",
			want: "python3",
		},
		{
			name: "cd-prefixed cmake",
			cmd:  "cd /build && /usr/bin/cmake -E touch /build/x",
			want: "cmake",
		},
		{
			name: "env wrapper with KEY=VAL is skipped",
			cmd:  "env SOURCE_DATE_EPOCH=0 /usr/bin/cmake -E touch /build/x",
			want: "cmake",
		},
		{
			name: "env wrapper with multiple KEY=VAL pairs is skipped",
			cmd:  "env LANG=C SOURCE_DATE_EPOCH=0 /usr/bin/python3 gen.py",
			want: "python3",
		},
		{
			// `-i` is a no-argument env flag (clear environment).
			// The wrapper-skip heuristic strips it as a leading flag.
			name: "env -i with no env pairs strips to driver",
			cmd:  "env -i /usr/bin/python3 gen.py",
			want: "python3",
		},
		{
			// Chained wrappers (nice + env). nice gets stripped first,
			// then env gets stripped via the same wrapper loop.
			name: "chained nice + env wrappers",
			cmd:  "nice env KEY=v /usr/bin/cmake -E touch /build/x",
			want: "cmake",
		},
		{
			name: "bash wrapper followed by absolute script path",
			cmd:  "bash /usr/bin/script.sh arg1",
			want: "script.sh",
		},
		{
			name: "cd-prefix without && remains in driver position",
			// `cd /build cmake` has no ` && ` so the prefix-strip
			// doesn't fire; the literal `cd` becomes the driver.
			cmd:  "cd /build cmake -E touch x",
			want: "cd",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := extractDriver(tc.cmd)
			if got != tc.want {
				t.Errorf("extractDriver(%q) = %q, want %q", tc.cmd, got, tc.want)
			}
		})
	}
}

// TestNormalizeInput covers the three-way path-normalization for
// genrule srcs: cmakeSrc-relative wins over buildDir-relative
// wins over basename fallback. Same family as #192's leak — if
// a sibling helper has the same bug, it'd ship wrong bytes to
// genrule srcs. Pins the priority order plus the rare-and-noisy
// basename fallback that flags an under-qualified entry.
func TestNormalizeInput(t *testing.T) {
	const (
		cmakeSrc = "/src/project"
		buildDir = "/tmp/build-1234"
	)
	cases := []struct {
		name     string
		in       string
		cmakeSrc string
		buildDir string
		want     string
	}{
		{
			name:     "relative path passes through as slash form",
			in:       "pkg/foo.h",
			cmakeSrc: cmakeSrc,
			buildDir: buildDir,
			want:     "pkg/foo.h",
		},
		{
			name:     "absolute path under cmakeSrc returns cmakeSrc-relative",
			in:       "/src/project/pkg/foo.cpp",
			cmakeSrc: cmakeSrc,
			buildDir: buildDir,
			want:     "pkg/foo.cpp",
		},
		{
			name:     "absolute path under buildDir returns buildDir-relative",
			in:       "/tmp/build-1234/gen/version.h",
			cmakeSrc: cmakeSrc,
			buildDir: buildDir,
			want:     "gen/version.h",
		},
		{
			name: "absolute path under neither falls back to basename",
			// /etc/passwd-shaped host leak — basename is the
			// documented refusal-flavored fallback.
			in:       "/etc/passwd",
			cmakeSrc: cmakeSrc,
			buildDir: buildDir,
			want:     "passwd",
		},
		{
			name:     "cmakeSrc wins when path is under both",
			in:       "/src/project/sub/foo.h",
			cmakeSrc: "/src/project",
			buildDir: "/src/project/sub", // pathologically nested
			want:     "sub/foo.h",
		},
		{
			name:     "empty cmakeSrc skips the cmakeSrc branch",
			in:       "/tmp/build-1234/gen.h",
			cmakeSrc: "",
			buildDir: buildDir,
			want:     "gen.h",
		},
		{
			name:     "empty cmakeSrc and buildDir falls all the way through to basename",
			in:       "/abs/path/foo.h",
			cmakeSrc: "",
			buildDir: "",
			want:     "foo.h",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := normalizeInput(tc.in, tc.cmakeSrc, tc.buildDir)
			if got != tc.want {
				t.Errorf("normalizeInput(%q, %q, %q) = %q, want %q",
					tc.in, tc.cmakeSrc, tc.buildDir, got, tc.want)
			}
		})
	}
}

func equalStrings(a, b []string) bool {
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
