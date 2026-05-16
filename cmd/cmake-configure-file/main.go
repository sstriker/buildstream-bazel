// cmake-configure-file is the Bazel-time substitution tool the
// recovered configure_file genrule invokes. Reads a values JSON
// (a flat object mapping cmake variable names to string values),
// reads the template body, applies cmake's documented
// substitution rules (see internal/configurefile package doc),
// and writes the rendered output.
//
// Two template-source modes:
//
//	# Template from a file in srcs (configure_file's INPUT form;
//	# also file(GENERATE)'s INPUT form):
//	cmake-configure-file [flags] --values=<values.json> \
//	    <input.h.in> <output>
//
//	# Template from an inline base64 blob (file(GENERATE)'s
//	# CONTENT form — the body is a literal string, not an on-
//	# disk file, so there's no srcs anchor to point at):
//	cmake-configure-file [flags] --values=<values.json> \
//	    --content-base64=<blob> <output>
//
// Exactly one of the positional <input> path or --content-base64
// must be supplied. The CONTENT-form blob carries the raw
// template bytes. Substitution shape per caller:
//
//   - configure_file lifts pass full `--values=<map>` and the
//     default option set (substitution active): @VAR@,
//     ${VAR}, #cmakedefine, #cmakedefine01 — and the relevant
//     subset of @ONLY, ESCAPE_QUOTES, NEWLINE_STYLE flags.
//   - file(GENERATE) lifts pass `--copy-only` with an empty
//     `--values={}` and only NEWLINE_STYLE varying: cmake's
//     file(GENERATE) is verbatim emit (no @VAR@/${VAR}/
//     #cmakedefine substitution) — only generator expressions
//     and NEWLINE_STYLE shape the bytes, and genex-bearing
//     templates short-circuit to the legacy bytes-embedded
//     genrule rather than this tool. Using --copy-only ensures
//     a later template edit that adds an @VAR@ marker stays
//     byte-equal to what cmake's file(GENERATE) would have
//     produced.
//   - cmake -E configure_file lifts behave like configure_file
//     lifts (the cmake -E op is documented as the same
//     substitution surface).
//
// The companion lift in converter/internal/lower
// (configureFileLiftedCmd / fileGenerateLiftedCmd) emits genrules
// of shape:
//
//	# INPUT form
//	genrule(
//	    name = "gen_config_h",
//	    srcs = ["src/config.h.in"],
//	    outs = ["config.h"],
//	    cmd  = "mkdir -p \"$$(dirname \"$@\")\" && "
//	           "VALUES=\"$$(mktemp \"$$(dirname \"$@\")/...XXXXXX\")\" && "
//	           "echo <base64-of-values-json> | base64 -d > \"$$VALUES\" && "
//	           "$(location //tools:cmake-configure-file) [--at-only] "
//	           "--values=\"$$VALUES\" \"$(location src/config.h.in)\" \"$@\" ; "
//	           "rc=$$?; rm -f \"$$VALUES\"; exit $$rc",
//	    tools = ["//tools:cmake-configure-file"],
//	)
//
//	# CONTENT form (no srcs entry — the template is an inline blob)
//	genrule(
//	    name = "gen_banner_h",
//	    outs = ["banner.h"],
//	    cmd  = "... --content-base64=<blob> --values=\"$$VALUES\" \"$@\" ...",
//	    tools = ["//tools:cmake-configure-file"],
//	)
//
// For the INPUT form, the .h.in lives in srcs and Bazel
// invalidates the genrule directly on edit. The values JSON is
// inlined into the cmd as a base64 blob containing the FULL
// cmake variable namespace at configure time (a few KB to tens
// of KB) so any @VAR@/${VAR}/#cmakedefine the user later adds
// to the template resolves correctly without convert-element-cmake
// rerunning. Smaller than embedding the full rendered output
// AND independent of .h.in content; .h.in becomes safely
// name-only for srckey purposes. Volatile path-bearing
// variables are filtered before the values JSON is emitted
// (see cmakerun.filterVolatilePaths) so BUILD.bazel stays
// byte-stable across cmake invocations against the same source
// tree.
//
// For the CONTENT form there's no .h.in source — the template
// bytes themselves are in the cmd (base64-encoded). Editing
// CMakeLists.txt's CONTENT string changes the blob and thus
// BUILD.bazel; CMakeLists.txt is already content-included in
// srckey so this re-runs convert-element-cmake correctly. The win
// vs. the legacy bytes-embedded shape is that BUILD.bazel
// content reflects the template, not the rendered output —
// edits to values (variables) re-render without changing the
// template blob.
//
// We could move the values to a separate sidecar file (with
// `srcs = [".h.in", ":gen_*_values"]`); the inline form keeps
// convert-element-cmake's output set unchanged (just BUILD.bazel) at
// the cost of a marginally longer cmd. Inline is the v1 shape;
// the sidecar form is a follow-up if the cmd's length becomes
// a problem in practice.
package main

