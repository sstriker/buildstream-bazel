package lower

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/sstriker/buildstream-bazel/converter/internal/cmakerun"
	"github.com/sstriker/buildstream-bazel/converter/internal/fileapi"
	"github.com/sstriker/buildstream-bazel/converter/ir"
	"github.com/sstriker/buildstream-bazel/internal/configurefile"
	"github.com/sstriker/buildstream-bazel/internal/genexeval"
	"github.com/sstriker/buildstream-bazel/internal/manifest"
	"github.com/sstriker/buildstream-bazel/internal/shadow"
)

// fileGenerateOut is one recovered file(GENERATE) emission.
// Mirrors configureFileOut so lowerTarget's consumer-attribution
// loop can walk the two recovery slices the same way.
type fileGenerateOut struct {
	AbsOutput string // recording-machine absolute path: ${cmakeBuild}/<rel>
	RelOutput string // <rel>: build-dir-relative slash path; package-relative in the BUILD file
}

// recoverFileGenerate walks the trace's file(GENERATE) events
// and emits one Bazel genrule per call. The shape mirrors
// recoverConfigureFiles: cmake renders the file at generate-
// time into the build dir, the recovered genrule reproduces
// the rendering at Bazel build time. Two source-of-template
// forms map to two genrule shapes:
//
//   - INPUT <file>: the template body lives on disk; the
//     genrule references it as a srcs entry + $(location).
//     Identical shape to configure_file's lifted form.
//   - CONTENT <string>: the template body is inline in
//     CMakeLists.txt. Lifted shape embeds the body as a
//     --content-base64 blob in the genrule cmd; legacy shape
//     embeds the rendered bytes.
//
// Generator expressions ($<...>) in the template short-circuit
// to the legacy bytes-embedded shape with the
// cmake-codegen-genex-unresolved tag — the verify-pass
// would fail anyway since configurefile.Substitute doesn't
// evaluate genexes, but we keep the explicit short-circuit so
// the audit signal distinguishes "lift skipped because of
// genex" from "lift failed because values weren't recoverable".
//
// Returns an empty slice with no error when calls is empty or
// hostBuildDir is unset — preserves the pre-trace behavior for
// offline runs without a stashed fixture.
func recoverFileGenerate(calls []shadow.FileGenerateCall, hostSrcDir, recordedSrcDir, hostBuildDir, recordedBuildDir string, liftEnabled bool, cmakeVars map[string]string, genexTargets map[string]genexeval.TargetInfo, imports *manifest.Resolver, cc *codegenContext) ([]fileGenerateOut, error) {
	if len(calls) == 0 || hostBuildDir == "" {
		return nil, nil
	}

	var out []fileGenerateOut
	seenRel := map[string]bool{}
	for _, call := range calls {
		if hasGenex([]byte(call.Output)) {
			// cmake allows generator expressions in the OUTPUT
			// path (e.g. `OUTPUT $<CONFIG>/foo.h`) and writes
			// the resolved filename at generate-time; the trace
			// records the literal `$<...>` string. Try the (a)
			// evaluator to resolve OUTPUT at convert time using
			// the same Context the body lift consults — if
			// every genex in the path resolves cleanly, replace
			// call.Output with the resolved literal and continue
			// down the normal lift path. The evaluator's
			// UnsupportedError (or a parse error) drops the call
			// the same way the pre-evaluator gate did. Resolving
			// OUTPUT at CONVERT time (not Bazel time) is the
			// right choice: the resolved path becomes the
			// genrule's `outs = [...]`, which Bazel needs to
			// know statically — re-evaluating at Bazel time
			// would require dynamic outputs, which Bazel doesn't
			// support for genrule.
			resolved, ok := resolveGenexInPath(call.Output, buildGenexContext(cmakeVars, genexTargets))
			if !ok {
				// The Go-side evaluator + structural probe couldn't
				// resolve every genex in the OUTPUT path. Try the
				// generalized two-pass literal probe: on pass 1 this
				// records a request (and returns false, so we drop
				// this round); on pass 2 it returns cmake's own
				// resolved path so the call lifts. OUTPUT must
				// resolve at convert time (it becomes the genrule's
				// outs), which the second configure pass — still
				// pre-Bazel — satisfies.
				probed, pok := cc.resolveLiteral(call.Output, "")
				if !pok {
					continue
				}
				if hasGenex([]byte(probed)) {
					// cmake's evaluator returned a value that still
					// carries `$<...>` — can't be a static output
					// path. Drop, same as a failed Go-side resolve.
					continue
				}
				resolved = probed
			}
			call.Output = resolved
		}
		if !filepath.IsAbs(call.Output) {
			// Relative outputs can't be anchored without
			// per-call binary-dir context (cmake resolves
			// against the current binary dir at expand time).
			// configure_file applies the same filter.
			continue
		}
		rel, ok := relativeIfInsideRelaxed(recordedBuildDir, call.Output)
		if !ok {
			// Output landed outside the build dir — unusual
			// for file(GENERATE) (the docs call out a relative
			// path being resolved under the current binary dir,
			// but an absolute path can in principle point
			// anywhere). Drop silently; not a recovery target.
			continue
		}
		if seenRel[rel] {
			// Dedupe: cmake can re-emit the same file(GENERATE)
			// event across multiple trace frames.
			continue
		}
		seenRel[rel] = true

		body, err := os.ReadFile(filepath.Join(hostBuildDir, rel))
		if err != nil {
			// Output not on disk — for CONDITION-gated calls
			// whose condition evaluated false, cmake skips the
			// write; for offline fixtures the stash may not
			// include every output. Skip with no error so
			// missing outputs degrade gracefully.
			continue
		}

		if _, exists := cc.OutToGenrule[rel]; exists {
			// Some other lifter (configure_file, execute_process)
			// already claimed this output path. Two recoveries
			// emitting the same rel would land duplicate rule
			// names + colliding outs in BUILD.bazel. cmake itself
			// rejects two writers to the same output, so this is
			// the "shouldn't happen with a sane CMakeLists" case;
			// defensive skip mirrors the execute_process lifters
			// and keeps the recovered BUILD valid.
			continue
		}

		name := configureFileGenruleName(rel) // reuse the gen_<path> namer
		gen := buildFileGenerateGenrule(name, rel, body, call, hostSrcDir, recordedSrcDir, liftEnabled, cmakeVars, genexTargets, imports, cc)
		cc.Genrules = append(cc.Genrules, gen)
		cc.OutToGenrule[rel] = name

		out = append(out, fileGenerateOut{
			AbsOutput: call.Output,
			RelOutput: rel,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].RelOutput < out[j].RelOutput })
	return out, nil
}

