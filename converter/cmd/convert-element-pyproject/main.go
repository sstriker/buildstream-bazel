// convert-element-pyproject translates one kind:pyproject
// source tree into a BUILD.bazel.out via static analysis of
// pyproject.toml + the source-file universe. Sister binary
// of cmd/convert-element-trace and converter/cmd/convert-
// element-meson; runs inside write-a's per-element genrule
// for kind:pyproject elements when --convert-element-pyproject
// is supplied.
//
// Pipeline:
//
//	pyproject.toml + source tree
//	         │
//	         ▼
//	  parse + per-backend discovery       (parse.go + backends.go)
//	         │
//	         ▼
//	   Lower(...) Target list             (lower.go)
//	         │
//	         ▼
//	      Emit(...) BUILD.bazel.out       (emit.go)
//
// See docs/architecture.md for the full
// architecture and the patterns covered vs refused.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"

	"github.com/sstriker/buildstream-bazel/converter/internal/failure"
	"github.com/sstriker/buildstream-bazel/internal/manifest"
)

const (
	exitSuccess = 0
	exitTier1   = 1
	exitUsage   = 64
	exitTier2   = 65
)

type args struct {
	sourceRoot          string
	outBuild            string
	outFailure          string
	importsManifest     string
	elementName         string
	probe               bool
	alwaysEmitEntryShim bool
}

func main() {
	a, code := parseArgs(os.Args[1:], os.Stderr)
	if code != exitSuccess {
		os.Exit(code)
	}
	if err := run(a); err != nil {
		os.Exit(handleError(a, err))
	}
}

func parseArgs(argv []string, stderr *os.File) (args, int) {
	flags := flag.NewFlagSet("convert-element-pyproject", flag.ContinueOnError)
	flags.SetOutput(stderr)
	a := args{}
	flags.StringVar(&a.sourceRoot, "source-root", "", "absolute path to the pyproject source root (the directory containing pyproject.toml)")
	flags.StringVar(&a.outBuild, "out-build", "BUILD.bazel.out", "destination path for generated BUILD.bazel.out")
	flags.StringVar(&a.outFailure, "out-failure", "", "write Tier-1 failure JSON here on per-codebase errors (optional)")
	flags.StringVar(&a.importsManifest, "imports-manifest", "", "path to JSON imports manifest mapping cross-element pyproject distribution names to Bazel labels (optional)")
	flags.StringVar(&a.elementName, "element-name", "", "the .bst element name (optional). When set, emit a stable `py_library(name = <element-name>)` facade target that aggregates the per-package targets, so downstream consumers can reference the element via the convention bind `//elements/<element-name>:<element-name>` even when the primary py_library is named differently (e.g. setuptools' dist-name → package-name normalization, or script-name collision suffixing _lib).")
	flags.BoolVar(&a.probe, "probe", false, "probe-only mode: parse + discover + lower without emitting output. Exit 0 on would-succeed; non-zero otherwise. Tier-1 refusals (typed pyproject codes, including unresolved-pyproject-dependency when --imports-manifest is omitted on a dep-bearing project) exit 1 with the failure on stderr. Exit 64 = CLI usage error; exit 65 = any other untyped/Tier-2 error (filesystem issues, malformed imports manifest, unhandled converter path — not necessarily a bug). write-a's --pyproject-fallback dispatch treats any non-zero exit as 'would refuse' and falls back to the pipeline shape, so the 0-vs-non-zero contract is what callers should rely on.")
	flags.BoolVar(&a.alwaysEmitEntryShim, "always-emit-entry-shim", false, "force the legacy entry-shim genrule + py_binary shape for every [project.scripts] entry, even when the target module self-invokes via `if __name__ == \"__main__\":`. Default false (Phase 5) emits the strict shape — py_binary pointing directly at the module file with no shim — for self-invoking entry modules. Operator opt-in for entry modules whose top-level side effects make the shim's clean `from <m> import <f>; sys.exit(f() or 0)` shape preferable.")
	if err := flags.Parse(argv); err != nil {
		return a, exitUsage
	}
	if a.sourceRoot == "" {
		fmt.Fprintln(stderr, "convert-element-pyproject: --source-root is required")
		flags.Usage()
		return a, exitUsage
	}
	if !filepath.IsAbs(a.sourceRoot) {
		abs, err := filepath.Abs(a.sourceRoot)
		if err != nil {
			fmt.Fprintf(stderr, "convert-element-pyproject: resolve --source-root %q: %v\n", a.sourceRoot, err)
			return a, exitUsage
		}
		a.sourceRoot = abs
	}
	return a, exitSuccess
}

