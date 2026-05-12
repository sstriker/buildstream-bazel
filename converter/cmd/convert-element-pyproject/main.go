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
// See docs/design/pyproject-native-render.md for the full
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

	"github.com/sstriker/cmake-to-bazel/converter/internal/failure"
	"github.com/sstriker/cmake-to-bazel/internal/manifest"
)

const (
	exitSuccess = 0
	exitTier1   = 1
	exitUsage   = 64
	exitTier2   = 65
)

type args struct {
	sourceRoot      string
	outBuild        string
	outFailure      string
	importsManifest string
	elementName     string
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
		SourceFiles: srcs,
		Imports:     imports,
		ElementName: a.elementName,
	})
	if err != nil {
		return err
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
		if a.outFailure != "" {
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
