// cmake-configure-file is the Bazel-time substitution tool the
// recovered configure_file genrule invokes. Reads a values JSON
// (a flat object mapping cmake variable names to string values),
// reads the .h.in template, applies cmake's documented
// substitution rules (see internal/configurefile package doc),
// and writes the rendered output.
//
// Usage (from within the recovered genrule):
//
//	cmake-configure-file [--at-only] \
//	    --values=<values.json> \
//	    <input.h.in> <output>
//
// The companion lift in converter/internal/lower
// (configureFileLiftedCmd) emits a genrule of shape:
//
//	genrule(
//	    name = "gen_config_h",
//	    srcs = ["src/config.h.in"],
//	    outs = ["config.h"],
//	    cmd  = "mkdir -p $$(dirname $@) && "
//	           "VALUES=$$(mktemp -p $$(dirname $@) ...) && "
//	           "echo <base64-of-values-json> | base64 -d > $$VALUES && "
//	           "$(execpath //tools:cmake-configure-file) [--at-only] "
//	           "--values=$$VALUES $(execpath src/config.h.in) $@ ; "
//	           "rc=$$?; rm -f $$VALUES; exit $$rc",
//	    tools = ["//tools:cmake-configure-file"],
//	)
//
// .h.in lives in srcs (Bazel invalidates the genrule directly
// on edit). The values JSON is inlined into the cmd as a small
// base64 blob (typically a few hundred bytes — one entry per
// variable the template references) — much smaller than the
// rendered output it replaces, AND independent of .h.in
// content. .h.in becomes name-only for srckey purposes;
// convert-element only reruns when the values blob changes,
// which already requires a CMakeLists.txt edit (already in
// srckey content-include).
//
// We could move the values to a separate sidecar file (with
// `srcs = [".h.in", ":gen_*_values"]`); the inline form keeps
// convert-element's output set unchanged (just BUILD.bazel) at
// the cost of a marginally longer cmd. Inline is the v1 shape;
// the sidecar form is a follow-up if the cmd's length becomes
// a problem in practice.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/sstriker/cmake-to-bazel/internal/configurefile"
)

func main() {
	valuesPath := flag.String("values", "", "path to JSON file containing the {VAR: value, ...} substitution map. Required.")
	atOnly := flag.Bool("at-only", false, "skip ${VAR} substitution; only @VAR@ markers are replaced. Mirrors configure_file's @ONLY flag.")
	copyOnly := flag.Bool("copy-only", false, "skip ALL substitution (@VAR@, ${VAR}, #cmakedefine*) and emit the template verbatim. Mirrors configure_file's COPYONLY flag.")
	escapeQuotes := flag.Bool("escape-quotes", false, "backslash-escape `\"` (and `\\\\`) in expanded values. Mirrors configure_file's ESCAPE_QUOTES flag.")
	newlineStyle := flag.String("newline-style", "", "rewrite the line terminator: 'lf'|'unix' for `\\n`, 'crlf'|'dos'|'win32' for `\\r\\n`. Empty preserves the template's original style. Mirrors configure_file's NEWLINE_STYLE flag.")
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: cmake-configure-file [--at-only] [--copy-only] [--escape-quotes] [--newline-style=lf|crlf] --values=<values.json> <input> <output>")
		flag.PrintDefaults()
	}
	flag.Parse()

	args := flag.Args()
	if *valuesPath == "" || len(args) != 2 {
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
	if err := run(*valuesPath, args[0], args[1], opts); err != nil {
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

func run(valuesPath, inPath, outPath string, opts configurefile.Options) error {
	values, err := loadValues(valuesPath)
	if err != nil {
		return fmt.Errorf("load values %s: %w", valuesPath, err)
	}
	tmpl, err := os.ReadFile(inPath)
	if err != nil {
		return fmt.Errorf("read template %s: %w", inPath, err)
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
