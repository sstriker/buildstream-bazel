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
	"github.com/sstriker/buildstream-bazel/internal/manifest"
	"github.com/sstriker/buildstream-bazel/internal/wrappergen"
)

// stringSlice collects a repeatable string flag.
type stringSlice []string

func (s *stringSlice) String() string     { return fmt.Sprint([]string(*s)) }
func (s *stringSlice) Set(v string) error { *s = append(*s, v); return nil }

// buildRegistry merges the sibling exports.json manifests into a
// cmake-target → bazel-label map, so a harvest resolves an
// INTERFACE_LINK_LIBRARIES / .pc Requires ref to a target ANOTHER element
// exports (a cross-element dep) instead of warn-dropping it. Later manifests
// win on a duplicate cmake target (deterministic: the caller orders --registry).
func buildRegistry(paths []string) (map[string]string, error) {
	if len(paths) == 0 {
		return nil, nil
	}
	reg := map[string]string{}
	for _, p := range paths {
		doc, err := manifest.LoadDoc(p)
		if err != nil {
			return nil, fmt.Errorf("registry %s: %w", p, err)
		}
		for _, el := range doc.Elements {
			for _, ex := range el.Exports {
				if ex.CMakeTarget != "" && ex.BazelLabel != "" {
					reg[ex.CMakeTarget] = ex.BazelLabel
				}
			}
		}
	}
	return reg, nil
}

func main() {
	fs := flag.NewFlagSet("imports-harvest", flag.ExitOnError)
	prefix := fs.String("prefix", "", "install-shaped tree to harvest (bst artifact checkout, host-install dir)")
	element := fs.String("element", "", "manifest element name")
	pkgPath := fs.String("package", "", "workspace-relative Bazel package the wrapper BUILD will land in (labels synthesize against it)")
	out := fs.String("out", "", "output manifest path")
	var registry stringSlice
	fs.Var(&registry, "registry", "sibling exports.json manifest to resolve cross-element deps against (repeatable); a ref this prefix didn't harvest but another element exports resolves to that element's label instead of being dropped")
	_ = fs.Parse(os.Args[1:])
	if *prefix == "" || *element == "" || *pkgPath == "" || *out == "" {
		fmt.Fprintln(os.Stderr, "imports-harvest: --prefix, --element, --package and --out are required")
		os.Exit(64)
	}
	reg, err := buildRegistry(registry)
	if err != nil {
		fmt.Fprintf(os.Stderr, "imports-harvest: %v\n", err)
		os.Exit(1)
	}
	im, warnings, err := harvest.HarvestWithRegistry(*prefix, *element, *pkgPath, reg)
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