// buildFileGenerateGenrule decides between the lifted shape
// (template body decoupled from BUILD.bazel content) and the bake
// shape (rendered bytes emitted via the shared bakeFileTarget —
// readable skylib write_file for \n-text, byte-exact base64 genrule
// for binary). Lift requires:
//
//   - liftEnabled, AND
//   - The template-source keyword is present (HasInput → INPUT
//     file resolvable under source root, OR HasContent → the
//     CONTENT string, which may legitimately be the empty
//     string for the empty-file emission shape), AND
//   - The template body has no generator expressions, AND
//   - configurefile.Substitute(template, values, opts) ==
//     rendered for some values map (the verify-pass — caught
//     by pickValues).
//
// Falls back to the bake shape otherwise. The genex short-circuit
// happens before pickValues so the audit tag tells the operator
// which exit fired.
func buildFileGenerateGenrule(name, outRel string, rendered []byte, call shadow.FileGenerateCall, hostSrcDir, recordedSrcDir string, liftEnabled bool, cmakeVars map[string]string, genexTargets map[string]genexeval.TargetInfo, imports *manifest.Resolver, cc *codegenContext) ir.Target {
	opts, optErr := fileGenerateOptions(call)
	bake := bakeFileTarget(name, outRel, rendered, fileGenerateTags(fileGenerateTagSet{}))
	if optErr != nil {
		return bake
	}
	if !liftEnabled {
		return bake
	}

	// Soundness gate: if any `$<TARGET_FILE*:t>` reference in
	// the template (or in the CONTENT body) names a target that
	// resolves to neither the local codemodel nor the imports
	// manifest, refuse the lift entirely. The (a) shape would
	// otherwise refuse with UnsupportedError → fall through to
	// (b) / legacy, both of which embed cmake's rendered bytes
	// → which for TARGET_FILE is the RECORDING MACHINE's
	// absolute path. Shipping that path into Bazel produces a
	// genrule that builds against a path that doesn't exist on
	// the executor. We catch this here rather than let it land
	// silently. Operators get an audit-tagged refusal stub that
	// fails the bazel build with a clear diagnostic — see
	// ROADMAP.md.
	//
	// Resolution-check input: scan templateBody once for any of
	// the seven TARGET_FILE-family ops + the call's CONTENT
	// body if present (templateBody isn't sourced yet at this
	// point in the flow, so we re-scan from call.Content for
	// the CONTENT form). The genex parser would be overkill —
	// targetFileFamilyRefs uses the same prefix-scan
	// extractTargetFileRefs does.
	if unresolved := unresolvedCrossPackageTargetFiles(call, hostSrcDir, recordedSrcDir, genexTargets, imports); len(unresolved) > 0 {
		return ir.Target{
			Name: name,
			Kind: ir.KindGenrule,
			GenruleCmd: fmt.Sprintf(
				`echo 'file(GENERATE) lift refused for output %q: template references cmake target(s) %v via `+
					`$<TARGET_FILE:...> (or a variant) that resolve to neither the local cmake codemodel `+
					`nor the imports.json manifest. Shipping cmake'"'"'s rendered bytes for these would `+
					`embed the recording-machine absolute path, which does not exist on the Bazel executor. `+
					`Add the missing target to the imports manifest or hand-edit the rendered output.' >&2; exit 1`,
				outRel, unresolved),
			GenruleOuts: []string{outRel},
			Tags:        fileGenerateTags(fileGenerateTagSet{GenexFallback: true, GenexCrossPackage: true}),
			Visibility:  []string{"//visibility:private"},
		}
	}

	// Source the template body. Exactly one of HasInput /
	// HasContent is true after a successful classify (the
	// extractor enforces XOR — both-keywords-present is
	// rejected as malformed). Keyword-presence (not
	// value-emptiness) is the discriminator:
	// `file(GENERATE CONTENT "")` is a legitimate empty-file
	// emission and the lifter routes it through the CONTENT
	// form so `--content-base64=<empty>` carries the empty
	// body to the Bazel-time tool.
	var templateBody []byte
	var inRel string // package-relative path; empty for the CONTENT form
	isContentForm := false
	switch {
	case call.HasInput:
		// cmake allows generator expressions in the INPUT
		// argument itself (e.g. `INPUT $<CONFIG>/foo.in`) —
		// the trace keeps the literal `$<...>` so
		// resolveTemplatePath / os.ReadFile would just fail
		// on the bogus path and we'd fall back to legacy
		// without the audit signal. Try the (a) evaluator
		// first — symmetric to the OUTPUT-side resolution
		// path in recoverFileGenerate. The same Context the
		// body lift consults drives INPUT-arg resolution; on
		// a clean resolve, the literal becomes the on-disk
		// template path and we continue down the normal lift
		// pipeline. UnsupportedError / empty Context routes
		// to the legacy fallback with the
		// cmake-codegen-genex-unresolved audit tag (same
		// exit as the pre-evaluator gate).
		if hasGenex([]byte(call.Input)) {
			resolved, ok := resolveGenexInPath(call.Input, buildGenexContext(cmakeVars, genexTargets))
			if !ok {
				genexLegacy := bake
				genexLegacy.Tags = fileGenerateTags(fileGenerateTagSet{GenexFallback: true})
				return genexLegacy
			}
			call.Input = resolved
		}
		templatePath, rel, ok := resolveTemplatePath(call.Input, hostSrcDir, recordedSrcDir)
		if !ok {
			return bake
		}
		body, err := os.ReadFile(templatePath)
		if err != nil {
			return bake
		}
		templateBody = body
		inRel = rel
	case call.HasContent:
		templateBody = []byte(call.Content)
		isContentForm = true
	default:
		return bake
	}

	// Generator expression in the template body → try the
	// lifts in order from most-faithful to least-flexible:
	//
	//   1. (a) Go-side evaluator: parse the template via
	//      genexeval.Parse, evaluate against a Context derived
	//      from cmakeVars (CMAKE_BUILD_TYPE / CMAKE_*_COMPILER_ID
	//      / CMAKE_SYSTEM_NAME), and check the result matches
	//      cmake's rendered bytes. On success the lifted shape
	//      ships the Context as a base64 sidecar; cmake-
	//      configure-file re-evaluates at Bazel time. This is
	//      the only shape that handles template edits that add
	//      NEW genex literals against the same Context (the
	//      operator gets the new genex resolved without rerunning
	//      convert-element-cmake). genexeval.UnsupportedError on
	//      ops outside the v1 subset (target-evaluator-dependent
	//      forms like $<TARGET_FILE:...>) routes to step 2.
	//
	//   2. (b) structured-base64 capture: walk the template's
	//      static chunks against cmake's rendered output to
	//      recover each top-level `$<...>`'s resolved bytes;
	//      ship as a literal-replace map. Sound when (a)
	//      refuses but the template's static surround can
	//      anchor each genex's value uniquely. Failure modes
	//      (adjacent genexes with no separator, same literal
	//      resolving to different values, static-chunk
	//      misalignment) route to step 3.
	//
	//   3. Legacy bytes-embedded with the
	//      cmake-codegen-genex-unresolved audit tag —
	//      rendered output content-load-bearing in srckey, no
	//      Bazel-time re-evaluation.
	//
	// INPUT-arg genex and OUTPUT-side genex are resolved at
	// convert time (above and in recoverFileGenerate
	// respectively) via the same (a) evaluator and Context
	// the body lift consults — by the time control reaches
	// here, both `call.Input` and the upstream `call.Output`
	// are already literal paths if their genexes could
	// resolve. The body-side decision below is purely about
	// the template's content.
	if hasGenex(templateBody) {
		// Payload pruning: only ship Targets in the marshaled
		// Context when the template actually references a per-
		// target op (TARGET_PROPERTY / TARGET_FILE family /
		// TARGET_OBJECTS). Avoids dumping every per-target dict
		// into the lifted cmd for templates that only reference
		// CONFIG / PLATFORM_ID / etc. — the (a) lift's payload
		// would otherwise grow linearly with target count for
		// no benefit.
		ctxTargets := genexTargets
		needsTargets := bytes.Contains(templateBody, []byte("$<TARGET_PROPERTY:"))
		if !needsTargets {
			for _, prefix := range targetFileOpPrefixes {
				if bytes.Contains(templateBody, []byte(prefix)) {
					needsTargets = true
					break
				}
			}
		}
		if !needsTargets && bytes.Contains(templateBody, []byte(targetObjectsOpPrefix)) {
			needsTargets = true
		}
		if !needsTargets {
			ctxTargets = nil
		}
		// Extract TARGET_FILE references for cmd emission. The
		// byte-equal check uses ctx's FileLocation values (set
		// by buildGenexTargets to the recording-machine artifact
		// paths so the eval matches cmake's rendered output);
		// the MARSHALED Context's wire struct omits FileLocation
		// per the wire-struct definition in marshalGenexContext,
		// so the lifted cmd stays byte-stable across recording
		// machines; --target-file flags carry the Bazel-time
		// values.
		//
		// TARGET_OBJECTS references are sibling-extracted via
		// extractTargetObjectsRefs — same convert-time machinery
		// (the byte-equal check consults ctx.Targets[t].Objects
		// populated from the probe-genex hook) but with a separate
		// Bazel-time wire (`--target-objects=` flags using
		// `$(locations :t)` plural-substitution).
		targetFileRefs := extractTargetFileRefs(templateBody)
		targetObjectsRefs := extractTargetObjectsRefs(templateBody)
		// $<TARGET_OBJECTS:t> soundness gate: t must be a same-element
		// OBJECT_LIBRARY for the converter to expose its objects as an
		// addressable label (a compilation_outputs filegroup over the
		// cc_library). Cross-element / imported / non-object-library refs
		// have no such label — refuse rather than ship a wrong wire (the
		// pre-fix shape pointed target_objects at the cc_library, whose
		// DefaultInfo is the archive, not the .o files).
		if unresolved := unresolvableObjectLibs(targetObjectsRefs, genexTargets); len(unresolved) > 0 {
			return targetObjectsRefusalStub(name, outRel, unresolved)
		}
		// PR 2: resolve each TARGET_FILE reference to a Bazel
		// label, branching per-target on resolution:
		//
		//   - same-package (genexTargets[t] exists) → `:t`
		//     (existing PR 1 behaviour).
		//   - manifest-resolved (imports.LookupCMakeTarget(t)
		//     returns an export) → that export's full Bazel
		//     label (`//elements/foo:bar`).
		//   - else → drop from the cmd flag set; the (a) eval
		//     refuses on missing FileLocation; the upstream
		//     unresolvedCrossPackageTargetFiles gate has already
		//     intercepted the truly-unresolvable case via the
		//     refusal stub. (Belt-and-suspenders: even if a
		//     downstream extension added a name we don't
		//     resolve, dropping it surfaces as an (a) refusal
		//     and falls to (b)/legacy rather than emitting a
		//     bogus label.)
		//
		// Cross-package labels also need to ride in the
		// genrule's srcs so Bazel resolves `$(location //pkg:t)`
		// at action time. Same-package `:t` labels resolve
		// without an explicit srcs entry (Bazel's per-package
		// lookup picks them up); cross-package labels do not.
		// target_files is a label-keyed attribute on the
		// cmake_configure_file rule, so Bazel tracks each referenced
		// target as a dependency and resolves its path at action time —
		// no explicit srcs entry needed for cross-package labels (the
		// genrule shape required threading those through srcs for
		// `$(location //pkg:t)`).
		targetFileLabels, _ := resolveTargetFileLabels(targetFileRefs, ctxTargets, imports)
		ctx := buildGenexContext(cmakeVars, ctxTargets)
		if nodes, err := genexeval.Parse(templateBody); err == nil {
			if evaled, evalErr := genexeval.Eval(nodes, ctx); evalErr == nil && bytes.Equal(evaled, rendered) {
				if ctxJSON, mErr := marshalGenexContext(ctx); mErr == nil {
					spec := newConfigureFileSpec(outRel, opts)
					spec.Values = map[string]string{}
					spec.GenexContext = string(ctxJSON)
					spec.TargetFiles = invertTargetFileLabels(targetFileLabels)
					// Emit a compilation_outputs filegroup per OBJECT library
					// and wire target_objects at it (gate above guaranteed each
					// ref is a same-element OBJECT_LIBRARY). Only on the (a)-eval
					// success path so no orphan filegroups are emitted when the
					// lift falls through.
					spec.TargetObjects = emitObjectFilegroups(targetObjectsRefs, cc)
					setTemplateOrContent(spec, inRel, templateBody, isContentForm)
					return cmakeConfigureFileTarget(name, spec, fileGenerateTags(fileGenerateTagSet{Lifted: true, GenexEvaluated: true}))
				}
			}
			// evalErr being UnsupportedError or a Context-
			// missing-field error is the expected refusal for
			// templates outside the v1 subset; fall through to
			// (b). Other errors (parse internals, byte-mismatch)
			// also fall through — soundness preserved by the
			// downstream lift choice.
		}

		// The (a)-eval did not resolve. If the template embeds
		// $<TARGET_OBJECTS:t>, the downstream (b)/(b′)/legacy shapes would
		// bake cmake's rendered output — the recording-machine object paths,
		// which don't exist on the executor. Refuse instead of baking. (The
		// gate above already rejected unresolvable t; reaching here means the
		// template as a whole was too complex for the evaluator.)
		if len(targetObjectsRefs) > 0 {
			return targetObjectsRefusalStub(name, outRel, targetObjectsRefs)
		}

		genexValues, err := extractGenexValues(templateBody, rendered)
		if err == nil {
			spec := newConfigureFileSpec(outRel, opts)
			spec.Values = map[string]string{}
			spec.GenexValues = genexValues
			setTemplateOrContent(spec, inRel, templateBody, isContentForm)
			return cmakeConfigureFileTarget(name, spec, fileGenerateTags(fileGenerateTagSet{Lifted: true, GenexCaptured: true}))
		}

		// (b′) Two-pass per-literal probe. (b)'s positional anchoring
		// failed (adjacent genexes / ambiguous static chunks); resolve
		// each top-level genex literal individually via cmake's own
		// evaluator (the warm second configure pass) and replay as a
		// literal-replace map. On pass 1 this records the probe
		// requests and returns false, so we fall to legacy this round;
		// the orchestrator runs the second pass and re-lifts.
		if probed, ok := probeGenexValuesForBody(cc, templateBody); ok {
			spec := newConfigureFileSpec(outRel, opts)
			spec.Values = map[string]string{}
			spec.GenexValues = probed
			setTemplateOrContent(spec, inRel, templateBody, isContentForm)
			return cmakeConfigureFileTarget(name, spec, fileGenerateTags(fileGenerateTagSet{Lifted: true, GenexProbed: true}))
		}
		// Per-config divergence: the flat probe declined because a top-level
		// literal resolves to different bytes per build config (Multi-Config).
		// Lower it to a select() over //config:<name> via GenexValuesPerConfig
		// rather than dropping to legacy.
		if perConfig, ok := probeGenexValuesPerConfigForBody(cc, templateBody); ok {
			spec := newConfigureFileSpec(outRel, opts)
			spec.Values = map[string]string{}
			spec.GenexValuesPerConfig = perConfig
			setTemplateOrContent(spec, inRel, templateBody, isContentForm)
			return cmakeConfigureFileTarget(name, spec, fileGenerateTags(fileGenerateTagSet{Lifted: true, GenexProbed: true}))
		}
		genexLegacy := bake
		genexLegacy.Tags = fileGenerateTags(fileGenerateTagSet{GenexFallback: true})
		return genexLegacy
	}

	// Verify-pass for file(GENERATE): the lift's contract is
	// `Substitute(template, {}, {CopyOnly:true, NewlineStyle:N})
	// == rendered`. file(GENERATE) doesn't do @VAR@/${VAR}/
	// #cmakedefine substitution (CopyOnly skips all three), so
	// passing the cmake namespace via cmakeVars is both wasteful
	// (bloats BUILD.bazel/cmd with the full dump) AND wrong:
	// a later template edit that adds an @VAR@ marker would
	// substitute under the lifted-with-cmakeVars shape but
	// emit-verbatim under cmake's actual file(GENERATE)
	// semantics. Bypass pickValues entirely; the values dict
	// is unconditionally empty for file(GENERATE) lifts.
	if !bytes.Equal(configurefile.Substitute(templateBody, nil, opts), rendered) {
		// Verify-pass failed — most likely an unmodeled
		// NEWLINE_STYLE rewrite or some other surface cmake
		// applied that we haven't captured. Soundness:
		// fall back to legacy bytes-embedded shape.
		return bake
	}
	spec := newConfigureFileSpec(outRel, opts)
	spec.Values = map[string]string{}
	setTemplateOrContent(spec, inRel, templateBody, isContentForm)
	return cmakeConfigureFileTarget(name, spec, fileGenerateTags(fileGenerateTagSet{Lifted: true}))
}

