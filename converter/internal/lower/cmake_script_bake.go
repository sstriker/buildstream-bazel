package lower

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/sstriker/buildstream-bazel/converter/internal/ninja"
	"github.com/sstriker/buildstream-bazel/converter/ir"
)

// bakeCmakeScriptGenrule executes `cmake -P <script>` at convert
// time, captures the script's declared OUTPUT files, and emits
// one genrule per output whose cmd materializes the baked bytes
// (base64-decode + write). Closes the hardcoded-absolute-paths
// gap that --cmake-script-runner alone can't: the script's
// internal paths only have to resolve at convert time (where
// they do — the script was authored against the convert host's
// layout); the resulting bytes ship as static data Bazel
// reproduces verbatim.
//
// Trade-off — and the reason this is opt-in via
// --cmake-script-bake:
//
//   - Outputs are convert-time-baked. Re-running cmake's
//     upstream input change won't re-run the script at Bazel
//     time; the operator has to re-convert. This is the same
//     trade-off as the legacy file(GENERATE) and configure_file
//     fallback captures, with the same warning surface (the
//     warnConvertTimeBaking post-pass picks up the
//     cmake-codegen-cmake-script-bake tag).
//
//   - Convert-time execution carries side-effect risk. The lift
//     runs in a fresh os.MkdirTemp workDir to contain
//     file(WRITE) calls; `execute_process(COMMAND rm -rf /...)`
//     would still execute for real. Opt-in flag is the gate.
//
// Returns (relOut, name, reason, ok). ok=true on a clean bake;
// reason carries a structured diagnostic on failure (cmake
// non-zero exit, missing output files, etc.) that the caller
// surfaces in the refusal message.
func bakeCmakeScriptGenrule(cc *codegenContext, b *ninja.Build, cmd, scriptArg, buildDir string, g *ninja.Graph) (relOut, name, reason string, ok bool) {
	if cc.CMakeBinary == "" {
		return "", "", "cmake binary not on PATH at convert time — --cmake-script-bake requires the convert host to have cmake available", false
	}
	outs := genruleOuts(b, buildDir)
	if len(outs) == 0 {
		return "", "", "", false
	}

	// Producer-chain pre-bake: libpng's genchk.cmake reads
	// pnglibconf.out which is produced by another cmake -P build
	// statement (genout.cmake), which in turn reads pnglibconf.c
	// from a third (gensrc.cmake -DOUTPUT=pnglibconf.c). The
	// scripts read inputs from `${BINDIR}/<input>` — the build dir
	// absolute path baked into the configure-substituted scripts.
	// cmake configure only emits the scripts; the intermediate
	// outputs aren't on disk yet. Walk this build's Inputs and
	// recursively bake any input that's itself produced by a
	// CUSTOM_COMMAND so the file lands at `${BINDIR}/<input>`
	// before this script runs.
	//
	// The recursion preserves cc.SeenBuilds, so two consumers of
	// the same producer share one bake. baking same producer
	// twice is correct (idempotent) but wasteful. Cycle detection
	// is implicit: SeenBuilds[producer] is set before bake
	// recurses, so a cycle (impossible in a valid ninja graph
	// but defensive) terminates on the second visit.
	if g != nil {
		for _, in := range b.Inputs {
			if reason, ok := bakeProducerChain(cc, g, in, buildDir); !ok {
				return "", "", "producer-chain bake of input " + in + ": " + reason, false
			}
		}
		for _, in := range b.ImplicitInputs {
			if reason, ok := bakeProducerChain(cc, g, in, buildDir); !ok {
				return "", "", "producer-chain bake of implicit input " + in + ": " + reason, false
			}
		}
	}

	dArgs := extractCmakePDashArgs(cmd)
	// Positional args after the script (libpng's gensrc.cmake
	// shape: `cmake -P gensrc.cmake <output-name>` — the script
	// reads ${CMAKE_ARGV3} as a dispatch switch and writes one
	// of several declared outputs per invocation). Without
	// forwarding these the script sees no dispatch input and the
	// bake either falls through to its error case or produces
	// the wrong output.
	posArgs := extractCmakePScriptPositionalArgs(cmd)

	// Working directory: prefer the cmake build dir itself.
	// configure-time-substituted scripts (libpng's gensrc.cmake
	// shape) bake `${BINDIR}` as an absolute path equal to the
	// cmake build dir and bridge between absolute-${BINDIR}
	// writes (via execute_process WORKING_DIRECTORY=${BINDIR})
	// and $CWD-relative file(RENAME)/file(READ), assuming the
	// two are the same. A tmpDir $CWD breaks that bridge — awk
	// output lands in the real build dir but the cmake-side
	// rename reads from tmpDir and fails.
	//
	// Running in buildDir means bake's side effects live in the
	// operator's cmake build dir at convert time. That's an
	// acceptable trade-off — bake is opt-in via --cmake-script-bake
	// and the buildDir is transient (cmake's own output). We still
	// record outputs in cc.Genrules so the rendered BUILD.bazel
	// is self-contained.
	//
	// Falls back to a tmpDir when buildDir is empty (the unit-test
	// shape that doesn't have a real cmake build dir on hand).
	var workDir string
	if buildDir != "" {
		workDir = buildDir
	} else {
		tmpDir, err := os.MkdirTemp("", "cmake-script-bake-*")
		if err != nil {
			return "", "", fmt.Sprintf("mktmpdir: %v", err), false
		}
		defer os.RemoveAll(tmpDir)
		workDir = tmpDir
	}

	// cmake -P doesn't set CMAKE_BINARY_DIR / CMAKE_SOURCE_DIR
	// itself (those are configure-time variables). Many cmake -P
	// scripts assume CMAKE_BINARY_DIR is the cmake-side build
	// dir at the time the script was generated — for
	// configure_file-derived scripts, the var was substituted
	// in already. For parameter-driven scripts (VTK shape), the
	// caller passes -D for everything the script needs.
	//
	// We pass through ONLY the -D args from the recovered cmd
	// and leave cmake's environment minimal. Scripts that need
	// more than that (the convert-machine BUILD_DIR equivalent)
	// won't bake cleanly — that's the same limitation as the
	// non-bake lift, surfaced honestly here.
	// Arg-ordering matters: cmake parses `-D <var>=<val>` only
	// when it appears BEFORE `-P <script>`. If `-D` comes after
	// `-P <script>`, cmake exposes it inside the script as a
	// positional ${CMAKE_ARGV*} rather than setting the variable,
	// so any `if(VAR STREQUAL ...)` dispatch (libpng's gensrc.cmake
	// shape: `if(OUTPUT STREQUAL "pnglibconf.h") ...`) sees an
	// unset variable and falls through to the script's error
	// branch. The recovered ninja COMMAND already places -D first;
	// preserve that ordering here.
	argv := append([]string{}, dArgs...)
	argv = append(argv, "-P", scriptArg)
	argv = append(argv, posArgs...)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	exe := exec.CommandContext(ctx, cc.CMakeBinary, argv...)
	exe.Dir = workDir
	exe.Env = []string{
		"HOME=" + workDir,
		"LC_ALL=C",
		"PATH=" + os.Getenv("PATH"),
	}
	exe.Stdout = io.Discard
	exe.Stderr = io.Discard
	if err := exe.Run(); err != nil {
		return "", "", fmt.Sprintf("cmake -P %s failed at convert time: %v", scriptArg, err), false
	}

	// Read each declared output's bytes. The script's
	// output-path is typically under the cmake build dir; we
	// look in workDir first (the directory cmake actually ran
	// in — either buildDir or a fresh tmp dir for unit tests)
	// and fall back to buildDir if the script's path
	// substitution wrote elsewhere.
	type baked struct {
		out, name string
		body      []byte
	}
	var entries []baked
	for _, out := range outs {
		// Try the workDir-relative location first.
		body, err := os.ReadFile(filepath.Join(workDir, out))
		if err != nil && workDir != buildDir {
			// Fall back to the original build dir when workDir
			// is a tmpDir — cmake-configure-time substitution
			// may have baked the absolute path in.
			body, err = os.ReadFile(filepath.Join(buildDir, out))
			if err != nil {
				return "", "", fmt.Sprintf("cmake -P bake of %q ran but didn't produce output %q (looked in %s and %s): %v",
					scriptArg, out, workDir, buildDir, err), false
			}
		}
		entries = append(entries, baked{
			out:  out,
			name: genruleNameFor(b, buildDir) + "_" + sanitizeForName(filepath.Base(out)),
			body: body,
		})
	}

	// One genrule per declared output; cmd materializes the
	// baked bytes via base64-decode.
	for i, e := range entries {
		encoded := base64.StdEncoding.EncodeToString(e.body)
		gen := ir.Target{
			Name:        e.name,
			Kind:        ir.KindGenrule,
			GenruleCmd:  fmt.Sprintf(`echo %q | base64 -d > $@`, encoded),
			GenruleOuts: []string{e.out},
			Tags: []string{
				"cmake-codegen-cmake-script-bake",
				"cmake-codegen-cmake-script-lift", // for the existing bake-warning shape
			},
			Visibility: []string{"//visibility:private"},
		}
		cc.Genrules = append(cc.Genrules, gen)
		cc.OutToGenrule[e.out] = e.name
		if i == 0 {
			relOut = e.out
			name = e.name
		}
	}
	cc.SeenBuilds[b] = name
	return relOut, name, "", true
}

