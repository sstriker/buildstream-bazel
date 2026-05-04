package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/sstriker/cmake-to-bazel/internal/srckeyregistry"
)

// init registers kind:autotools. The handler always falls back
// to the coarse install-pipeline shape; when --convert-element-
// autotools is supplied, it additionally wraps the build cmd in
// build-tracer + runs convert-element-autotools to emit a native
// BUILD.bazel.out alongside the install_tree.tar.
//
// One genrule with two outputs (install_tree.tar +
// BUILD.bazel.out). Bazel's action cache (buildbarn in CI)
// handles convergence — same source + same toolchain + same
// converter version → same action result, shared across nodes
// via the existing remote-cache plumbing. No separate registry
// needed; the "B → A feedback" lives entirely inside the
// Bazel-action graph.
func init() {
	registerHandler(autotoolsHandler{})
}

// autotoolsConfig holds the render-time settings for the
// trace-driven autotools converter. Populated from main()'s
// flags before the per-element render loop runs. Empty
// convertBin disables the trace+convert wrap entirely
// (rendered output is the unmodified pipeline shape).
//
// Package-level state keeps the kindHandler interface small
// (RenderA / RenderB don't take a config arg) while letting
// the autotools handler decide per-element whether to install
// the extension hooks.
var autotoolsConfig struct {
	convertBin     string // absolute path to convert-element-autotools
	tracerBin      string // absolute path to build-tracer
	srckeyRegistry string // optional: absolute path to the srckey registry root
}

// autotoolsHandler picks the right pipelineHandler shape based
// on the global autotoolsConfig. Without a converter binary,
// the coarse install_tree.tar pipeline is the rendered shape;
// with it, the pipelineExtension wraps the cmd in build-tracer
// and runs convert-element-autotools after the install phase.
type autotoolsHandler struct{}

func (autotoolsHandler) Kind() string                                 { return "autotools" }
func (autotoolsHandler) NeedsSources() bool                           { return true }
func (autotoolsHandler) HasProjectABuild() bool                       { return true }
func (autotoolsHandler) DefaultReadPathsPatterns() *readPathsPatterns { return nil }

func (autotoolsHandler) RenderA(elem *element, elemPkg string) error {
	h, err := autotoolsPipelineHandlerForElement(elem, elemPkg)
	if err != nil {
		return err
	}
	if err := h.RenderA(elem, elemPkg); err != nil {
		return err
	}
	// Emit srckey.txt + srckey-breakdown.txt — the per-element
	// build-graph identity used by the trace-driven registry
	// (see srckey.go). Only emitted when the trace-driven path
	// is enabled (matches the `convertBin` set guard the
	// pipelineHandlerForElement applied above), since coarse
	// pipeline elements don't participate in the registry.
	if autotoolsConfig.convertBin != "" {
		if err := renderSrckey(elem, elemPkg, autotoolsSrckeyPatterns()); err != nil {
			return err
		}
		if err := renderSrckeyCacheStatus(elem, elemPkg); err != nil {
			return err
		}
	}
	return nil
}

