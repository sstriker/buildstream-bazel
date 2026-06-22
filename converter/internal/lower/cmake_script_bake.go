package lower

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/sstriker/buildstream-bazel/converter/internal/ninja"
)

// extractCdDir returns the directory from a leading `cd <dir> && …` prefix on a
// ninja-emitted command (cmake's custom-command WORKING_DIRECTORY), or "" when
// the command doesn't start with one. Handles a quoted or bare dir token.
func extractCdDir(cmd string) string {
	cmd = strings.TrimSpace(cmd)
	if !strings.HasPrefix(cmd, "cd ") {
		return ""
	}
	i := strings.Index(cmd, " && ")
	if i < 0 {
		return ""
	}
	dir := strings.TrimSpace(cmd[len("cd "):i])
	dir = strings.Trim(dir, `"'`)
	return dir
}

// dirExists reports whether p is an existing directory.
func dirExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

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
// Returns (name, reason, ok) — name is the genrule for the FIRST declared
// output; every output (explicit and implicit alike, per genruleOuts) gets
// its own baked target registered in cc.OutToGenrule, and callers map a
// consumer to the specific output it requested via that index. ok=true on
// a clean bake; reason carries a structured diagnostic on failure (cmake
// non-zero exit, missing output files, etc.) that the caller surfaces in
// the refusal message.
// readFirstExisting returns the bytes of out (a slash-form relative path) read
// from the first root under which it exists, or found=false when no root has it.
// Used by the bake to resolve a declared output against whichever build dir
// physically owns it (the nested build dir, or an ancestor outer one for the
// cross-boundary shape).
func readFirstExisting(roots []string, out string) (body []byte, found bool) {
	for _, root := range roots {
		if b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(out))); err == nil {
			return b, true
		}
	}
	return nil, false
}

