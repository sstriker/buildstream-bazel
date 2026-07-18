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
		// Best-effort fallback when no BYPRODUCTS were declared: the command argv
		// is often a stronger output signal than their absence — a compiler called
		// with `-o <path>` writes that path; a `> <file>` redirect writes that
		// file. Infer those build-dir operands as outputs rather than dropping the
		// command as a pure side-effect. Inference, not declaration, so it only
		// runs as a fallback; the declared-BYPRODUCTS path stays authoritative.
		inferred := false
		if len(outs) == 0 {
			if io := inferTargetEventOutputs(call.Commands, cmakeBuild); len(io) > 0 {
				outs, inferred = io, true
			}
		}
		if len(outs) == 0 {
			fmt.Fprintf(warningsOrDiscard(warn),
				"lower: add_custom_command(TARGET %s %s) carries no recoverable BYPRODUCTS and no inferable command-line output (-o / > redirect) — a build-event command with no Bazel cc-rule equivalent; dropped\n",
				call.Target, call.Event)
			continue
		}
		if inferred {
			fmt.Fprintf(warningsOrDiscard(warn),
				"lower: add_custom_command(TARGET %s %s) declares no BYPRODUCTS; inferred output(s) %v from the command line (-o flag / > redirect) — best-effort, declare BYPRODUCTS to make recovery authoritative\n",
				call.Target, call.Event, outs)
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

		cmd := targetEventCommandString(call.Commands)
		// Source-tree abs paths -> package-relative; exec-root anchoring.
		cmd = rewriteGenruleCmd(cmd, cmakeSrc, cmakeBuild, "", bazelPackagePath)
		// Target-artifact refs (a POST_BUILD running on the binary) -> $(execpath
		// :tgt) + a tools dep.
		cmd, tools, toolchains := rewriteToolFromTarget(cmd, cc.ArtifactToName, cc.ExecArtifacts, cc.Imports, cc.HostPrefixDir, cc.toolchainTools())

		// Token-granular pass: anchor the declared outputs to $(RULEDIR) and
		// recover source-tree inputs into srcs. This runs AFTER rewriteGenruleCmd:
		// the outSet keys are cmakeBuild-relative, and rewriteGenruleCmd is what
		// relativizes an abs operand to that form AND (via stripLeadingCd +
		// qualifyRedirectBasenames) synthesizes the build-dir-relative prefix for a
		// bare-basename redirect target under a stripped `cd` — a form the raw argv
		// doesn't carry — so the match can't move earlier without regressing those.
		// Tokenize QUOTE-AWARE (not strings.Fields): a shell-quoted argument is one
		// atomic token, so an output/source name appearing INSIDE a quoted arg
		// (e.g. `'--msg=building foo.c now'`) can't be split out and clobbered with
		// $(RULEDIR). Exact-token matching also avoids a byproduct `foo.c` clobbering
		// an input `foo.c.in` that shares its prefix.
		outSet := map[string]bool{}
		for _, o := range outs {
			outSet[o] = true
		}
		var srcs []string
		seenSrc := map[string]bool{}
		toks := quoteAwareTokens(cmd)
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

		// Unresolved generator expression guard. cmake's --trace-expand expands
		// ${VAR} but NOT $<...> genexes (they resolve at generation time), so a
		// command like a POST_BUILD `cmake -E copy $<TARGET_FILE:t> …` carries the
		// genex verbatim. We don't yet rewrite $<TARGET_FILE:t> → $(execpath :t)
		// for build-event commands, and emitting the literal would be a broken
		// genrule (the action can't open a file named "$<TARGET_FILE:t>"). Skip +
		// warn instead; the byproduct then surfaces via the unrecognized-form /
		// missing-producer breadcrumbs rather than a build that fails opaquely.
		// Resolving the TARGET_FILE family here is a roadmapped enhancement.
		if strings.Contains(cmd, "$<") {
			fmt.Fprintf(warningsOrDiscard(warn),
				"lower: add_custom_command(TARGET %s %s) command references an unresolved generator expression (e.g. $<TARGET_FILE:…>) not yet rewritten for build-event commands; skipping its byproduct genrule to avoid emitting a broken rule\n",
				call.Target, call.Event)
			continue
		}

		base := sanitizeOutputName(call.Target) + "_" + strings.ToLower(call.Event)
		name := base
		for n := 2; used[name]; n++ {
			name = fmt.Sprintf("%s_%d", base, n)
		}
		used[name] = true

		tags := []string{"cmake-codegen-target-event-command"}
		if inferred {
			// Auditable: this genrule's outputs were inferred from the command
			// line, not declared via BYPRODUCTS.
			tags = append(tags, "cmake-codegen-target-event-inferred-output")
		}
		cc.appendExecProcGenrule(ir.Target{
			Name:              name,
			Kind:              ir.KindGenrule,
			GenruleCmd:        cmd,
			GenruleOuts:       outs,
			GenruleTools:      tools,
			GenruleToolchains: toolchains,
			Srcs:              srcs,
			Visibility:        []string{"//visibility:private"},
			Tags:              tags,
		})
		for _, o := range outs {
			cc.OutToGenrule[o] = name
		}
	}
}