// renderSrckeyCacheStatus checks the srckey registry (when
// configured) for a hit on the element's srckey and emits a
// `srckey-cache-status.txt` companion file recording the
// outcome. Format:
//
//	hit	<srckey>		   — registry has a registered trace.log
//	miss	<srckey>	   — registry doesn't (yet) have one
//	off	<srckey>		   — no --srckey-registry flag passed; no lookup
//
// Round-1/round-2 plumbing reads this file to decide whether
// the install genrule emits the standard build-tracer-wrapped
// pipeline (miss) or a converter-only shape (hit). PR4 emits
// the status file as a foundation; the round-2 render-shape
// switch lands separately so the registry plumbing can be
// reviewed in isolation.
func renderSrckeyCacheStatus(elem *element, elemPkg string) error {
	keyPath := filepath.Join(elemPkg, "srckey.txt")
	keyBytes, err := os.ReadFile(keyPath)
	if err != nil {
		return fmt.Errorf("element %q: read srckey: %w", elem.Name, err)
	}
	srckey := strings.TrimSpace(string(keyBytes))

	status := "off"
	if autotoolsConfig.srckeyRegistry != "" {
		r, err := srckeyregistry.New(autotoolsConfig.srckeyRegistry)
		if err != nil {
			return fmt.Errorf("element %q: open registry: %w", elem.Name, err)
		}
		// Use trace.log as the hit indicator. The registration
		// flow (post-build wrapper) writes the full set of
		// artifacts under a srckey atomically enough that a
		// trace.log entry being present implies the others
		// (make-db.txt, BUILD.bazel.out, install-mapping.json)
		// are too. If a future backend can't make that
		// guarantee, this check grows to AND across the full
		// set.
		hit, err := r.Has(srckey, "trace.log")
		if err != nil {
			return fmt.Errorf("element %q: registry lookup: %w", elem.Name, err)
		}
		if hit {
			status = "hit"
		} else {
			status = "miss"
		}
	}
	body := status + "\t" + srckey + "\n"
	return writeFile(filepath.Join(elemPkg, "srckey-cache-status.txt"), body)
}

// autotoolsSrckeyPatterns is the per-kind narrowing rule set
// for autotools' build-graph srckey. The returned rules
// classify each source-tree file as content-included (its
// bytes contribute to srckey) or name-only (only the path
// contributes).
//
// Content-included families:
//
//   - configure / configure.ac / configure.in / config.h.in —
//     autoconf entry points. Changing these changes the
//     generated Makefile.
//   - *.am / Makefile.in — automake / Makefile templates.
//     Direct source of truth for make's recipes.
//   - *.m4 — autoconf macro libraries fed into configure
//     processing.
//   - **/*.h / **/*.hpp / **/*.hxx — header files. Their
//     content rarely changes the build COMMANDS, but config.h
//     -style preprocessor switches CAN affect which compile
//     directives the Makefile emits (target-specific CFLAGS
//     keyed on HAVE_FOO macros). Kept conservatively.
//
// Everything else falls into name-only territory by default.
// Most importantly: **/*.c / *.cpp / *.cc / *.S / *.s. Their
// CONTENT doesn't influence the build graph — make's recipes
// stay the same; only the .o bytes the compiler emits change.
// Their NAMES still contribute (a wildcard rule in Makefile.in
// could pick up a newly-added .c, so adding/removing files
// must invalidate srckey).
//
// Pattern grammar matches the read-paths patterns
// (read_paths_patterns.go), since computeSrckey reuses the
// same matcher.
func autotoolsSrckeyPatterns() *readPathsPatterns {
	return &readPathsPatterns{
		Rules: []patternRule{
			{Include: true, Pattern: "configure"},
			{Include: true, Pattern: "configure.ac"},
			{Include: true, Pattern: "configure.in"},
			{Include: true, Pattern: "config.h.in"},
			{Include: true, Pattern: "**/*.ac"},
			{Include: true, Pattern: "**/*.am"},
			{Include: true, Pattern: "**/*.in"},
			{Include: true, Pattern: "**/*.m4"},
			{Include: true, Pattern: "**/*.h"},
			{Include: true, Pattern: "**/*.hpp"},
			{Include: true, Pattern: "**/*.hxx"},
		},
	}
}

func (autotoolsHandler) RenderB(elem *element, elemPkg string) error {
	return autotoolsBasePipelineHandler().RenderB(elem, elemPkg)
}

// autotoolsPipelineHandlerForElement builds the per-element
// pipelineHandler. Side effect: when the trace-driven native
// path is enabled AND the element has cross-element deps, an
// imports.json is rendered next to the BUILD that maps each
// dep's link library to its Bazel label (the convention bind
// from the kind:cmake handler — see writeAutotoolsImportsManifest).
// The extension's ExtraSrcs lists imports.json so the genrule
// stages it; AppendCmd's --imports-manifest flag references it
// via $(location imports.json).
//
// Without --convert-element-autotools / --build-tracer-bin, the
// returned handler has no extension — the unmodified coarse
// install_tree.tar pipeline renders.
func autotoolsPipelineHandlerForElement(elem *element, elemPkg string) (pipelineHandler, error) {
	h := autotoolsBasePipelineHandler()
	if autotoolsConfig.convertBin == "" {
		return h, nil
	}
	hasImports, err := writeAutotoolsImportsManifest(elem, elemPkg)
	if err != nil {
		return pipelineHandler{}, err
	}
	h.extension = autotoolsTraceExtension(elem, hasImports)
	return h, nil
}