// setTemplateOrContent points a cmake_configure_file spec at its template
// source: the INPUT form references the on-disk file via the `template`
// label; the CONTENT form carries the body inline (the rule writes it to a
// file and feeds it to the tool — no --content-base64).
func setTemplateOrContent(spec *ir.CMakeConfigureFileSpec, inRel string, templateBody []byte, isContentForm bool) {
	if isContentForm {
		spec.Content = string(templateBody)
	} else {
		spec.Template = inRel
	}
}

// invertTargetFileLabels flips the lifter's cmake-name -> bazel-label map
// into the label -> cmake-name shape the cmake_configure_file rule's
// `target_files` label-keyed dict expects. Returns nil for an empty input
// so the emitter omits the attribute.
func invertTargetFileLabels(byName map[string]string) map[string]string {
	if len(byName) == 0 {
		return nil
	}
	out := make(map[string]string, len(byName))
	for name, label := range byName {
		out[label] = name
	}
	return out
}

// unresolvableObjectLibs returns the $<TARGET_OBJECTS:t> reference names the
// converter can't expose as an addressable Bazel object-file label. A name is
// resolvable only when it's a same-element OBJECT_LIBRARY (genexTargets[t]
// exists with Type=="OBJECT_LIBRARY"), which lowers to a cc_library whose
// compiled objects we surface via a `compilation_outputs` filegroup. Cross-
// element / imported / non-object-library refs have no such label, so they
// drive a refusal stub rather than a wrong wire.
func unresolvableObjectLibs(refs []string, genexTargets map[string]genexeval.TargetInfo) []string {
	var bad []string
	for _, t := range refs {
		ti, ok := genexTargets[t]
		if !ok || ti.Type != "OBJECT_LIBRARY" {
			bad = append(bad, t)
		}
	}
	return bad
}

// emitObjectFilegroups emits one filegroup per $<TARGET_OBJECTS:t> reference
// that gathers the OBJECT library's compiled objects from its cc_library's
// `compilation_outputs` output group (deduped across calls), and returns the
// `target_objects` label-keyed dict ({":t_objects": "t"}) for the
// cmake_configure_file spec. The filegroup's DefaultInfo.files is the .o set,
// so the rule's `dep[DefaultInfo].files` → `--target-objects` carries Bazel's
// real object paths at action time. Callers must have already rejected
// unresolvable refs via unresolvableObjectLibs. Returns nil for empty input.
func emitObjectFilegroups(refs []string, cc *codegenContext) map[string]string {
	if len(refs) == 0 {
		return nil
	}
	out := make(map[string]string, len(refs))
	for _, t := range refs {
		fg := t + "_objects"
		out[":"+fg] = t
		if cc.hasSynthesizedTarget(fg) {
			continue // dedup: another file(GENERATE) already emitted it
		}
		cc.Genrules = append(cc.Genrules, ir.Target{
			Name:                 fg,
			Kind:                 ir.KindFilegroup,
			Srcs:                 []string{":" + t},
			FilegroupOutputGroup: "compilation_outputs",
			Tags:                 []string{"cmake-codegen", "cmake-codegen-target-objects"},
			Visibility:           []string{"//visibility:private"},
		})
	}
	return out
}

// targetObjectsRefusalStub builds the audit-tagged refusal genrule for a
// file(GENERATE) whose $<TARGET_OBJECTS:t> references can't be soundly lowered.
// It fails the bazel build with a clear diagnostic instead of baking cmake's
// recording-machine object paths (which don't exist on the executor).
func targetObjectsRefusalStub(name, outRel string, refs []string) ir.Target {
	return ir.Target{
		Name: name,
		Kind: ir.KindGenrule,
		GenruleCmd: fmt.Sprintf(
			`echo 'file(GENERATE) lift refused for output %q: template references `+
				`$<TARGET_OBJECTS:...> for target(s) %v that are not a same-element OBJECT_LIBRARY `+
				`(or the template could not be resolved offline). Bazel exposes a target'"'"'s object `+
				`files only for an OBJECT library this element builds (via a compilation_outputs `+
				`filegroup); cross-element / imported object lists have no addressable label. Baking `+
				`cmake'"'"'s rendered object paths would embed recording-machine paths that do not `+
				`exist on the executor.' >&2; exit 1`,
			outRel, refs),
		GenruleOuts: []string{outRel},
		Tags:        fileGenerateTags(fileGenerateTagSet{GenexFallback: true, GenexTargetObjectsRefused: true}),
		Visibility:  []string{"//visibility:private"},
	}
}