import (
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/sstriker/buildstream-bazel/internal/configurefile"
	"github.com/sstriker/buildstream-bazel/internal/genexeval"
)

// optionalString is a flag.Value that tracks whether a string
// flag was supplied on the command line, independent of whether
// its value was the empty string. Used for --content-base64
// where the empty-string value is meaningful (base64 of an
// empty template body, for `file(GENERATE CONTENT "")` lifts);
// flag.String would conflate that with "flag not supplied".
type optionalString struct {
	set bool
	val string
}

func (o *optionalString) String() string {
	if o == nil {
		return ""
	}
	return o.val
}

func (o *optionalString) Set(s string) error {
	o.set = true
	o.val = s
	return nil
}

func main() {
	valuesPath := flag.String("values", "", "path to JSON file containing the {VAR: value, ...} substitution map. Required.")
	genexValuesPath := flag.String("genex-values", "", "optional: path to JSON file mapping each top-level `$<...>` literal in the template to its cmake-resolved bytes (the captured-at-convert-time \"structured base64\" lift's payload). Applied AFTER --values substitution; literal replacement, not recursive evaluation. Empty (default) skips the genex-replay step, matching the configure_file lift's behaviour where templates carry no genexes.")
	genexContextPath := flag.String("genex-context", "", "optional: path to JSON file carrying the cmake configure-time context the Go-side genex evaluator (genexeval package) consults to resolve `$<...>` at Bazel time. Schema: {\"config\": str, \"compiler_id\": {lang: str}, \"platform_id\": str, \"compiler_language\": str}. Applied AFTER --values substitution. Mutually exclusive with --genex-values (the two genex shapes are different replacement strategies — pick one per call site).")
	var contentBase64 optionalString
	flag.Var(&contentBase64, "content-base64", "base64-encoded inline template body (mutually exclusive with the positional <input> path). Used by file(GENERATE CONTENT ...) lifts where the template has no on-disk srcs anchor. An explicit `--content-base64=` (empty value) is treated as the literal empty template — distinct from omitting the flag.")
	atOnly := flag.Bool("at-only", false, "skip ${VAR} substitution; only @VAR@ markers are replaced. Mirrors configure_file's @ONLY flag.")
	copyOnly := flag.Bool("copy-only", false, "skip ALL substitution (@VAR@, ${VAR}, #cmakedefine*) and emit the template verbatim. Mirrors configure_file's COPYONLY flag.")
	escapeQuotes := flag.Bool("escape-quotes", false, "backslash-escape `\"` (and `\\\\`) in expanded values. Mirrors configure_file's ESCAPE_QUOTES flag.")
	newlineStyle := flag.String("newline-style", "", "rewrite the line terminator: 'lf'|'unix' for `\\n`, 'crlf'|'dos'|'win32' for `\\r\\n`. Empty preserves the template's original style. Mirrors configure_file's NEWLINE_STYLE flag.")
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: cmake-configure-file [flags] --values=<values.json> [--content-base64=<blob>] [<input>] <output>")
		flag.PrintDefaults()
	}
	flag.Parse()

	args := flag.Args()
	// Argv shape:
	//   INPUT form:   --values=v <input> <output>          (2 positional)
	//   CONTENT form: --values=v --content-base64=b <output> (1 positional;
	//                 b may be the empty string for the empty-template case)
	// Mutual exclusion: setting both --content-base64 and a positional
	// <input> is ambiguous about which template source wins, so reject
	// at the CLI rather than picking silently.
	hasInputPath := false
	switch {
	case *valuesPath == "":
		flag.Usage()
		os.Exit(2)
	case contentBase64.set && len(args) == 1:
		// CONTENT form: just <output>.
	case !contentBase64.set && len(args) == 2:
		// INPUT form: <input> <output>.
		hasInputPath = true
	case contentBase64.set && len(args) == 2:
		fmt.Fprintln(os.Stderr, "cmake-configure-file: --content-base64 and positional <input> are mutually exclusive")
		os.Exit(2)
	default:
		flag.Usage()
		os.Exit(2)
	}
	style, err := parseNewlineStyle(*newlineStyle)
	if err != nil {
		fmt.Fprintf(os.Stderr, "cmake-configure-file: %v\n", err)
		os.Exit(2)
	}
	opts := configurefile.Options{
		AtOnly:       *atOnly,
		CopyOnly:     *copyOnly,
		EscapeQuotes: *escapeQuotes,
		NewlineStyle: style,
	}
	inPath, outPath := "", args[len(args)-1]
	if hasInputPath {
		inPath = args[0]
	}
	if *genexValuesPath != "" && *genexContextPath != "" {
		fmt.Fprintln(os.Stderr, "cmake-configure-file: --genex-values and --genex-context are mutually exclusive (pick one genex shape per call site)")
		os.Exit(2)
	}
	if err := run(*valuesPath, *genexValuesPath, *genexContextPath, inPath, contentBase64.set, contentBase64.val, outPath, opts); err != nil {
		fmt.Fprintf(os.Stderr, "cmake-configure-file: %v\n", err)
		os.Exit(1)
	}
}