// writeAutotoolsImportsManifest renders an imports.json next
// to the element's BUILD when there are cross-element deps to
// resolve. Convention bind: each dep "<name>" maps to
// link_libraries=["<name>"] → "//elements/<name>:<name>".
// Mirrors writeCmakeImportsManifest's shape, except the
// resolution key is the link-library name (matched against
// `-l<name>` flags by convert-element-autotools'
// LookupLinkLibrary) rather than the cmake target name.
//
// Returns (true, nil) when imports.json was written;
// (false, nil) when the element has no deps that need
// cross-element resolution (no file written).
func writeAutotoolsImportsManifest(elem *element, elemPkg string) (bool, error) {
	if len(elem.Deps) == 0 {
		return false, nil
	}
	var b strings.Builder
	b.WriteString(`{
  "version": 1,
  "elements": [
`)
	first := true
	for _, dep := range elem.Deps {
		if dep == nil {
			continue
		}
		if !first {
			b.WriteString(",\n")
		}
		first = false
		fmt.Fprintf(&b, `    {
      "name": %q,
      "exports": [
        {
          "cmake_target": %q,
          "bazel_label": "//elements/%s:%s",
          "link_libraries": [%q]
        }
      ]
    }`, dep.Name, dep.Name+"::"+dep.Name, dep.Name, dep.Name, dep.Name)
	}
	b.WriteString(`
  ]
}
`)
	if first {
		// All deps were nil — shouldn't happen but tolerated.
		return false, nil
	}
	if err := writeFile(filepath.Join(elemPkg, "imports.json"), b.String()); err != nil {
		return false, err
	}
	return true, nil
}

// autotoolsTraceExtension is the pipelineExtension that wires
// the build-tracer + convert-element-autotools steps into the
// rendered install-genrule cmd. Outputs: install_tree.tar
// (existing) + BUILD.bazel.out (converter output) + make-db.txt
// (post-build dump of `make -np`, fed back to the converter as
// a structural hint) + install-mapping.json (sidecar). Tools:
// build-tracer + convert-element-autotools (both staged into
// project A's tools/ at write-a time). When hasImports is true,
// imports.json is added to the genrule's srcs and the converter
// step's `--imports-manifest` flag references it via
// $(location imports.json).
//
// Note on the convert action's cache key: this lives in the
// SAME genrule action as the build, so any change that
// invalidates the install action also re-runs the converter.
// We attempted to split into _install + _converted (sibling
// genrules) to give the converter a narrower cache key, but
// reverted: trace + make-db are not byte-stable across builds
// (pid prefix, cc1 temp paths, make-db's `# Last modified`
// timestamps), so the narrow cache key never hit anyway. See
// docs/trace-driven-autotools.md for the determinism work
// that would let us re-introduce the split.
func autotoolsTraceExtension(elem *element, hasImports bool) *pipelineExtension {
	ext := &pipelineExtension{
		WrapPipelineCmds: wrapAutotoolsPipelineCmds,
		AppendCmd:        autotoolsConverterStep(hasImports),
		ExtraOuts: []string{
			"BUILD.bazel.out",
			"make-db.txt",
			"install-mapping.json",
		},
		ExtraTools: []string{
			"//tools:build-tracer",
			"//tools:convert-element-autotools",
		},
	}
	if hasImports {
		ext.ExtraSrcs = []string{"imports.json"}
	}
	// Wire dep install_tree.tar outputs into the consumer's
	// _install srcs so configure / make can find dep .h / .a.
	// Scoped to autotools-kind deps for now: pipeline kinds
	// install under the same /usr/{include,lib} convention,
	// so a single $DEP_PREFIX with CPPFLAGS / LDFLAGS overlay
	// is a clean, kind-uniform extraction. Other dep kinds
	// (kind:cmake, kind:manual) likely need similar wiring;
	// expand when those fixtures land.
	var depLabels []string
	for _, dep := range elem.Deps {
		if dep == nil || dep.Bst.Kind != "autotools" {
			continue
		}
		depLabels = append(depLabels, fmt.Sprintf("//elements/%s:install_tree.tar", dep.Name))
	}
	if len(depLabels) > 0 {
		ext.DepLabels = depLabels
		ext.DepExtractCmd = autotoolsDepExtractCmd()
	}
	return ext
}