// targetEventCommandString renders the structured argv (one []string per
// COMMAND) into the genrule shell string, shell-quoting each token — consistent
// with the execute_process codegen path (stampCommandLine / rewriteArgvCodegen).
// Joining the argv verbatim word-splits an argument containing spaces and lets a
// token's shell metacharacters ( () $ ' " ) be reinterpreted by the shell —
// parse errors or word-splits. shellQuoteArg leaves safe tokens (paths, flags,
// byproduct/source operands) untouched so the downstream exec-root anchoring,
// tool resolution, and $(RULEDIR)/src token passes still see them; only a
// metacharacter-bearing token is wrapped.
//
// Bare shell control operators are preserved UNQUOTED. Unlike execute_process (a
// direct exec), this command is lowered into a shell genrule and legitimately
// carries shell operators — a `>` redirect is how cmake's Make/Ninja generators
// run it, and inferTargetEventOutputs reads that redirect as an output — so a
// bare control operator stays a control operator. Everything else is a literal
// argument and gets quoted; the raw trace argv carries no Bazel make-vars at
// this stage ($(RULEDIR)/$(location)/$(execpath) are injected downstream by
// rewriteGenruleCmd, rewriteToolFromTarget, and the output-anchoring pass, all
// of which emit them unquoted). (`;`-joined CMake list values are split into
// separate tokens upstream in the shadow classifier, so they never reach here as
// one token.)
func targetEventCommandString(commands [][]string) string {
	var parts []string
	for _, c := range commands {
		if len(c) == 0 {
			continue
		}
		quoted := make([]string, len(c))
		for i, tok := range c {
			quoted[i] = quoteTargetEventToken(tok)
		}
		parts = append(parts, strings.Join(quoted, " "))
	}
	return strings.Join(parts, " && ")
}

// quoteTargetEventToken shell-quotes one command token, preserving a bare shell
// control operator unquoted (see targetEventCommandString).
func quoteTargetEventToken(tok string) string {
	if isShellControlOperator(tok) {
		return tok
	}
	return shellQuoteArg(tok)
}

// isShellControlOperator reports whether a token is a BARE shell control /
// redirection operator the lowered genrule's shell must interpret as such —
// preserved unquoted while ordinary argument tokens are shell-quoted. Only an
// exact match counts: `>` is a redirect, but `a>b` or a `;`-joined list value
// (`a;b;c`) is an argument that must be quoted. The set covers the pipes/lists
// and redirection forms cmake's shell-wrapped COMMAND recipes emit.
func isShellControlOperator(tok string) bool {
	switch tok {
	case "|", "||", "&&", ";", "&",
		"<", "<<", "<<<",
		">", ">>", ">&", "&>", "&>>",
		"1>", "2>", "1>>", "2>>", "2>&1", "1>&2":
		return true
	}
	return false
}

// quoteAwareTokens splits a shell command string into tokens on unquoted
// whitespace, keeping a single-quoted span (and a backslash-escaped char outside
// quotes) as part of its token — the shapes shellQuoteArg emits. Unlike
// strings.Fields it does NOT split inside a quoted argument, so the
// output-anchoring / source-recovery pass matches whole arguments and can't
// rewrite a name that merely appears inside a quoted arg. Inter-token whitespace
// is dropped (the caller rejoins with single spaces); whitespace inside a quote
// is preserved.
func quoteAwareTokens(cmd string) []string {
	var toks []string
	var cur strings.Builder
	inSingle, escaped, started := false, false, false
	flush := func() {
		if started {
			toks = append(toks, cur.String())
			cur.Reset()
			started = false
		}
	}
	for i := 0; i < len(cmd); i++ {
		c := cmd[i]
		switch {
		case escaped:
			cur.WriteByte(c)
			escaped = false
			started = true
		case c == '\\' && !inSingle:
			cur.WriteByte(c)
			escaped = true
			started = true
		case c == '\'':
			cur.WriteByte(c)
			inSingle = !inSingle
			started = true
		case (c == ' ' || c == '\t') && !inSingle:
			flush()
		default:
			cur.WriteByte(c)
			started = true
		}
	}
	flush()
	return toks
}

// inferTargetEventOutputs is the best-effort output signal for a TARGET-event
// command that declares no (recoverable) BYPRODUCTS. Two argv shapes name an
// output strongly enough to act on:
//
//   - **compiler `-o <path>`** — the operand after a standalone `-o` flag is the
//     thing the command writes (a `VERBATIM` compile/link the project hooked onto
//     a build event without listing the result as a byproduct).
//   - **shell `> <file>` redirect** — a standalone `>` or `1>` token's operand is
//     the redirect target. Only plain stdout redirects: `>>` (append), `2>`
//     (stderr) and `&>` (both) are different tokens and deliberately excluded —
//     their target isn't the command's data output.
//
// An operand is taken only when it relativizes under the build dir (the same
// frame a declared byproduct uses), which also filters out `$`-bearing operands
// (unexpanded `${VAR}` / `$<genex>`) and out-of-tree paths. This is inference,
// so the caller runs it only as a fallback when no BYPRODUCTS were recovered.
func inferTargetEventOutputs(commands [][]string, cmakeBuild string) []string {
	var outs []string
	add := func(operand string) {
		operand = strings.TrimSpace(operand)
		if operand == "" || strings.Contains(operand, "$") {
			return
		}
		if rel, ok := relativeIfInsideRelaxed(cmakeBuild, operand); ok {
			outs = append(outs, rel)
		}
	}
	for _, argv := range commands {
		for i := 0; i < len(argv); i++ {
			switch tok := argv[i]; {
			case tok == "-o" && i+1 < len(argv):
				add(argv[i+1])
				i++
			case (tok == ">" || tok == "1>") && i+1 < len(argv):
				add(argv[i+1])
				i++
			}
		}
	}
	return sliceutil.SortedUnique(outs)
}