// parseNewlineStyle accepts cmake's NEWLINE_STYLE token set
// (case-insensitive). Empty string maps to NewlineDefault
// (preserve the template's original line terminator).
func parseNewlineStyle(s string) (configurefile.NewlineStyle, error) {
	switch strings.ToLower(s) {
	case "":
		return configurefile.NewlineDefault, nil
	case "lf", "unix":
		return configurefile.NewlineLF, nil
	case "crlf", "dos", "win32":
		return configurefile.NewlineCRLF, nil
	}
	return 0, fmt.Errorf("--newline-style: %q not one of LF|UNIX|CRLF|DOS|WIN32", s)
}

// run loads the values JSON, sources the template body from
// either inPath (INPUT form; inPath != "") or the
// --content-base64 blob (CONTENT form; hasContent == true,
// content may be the empty string for the empty-template
// case), substitutes, and writes the rendered output.
//
// Invariant: exactly one of inPath / hasContent must be set.
// main enforces this from the CLI argv shape; run validates
// it again so future programmatic callers (and any reshuffled
// CLI parsing) can't silently slip through with neither set
// (which would degenerate to "render an empty template" — a
// suspiciously well-formed output that masks the bug) or
// both set (which is ambiguous about which template source
// wins).
func run(valuesPath, genexValuesPath, genexContextPath, inPath string, hasContent bool, content, outPath string, opts configurefile.Options) error {
	switch {
	case inPath == "" && !hasContent:
		return fmt.Errorf("internal: neither inPath nor hasContent set; main's CLI gate should have rejected this argv")
	case inPath != "" && hasContent:
		return fmt.Errorf("internal: both inPath and hasContent set; main's CLI gate should have rejected this argv")
	}
	values, err := loadValues(valuesPath)
	if err != nil {
		return fmt.Errorf("load values %s: %w", valuesPath, err)
	}
	var tmpl []byte
	if inPath != "" {
		tmpl, err = os.ReadFile(inPath)
		if err != nil {
			return fmt.Errorf("read template %s: %w", inPath, err)
		}
	} else {
		tmpl, err = base64.StdEncoding.DecodeString(content)
		if err != nil {
			return fmt.Errorf("decode --content-base64: %w", err)
		}
	}
	rendered := configurefile.Substitute(tmpl, values, opts)
	if genexValuesPath != "" {
		// The genex-replay step runs AFTER Substitute. For the
		// file(GENERATE) lifts that exercise this path, Substitute
		// is a verbatim copy (CopyOnly + empty values), so the
		// order doesn't matter byte-wise — applyGenexValues
		// receives the template content untouched. Keeping the
		// order explicit (Substitute first, then genex replay)
		// matches cmake's own pipeline: cmake substitutes
		// @VAR@/${VAR}/#cmakedefine first, then evaluates `$<...>`
		// against the substituted text — so if a future lift
		// activates both substitution AND a genex-values payload
		// on the same call, the byte order matches what cmake
		// would produce.
		gv, err := loadGenexValues(genexValuesPath)
		if err != nil {
			return fmt.Errorf("load genex values %s: %w", genexValuesPath, err)
		}
		rendered, err = applyGenexValuesAtRuntime(rendered, gv)
		if err != nil {
			return fmt.Errorf("apply genex values: %w", err)
		}
	}
	if genexContextPath != "" {
		// (a)-shape lift: parse the (post-Substitute) bytes via
		// the Go-side genex evaluator and resolve `$<...>` against
		// the captured Context. Same Substitute-first ordering as
		// the (b) path so the @VAR@/${VAR}/#cmakedefine output is
		// what the genex evaluator sees — matches cmake's pipeline.
		// CLI gate above already rejects the --genex-values +
		// --genex-context combination.
		ctx, err := loadGenexContext(genexContextPath)
		if err != nil {
			return fmt.Errorf("load genex context %s: %w", genexContextPath, err)
		}
		nodes, err := genexeval.Parse(rendered)
		if err != nil {
			return fmt.Errorf("parse template for genex evaluation: %w", err)
		}
		evaled, err := genexeval.Eval(nodes, ctx)
		if err != nil {
			return fmt.Errorf("evaluate genex: %w", err)
		}
		rendered = evaled
	}
	if err := os.WriteFile(outPath, rendered, 0o644); err != nil {
		return fmt.Errorf("write output %s: %w", outPath, err)
	}
	return nil
}

