package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func init() {
	registerHandler(pyprojectHandler{})
}

// pyprojectConfig holds render-time settings for the kind:pyproject
// native converter. Populated from main()'s --convert-element-
// pyproject flag before the per-element render loop runs. Empty
// convertBin disables the native path; kind:pyproject elements
// then render as the historical pipeline shape (coarse
// `python -m build --wheel` followed by `python -m pip install
// _bst_dist/*.whl` into %{install-root} — see
// pyprojectPipelineHandler below). Upstream buildstream-plugins-
// community ships an `installer`-based pipeline; this repo's
// pipeline shape was authored against pip first and keeps that
// shape so existing operator scripts that pass extra
// `--pip-args=...` overrides keep working.
//
// The split mirrors mesonConfig / traceConfig: keep the
// kindHandler interface small (RenderA / RenderB don't take a
// config arg) while letting the pyproject handler decide per-
// element whether to use native conversion.
var pyprojectConfig struct {
	// convertBin is the absolute path to convert-element-pyproject.
	// When set: the per-element BUILD.bazel in project A renders
	// as a genrule invoking //tools:convert-element-pyproject
	// against the staged source tree, producing BUILD.bazel.out.
	// When empty: the historical pipelineHandler shape renders.
	convertBin string
}

// pyprojectHandler is the kind:pyproject dispatch. It picks
// between the native render (when convert-element-pyproject is
// staged) and the pipelineHandler fallback (the historical
// coarse shape) at each RenderA / RenderB call. Stateless apart
// from the global config.
type pyprojectHandler struct{}

func (pyprojectHandler) Kind() string                                 { return "pyproject" }
func (pyprojectHandler) NeedsSources() bool                           { return true }
func (pyprojectHandler) HasProjectABuild() bool                       { return true }
func (pyprojectHandler) DefaultReadPathsPatterns() *readPathsPatterns { return nil }

func (pyprojectHandler) RenderA(elem *element, elemPkg string) error {
	if pyprojectConfig.convertBin == "" || pyprojectNativeIncompatible(elem) {
		return pyprojectPipelineHandler().RenderA(elem, elemPkg)
	}
	srcStage := filepath.Join(elemPkg, "sources")
	if err := os.RemoveAll(srcStage); err != nil {
		return err
	}
	if err := stageAllSources(elem, srcStage); err != nil {
		return err
	}
	hasImports, err := writePyprojectImportsManifest(elem, elemPkg)
	if err != nil {
		return err
	}
	return writeFile(filepath.Join(elemPkg, "BUILD.bazel"), pyprojectElementBuildA(elem, hasImports))
}

func (pyprojectHandler) RenderB(elem *element, elemPkg string) error {
	if pyprojectConfig.convertBin == "" || pyprojectNativeIncompatible(elem) {
		return pyprojectPipelineHandler().RenderB(elem, elemPkg)
	}
	if err := stageAllSources(elem, elemPkg); err != nil {
		return err
	}
	placeholder := fmt.Sprintf(`# Placeholder for cmd/write-a-rendered project B (kind:pyproject native).
# The driver script overwrites this file with project A's
# bazel-bin/elements/%s/BUILD.bazel.out (the converter's output)
# after the project-A bazel build succeeds. If this file is still
# the placeholder when project B's bazel build runs, the staging
# step was skipped.
filegroup(name = "BUILD_NOT_YET_STAGED", srcs = [])
`, elem.Name)
	return writeFile(filepath.Join(elemPkg, "BUILD.bazel"), placeholder)
}

// pyprojectNativeIncompatible reports whether the element's
// shape would break the native genrule's `--source-root=$SHADOW`
// invocation, forcing the pipeline-shape render even when the
// operator has supplied --convert-element-pyproject.
//
//	multi-source: stageAllSources merges every source into one
//	  shadow tree, but each source's contents land at distinct
//	  shadow-relative paths. The converter expects a single
//	  source-root containing one pyproject.toml.
//	Sources[0].Directory!="": stageAllSources places the source
//	  contents at `sources/<Directory>/...`, so pyproject.toml
//	  ends up at $SHADOW/<Directory>/pyproject.toml — but the
//	  genrule invokes the converter with `--source-root=$SHADOW`
//	  (no Directory suffix), so the converter wouldn't find
//	  pyproject.toml.
//
// These structural mismatches surface as confusing Bazel-build-
// time errors today; routing to pipeline shape at write-a time
// avoids the surprise. The per-element diagnostic is printed on
// stderr exactly once (cached by element name across the back-
// to-back RenderA / RenderB call pair). Operators see WHY a
// particular Directory-set or multi-source element fell back.
func pyprojectNativeIncompatible(elem *element) bool {
	if cached, ok := pyprojectStructuralFallback[elem.Name]; ok {
		return cached
	}
	if len(elem.Sources) > 1 {
		fmt.Fprintf(os.Stderr, "kind:pyproject %s: %d sources declared; native render's genrule passes --source-root=$SHADOW with the merged staged tree, but the converter expects a single source-root with one pyproject.toml. Routing to pipeline shape (the wheel-build genrule handles multi-source fine).\n",
			elem.Name, len(elem.Sources))
		pyprojectStructuralFallback[elem.Name] = true
		return true
	}
	if len(elem.Sources) == 1 && elem.Sources[0].Directory != "" {
		fmt.Fprintf(os.Stderr, "kind:pyproject %s: source has Directory=%q; the native genrule stages it under that subpath, but invokes the converter with --source-root=$SHADOW (no Directory suffix). Routing to pipeline shape (which honors Directory via the pipeline handler's source staging).\n",
			elem.Name, elem.Sources[0].Directory)
		pyprojectStructuralFallback[elem.Name] = true
		return true
	}
	pyprojectStructuralFallback[elem.Name] = false
	return false
}

