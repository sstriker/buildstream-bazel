package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func init() {
	registerHandler(mesonHandler{})
}

// mesonConfig holds render-time settings for the kind:meson native
// converter. Populated from main()'s --convert-element-meson flag
// before the per-element render loop runs. Empty convertBin
// disables the native path entirely; kind:meson elements then
// render as the historical pipeline shape (coarse `meson setup +
// ninja + meson install` genrule), which is what FDSDK has been
// running against since the pipelineHandler-based handler landed.
//
// The split mirrors traceConfig: keep the kindHandler interface
// small (RenderA/RenderB don't take a config arg) while letting the
// meson handler decide per-element whether to use native conversion.
var mesonConfig struct {
	// convertBin is the absolute path to convert-element-meson.
	// When set: the per-element BUILD.bazel in project A renders
	// as a genrule invoking //tools:convert-element-meson against
	// the staged source tree, producing BUILD.bazel.out + a
	// (currently empty) pkg-config-bundle.tar.
	// When empty: the historical pipelineHandler shape renders.
	convertBin string
}

// mesonHandler is the kind:meson dispatch. It picks between the
// native render (when convert-element-meson is staged) and the
// pipelineHandler fallback (the historical coarse shape) at each
// RenderA/RenderB call. Stateless apart from the global config.
type mesonHandler struct{}

func (mesonHandler) Kind() string                                 { return "meson" }
func (mesonHandler) NeedsSources() bool                           { return true }
func (mesonHandler) HasProjectABuild() bool                       { return true }
func (mesonHandler) DefaultReadPathsPatterns() *readPathsPatterns { return nil }

func (mesonHandler) RenderA(elem *element, elemPkg string) error {
	if mesonConfig.convertBin == "" {
		return mesonPipelineHandler().RenderA(elem, elemPkg)
	}
	srcStage := filepath.Join(elemPkg, "sources")
	if err := os.RemoveAll(srcStage); err != nil {
		return err
	}
	if err := stageAllSources(elem, srcStage); err != nil {
		return err
	}
	hasImports, err := writeMesonImportsManifest(elem, elemPkg)
	if err != nil {
		return err
	}
	return writeFile(filepath.Join(elemPkg, "BUILD.bazel"), mesonElementBuildA(elem, hasImports))
}

func (mesonHandler) RenderB(elem *element, elemPkg string) error {
	if mesonConfig.convertBin == "" {
		return mesonPipelineHandler().RenderB(elem, elemPkg)
	}
	if err := stageAllSources(elem, elemPkg); err != nil {
		return err
	}
	// Same placeholder shape kind:cmake's RenderB writes when the
	// converter genrule lives in project A. The driver script
	// overwrites this with project A's BUILD.bazel.out after
	// `bazel build` completes.
	placeholder := fmt.Sprintf(`# Placeholder for cmd/write-a-rendered project B (kind:meson native).
# The driver script overwrites this file with project A's
# bazel-bin/elements/%s/BUILD.bazel.out (the converter's output)
# after the project-A bazel build succeeds. If this file is still
# the placeholder when project B's bazel build runs, the staging
# step was skipped.
filegroup(name = "BUILD_NOT_YET_STAGED", srcs = [])
`, elem.Name)
	return writeFile(filepath.Join(elemPkg, "BUILD.bazel"), placeholder)
}

// mesonPipelineHandler returns the pipeline-shape handler used
// when the native path is disabled. Defaults mirror upstream
// buildstream-plugins' meson element (see meson.py) and the FDSDK
// reality-check probe (`meson-local` is the dominant per-element
// customization point).
func mesonPipelineHandler() pipelineHandler {
	return pipelineHandler{
		kindName: "meson",
		defaultVars: map[string]string{
			"build-dir":    "_builddir",
			"meson-source": ".",
			"meson-args":   "%{meson-source} %{build-dir}",
			"meson-extra":  "",
			"meson-local":  "",
			"meson-global": "",
			"make":         `ninja -C "%{build-dir}" -j 0`,
			"make-install": `DESTDIR="%{install-root}" ninja -C "%{build-dir}" install`,
			"meson":        `meson %{meson-args} %{meson-extra} %{meson-local} %{meson-global}`,
		},
		defaults: pipelineDefaults{
			Configure: []string{`%{meson}`},
			Build:     []string{`%{make}`},
			Install:   []string{`%{make-install}`},
		},
	}
}

