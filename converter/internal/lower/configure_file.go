package lower

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/sstriker/buildstream-bazel/converter/ir"
	"github.com/sstriker/buildstream-bazel/internal/configurefile"
	"github.com/sstriker/buildstream-bazel/internal/shadow"
)

// configureFileOut is one recovered configure_file emission: the
// recorded absolute path of the output (used to associate the
// output with consuming targets via build-dir include matching)
// and the build-dir-relative path the genrule writes the file at
// (used as the package-relative path consumers reference in
// hdrs/srcs).
type configureFileOut struct {
	AbsOutput string // recording-machine absolute path: ${cmakeBuild}/<rel>
	RelOutput string // <rel>: build-dir-relative path; package-relative in the BUILD file
	// ExportHeader marks a generate_export_header output (template
	// exportheader.cmake.in). cmake generates it into CMAKE_CURRENT_BINARY_DIR
	// and puts that dir on the target's include path, so consumers #include the
	// header by BARE name (`#include "<name>_export.h"`). The consumer
	// attribution therefore must add the output's OWN directory to the target's
	// includes — the generic hosting-include match can pick a shallower parent
	// dir, which leaves the bare include unresolved.
	ExportHeader bool
}

// recoverConfigureFiles walks the trace's configure_file events
// and emits one Bazel target per call: a cmake_configure_file lift
// (re-renders the .h.in template at Bazel time) when the template +
// values resolve, else a bake target (the rendered bytes via
// write_file / base64 genrule). Returns the list of recovered outputs
// so lowerTarget can attach them to consuming targets.
//
// hostBuildDir is the host-real path of the cmake build dir
// (where configured outputs live on this machine);
// recordedBuildDir is the path cmake itself recorded in the
// trace (= r.Codemodel.Paths.Build). They differ in offline
// tests where the recording machine wrote the trace and this
// machine doesn't have that path. We strip recordedBuildDir
// from the trace's output path to get a relative path, then
// re-anchor to hostBuildDir for the actual byte read.
//
// Returns an empty slice with no error when traceRaw is empty
// or no configure_file events are recorded — preserves the
// pre-trace behavior for offline runs without a stashed
// fixture.
func recoverConfigureFiles(traceRaw []byte, hostSrcDir, hostBuildDir, recordedSrcDir, recordedBuildDir string, liftEnabled bool, cmakeVars map[string]string, cc *codegenContext) ([]configureFileOut, error) {
	if len(traceRaw) == 0 || hostBuildDir == "" {
		return nil, nil
	}
	if hostSrcDir == "" {
		hostSrcDir = recordedSrcDir
	}
	return recoverConfigureFilesFromCalls(shadow.ExtractConfigureFiles(traceRaw, recordedSrcDir), hostSrcDir, recordedSrcDir, hostBuildDir, recordedBuildDir, liftEnabled, cmakeVars, cc)
}

