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

	"github.com/sstriker/cmake-to-bazel/converter/ir"
	"github.com/sstriker/cmake-to-bazel/internal/configurefile"
	"github.com/sstriker/cmake-to-bazel/internal/shadow"
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
func recoverFileGenerate(calls []shadow.FileGenerateCall, hostSrcDir, recordedSrcDir, hostBuildDir, recordedBuildDir string, liftEnabled bool, cmakeVars map[string]string, cc *codegenContext) ([]fileGenerateOut, error) {
	if len(calls) == 0 || hostBuildDir == "" {
		return nil, nil
	}

	var out []fileGenerateOut
	seenRel := map[string]bool{}
	for _, call := range calls {
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
		gen := buildFileGenerateGenrule(name, rel, body, call, hostSrcDir, recordedSrcDir, liftEnabled, cmakeVars)
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
func buildFileGenerateGenrule(name, outRel string, rendered []byte, call shadow.FileGenerateCall, hostSrcDir, recordedSrcDir string, liftEnabled bool, cmakeVars map[string]string) ir.Target {
	opts, optErr := fileGenerateOptions(call)
	legacy := ir.Target{
		Name:        name,
		Kind:        ir.KindGenrule,
		GenruleCmd:  configureFileLegacyCmd(outRel, rendered),
		GenruleOuts: []string{outRel},
		Tags:        fileGenerateTags(false, false),
		Visibility:  []string{"//visibility:private"},
	}
	if optErr != nil {
		return legacy
	}
	if !liftEnabled {
		return legacy
	}

	// Source the template body. Exactly one of HasInput /
	// HasContent is true on a well-formed call (the extractor
	// enforces that); a defensive prefer-Input mirrors
	// configure_file's INPUT-form shape when both are
	// accidentally set. Keyword-presence (not value-emptiness)
	// is the discriminator: `file(GENERATE CONTENT "")` is a
	// legitimate empty-file emission and the lifter routes it
	// through the CONTENT form so `--content-base64=<empty>`
	// carries the empty body to the Bazel-time tool.
	var templateBody []byte
	var inRel string // package-relative path; empty for the CONTENT form
	isContentForm := false
	switch {
	case call.HasInput:
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

	// Generator expression in the template → skip lift, tag
	// the legacy fallback so the audit can find it.
	if hasGenex(templateBody) {
		genexLegacy := legacy
		genexLegacy.Tags = fileGenerateTags(false, true)
		return genexLegacy
	}

	values, ok := pickValues(templateBody, rendered, opts, cmakeVars)
	if !ok {
		return legacy
	}

	cmd, err := fileGenerateLiftedCmd(inRel, templateBody, values, opts, isContentForm)
	if err != nil {
		return legacy
	}
	target := ir.Target{
		Name:         name,
		Kind:         ir.KindGenrule,
		GenruleCmd:   cmd,
		GenruleOuts:  []string{outRel},
		GenruleTools: []string{"//tools:cmake-configure-file"},
		Tags:         fileGenerateTags(true, false),
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
// Unlike configure_file, file(GENERATE) has no @ONLY /
// COPYONLY / ESCAPE_QUOTES — cmake's docs don't list them for
// the GENERATE subcommand. configurefile.Options' other fields
// default to zero, which matches the @VAR@-and-${VAR} both-on,
// no-escape, no-copy-only default.
func fileGenerateOptions(call shadow.FileGenerateCall) (configurefile.Options, error) {
	out := configurefile.Options{}
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
// lift; the verify-pass in pickValues catches those, so this
// is a fast-path short-circuit rather than the soundness gate.
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
// VALUES staging mirrors configureFileLiftedCmd's portable
// mktemp + trap-style cleanup; ensures the values JSON lives
// in the action's sandbox, not /tmp.
func fileGenerateLiftedCmd(inRel string, template []byte, values map[string]string, opts configurefile.Options, isContentForm bool) (string, error) {
	body, err := json.Marshal(values)
	if err != nil {
		return "", fmt.Errorf("marshal values: %w", err)
	}
	valuesEnc := base64.StdEncoding.EncodeToString(body)
	flags := configureFileToolFlags(opts)

	if isContentForm {
		contentEnc := base64.StdEncoding.EncodeToString(template)
		return fmt.Sprintf(
			`mkdir -p "$$(dirname "$@")" && `+
				`VALUES="$$(mktemp "$$(dirname "$@")/cmake-configure-file.values.XXXXXX")" && `+
				`echo %s | base64 -d > "$$VALUES" && `+
				`$(location //tools:cmake-configure-file) %s--values="$$VALUES" --content-base64=%s "$@" ; `+
				`rc=$$?; [ -n "$${VALUES:-}" ] && rm -f "$$VALUES"; exit $$rc`,
			valuesEnc, flags, contentEnc,
		), nil
	}

	return fmt.Sprintf(
		`mkdir -p "$$(dirname "$@")" && `+
			`VALUES="$$(mktemp "$$(dirname "$@")/cmake-configure-file.values.XXXXXX")" && `+
			`echo %s | base64 -d > "$$VALUES" && `+
			`$(location //tools:cmake-configure-file) %s--values="$$VALUES" "$(location %s)" "$@" ; `+
			`rc=$$?; [ -n "$${VALUES:-}" ] && rm -f "$$VALUES"; exit $$rc`,
		valuesEnc, flags, inRel,
	), nil
}

// fileGenerateTags returns the cmake-codegen tag set for a
// file(GENERATE) emission. Distinguishes from configure_file
// via cmake-codegen-driver=file_generate so audit queries can
// split the two cleanly. The lifted facet ("cmake-codegen-
// lifted") and genex-fallback facet ("cmake-codegen-file-
// generate-genex") are mutually exclusive in v1: a lifted
// genrule by construction has no genex; a genex-bearing
// template falls back to legacy.
func fileGenerateTags(lifted, genexFallback bool) []string {
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
	sort.Strings(tags)
	return tags
}
