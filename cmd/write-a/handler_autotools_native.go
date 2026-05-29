package main

import (
	"fmt"
	"path/filepath"
	"strings"
)

// init registers kind:autotools. The handler always falls back
// to the coarse install-pipeline shape; when --convert-element-trace
// is supplied, it additionally wraps the build cmd in build-tracer
// + runs convert-element-trace to emit a native BUILD.bazel.out
// alongside the install-root TreeArtifact.
//
// One rule with two outputs (the install-root TreeArtifact +
// BUILD.bazel.out). Bazel's action cache (buildbarn in CI)
// handles convergence — same source + same toolchain + same
// converter version → same action result, shared across nodes
// via the existing remote-cache plumbing. No separate registry
// needed; the "B → A feedback" lives entirely inside the
// Bazel-action graph.
func init() {
	registerHandler(autotoolsHandler{})
}

// traceConfig holds the render-time settings shared by every
// kind that opts into the trace-driven path (autotools / make /
// manual / script / makemaker / modulebuild) plus the
// kind:cmake round-2 fallback's tool-staging needs (build-tracer
// + trace-publish + trace-lookup land in tools/ regardless of
// which kind activated round-2). Populated from main()'s flags
// before the per-element render loop runs. Empty convertBin
// disables the trace+convert wrap for the trace-driven kinds
// entirely (rendered output is the unmodified pipeline shape);
// the cmake-round2-fallback tool staging is gated by
// cmakeConfig.round2FallbackEnabled instead, so it can activate
// with convertBin empty.
//
// The struct lives in handler_autotools_native.go for historical
// reasons (kind:autotools was the first opt-in) but is no longer
// autotools-specific. Package-level state keeps the kindHandler
// interface small (RenderA / RenderB don't take a config arg)
// while letting each kind's handler consult the same flags.
var traceConfig struct {
	convertBin    string // absolute path to convert-element-trace
	tracerBin     string // absolute path to build-tracer
	publishBin    string // absolute path to trace-publish (round 2 publisher)
	lookupBin     string // absolute path to trace-lookup (round 2 consumer; staged so CI can find it on PATH)
	foldBin       string // absolute path to fold-element (multi-platform fold of N per-platform ir.Package JSONs). Empty when --platforms-json isn't set.
	round2Enabled bool   // round-2 active: pivot project A from marker → converter genrule, project B from inline-converter → inline-trace-publish. Set when --convert-element-trace is supplied AND the operator did NOT pass --trace-round1; cleared when only round-1 is wanted.
	// platforms, when non-empty, switches the trace-driven round-2
	// render to per-platform fan-out on both project sides:
	// project A emits N converter genrules per element (one per
	// (element, platform) cell) plus one fold-element genrule
	// composing their ir.json outputs into a unified BUILD.bazel;
	// project B emits N install genrules per element via
	// renderPipelineRound2B's multi-platform branch, each
	// baking --platform=<name> into the trace-publish step and
	// carrying exec_compatible_with constraints + an
	// install-root select() arm. The per-element BUILD also
	// gets one trace_load target per platform so the per-platform
	// AC lookups partition correctly. The fan-out covers
	// every cc-emitting trace-driven kind today
	// (pipelineHandler kinds, kind:autotools via the round-2
	// RenderB dispatch, kind:cmake Phase B fallback via
	// renderCmakeRound2B). Empty (the default) preserves the
	// single-platform shape — byte-stable goldens, single
	// converter genrule per element, single install genrule per
	// element.
	platforms []tracePlatform

	// traceSourceRoot, when true, threads --source-root=$$BUILD_ROOT
	// into wrapAutotoolsPipelineCmds's build-tracer invocation so
	// openat events get captured into the recovered trace. The
	// wrapper is used by both round-1 (single-genrule shape) and
	// round-2 (separate install genrule) trace-driven pipelines,
	// so the flag's effect spans both — flipping it shifts the
	// trace bytes for every trace-driven element this build
	// renders. Without it, build-tracer drops openat events
	// entirely (the legacy AC byte schema for trace-driven kinds,
	// kept stable for existing AC entries). Opting in is the
	// precondition for the narrowing-undercoverage audit's trace
	// oracle to fire; flipping the flag invalidates the AC
	// entries for the trace-driven elements this build touched
	// (one-shot rebake). Today the flag is global rather than
	// per-element — CI and e2e fixtures opt in unconditionally;
	// production deployments opt in once they've absorbed the
	// AC churn.
	traceSourceRoot bool
}