// autotoolsDepExtractCmd is the shell snippet that stages
// upstream autotools deps' install trees. The pipeline cmd
// template's source-staging loop already skips
// `*/install_tree.tar` entries; this loop picks them up
// and untars each into a shared $DEP_PREFIX. CPPFLAGS /
// LDFLAGS prepend the conventional /usr layout (matches
// every fixture's `./configure --prefix=/usr`).
//
// The `${VAR:-}` fallback preserves any user-set values
// from the .bst's environment block.
func autotoolsDepExtractCmd() string {
	return `        # Stage upstream autotools deps' install trees under
        # a shared $$DEP_PREFIX. The for-src loop above skipped
        # */install_tree.tar entries; here we iterate $(SRCS)
        # again to pick them up. CPPFLAGS / LDFLAGS prepend the
        # /usr layout each dep installed with so the build's
        # configure / make can find the dep's .h / .a.
        DEP_PREFIX="$$(mktemp -d)"
        for src in $(SRCS); do
            case "$$src" in
                */install_tree.tar) tar -xf "$$src" -C "$$DEP_PREFIX" ;;
            esac
        done
        export CPPFLAGS="-I$$DEP_PREFIX/usr/include $${CPPFLAGS:-}"
        export LDFLAGS="-L$$DEP_PREFIX/usr/lib $${LDFLAGS:-}"`
}

// wrapAutotoolsPipelineCmds rewrites the resolved
// configure/build/install commands block. Every command runs
// under build-tracer so a single trace.log captures the entire
// process tree (compile / archive / link / install execve
// calls). The trace lives in $$AUTOTOOLS_TRACE; the converter
// step (AppendCmd) reads it.
//
// We use one tracer invocation around the whole pipeline —
// configure + build + install — rather than per-phase, so the
// process-tree filtering is straightforward (one strace
// session, one trace file).
//
// Path note: the tool reference is anchored to $$EXEC_ROOT.
// Bazel resolves $(location //tools:build-tracer) to an
// exec-root-relative path; pipelineHandler's prelude already
// `cd "$$BUILD_ROOT"` by the time this runs, so the bare
// relative path wouldn't find the staged binary.
func wrapAutotoolsPipelineCmds(cmds string) string {
	return fmt.Sprintf(`        # Build-tracer wraps the entire configure/build/install
        # pipeline. The trace artifact captures every execve under
        # the build sandbox; convert-element-autotools (run by the
        # AppendCmd step) reads it to emit BUILD.bazel.out.
        #
        # --normalize-prefix substitutions neutralize action-time
        # mktemp paths (INSTALL_ROOT, BUILD_ROOT, DEP_PREFIX). Their
        # bytes vary across bazel invocations even when the build
        # is otherwise identical, so without normalization the
        # canonical trace would still drift run-to-run. The
        # placeholder names (/INSTALL_ROOT, etc.) are stable
        # across machines and human-readable. DEP_PREFIX is only
        # set when the element has autotools deps — using
        # ${DEP_PREFIX:-} keeps the flag harmless when unset
        # (substitutes empty-string, which trivially matches
        # nothing).
        export AUTOTOOLS_TRACE="$$(mktemp)"
        "$$EXEC_ROOT/$(location //tools:build-tracer)" \
            --normalize-prefix="$$INSTALL_ROOT=/INSTALL_ROOT" \
            --normalize-prefix="$$BUILD_ROOT=/BUILD_ROOT" \
            --normalize-prefix="$${DEP_PREFIX:-/__unset_dep_prefix__}=/DEP_PREFIX" \
            --out="$$AUTOTOOLS_TRACE" -- sh -c '
%s
'`, cmds)
}

