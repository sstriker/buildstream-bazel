package lower

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/sstriker/buildstream-bazel/converter/ir"
	"github.com/sstriker/buildstream-bazel/internal/shadow"
)

// liftArgvFileProducing lifts the argv-declared codegen shape:
// `execute_process(COMMAND tool <input…> <output…>)` where the input and
// output FILES appear as argv elements (dd's if=/of=, m4 -o, custom
// generators taking explicit paths) rather than via the OUTPUT_FILE stdout
// redirect liftFileProducing hoists. add_custom_command gets this lift for
// free because cmake declares OUTPUT/DEPENDS into the ninja edge;
// execute_process declares nothing — but the contract is recoverable
// WITHOUT convert-time re-execution, because the configure already ran the
// call:
//
//   - an argv path anchoring under the SOURCE root and existing on disk is
//     an input (staged via srcs + $(location), the hoist's own policy);
//   - an argv path anchoring under the BUILD dir that another recovery
//     already produces (cc.OutToGenrule) is a GENERATED input — chains
//     resolve in trace order, the producing rule's out is referenced via
//     srcs exactly like a consumed generated source;
//   - a remaining build-dir argv FILE that exists on disk post-configure is
//     this call's OUTPUT — its presence is the configure's own evidence;
//   - anything build-dir-anchored that can't be classified (absent from
//     disk, or a directory) declines the lift, falling through to the
//     loud refusal (now mirrored into conversion-todos.json);
//   - a RELATIVE operand resolves against the process cwd — the BUILD
//     root, where cmake's configure runs — with on-disk existence (or a
//     known producer) as the path-vs-plain-flag discriminator: a relative
//     arg that names no build file is an ordinary string argument and
//     stays literal.
//
// Documented residue (sound-but-limited, all failing LOUDLY rather than
// silently wrong): an in-source WRITE classifies as an input (it exists
// post-configure; indistinguishable from a read without write tracing) and
// the re-run's attempt to write an input fails in the sandbox; undeclared
// sibling outputs (a tool writing out.h AND out.d) aren't in outs, so a
// consumer of the sibling fails at analysis; undeclared reads outside the
// argv fail in the sandbox. In-place shapes (an output that is also a
// staged input) decline — Bazel rejects input==output.
//
// Attempted only for BucketRefuse calls (cmake -E ops, POSIX copies,
// OUTPUT_FILE hoists, stamps and probes all classify earlier), with the
// same conservative keyword gates as liftFileProducing — the ROADMAP
// records the keyword-expansion order. Emits one multi-output genrule
// re-running the tool at Bazel build time (argv[0] PATH-portability per the
// hoist's contract), tagged with the -hoisted facet plus -argv-outs so
// audits can split the two hoist shapes.
func liftArgvFileProducing(call shadow.ExecuteProcessCall, anc execAnchors, cc *codegenContext) ([]string, bool) {
	if !argvCodegenEligible(call) || anc.hostBuildDir == "" {
		return nil, false
	}
	argv := call.Commands[0]
	outSet, ok := classifyArgvOutputs(argv, anc, cc)
	if !ok || len(outSet) == 0 {
		return nil, false
	}
	// Idempotency across duplicate trace calls: if every output is already
	// registered, reuse (the same contract as the other lifts).
	allRegistered := true
	for _, rel := range outSet {
		if _, exists := cc.OutToGenrule[rel]; !exists {
			allRegistered = false
			break
		}
	}
	rels := sortedArgvOuts(outSet)
	if allRegistered {
		return rels, true
	}

	srcs, rewritten, ok := rewriteArgvCodegen(argv, outSet, anc, cc)
	if !ok {
		return nil, false
	}
	// In-place decline: an output that is also a staged input would make
	// the genrule's src and out the same file — Bazel rejects it.
	srcSet := map[string]bool{}
	for _, sr := range srcs {
		srcSet[sr] = true
	}
	for _, rel := range rels {
		if srcSet[rel] {
			return nil, false
		}
	}
	var mkdirs []string
	seenDir := map[string]bool{}
	for _, rel := range rels {
		d := filepath.Dir(rel)
		if seenDir[d] {
			continue
		}
		seenDir[d] = true
		mkdirs = append(mkdirs, fmt.Sprintf(`mkdir -p "$$(dirname "$(location %s)")"`, rel))
	}
	cmd := strings.Join(append(mkdirs, strings.Join(rewritten, " ")), " && ")

	driver := executeProcessDriverBasename(argv[0])
	if driver == "" {
		driver = "unknown"
	}
	tags := append(fileProducingTags(driver), "cmake-codegen-execute-process-argv-outs")
	sort.Strings(tags)

	name := executeProcessGenruleName(rels[0])
	cc.Genrules = append(cc.Genrules, ir.Target{
		Name:        name,
		Kind:        ir.KindGenrule,
		Srcs:        srcs,
		GenruleCmd:  cmd,
		GenruleOuts: rels,
		Tags:        tags,
		Visibility:  []string{"//visibility:private"},
	})
	for _, rel := range rels {
		cc.OutToGenrule[rel] = name
	}
	return rels, true
}

