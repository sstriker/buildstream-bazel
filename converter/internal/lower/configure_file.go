package lower

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/sstriker/cmake-to-bazel/converter/internal/ir"
	"github.com/sstriker/cmake-to-bazel/internal/configurefile"
	"github.com/sstriker/cmake-to-bazel/internal/shadow"
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
}

// recoverConfigureFiles walks the trace's configure_file events
// and emits one Bazel genrule per call. The genrule's cmd
// base64-encodes the rendered output bytes (configure-time
// substitution already done by cmake) and decodes them at Bazel
// build time — sidesteps the need to re-run cmake or implement
// @VAR@ expansion. Returns the list of recovered outputs so
// lowerTarget can attach them to consuming targets.
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
func recoverConfigureFiles(traceRaw []byte, hostBuildDir, recordedSrcDir, recordedBuildDir string, cc *codegenContext) ([]configureFileOut, error) {
	if len(traceRaw) == 0 || hostBuildDir == "" {
		return nil, nil
	}
	return recoverConfigureFilesFromCalls(shadow.ExtractConfigureFiles(traceRaw, recordedSrcDir), recordedSrcDir, recordedSrcDir, hostBuildDir, recordedBuildDir, cc)
}

// recoverConfigureFilesFromCalls is the same logic as
// recoverConfigureFiles but takes pre-decoded ConfigureFileCall
// records. Used by Lower's single-pass trace dispatch so the
// trace is parsed once total across all extractors (including
// the configure_file recovery), instead of one pass per
// extractor.
func recoverConfigureFilesFromCalls(calls []shadow.ConfigureFileCall, hostSrcDir, recordedSrcDir, hostBuildDir, recordedBuildDir string, cc *codegenContext) ([]configureFileOut, error) {
	if len(calls) == 0 || hostBuildDir == "" {
		return nil, nil
	}

	var out []configureFileOut
	seenRel := map[string]bool{}
	for _, call := range calls {
		// configure_file output is sometimes a relative path
		// (cmake resolves against the current binary dir at
		// expand time). Trace records the resolved string so
		// most calls have absolute paths. Skip relative —
		// can't anchor without per-call binary-dir context.
		if !filepath.IsAbs(call.Output) {
			continue
		}
		rel, ok := relativeIfInsideRelaxed(recordedBuildDir, call.Output)
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
		gen := buildConfigureFileGenrule(name, rel, body, call, hostSrcDir, recordedSrcDir)
		cc.Genrules = append(cc.Genrules, gen)
		cc.OutToGenrule[rel] = name

		out = append(out, configureFileOut{
			AbsOutput: call.Output,
			RelOutput: rel,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].RelOutput < out[j].RelOutput })
	return out, nil
}

