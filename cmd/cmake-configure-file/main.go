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
// template bytes; substitution rules are identical between the
// two modes — cmake's file(GENERATE) and configure_file share
// the @VAR@/${VAR}/#cmakedefine* surface.
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
// to the template resolves correctly without convert-element
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
// srckey so this re-runs convert-element correctly. The win
// vs. the legacy bytes-embedded shape is that BUILD.bazel
// content reflects the template, not the rendered output —
// edits to values (variables) re-render without changing the
// template blob.
//
// We could move the values to a separate sidecar file (with
// `srcs = [".h.in", ":gen_*_values"]`); the inline form keeps
// convert-element's output set unchanged (just BUILD.bazel) at
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

	"github.com/sstriker/cmake-to-bazel/internal/configurefile"
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
	if err := run(*valuesPath, inPath, contentBase64.set, contentBase64.val, outPath, opts); err != nil {
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
// case), substitutes, and writes the rendered output. The
// caller (main) is responsible for asserting exactly one of
// inPath / hasContent is set; run trusts that invariant.
func run(valuesPath, inPath string, hasContent bool, content, outPath string, opts configurefile.Options) error {
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
	} else if hasContent {
		tmpl, err = base64.StdEncoding.DecodeString(content)
		if err != nil {
			return fmt.Errorf("decode --content-base64: %w", err)
		}
	}
	rendered := configurefile.Substitute(tmpl, values, opts)
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