// fileGenerateOptions translates the call's NewlineStyle into
// configurefile.Options. file(GENERATE) supports NEWLINE_STYLE
// and permission keywords; only NEWLINE_STYLE affects content
// bytes (permission tokens are dropped by the extractor). On
// an unknown style, returns an error so the caller falls back
// to legacy (with the rendered bytes already accounting for
// cmake's actual choice).
//
// CopyOnly is always set: file(GENERATE) is NOT configure_file
// — it does NOT do @VAR@/${VAR}/#cmakedefine substitution.
// Its body-transform surface is exactly (a) generator-
// expression resolution (handled in cmake; the lifter's
// hasGenex short-circuit routes genex-bearing templates to
// legacy) and (b) NEWLINE_STYLE rewriting. CopyOnly=true on
// the Bazel-time tool produces the same byte transform —
// verbatim copy modulo line terminators — which is what
// matches cmake's semantics regardless of what @VAR@ markers
// the user later adds to the template.
//
// AtOnly / EscapeQuotes are not applicable to file(GENERATE);
// they stay at their zero values.
func fileGenerateOptions(call shadow.FileGenerateCall) (configurefile.Options, error) {
	out := configurefile.Options{CopyOnly: true}
	switch strings.ToUpper(call.NewlineStyle) {
	case "":
		out.NewlineStyle = configurefile.NewlineDefault
	case "UNIX", "LF":
		out.NewlineStyle = configurefile.NewlineLF
	case "DOS", "WIN32", "CRLF":
		out.NewlineStyle = configurefile.NewlineCRLF
	default:
		return out, fmt.Errorf("NEWLINE_STYLE: unknown value %q", call.NewlineStyle)
	}
	return out, nil
}

// hasGenex reports whether the template body contains a
// generator expression. cmake's `$<...>` syntax can nest, but
// the marker bytes `$<` are unambiguous — any occurrence means
// cmake would evaluate something at generate-time that
// configurefile.Substitute doesn't model. v1 treats any genex
// as a lift refusal; future work (see ROADMAP "Generator-
// expression evaluation in lifted genrules") may evaluate the
// configure-time-resolvable subset.
//
// False positives are harmless — they only force the legacy
// bytes-embedded shape, which is sound (just less cache-key
// friendly). False negatives would silently produce a wrong
// lift; the direct `Substitute(template, nil, opts) == rendered`
// verify-pass in buildFileGenerateGenrule (and the analogous
// pickValues-driven check in configure_file's buildConfigureFileGenrule)
// catches those, so this is a fast-path short-circuit rather
// than the soundness gate.
func hasGenex(template []byte) bool {
	return bytes.Contains(template, []byte("$<"))
}

// fileGenerateTags returns the cmake-codegen tag set for a
// file(GENERATE) emission. Distinguishes from configure_file
// via cmake-codegen-driver=file_generate so audit queries can
// split the two cleanly.
//
// Four facets:
//
//   - lifted: the rendered output bytes are NOT content-load-
//     bearing in srckey; the template (in srcs or as a
//     base64 blob) is. Bazel re-renders via
//     //tools:cmake-configure-file.
//   - genexFallback: the template had at least one top-level
//     `$<...>` AND every lift shape refused. The legacy
//     bytes-embedded shape is in play; rendered bytes are
//     load-bearing.
//   - genexCaptured: the template had at least one top-level
//     `$<...>` AND the lifter produced the (b) "structured
//     base64" shape: the captured genex literal → resolved
//     bytes map ships in the cmd alongside the template; at
//     Bazel time //tools:cmake-configure-file replays the
//     substitution. Implies lifted.
//   - genexEvaluated: the template had at least one top-level
//     `$<...>` AND the lifter produced the (a) "Go-side
//     evaluator" shape: a captured cmake configure-time
//     Context ships in the cmd; at Bazel time
//     //tools:cmake-configure-file parses the template via
//     genexeval and resolves each genex against the Context.
//     Implies lifted. Template edits that add NEW genex
//     literals against the same Context get evaluated without
//     a convert-element-cmake re-run.
//
// Mutual exclusion: genexFallback excludes lifted (a fallback
// by definition isn't lifted). genexCaptured and genexEvaluated
// are mutually exclusive (one lift shape per call site) but
// both imply lifted.
// fileGenerateTagSet names each tag-emit facet so call sites
// document the lift outcome directly: only the true-valued
// facets appear, vs the 5-positional-bool form where readers
// had to count positions. Zero value = the legacy bytes-
// embedded shape with the base cmake-codegen tags only.
//
// Five facets, mutually-compatible at the call-site level
// (some combos are nonsensical — e.g. Lifted + GenexCrossPackage
// shouldn't co-occur — but the tag emission is independent per
// facet so the type doesn't enforce exclusivity).
// bakeFileTarget builds a "bake" target — a fully-resolved body whose
// exact bytes are known at convert time and need no Bazel-time
// re-evaluation. Shared by the file(GENERATE) and configure_file
// lowerings (both fall back here when their lift tiers decline). It
// prefers skylib write_file (the content carried as a human-readable
// list of lines) over the legacy `echo <base64> | base64 -d > $@`
// genrule, falling back to the genrule only for bodies write_file
// can't represent readably + round-trip exactly (binary / control
// bytes / CRLF — see writeFileLines). The base64 genrule is byte-exact
// regardless, so it's the safe fallback; write_file is the
// maintainability win for the common \n-only-text case (config
// headers, manifests).
//
// tags is the already-rendered tag list (callers pass their own
// driver's bake / genex-fallback tag set). Both kinds carry the same
// tags; only the mechanism differs.
func bakeFileTarget(name, outRel string, rendered []byte, tags []string) ir.Target {
	base := ir.Target{
		Name:       name,
		Tags:       tags,
		Visibility: []string{"//visibility:private"},
	}
	if lines, ok := writeFileLines(rendered); ok {
		base.Kind = ir.KindWriteFile
		base.WriteFileOut = outRel
		base.WriteFileContent = lines
		base.WriteFileNewline = "unix"
		return base
	}
	base.Kind = ir.KindGenrule
	base.GenruleCmd = configureFileLegacyCmd(outRel, rendered)
	base.GenruleOuts = []string{outRel}
	return base
}

// writeFileLines splits a readable, exactly-reproducible body into the
// line list skylib write_file (newline="unix") joins back to the
// original bytes. Eligible iff the body is valid UTF-8 carrying no
// control bytes other than newline (\n) and tab (\t): then
// strings.Split(body, "\n") joined by "\n" equals body exactly
// (Split/Join are inverses; a trailing "" element reproduces a
// trailing newline, e.g. "a\n" -> ["a", ""] -> "a\n").
//
// The control-byte rejection is deliberate and twofold: (1) CR (\r)
// wouldn't round-trip through write_file's unix-newline join, and
// (2) NUL / other control bytes are valid UTF-8 but would render as
// unreadable \x00-style escapes in the BUILD string literal — which
// defeats the whole point (readability) and isn't what "text" means
// here. Such bodies stay on the byte-exact base64 genrule. (Non-ASCII
// printable UTF-8 — accented chars, etc. — has all bytes >= 0x80 and
// passes; only the C0 controls + DEL are rejected.)
func writeFileLines(body []byte) ([]string, bool) {
	if !utf8.Valid(body) {
		return nil, false
	}
	for _, b := range body {
		if b == '\n' || b == '\t' {
			continue
		}
		if b < 0x20 || b == 0x7f {
			return nil, false
		}
	}
	return strings.Split(string(body), "\n"), true
}

type fileGenerateTagSet struct {
	// Lifted: the genrule uses the cmake-configure-file tool
	// (template body decoupled from BUILD.bazel content) vs.
	// the legacy bytes-embedded shape.
	Lifted bool
	// GenexFallback: template carries genexes the lift couldn't
	// resolve; the (b) or legacy fallback shape is in play.
	GenexFallback bool
	// GenexCaptured: the (b) structured-base64 lift succeeded
	// — rendered bytes no longer in srckey.
	GenexCaptured bool
	// GenexEvaluated: the (a) Go-side evaluator lift succeeded
	// — the most-faithful shape.
	GenexEvaluated bool
	// GenexProbed: the (b′) two-pass literal-probe lift succeeded
	// — each top-level genex was resolved individually by cmake's
	// own evaluator (the warm second configure pass), so the
	// literal-replace map is sound even where (b)'s positional
	// anchoring couldn't recover the per-genex values (adjacent
	// genexes, ambiguous static chunks). Still "resolved" for
	// audit purposes — rendered bytes no longer in srckey.
	GenexProbed bool
	// GenexCrossPackage: cross-package TARGET_FILE soundness
	// gate fired — the genrule is a refusal stub that fails
	// at bazel-build time. See
	// ROADMAP.md.
	GenexCrossPackage bool
	// GenexTargetObjectsRefused: $<TARGET_OBJECTS:t> soundness
	// gate fired — t is not a same-element OBJECT_LIBRARY (so its
	// objects have no addressable Bazel label), or the (a)-eval
	// couldn't resolve the template (so a fallback would bake
	// cmake's recording-machine object paths). The genrule is a
	// refusal stub that fails at bazel-build time. See ROADMAP.md.
	GenexTargetObjectsRefused bool
}