// autotoolsHandler picks the right pipelineHandler shape based
// on the global traceConfig. Without a converter binary,
// the coarse install-root pipeline is the rendered shape;
// with it, the pipelineExtension wraps the cmd in build-tracer
// and runs convert-element-trace after the install phase.
type autotoolsHandler struct{}

func (autotoolsHandler) Kind() string                                 { return "autotools" }
func (autotoolsHandler) NeedsSources() bool                           { return true }
func (autotoolsHandler) HasProjectABuild() bool                       { return true }
func (autotoolsHandler) DefaultReadPathsPatterns() *readPathsPatterns { return nil }

// RenderA writes project A's contribution for a kind:autotools
// element. Behavior splits on whether the trace-driven path is
// enabled:
//
//   - Trace-driven (traceConfig.convertBin set): the install
//     rule lives in PROJECT B (RenderB below), where deps are
//     materialized as Bazel cc_library / install-root TreeArtifact
//     targets. Project A only carries a marker BUILD plus the
//     srckey debug artifacts the registry-driven round-2 lookup
//     consults. See docs/architecture.md for the
//     1 → 2 → 3 → 2′ → 3′ loop.
//   - Coarse (no --convert-element-trace): existing
//     pipeline-shape rendering — install genrule in A, B is a
//     placeholder. Preserved verbatim so kinds that haven't
//     opted into the trace-driven path don't change.
func (autotoolsHandler) RenderA(elem *element, elemPkg string) error {
	if traceConfig.convertBin == "" {
		// Coarse path: fall back to the historical
		// pipelineHandler shape (install genrule in A).
		h, err := autotoolsPipelineHandlerForElement(elem, elemPkg)
		if err != nil {
			return err
		}
		return h.RenderA(elem, elemPkg)
	}
	if traceConfig.round2Enabled {
		// Round 2: project A hosts the per-element converter
		// genrule. Reads @trace_<elem>//:trace from the AC at
		// load time; emits BUILD.bazel.out (cc_library /
		// cc_binary on AC hit; placeholder on miss).
		return renderTraceDrivenRound2A(elem, elemPkg, "autotools", autotoolsSrckeyPatterns())
	}
	// Round 1: A-side is just a marker. The install genrule +
	// source tree live in project B (where the converter runs
	// inline as a sibling output of the install action).
	return writeFile(filepath.Join(elemPkg, "BUILD.bazel"),
		"# Generated by cmd/write-a. Do not edit by hand.\n"+
			"# kind:autotools (trace-driven) — install genrule lives\n"+
			"# in project B's elements/"+elem.Name+"/BUILD.bazel.\n"+
			"filegroup(name = \"BUILD_IN_PROJECT_B\", srcs = [])\n",
	)
}