// argvCodegenEligible applies the conservative shape gates: a single
// COMMAND with argv, none of the keywords the file-producing lifters
// refuse, and no capture/redirect keywords (an OUTPUT_FILE call classifies
// as BucketFileProducing before ever reaching here; OUTPUT_VARIABLE shapes
// are probe-classified).
func argvCodegenEligible(call shadow.ExecuteProcessCall) bool {
	return len(call.Commands) == 1 && len(call.Commands[0]) > 1 &&
		call.WorkingDirectory == "" && len(call.Environment) == 0 &&
		call.Timeout == "" && call.InputFile == "" && call.ErrorFile == "" &&
		call.OutputFile == "" && call.OutputVariable == "" &&
		call.ErrorVariable == "" && call.ResultVariable == ""
}

// classifyArgvOutputs walks argv (past the tool) and partitions the
// build-dir-anchored elements: produced-elsewhere paths are inputs (handled
// by the rewrite); a FILE existing on disk post-configure is an output;
// anything else build-dir-anchored is unclassifiable → decline. Returns the
// argv index → build-relative out path map.
func classifyArgvOutputs(argv []string, anc execAnchors, cc *codegenContext) (map[int]string, bool) {
	outs := map[int]string{}
	for i, a := range argv {
		if i == 0 {
			continue
		}
		p := stripArgvPathPrefix(a)
		rel, anchored := executeProcessAnchorOutput(p, anc)
		if !anchored {
			// Relative operand: resolves against the process cwd (the
			// build root). Existence / a known producer discriminates a
			// path from a plain string argument — a relative non-file is
			// an ordinary flag value and stays literal, NOT a decline.
			r, ok := relativeArgvBuildRel(p)
			if !ok {
				continue
			}
			if _, produced := cc.OutToGenrule[r]; produced {
				continue
			}
			if st, err := os.Stat(filepath.Join(anc.hostBuildDir, filepath.FromSlash(r))); err == nil && !st.IsDir() {
				outs[i] = r
			}
			continue
		}
		if _, produced := cc.OutToGenrule[rel]; produced {
			continue
		}
		st, err := os.Stat(filepath.Join(anc.hostBuildDir, filepath.FromSlash(rel)))
		if err != nil || st.IsDir() {
			return nil, false
		}
		outs[i] = rel
	}
	return outs, true
}

// relativeArgvBuildRel normalizes a relative argv operand into a build-root-
// relative slash path, rejecting flags ("-…"), the bare dot, and anything
// escaping the build root.
func relativeArgvBuildRel(p string) (string, bool) {
	if p == "" || filepath.IsAbs(p) || strings.HasPrefix(p, "-") {
		return "", false
	}
	rel := filepath.ToSlash(filepath.Clean(p))
	if rel == "." || rel == ".." || strings.HasPrefix(rel, "../") {
		return "", false
	}
	return rel, true
}

