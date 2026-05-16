package lower

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/sstriker/buildstream-bazel/converter/internal/fileapi"
	"github.com/sstriker/buildstream-bazel/converter/ir"
	"github.com/sstriker/buildstream-bazel/internal/configurefile"
	"github.com/sstriker/buildstream-bazel/internal/genexeval"
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
// cmake-codegen-file-generate-genex tag — the verify-pass
// would fail anyway since configurefile.Substitute doesn't
// evaluate genexes, but we keep the explicit short-circuit so
// the audit signal distinguishes "lift skipped because of
// genex" from "lift failed because values weren't recoverable".
//
// Returns an empty slice with no error when calls is empty or
// hostBuildDir is unset — preserves the pre-trace behavior for
// offline runs without a stashed fixture.
func recoverFileGenerate(calls []shadow.FileGenerateCall, hostSrcDir, recordedSrcDir, hostBuildDir, recordedBuildDir string, liftEnabled bool, cmakeVars map[string]string, genexTargets map[string]genexeval.TargetInfo, cc *codegenContext) ([]fileGenerateOut, error) {
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
				continue
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
		gen := buildFileGenerateGenrule(name, rel, body, call, hostSrcDir, recordedSrcDir, liftEnabled, cmakeVars, genexTargets)
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
// (template body decoupled from BUILD.bazel content) and the
// legacy shape (rendered bytes base64-embedded in cmd). Lift
// requires:
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
// Falls back to legacy otherwise. The genex short-circuit
// happens before pickValues so the audit tag tells the operator
// which exit fired.
func buildFileGenerateGenrule(name, outRel string, rendered []byte, call shadow.FileGenerateCall, hostSrcDir, recordedSrcDir string, liftEnabled bool, cmakeVars map[string]string, genexTargets map[string]genexeval.TargetInfo) ir.Target {
	opts, optErr := fileGenerateOptions(call)
	legacy := ir.Target{
		Name:        name,
		Kind:        ir.KindGenrule,
		GenruleCmd:  configureFileLegacyCmd(outRel, rendered),
		GenruleOuts: []string{outRel},
		Tags:        fileGenerateTags(false, false, false, false),
		Visibility:  []string{"//visibility:private"},
	}
	if optErr != nil {
		return legacy
	}
	if !liftEnabled {
		return legacy
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
		// cmake-codegen-file-generate-genex audit tag (same
		// exit as the pre-evaluator gate).
		if hasGenex([]byte(call.Input)) {
			resolved, ok := resolveGenexInPath(call.Input, buildGenexContext(cmakeVars, genexTargets))
			if !ok {
				genexLegacy := legacy
				genexLegacy.Tags = fileGenerateTags(false, true, false, false)
				return genexLegacy
			}
			call.Input = resolved
		}
		templatePath, rel, ok := resolveTemplatePath(call.Input, hostSrcDir, recordedSrcDir)
		if !ok {
			return legacy
		}
		body, err := os.ReadFile(templatePath)
		if err != nil {
			return legacy
		}
		templateBody = body
		inRel = rel
	case call.HasContent:
		templateBody = []byte(call.Content)
		isContentForm = true
	default:
		return legacy
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
	//      cmake-codegen-file-generate-genex audit tag —
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
		// target op (TARGET_PROPERTY or TARGET_FILE). Avoids
		// dumping every per-target dict into the lifted cmd
		// for templates that only reference CONFIG /
		// PLATFORM_ID / etc. — the (a) lift's payload would
		// otherwise grow linearly with target count for no
		// benefit.
		ctxTargets := genexTargets
		needsTargets := bytes.Contains(templateBody, []byte("$<TARGET_PROPERTY:")) ||
			bytes.Contains(templateBody, []byte("$<TARGET_FILE:"))
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
		targetFileRefs := extractTargetFileRefs(templateBody)
		ctx := buildGenexContext(cmakeVars, ctxTargets)
		if nodes, err := genexeval.Parse(templateBody); err == nil {
			if evaled, evalErr := genexeval.Eval(nodes, ctx); evalErr == nil && bytes.Equal(evaled, rendered) {
				cmd, cmdErr := fileGenerateEvaluatorCmd(inRel, templateBody, ctx, targetFileRefs, opts, isContentForm)
				if cmdErr == nil {
					target := ir.Target{
						Name:         name,
						Kind:         ir.KindGenrule,
						GenruleCmd:   cmd,
						GenruleOuts:  []string{outRel},
						GenruleTools: []string{"//tools:cmake-configure-file"},
						Tags:         fileGenerateTags(true, false, false, true),
						Visibility:   []string{"//visibility:private"},
					}
					if !isContentForm {
						target.Srcs = []string{inRel}
					}
					return target
				}
			}
			// evalErr being UnsupportedError or a Context-
			// missing-field error is the expected refusal for
			// templates outside the v1 subset; fall through to
			// (b). Other errors (parse internals, byte-mismatch)
			// also fall through — soundness preserved by the
			// downstream lift choice.
		}

		genexValues, err := extractGenexValues(templateBody, rendered)
		if err == nil {
			cmd, cmdErr := fileGenerateLiftedCmd(inRel, templateBody, map[string]string{}, genexValues, opts, isContentForm)
			if cmdErr == nil {
				target := ir.Target{
					Name:         name,
					Kind:         ir.KindGenrule,
					GenruleCmd:   cmd,
					GenruleOuts:  []string{outRel},
					GenruleTools: []string{"//tools:cmake-configure-file"},
					Tags:         fileGenerateTags(true, false, true, false),
					Visibility:   []string{"//visibility:private"},
				}
				if !isContentForm {
					target.Srcs = []string{inRel}
				}
				return target
			}
		}
		genexLegacy := legacy
		genexLegacy.Tags = fileGenerateTags(false, true, false, false)
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
		return legacy
	}
	values := map[string]string{}

	cmd, err := fileGenerateLiftedCmd(inRel, templateBody, values, nil, opts, isContentForm)
	if err != nil {
		return legacy
	}
	target := ir.Target{
		Name:         name,
		Kind:         ir.KindGenrule,
		GenruleCmd:   cmd,
		GenruleOuts:  []string{outRel},
		GenruleTools: []string{"//tools:cmake-configure-file"},
		Tags:         fileGenerateTags(true, false, false, false),
		Visibility:   []string{"//visibility:private"},
	}
	if !isContentForm {
		target.Srcs = []string{inRel}
	}
	return target
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

// fileGenerateLiftedCmd builds the lifted shell command. For
// the INPUT form (isContentForm=false), the template lives in
// srcs via $(location inRel) — same shape as
// configureFileLiftedCmd. For the CONTENT form
// (isContentForm=true), the template body rides inline in the
// cmd as --content-base64 — no srcs entry, but the body itself
// (not the rendered output) is what appears in BUILD.bazel,
// so edits to values still re-render without a BUILD.bazel
// edit.
//
// genexValues is the "structured base64" (b) lift's payload:
// a map of each top-level `$<...>` literal in template to the
// cmake-resolved bytes. Empty or nil means no genex replay
// (the non-genex code path). Non-empty stages a second sidecar
// JSON file alongside VALUES and threads `--genex-values=` at
// the tool. Same mktemp + trap cleanup pattern keeps the
// sidecar in the action's sandbox.
//
// VALUES staging mirrors configureFileLiftedCmd's portable
// mktemp + trap-style cleanup; ensures the values JSON lives
// in the action's sandbox, not /tmp.
func fileGenerateLiftedCmd(inRel string, template []byte, values, genexValues map[string]string, opts configurefile.Options, isContentForm bool) (string, error) {
	body, err := json.Marshal(values)
	if err != nil {
		return "", fmt.Errorf("marshal values: %w", err)
	}
	valuesEnc := base64.StdEncoding.EncodeToString(body)
	flags := configureFileToolFlags(opts)

	// Stage GENEX_VALUES only when there's a payload — keeps the
	// non-genex cmd shape byte-stable with the pre-(b)-lift cmd
	// so the existing file(GENERATE) goldens don't rewrite.
	genexPrep, genexFlag, genexCleanup := "", "", ""
	if len(genexValues) > 0 {
		genexBody, err := json.Marshal(genexValues)
		if err != nil {
			return "", fmt.Errorf("marshal genex values: %w", err)
		}
		genexEnc := base64.StdEncoding.EncodeToString(genexBody)
		genexPrep = fmt.Sprintf(
			`GENEX_VALUES="$$(mktemp "$$(dirname "$@")/cmake-configure-file.genex.XXXXXX")" && `+
				`echo %s | base64 -d > "$$GENEX_VALUES" && `,
			genexEnc,
		)
		genexFlag = `--genex-values="$$GENEX_VALUES" `
		genexCleanup = ` [ -n "$${GENEX_VALUES:-}" ] && rm -f "$$GENEX_VALUES";`
	}

	if isContentForm {
		contentEnc := base64.StdEncoding.EncodeToString(template)
		return fmt.Sprintf(
			`mkdir -p "$$(dirname "$@")" && `+
				`VALUES="$$(mktemp "$$(dirname "$@")/cmake-configure-file.values.XXXXXX")" && `+
				`echo %s | base64 -d > "$$VALUES" && `+
				`%s`+
				`$(location //tools:cmake-configure-file) %s%s--values="$$VALUES" --content-base64=%s "$@" ; `+
				`rc=$$?; [ -n "$${VALUES:-}" ] && rm -f "$$VALUES";%s exit $$rc`,
			valuesEnc, genexPrep, flags, genexFlag, contentEnc, genexCleanup,
		), nil
	}

	return fmt.Sprintf(
		`mkdir -p "$$(dirname "$@")" && `+
			`VALUES="$$(mktemp "$$(dirname "$@")/cmake-configure-file.values.XXXXXX")" && `+
			`echo %s | base64 -d > "$$VALUES" && `+
			`%s`+
			`$(location //tools:cmake-configure-file) %s%s--values="$$VALUES" "$(location %s)" "$@" ; `+
			`rc=$$?; [ -n "$${VALUES:-}" ] && rm -f "$$VALUES";%s exit $$rc`,
		valuesEnc, genexPrep, flags, genexFlag, inRel, genexCleanup,
	), nil
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
func fileGenerateTags(lifted, genexFallback, genexCaptured, genexEvaluated bool) []string {
	tags := []string{
		"cmake-codegen",
		"cmake-codegen-driver=file_generate",
		"cmake-codegen-file-generate",
	}
	if lifted {
		tags = append(tags, "cmake-codegen-lifted")
	}
	if genexFallback {
		tags = append(tags, "cmake-codegen-file-generate-genex")
	}
	if genexCaptured {
		tags = append(tags, "cmake-codegen-file-generate-genex-lifted")
	}
	if genexEvaluated {
		tags = append(tags, "cmake-codegen-file-generate-genex-evaluated")
	}
	sort.Strings(tags)
	return tags
}

// buildGenexTargets projects the fileapi codemodel's per-target
// data into the genexeval.TargetInfo map the evaluator
// consults for `$<TARGET_PROPERTY:t,p>`. Keyed by the target's
// cmake name (not its fileapi ID). Captures only the
// properties the v1 evaluator supports verbatim (Type / Sources
// / Imported); INTERFACE_* aggregation isn't modeled here so
// queries against those properties surface as UnsupportedError
// from the evaluator. cmake-internal helper targets
// (ZERO_CHECK / INSTALL / PACKAGE / ...) are skipped — they
// have no Bazel equivalent and the user-authored CMakeLists
// shouldn't reference them via TARGET_PROPERTY.
//
// Returns nil when r is nil or has no usable targets — the
// evaluator's UnsupportedError on missing-target surfaces
// cleanly and routes the lift to (b) / legacy.
func buildGenexTargets(r *fileapi.Reply, recordedBuildDir string) map[string]genexeval.TargetInfo {
	if r == nil || len(r.Targets) == 0 {
		return nil
	}
	out := make(map[string]genexeval.TargetInfo, len(r.Targets))
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
	return out
}

// extractTargetFileRefs walks template body for `$<TARGET_FILE:name>`
// occurrences and returns the unique target names in sorted
// order. Sorted iteration keeps the lifted cmd byte-stable.
// Uses a simple prefix scan rather than the full parser since
// only the literal `$<TARGET_FILE:` prefix matters — false
// positives (a `$<TARGET_FILE:` inside a longer op like
// `$<TARGET_FILE_NAME:`) are caught by the next char check.
func extractTargetFileRefs(body []byte) []string {
	const prefix = "$<TARGET_FILE:"
	seen := map[string]bool{}
	rest := body
	for {
		i := bytes.Index(rest, []byte(prefix))
		if i < 0 {
			break
		}
		// Scan forward for the closing `>` at depth 0 from the
		// arg start. v1 targets don't contain nested `$<...>`
		// in their name slot (cmake target names are literals);
		// a plain rune scan to `>` suffices.
		argStart := i + len(prefix)
		end := bytes.IndexByte(rest[argStart:], '>')
		if end < 0 {
			break
		}
		name := string(rest[argStart : argStart+end])
		if name != "" && !seen[name] {
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

// fileGenerateEvaluatorCmd is the (a)-shape companion to
// fileGenerateLiftedCmd. The shape is identical except for the
// genex payload: instead of a literal-replace map staged at
// $GENEX_VALUES, the Context lands at $GENEX_CONTEXT and
// cmake-configure-file receives --genex-context=. The
// genexeval evaluator does the substitution at Bazel time
// against that Context.
//
// targetFileRefs is the sorted set of target names the template
// references via `$<TARGET_FILE:name>`. The lifter resolves
// each to a Bazel `$(location :name)` substitution at action-
// time; the resulting flags accumulate as
// `--target-file=name=$(location :name)` per reference and
// override the marshaled Context's FileLocation (which is
// always wire-omitted to keep the lifted cmd byte-stable
// across recording machines).
//
// Like fileGenerateLiftedCmd, --values stays empty for
// file(GENERATE) (no @VAR@/${VAR}/#cmakedefine surface).
func fileGenerateEvaluatorCmd(inRel string, template []byte, ctx genexeval.Context, targetFileRefs []string, opts configurefile.Options, isContentForm bool) (string, error) {
	emptyValues, err := json.Marshal(map[string]string{})
	if err != nil {
		return "", fmt.Errorf("marshal values: %w", err)
	}
	valuesEnc := base64.StdEncoding.EncodeToString(emptyValues)
	flags := configureFileToolFlags(opts)

	ctxJSON, err := marshalGenexContext(ctx)
	if err != nil {
		return "", fmt.Errorf("marshal genex context: %w", err)
	}
	ctxEnc := base64.StdEncoding.EncodeToString(ctxJSON)

	ctxPrep := fmt.Sprintf(
		`GENEX_CONTEXT="$$(mktemp "$$(dirname "$@")/cmake-configure-file.ctx.XXXXXX")" && `+
			`echo %s | base64 -d > "$$GENEX_CONTEXT" && `,
		ctxEnc,
	)
	ctxFlag := `--genex-context="$$GENEX_CONTEXT" `
	// --target-file flags for each $<TARGET_FILE:t> reference.
	// v1 assumes same-package targets — the Bazel label is `:t`.
	// Sorted iteration so the cmd is stable across runs.
	var targetFileFlags strings.Builder
	for _, name := range targetFileRefs {
		fmt.Fprintf(&targetFileFlags, `--target-file=%s="$(location :%s)" `, name, name)
	}
	ctxCleanup := ` [ -n "$${GENEX_CONTEXT:-}" ] && rm -f "$$GENEX_CONTEXT";`

	if isContentForm {
		contentEnc := base64.StdEncoding.EncodeToString(template)
		return fmt.Sprintf(
			`mkdir -p "$$(dirname "$@")" && `+
				`VALUES="$$(mktemp "$$(dirname "$@")/cmake-configure-file.values.XXXXXX")" && `+
				`echo %s | base64 -d > "$$VALUES" && `+
				`%s`+
				`$(location //tools:cmake-configure-file) %s%s%s--values="$$VALUES" --content-base64=%s "$@" ; `+
				`rc=$$?; [ -n "$${VALUES:-}" ] && rm -f "$$VALUES";%s exit $$rc`,
			valuesEnc, ctxPrep, flags, ctxFlag, targetFileFlags.String(), contentEnc, ctxCleanup,
		), nil
	}

	return fmt.Sprintf(
		`mkdir -p "$$(dirname "$@")" && `+
			`VALUES="$$(mktemp "$$(dirname "$@")/cmake-configure-file.values.XXXXXX")" && `+
			`echo %s | base64 -d > "$$VALUES" && `+
			`%s`+
			`$(location //tools:cmake-configure-file) %s%s%s--values="$$VALUES" "$(location %s)" "$@" ; `+
			`rc=$$?; [ -n "$${VALUES:-}" ] && rm -f "$$VALUES";%s exit $$rc`,
		valuesEnc, ctxPrep, flags, ctxFlag, targetFileFlags.String(), inRel, ctxCleanup,
	), nil
}

// marshalGenexContext mirrors cmake-configure-file's
// genexContextJSON shape — flat snake_case keys so the wire
// representation is the same on both sides. Empty fields are
// omitted to keep the base64-encoded blob small (typical
// Context is ~50-100 bytes after compaction).
//
// Targets, when present, ships as a nested map; only the
// fields the v1 evaluator consults (Type / Sources / Imported)
// land on the wire so future Context extensions don't bloat
// the payload until the matching evaluator support lands.
func marshalGenexContext(ctx genexeval.Context) ([]byte, error) {
	type targetJSON struct {
		Type     string `json:"type,omitempty"`
		Sources  string `json:"sources,omitempty"`
		Imported bool   `json:"imported,omitempty"`
	}
	type contextJSON struct {
		Config           string                `json:"config,omitempty"`
		CompilerID       map[string]string     `json:"compiler_id,omitempty"`
		PlatformID       string                `json:"platform_id,omitempty"`
		CompilerLanguage string                `json:"compiler_language,omitempty"`
		Targets          map[string]targetJSON `json:"targets,omitempty"`
	}
	var targets map[string]targetJSON
	if len(ctx.Targets) > 0 {
		targets = make(map[string]targetJSON, len(ctx.Targets))
		for name, t := range ctx.Targets {
			targets[name] = targetJSON{
				Type:     t.Type,
				Sources:  t.Sources,
				Imported: t.Imported,
			}
		}
	}
	return json.Marshal(contextJSON{
		Config:           ctx.Config,
		CompilerID:       ctx.CompilerID,
		PlatformID:       ctx.PlatformID,
		CompilerLanguage: ctx.CompilerLanguage,
		Targets:          targets,
	})
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