// recoverConfigureFilesFromCalls is the same logic as
// recoverConfigureFiles but takes pre-decoded ConfigureFileCall
// records. Used by Lower's single-pass trace dispatch so the
// trace is parsed once total across all extractors (including
// the configure_file recovery), instead of one pass per
// extractor.
func recoverConfigureFilesFromCalls(calls []shadow.ConfigureFileCall, hostSrcDir, recordedSrcDir, hostBuildDir, recordedBuildDir string, liftEnabled bool, cmakeVars map[string]string, cc *codegenContext) ([]configureFileOut, error) {
	if len(calls) == 0 || hostBuildDir == "" {
		return nil, nil
	}

	var out []configureFileOut
	seenRel := map[string]bool{}
	for _, call := range calls {
		// configure_file output is sometimes a relative path: cmake
		// resolves it against CMAKE_CURRENT_BINARY_DIR — the build-dir
		// mirror of the directory of the CMakeLists that made the call.
		// The trace records the call's file (CallFile), so anchor the
		// relative output there. This recovers the ubiquitous
		// `configure_file(config.h.in config.h)` autotools idiom, which
		// was previously dropped for "lack of per-call binary-dir context".
		output := call.Output
		if !filepath.IsAbs(output) {
			if call.CallFile == "" || recordedSrcDir == "" {
				continue
			}
			relDir, inside := relativeIfInside(recordedSrcDir, filepath.Dir(call.CallFile))
			if !inside {
				continue
			}
			output = filepath.Join(recordedBuildDir, relDir, call.Output)
		}
		rel, ok := relativeIfInsideRelaxed(recordedBuildDir, output)
		if !ok {
			// Output landed outside the build dir — unusual
			// (configure_file with absolute non-build dest).
			// Drop silently; not a recovery target.
			continue
		}
		if seenRel[rel] {
			// Trace can record duplicate calls when cmake
			// re-evaluates the same configure_file across
			// multiple frames. Dedupe by output path.
			continue
		}
		seenRel[rel] = true

		body, err := os.ReadFile(filepath.Join(hostBuildDir, rel))
		if err != nil {
			// Configured output not on disk — for offline
			// fixtures the stash may not include every
			// output, and for production the live build dir
			// always has them. Skip with no error so
			// missing fixtures degrade gracefully to the
			// pre-trace shape.
			continue
		}

		name := configureFileGenruleName(rel)
		gen := buildConfigureFileGenrule(name, rel, body, call, hostSrcDir, recordedSrcDir, liftEnabled, cmakeVars, cc.StampVars)
		cc.Genrules = append(cc.Genrules, gen)
		cc.OutToGenrule[rel] = name

		out = append(out, configureFileOut{
			AbsOutput: output,
			RelOutput: rel,
			// Normalize backslashes so a Windows-separator trace still matches
			// (mirrors shadow.isGenerateExportHeaderTemplate).
			ExportHeader: strings.HasSuffix(strings.ReplaceAll(call.Input, "\\", "/"), "/exportheader.cmake.in"),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].RelOutput < out[j].RelOutput })
	return out, nil
}

// buildConfigureFileGenrule decides between the lifted shape (a
// cmake_configure_file rule carrying the .h.in template + the captured
// values dict, re-rendered at Bazel time by //tools:cmake-configure-file)
// and the bake shape (rendered output emitted via the shared
// bakeFileTarget — readable skylib write_file for \n-text, byte-exact
// base64 genrule for binary). Picks the lifted shape when:
//
//   - The template input path resolves to a readable file
//     inside the source root, AND
//   - A values namespace is available (cmakeVars from
//     cmakerun's dump-vars hook, OR — for offline/fixture runs
//     where the dump isn't available — configurefile.Extract
//     recovers the per-template values dict), AND
//   - The verify-pass `Substitute(template, values, opts) ==
//     rendered` succeeds (catches any unmodeled option's byte
//     divergence; for the cmakeVars path this is the soundness
//     gate against future cmake additions we haven't taught
//     Substitute about).
//
// Falls back to the bake shape otherwise — soundness is
// preserved (the .h.in stays load-bearing in srckey via the
// existing read-paths.txt narrowing); the audit tool's
// undercoverage report continues to flag those .h.in paths
// until the templating shape is supported.
//
// cmakeVars vs Extract: Extract recovers ONLY the variables
// the current template references; if the user later edits
// .h.in to add a new @VAR@, Extract's values are stale and
// the lifted genrule renders empty for the new marker. The
// cmakeVars path captures the FULL cmake variable namespace
// at configure time, so any marker the user later adds
// resolves correctly through the Bazel-time tool — closing
// the soundness gap PR #94 review identified.
func buildConfigureFileGenrule(name, outRel string, rendered []byte, call shadow.ConfigureFileCall, hostSrcDir, recordedSrcDir string, liftEnabled bool, cmakeVars, stampVars map[string]string) ir.Target {
	// Bake the fully-resolved bytes via the shared bakeFileTarget chooser:
	// readable skylib write_file for \n-only text, byte-exact base64
	// genrule for binary / control-byte / CRLF bodies. Same de-base64
	// maintainability win as the file(GENERATE) bake.
	bake := bakeFileTarget(name, outRel, rendered, configureFileTags(configureFileTagSet{}))

	if !liftEnabled || hostSrcDir == "" || recordedSrcDir == "" {
		return bake
	}
	opts, optErr := configureFileOptionsFromCall(call.Options)
	if optErr != nil {
		return bake
	}
	templatePath, inRel, ok := resolveTemplatePath(call.Input, hostSrcDir, recordedSrcDir)
	if !ok {
		return bake
	}
	templateBody, err := os.ReadFile(templatePath)
	if err != nil {
		return bake
	}
	values, ok := pickValues(templateBody, rendered, opts, cmakeVars)
	if !ok {
		return bake
	}
	spec := newConfigureFileSpec(outRel, opts)
	spec.Template = inRel
	spec.Values = values
	// VCS-stamp lift: a template var written by a stamp execute_process
	// (cc.StampVars) re-reads its value from the Bazel workspace status at
	// build time instead of baking the convert-time revision. Only vars the
	// template actually references are wired (others would couple this rule
	// to the stamp status for nothing); the baked value stays in `values`
	// as the non-stamped fallback.
	spec.StampValues = stampValuesForTemplate(templateBody, opts, stampVars)
	return cmakeConfigureFileTarget(name, spec, configureFileTags(configureFileTagSet{Lifted: true}))
}