func loadValues(path string) (map[string]string, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var values map[string]string
	if err := json.Unmarshal(body, &values); err != nil {
		return nil, fmt.Errorf("parse JSON: %w", err)
	}
	if values == nil {
		// Treat null as empty so callers can pass `null` for a
		// no-substitutions template (e.g., COPYONLY equivalent
		// minus the COPYONLY shortcut).
		values = map[string]string{}
	}
	return values, nil
}

// loadGenexValues reads a JSON map of `$<...>` literals to
// their cmake-resolved bytes. Mirrors loadValues's shape — a
// flat string→string map — except the keys carry the full
// genex literal (including the `$<` and `>` bookends). Mapped
// values are the raw bytes cmake emitted for that genex at
// generate-time; the genex-replay step substitutes them literally
// at Bazel time without re-evaluating any genex grammar.
//
// Empty JSON object (no entries) is valid: it just means the
// caller staged an empty payload, which makes the replay a no-op.
// `null` is normalized to empty for the same null-tolerance
// reason loadValues applies.
func loadGenexValues(path string) (map[string]string, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var values map[string]string
	if err := json.Unmarshal(body, &values); err != nil {
		return nil, fmt.Errorf("parse JSON: %w", err)
	}
	if values == nil {
		values = map[string]string{}
	}
	return values, nil
}

// genexContextJSON is the JSON wire form for genexeval.Context.
// Mirrors the Go struct field-for-field but uses snake_case
// names to match the rest of the project's JSON convention
// (see internal/shadow / values JSON shapes). The (a)-shape
// lifter at convert-element-cmake time serialises the
// captured cmake configure-time slice as this JSON; the tool
// deserialises and feeds genexeval.Eval.
type genexContextJSON struct {
	Config           string                    `json:"config,omitempty"`
	CompilerID       map[string]string         `json:"compiler_id,omitempty"`
	PlatformID       string                    `json:"platform_id,omitempty"`
	CompilerLanguage string                    `json:"compiler_language,omitempty"`
	Targets          map[string]targetInfoJSON `json:"targets,omitempty"`
}

