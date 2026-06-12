// imports-harvest produces an imports manifest from an install-shaped
// prefix tree — a bst artifact checkout, a host-install dir. It parses
// the prefix's cmake export bundles (lib/cmake/<Pkg>/*Targets*.cmake),
// .pc files, and stray bin/ executables into one manifest element with
// DIRECT deps (never flattened): the output is imports-wrapper-gen
// INPUT, whose synthesized cc_library wrappers give Bazel transitivity
// the closure. Labels are synthesized against --package using the
// generator's own naming, so generation is label-idempotent.
//
// Usage:
//
//	imports-harvest --prefix <dir> --element <name> \
//	  --package prebuilts/<name> --out exports.json
//
// Pipe into the generator to complete the loop:
//
//	imports-wrapper-gen --manifest exports.json --package prebuilts/<name> \
//	  --out-build <prefix>/BUILD.bazel --out-manifest exports.wrapped.json
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/sstriker/buildstream-bazel/internal/harvest"
	"github.com/sstriker/buildstream-bazel/internal/wrappergen"
)

func main() {
	fs := flag.NewFlagSet("imports-harvest", flag.ExitOnError)
	prefix := fs.String("prefix", "", "install-shaped tree to harvest (bst artifact checkout, host-install dir)")
	element := fs.String("element", "", "manifest element name")
	pkgPath := fs.String("package", "", "workspace-relative Bazel package the wrapper BUILD will land in (labels synthesize against it)")
	out := fs.String("out", "", "output manifest path")
	_ = fs.Parse(os.Args[1:])
	if *prefix == "" || *element == "" || *pkgPath == "" || *out == "" {
		fmt.Fprintln(os.Stderr, "imports-harvest: --prefix, --element, --package and --out are required")
		os.Exit(64)
	}
	im, warnings, err := harvest.Harvest(*prefix, *element, *pkgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "imports-harvest: %v\n", err)
		os.Exit(1)
	}
	for _, w := range warnings {
		fmt.Fprintf(os.Stderr, "imports-harvest: warning: %s\n", w)
	}
	if err := wrappergen.WriteManifest(*out, im); err != nil {
		fmt.Fprintf(os.Stderr, "imports-harvest: %v\n", err)
		os.Exit(1)
	}
}
