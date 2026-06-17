package lower

import (
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/sstriker/buildstream-bazel/converter/ir"
	"github.com/sstriker/buildstream-bazel/internal/shadow"
	"github.com/sstriker/buildstream-bazel/internal/sliceutil"
)

// lowerTargetEventCommands recovers the TARGET-event form of add_custom_command
// (PRE_BUILD / PRE_LINK / POST_BUILD) — the build-event "stamp" pattern: a hook
// attached to a target that generates a file (a version/stamp header before
// link, a marker or post-processed artifact after build). cmake folds these
// into the target's link edge (a $PRE_LINK / $POST_BUILD binding) and lists the
// command's BYPRODUCTS as extra outputs of that LINKER edge — so recoverGenrule
// can't reach them (the producing rule isn't a CUSTOM_COMMAND) and a consumer of
// a stamp byproduct would dangle or refuse.
//
// For each such command that declares BYPRODUCTS, synthesize a genrule that runs
// the command and declares the byproducts as outs, registered in OutToGenrule so
// the existing outputClaimed consumer-resolution wires them. The command is
// rewritten like any recovered genrule: exec-root anchoring (rewriteGenruleCmd),
// tool/artifact resolution (rewriteToolFromTarget — this also turns a POST_BUILD
// reference to the target's own binary into $(execpath :tgt) + a tools dep), and
// token-granular output anchoring to $(RULEDIR) (token-exact so a byproduct
// `foo.c` doesn't clobber a source input `foo.c.in` that shares its prefix).
// Source-tree inputs the command reads are recovered into srcs. A command with
// no byproducts is a pure side-effect with no Bazel cc-rule equivalent (the
// rules have no pre-link / post-build hook) — warned and dropped.
func lowerTargetEventCommands(calls []shadow.TargetEventCommandCall, cc *codegenContext, cmakeSrc, cmakeBuild, hostSrc, bazelPackagePath string, warn io.Writer) {
	if cc == nil || len(calls) == 0 {
		return
	}
	used := map[string]bool{}
	for i := range cc.Genrules {
		used[cc.Genrules[i].Name] = true
	}
	for _, call := range calls {
		var outs []string
		for _, bp := range call.ByProducts {
			if rel, ok := relativeIfInsideRelaxed(cmakeBuild, bp); ok && !strings.Contains(rel, "${") {
				outs = append(outs, rel)
			}
		}
		outs = sliceutil.SortedUnique(outs)
		if len(outs) == 0 {
			fmt.Fprintf(warningsOrDiscard(warn),
				"lower: add_custom_command(TARGET %s %s) carries no recoverable BYPRODUCTS — a build-event command with no Bazel cc-rule equivalent; dropped\n",
				call.Target, call.Event)
			continue
		}
		// Skip if every byproduct is already produced (e.g. it's also an
		// OUTPUT-form output, or two events share a byproduct).
		allClaimed := true
		for _, o := range outs {
			if !cc.outputClaimed(o) {
				allClaimed = false
				break
			}
		}
		if allClaimed {
			continue
		}

		var parts []string
		for _, c := range call.Commands {
			if len(c) > 0 {
				parts = append(parts, strings.Join(c, " "))
			}
		}
		cmd := strings.Join(parts, " && ")
		// Source-tree abs paths -> package-relative; exec-root anchoring.
		cmd = rewriteGenruleCmd(cmd, cmakeSrc, cmakeBuild, "", bazelPackagePath)
		// Target-artifact refs (a POST_BUILD running on the binary) -> $(execpath
		// :tgt) + a tools dep.
		cmd, tools := rewriteToolFromTarget(cmd, cc.ArtifactToName, cc.ExecArtifacts, cc.Imports, cc.HostPrefixDir)

		// Token-granular pass: anchor the declared outputs to $(RULEDIR) and
		// recover source-tree inputs into srcs. Exact-token matching avoids a
		// byproduct `foo.c` clobbering an input `foo.c.in` that shares its prefix
		// (the substring hazard a blanket replace would hit).
		outSet := map[string]bool{}
		for _, o := range outs {
			outSet[o] = true
		}
		var srcs []string
		seenSrc := map[string]bool{}
		toks := strings.Fields(cmd)
		for i, tok := range toks {
			if outSet[tok] {
				toks[i] = "$(RULEDIR)/" + tok
				continue
			}
			rel := strings.TrimPrefix(tok, bazelPackagePath+"/")
			if rel != "" && !strings.Contains(rel, "$") &&
				isExistingFile(filepath.Join(hostSrc, filepath.FromSlash(rel))) && !seenSrc[tok] {
				seenSrc[tok] = true
				srcs = append(srcs, tok)
			}
		}
		cmd = strings.Join(toks, " ")

		base := sanitizeOutputName(call.Target) + "_" + strings.ToLower(call.Event)
		name := base
		for n := 2; used[name]; n++ {
			name = fmt.Sprintf("%s_%d", base, n)
		}
		used[name] = true

		cc.Genrules = append(cc.Genrules, ir.Target{
			Name:         name,
			Kind:         ir.KindGenrule,
			GenruleCmd:   cmd,
			GenruleOuts:  outs,
			GenruleTools: tools,
			Srcs:         srcs,
			Visibility:   []string{"//visibility:private"},
			Tags:         []string{"cmake-codegen-target-event-command"},
		})
		for _, o := range outs {
			cc.OutToGenrule[o] = name
		}
	}
}