// targetInfoJSON mirrors genexeval.TargetInfo on the wire. Same
// snake_case-keys convention as genexContextJSON.
type targetInfoJSON struct {
	Type     string `json:"type,omitempty"`
	Sources  string `json:"sources,omitempty"`
	Imported bool   `json:"imported,omitempty"`
}

// loadGenexContext reads the (a)-shape Context sidecar. Empty
// object {} is valid (every Context field is optional; an op
// the loaded Context can't satisfy surfaces as
// UnsupportedError at Eval time, propagating to the caller as
// "evaluate genex: ..."). `null` is normalized to an empty
// Context for the same null-tolerance reason loadValues
// applies.
func loadGenexContext(path string) (genexeval.Context, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return genexeval.Context{}, err
	}
	var raw *genexContextJSON
	if err := json.Unmarshal(body, &raw); err != nil {
		return genexeval.Context{}, fmt.Errorf("parse JSON: %w", err)
	}
	if raw == nil {
		return genexeval.Context{}, nil
	}
	var targets map[string]genexeval.TargetInfo
	if len(raw.Targets) > 0 {
		targets = make(map[string]genexeval.TargetInfo, len(raw.Targets))
		for name, t := range raw.Targets {
			targets[name] = genexeval.TargetInfo{
				Type:     t.Type,
				Sources:  t.Sources,
				Imported: t.Imported,
			}
		}
	}
	return genexeval.Context{
		Config:           raw.Config,
		CompilerID:       raw.CompilerID,
		PlatformID:       raw.PlatformID,
		CompilerLanguage: raw.CompilerLanguage,
		Targets:          targets,
	}, nil
}

// applyGenexValuesAtRuntime is the Bazel-time complement of
// converter/internal/lower's applyGenexValues. It walks
// rendered's top-level `$<...>` blocks (using the same depth-
// aware scanner the lifter uses at capture time) and replaces
// each with its mapped value. The function is duplicated rather
// than imported so cmake-configure-file stays a leaf binary
// without a converter dependency.
func applyGenexValuesAtRuntime(rendered []byte, values map[string]string) ([]byte, error) {
	ranges := topLevelGenexes(rendered)
	if len(ranges) == 0 {
		// No genex to replace — template already final.
		return rendered, nil
	}
	var out []byte
	out = make([]byte, 0, len(rendered))
	pos := 0
	for _, r := range ranges {
		out = append(out, rendered[pos:r.start]...)
		literal := string(rendered[r.start:r.end])
		val, ok := values[literal]
		if !ok {
			return nil, fmt.Errorf("no value for genex %q (the values JSON staged at lift time is missing a literal the template carries)", literal)
		}
		out = append(out, val...)
		pos = r.end
	}
	out = append(out, rendered[pos:]...)
	return out, nil
}

// topLevelGenexes mirrors converter/internal/lower's
// topLevelGenexes (depth-aware scan of `$<...>` blocks).
// Duplicated rather than imported so cmake-configure-file stays
// a leaf binary; the algorithm is small, unit-tested on both
// sides, and pure data.
func topLevelGenexes(s []byte) []struct{ start, end int } {
	var ranges []struct{ start, end int }
	i := 0
	for i < len(s) {
		if i+1 < len(s) && s[i] == '$' && s[i+1] == '<' {
			start := i
			depth := 1
			j := i + 2
			for j < len(s) && depth > 0 {
				switch {
				case j+1 < len(s) && s[j] == '$' && s[j+1] == '<':
					depth++
					j += 2
				case s[j] == '>':
					depth--
					j++
				default:
					j++
				}
			}
			if depth == 0 {
				ranges = append(ranges, struct{ start, end int }{start, j})
				i = j
				continue
			}
			i++
			continue
		}
		i++
	}
	return ranges
}
