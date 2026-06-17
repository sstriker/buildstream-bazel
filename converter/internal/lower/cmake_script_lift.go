package lower

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
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
// Returns (name, reason, ok) — every ninja-edge output is declared
// in the genrule's outs and registered in cc.OutToGenrule, so the
// caller maps each consumer to the specific output it requested via
// that index. ok=true on success; reason is empty. ok=false means
// the lift declined; reason carries a structured diagnostic for the
// caller's refusal message (empty reason ⇒ the caller's generic
// message stands).
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
func liftCmakeScriptGenrule(cc *codegenContext, b *ninja.Build, cmd, scriptArg, cmakeSrc, buildDir string) (name, reason string, ok bool) {
	scriptRel, ok := relativeIfInside(cmakeSrc, scriptArg)
	if !ok {
		return "", "script path %q is not under the source root — typical of configure_file-derived scripts whose hardcoded /tmp/build paths won't survive Bazel's sandbox", false
	}
	dArgs := extractCmakePDashArgs(cmd)

	outs := genruleOuts(b, buildDir)
	if len(outs) == 0 {
		// The missed path: a `cmake -P <script>` custom command whose ninja edge
		// declares NO outputs (the add_custom_command had no OUTPUT/BYPRODUCTS, or
		// the producing edge is a phony the codemodel didn't tie outputs to). The
		// files the SCRIPT itself writes — `configure_file` / `file(WRITE|GENERATE
		// |APPEND|TOUCH)` / `execute_process(OUTPUT_FILE)` — are the real outputs;
		// recover them from the script, resolving `${VAR}` against the command's
		// `-D` args (the VTK `-DSCRIPT_OUT=<path>` / parameter-driven shape). Empty
		// when the script is unreadable or its outputs don't resolve to in-tree
		// paths — the lift then declines as before.
		outs = discoverCmakeScriptOutputs(scriptArg, dArgs, buildDir, cmakeSrc)
		if len(outs) == 0 {
			return "", "", false
		}
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
		traceRaw, err := TraceCmakeScript(context.Background(), cc.CMakeBinary, scriptArg, dArgs, "")
		if err != nil {
			return "", fmt.Sprintf("cmake --trace -P %s failed: %v — convert-time trace required for --cmake-script-trace; rerun without it (sandbox miss may occur at Bazel build time) or fix the script", scriptArg, err), false
		}
		cls := ClassifyScriptTrace(traceRaw, cmakeSrc, buildDir)

		// Unknown paths block the lift. The script touches a
		// path Bazel's sandbox won't reproduce; refusing at
		// convert time is more actionable than a runtime
		// sandbox miss.
		if len(cls.UnknownPaths) > 0 {
			return "", fmt.Sprintf("cmake -P script reads %d path(s) outside source/build/sysroot — Bazel's sandbox won't have these:\n  %s",
				len(cls.UnknownPaths), strings.Join(cls.UnknownPaths, "\n  ")), false
		}

		// Build-class reads need a ninja producer to substitute
		// $(location :producer). Skip the cross-reference for
		// now (queued for a follow-up — current shape: refuse
		// unresolved build paths so the diagnostic is honest).
		if len(cls.BuildPaths) > 0 {
			return "", fmt.Sprintf("cmake -P script reads %d path(s) under the build dir — these are likely cmake-side configure-time outputs and need explicit producer-lift wiring (not yet implemented):\n  %s",
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
	return name, "", true
}

// Output-producing cmake statements a `cmake -P` script runs. Pragmatic blob
// scans (like the rest of the converter's cmake-text handling), not a parser:
// each captures the OUTPUT path token of one statement form. quotes are
// stripped by the caller. configure_file's output is its SECOND argument;
// file(WRITE|APPEND|TOUCH|TOUCH_NOCREATE) and execute_process(OUTPUT_FILE) take
// the path right after the keyword; file(GENERATE ... OUTPUT <path> ...) after
// its OUTPUT keyword (which may follow other GENERATE args).
var (
	scriptConfigureFileRe = regexp.MustCompile(`(?is)\bconfigure_file\s*\(\s*("[^"]*"|[^\s)]+)\s+("[^"]*"|[^\s)]+)`)
	scriptFileWriteRe     = regexp.MustCompile(`(?is)\bfile\s*\(\s*(?:WRITE|APPEND|TOUCH|TOUCH_NOCREATE)\s+("[^"]*"|[^\s)]+)`)
	scriptFileGenerateRe  = regexp.MustCompile(`(?is)\bfile\s*\(\s*GENERATE\b[^)]*?\bOUTPUT\s+("[^"]*"|[^\s)]+)`)
	scriptExecOutFileRe   = regexp.MustCompile(`(?is)\bexecute_process\s*\([^)]*?\bOUTPUT_FILE\s+("[^"]*"|[^\s)]+)`)
	cmakeVarRefRe         = regexp.MustCompile(`\$\{([A-Za-z0-9_]+)\}`)
)

// discoverCmakeScriptOutputs returns the build-dir-relative paths a `cmake -P`
// custom command WRITES, recovered by scanning the output-producing statements —
// configure_file / file(WRITE|GENERATE|APPEND|TOUCH) / execute_process(OUTPUT_FILE) —
// of (a) the `-P` script itself AND (b) every GENERATED RECIPE `.cmake` file the
// command names through a `-D` arg (the `-DSCRIPT_OUT=abc_out.cmake` shape, where
// the recipe file enumerates the real outputs). `${VAR}` references are resolved
// against the command's `-D` args plus the standard CMAKE_*_DIR locations. Used
// when the ninja edge declared no outputs, so the recognizer / genrule fallback
// still get a real output set as discovered_outputs. Returns nil when nothing is
// readable or no output resolves to an in-tree (build- or source-relative) path.
func discoverCmakeScriptOutputs(scriptArg string, dArgs []string, buildDir, cmakeSrc string) []string {
	vars := cmakeScriptDefineMap(dArgs)
	// Best-effort standard locations a configure-time script resolves outputs
	// against. The -D args win (set first) so a project override is honored.
	for k, v := range map[string]string{
		"CMAKE_CURRENT_BINARY_DIR": buildDir,
		"CMAKE_BINARY_DIR":         buildDir,
		"CMAKE_CURRENT_SOURCE_DIR": cmakeSrc,
		"CMAKE_SOURCE_DIR":         cmakeSrc,
	} {
		if _, ok := vars[k]; !ok {
			vars[k] = v
		}
	}

	// Recipe files to scan: the -P script, plus every -D value that names a
	// `.cmake` file (the generated-recipe-via-SCRIPT_OUT shape). The recipe path
	// may itself carry a ${VAR}; expand it before resolving on disk.
	recipes := []string{scriptArg}
	for _, v := range vars {
		if expanded, ok := expandCmakeVars(v, vars); ok && strings.HasSuffix(strings.ToLower(expanded), ".cmake") {
			recipes = append(recipes, expanded)
		}
	}
	// `vars` is a map, so its iteration order is random — sort the collected
	// recipe files (keeping the -P script first) so the scan order, and thus the
	// accumulated `outs` order, is deterministic when 2+ -D args name readable
	// recipe .cmake files. Without this the emitted genrule outs / discovered
	// outputs would vary run-to-run, breaking the converter's byte-identity.
	sort.Strings(recipes[1:])

	var raw []string
	scannedFile := map[string]struct{}{}
	for _, recipe := range recipes {
		body, err := readCmakeScript(recipe, buildDir, cmakeSrc)
		if err != nil {
			continue
		}
		if _, dup := scannedFile[recipe]; dup {
			continue
		}
		scannedFile[recipe] = struct{}{}
		raw = append(raw, scanCmakeScriptOutputRefs(string(body))...)
	}

	seen := map[string]struct{}{}
	var outs []string
	for _, o := range raw {
		expanded, ok := expandCmakeVars(o, vars)
		if !ok || expanded == "" {
			continue // unresolved ${VAR} or empty — not a concrete output
		}
		rel, ok := relativeIfInsideRelaxed(buildDir, expanded)
		if !ok {
			// A source-tree write (rare for generated outputs) — keep it relative
			// to the source root so the genrule/consumer frame still resolves.
			if rel, ok = relativeIfInsideRelaxed(cmakeSrc, expanded); !ok {
				continue
			}
		}
		if _, dup := seen[rel]; dup {
			continue
		}
		seen[rel] = struct{}{}
		outs = append(outs, rel)
	}
	return outs
}

// scanCmakeScriptOutputRefs returns the raw (un-expanded, unquoted) OUTPUT path
// tokens of the output-producing statements in one cmake script's text:
// configure_file (2nd arg), file(WRITE|APPEND|TOUCH|TOUCH_NOCREATE) and
// execute_process(OUTPUT_FILE) (the path after the keyword), and
// file(GENERATE ... OUTPUT <path>). The caller resolves ${VAR} and relativizes.
func scanCmakeScriptOutputRefs(text string) []string {
	var raw []string
	collect := func(re *regexp.Regexp, group int) {
		for _, m := range re.FindAllStringSubmatch(text, -1) {
			raw = append(raw, strings.Trim(m[group], `"`))
		}
	}
	collect(scriptConfigureFileRe, 2)
	collect(scriptFileWriteRe, 1)
	collect(scriptFileGenerateRe, 1)
	collect(scriptExecOutFileRe, 1)
	return raw
}

// readCmakeScript loads a `cmake -P` script's bytes, trying scriptArg as given
// (usually an absolute, trace-expanded path) then under the build and source
// roots for a relative arg. Returns the error from the last attempt on miss.
func readCmakeScript(scriptArg, buildDir, cmakeSrc string) ([]byte, error) {
	candidates := []string{scriptArg}
	if !filepath.IsAbs(scriptArg) {
		candidates = append(candidates,
			filepath.Join(buildDir, scriptArg),
			filepath.Join(cmakeSrc, scriptArg))
	}
	var lastErr error
	for _, p := range candidates {
		if b, err := os.ReadFile(p); err == nil {
			return b, nil
		} else {
			lastErr = err
		}
	}
	return nil, lastErr
}

// cmakeScriptDefineMap turns a `cmake -P` command's -D args (as
// extractCmakePDashArgs returns them: a mix of "-D","VAR=VAL" pairs and joined
// "-DVAR=VAL" tokens) into a VAR->VALUE map. A cache-typed `VAR:TYPE=VAL` keeps
// only VAR; the first definition of a name wins (callers seed overridable
// defaults after).
func cmakeScriptDefineMap(dArgs []string) map[string]string {
	m := map[string]string{}
	for i := 0; i < len(dArgs); i++ {
		tok := dArgs[i]
		switch {
		case tok == "-D" && i+1 < len(dArgs):
			tok = dArgs[i+1]
			i++
		case strings.HasPrefix(tok, "-D"):
			tok = tok[2:]
		default:
			continue
		}
		eq := strings.IndexByte(tok, '=')
		if eq < 0 {
			continue
		}
		name := tok[:eq]
		if c := strings.IndexByte(name, ':'); c >= 0 {
			name = name[:c]
		}
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if _, ok := m[name]; !ok {
			m[name] = strings.Trim(tok[eq+1:], `"`)
		}
	}
	return m
}

// expandCmakeVars substitutes ${VAR} references in s from vars, to a fixpoint
// (bounded), and reports whether the result is fully resolved (no ${...} left).
// An unknown ${VAR} is left verbatim, so ok=false signals "do not treat as a
// concrete path."
func expandCmakeVars(s string, vars map[string]string) (string, bool) {
	for i := 0; i < 8 && strings.Contains(s, "${"); i++ {
		next := cmakeVarRefRe.ReplaceAllStringFunc(s, func(ref string) string {
			if v, ok := vars[ref[2:len(ref)-1]]; ok {
				return v
			}
			return ref
		})
		if next == s {
			break // no further resolution possible
		}
		s = next
	}
	return s, !strings.Contains(s, "${")
}

// cmakeScriptPathFromTokens returns the `-P <script>` argument from an already-
// tokenized cmake command (the token after `-P`), or "<unknown-script>" when
// there's no `-P`. The token-level core shared by extractCmakeScriptPath (string
// form) and nestedCmakeScriptCall (argv form, which must NOT be re-tokenized — a
// Windows backslash path would be mangled by shell tokenization).
func cmakeScriptPathFromTokens(tokens []string) string {
	for i, tok := range tokens {
		if tok == "-P" && i+1 < len(tokens) {
			return tokens[i+1]
		}
	}
	return "<unknown-script>"
}

// cmakePDashArgsFromTokens returns the `-D <var>=<value>` cache args (both the
// `-D val` and `-Dval` forms) from an already-tokenized cmake command, after
// stripping a leading `cd <dir> &&`. The token-level core shared by
// extractCmakePDashArgs (string form) and nestedCmakeScriptCall (argv form).
func cmakePDashArgsFromTokens(tokens []string) []string {
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
		}
	}
	return out
}

// extractCmakePDashArgs walks the recovered command and returns
// the `-D <var>=<value>` arguments cmake -P scripts often take.
// Order is preserved; everything except `cmake`, `-P`, the
// script path, and the -D args is dropped (no place to land
// shell redirects, env-vars, etc. — those make the lift unsafe).
// Returns nil for argless invocations.
func extractCmakePDashArgs(cmd string) []string {
	return cmakePDashArgsFromTokens(splitShellTokens(cmd))
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
