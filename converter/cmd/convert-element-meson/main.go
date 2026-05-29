// convert-element-meson translates one meson source tree into a
// BUILD.bazel.out via meson's introspection JSON. Sister binary of
// converter/cmd/convert-element-cmake (the cmake-side translator).
//
// One invocation handles exactly one source tree. Run by write-a's
// per-element genrule (kind:meson) when --convert-element-meson is
// supplied to write-a; runnable standalone for development against
// a fixture.
//
// Pipeline:
//
//	meson setup <bd> <src>      (meson's CLI is build-dir first,
//	    └─▶ <bd>/meson-info/    source-dir second)
//	    └─▶ intro-*.json
//	parse  ─▶ Introspect
//	lower  ─▶ ir.Package
//	emit   ─▶ BUILD.bazel.out
//
// See docs/architecture.md for the full architecture
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

	"github.com/sstriker/buildstream-bazel/converter/emit/bazel"
	"github.com/sstriker/buildstream-bazel/converter/internal/failure"
	"github.com/sstriker/buildstream-bazel/internal/convmode"
	"github.com/sstriker/buildstream-bazel/internal/manifest"
)

const (
	exitSuccess = 0
	exitTier1   = 1
	exitUsage   = 64
	exitTier2   = 65
)

type args struct {
	sourceRoot                string
	infoDir                   string // offline mode: pre-recorded meson-info dir
	outBuild                  string
	outFailure                string
	outBundleDir              string
	importsManifest           string
	mesonExtraArgs            []string
	unsupportedTargetFallback bool
	// fidelity is the operator-facing refusal-handling dial
	// threaded from cmd/write-a. "best-effort" implicitly enables
	// unsupportedTargetFallback. The same enum exists on every
	// converter binary so the operator-facing vocabulary is
	// uniform; per-converter --unsupported-*-fallback flags stay
	// as escape hatches.
	fidelity string
	// diagnostics is the operator-facing diagnostic-mode dial
	// threaded from cmd/write-a. Today meson has no rejection-
	// collection machinery (the converter exits on the first
	// refusal), so the flag is currently a no-op pass-through:
	// validation exists so the CLI accepts the flag uniformly
	// across converters, but enabling it changes nothing yet. A
	// future PR can add a Collector and write the rejection report
	// the way convert-element-cmake's --ignore-rejections-for-
	// diagnostics + --rejections-report shape does.
	diagnostics      bool
	bazelPackagePath string
}