func bakeCmakeScriptGenrule(cc *codegenContext, b *ninja.Build, cmd, scriptArg, buildDir string, g *ninja.Graph, declaredOuts []string) (name, reason string, ok bool) {
	if cc.CMakeBinary == "" {
		return "", "cmake binary not on PATH at convert time — --cmake-script-bake requires the convert host to have cmake available", false
	}
	// declaredOuts overrides the edge's own output (the nested-recipe recovery's
	// STABLE gen sources, which the recipe `.cmake` edge writes as undeclared side
	// outputs). The script still runs to produce them; the bake then reads the gen
	// sources' bytes (not the recipe's) and registers THEM in cc.OutToGenrule.
	outs := declaredOuts
	if len(outs) == 0 {
		outs = genruleOuts(b, buildDir)
	}
	if len(outs) == 0 {
		// Ninja edge declared no outputs — recover them from the script's own
		// write statements, resolving ${VAR} against the command's -D args (the
		// libpng `gensrc.cmake -DOUTPUT=pnglibconf.c` shape noted below, and the
		// VTK -DSCRIPT_OUT= shape). cmakeSrc isn't in scope here; generated bake
		// outputs land in the build dir, which discoverCmakeScriptOutputs resolves
		// without it. Still empty ⇒ decline as before (the bake then can't know
		// what bytes to capture).
		outs = discoverCmakeScriptOutputs(scriptArg, extractCmakePDashArgs(cmd), buildDir, "")
		if len(outs) == 0 {
			return "", "", false
		}
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
				return "", "producer-chain bake of input " + in + ": " + reason, false
			}
		}
		for _, in := range b.ImplicitInputs {
			if reason, ok := bakeProducerChain(cc, g, in, buildDir); !ok {
				return "", "producer-chain bake of implicit input " + in + ": " + reason, false
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
	// Honor the recovered command's WORKING_DIRECTORY. cmake's Ninja generator
	// emits custom commands as `cd <WORKING_DIRECTORY> && cmake … -P script`,
	// and some scripts depend on that cwd: VTK's libproj generate_proj_db.cmake
	// runs from the data SOURCE dir so `include(sql_filelist.cmake)` and its
	// `${CMAKE_CURRENT_SOURCE_DIR}/sql/*.sql` paths resolve. Running from buildDir
	// (the default) breaks that relative include. When the cd-dir exists, prefer
	// it; it equals buildDir for the libpng ${BINDIR}-bridge shape (those custom
	// commands cd into the build subdir), so this is safe for both.
	var workDir string
	if cd := extractCdDir(cmd); cd != "" && dirExists(cd) {
		workDir = cd
	} else if buildDir != "" {
		workDir = buildDir
	} else {
		tmpDir, err := os.MkdirTemp("", "cmake-script-bake-*")
		if err != nil {
			return "", fmt.Sprintf("mktmpdir: %v", err), false
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

	// The parallel pre-warm (prewarmScriptBakes) may already have run this
	// exact build's script — consult the per-build result cache before
	// paying for a serial run. The conversion-latency profile showed these
	// sequential cmake -P waits dominating large converts' translation wall
	// time (VTK: 238 runs ≈ 95s of a 126s multi-config translation).
	if res, hit := cc.ScriptBakeRuns[b]; hit {
		if res != nil {
			return "", fmt.Sprintf("cmake -P %s failed at convert time: %v", scriptArg, res), false
		}
	} else if err := runScriptExec(cc.CMakeBinary, argv, workDir); err != nil {
		return "", fmt.Sprintf("cmake -P %s failed at convert time: %v", scriptArg, err), false
	}

	// Read each declared output's bytes. The script's
	// output-path is typically under the cmake build dir; we
	// look in workDir first (the directory cmake actually ran
	// in — either buildDir or a fresh tmp dir for unit tests)
	// and fall back to buildDir if the script's path
	// substitution wrote elsewhere.
	// Candidate roots for a declared output, in order: the dir cmake ran in,
	// the build dir, then ancestor (outer) build dirs. The last covers the
	// CROSS-BOUNDARY shape — the recipe's gen source is declared relative to an
	// OUTER build dir (genSrcRelToOwningBuild) because the nested script wrote it
	// up there, but the script ran in this nested build dir. Reading only
	// workDir/buildDir missed it; and when workDir == buildDir the prior
	// fallback's error check was unreachable, so a failed read silently baked an
	// EMPTY file. Try every owning root and decline cleanly if none has it.
	roots := []string{workDir}
	if buildDir != "" && buildDir != workDir {
		roots = append(roots, buildDir)
	}
	for _, ob := range cc.OuterBuildDirs {
		if ob != "" {
			roots = append(roots, ob)
		}
	}
	type baked struct {
		out, name string
		body      []byte
	}
	var entries []baked
	for _, out := range outs {
		body, found := readFirstExisting(roots, out)
		if !found {
			return "", fmt.Sprintf("cmake -P bake of %q ran but didn't produce output %q (looked in %v)",
				scriptArg, out, roots), false
		}
		entries = append(entries, baked{
			out:  out,
			name: genruleNameFor(b, buildDir) + "_" + sanitizeForName(filepath.Base(out)),
			body: body,
		})
	}

	// One target per declared output; the shared bakeFileTarget chooser
	// materializes the baked bytes as a readable skylib write_file for
	// \n-only text (the common case — generated headers like
	// pnglibconf.h) and falls back to the byte-exact base64 genrule for
	// binary / control-byte / CRLF outputs.
	bakeTags := []string{
		"cmake-codegen-cmake-script-bake",
		"cmake-codegen-cmake-script-lift", // for the existing bake-warning shape
	}
	for i, e := range entries {
		gen := bakeFileTarget(e.name, e.out, e.body, bakeTags)
		cc.Genrules = append(cc.Genrules, gen)
		cc.OutToGenrule[e.out] = e.name
		if i == 0 {
			name = e.name
		}
	}
	cc.SeenBuilds[b] = name
	return name, "", true
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
	_, reason, ok := bakeCmakeScriptGenrule(cc, producer, cmd, script, buildDir, g, nil)
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