// bakeProducerChain recurses to bake any CUSTOM_COMMAND build
// statement that produces inputPath, so the input file lands at
// `${BINDIR}/<inputPath>` before the consumer's bake invocation
// reads it. Quietly returns ok=true for inputs that aren't
// CUSTOM_COMMAND outputs (regular source files, phony aliases,
// inputs from other build statements that don't use cmake -P).
// Returns ok=false + a structured reason only when the recursive
// bake of a cmake -P producer fails — surfaces that up the chain
// so the original refusal message names the precise upstream
// failure.
func bakeProducerChain(cc *codegenContext, g *ninja.Graph, inputPath, buildDir string) (string, bool) {
	producer := g.BuildFor(inputPath)
	if producer == nil {
		// Not produced by any ninja build — source file or
		// out-of-graph external. Nothing to do.
		return "", true
	}
	if producer.Rule != "CUSTOM_COMMAND" {
		// Object files, etc. Not a script we can bake.
		return "", true
	}
	if _, seen := cc.SeenBuilds[producer]; seen {
		// Already baked (or recursion-in-progress sentinel).
		return "", true
	}
	cmd, ok := ninja.CommandFor(g, producer)
	if !ok || strings.TrimSpace(cmd) == "" {
		// Producer's command isn't resolvable. Don't fail —
		// the consumer's bake will surface the missing file
		// with its own diagnostic, which is more informative
		// for operators.
		return "", true
	}
	if !usesCmakeScriptMode(cmd) {
		// Producer isn't a cmake -P invocation (could be a
		// COMPILE rule, a copy, etc.). Out of bake's scope.
		return "", true
	}
	script := extractCmakeScriptPath(cmd)
	// Mark in-progress to break any pathological cycles (a valid
	// ninja graph is acyclic, but defensive).
	cc.SeenBuilds[producer] = ""
	_, _, reason, ok := bakeCmakeScriptGenrule(cc, producer, cmd, script, buildDir, g)
	if !ok {
		delete(cc.SeenBuilds, producer)
		return reason, false
	}
	return "", true
}

// sanitizeForName replaces non-identifier chars with `_` so the
// generated genrule names are valid Bazel labels. Mirrors the
// pattern genruleNameFor uses; kept local to bake to avoid
// reaching across files.
func sanitizeForName(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9',
			r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	return b.String()
}
