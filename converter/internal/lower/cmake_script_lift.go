package lower

import (
	"context"
	"fmt"
	"strings"

	"github.com/sstriker/buildstream-bazel/converter/internal/ninja"
	"github.com/sstriker/buildstream-bazel/converter/ir"
)

// liftCmakeScriptGenrule converts a custom-command `cmake -P
// <script> [-D <var>=<val> ...]` invocation into a Bazel genrule
// that runs the operator-staged runner tool at build time:
//
//	genrule(
//	    name = ...,
//	    srcs = [<script>, <ninja-implicit-inputs>...],
//	    outs = [<ninja-edge-outs>...],
//	    cmd  = `$(execpath <runner>) -P "$(execpath <script>)" <preserved-D-args>`,
//	    tools = ["<runner-label>"],
//	    tags = ["cmake-codegen-cmake-script-lift", ...],
//	)
//
// Returns (relOut, name, reason, ok). ok=true on success; reason
// is empty. ok=false means the lift declined; reason carries a
// structured diagnostic for the caller's refusal message
// (empty reason ⇒ the caller's generic message stands).
//
// When CMakeScriptTrace is on, the lift runs the script under
// `cmake --trace --trace-format=json-v1 -P <script>` at convert
// time, classifies every read path (source / build / sysroot /
// unknown), and:
//   - augments srcs with source-class paths the operator's
//     add_custom_command(DEPENDS) didn't capture;
//   - warns about sysroot-class paths (operator-toolchain dep);
//   - refuses with a structured list of unknown / unresolvable
//     build-class paths.
//
// Soundness: cmake -P scripts can read files via cmake's
// `file(READ)` / `include()` / `execute_process()` etc. The
// lift's `srcs` list captures the cmake-declared explicit inputs
// (which cmake reflected as the ninja edge's DEPENDS); anything
// the script reads beyond that fails at action time under
// Bazel's sandbox (the operator's diagnostic). Parameter-driven
// scripts that take their inputs via `-D` args (VTK's
// vtkHashSource shape) work cleanly; configure_file-derived
// scripts with hardcoded absolute paths fail because the paths
// don't exist on the action's sandbox filesystem.
func liftCmakeScriptGenrule(cc *codegenContext, b *ninja.Build, cmd, scriptArg, cmakeSrc, buildDir string) (relOut, name, reason string, ok bool) {
	scriptRel, ok := relativeIfInside(cmakeSrc, scriptArg)
	if !ok {
		return "", "", "script path %q is not under the source root — typical of configure_file-derived scripts whose hardcoded /tmp/build paths won't survive Bazel's sandbox", false
	}
	dArgs := extractCmakePDashArgs(cmd)

	outs := genruleOuts(b, buildDir)
	if len(outs) == 0 {
		return "", "", "", false
	}
	srcs := genruleSrcs(b, cmakeSrc, buildDir, "")
	srcs = appendUnique(srcs, scriptRel)

	// Trace-based dep discovery + path classification. Opt-in
	// via codegenContext.CMakeScriptTrace because actually
	// running the script at convert time has side-effect risk;
	// operators acknowledge the risk by setting the flag. When
	// off, the lift behaves as the pre-trace shape: srcs only
	// reflect the ninja edge's DEPENDS, and script-internal
	// reads fail at action time with a Bazel sandbox miss.
	tags := []string{"cmake-codegen-cmake-script-lift"}
	if cc.CMakeScriptTrace && cc.CMakeBinary != "" {
		traceRaw, err := TraceCmakeScript(context.Background(), cc.CMakeBinary, scriptArg, dArgs)
		if err != nil {
			return "", "", fmt.Sprintf("cmake --trace -P %s failed: %v — convert-time trace required for --cmake-script-trace; rerun without it (sandbox miss may occur at Bazel build time) or fix the script", scriptArg, err), false
		}
		cls := ClassifyScriptTrace(traceRaw, cmakeSrc, buildDir)

		// Unknown paths block the lift. The script touches a
		// path Bazel's sandbox won't reproduce; refusing at
		// convert time is more actionable than a runtime
		// sandbox miss.
		if len(cls.UnknownPaths) > 0 {
			return "", "", fmt.Sprintf("cmake -P script reads %d path(s) outside source/build/sysroot — Bazel's sandbox won't have these:\n  %s",
				len(cls.UnknownPaths), strings.Join(cls.UnknownPaths, "\n  ")), false
		}

		// Build-class reads need a ninja producer to substitute
		// $(location :producer). Skip the cross-reference for
		// now (queued for a follow-up — current shape: refuse
		// unresolved build paths so the diagnostic is honest).
		if len(cls.BuildPaths) > 0 {
			return "", "", fmt.Sprintf("cmake -P script reads %d path(s) under the build dir — these are likely cmake-side configure-time outputs and need explicit producer-lift wiring (not yet implemented):\n  %s",
				len(cls.BuildPaths), strings.Join(cls.BuildPaths, "\n  ")), false
		}

		// Augment srcs with source-class paths the trace found
		// beyond the ninja edge's declared DEPENDS.
		for _, p := range cls.SourcePaths {
			srcs = appendUnique(srcs, p)
		}

		// Sysroot-class: warn but proceed. The operator's
		// runner image must have these paths; tagging makes
		// the assumption visible.
		if len(cls.SysrootPaths) > 0 && cc.Warnings != nil {
			fmt.Fprintf(cc.Warnings, "lower: cmake -P lift of %s assumes sysroot paths exist on the build host:\n", name)
			for _, p := range cls.SysrootPaths {
				fmt.Fprintf(cc.Warnings, "  - %s\n", p)
			}
		}
		tags = append(tags, "cmake-codegen-cmake-script-traced")
	}

	name = genruleNameFor(b, buildDir)
	runnerExec := fmt.Sprintf("$(execpath %s)", cc.CMakeScriptRunner)
	scriptExec := fmt.Sprintf("$(execpath %s)", scriptRel)
	cmdParts := []string{runnerExec, "-P", scriptExec}
	cmdParts = append(cmdParts, dArgs...)
	gen := ir.Target{
		Name:         name,
		Kind:         ir.KindGenrule,
		GenruleCmd:   strings.Join(cmdParts, " "),
		GenruleOuts:  outs,
		Srcs:         srcs,
		GenruleTools: []string{cc.CMakeScriptRunner},
		Tags:         tags,
		Visibility:   []string{"//visibility:private"},
	}
	cc.Genrules = append(cc.Genrules, gen)
	cc.SeenBuilds[b] = name
	for _, o := range outs {
		cc.OutToGenrule[o] = name
	}
	return outs[0], name, "", true
}