func run(a args) error {
	pyprojectPath := filepath.Join(a.sourceRoot, "pyproject.toml")
	p, err := Load(pyprojectPath)
	if err != nil {
		return err
	}

	srcs, err := walkSourceFiles(a.sourceRoot)
	if err != nil {
		return fmt.Errorf("walk source root: %w", err)
	}

	pkgs, err := Discover(p, srcs)
	if err != nil {
		return err
	}

	var imports *manifest.Resolver
	if a.importsManifest != "" {
		imports, err = manifest.Load(a.importsManifest)
		if err != nil {
			return err
		}
	}

	targets, err := Lower(p, pkgs, LowerOptions{
		SourceFiles:         srcs,
		Imports:             imports,
		ElementName:         a.elementName,
		AlwaysEmitEntryShim: a.alwaysEmitEntryShim,
		// Phase 5: read entry-module source for self-invoke
		// detection. Rooted at --source-root; the rel path
		// comes from entryModuleSourcePath which only ever
		// returns paths inside the package's Sources list (and
		// thus inside source-root). os.ReadFile errors propagate
		// to lowerScripts as "fall back to shim" — strictly
		// safe.
		ReadSource: func(rel string) ([]byte, error) {
			return os.ReadFile(filepath.Join(a.sourceRoot, rel))
		},
	})
	if err != nil {
		return err
	}

	// --probe stops here. Lower() returning nil error means the
	// native render would succeed against this source tree; exit
	// 0 without writing output. Operator (write-a's
	// --pyproject-fallback dispatch) reads the exit code to
	// decide native vs pipeline.
	if a.probe {
		return nil
	}

	body := Emit(targets)
	if err := os.MkdirAll(filepath.Dir(a.outBuild), 0o755); err != nil {
		return err
	}
	return os.WriteFile(a.outBuild, body, 0o644)
}

// walkSourceFiles returns every regular file under root, as
// source-relative slash-separated paths, sorted. Symlinks,
// devices, sockets, and other non-regular entries are
// skipped — the discovery + lowering passes only need the
// names of materialized source files, and a stray symlink to
// a path outside the source root would confuse the c-extension
// scan and the package-directory walks. The discovery pass +
// lowering pass both consume this list.
func walkSourceFiles(root string) ([]string, error) {
	var out []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.Type().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		out = append(out, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(out)
	return out, nil
}

func handleError(a args, err error) int {
	var tier1 *failure.Error
	if errors.As(err, &tier1) {
		fmt.Fprintf(os.Stderr, "convert-element-pyproject: %s\n", tier1.Error())
		// Probe mode is contract-side-effect-free: callers (write-a's
		// --pyproject-fallback dispatch) rely only on the exit code
		// + stderr text. Skip writing --out-failure so a probe run
		// can't unintentionally clobber a previous non-probe run's
		// failure JSON.
		if a.outFailure != "" && !a.probe {
			payload, _ := json.MarshalIndent(map[string]any{
				"tier":    1,
				"code":    string(tier1.Code),
				"message": tier1.Message,
			}, "", "  ")
			_ = os.MkdirAll(filepath.Dir(a.outFailure), 0o755)
			_ = os.WriteFile(a.outFailure, append(payload, '\n'), 0o644)
		}
		return exitTier1
	}
	fmt.Fprintf(os.Stderr, "convert-element-pyproject: %v\n", err)
	return exitTier2
}
