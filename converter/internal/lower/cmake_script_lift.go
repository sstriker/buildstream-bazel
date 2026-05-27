package lower

import (
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
// Returns the package-relative first output, the genrule name,
// and ok=true on success. ok=false means the lift declined
// (script path can't anchor under sourceRoot — typical when the
// script lives under the build dir as the output of a
// configure_file). The caller falls back to refusal in that case.
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
func liftCmakeScriptGenrule(cc *codegenContext, b *ninja.Build, cmd, scriptArg, cmakeSrc, buildDir string) (relOut, name string, ok bool) {
	scriptRel, ok := relativeIfInside(cmakeSrc, scriptArg)
	if !ok {
		// Script not under source root — typically a
		// configure_file output under the build dir. Refuse
		// the lift; the script's hardcoded paths likely
		// wouldn't work anyway.
		return "", "", false
	}
	dArgs := extractCmakePDashArgs(cmd)

	outs := genruleOuts(b, buildDir)
	if len(outs) == 0 {
		// Defensive: a CUSTOM_COMMAND with no declared outputs
		// can't form a valid genrule. Refuse so the caller's
		// existing error path covers it.
		return "", "", false
	}
	srcs := genruleSrcs(b, cmakeSrc, buildDir)
	srcs = appendUnique(srcs, scriptRel)

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
		Tags:         []string{"cmake-codegen-cmake-script-lift"},
		Visibility:   []string{"//visibility:private"},
	}
	cc.Genrules = append(cc.Genrules, gen)
	cc.SeenBuilds[b] = name
	for _, o := range outs {
		cc.OutToGenrule[o] = name
	}
	return outs[0], name, true
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