// extractCmakePDashArgs walks the recovered command and returns
// the `-D <var>=<value>` arguments cmake -P scripts often take.
// Order is preserved; everything except `cmake`, `-P`, the
// script path, and the -D args is dropped (no place to land
// shell redirects, env-vars, etc. — those make the lift unsafe).
// Returns nil for argless invocations.
func extractCmakePDashArgs(cmd string) []string {
	tokens := splitShellTokens(cmd)
	// Strip a leading `cd <dir> &&` the same way usesCmakeScriptMode does.
	for i, tok := range tokens {
		if tok == "&&" {
			tokens = tokens[i+1:]
			break
		}
	}
	var out []string
	for i := 0; i < len(tokens); i++ {
		tok := tokens[i]
		if tok == "-D" && i+1 < len(tokens) {
			out = append(out, "-D", tokens[i+1])
			i++
			continue
		}
		if strings.HasPrefix(tok, "-D") {
			out = append(out, tok)
			continue
		}
	}
	return out
}

// extractCmakePScriptPositionalArgs walks the recovered command
// and returns the positional arguments that appear AFTER the
// script path (i.e. the args cmake exposes inside the script as
// ${CMAKE_ARGV3}, ${CMAKE_ARGV4}, ...). Excludes `cmake`, `-P`,
// `-D <var>` / `-D<var>` pairs, and the script path itself.
// Order is preserved.
//
// Surfaced by libpng's gensrc.cmake shape: a single script
// reads its first positional arg as a switch (`if(${CMAKE_ARGV3}
// STREQUAL "pnglibconf.h") ...`) and writes one of several
// declared outputs per invocation. extractCmakePDashArgs alone
// drops the switch arg, so the bake invocation runs without the
// dispatch input and the script falls through to its error case.
func extractCmakePScriptPositionalArgs(cmd string) []string {
	tokens := splitShellTokens(cmd)
	for i, tok := range tokens {
		if tok == "&&" {
			tokens = tokens[i+1:]
			break
		}
	}
	// Find the `-P <script>` pair; positional args are everything
	// after that, excluding any further `-D` pairs (those still
	// flow through extractCmakePDashArgs).
	var pIdx = -1
	for i := 0; i < len(tokens); i++ {
		if tokens[i] == "-P" && i+1 < len(tokens) {
			pIdx = i + 1 // script path index
			break
		}
	}
	if pIdx < 0 || pIdx+1 >= len(tokens) {
		return nil
	}
	var out []string
	for i := pIdx + 1; i < len(tokens); i++ {
		tok := tokens[i]
		if tok == "-D" && i+1 < len(tokens) {
			i++
			continue
		}
		if strings.HasPrefix(tok, "-D") {
			continue
		}
		out = append(out, tok)
	}
	return out
}

// appendUnique appends entries to a slice, dropping duplicates of
// values already present. Stable order: keeps the first-seen
// position.
func appendUnique(slice []string, entries ...string) []string {
	seen := make(map[string]struct{}, len(slice)+len(entries))
	for _, s := range slice {
		seen[s] = struct{}{}
	}
	for _, e := range entries {
		if e == "" {
			continue
		}
		if _, ok := seen[e]; ok {
			continue
		}
		seen[e] = struct{}{}
		slice = append(slice, e)
	}
	return slice
}