// mesonElementBuildA renders the per-element BUILD.bazel for
// project A's kind:meson native shape. Mirrors the structure of
// cmakeElementBuild — one genrule that:
//
//   - Stages the source tree under a freshly-created shadow dir
//     (so the meson source root is a single materialized tree
//     rather than scattered Bazel-supplied paths).
//   - Invokes //tools:convert-element-meson against it.
//   - Produces BUILD.bazel.out + pkg-config-bundle.tar (the latter
//     is an empty tar in v1; kept as a declared output so the
//     genrule contract is forward-compatible with the bundle
//     synthesis follow-up).
//
// hasImports tells us whether writeMesonImportsManifest wrote a
// non-empty imports.json next to this BUILD.bazel; when true, the
// genrule pulls it into srcs and threads --imports-manifest into
// the converter invocation. The hint is computed by the caller
// because the manifest is rendered at write-a time (not here),
// and we need to keep the BUILD shape in sync with what's
// actually on disk.
//
// Per-element customization (the FDSDK `meson-local` slot) flows
// through the variables: block but isn't yet wired here — the
// converter's --meson-args flag is left empty for v1. When a fixture
// surfaces a need, plumb it in.
func mesonElementBuildA(elem *element, hasImports bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, `# Generated by cmd/write-a. Do not edit by hand.

package(default_visibility = ["//visibility:public"])
`)

	fmt.Fprintf(&b, `
filegroup(
    name = "%[1]s_real",
    srcs = glob(["sources/**"]),
)
`, elem.Name)

	// v1: imports.json is the only cross-element wiring we plumb
	// into convert-element-meson today (used to resolve external
	// dependency('foo') names onto Bazel labels). Gate it on
	// whether the manifest was actually written rather than on
	// the kind:meson-only bundle list — meson can resolve deps
	// against any provider that emits an exports entry, including
	// kind:cmake / kind:autotools elements. The pkg-config bundle
	// synthesis + --prefix-dir staging land alongside the Phase B
	// install-plan fallback queued in ROADMAP.md.
	srcsList := fmt.Sprintf(`":%s_real"`, elem.Name)
	importsFlag := ""
	if hasImports {
		srcsList += `, "imports.json"`
		importsFlag = ` \
            --imports-manifest="$(location imports.json)"`
	}

	fmt.Fprintf(&b, `
genrule(
    name = "%[1]s_converted",
    srcs = [%[2]s],
    outs = [
        "BUILD.bazel.out",
        "pkg-config-bundle.tar",
    ],
    cmd = """
        # Build a unified source-root by copying the staged real
        # srcs into a fresh shadow dir. The pattern mirrors the
        # kind:cmake handler's shadow-merge: each src path contains
        # "sources/" — strip up to that segment to recover the
        # source-relative suffix and lay it down inside SHADOW.
        SHADOW="$$(mktemp -d)"
        for src in $(SRCS); do
            case "$$src" in
                */imports.json) continue ;;
            esac
            rel="$${src##*sources/}"
            mkdir -p "$$SHADOW/$$(dirname "$$rel")"
            cp -L "$$src" "$$SHADOW/$$rel"
        done
        BUNDLE_DIR="$$(mktemp -d)"
        $(location //tools:convert-element-meson) \\
            --source-root="$$SHADOW" \\
            --out-build="$(location BUILD.bazel.out)" \\
            --out-bundle-dir="$$BUNDLE_DIR"%[3]s
        # v1 emits an empty bundle dir. We deliberately do NOT use
        # "tar -C $$BUNDLE_DIR ." — that includes the "." directory
        # entry with the bundle dir's mtime/uid/gid, making the tar
        # non-deterministic across builds and invalidating Bazel's
        # cache for unrelated reasons. Instead, tar an explicit
        # empty file list via -T /dev/null, which produces a
        # zero-content archive that's byte-identical run-to-run.
        # When the bundle synthesis follow-up actually populates
        # BUNDLE_DIR, switch to a determinism-preserving
        # invocation (--sort=name --owner=0 --group=0 --mtime=@0)
        # so producing-content stays cache-stable.
        tar -cf "$(location pkg-config-bundle.tar)" -T /dev/null
    """,
    tools = ["//tools:convert-element-meson"],
)

filegroup(
    name = "build_bazel",
    srcs = ["BUILD.bazel.out"],
)

filegroup(
    name = "pkg_config_bundle",
    srcs = ["pkg-config-bundle.tar"],
)
`, elem.Name, srcsList, importsFlag)

	return b.String()
}