// pyprojectStructuralFallback memoizes pyprojectNativeIncompatible's
// result by element name so the back-to-back RenderA / RenderB
// pair prints the diagnostic at most once per element per
// write-a invocation.
var pyprojectStructuralFallback = map[string]bool{}

// pyprojectPipelineHandler returns the pipeline-shape handler
// used when the native path is disabled. Defaults mirror upstream
// buildstream-plugins-community's pyproject.{py,yaml} (see
// docs/design/pyproject-native-render.md for the upstream
// snippet), with one shape difference: upstream installs via
// `python -m installer`, this repo's pipeline uses
// `python -m pip install` so existing operator scripts that
// pass extra `--pip-args=...` overrides keep working.
// `dist-dir` defaults to `_bst_dist` to avoid colliding with a
// project's own `./dist/` if its sources already ship one;
// `build-args` carries the default `--wheel --no-isolation` so
// an operator overriding `variables: build-args: ...` in their
// .bst element actually changes the rendered command.
// `installer-args` is intentionally absent — upstream uses it
// with `python -m installer`, but our `pip install` shape has
// no equivalent knob, and keeping a documented-but-ignored var
// is more confusing than not having it.
func pyprojectPipelineHandler() pipelineHandler {
	return pipelineHandler{
		kindName: "pyproject",
		defaultVars: map[string]string{
			"python":        "python3",
			"pip":           "pip",
			"python-prefix": "%{prefix}/lib/python3",
			"pip-args":      `--no-build-isolation --no-deps --no-index --target="%{install-root}%{python-prefix}"`,
			"build-args":    "--wheel --no-isolation",
			"dist-dir":      "_bst_dist",
		},
		defaults: pipelineDefaults{
			Build: []string{
				`%{python} -m build %{build-args} --outdir %{dist-dir} .`,
			},
			Install: []string{
				`%{python} -m %{pip} install %{pip-args} %{dist-dir}/*.whl`,
			},
		},
	}
}

// pyprojectElementBuildA renders the per-element BUILD.bazel
// for project A's kind:pyproject native shape. Mirrors
// mesonElementBuildA / cmakeElementBuild — one genrule that:
//
//   - Stages the element's source tree under a fresh shadow dir
//     (so pyproject.toml + the package layout live in a single
//     materialized tree rather than scattered Bazel-supplied
//     paths).
//   - Invokes //tools:convert-element-pyproject against it.
//   - Produces BUILD.bazel.out (no bundle artifact in v1; cross-
//     element resolution is purely via the imports manifest).
//
// hasImports tells us whether writePyprojectImportsManifest
// wrote a non-empty imports.json; when true, the genrule pulls
// it into srcs and threads --imports-manifest into the
// converter invocation.
func pyprojectElementBuildA(elem *element, hasImports bool) string {
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
    ],
    cmd = """
        # Build a unified source-root by copying the staged real
        # srcs into a fresh shadow dir. The pattern mirrors the
        # kind:cmake / kind:meson handlers' shadow-merge: each
        # src path contains "sources/" — strip up to that
        # segment to recover the source-relative suffix and lay
        # it down inside SHADOW.
        SHADOW="$$(mktemp -d)"
        for src in $(SRCS); do
            case "$$src" in
                */imports.json) continue ;;
            esac
            rel="$${src##*sources/}"
            mkdir -p "$$SHADOW/$$(dirname "$$rel")"
            cp -L "$$src" "$$SHADOW/$$rel"
        done
        $(location //tools:convert-element-pyproject) \\
            --source-root="$$SHADOW" \\
            --element-name="%[1]s" \\
            --out-build="$(location BUILD.bazel.out)"%[3]s
    """,
    tools = ["//tools:convert-element-pyproject"],
)

filegroup(
    name = "build_bazel",
    srcs = ["BUILD.bazel.out"],
)
`, elem.Name, srcsList, importsFlag)

	return b.String()
}

// writePyprojectImportsManifest renders an imports.json next to
// the element's BUILD.bazel when the element has any cross-
// element deps. Kind-agnostic walk — convert-element-pyproject
// resolves [project.dependencies] entries against any provider
// that emits an exports entry (kind:autotools / kind:cmake /
// kind:meson / kind:pyproject). Schema is shared with
// internal/manifest; convention bind <dep>::<dep> →
// //elements/<dep>:<dep>.
//
// Returns (true, nil) when imports.json was written; (false,
// nil) when the element has no resolvable cross-element deps.
func writePyprojectImportsManifest(elem *element, elemPkg string) (bool, error) {
	var entries []string
	for _, dep := range elem.Deps {
		if dep == nil || dep.Bst == nil {
			continue
		}
		entries = append(entries, dep.Name)
	}
	if len(entries) == 0 {
		return false, nil
	}
	var b strings.Builder
	b.WriteString(`{
  "version": 1,
  "elements": [
`)
	for i, name := range entries {
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
    }`, name,
			name+"::"+name,
			name, name)
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