// buildConfigureFileGenrule decides between the lifted shape
// (.h.in as a real srcs + cmake-configure-file tool + values
// JSON in cmd) and the legacy shape (rendered output base64-
// embedded in cmd). Picks the lifted shape when:
//
//   - The template input path resolves to a readable file
//     inside the source root, AND
//   - configurefile.Extract recovers a values dict that
//     round-trips to the rendered output byte-for-byte.
//
// Falls back to the legacy shape otherwise — soundness is
// preserved (the .h.in stays load-bearing in srckey via the
// existing read-paths.txt narrowing); the audit tool's
// undercoverage report continues to flag those .h.in paths
// until the templating shape is supported.
func buildConfigureFileGenrule(name, outRel string, rendered []byte, call shadow.ConfigureFileCall, hostSrcDir, recordedSrcDir string) ir.Target {
	legacy := ir.Target{
		Name:        name,
		Kind:        ir.KindGenrule,
		GenruleCmd:  configureFileLegacyCmd(outRel, rendered),
		GenruleOuts: []string{outRel},
		Tags:        configureFileTags(),
		Visibility:  []string{"//visibility:private"},
	}

	if hostSrcDir == "" || recordedSrcDir == "" {
		return legacy
	}
	templatePath, inRel, ok := resolveTemplatePath(call.Input, hostSrcDir, recordedSrcDir)
	if !ok {
		return legacy
	}
	templateBody, err := os.ReadFile(templatePath)
	if err != nil {
		return legacy
	}
	opts := configurefile.Options{AtOnly: hasOption(call.Options, "@ONLY")}
	values, err := configurefile.Extract(templateBody, rendered, opts)
	if err != nil {
		return legacy
	}
	cmd, err := configureFileLiftedCmd(inRel, outRel, values, opts)
	if err != nil {
		return legacy
	}
	return ir.Target{
		Name:         name,
		Kind:         ir.KindGenrule,
		Srcs:         []string{inRel},
		GenruleCmd:   cmd,
		GenruleOuts:  []string{outRel},
		GenruleTools: []string{"//tools:cmake-configure-file"},
		Tags:         append(configureFileTags(), "cmake-codegen-lifted"),
		Visibility:   []string{"//visibility:private"},
	}
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

// hasOption is a case-insensitive search of cmake's
// configure_file option flags (the trailing tokens beyond
// input/output: COPYONLY / @ONLY / ESCAPE_QUOTES /
// NEWLINE_STYLE...).
func hasOption(options []string, want string) bool {
	for _, o := range options {
		if strings.EqualFold(o, want) {
			return true
		}
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
// in convert-element's srckey for soundness. Audit catches
// this case (it's the cmake-side oracle's whole point); the
// fix is the lifted shape, taken when Extract succeeds.
func configureFileLegacyCmd(rel string, body []byte) string {
	encoded := base64.StdEncoding.EncodeToString(body)
	return fmt.Sprintf("mkdir -p $$(dirname $@) && echo %s | base64 -d > $@", encoded)
}

// configureFileLiftedCmd builds the lifted shell command:
// stage the values JSON to a tmpfile, then run the
// cmake-configure-file tool with the .h.in template (resolved
// via $(location <inRel>)) and the captured values. The
// rendered output bytes do NOT appear in the cmd; only the
// values dict does. Edits to .h.in invalidate the genrule
// through srcs, not through BUILD.bazel content.
//
// Values are base64'd JSON to avoid shell-quoting concerns;
// the JSON is small (one entry per cmake variable the
// template references) so this stays compact.
func configureFileLiftedCmd(inRel, outRel string, values map[string]string, opts configurefile.Options) (string, error) {
	body, err := json.Marshal(values)
	if err != nil {
		return "", fmt.Errorf("marshal values: %w", err)
	}
	encoded := base64.StdEncoding.EncodeToString(body)
	atOnly := ""
	if opts.AtOnly {
		atOnly = "--at-only "
	}
	// $(execpath) resolves srcs labels relative to exec root;
	// $@ is the genrule output. Tmpfile lives under $(@D) (the
	// genrule's output dir) so it's auto-cleaned on rebuild and
	// doesn't pollute /tmp on shared executors.
	return fmt.Sprintf(
		"mkdir -p $$(dirname $@) && "+
			"VALUES=$$(mktemp) && "+
			"echo %s | base64 -d > $$VALUES && "+
			"$(execpath //tools:cmake-configure-file) %s--values=$$VALUES $(execpath %s) $@ ; "+
			"rc=$$?; rm -f $$VALUES; exit $$rc",
		encoded, atOnly, inRel,
	), nil
}

// configureFileTags returns the cmake-codegen tag set for a
// configure_file emission. Distinguishes from
// CUSTOM_COMMAND-recovered genrules via cmake-codegen-driver=
// =configure_file, so audit queries can split the two cleanly.
func configureFileTags() []string {
	return []string{
		"cmake-codegen",
		"cmake-codegen-configure-file",
		"cmake-codegen-driver=configure_file",
	}
}