// stripArgvPathPrefix peels a `key=` prefix off an argv element so dd-style
// `if=/abs/in` / `of=/abs/out` operands classify by their path part. A
// plain path returns unchanged.
func stripArgvPathPrefix(a string) string {
	if eq := strings.IndexByte(a, '='); eq > 0 && !strings.ContainsAny(a[:eq], "/\\") {
		return a[eq+1:]
	}
	return a
}

// rewriteArgvCodegen renders the genrule cmd argv: outputs →
// `$(location <out>)`, source-tree FILE inputs → srcs + `$(location)`,
// produced build-dir inputs → srcs (the producing rule's out label) +
// `$(location)`, source-tree dirs → literal relative path, abs tool path →
// basename (the hoist's portability policy). A source-anchored FILE that
// does not exist on disk declines — it could be an in-source WRITE the
// classifier must not mistake for an input.
func rewriteArgvCodegen(argv []string, outs map[int]string, anc execAnchors, cc *codegenContext) (srcs, rewritten []string, ok bool) {
	srcSet := map[string]bool{}
	addSrc := func(rel string) {
		if !srcSet[rel] {
			srcSet[rel] = true
			srcs = append(srcs, rel)
		}
	}
	emitKeyed := func(a, repl string) string {
		if eq := strings.IndexByte(a, '='); eq > 0 && !strings.ContainsAny(a[:eq], "/\\") {
			return a[:eq+1] + repl
		}
		return repl
	}
	for i, a := range argv {
		if rel, isOut := outs[i]; isOut {
			rewritten = append(rewritten, emitKeyed(a, fmt.Sprintf("$(location %s)", rel)))
			continue
		}
		path := stripArgvPathPrefix(a)
		if rel, anchored := executeProcessAnchorOutput(path, anc); anchored {
			// Build-dir input produced by an earlier recovery (the
			// classifier declined otherwise): reference the generated file.
			addSrc(rel)
			rewritten = append(rewritten, emitKeyed(a, fmt.Sprintf("$(location %s)", rel)))
			continue
		}
		if rel, ok := relativeArgvBuildRel(path); ok && !filepath.IsAbs(path) {
			if _, produced := cc.OutToGenrule[rel]; produced {
				// Relative reference to another recovery's generated file.
				addSrc(rel)
				rewritten = append(rewritten, emitKeyed(a, fmt.Sprintf("$(location %s)", rel)))
				continue
			}
		}
		if rel, anchored := executeProcessAnchorSource(path, anc); anchored {
			isDir := rel == "" || isExistingDir(filepath.Join(anc.hostSrcDir, rel))
			if rel == "" {
				rel = "."
			}
			if isDir {
				rewritten = append(rewritten, shellQuoteArg(rel))
				continue
			}
			if _, err := os.Stat(filepath.Join(anc.hostSrcDir, filepath.FromSlash(rel))); err != nil {
				return nil, nil, false
			}
			addSrc(rel)
			rewritten = append(rewritten, emitKeyed(a, fmt.Sprintf("$(location %s)", rel)))
			continue
		}
		if i == 0 && filepath.IsAbs(a) {
			rewritten = append(rewritten, shellQuoteArg(filepath.Base(a)))
			continue
		}
		rewritten = append(rewritten, shellQuoteArg(a))
	}
	return srcs, rewritten, true
}

// sortedArgvOuts returns the deduped, sorted out rels of the classify map.
func sortedArgvOuts(outs map[int]string) []string {
	seen := map[string]bool{}
	var rels []string
	for _, rel := range outs {
		if !seen[rel] {
			seen[rel] = true
			rels = append(rels, rel)
		}
	}
	sort.Strings(rels)
	return rels
}