// cmakeConfigureFileToolLabel is the Bazel label of the
// cmake-configure-file binary write-a stages into each project's tools/.
// Resolves in whichever repo the lift's BUILD lands (project A or B),
// same as the legacy genrule shape's tools=[...] entry.
const cmakeConfigureFileToolLabel = "//tools:cmake-configure-file"

// newConfigureFileSpec builds the shared base of a cmake_configure_file
// rule spec from the configure_file option set (tool label + the
// option-mirror attributes). Callers set Template / Content / Values /
// Genex* per lift shape.
func newConfigureFileSpec(outRel string, opts configurefile.Options) *ir.CMakeConfigureFileSpec {
	return &ir.CMakeConfigureFileSpec{
		Out:          outRel,
		Tool:         cmakeConfigureFileToolLabel,
		AtOnly:       opts.AtOnly,
		CopyOnly:     opts.CopyOnly,
		EscapeQuotes: opts.EscapeQuotes,
		NewlineStyle: newlineStyleFlag(opts),
	}
}

// newlineStyleFlag maps configurefile.NewlineStyle to the
// cmake_configure_file rule's newline_style attribute value ("" preserves
// the template's style).
func newlineStyleFlag(opts configurefile.Options) string {
	switch opts.NewlineStyle {
	case configurefile.NewlineLF:
		return "lf"
	case configurefile.NewlineCRLF:
		return "crlf"
	}
	return ""
}

// cmakeConfigureFileTarget wraps a spec into the KindCMakeConfigureFile
// ir.Target shared by the configure_file, file(GENERATE), and cmake -E
// configure_file lifts.
func cmakeConfigureFileTarget(name string, spec *ir.CMakeConfigureFileSpec, tags []string) ir.Target {
	return ir.Target{
		Name:               name,
		Kind:               ir.KindCMakeConfigureFile,
		CMakeConfigureFile: spec,
		Tags:               tags,
		Visibility:         []string{"//visibility:private"},
	}
}

// pickValues chooses the values map the lifted genrule's
// Substitute will run against. Prefers the full cmake namespace
// (cmakeVars from dump-vars.cmake) since that resolves any
// @VAR@ the user later adds to the template; falls back to
// per-template Extract when the dump isn't available (offline
// fixtures); falls back to "no lift" when neither produces a
// values map that round-trips to cmake's rendered output.
//
// Returns (values, true) on success, (nil, false) on giving up.
// The verify-pass — running Substitute against the chosen
// values and byte-comparing against rendered — is what makes
// the cmakeVars path safe even when Substitute hasn't modeled
// every cmake configure_file option yet: divergence fails the
// pass and we drop back to legacy.
func pickValues(templateBody, rendered []byte, opts configurefile.Options, cmakeVars map[string]string) (map[string]string, bool) {
	if len(cmakeVars) > 0 {
		got := configurefile.Substitute(templateBody, cmakeVars, opts)
		if bytes.Equal(got, rendered) {
			return cmakeVars, true
		}
		// Verify-pass failed — Substitute doesn't model some
		// option this call uses, OR cmakeVars is somehow
		// stale relative to the configure run that produced
		// `rendered`. Fall through to Extract.
	}
	values, err := configurefile.Extract(templateBody, rendered, opts)
	if err != nil {
		return nil, false
	}
	return values, true
}