// fileGenerateTags returns the cmake-codegen tag set for a
// recovered file(GENERATE) genrule. Always carries the three
// base tags (cmake-codegen, cmake-codegen-driver=file_generate,
// cmake-codegen-file-generate); the facet flags append one
// tag each when true. Sorted on return for byte-stable
// BUILD.bazel output.
//
// Phase 3 genex-tag collapse (ROADMAP.md): the former four-way
// split — cmake-codegen-file-generate-genex{,-lifted,-evaluated,
// -cross-package} — folds to ONE positive tag,
// cmake-codegen-genex-resolved, for any genex the converter
// actually resolved (whether via the (a) Go-side evaluator, the
// cmake probe that feeds it, or the (b) structured-base64
// capture). Both the (a) and (b) shapes ship a Bazel-time-stable
// genrule whose rendered bytes are no longer content-load-bearing
// in srckey — the audit distinction between "resolved by the Go
// evaluator" vs "resolved by static-chunk capture" carried no
// downstream consumer, so collapsing them removes a tag
// consumers had to OR together.
//
// Two facets stay distinct because they are NOT "resolved" and a
// meaningful audit query still wants to find them:
//
//   - GenexFallback → cmake-codegen-genex-unresolved: the legacy
//     bytes-embedded shape. The genex was NOT resolved; cmake's
//     rendered output is baked into the cmd and stays
//     content-load-bearing in srckey. Audits hunting for
//     conversions that haven't reached genex parity key on this.
//
//   - GenexCrossPackage → cmake-codegen-genex-cross-package: the
//     cross-package TARGET_FILE soundness refusal stub. The
//     genrule fails at bazel-build time on purpose; this is a
//     refusal, not a resolution, so it keeps its own tag.
func fileGenerateTags(s fileGenerateTagSet) []string {
	tags := []string{
		"cmake-codegen",
		"cmake-codegen-driver=file_generate",
		"cmake-codegen-file-generate",
	}
	if s.Lifted {
		tags = append(tags, "cmake-codegen-lifted")
	}
	if s.GenexFallback {
		tags = append(tags, "cmake-codegen-genex-unresolved")
	}
	if s.GenexCaptured || s.GenexEvaluated || s.GenexProbed {
		tags = append(tags, "cmake-codegen-genex-resolved")
	}
	if s.GenexCrossPackage {
		tags = append(tags, "cmake-codegen-genex-cross-package")
	}
	if s.GenexTargetObjectsRefused {
		tags = append(tags, "cmake-codegen-genex-target-objects-refused")
	}
	sort.Strings(tags)
	return tags
}

// buildGenexTargets projects the fileapi codemodel's per-target
// data into the genexeval.TargetInfo map the evaluator
// consults for `$<TARGET_PROPERTY:t,p>` and `$<TARGET_OBJECTS:t>`.
// Keyed by the target's cmake name (not its fileapi ID).
//
// Captured from the codemodel: Type / Sources / Imported /
// FileLocation. cmake-internal helper targets (ZERO_CHECK /
// INSTALL / PACKAGE / ...) are skipped — they have no Bazel
// equivalent and the user-authored CMakeLists shouldn't reference
// them via TARGET_PROPERTY.
//
// Captured from probes (Phase 3 of the generator-parity uplift)
// when probes is non-empty: INTERFACE_* aggregates that cmake
// resolves at generation time by walking the dependency graph,
// plus the per-OBJECT_LIBRARY Objects list. Probes for unknown
// targets (probe data without a corresponding codemodel entry)
// are dropped silently — the codemodel is the ground truth for
// "what targets exist."
//
// Captured from the decoded trace (ROADMAP `Later` "TARGET_PROPERTY
// INTERFACE_* aggregation") when probes did not populate the
// INTERFACE_* fields: walks each target's PUBLIC / INTERFACE
// target_include_directories / target_compile_definitions /
// target_compile_options / target_link_libraries calls to
// recover the per-target DIRECT contribution, then walks
// codemodel Dependencies[] transitively (depth-first, first-
// occurrence preserved, dedup'd, cycles broken) to assemble
// the post-aggregation value cmake's generator-phase evaluator
// would produce. Probe-populated values take precedence over
// the convert-time aggregate — when both run (cmake 3.24+ with
// the probe hook staged), the probe is the source of truth.
//
// Captured from the imports manifest (PR 2 cross-package
// TARGET_FILE) when imports is non-nil: each export's
// CMakeTarget surfaces as an Imported=true TargetInfo entry
// keyed by the namespaced cmake name (e.g. `Foo::bar`). Its
// FileLocation comes from the export's LinkPaths[0] — cmake's
// `$<TARGET_FILE:Foo::bar>` at recording time resolves to the
// IMPORTED_LOCATION_<CONFIG> value, which the orchestrator side
// captured into LinkPaths. Populating FileLocation lets the (a)
// evaluator's byte-equal check pass at convert time for
// templates referencing imported targets. The marshaled wire
// struct OMITS FileLocation (json:"-"), so the recording-machine
// path never lands in the lifted cmd — the Bazel-time
// `--target-file` flag carries the cross-package label
// resolved at action time. A codemodel-local target with the
// same name (rare; cmake namespaces avoid this) wins over the
// manifest entry. The imports fold runs LAST so a manifest
// entry never shadows a local codemodel target.
//
// Returns nil when r is nil or has no usable targets — the
// evaluator's UnsupportedError on missing-target surfaces
// cleanly and routes the lift to (b) / legacy.
func buildGenexTargets(r *fileapi.Reply, recordedBuildDir string, probes []cmakerun.GenexProbe, decoded *shadow.Decoded, imports *manifest.Resolver) map[string]genexeval.TargetInfo {
	if r == nil || len(r.Targets) == 0 {
		// No local codemodel. Two sources can still populate the
		// dict: imported targets (a template referencing
		// `$<TARGET_FILE:Foo::bar>` with no project-local targets)
		// and INTERFACE_LIBRARY probes (the codemodel omits
		// INTERFACE libraries from targets[], so an interface-only
		// project lands here even though its probe captured the
		// resolved INTERFACE_* aggregates lowerInterfaceLibraries
		// needs).
		out := map[string]genexeval.TargetInfo{}
		foldInterfaceLibraryProbes(out, probes)
		foldImportedTargets(out, imports)
		if len(out) == 0 {
			return nil
		}
		return out
	}
	out := make(map[string]genexeval.TargetInfo, len(r.Targets))
	// Build a name → fileapi target map for the dep walk. The
	// codemodel keys r.Targets by name already, but the slice
	// shape from Reply (a flat map) makes the lookup cheap
	// without re-walking — we reuse r.Targets directly below.
	for _, t := range r.Targets {
		if t.IsGeneratorProvided {
			continue
		}
		// SOURCES property: semicolon-join the source paths in
		// fileapi's documented order, matching cmake's list
		// serialization for property strings.
		var sources []string
		for _, s := range t.Sources {
			sources = append(sources, s.Path)
		}
		// FileLocation: the primary on-disk artifact path.
		// Build by joining the recorded build dir (where cmake
		// emitted the artifact) with the artifact's build-dir-
		// relative path. This matches what cmake's
		// `$<TARGET_FILE:t>` expands to at generate time and is
		// what the convert-time byte-equal check needs to match
		// cmake's rendered output. The marshaled wire struct
		// (marshalGenexContext) omits FileLocation so this
		// recording-machine path never lands in the lifted
		// genrule's cmd — only the Bazel-time --target-file
		// flag's value does, populated via $(location :t).
		var fileLoc string
		if len(t.Artifacts) > 0 && recordedBuildDir != "" {
			fileLoc = filepath.Join(recordedBuildDir, t.Artifacts[0].Path)
		}
		out[t.Name] = genexeval.TargetInfo{
			Type:    t.Type,
			Sources: strings.Join(sources, ";"),
			// Imported targets carry no fileapi codemodel
			// directly; the fileapi codemodel only lists targets
			// added by the project's own CMakeLists. Imported
			// targets surfaced via find_package etc. would need
			// a separate capture path; for v1 we report Imported
			// as false for all captured targets (matches reality
			// — they're all locally-defined).
			Imported:     false,
			FileLocation: fileLoc,
		}
	}
	// Convert-time INTERFACE_* aggregation. Runs unconditionally
	// (when decoded != nil); probe data below takes precedence
	// per-property. The two-step layering — aggregate-first,
	// then overlay probe — keeps the fallback path consistent
	// with "probe missed this target / this property" without
	// the caller having to coordinate.
	if decoded != nil {
		agg := aggregateInterfaceProperties(r, decoded)
		for name, ti := range out {
			if a, ok := agg[name]; ok {
				ti.InterfaceIncludeDirectories = a.includeDirectories
				ti.InterfaceCompileDefinitions = a.compileDefinitions
				ti.InterfaceCompileOptions = a.compileOptions
				ti.InterfaceLinkLibraries = a.linkLibraries
				out[name] = ti
			}
		}
	}
	foldInterfaceLibraryProbes(out, probes)
	// PR 2: fold imports manifest entries — each export's
	// namespaced cmake target name surfaces as an Imported=true
	// TargetInfo with FileLocation derived from the
	// IMPORTED_LOCATION-captured LinkPaths. Local-codemodel
	// entries win on name collision (the manifest is a fallback
	// for cmake names not in the codemodel).
	foldImportedTargets(out, imports)
	return out
}

