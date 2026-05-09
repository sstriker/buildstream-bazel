// probe-cell-fixture is a small helper that produces probe.json
// artifacts from a recorded fileapi reply directory. Used by the
// Stage 5 render gate (scripts/meta-unify-toolchains.sh) to feed
// unify-toolchains synthetic per-cell probe data without needing a
// real cmake invocation.
//
// Not intended for production use — production runs build probe
// artifacts via the project-A genrules invoking probe-cell against
// real cmake. This binary only exists so the render gate has a
// hermetic input source.
//
// Usage:
//
//	probe-cell-fixture \
//	    --fileapi-fixture <dir> \
//	    --out-dir <dir> \
//	    --cell <platform>:<variant>[:K=V[,K=V...]] \
//	    --cell ...
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/sstriker/cmake-to-bazel/converter/internal/fileapi"
	"github.com/sstriker/cmake-to-bazel/converter/internal/toolchain"
	"github.com/sstriker/cmake-to-bazel/converter/internal/toolchain/probejson"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "probe-cell-fixture:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	fs := flag.NewFlagSet("probe-cell-fixture", flag.ContinueOnError)
	var (
		fixDir = fs.String("fileapi-fixture", "", "directory of a recorded cmake File API reply")
		outDir = fs.String("out-dir", "", "where probe.json files land")
	)
	var cells stringSliceFlag
	fs.Var(&cells, "cell", "cell spec: <platform>:<variant>[:K=V[,K=V...]] (repeatable)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *fixDir == "" || *outDir == "" || len(cells) == 0 {
		return fmt.Errorf("--fileapi-fixture, --out-dir, and at least one --cell are required")
	}

	r, err := fileapi.Load(*fixDir)
	if err != nil {
		return fmt.Errorf("load fixture: %w", err)
	}
	if err := os.MkdirAll(*outDir, 0o755); err != nil {
		return err
	}

	for _, spec := range cells {
		plat, variant, err := parseCellSpec(spec)
		if err != nil {
			return err
		}
		body, err := probejson.Marshal(variant, r)
		if err != nil {
			return fmt.Errorf("marshal %s: %w", spec, err)
		}
		name := plat + "." + variant.Name + ".probe.json"
		if err := os.WriteFile(filepath.Join(*outDir, name), body, 0o644); err != nil {
			return fmt.Errorf("write %s: %w", name, err)
		}
	}
	return nil
}

// parseCellSpec decodes "<platform>:<variant>[:K=V,K2=V2,...]".
func parseCellSpec(s string) (platform string, v toolchain.Variant, err error) {
	parts := strings.SplitN(s, ":", 3)
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return "", toolchain.Variant{}, fmt.Errorf("--cell %q: expected <platform>:<variant>[:K=V,...]", s)
	}
	v = toolchain.Variant{Name: parts[1]}
	if len(parts) == 3 && parts[2] != "" {
		v.CacheVars = map[string]string{}
		for _, kv := range strings.Split(parts[2], ",") {
			k, val, ok := strings.Cut(kv, "=")
			if !ok || k == "" {
				return "", toolchain.Variant{}, fmt.Errorf("--cell %q: cache var %q not in K=V form", s, kv)
			}
			v.CacheVars[k] = val
		}
	}
	return parts[0], v, nil
}

type stringSliceFlag []string

func (s *stringSliceFlag) String() string     { return strings.Join(*s, ",") }
func (s *stringSliceFlag) Set(v string) error { *s = append(*s, v); return nil }
