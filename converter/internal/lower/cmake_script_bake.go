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
func bakeCmakeScriptGenrule(cc *codegenContext, b *ninja.Build, cmd, scriptArg, buildDir string) (relOut, name, reason string, ok bool) {
	if cc.CMakeBinary == "" {
		return "", "", "cmake binary not on PATH at convert time — --cmake-script-bake requires the convert host to have cmake available", false
	}
	outs := genruleOuts(b, buildDir)
	if len(outs) == 0 {
		return "", "", "", false
	}
	dArgs := extractCmakePDashArgs(cmd)

	// Run the script in a fresh tmp dir. Map cmake's
	// CMAKE_BINARY_DIR to it via the workDir so file(WRITE
	// "${CMAKE_BINARY_DIR}/foo") lands inside the sandbox.
	tmpDir, err := os.MkdirTemp("", "cmake-script-bake-*")
	if err != nil {
		return "", "", fmt.Sprintf("mktmpdir: %v", err), false
	}
	defer os.RemoveAll(tmpDir)

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
	argv := []string{"-P", scriptArg}
	argv = append(argv, dArgs...)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	exe := exec.CommandContext(ctx, cc.CMakeBinary, argv...)
	exe.Dir = tmpDir
	exe.Env = []string{
		"HOME=" + tmpDir,
		"LC_ALL=C",
		"PATH=" + os.Getenv("PATH"),
	}
	exe.Stdout = io.Discard
	exe.Stderr = io.Discard
	if err := exe.Run(); err != nil {
		return "", "", fmt.Sprintf("cmake -P %s failed at convert time: %v", scriptArg, err), false
	}

	// Read each declared output's bytes. The script's
	// output-path is typically under the cmake build dir; map
	// from the build-dir-relative `out` back to the script's
	// actual write target. For now, we look for the file at
	// `<tmpDir>/<out>` AND at `<buildDir>/<out>` (cmake may
	// have written to the original build dir if the script's
	// var substitution baked that in).
	type baked struct {
		out, name string
		body      []byte
	}
	var entries []baked
	for _, out := range outs {
		// Try the sandbox-relative location first.
		body, err := os.ReadFile(filepath.Join(tmpDir, out))
		if err != nil {
			// Fall back to the original build dir — cmake-
			// configure-time substitution may have baked the
			// absolute path in.
			body, err = os.ReadFile(filepath.Join(buildDir, out))
			if err != nil {
				return "", "", fmt.Sprintf("cmake -P bake of %q ran but didn't produce output %q (looked in %s and %s): %v",
					scriptArg, out, tmpDir, buildDir, err), false
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