// autotoolsConverterStep is the shell snippet inserted between
// the (tracer-wrapped) pipeline cmds and the install-tree tar.
// Two sub-steps:
//
//  1. Dump `make -np` (dry-run + print-database) to capture
//     the post-build Makefile state — fully variable-resolved
//     after configure ran. Cwd is $$BUILD_ROOT here (the
//     pipeline's `cd "$$BUILD_ROOT"` is still in effect), so
//     make finds its Makefile.
//  2. Run convert-element-autotools against the trace + the
//     captured make database, emitting BUILD.bazel.out.
//
// `make -np` may exit non-zero on a healthy build (it skips
// the actual build but still attempts `nothing to do` — safe
// to ignore). The `|| true` keeps the genrule action
// successful even if make is unhappy with the dry run.
//
// When hasImports is true, --imports-manifest=$(location
// imports.json) threads through so cross-element `-l<name>`
// flags resolve to the right Bazel labels.
func autotoolsConverterStep(hasImports bool) string {
	importsFlag := ""
	if hasImports {
		importsFlag = ` \
            --imports-manifest="$(location imports.json)"`
	}
	return fmt.Sprintf(`        # Capture the post-build make database. Run from
        # $$BUILD_ROOT (pipeline cmds left us there); `+"`make -np`"+`
        # dumps every rule, variable, and prereq edge after
        # configure-time substitutions are baked in. Tolerate
        # non-zero exit (make's dry-run can grumble about
        # "nothing to do" or missing optional targets).
        #
        # Two-stage filter:
        #
        #   1. Drop diagnostic lines that vary across runs of an
        #      otherwise-identical build:
        #        - "#  Last modified <timestamp>" — file mtime
        #          drift even when content is unchanged.
        #        - "# (device X, inode Y): N files, ..." and
        #          "# N files, M impossibilities in D directories"
        #          — vary with filesystem state (.deps files).
        #      These are diagnostic-only metadata; rule + variable
        #      data the converter consumes lives in other lines.
        #   2. Substitute action-time mktemp paths
        #      ($$INSTALL_ROOT, $$BUILD_ROOT, $$DEP_PREFIX) with
        #      stable placeholders. Same set of substitutions
        #      build-tracer's --normalize-prefix applies to the
        #      trace; here the values appear in make's PWD /
        #      CURDIR / DESTDIR variable dumps. The empty-string
        #      fallback (bash ${VAR:-default} form) keeps the sed
        #      script harmless when the variable isn't set.
        ( make -np 2>/dev/null || true ) \
            | sed -E '/^#[[:space:]]+Last modified /d; /\(device [0-9]+, inode [0-9]+\): [0-9]+ files,/d; /^# [0-9]+ files,.*impossibilities in /d; /^# Make data base, printed on /d; /^# Finished Make data base on /d' \
            | sed -e "s|$$INSTALL_ROOT|/INSTALL_ROOT|g" \
                  -e "s|$$BUILD_ROOT|/BUILD_ROOT|g" \
                  -e "s|$${DEP_PREFIX:-/__unset_dep_prefix__}|/DEP_PREFIX|g" \
            > "$$EXEC_ROOT/$(location make-db.txt)"

        # Trace + make-db -> native cc_library / cc_binary
        # BUILD.bazel.out. Output goes through bazel's normal
        # action cache (buildbarn in CI), which is what gives us
        # cross-node convergence — same trace + same converter
        # version => same BUILD.bazel.out everywhere.
        cd "$$EXEC_ROOT"
        $(location //tools:convert-element-autotools) \
            --trace="$$AUTOTOOLS_TRACE" \
            --make-db="$(location make-db.txt)" \
            --out-install-mapping="$(location install-mapping.json)" \
            --out-build="$(location BUILD.bazel.out)"%s`, importsFlag)
}
