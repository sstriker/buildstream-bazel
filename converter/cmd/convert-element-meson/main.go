// convert-element-meson translates one meson source tree into a
// BUILD.bazel.out via meson's introspection JSON. Sister binary of
// converter/cmd/convert-element (the cmake-side translator).
//
// One invocation handles exactly one source tree. Run by write-a's
// per-element genrule (kind:meson) when --convert-element-meson is
// supplied to write-a; runnable standalone for development against
// a fixture.
//
// Pipeline:
//
//	meson setup <src> <bd>
//	    └─▶ <bd>/meson-info/intro-*.json
//	parse  ─▶ Introspect
//	lower  ─▶ ir.Package
//	emit   ─▶ BUILD.bazel.out
//
// See docs/design/meson-native-render.md for the full architecture
// and the patterns we cover vs. refuse.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/sstriker/cmake-to-bazel/converter/internal/emit/bazel"
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
	infoDir         string // offline mode: pre-recorded meson-info dir
	outBuild        string
	outFailure      string
	outBundleDir    string
	importsManifest string
	mesonExtraArgs  []string
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
	fs := flag.NewFlagSet("convert-element-meson", flag.ContinueOnError)
	fs.SetOutput(stderr)
	a := args{}
	fs.StringVar(&a.sourceRoot, "source-root", "", "absolute path to the meson source root")
	fs.StringVar(&a.infoDir, "info-dir", "", "skip meson invocation; read intro-*.json from this dir (testing)")
	fs.StringVar(&a.outBuild, "out-build", "BUILD.bazel.out", "destination path for generated BUILD.bazel.out")
	fs.StringVar(&a.outFailure, "out-failure", "", "write Tier-1 failure JSON here on per-codebase errors (optional)")
	fs.StringVar(&a.outBundleDir, "out-bundle-dir", "", "directory for synthesized pkg-config bundle (optional; v1 emits an empty bundle)")
	fs.StringVar(&a.importsManifest, "imports-manifest", "", "path to JSON imports manifest mapping cross-element meson dependency names to Bazel labels (optional)")
	var mesonArgs string
	fs.StringVar(&mesonArgs, "meson-args", "", "additional arguments to pass to `meson setup` (FDSDK's meson-local slot). Whitespace-split.")
	if err := fs.Parse(argv); err != nil {
		return a, exitUsage
	}
	if a.sourceRoot == "" && a.infoDir == "" {
		fmt.Fprintln(stderr, "convert-element-meson: must set --source-root or --info-dir")
		fs.Usage()
		return a, exitUsage
	}
	if mesonArgs != "" {
		a.mesonExtraArgs = strings.Fields(mesonArgs)
	}
	return a, exitSuccess
}

func run(a args) error {
	infoDir := a.infoDir
	buildDir := ""
	if infoDir == "" {
		bd, err := os.MkdirTemp("", "convert-element-meson-build-*")
		if err != nil {
			return err
		}
		buildDir = bd
		defer os.RemoveAll(bd)
		ctx := context.Background()
		got, err := runMesonSetup(ctx, mesonOptions{
			SourceRoot: a.sourceRoot,
			BuildDir:   bd,
			ExtraArgs:  a.mesonExtraArgs,
			Stdout:     os.Stderr,
			Stderr:     os.Stderr,
		})
		if err != nil {
			return failure.New(mesonSetupFailed, "%v", err)
		}
		infoDir = got
	} else if buildDir == "" {
		// Offline path: build dir is the meson-info's parent.
		buildDir = filepath.Dir(infoDir)
	}

	intro, err := Load(infoDir)
	if err != nil {
		return failure.New(mesonSetupFailed, "load introspect: %v", err)
	}

	var imports *manifest.Resolver
	if a.importsManifest != "" {
		imports, err = manifest.Load(a.importsManifest)
		if err != nil {
			return err
		}
	}

	pkg, err := Lower(intro, LowerOptions{
		SourceRoot: a.sourceRoot,
		BuildDir:   buildDir,
		Imports:    imports,
	})
	if err != nil {
		return err
	}

	out, err := bazel.Emit(pkg)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(a.outBuild), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(a.outBuild, out, 0o644); err != nil {
		return err
	}

	if a.outBundleDir != "" {
		// v1: emit an empty bundle directory. The genrule contract
		// requires the declared output to exist; the synthesis of
		// a real pkg-config tree is queued in
		// docs/design/meson-native-render.md.
		if err := os.MkdirAll(a.outBundleDir, 0o755); err != nil {
			return err
		}
	}
	return nil
}

func handleError(a args, err error) int {
	var tier1 *failure.Error
	if errors.As(err, &tier1) {
		fmt.Fprintf(os.Stderr, "convert-element-meson: %s\n", tier1.Error())
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
	fmt.Fprintf(os.Stderr, "convert-element-meson: %v\n", err)
	return exitTier2
}