// foldInterfaceLibraryProbes folds probe-captured INTERFACE_*
// aggregates and the OBJECT_LIBRARY Objects list into out. Non-empty
// probe values override any convert-time aggregate already in the
// entry (cmake's own generation-time evaluator is the source of
// truth when it ran).
//
// Probes for targets not already in out are normally skipped — the
// codemodel is ground truth for "what targets exist" — with ONE
// exception: INTERFACE_LIBRARY targets, which cmake's File API
// deliberately omits from targets[] (they have no link step to
// model). Their probe is the only structured record of the resolved
// INTERFACE_* aggregates, and lowerInterfaceLibraries (which
// synthesizes their cc_library from the trace) needs it to reconcile
// genex-bearing INTERFACE_COMPILE_DEFINITIONS. So add a fresh entry
// for an INTERFACE_LIBRARY probe rather than dropping it.
func foldInterfaceLibraryProbes(out map[string]genexeval.TargetInfo, probes []cmakerun.GenexProbe) {
	for _, p := range probes {
		ti, ok := out[p.Name]
		if !ok {
			if p.Type != "INTERFACE_LIBRARY" {
				continue
			}
			ti = genexeval.TargetInfo{Type: p.Type}
		}
		ti.Objects = p.Objects
		if v, ok := p.Interface["INCLUDE_DIRECTORIES"]; ok {
			ti.InterfaceIncludeDirectories = v
		}
		if v, ok := p.Interface["COMPILE_DEFINITIONS"]; ok {
			ti.InterfaceCompileDefinitions = v
		}
		if v, ok := p.Interface["COMPILE_OPTIONS"]; ok {
			ti.InterfaceCompileOptions = v
		}
		if v, ok := p.Interface["LINK_LIBRARIES"]; ok {
			ti.InterfaceLinkLibraries = v
		}
		if v, ok := p.Interface["LINK_OPTIONS"]; ok {
			ti.InterfaceLinkOptions = v
		}
		out[p.Name] = ti
	}
}

// foldImportedTargets adds an Imported=true TargetInfo per
// imports.Resolver export, keyed by the namespaced cmake
// target name (e.g. `Foo::bar`). Skips names already present
// in `out` — the local codemodel wins. FileLocation is
// populated from the export's first LinkPath when present:
// cmake's `$<TARGET_FILE:Foo::bar>` at recording time resolves
// to IMPORTED_LOCATION_<CONFIG>, which the orchestrator side
// captured in LinkPaths. An empty LinkPaths leaves FileLocation
// empty; the byte-equal check fails for those, the (a) lift
// refuses, and the call falls through to (b)/legacy via the
// existing fallthrough rules. (The upstream
// unresolvedCrossPackageTargetFiles gate doesn't fire because
// the manifest does carry the name — just not its location.)
func foldImportedTargets(out map[string]genexeval.TargetInfo, imports *manifest.Resolver) {
	if imports == nil {
		return
	}
	for _, ex := range imports.AllExports() {
		if ex == nil || ex.CMakeTarget == "" {
			continue
		}
		if _, present := out[ex.CMakeTarget]; present {
			continue
		}
		var fileLoc string
		if len(ex.LinkPaths) > 0 {
			fileLoc = ex.LinkPaths[0]
		}
		out[ex.CMakeTarget] = genexeval.TargetInfo{
			Imported:     true,
			FileLocation: fileLoc,
		}
	}
}

// aggregatedInterface holds the post-walk values for one
// target's four convert-time-aggregated INTERFACE_* properties.
// Values are semicolon-joined to match cmake's list-property
// serialization shape.
type aggregatedInterface struct {
	includeDirectories string
	compileDefinitions string
	compileOptions     string
	linkLibraries      string
}

// aggregateInterfaceProperties walks the codemodel's per-target
// Dependencies[] graph and accumulates each target's effective
// INTERFACE_* property values. The aggregation mirrors cmake's
// own generator-phase walk:
//
//   - For each target T, start with T's DIRECT INTERFACE_*
//     contribution (PUBLIC + INTERFACE arms of the corresponding
//     target_* call recorded in the trace).
//   - Walk T's Dependencies[] in fileapi's recorded order
//     (which mirrors target_link_libraries' arg order).
//   - For each dep D in scope, accumulate D's own aggregated
//     INTERFACE_* (recurse). Cycles (rare; cmake errors on
//     them but we don't want to spin) break on first re-entry
//     via the visited set.
//   - Deduplicate values across the walk while preserving
//     first-occurrence order — matches cmake's documented
//     "first wins" property accumulation behavior.
//
// Returns a per-target map keyed by target name. Targets not in
// the codemodel get no entry; targets present but with no direct
// nor transitive contribution get an entry with empty strings
// (distinguishing "we walked, found nothing" from "we couldn't
// walk").
func aggregateInterfaceProperties(r *fileapi.Reply, decoded *shadow.Decoded) map[string]aggregatedInterface {
	// Build a name → Target index up front; the dep walk looks
	// up dep names many times. Values are stored by copy
	// (r.Targets is map[string]Target keyed by fileapi target id;
	// taking &r.Targets[id] isn't allowed).
	byName := make(map[string]fileapi.Target, len(r.Targets))
	// id → name resolver for TargetDependency.Id (which uses
	// the cmake-internal `<name>::@<hash>` form for in-tree
	// targets — share the depLibNameFromId helper).
	idToName := make(map[string]string, len(r.Targets))
	for _, t := range r.Targets {
		if t.IsGeneratorProvided {
			continue
		}
		byName[t.Name] = t
		idToName[t.Id] = t.Name
	}

	// Per-target DIRECT contributions, from the trace. These
	// are the PUBLIC + INTERFACE arms of target_* calls (the
	// PRIVATE arm is consumed-only and doesn't propagate).
	directIncludes := directInterfaceFromIncludes(decoded.Includes)
	directDefines := directInterfaceFromCompile(decoded.CompileDefinitions)
	directOptions := directInterfaceFromCompile(decoded.CompileOptions)
	directLinks := directInterfaceFromLinks(decoded.Links)

	// Per-target dep-chain — the names to recurse on. cmake's
	// File API drops pure-INTERFACE_LIBRARY targets from
	// configurations[].targets[] entirely (verified against
	// cmake 3.28: an INTERFACE lib with no buildable artifact
	// is invisible to fileapi), so the codemodel's
	// Dependencies[] alone is insufficient for chains like
	// `base (INTERFACE) → mid (INTERFACE) → leaf (STATIC)`.
	// Trace-recorded target_link_libraries entries (in their
	// PUBLIC / INTERFACE arms, plus the legacy positional shape
	// cmake treats as PUBLIC) recover the full graph. Falls back
	// to the codemodel Dependencies[] order when the trace
	// recorded nothing for a target (offline replay; older cmake
	// builds without --trace-expand).
	depChain := buildDepChain(byName, idToName, decoded.Links)

	out := make(map[string]aggregatedInterface, len(byName))
	// Memoize per-target aggregates so a target referenced as
	// a dep by multiple consumers is walked once.
	memo := make(map[string]aggregatedInterface, len(byName))
	// Walk every target we know about — both codemodel entries
	// (byName) and any extra names the trace mentioned (e.g.
	// invisible INTERFACE_LIBRARY targets). Walking the union
	// keeps base / mid populated in `out` so the leaf's
	// recursion finds them via memo.
	walkNames := make(map[string]bool, len(byName)+len(depChain))
	for name := range byName {
		walkNames[name] = true
	}
	for name := range depChain {
		walkNames[name] = true
	}
	for name := range directIncludes {
		walkNames[name] = true
	}
	for name := range directDefines {
		walkNames[name] = true
	}
	for name := range directOptions {
		walkNames[name] = true
	}
	for name := range directLinks {
		walkNames[name] = true
	}
	for name := range walkNames {
		visiting := map[string]bool{}
		agg := walkAggregate(name, depChain, directIncludes, directDefines, directOptions, directLinks, memo, visiting)
		out[name] = agg
	}
	return out
}

// buildDepChain assembles the per-target dep-name list the
// aggregation walker recurses on. Priority:
//
//  1. Trace's target_link_libraries calls (PUBLIC + INTERFACE
//     arms + the legacy positional shape cmake treats as PUBLIC).
//     Covers INTERFACE_LIBRARY chains the codemodel hides.
//  2. Codemodel's Dependencies[] in order (preserved as the
//     fallback for targets without trace data).
//
// Returns names in cmake's documented first-listed-first
// order, deduped.
func buildDepChain(byName map[string]fileapi.Target, idToName map[string]string, links []shadow.TargetLinkCall) map[string][]string {
	out := map[string][]string{}
	// Seed from trace first.
	for _, call := range links {
		for _, grp := range call.Groups {
			if grp.Visibility != "PUBLIC" && grp.Visibility != "INTERFACE" && grp.Visibility != "" {
				continue
			}
			out[call.Target] = appendDedup(out[call.Target], grp.Libs)
		}
	}
	// Fall back to codemodel for targets the trace didn't
	// surface. Skip targets the trace already covered — the
	// trace's order is authoritative for chain semantics.
	for name, t := range byName {
		if _, present := out[name]; present {
			continue
		}
		var deps []string
		for _, d := range t.Dependencies {
			depName := idToName[d.Id]
			if depName == "" {
				depName = depLibNameFromId(d.Id)
			}
			if depName == "" || depName == name {
				continue
			}
			deps = append(deps, depName)
		}
		if len(deps) > 0 {
			out[name] = deps
		}
	}
	return out
}

