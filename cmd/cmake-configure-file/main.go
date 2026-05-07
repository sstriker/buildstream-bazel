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
// The companion lift in converter/internal/lower (forthcoming
// commit on this branch) emits a genrule of shape
//
//	genrule(
//	    name = "gen_config_h",
//	    srcs = ["src/config.h.in", ":gen_config_h_values"],
//	    outs = ["config.h"],
//	    cmd  = "$(location //tools:cmake-configure-file) " +
//	           "--values=$(location :gen_config_h_values) " +
//	           "$(location src/config.h.in) $@",
//	    tools = ["//tools:cmake-configure-file"],
//	)
//
// .h.in lives in srcs (Bazel invalidates the genrule directly
// on edit), the values JSON sidecar carries the resolved cmake
// variables convert-element captured at conversion time. .h.in
// becomes name-only for srckey purposes; convert-element only
// reruns when the values change, which already requires a
// CMakeLists.txt edit (already in srckey content-include).
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/sstriker/cmake-to-bazel/internal/configurefile"
)

func main() {
	valuesPath := flag.String("values", "", "path to JSON file containing the {VAR: value, ...} substitution map. Required.")
	atOnly := flag.Bool("at-only", false, "skip ${VAR} substitution; only @VAR@ markers are replaced. Mirrors configure_file's @ONLY flag.")
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: cmake-configure-file [--at-only] --values=<values.json> <input> <output>")
		flag.PrintDefaults()
	}
	flag.Parse()

	args := flag.Args()
	if *valuesPath == "" || len(args) != 2 {
		flag.Usage()
		os.Exit(2)
	}
	if err := run(*valuesPath, args[0], args[1], configurefile.Options{AtOnly: *atOnly}); err != nil {
		fmt.Fprintf(os.Stderr, "cmake-configure-file: %v\n", err)
		os.Exit(1)
	}
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