// RenderB writes project B's contribution. Trace-driven
// elements get the full install genrule (sources + the build-
// tracer + convert-element-trace wrapper) here, where
// dep elements' Bazel targets are addressable. Coarse-path
// elements get the historical placeholder.
func (autotoolsHandler) RenderB(elem *element, elemPkg string) error {
	if traceConfig.convertBin == "" {
		return autotoolsBasePipelineHandler().RenderB(elem, elemPkg)
	}
	// Trace-driven path: stage sources + render the install
	// genrule into B. pipelineHandler.RenderA does both, and
	// it's parameterized over elemPkg, so calling it with
	// project-B's per-element package directory does the
	// right thing — //tools:build-tracer +
	// //tools:convert-element-trace resolve in B because
	// stageAutotoolsTools already staged them at writeProjectB
	// time (PR #67).
	//
	// Round-2 also gets per-platform fan-out via
	// renderPipelineRound2B when --platforms-json is set: same
	// single-platform shape as h.RenderA when the matrix is
	// unset, plus the N per-platform install genrules + top-
	// level select()-filegroup when it's populated. Round-1
	// keeps the existing h.RenderA path because the round-1
	// extension (autotoolsTraceExtension) wraps the converter
	// inline alongside the install action — a different
	// genrule shape from the round-2 trace-publish wrapper that
	// renderPipelineRound2B's single-platform branch
	// constructs, so calling renderPipelineRound2B in round-1
	// would overwrite the round-1 extension and lose the inline
	// converter step.
	h, err := autotoolsPipelineHandlerForElement(elem, elemPkg)
	if err != nil {
		return err
	}
	if traceConfig.round2Enabled {
		if err := h.renderPipelineRound2B(elem, elemPkg); err != nil {
			return err
		}
	} else {
		if err := h.RenderA(elem, elemPkg); err != nil {
			return err
		}
	}
	// Emit srckey.txt + srckey-breakdown.txt — the per-element
	// build-graph identity used by the trace-driven registry
	// (see srckey.go). Only emitted when the trace-driven path
	// is enabled (matches the `convertBin` set guard the
	// pipelineHandlerForElement applied above), since coarse
	// pipeline elements don't participate in the registry.
	if traceConfig.convertBin != "" {
		if err := renderSrckey(elem, elemPkg, autotoolsSrckeyPatterns()); err != nil {
			return err
		}
	}
	return nil
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
// Without --convert-element-trace / --build-tracer-bin, the
// returned handler has no extension — the unmodified coarse
// install-root pipeline renders.
func autotoolsPipelineHandlerForElement(elem *element, elemPkg string) (pipelineHandler, error) {
	h := autotoolsBasePipelineHandler()
	if traceConfig.convertBin == "" {
		return h, nil
	}
	if traceConfig.round2Enabled {
		// Round 2: converter runs in project A, not B. imports.json
		// is rendered in A only (by renderTraceDrivenRound2A);
		// don't write a B-side copy that no action references —
		// it would just invalidate the install genrule's cache
		// key on every imports.json change with no behavioral
		// effect.
		// Pass an empty tracePlatform{} so
		// pipelineTraceExtensionRound2 emits the byte-stable
		// legacy shape with no OutputPrefix / NameSuffix /
		// ExecCompatibleWith. The multi-platform fan-out for
		// kind:autotools is driven separately by RenderB
		// (which dispatches to renderPipelineRound2B and walks
		// traceConfig.platforms to emit N install genrules);
		// this extension-construction site stays single-
		// platform because pipelineTraceExtensionRound2's
		// extension threads into the *converter* genrule
		// (project-A side), and project-A fan-out is the
		// orchestrator's job via --platforms-json, not write-a's.
		h.extension = pipelineTraceExtensionRound2(elem, []string{"autotools"}, tracePlatform{})
		return h, nil
	}
	// Round 1: converter runs inline in B's install genrule, so
	// imports.json must be in B's elemPkg + the genrule's srcs.
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
// `-l<name>` flags by convert-element-trace'
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
// the build-tracer + convert-element-trace steps into the
// rendered install-rule cmd. Outputs: the install-root
// TreeArtifact + BUILD.bazel.out (converter output) + make-db.txt
// (post-build dump of `make -np`, fed back to the converter as
// a structural hint) + install-mapping.json (sidecar). Tools:
// build-tracer + convert-element-trace (both staged into
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
// docs/architecture.md for the determinism work
// that would let us re-introduce the split.
func autotoolsTraceExtension(elem *element, hasImports bool) *pipelineExtension {
	ext := &pipelineExtension{
		// autotoolsTraceExtension is the round-1 path: the
		// converter runs inline in project B's install genrule
		// and there's exactly one such genrule per element
		// regardless of --platforms-json. The wrapper's
		// outputPrefix is always "" because there's only one
		// declared install-root TreeArtifact output for the
		// generated-headers.txt $(location ...) reference to
		// resolve against. Round-2's per-platform install
		// fan-out is the separate renderPipelineRound2B call
		// site in RenderB above.
		// ExtraTools order fixes the @@TOOL:N@@ indices the command
		// builders reference: build-tracer is tool 0,
		// convert-element-trace is tool 1.
		WrapPipelineCmds: func(cmds string) string { return wrapAutotoolsPipelineCmds(cmds, "", 0) },
		AppendCmd:        autotoolsConverterStep(hasImports, elem.Name, 1),
		ExtraOuts: []string{
			"BUILD.bazel.out",
			"make-db.txt",
			"install-mapping.json",
			"generated-headers.txt",
		},
		ExtraTools: []string{
			"//tools:build-tracer",
			"//tools:convert-element-trace",
		},
	}
	if hasImports {
		ext.ExtraSrcs = []string{"imports.json"}
	}
	// Wire dep install-root TreeArtifacts into the consumer's
	// pipeline_install deps so configure / make can find dep
	// .h / .a in place (no untar; @@DEP_INSTALL_DIRS@@ overlay).
	// Scoped to autotools-kind deps for now: pipeline kinds
	// install under the same /usr/{include,lib} convention,
	// so a single CPPFLAGS / LDFLAGS overlay per dep dir is a
	// clean, kind-uniform wiring. Other dep kinds (kind:cmake,
	// kind:manual) likely need similar wiring; expand when
	// those fixtures land.
	var depLabels []string
	for _, dep := range elem.Deps {
		if dep == nil || dep.Bst.Kind != "autotools" {
			continue
		}
		depLabels = append(depLabels, fmt.Sprintf("//elements/%s:%s_install", dep.Name, dep.Name))
	}
	if len(depLabels) > 0 {
		ext.DepLabels = depLabels
		ext.DepExtractCmd = autotoolsDepExtractCmd()
	}
	return ext
}

// autotoolsDepExtractCmd is the shell snippet that wires upstream
// autotools deps' install-root TreeArtifacts into the build's
// compile flags. The deps ride the pipeline_install rule's `deps`
// attr; @@DEP_INSTALL_DIRS@@ expands to the space-separated
// exec-root-relative install-root DIRECTORIES. Each is referenced
// IN PLACE — no untar, no per-consumer $DEP_PREFIX copy. CPPFLAGS /
// LDFLAGS prepend the conventional /usr layout (matches every
// fixture's `./configure --prefix=/usr`), accumulating one -I/-L
// pair per dep.
//
// The `${VAR:-}` fallback preserves any user-set values from the
// .bst's environment block. The DEP_PREFIX placeholder (a synthetic
// pipe-joined list of the in-place dirs) is still exported so the
// trace-normalization sed (build-tracer --normalize-prefix and the
// make-db filter) can neutralize the action-time dep paths.
func autotoolsDepExtractCmd() string {
	return `        # Wire upstream autotools deps' install-root TreeArtifact
        # directories into CPPFLAGS / LDFLAGS. @@DEP_INSTALL_DIRS@@
        # expands to the space-separated install-root dirs (the
        # rule's deps attr, consumed in place — no untar). One -I/-L
        # pair per dep so the build's configure / make can find each
        # dep's .h / .a under the /usr layout it installed with.
        DEP_PREFIX=""
        for d in @@DEP_INSTALL_DIRS@@; do
            ad="$$EXEC_ROOT/$$d"
            export CPPFLAGS="-I$$ad/usr/include $${CPPFLAGS:-}"
            export LDFLAGS="-L$$ad/usr/lib $${LDFLAGS:-}"
            if [ -z "$$DEP_PREFIX" ]; then DEP_PREFIX="$$ad"; else DEP_PREFIX="$$DEP_PREFIX|$$ad"; fi
        done`
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
//
// outputPrefix is the same OutputPrefix the surrounding
// pipelineExtension applies to declared outputs — empty for
// the legacy single-platform shape, "<platform>" for the
// project-B per-platform install fan-out. The
// generated-headers.txt diff is written to that prefix so
// $(location <prefix>/generated-headers.txt) resolves to the
// genrule's actual declared output path.
//
// toolIdx is the positional index of //tools:build-tracer in the
// pipeline_install rule's `tools` attr (write-a controls the order
// via the extension's ExtraTools). The command references the tracer
// binary via the @@TOOL:<toolIdx>@@ sentinel the rule substitutes
// with the binary's exec-root path.
func wrapAutotoolsPipelineCmds(cmds, outputPrefix string, toolIdx int) string {
	generatedHeaders := "@@OUT:generated-headers.txt@@"
	if outputPrefix != "" {
		generatedHeaders = "@@OUT:" + outputPrefix + "/generated-headers.txt@@"
	}
	// --source-root opts the tracer into capturing openat events
	// (filtered to the source tree). Required for the narrowing-
	// undercoverage audit's trace oracle to fire; without it,
	// openat events are dropped and only the cmake oracle carries
	// signal for trace-driven kinds. Opting in here changes the
	// trace bytes → changes the published AC entry; controlled
	// per-build via the --trace-source-root write-a flag so
	// existing AC entries stay valid until the operator chooses
	// to rebake.
	sourceRootFlag := ""
	if traceConfig.traceSourceRoot {
		sourceRootFlag = ` \
            --source-root="$$BUILD_ROOT"`
	}
	return fmt.Sprintf(`        # Snapshot the build dir's existing .h files BEFORE
        # configure runs. After the tracer-wrapped pipeline
        # completes, a second snapshot diffs against this one to
        # recover the set of build-time-generated headers
        # (AC_CONFIG_HEADERS-style config.h, yacc/bison parser
        # headers, etc.). The diff feeds convert-element-trace'
        # --generated-headers flag so cc_library rules in
        # BUILD.bazel.out document the dependency.
        PRE_HEADERS_LIST="$$(mktemp)"
        ( cd "$$BUILD_ROOT" && find . -type f -name '*.h' | sort > "$$PRE_HEADERS_LIST" )

        # Build-tracer wraps the entire configure/build/install
        # pipeline. The trace artifact captures every execve under
        # the build sandbox; convert-element-trace (run by the
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
        # Build one --normalize-prefix flag per upstream dep install
        # root (the pipe-joined DEP_PREFIX set by the dep-extract
        # snippet) so each dep's action-time bazel-out path is
        # neutralized to a stable /DEP_PREFIX_N placeholder in the
        # canonical trace. Empty DEP_PREFIX (no deps) yields no extra
        # flags.
        DEP_NORMALIZE=""
        if [ -n "$${DEP_PREFIX:-}" ]; then
            _i=0
            _ifs="$$IFS"; IFS='|'
            for _d in $$DEP_PREFIX; do
                DEP_NORMALIZE="$$DEP_NORMALIZE --normalize-prefix=$$_d=/DEP_PREFIX_$$_i"
                _i=$$((_i+1))
            done
            IFS="$$_ifs"
        fi
        "@@TOOL:%[4]d@@" \
            --normalize-prefix="$$INSTALL_ROOT=/INSTALL_ROOT" \
            --normalize-prefix="$$BUILD_ROOT=/BUILD_ROOT" \
            $$DEP_NORMALIZE%[1]s \
            --out="$$AUTOTOOLS_TRACE" -- sh -c '
%[2]s
'

        # Post-pipeline header snapshot. comm -13 = lines in the
        # post-list not in the pre-list = generated by configure
        # / make. Output is build-tree-relative (./config.h etc.);
        # the converter's parseGeneratedHeaderList strips the
        # leading "./" before adding to BUILD.bazel.out.
        POST_HEADERS_LIST="$$(mktemp)"
        ( cd "$$BUILD_ROOT" && find . -type f -name '*.h' | sort > "$$POST_HEADERS_LIST" )
        comm -13 "$$PRE_HEADERS_LIST" "$$POST_HEADERS_LIST" > "$$EXEC_ROOT/%[3]s"
`, sourceRootFlag, cmds, generatedHeaders, toolIdx)
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
//  2. Run convert-element-trace against the trace + the
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
//
// convertToolIdx is the positional index of
// //tools:convert-element-trace in the pipeline_install rule's
// `tools` attr. The command invokes it via @@TOOL:<convertToolIdx>@@.
func autotoolsConverterStep(hasImports bool, elementName string, convertToolIdx int) string {
	importsFlag := ""
	if hasImports {
		importsFlag = ` \
            --imports-manifest="$$IMPORTS_JSON"`
	}
	bazelPkgFlag := fmt.Sprintf(` \
            --bazel-package-path="elements/%s"`, elementName)
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
        # Build per-dep sed substitutions for the pipe-joined
        # DEP_PREFIX so each dep install root in make's variable
        # dumps maps to a stable /DEP_PREFIX_N placeholder.
        DEP_SED=""
        if [ -n "$${DEP_PREFIX:-}" ]; then
            _i=0
            _ifs="$$IFS"; IFS='|'
            for _d in $$DEP_PREFIX; do
                DEP_SED="$$DEP_SED -e s|$$_d|/DEP_PREFIX_$$_i|g"
                _i=$$((_i+1))
            done
            IFS="$$_ifs"
        fi
        ( make -np 2>/dev/null || true ) \
            | sed -E '/^#[[:space:]]+Last modified /d; /\(device [0-9]+, inode [0-9]+\): [0-9]+ files,/d; /^# [0-9]+ files,.*impossibilities in /d; /^# Make data base, printed on /d; /^# Finished Make data base on /d' \
            | sed -e "s|$$INSTALL_ROOT|/INSTALL_ROOT|g" \
                  -e "s|$$BUILD_ROOT|/BUILD_ROOT|g" \
                  $$DEP_SED \
            > "$$EXEC_ROOT/@@OUT:make-db.txt@@"

        # Trace + make-db -> native cc_library / cc_binary
        # BUILD.bazel.out. Output goes through bazel's normal
        # action cache (buildbarn in CI), which is what gives us
        # cross-node convergence — same trace + same converter
        # version => same BUILD.bazel.out everywhere.
        cd "$$EXEC_ROOT"
        "@@TOOL:%[3]d@@" \
            --trace="$$AUTOTOOLS_TRACE" \
            --make-db="@@OUT:make-db.txt@@" \
            --generated-headers="@@OUT:generated-headers.txt@@" \
            --out-install-mapping="@@OUT:install-mapping.json@@" \
            --out-build="@@OUT:BUILD.bazel.out@@"%[1]s%[2]s`, importsFlag, bazelPkgFlag, convertToolIdx)
}