// walkAggregate is the recursive worker for
// aggregateInterfaceProperties. Memoizes by target name. visiting
// tracks the in-flight call chain so a cycle (cmake itself
// rejects these but the codemodel may surface one in pathological
// fixtures) breaks rather than loops.
func walkAggregate(
	name string,
	depChain map[string][]string,
	directIncludes, directDefines, directOptions, directLinks map[string][]string,
	memo map[string]aggregatedInterface,
	visiting map[string]bool,
) aggregatedInterface {
	if cached, ok := memo[name]; ok {
		return cached
	}
	if visiting[name] {
		// Cycle: return empty for this branch; the upstream
		// frame keeps the partial contribution it already
		// accumulated. cmake errors on cycles; we just
		// terminate.
		return aggregatedInterface{}
	}
	visiting[name] = true
	defer delete(visiting, name)

	// Start with this target's DIRECT contribution.
	includes := dedupCopy(directIncludes[name])
	defines := dedupCopy(directDefines[name])
	options := dedupCopy(directOptions[name])
	links := dedupCopy(directLinks[name])

	// Walk the dep chain (trace-derived, falling back to
	// codemodel Dependencies[] order — see buildDepChain).
	// cmake's documented first-listed-first aggregation order.
	for _, depName := range depChain[name] {
		if depName == "" || depName == name {
			continue
		}
		depAgg := walkAggregate(depName, depChain, directIncludes, directDefines, directOptions, directLinks, memo, visiting)
		includes = appendDedup(includes, splitNonEmpty(depAgg.includeDirectories))
		defines = appendDedup(defines, splitNonEmpty(depAgg.compileDefinitions))
		options = appendDedup(options, splitNonEmpty(depAgg.compileOptions))
		links = appendDedup(links, splitNonEmpty(depAgg.linkLibraries))
	}

	agg := aggregatedInterface{
		includeDirectories: strings.Join(includes, ";"),
		compileDefinitions: strings.Join(defines, ";"),
		compileOptions:     strings.Join(options, ";"),
		linkLibraries:      strings.Join(links, ";"),
	}
	memo[name] = agg
	return agg
}

// directInterfaceFromIncludes extracts each target's DIRECT
// INTERFACE_INCLUDE_DIRECTORIES contribution from the decoded
// target_include_directories calls: the union of all PUBLIC +
// INTERFACE arms, preserving first-occurrence order (a dir
// listed under PUBLIC in one call then again under INTERFACE
// in another stays at its first position).
func directInterfaceFromIncludes(calls []shadow.TargetIncludeCall) map[string][]string {
	out := map[string][]string{}
	for _, call := range calls {
		for _, grp := range call.Groups {
			if grp.Visibility != "PUBLIC" && grp.Visibility != "INTERFACE" {
				continue
			}
			out[call.Target] = appendDedup(out[call.Target], grp.Dirs)
		}
	}
	return out
}

// directInterfaceFromCompile mirrors directInterfaceFromIncludes
// for target_compile_definitions / target_compile_options. The
// caller passes the relevant decoded slice (CompileDefinitions
// or CompileOptions).
func directInterfaceFromCompile(calls []shadow.TargetCompileCall) map[string][]string {
	out := map[string][]string{}
	for _, call := range calls {
		for _, grp := range call.Groups {
			if grp.Visibility != "PUBLIC" && grp.Visibility != "INTERFACE" {
				continue
			}
			out[call.Target] = appendDedup(out[call.Target], grp.Items)
		}
	}
	return out
}

// directInterfaceFromLinks extracts each target's DIRECT
// INTERFACE_LINK_LIBRARIES contribution from the decoded
// target_link_libraries calls: the union of PUBLIC + INTERFACE
// arms. The legacy positional shape (no keyword) is treated
// as PUBLIC per cmake's documented default semantics, so its
// libs also count as interface-propagating.
func directInterfaceFromLinks(calls []shadow.TargetLinkCall) map[string][]string {
	out := map[string][]string{}
	for _, call := range calls {
		for _, grp := range call.Groups {
			if grp.Visibility != "PUBLIC" && grp.Visibility != "INTERFACE" && grp.Visibility != "" {
				continue
			}
			out[call.Target] = appendDedup(out[call.Target], grp.Libs)
		}
	}
	return out
}

// appendDedup appends items not already in dst, preserving
// dst's order. Used by the aggregation walk to match cmake's
// "first-occurrence wins" property accumulation semantics.
func appendDedup(dst, items []string) []string {
	seen := make(map[string]bool, len(dst))
	for _, d := range dst {
		seen[d] = true
	}
	for _, it := range items {
		if it == "" {
			continue
		}
		if seen[it] {
			continue
		}
		seen[it] = true
		dst = append(dst, it)
	}
	return dst
}