// fallbackInstallTarget derives the same-package pipeline_install
// target label the round-2 target-fallback's pick_file stubs
// project files out of. write-a names that target
// "<elem>_trace_build", and the bazel package path's basename is
// the element name (e.g. "elements/foo" -> ":foo_trace_build").
// Empty package path yields "" (the lower-side default).
func fallbackInstallTarget(bazelPackagePath string) string {
	if bazelPackagePath == "" {
		return ""
	}
	return ":" + filepath.Base(bazelPackagePath) + "_trace_build"
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
	fs.StringVar(&a.sourceRoot, "source-root", "", "absolute path to the meson source root (required in both live and --info-dir mode; intro-targets.json records absolute source paths that the lowering pass projects against this prefix)")
	fs.StringVar(&a.infoDir, "info-dir", "", "skip meson invocation; read intro-*.json from this dir (testing). Still requires --source-root.")
	fs.StringVar(&a.outBuild, "out-build", "BUILD.bazel.out", "destination path for generated BUILD.bazel.out")
	fs.StringVar(&a.outFailure, "out-failure", "", "write Tier-1 failure JSON here on per-codebase errors (optional)")
	fs.StringVar(&a.outBundleDir, "out-bundle-dir", "", "directory for synthesized pkg-config bundle (optional; v1 emits an empty bundle)")
	fs.StringVar(&a.importsManifest, "imports-manifest", "", "path to JSON imports manifest mapping cross-element meson dependency names to Bazel labels (optional)")
	var mesonArgs string
	fs.StringVar(&mesonArgs, "meson-args", "", "additional arguments to pass to `meson setup` (FDSDK's meson-local slot). Whitespace-split.")
	fs.BoolVar(&a.unsupportedTargetFallback, "unsupported-target-fallback", false, "on typed Tier-1 refusal of the native lowering pass (unsupported-meson-subproject, unsupported-meson-custom-target, unsupported-meson-generated-sources, unsupported-meson-cross-compile, unresolved-meson-dependency, unsupported-meson-target-type), emit a placeholder BUILD.bazel.out derived from intro-install_plan.json + intro-buildoptions.json — per-target cc_import / sh_binary stubs plus pick_file targets projecting each referenced file out of the install-root TreeArtifact. Project B's pipeline_install (write-a's --fidelity=best-effort shape for kind:meson) installs into that TreeArtifact from a real `meson setup + ninja + meson install --destdir` run wrapped under build-tracer. Off by default to preserve the strict-fail behaviour. See docs/design/rendezvous.md. Low-level per-kind escape hatch; --fidelity=best-effort enables it implicitly.")
	fs.StringVar(&a.fidelity, "fidelity", "", "operator-facing refusal-handling dial: \"strict\" (default; refusals exit non-zero) or \"best-effort\" (refusals lower to placeholder shapes — for kind:meson, install-plan-derived stubs over the install-root TreeArtifact (via pick_file)). Implicitly enables --unsupported-target-fallback. Threaded verbatim from cmd/write-a; the same vocabulary applies to convert-element-cmake / -pyproject so the dial reads consistently across kinds.")
	fs.BoolVar(&a.diagnostics, "diagnostics", false, "operator-facing diagnostic-mode dial: when set, the converter is supposed to collect every Tier-1 refusal and continue rather than aborting on the first. Today the meson converter has no rejection-collection machinery (exits on the first refusal), so this flag is a no-op pass-through — the same flag exists on convert-element-cmake (where it actually does something) and on -pyproject so the CLI surface is uniform.")
	fs.StringVar(&a.bazelPackagePath, "bazel-package-path", "", "repo-root-relative path of the destination Bazel package (e.g. \"elements/foo\"). Frames the emitted `# gazelle:cc_search` directives so gazelle_cc's resolver — which interprets cc_search arguments repo-root relative — picks up the same include search paths meson recorded. Empty suppresses the directive.")
	if err := fs.Parse(argv); err != nil {
		return a, exitUsage
	}
	// Validate + apply the operator-facing dial.
	fidelity, err := convmode.ParseFidelity(a.fidelity)
	if err != nil {
		fmt.Fprintln(stderr, "convert-element-meson: "+err.Error())
		return a, exitUsage
	}
	a.fidelity = string(fidelity)
	if fidelity == convmode.FidelityBestEffort {
		a.unsupportedTargetFallback = true
	}
	// --source-root is required in both modes: meson introspection
	// records absolute source paths in intro-targets.json's
	// `sources` field, and Lower() refuses absolute paths without
	// a source-root to project against.
	if a.sourceRoot == "" {
		fmt.Fprintln(stderr, "convert-element-meson: --source-root is required (also in --info-dir mode; introspection sources are absolute paths)")
		fs.Usage()
		return a, exitUsage
	}
	// The lowering pass projects intro-targets.json's absolute
	// source paths against SourceRoot via string-prefix matching;
	// a relative --source-root would silently fall into the
	// "outside source root" / subproject refusal arm. Normalize
	// to an absolute path here so a caller-supplied relative
	// path Just Works (matches the convention every other
	// converter binary in this repo follows).
	if !filepath.IsAbs(a.sourceRoot) {
		abs, err := filepath.Abs(a.sourceRoot)
		if err != nil {
			fmt.Fprintf(stderr, "convert-element-meson: resolve --source-root %q: %v\n", a.sourceRoot, err)
			return a, exitUsage
		}
		a.sourceRoot = abs
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
		// Phase B fallback contract: the install rule in
		// project B pins `meson setup --prefix=/ --libdir=lib`,
		// which makes intro-install_plan.json's `{libdir_static}`
		// / `{libdir_shared}` / `{bindir}` / `{includedir}`
		// placeholders resolve to clean install-tree-relative
		// paths (lib/, bin/, include/). The converter side has to
		// match — without the same pin, intro-buildoptions reports
		// the host's defaults (multiarch libdir on debian,
		// /usr/local prefix everywhere) and the placeholder shape
		// in BUILD.bazel.out references paths that don't exist
		// inside the install root.
		//
		// We thread the pin via ExtraArgs so operator-supplied
		// --meson-args (the FDSDK meson-local slot) still wins on
		// duplicate-key resolution (meson takes the last value
		// for repeated -D flags). The pin only fires when the
		// fallback is enabled; Phase A's byte-stable shape stays
		// untouched.
		extraArgs := a.mesonExtraArgs
		if a.unsupportedTargetFallback {
			extraArgs = append([]string{"--prefix=/", "--libdir=lib"}, extraArgs...)
		}
		got, err := runMesonSetup(ctx, mesonOptions{
			SourceRoot: a.sourceRoot,
			BuildDir:   bd,
			ExtraArgs:  extraArgs,
			Stdout:     os.Stderr,
			Stderr:     os.Stderr,
		})
		if err != nil {
			return failure.New(mesonSetupFailed, "%v", err)
		}
		infoDir = got
	} else {
		// Offline path: build dir is the meson-info's parent.
		// Normalize the caller-supplied --info-dir to an
		// absolute path first; intro-targets.json's `sources` /
		// `filename` paths are absolute, and the lowering pass
		// compares them against buildDir via prefix-match. A
		// relative buildDir would never match those, so
		// custom_target output relativization would refuse with
		// "outside build dir" on every entry. filepath.Clean
		// strips a trailing slash so a caller-supplied
		// "/tmp/build/meson-info/" still yields "/tmp/build"
		// rather than "/tmp/build/meson-info".
		absInfo, err := filepath.Abs(infoDir)
		if err != nil {
			return fmt.Errorf("resolve --info-dir %q: %w", infoDir, err)
		}
		infoDir = absInfo
		buildDir = filepath.Dir(filepath.Clean(infoDir))
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
		// Phase B fallback: when --unsupported-target-fallback is
		// on and the native lowering refused with a typed Tier-1
		// failure, emit an install-plan-driven placeholder package
		// instead of propagating the refusal. Untyped errors (parse
		// failures, internal mismatches) still propagate — those
		// aren't kind:meson refusals the placeholder shape covers.
		//
		// Tier-1 refusals fall back when at least one install_plan
		// targets row exists. An empty install plan means there's
		// nothing for the placeholder to anchor against (no per-
		// target labels, no extract genrule outs); propagating the
		// Tier-1 in that case keeps the operator's signal honest —
		// the round-2 fallback can't help an element with no
		// installable outputs.
		//
		// The post-emit len(pkg.Targets) check covers a related
		// degenerate: a non-empty install plan whose rows all
		// filter out (every entry is a subproject, or every
		// destination has an unresolved placeholder, or every
		// (tag, basename) lands in artefactUnknown). The
		// pre-emit length check passes but the placeholder
		// produces zero stubs, so we'd land an empty BUILD on
		// disk and silently hide the typed refusal. Propagating
		// the original Tier-1 keeps the operator's diagnostic
		// signal intact in that case.
		if a.unsupportedTargetFallback {
			var tier1 *failure.Error
			if errors.As(err, &tier1) && len(intro.InstallPlan.Targets) > 0 {
				placeholderPkg, placeholderErr := emitFallbackPlaceholder(intro, LowerOptions{
					SourceRoot:            a.sourceRoot,
					BuildDir:              buildDir,
					Imports:               imports,
					FallbackInstallTarget: fallbackInstallTarget(a.bazelPackagePath),
				})
				if placeholderErr == nil && len(placeholderPkg.Targets) > 0 {
					pkg = placeholderPkg
					err = nil
				}
				// placeholderErr != nil OR zero targets: leave
				// `err` holding the original Tier-1 so the
				// caller falls through to the propagating
				// return below.
			}
		}
		if err != nil {
			return err
		}
	}

	out, err := bazel.EmitWithOptions(pkg, bazel.Options{BazelPackagePath: a.bazelPackagePath})
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
		// docs/architecture.md.
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