// stampValuesForTemplate returns the subset of stampVars (cmake var ->
// workspace-status key) the template actually references, as the
// cmake_configure_file rule's stamp_values map. Filtering to referenced
// vars keeps unrelated configure_files from gaining a spurious dependency
// on the stamp status (and the rebuild a revision change would otherwise
// trigger on them). Returns nil when nothing matches — the common
// no-stamp configure_file — so the emitter omits the attribute.
func stampValuesForTemplate(templateBody []byte, opts configurefile.Options, stampVars map[string]string) map[string]string {
	if len(stampVars) == 0 {
		return nil
	}
	var out map[string]string
	for varName, statusKey := range stampVars {
		if templateMentionsVar(templateBody, varName, opts) {
			if out == nil {
				out = map[string]string{}
			}
			out[varName] = statusKey
		}
	}
	return out
}

// templateMentionsVar reports whether a configure_file template references
// varName via cmake's substitution markers: @VAR@ always, or ${VAR} unless
// @ONLY (opts.AtOnly) restricts substitution to the @-form. A conservative
// substring test — an exotic reference shape we miss simply keeps the
// baked value rather than the build-time stamp read.
func templateMentionsVar(templateBody []byte, varName string, opts configurefile.Options) bool {
	if bytes.Contains(templateBody, []byte("@"+varName+"@")) {
		return true
	}
	if !opts.AtOnly && bytes.Contains(templateBody, []byte("${"+varName+"}")) {
		return true
	}
	return false
}

// resolveTemplatePath converts the trace's recorded template
// input path into (host-real path, package-relative path) so
// the genrule's srcs label resolves at Bazel time. Returns
// ok=false when the input lives outside the source tree or
// can't be made source-relative.
func resolveTemplatePath(input, hostSrcDir, recordedSrcDir string) (string, string, bool) {
	if !filepath.IsAbs(input) {
		// Trace usually records absolute paths after cmake
		// expands variables; if not, we can't anchor without
		// per-call current-source-dir context.
		return "", "", false
	}
	rel, ok := relativeIfInsideRelaxed(recordedSrcDir, input)
	if !ok {
		return "", "", false
	}
	return filepath.Join(hostSrcDir, rel), filepath.ToSlash(rel), true
}

// configureFileOptionsFromCall parses cmake's configure_file
// option list (the trailing tokens beyond input/output:
// `@ONLY`, `COPYONLY`, `ESCAPE_QUOTES`, `NEWLINE_STYLE <style>`,
// permission keywords) into a configurefile.Options. Returns
// an error on a malformed list (e.g. NEWLINE_STYLE without a
// value); the caller falls back to legacy in that case.
//
// Permission keywords (FILE_PERMISSIONS / USE_SOURCE_PERMISSIONS
// / NO_SOURCE_PERMISSIONS) are accepted-and-ignored: they affect
// the output file's mode bits, not its content; Bazel's
// genrule sets its own mode independent of cmake's choice, and
// for config.h-style headers the mode doesn't matter for
// downstream compilation.
func configureFileOptionsFromCall(opts []string) (configurefile.Options, error) {
	out := configurefile.Options{}
	for i := 0; i < len(opts); i++ {
		switch strings.ToUpper(opts[i]) {
		case "@ONLY":
			out.AtOnly = true
		case "COPYONLY":
			out.CopyOnly = true
		case "ESCAPE_QUOTES":
			out.EscapeQuotes = true
		case "NEWLINE_STYLE":
			if i+1 >= len(opts) {
				return out, fmt.Errorf("NEWLINE_STYLE without value")
			}
			i++
			switch strings.ToUpper(opts[i]) {
			case "UNIX", "LF":
				out.NewlineStyle = configurefile.NewlineLF
			case "DOS", "WIN32", "CRLF":
				out.NewlineStyle = configurefile.NewlineCRLF
			default:
				return out, fmt.Errorf("NEWLINE_STYLE: unknown value %q", opts[i])
			}
		case "FILE_PERMISSIONS",
			"USE_SOURCE_PERMISSIONS",
			"NO_SOURCE_PERMISSIONS":
			// Mode bits — ignored (see fn doc).
			// FILE_PERMISSIONS takes value args; consume
			// until we hit a known keyword or the end.
			if strings.EqualFold(opts[i], "FILE_PERMISSIONS") {
				for i+1 < len(opts) && !isConfigureFileKeyword(opts[i+1]) {
					i++
				}
			}
		default:
			// Unknown token — defer to Extract's verify-pass
			// rather than guessing semantics. Returning here
			// would force fallback even for a benign new
			// option; instead we accept and let the verify
			// pass tell us if it actually affected bytes.
		}
	}
	return out, nil
}