// mesonDepBundleLabel pairs a cross-element kind:meson dep's name
// with the Bazel label of its `pkg_config_bundle` filegroup. v1
// preserves the field for symmetry with cmakeDepBundleLabel; the
// bundle is empty until the synthesis follow-up.
type mesonDepBundleLabel struct {
	DepName string
	Label   string
}

// mesonDepBundleLabels returns the cross-element bundle labels
// the consumer's genrule should stage. Filters to kind:meson
// deps; other kinds don't ship a meson-side bundle.
func mesonDepBundleLabels(elem *element) []mesonDepBundleLabel {
	var out []mesonDepBundleLabel
	for _, dep := range elem.Deps {
		if dep == nil || dep.Bst == nil {
			continue
		}
		if dep.Bst.Kind != "meson" {
			continue
		}
		out = append(out, mesonDepBundleLabel{
			DepName: dep.Name,
			Label:   fmt.Sprintf("//elements/%s:pkg_config_bundle", dep.Name),
		})
	}
	return out
}

// writeMesonImportsManifest renders an imports.json next to the
// element's BUILD.bazel when the element has any cross-element
// deps. One Element entry per dep, with a single Export per dep
// following the convention `<dep>::<dep>` → //elements/<dep>:<dep>.
//
// The manifest schema is shared with the cmake side
// (internal/manifest); convert-element-meson's
// `LookupCMakeTarget(name)` resolves both `<dep>` and
// `<dep>::<dep>` so the convention bind matches.
//
// Dep walk is intentionally kind-agnostic: meson elements can
// `dependency('foo')` against providers of any kind (kind:cmake,
// kind:autotools, kind:meson, …), and convert-element-meson
// resolves through the shared manifest schema regardless of who
// emitted the dep. Restricting the walk to kind:meson would
// silently drop valid bindings (PR #106 review feedback).
// The mesonDepBundleLabels filter stays kind:meson-only — only
// meson elements ship a meson-shaped pkg-config bundle.
//
// Returns (true, nil) when imports.json was written;
// (false, nil) when the element has no resolvable cross-element
// deps.
func writeMesonImportsManifest(elem *element, elemPkg string) (bool, error) {
	type entry struct{ name string }
	var entries []entry
	for _, dep := range elem.Deps {
		if dep == nil || dep.Bst == nil {
			continue
		}
		entries = append(entries, entry{name: dep.Name})
	}
	if len(entries) == 0 {
		return false, nil
	}
	var b strings.Builder
	b.WriteString(`{
  "version": 1,
  "elements": [
`)
	for i, e := range entries {
		if i > 0 {
			b.WriteString(",\n")
		}
		fmt.Fprintf(&b, `    {
      "name": %q,
      "exports": [
        {
          "cmake_target": %q,
          "bazel_label": "//elements/%s:%s"
        }
      ]
    }`, e.name,
			e.name+"::"+e.name,
			e.name, e.name)
	}
	b.WriteString(`
  ]
}
`)
	if err := writeFile(filepath.Join(elemPkg, "imports.json"), b.String()); err != nil {
		return false, err
	}
	return true, nil
}