// dedupCopy returns a deduped copy of src, preserving order
// (first occurrence wins). Empty strings are dropped.
func dedupCopy(src []string) []string {
	if len(src) == 0 {
		return nil
	}
	out := make([]string, 0, len(src))
	seen := map[string]bool{}
	for _, s := range src {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

// splitNonEmpty splits a semicolon-joined cmake list into its
// component entries, dropping empty pieces (cmake's list
// serialization can introduce empty strings at the edges when
// the input had a trailing `;`).
func splitNonEmpty(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ";")
	out := parts[:0]
	for _, p := range parts {
		if p == "" {
			continue
		}
		out = append(out, p)
	}
	return out
}

// targetFileOpPrefixes is the set of `$<...:` prefixes that
// trigger a `--target-file=<name>=$(location :name)` flag
// emission. All seven variants resolve via the same wire (the
// genexeval evaluator derives DIR / NAME / LINKER / SONAME
// from FileLocation at runtime), so the lifter only needs the
// referenced target names — the variant op chosen at the
// template site doesn't change what we pass to Bazel.
//
// The same set drives the cross-package soundness gate in
// unresolvedCrossPackageTargetFiles below: any of these ops
// referenced against a target absent from both the local
// codemodel and the imports manifest would otherwise embed
// the recording-machine absolute path via (b) fallback, which
// doesn't exist on Bazel's executor.
//
// Trailing `:` on each prefix disambiguates the shorter forms
// from the longer ones during the scan (e.g. `$<TARGET_FILE:`
// vs `$<TARGET_FILE_DIR:` — the latter's char-after-`E` is
// `_`, not `:`, so a search for the shorter prefix won't
// false-positive on the longer).
var targetFileOpPrefixes = []string{
	"$<TARGET_FILE:",
	"$<TARGET_FILE_DIR:",
	"$<TARGET_FILE_NAME:",
	"$<TARGET_LINKER_FILE:",
	"$<TARGET_LINKER_FILE_DIR:",
	"$<TARGET_LINKER_FILE_NAME:",
	"$<TARGET_SONAME_FILE:",
}

// targetObjectsOpPrefix is the `$<...:` prefix that triggers a
// `--target-objects=<name>=<paths>` flag emission. Distinct from
// the TARGET_FILE family above because (i) the wire shape is
// list-valued (cmake's `$<TARGET_OBJECTS:t>` resolves to a
// semicolon-separated list of .o paths for OBJECT_LIBRARY targets)
// and (ii) Bazel's `$(locations :t)` (plural) is what enumerates
// the .o paths, vs. `$(location :t)` (singular) for TARGET_FILE.
//
// Same cross-package soundness gate concerns apply: a TARGET_OBJECTS
// reference against a target absent from both the local codemodel
// AND the imports manifest can't lift safely. The
// unresolvedCrossPackageTargetFiles scan below includes this prefix
// so the refusal-stub treatment matches the TARGET_FILE family.
var targetObjectsOpPrefix = "$<TARGET_OBJECTS:"

// unresolvedCrossPackageTargetFiles is the soundness gate for
// cross-package `$<TARGET_FILE*:t>` references. It scans both
// the on-disk INPUT template (if HasInput) and the inline
// CONTENT body (if HasContent) for any of the seven
// target-file-family ops (targetFileOpPrefixes above), and
// returns the sorted unique names that resolve to NEITHER the
// local cmake codemodel (genexTargets) NOR the imports.json
// manifest. The caller uses a non-empty result to refuse the
// lift entirely — see the caller-site comment for the why.
//
// Sorted return order keeps the refusal-stub diagnostic
// byte-stable across runs (Go's map iteration is otherwise
// randomized).
func unresolvedCrossPackageTargetFiles(call shadow.FileGenerateCall, hostSrcDir, recordedSrcDir string, genexTargets map[string]genexeval.TargetInfo, imports *manifest.Resolver) []string {
	// Source the template body the same way buildFileGenerateGenrule
	// does shortly after this gate. Failure to source ⇒ no scan
	// possible ⇒ assume safe (the downstream legacy path will
	// surface the real error).
	var body []byte
	switch {
	case call.HasContent:
		body = []byte(call.Content)
	case call.HasInput:
		inAbs, _, ok := resolveTemplatePath(call.Input, hostSrcDir, recordedSrcDir)
		if !ok {
			return nil
		}
		b, err := os.ReadFile(inAbs)
		if err != nil {
			return nil
		}
		body = b
	default:
		return nil
	}

	seen := map[string]bool{}
	unresolved := map[string]bool{}
	// Scan the TARGET_FILE family AND TARGET_OBJECTS — same soundness
	// concern: an unresolved cross-package reference would embed the
	// recording-machine absolute path via the (b) fallback, which
	// doesn't exist on Bazel's executor.
	allPrefixes := append([]string{}, targetFileOpPrefixes...)
	allPrefixes = append(allPrefixes, targetObjectsOpPrefix)
	for _, prefix := range allPrefixes {
		rest := body
		for {
			i := bytes.Index(rest, []byte(prefix))
			if i < 0 {
				break
			}
			argStart := i + len(prefix)
			end := bytes.IndexByte(rest[argStart:], '>')
			if end < 0 {
				break
			}
			name := string(rest[argStart : argStart+end])
			rest = rest[argStart+end+1:]
			if name == "" || seen[name] {
				continue
			}
			seen[name] = true
			if _, inLocal := genexTargets[name]; inLocal {
				continue
			}
			if imports != nil && imports.LookupCMakeTarget(name) != nil {
				continue
			}
			unresolved[name] = true
		}
	}
	if len(unresolved) == 0 {
		return nil
	}
	out := make([]string, 0, len(unresolved))
	for n := range unresolved {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// extractTargetFileRefs walks template body for each of the
// `$<TARGET_FILE*:name>` / `$<TARGET_LINKER_FILE*:name>` /
// `$<TARGET_SONAME_FILE:name>` op forms and returns the union
// of unique target names in sorted order. Sorted iteration
// keeps the lifted cmd byte-stable. Uses a simple prefix scan
// per op rather than the full parser; v1 targets don't contain
// nested `$<...>` in their name slot (cmake target names are
// literals) so a plain rune scan to `>` suffices.
func extractTargetFileRefs(body []byte) []string {
	seen := map[string]bool{}
	for _, prefix := range targetFileOpPrefixes {
		rest := body
		for {
			i := bytes.Index(rest, []byte(prefix))
			if i < 0 {
				break
			}
			argStart := i + len(prefix)
			end := bytes.IndexByte(rest[argStart:], '>')
			if end < 0 {
				break
			}
			name := string(rest[argStart : argStart+end])
			if name != "" {
				seen[name] = true
			}
			rest = rest[argStart+end+1:]
		}
	}
	if len(seen) == 0 {
		return nil
	}
	names := make([]string, 0, len(seen))
	for n := range seen {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// resolveTargetFileLabels turns the sorted list of cmake target
// names referenced via `$<TARGET_FILE:t>` (and the six on-disk-
// path variants) into the per-target Bazel label the lifter
// will substitute for cmake's `$<TARGET_FILE>` at Bazel time
// via the cmake-configure-file --target-file flag. PR 2's
// resolved-lift wire.
//
// For each name:
//
//   - if genexTargets[name] exists → label is `:name`
//     (same-package; PR 1 behaviour unchanged).
//   - else if imports.LookupCMakeTarget(name) returns an
//     export → label is that export's full Bazel label
//     (`//elements/foo:bar`; PR 2's resolved-lift path).
//   - else → name is dropped from both the returned label
//     map and the returned src-list. The upstream
//     unresolvedCrossPackageTargetFiles gate would have already
//     turned the call into a refusal stub before reaching here;
//     this is the belt-and-suspenders default for the "never
//     should fire" case.
//
// Returns the label map plus the sorted set of cross-package
// labels the caller must add to genrule.srcs so
// `$(location //pkg:t)` resolves at Bazel time. Same-package
// `:name` labels resolve without an explicit srcs entry —
// Bazel finds them via package-internal lookup, so they're
// omitted from the srcs list.
func resolveTargetFileLabels(names []string, genexTargets map[string]genexeval.TargetInfo, imports *manifest.Resolver) (map[string]string, []string) {
	if len(names) == 0 {
		return nil, nil
	}
	labels := make(map[string]string, len(names))
	var crossPackage []string
	for _, name := range names {
		ti, inLocal := genexTargets[name]
		// The local-codemodel arm: present in genexTargets AND
		// not Imported (Imported=true entries come from the
		// imports manifest fold in buildGenexTargets — they
		// resolve to cross-package labels, not `:name`).
		if inLocal && !ti.Imported {
			labels[name] = ":" + name
			continue
		}
		// The manifest-resolved arm: PR 2's resolved-lift path.
		// Either the entry surfaced via imports.LookupCMakeTarget
		// directly, or it landed in genexTargets via the imports
		// fold (Imported=true). Both routes lead to the same
		// full Bazel label.
		if imports != nil {
			if ex := imports.LookupCMakeTarget(name); ex != nil {
				labels[name] = ex.BazelLabel
				crossPackage = append(crossPackage, ex.BazelLabel)
				continue
			}
		}
		// Neither local nor manifest-resolved: drop. The
		// refusal-stub gate above this point handles the
		// genuinely-unresolvable case; if we reach here for a
		// dropped name, the (a) eval will fail FileLocation
		// lookup → fall to (b)/legacy, which the latent-bug
		// gate already protects against.
	}
	if len(crossPackage) > 0 {
		sort.Strings(crossPackage)
	}
	return labels, crossPackage
}

// extractTargetObjectsRefs walks template body for `$<TARGET_OBJECTS:name>`
// references and returns the union of unique target names in
// sorted order. Sibling to extractTargetFileRefs but kept distinct
// because the lifted flag shape differs: --target-objects emits
// `$(locations :t)` (plural, list-valued) with a `tr ' ' ':'` shell
// rewrite, while --target-file emits the singular `$(location :t)`.
// One target referenced via N TARGET_OBJECTS occurrences collapses
// to one flag — the Bazel-time expansion is the same path list
// regardless of how many references the template carries.
func extractTargetObjectsRefs(body []byte) []string {
	seen := map[string]bool{}
	rest := body
	for {
		i := bytes.Index(rest, []byte(targetObjectsOpPrefix))
		if i < 0 {
			break
		}
		argStart := i + len(targetObjectsOpPrefix)
		end := bytes.IndexByte(rest[argStart:], '>')
		if end < 0 {
			break
		}
		name := string(rest[argStart : argStart+end])
		if name != "" {
			seen[name] = true
		}
		rest = rest[argStart+end+1:]
	}
	if len(seen) == 0 {
		return nil
	}
	names := make([]string, 0, len(seen))
	for n := range seen {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// buildGenexContext extracts the configure-time fields the (a)
// evaluator consults from the cmake variable dump. The Context
// is a thin projection over cmakeVars — the evaluator reads
// these specific keys, not the full namespace, so the lifted
// genrule's payload stays small.
//
// Field sources (cmake's documented variable names):
//
//   - Config       <- CMAKE_BUILD_TYPE
//   - PlatformID   <- CMAKE_SYSTEM_NAME
//   - CompilerID   <- CMAKE_<LANG>_COMPILER_ID for each language
//     in the dump (C, CXX, OBJC, OBJCXX, Fortran,
//     ASM, ...)
//   - CompilerLanguage stays empty — file(GENERATE) is language-
//     agnostic; the evaluator picks the first
//     entry in CompilerID if a `$<COMPILER_ID>`
//     genex appears.
//
// Returns a zero Context when cmakeVars is nil; the evaluator
// will refuse most ops via UnsupportedError and the lifter
// falls through to (b) / legacy.
func buildGenexContext(cmakeVars map[string]string, targets map[string]genexeval.TargetInfo) genexeval.Context {
	if cmakeVars == nil && targets == nil {
		return genexeval.Context{}
	}
	ctx := genexeval.Context{
		Config:     cmakeVars["CMAKE_BUILD_TYPE"],
		PlatformID: cmakeVars["CMAKE_SYSTEM_NAME"],
		Targets:    targets,
	}
	for k, v := range cmakeVars {
		if !strings.HasPrefix(k, "CMAKE_") || !strings.HasSuffix(k, "_COMPILER_ID") {
			continue
		}
		// Extract the language: CMAKE_<LANG>_COMPILER_ID.
		lang := strings.TrimSuffix(strings.TrimPrefix(k, "CMAKE_"), "_COMPILER_ID")
		if lang == "" || v == "" {
			continue
		}
		if ctx.CompilerID == nil {
			ctx.CompilerID = map[string]string{}
		}
		ctx.CompilerID[lang] = v
	}
	return ctx
}

// marshalGenexContext emits ctx in the wire shape both sides
// share — genexeval.Context's struct json tags carry the
// snake_case keys and FileLocation is json:"-" so per-machine
// paths never land in the lifted cmd. The function is a thin
// wrapper to keep the call site readable; json.Marshal is the
// load-bearing primitive.
func marshalGenexContext(ctx genexeval.Context) ([]byte, error) {
	return json.Marshal(ctx)
}

// resolveGenexInPath parses path as a genex-bearing string and
// evaluates each genex against ctx. Returns (resolved, true) if
// every genex resolves cleanly (no UnsupportedError, no parse
// error); (path, false) otherwise — caller drops the call.
//
// Used at the OUTPUT-side of file(GENERATE) and INPUT-arg
// resolution paths where the genex appears in a filename
// rather than in template body. The evaluator's typed refusal
// for target-evaluator-dependent ops surfaces here as "false"
// just like at the body site; the call is dropped (no Bazel
// shape can carry a Bazel-time-dynamic output filename).
func resolveGenexInPath(path string, ctx genexeval.Context) (string, bool) {
	nodes, err := genexeval.Parse([]byte(path))
	if err != nil {
		return path, false
	}
	out, err := genexeval.Eval(nodes, ctx)
	if err != nil {
		return path, false
	}
	if hasGenex(out) {
		// Defensive: a malformed Eval result that still carries
		// `$<...>` markers can't be used as an output path.
		// Shouldn't happen with the v1 evaluator (Parse+Eval
		// either resolves cleanly or returns an error), but the
		// belt-and-suspenders check avoids a downstream srckey
		// surprise if a future evaluator extension is buggy.
		return path, false
	}
	return string(out), true
}