// isConfigureFileKeyword reports whether s is one of the
// documented configure_file flag keywords. Used to bound
// FILE_PERMISSIONS' variadic value list.
func isConfigureFileKeyword(s string) bool {
	switch strings.ToUpper(s) {
	case "@ONLY",
		"COPYONLY",
		"ESCAPE_QUOTES",
		"NEWLINE_STYLE",
		"FILE_PERMISSIONS",
		"USE_SOURCE_PERMISSIONS",
		"NO_SOURCE_PERMISSIONS":
		return true
	}
	return false
}

// configureFileGenruleName turns a build-dir-relative output
// path into a Bazel-rule-name-safe identifier mirroring
// genruleNameFor: "config.h" -> "gen_config_h".
func configureFileGenruleName(rel string) string {
	rel = filepath.ToSlash(rel)
	rel = strings.TrimPrefix(rel, "./")
	var sb strings.Builder
	sb.WriteString("gen_")
	for i := 0; i < len(rel); i++ {
		c := rel[i]
		switch {
		case (c >= 'a' && c <= 'z'),
			(c >= 'A' && c <= 'Z'),
			(c >= '0' && c <= '9'),
			c == '_':
			sb.WriteByte(c)
		default:
			sb.WriteByte('_')
		}
	}
	return sb.String()
}

// configureFileLegacyCmd builds the fallback shell command
// when configurefile.Extract can't recover a values dict.
// Embeds the rendered bytes verbatim via base64 so any byte
// content (including embedded newlines, single-quotes, $, etc.)
// round-trips losslessly without shell-escaping concerns. The
// `mkdir -p $$(dirname $@)` prefix is harmless for top-level
// outputs and necessary for nested ones (e.g. subdir/version.h).
//
// Soundness consequence: the rendered bytes appear in the
// genrule's cmd, so the .h.in template content drives the
// BUILD.bazel content; the .h.in must remain content-included
// in convert-element-cmake's srckey for soundness. Audit catches
// this case (it's the cmake-side oracle's whole point); the
// fix is the lifted shape, taken when Extract succeeds.
func configureFileLegacyCmd(rel string, body []byte) string {
	encoded := base64.StdEncoding.EncodeToString(body)
	return fmt.Sprintf("mkdir -p $$(dirname $@) && echo %s | base64 -d > $@", encoded)
}

// configureFileTagSet names each tag-emit facet for a
// configure_file emission. Zero value = the legacy bytes-
// embedded shape (rendered output base64'd into the genrule cmd);
// Lifted = the cmake-configure-file tool emits at Bazel time
// from the .h.in template + captured values. Mirrors
// fileGenerateTagSet so the two lifters share the same call-
// site shape ("only the true-valued facets appear").
type configureFileTagSet struct {
	// Lifted: the genrule uses the cmake-configure-file tool
	// (template decoupled from BUILD.bazel content) vs. the
	// legacy bytes-embedded shape.
	Lifted bool
}

// configureFileTags returns the cmake-codegen tag set for a
// configure_file emission. Always carries the three base tags
// (cmake-codegen, cmake-codegen-configure-file,
// cmake-codegen-driver=configure_file); the facet flags
// append one tag each when true. Sorted on return for byte-
// stable BUILD.bazel output.
//
// The driver tag distinguishes configure_file emissions from
// CUSTOM_COMMAND-recovered genrules so audit queries can split
// the two cleanly.
func configureFileTags(s configureFileTagSet) []string {
	tags := []string{
		"cmake-codegen",
		"cmake-codegen-configure-file",
		"cmake-codegen-driver=configure_file",
	}
	if s.Lifted {
		tags = append(tags, "cmake-codegen-lifted")
	}
	sort.Strings(tags)
	return tags
}
