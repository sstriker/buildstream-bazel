// Command write-a is the production writer-of-A for the meta-project
// (Bazel-as-orchestrator) shape described in docs/whole-project-plan.md.
// It parses .bst element files, resolves their sources and dependencies,
// and renders project A (the meta workspace whose genrules invoke
// per-kind translator binaries) and project B (the consumer workspace
// built against project A's outputs).
//
// Phase 1 — kind:cmake only, single-element fixtures (hello-world.bst).
// Phase 2 (this file) — multi-element graphs + per-kind dispatch +
// kind:stack. Subsequent phases extend the kind set (kind:manual
// coarse-grained pipeline, then meson, autotools, ...) and the
// source-kind set (git, tar, remote-asset).
//
// Per-kind dispatch is mediated by the kindHandler interface (see
// kindHandler below); each kind's renderer takes the graph + a single
// element and contributes a per-element package to project A and/or
// project B as appropriate. Kinds that don't need an action-graph step
// (stack, filter, import, …) only contribute project-B starlark; the
// driver script's stage step is a no-op for them.
//
// Shadow-tree narrowing (kind:cmake):
//   - Default (no <element>.read-paths.txt sibling): every source
//     file is staged real. Conservative; matches pre-narrowing
//     behaviour.
//   - With <element>.read-paths.txt: include / exclude glob
//     patterns partition the source tree. Matched files stage
//     real; the rest stage as zero stubs (empty content via the
//     zero_files starlark rule). cmake's directory walks see the
//     entries; reads against zero stubs hit empty files. The
//     action input merkle is content-stable across edits to
//     non-included source files.
//
// CMakeLists.txt files always stay real regardless of the
// patterns — cmake parses the entry CMakeLists before any trace
// event could fire, so auto-including them keeps cmake configure
// correct.
package main

import (
	_ "embed"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// zero_files.bzl is embedded into the binary so the writer doesn't
// depend on its caller's working directory. A future iteration may
// expose the rule via a published bazel module so consumers can
// `bazel_dep` it directly; for now embedding keeps the deployment
// shape one-binary-and-go.
//
//go:embed assets/zero_files.bzl
var zeroFilesBzl string

// bstFile is the YAML shape we parse out of a .bst element file.
// We only read the fields write-a's per-kind dispatch and source
// resolution need; other fields BuildStream understands (e.g.
// `variables:`) are ignored for now and will get plumbed in by the
// per-kind handlers that need them.
type bstFile struct {
	Kind    string      `yaml:"kind"`
	Sources []bstSource `yaml:"sources"`
	// Depends / BuildDepends / RuntimeDepends are the three
	// dependency categories BuildStream defines. `depends` covers
	// both build- and run-time; `build-depends` is build-only;
	// `runtime-depends` is runtime-only. write-a (v1) merges all
	// three into a single dep edge set in element.Deps — the build-
	// vs-runtime distinction matters once the typed-filegroup
	// wrapper for pipeline-kind outputs lets consumers reference
	// runtime-only labels separately, which lands later.
	Depends        []bstDep `yaml:"depends"`
	BuildDepends   []bstDep `yaml:"build-depends"`
	RuntimeDepends []bstDep `yaml:"runtime-depends"`
	// Config is the per-kind freeform configuration block. Each
	// handler picks the keys it cares about (kind:manual reads
	// build-commands / install-commands / etc.; kind:cmake currently
	// uses none). yaml.v3 represents arbitrary YAML as a Node tree;
	// using a Node here lets handlers re-extract specific shapes
	// without forcing every kind to share one struct.
	Config yaml.Node `yaml:"config"`
	// Variables is the per-element BuildStream variable scope. Layered
	// on top of project defaults and the per-kind defaults declared
	// by the handler; consumed via resolveVars in variables.go. Each
	// pipeline-kind handler runs phase commands through
	// substituteCmd against the resolved map.
	Variables map[string]string `yaml:"variables"`
	// Environment is the per-element env-var map, layered on top of
	// project.conf's project-level environment. Variable references
	// (%{...}) resolve against the same scope as phase commands.
	// Pipeline handlers emit `env = {...}` on the genrule attribute.
	Environment map[string]string `yaml:"environment"`
	// Conditionals are the per-arch (?): branches extracted from
	// `variables:` before the YAML decode pass (yaml.v3 can't
	// directly unmarshal the (?): shape into a string-map). Empty
	// for elements with no (?): block. Pipeline handlers consume
	// these to lower per-arch overrides into project-B select()
	// over @platforms//cpu:* rather than baking write-a's host arch
	// into the rendered cmd. See conditional.go.
	Conditionals []conditionalBranch `yaml:"-"`
	// ConfigConditionals are the per-arch (?): branches extracted
	// from `config:` (the FDSDK bootstrap pattern: per-arch
	// configure-commands overrides on the same .bst). Empty when
	// no config: (?): block is present. The pipeline handler
	// merges matching branches' partial pipelineCfg overrides
	// into the per-tuple resolved cfg in resolveAt.
	ConfigConditionals []conditionalBranch `yaml:"-"`
	// Public is the BuildStream public-data block: per-element
	// downstream metadata (split-rules, environment overrides, ...).
	// 33 % of FDSDK elements declare it. For v1 we decode it as a
	// yaml.Node so the file parses but don't act on its contents —
	// kind:filter's domain enforcement (which consumes
	// public.bst.split-rules) is a follow-up.
	Public yaml.Node `yaml:"public"`
}

// bstDep is one entry inside a depends / build-depends / runtime-
// depends list. Real .bst files declare deps in three shapes:
//
//   - String:        "- foo.bst"
//   - Map (single):  "- filename: foo.bst, junction: jx.bst, config: {...}"
//   - Map (list):    "- filename: [foo.bst, bar.bst], config: {...}"
//
// The map shapes carry junction-targeting and per-dep config (e.g.
// kind:filter overriding parent's domain choice). The list-of-
// filenames form (FDSDK uses it heavily for "depend on each of
// these with the same config:" patterns) expands into N dep edges
// at graph-load time. For v1 we only consume Filename / Filenames
// — junction and config get parsed and recorded but aren't yet
// acted on.
type bstDep struct {
	// Filename holds the single-string and single-filename map
	// shapes; Filenames holds the list-form map shape. Mutually
	// exclusive — exactly one is populated per parsed entry.
	Filename  string
	Filenames []string
	Junction  string
	Config    yaml.Node
}

// UnmarshalYAML accepts either a scalar (string-form dep) or a
// mapping (map-form dep, single or list filename). yaml.v3 picks
// per-entry shape via the Node's Kind, so a single list can mix
// shapes.
func (d *bstDep) UnmarshalYAML(node *yaml.Node) error {
	switch node.Kind {
	case yaml.ScalarNode:
		d.Filename = node.Value
		return nil
	case yaml.MappingNode:
		for i := 0; i < len(node.Content); i += 2 {
			k := node.Content[i].Value
			v := node.Content[i+1]
			switch k {
			case "filename":
				switch v.Kind {
				case yaml.ScalarNode:
					d.Filename = v.Value
				case yaml.SequenceNode:
					for _, c := range v.Content {
						if c.Kind != yaml.ScalarNode {
							return fmt.Errorf("dep: filename list entry must be a string, got node kind %d", c.Kind)
						}
						d.Filenames = append(d.Filenames, c.Value)
					}
				default:
					return fmt.Errorf("dep: filename must be a string or list of strings, got node kind %d", v.Kind)
				}
			case "junction":
				if v.Kind == yaml.ScalarNode {
					d.Junction = v.Value
				}
			case "config":
				d.Config = *v
			}
		}
		if d.Filename == "" && len(d.Filenames) == 0 {
			return fmt.Errorf("dep: map-form entry must have a `filename:` key")
		}
		return nil
	default:
		return fmt.Errorf("dep: expected scalar or mapping, got yaml node kind %d", node.Kind)
	}
}

// expandedFilenames returns every dep filename declared on this
// entry — either a single Filename or every Filenames entry. The
// graph-resolution loop iterates the result so a list-form dep
// expands to N edges.
func (d *bstDep) expandedFilenames() []string {
	if len(d.Filenames) > 0 {
		return d.Filenames
	}
	if d.Filename != "" {
		return []string{d.Filename}
	}
	return nil
}

type bstSource struct {
	Kind string `yaml:"kind"`
	Path string `yaml:"path"`
	// Directory is the optional staging subpath inside the element's
	// source tree (BuildStream defaults to ""). When set, this
	// source's content lands under <element-pkg>/<directory>/ rather
	// than at the package root. 64 of FDSDK's elements use it (most
	// commonly to keep separately-fetched component sources from
	// colliding with the primary source tree).
	Directory string `yaml:"directory"`
	// URL / Ref / Track are non-kind:local source metadata (kind:git_repo
	// / kind:tar / kind:remote / kind:patch_queue / etc.). For v1
	// write-a parses and records them on resolvedSource so the
	// element's bstFile + Sources fully describe what was declared,
	// but doesn't fetch — actual checkout is deferred to a later
	// integration with the existing orchestrator/sourcecheckout
	// layer. Unknown source kinds get the same record-and-skip
	// treatment so write-a's render pass succeeds against full FDSDK
	// content even where bazel-build wouldn't (yet) compile.
	//
	// Ref is yaml.Node rather than string because language-package
	// source kinds (kind:cargo2 / kind:go_module / kind:pypi /
	// kind:cpan) declare ref as a vendored list of registry
	// entries, not a single ref string. v1 records the raw node
	// untyped — we don't act on these yet at staging time, so the
	// shape doesn't matter beyond "yaml.v3 unmarshal succeeds".
	URL   string    `yaml:"url"`
	Ref   yaml.Node `yaml:"ref"`
	Track string    `yaml:"track"`
}

// resolvedSource is one entry in element.Sources: a per-source
// record with everything write-a's render layer needs. Kind:local
// sources carry the resolved AbsPath; non-kind:local sources carry
// their URL/Ref metadata (parsed for completeness, ignored at
// staging time pending real source-fetch integration).
type resolvedSource struct {
	Kind      string
	AbsPath   string // populated only for kind:local
	Directory string
	URL       string
	// Ref is the raw ref node (string for git/tar; list-of-mapping
	// for language-package source kinds). v1 doesn't act on it.
	Ref   yaml.Node
	Track string
}

type element struct {
	Name string // derived from .bst filename (basename without .bst suffix)
	Bst  *bstFile
	// Sources is the resolved source list for this element — one
	// entry per kind:local source declared in the .bst, with each
	// AbsPath pre-resolved against the .bst's directory. Empty for
	// kinds that don't resolve their own source tree (kind:stack /
	// kind:compose / kind:filter).
	//
	// Single-source elements (most v1 fixtures) have len(Sources) ==
	// 1 with Directory == "". Handlers that pre-date multi-source
	// expect that shape; the staging loops in handler_cmake /
	// handler_pipeline / handler_import iterate Sources so multi-
	// source elements stage all their trees correctly.
	Sources []resolvedSource
	// Deps are the resolved depends-on edges of this element. Populated
	// during loadGraph; parents reference children.
	Deps []*element
	// BuildDeps is the subset of Deps drawn from build-depends (vs
	// runtime-depends). kind:filter's invariant is "exactly one
	// parent to filter from"; that parent is the single build-dep,
	// independent of how many runtime-deps the filter declares for
	// downstream consumers. Pipeline kinds (autotools/cmake/make/…)
	// may grow build-vs-runtime separation here when their typed-
	// filegroup wrappers ship.
	BuildDeps []*element
	// Patterns is the parsed <element>.read-paths.txt content
	// (committed alongside the .bst). Nil when the file is
	// absent — that's the default "entire tree real" case. Only
	// consumed by kind:cmake's handler today; pipeline-shape
	// handlers stage everything regardless.
	Patterns *readPathsPatterns

	// RealPaths / ZeroPaths are derived during the cmake handler's
	// per-element rendering: real files staged on disk, zero paths
	// handed to the zero_files starlark rule.
	RealPaths []string
	ZeroPaths []string

	// ProjectConfVars is the project-level variable override layer
	// loaded from the meta-project's project.conf (see
	// project_conf.go). Same map across every element resolved from
	// the same project.conf; nil when no project.conf was found
	// walking up from the .bst file's directory.
	ProjectConfVars map[string]string
	// ProjectConfConditionals are the project-level (?): branches
	// loaded from project.conf's variables: block. Same shape as
	// bstFile.Conditionals; together they feed the per-arch
	// select() pipeline-handler emission.
	ProjectConfConditionals []conditionalBranch
	// ProjectConfEnvironment is the project-level environment-var
	// map from project.conf. Element-level environment: blocks
	// override per key; the pipeline handler composes them and
	// resolves variable references before emitting on the
	// genrule's env attribute.
	ProjectConfEnvironment map[string]string
	// ProjectConfOptions is the project.conf options:
	// declarations carried onto the element so the pipeline
	// handler can identify option-typed dispatch variables in
	// (?): branches and look up their value spaces.
	ProjectConfOptions map[string]bstOption
}

// graph is the loaded set of elements with cross-references resolved.
// Elements is topologically sorted (dependencies before dependents).
type graph struct {
	Elements []*element
	ByName   map[string]*element
	// Options is the project.conf options: declarations the
	// graph carries through to writeProjectA so the //options
	// package renders alongside per-element packages. Empty
	// when no project.conf was found or no options: block was
	// declared.
	Options map[string]bstOption
}

// useFuseSourcesGlobal is the package-wide toggle for the
// PR #60 fuse-sources path. Set from main()'s --use-fuse-sources
// flag; consulted by handler_cmake.RenderA. Lives at package
// scope because the handler interface (RenderA(elem, elemPkg))
// has no plumbing for per-render config; threading it through
// would touch every handler. Acceptable singleton for a flag
// that's structurally process-wide anyway.
var useFuseSourcesGlobal bool

// stringList is a flag.Value for repeated flags (--bst foo.bst --bst bar.bst).
type stringList []string

func (s *stringList) String() string     { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error { *s = append(*s, v); return nil }

func main() {
	log.SetFlags(0)
	var bstPaths stringList
	flag.Var(&bstPaths, "bst", "path to a .bst file. Repeatable; pass once per element.")
	outA := flag.String("out", "", "output directory for project A (the meta workspace whose genrules run convert-element)")
	outB := flag.String("out-b", "", "optional: output directory for project B (the consumer workspace built against project A's outputs). When unset, only project A is rendered.")
	convertBin := flag.String("convert-element", "", "path to the convert-element binary (will be referenced from project-A's tools/)")
	sourceCache := flag.String("source-cache", "", "optional: directory of pre-fetched source trees, indexed by source-key. Non-kind:local sources whose key (SHA of kind+url+ref) hits a directory under this cache stage as if they were kind:local at that path. Callers populate the cache via the orchestrator's source-checkout layer or by hand for tests; write-a itself doesn't fetch.")
	useFuseSources := flag.Bool("use-fuse-sources", false, "experimental: render kind:cmake elements to consume sources via @src_<key>//:tree (the FUSE-mounted CAS path) rather than staging files into elements/<name>/sources/. Requires cas-fuse running and CAS_FUSE_MOUNT passed to bazel via --repo_env.")
	hostArch := flag.String("host-arch", "", "override the static host_arch dispatch variable (default: auto-detected from the build host).")
	buildArch := flag.String("build-arch", "", "override the static build_arch dispatch variable (default: auto-detected from the build host).")
	bootstrapBuildArch := flag.String("bootstrap-build-arch", "", "override the static bootstrap_build_arch dispatch variable (default: auto-detected from the build host).")
	traceBin := flag.String("convert-element-trace", "", "optional: path to convert-element-trace. When set (alongside --build-tracer-bin), every trace-driven kind (autotools / make / manual / script / makemaker / modulebuild) renders with the trace-driven native converter; round-2 (default) wires it via project A's per-element converter genrule, round-1 (opt-out via --trace-round1) wires it inline in project B's install genrule.")
	tracerBin := flag.String("build-tracer-bin", "", "optional: path to build-tracer. Required when --convert-element-trace is set, and required when --cmake-round2-fallback is set (Project B's kind:cmake install genrule wraps cmake configure / build / install under build-tracer).")
	publishBin := flag.String("trace-publish-bin", "", "optional: path to cmd/trace-publish. Required for the trace-driven round-2 path (the default when --convert-element-trace is set) and for --cmake-round2-fallback — staged into Project B's tools/ so the round-2 install genrule can publish its trace to the REAPI ActionCache.")
	lookupBin := flag.String("trace-lookup-bin", "", "optional: path to cmd/trace-lookup. Required for the trace-driven round-2 path (the default when --convert-element-trace is set) AND for --cmake-round2-fallback — staged into Project A's tools/ so the _trace_repo Bazel rule (rules/traces.bzl) can shell out at load time. The repo rule reads TRACE_LOOKUP_BIN from --repo_env at bazel build time, so the absolute path matters at build time, not render time. kind:cmake's round-2 fallback wires the same @trace_<elem>//:trace load-time lookup as the trace-driven kinds; convert-element doesn't yet CONSUME the trace bytes for refusal-refinement (that's queued behind the trace-driven convergence research follow-on), but the lookup is staged today so the follow-on is converter-side only.")
	round1 := flag.Bool("trace-round1", false, "opt out of round-2 (the default trace-driven path). Round-1 is the legacy single-genrule shape: project A is a marker filegroup; project B's install genrule runs configure / build / install + build-tracer + the converter inline, producing install_tree.tar + BUILD.bazel.out as sibling outputs of one action. Use when --trace-publish-bin / --trace-lookup-bin aren't on hand or when the round-2 rendezvous infra (REAPI AC + cas-fuse / bb_clientd mount) isn't available. Previously named with an autotools- prefix; the trace-driven path now serves multiple kinds (autotools / make / manual / script / makemaker / modulebuild), so the prefix dropped the kind specificity.")
	cmakeConfigureFileBin := flag.String("cmake-configure-file-bin", "", "optional: path to cmd/cmake-configure-file. When set, kind:cmake elements opt into the configure_file lift: convert-element emits genrules with .h.in as a real srcs input + //tools:cmake-configure-file invocation at Bazel build time, removing .h.in content from convert-element's cache key. The binary is staged into project A and project B tools/ so the genrule's tool label resolves. Off (the default) preserves the legacy base64-of-rendered-bytes shape; the audit's undercoverage report will continue to flag .h.in paths until the lift is opted into.")
	cmakeRound2Fallback := flag.Bool("cmake-round2-fallback", false, "optional: enable kind:cmake round-2 fallback shape (Phase B). Project A's converter genrule threads --unsupported-execute-process-fallback=true into convert-element so classifier refusals on execute_process produce the placeholder shape instead of Tier-1 exit; Project B emits a real install genrule (cmake configure + ninja + install + tar under build-tracer + inline trace-publish) replacing the current placeholder RenderB. Requires --build-tracer-bin + --trace-publish-bin + --trace-lookup-bin: the lookup wiring (load-time @trace_<elem>//:trace via the kind-agnostic _trace_repo rule) is staged today; convert-element doesn't yet CONSUME the trace bytes for refusal-refinement (that's queued behind the trace-driven convergence research follow-on) but the wiring is in place so the follow-on is converter-side only. See docs/design/cmake-execute-process-round2-fallback.md.")
	mesonBin := flag.String("convert-element-meson", "", "optional: path to convert-element-meson. When set, kind:meson elements render natively (per-element genrule that runs `meson setup` + introspection-driven IR translation, producing cc_library / cc_binary in BUILD.bazel.out). Off (the default) preserves the legacy pipeline-shape coarse install genrule. See docs/design/meson-native-render.md.")
	platformsJSON := flag.String("platforms-json", "", "optional: path to a JSON manifest declaring the multi-platform matrix for round-2 trace-driven kinds. Same shape as the orchestrator's --platforms-json (one entry per platform: name, constraints, optional select_label, optional reapi_properties — write-a ignores reapi_properties). When set, project A's per-element render fans out to N converter genrules per element (one per (element, platform) cell) plus one fold-element genrule composing their ir.json outputs; tools/traces.json gets N entries per element keyed `<elem>__<platform>` so the per-platform _trace_repo lookups don't collide. Project B's install genrule fan-out is queued as a follow-up — today's render emits one install per element regardless of --platforms-json, so the multi-platform path is render-shape complete but at runtime publishes only one platform's trace. Requires --fold-element-bin. Unset preserves the single-platform render shape byte-stably.")
	foldBin := flag.String("fold-element-bin", "", "optional: path to converter/cmd/fold-element. Required when --platforms-json is set — staged into Project A's tools/ so the per-element fold genrule can compose N per-platform ir.Package JSONs into one BUILD.bazel.")
	pyprojectBin := flag.String("convert-element-pyproject", "", "optional: path to convert-element-pyproject. When set, kind:pyproject elements render natively (per-element genrule that statically analyzes pyproject.toml + the source tree, producing py_library / py_binary in BUILD.bazel.out). Off (the default) preserves the legacy pipeline-shape coarse install genrule. See docs/design/pyproject-native-render.md.")
	pyprojectFallback := flag.Bool("pyproject-fallback", false, "optional: per-element auto-detection. When set (alongside --convert-element-pyproject), write-a probes each element's pyproject.toml at render time (running the converter with --probe) and emits the pipeline-shape coarse install genrule for any element whose probe doesn't return exit 0. That covers typed Tier-1 refusals (the native render would refuse), CLI/usage errors (exit 64), untyped Tier-2 errors (exit 65 — filesystem issues, malformed imports manifest), spawn failures (binary missing / wrong arch), and timeouts (probe hung past the per-element deadline). Operators see per-element refusal reasons on stderr; refused elements are still install_tree.tar-shaped (no per-target Bazel labels, but the element builds).")
	flag.Parse()

	if len(bstPaths) == 0 || *outA == "" || *convertBin == "" {
		flag.Usage()
		os.Exit(2)
	}

	// Wire the trace-driven autotools converter's render-time
	// config. Empty convertBin disables the trace+convert wrap
	// entirely — kind:autotools elements render as the
	// unmodified coarse install_tree.tar pipeline. With both
	// flags set, the install genrule wraps the build cmd in
	// build-tracer, runs convert-element-trace against the
	// trace, and produces a native BUILD.bazel.out alongside
	// install_tree.tar. Bazel's action cache (buildbarn in CI)
	// handles cross-node convergence via the existing
	// remote-cache plumbing.
	// --build-tracer-bin without --convert-element-trace is
	// allowed when --cmake-round2-fallback is set (kind:cmake's
	// install genrule wraps cmake under build-tracer without
	// involving the autotools converter); the inverse (autotools
	// without tracer) is still an error. The earlier check
	// rejected both shapes; relax it for the cmake-only case.
	if *traceBin != "" && *tracerBin == "" {
		log.Fatalf("--convert-element-trace requires --build-tracer-bin")
	}
	if *tracerBin != "" && *traceBin == "" && !*cmakeRound2Fallback {
		log.Fatalf("--build-tracer-bin requires either --convert-element-trace (the trace-driven round-{1,2} path for autotools / make / manual / script / makemaker / modulebuild kinds) or --cmake-round2-fallback (kind:cmake fallback)")
	}
	if *traceBin != "" {
		abs, err := filepath.Abs(*traceBin)
		if err != nil {
			log.Fatalf("resolve convert-element-trace path: %v", err)
		}
		traceConfig.convertBin = abs
	}
	if *tracerBin != "" {
		abs, err := filepath.Abs(*tracerBin)
		if err != nil {
			log.Fatalf("resolve build-tracer path: %v", err)
		}
		traceConfig.tracerBin = abs
	}
	if *cmakeConfigureFileBin != "" {
		abs, err := filepath.Abs(*cmakeConfigureFileBin)
		if err != nil {
			log.Fatalf("resolve cmake-configure-file path: %v", err)
		}
		cmakeConfig.configureFileBin = abs
	}
	if *mesonBin != "" {
		abs, err := filepath.Abs(*mesonBin)
		if err != nil {
			log.Fatalf("resolve convert-element-meson path: %v", err)
		}
		if _, err := os.Stat(abs); err != nil {
			log.Fatalf("convert-element-meson binary at %s: %v", abs, err)
		}
		mesonConfig.convertBin = abs
	}
	if *pyprojectBin != "" {
		abs, err := filepath.Abs(*pyprojectBin)
		if err != nil {
			log.Fatalf("resolve convert-element-pyproject path: %v", err)
		}
		if _, err := os.Stat(abs); err != nil {
			log.Fatalf("convert-element-pyproject binary at %s: %v", abs, err)
		}
		pyprojectConfig.convertBin = abs
		// Belt-and-suspenders with writeProjectA's reset: the
		// in-process entrypoint resets unconditionally, but
		// resetting here too clears any state from a previous
		// run before flag parsing completes — useful when a
		// library caller mutates the package-global caches
		// directly between runs and never reaches writeProjectA.
		// See resetPyprojectCaches's doc-comment.
		resetPyprojectCaches()
	}
	if *pyprojectFallback {
		if pyprojectConfig.convertBin == "" {
			log.Fatalf("--pyproject-fallback requires --convert-element-pyproject (the fallback flag drives per-element dispatch between the native genrule and the pipeline shape; with no native binary there's nothing to dispatch to)")
		}
		pyprojectConfig.fallbackEnabled = true
	}
	// kind:cmake round-2 fallback. Reuses the same build-tracer
	// + trace-publish + trace-lookup staging the autotools
	// round-2 path needs:
	//   - build-tracer wraps Project B's install genrule
	//     (cmake configure + ninja + install).
	//   - trace-publish runs inline at the end of B's install
	//     genrule, landing the AC entry keyed by srckey.
	//   - trace-lookup runs at A's load time (via the
	//     @trace_<elem>//:trace _trace_repo rule) so a previous
	//     Project B run's trace is available at convert-element
	//     action time.
	// All three are kind-agnostic; resolved here when
	// --cmake-round2-fallback is set without
	// --convert-element-trace (the trace-driven round-2 path
	// resolves them itself when both flags are set).
	if *cmakeRound2Fallback {
		if *tracerBin == "" || *publishBin == "" || *lookupBin == "" {
			log.Fatalf("--cmake-round2-fallback requires --build-tracer-bin, --trace-publish-bin, and --trace-lookup-bin (Project B wraps the build via build-tracer and publishes the trace; Project A wires the load-time @trace_<elem>//:trace lookup via trace-lookup)")
		}
		// The FUSE-sources kind:cmake template
		// (cmakeElementBuildFuse) renders a different
		// convert-element invocation that doesn't yet thread the
		// fallback flag (or other convert-element flags). Until
		// that template grows feature parity, reject the
		// combination outright rather than silently letting
		// classifier refusals Tier-1-exit on FUSE-mode runs.
		if *useFuseSources {
			log.Fatalf("--cmake-round2-fallback is incompatible with --use-fuse-sources today; the FUSE template doesn't yet thread --unsupported-execute-process-fallback into convert-element. Drop one of the flags.")
		}
		// build-tracer / trace-publish abs paths are resolved
		// above (trace-driven round-2 path uses the same flags),
		// so traceConfig.tracerBin / .publishBin already
		// hold the resolved values when --convert-element-trace
		// is also set. When ONLY --cmake-round2-fallback is set
		// (not autotools round-2), resolve here.
		if traceConfig.tracerBin == "" {
			abs, err := filepath.Abs(*tracerBin)
			if err != nil {
				log.Fatalf("resolve build-tracer path: %v", err)
			}
			traceConfig.tracerBin = abs
		}
		if traceConfig.publishBin == "" {
			abs, err := filepath.Abs(*publishBin)
			if err != nil {
				log.Fatalf("resolve trace-publish path: %v", err)
			}
			traceConfig.publishBin = abs
		}
		if traceConfig.lookupBin == "" {
			abs, err := filepath.Abs(*lookupBin)
			if err != nil {
				log.Fatalf("resolve trace-lookup path: %v", err)
			}
			traceConfig.lookupBin = abs
		}
		cmakeConfig.round2FallbackEnabled = true
	}
	// Round-2 is the default trace-driven path. It activates
	// when --convert-element-trace is set AND the user
	// hasn't passed --trace-round1. The round-2 wiring
	// requires the publisher + lookup binaries; without them,
	// hard-fail with a directive at the user (either supply the
	// binaries OR opt out via --trace-round1).
	if traceConfig.convertBin != "" && !*round1 {
		if *publishBin == "" || *lookupBin == "" {
			log.Fatalf("trace-driven round-2 (the default for kinds opted into the trace-driven path — autotools / make / manual / script / makemaker / modulebuild — when --convert-element-trace is set) requires --trace-publish-bin and --trace-lookup-bin; pass --trace-round1 to opt back into the legacy single-genrule shape that doesn't need them")
		}
		pubAbs, err := filepath.Abs(*publishBin)
		if err != nil {
			log.Fatalf("resolve trace-publish path: %v", err)
		}
		traceConfig.publishBin = pubAbs
		lkAbs, err := filepath.Abs(*lookupBin)
		if err != nil {
			log.Fatalf("resolve trace-lookup path: %v", err)
		}
		traceConfig.lookupBin = lkAbs
		traceConfig.round2Enabled = true
	}

	// Multi-platform fold for round-2 trace-driven kinds: when
	// --platforms-json is set, project A's per-element render
	// fans out one converter genrule per platform and one
	// fold-element genrule composing their ir.json outputs.
	// Requires --fold-element-bin so the fold tool is staged
	// into project A's tools/.
	if *platformsJSON != "" {
		if *foldBin == "" {
			log.Fatalf("--platforms-json requires --fold-element-bin (the fold tool composes N per-platform ir.json outputs into one BUILD.bazel)")
		}
		if !traceConfig.round2Enabled {
			log.Fatalf("--platforms-json requires the trace-driven round-2 path (--convert-element-trace + --build-tracer-bin + --trace-publish-bin + --trace-lookup-bin without --trace-round1); the per-platform fold only applies to round-2's per-element converter genrule")
		}
		fAbs, err := filepath.Abs(*foldBin)
		if err != nil {
			log.Fatalf("resolve fold-element path: %v", err)
		}
		traceConfig.foldBin = fAbs
		platforms, err := loadPlatformsManifest(*platformsJSON)
		if err != nil {
			log.Fatalf("%v", err)
		}
		traceConfig.platforms = platforms
	}

	g, err := loadGraph(bstPaths, *sourceCache)
	if err != nil {
		log.Fatalf("load graph: %v", err)
	}
	for _, elem := range g.Elements {
		if _, ok := handlers[elem.Bst.Kind]; !ok {
			log.Fatalf("element %q: write-a (Phase 2) supports kinds %s; got %q",
				elem.Name, supportedKinds(), elem.Bst.Kind)
		}
	}

	convertAbs, err := filepath.Abs(*convertBin)
	if err != nil {
		log.Fatalf("resolve convert-element path: %v", err)
	}
	if _, err := os.Stat(convertAbs); err != nil {
		log.Fatalf("convert-element binary at %s: %v", convertAbs, err)
	}

	useFuseSourcesGlobal = *useFuseSources

	// Apply CLI overrides for the static dispatch vars.
	// Auto-detected defaults from runtime.GOARCH cover the
	// common case (a dev host that matches the build host);
	// these flags are for cross-compile / host-emulation
	// scenarios where the operator knows better than the
	// detected GOARCH.
	if *hostArch != "" {
		staticDispatchVars["host_arch"] = *hostArch
	}
	if *buildArch != "" {
		staticDispatchVars["build_arch"] = *buildArch
	}
	if *bootstrapBuildArch != "" {
		staticDispatchVars["bootstrap_build_arch"] = *bootstrapBuildArch
	}

	if err := writeProjectA(g, *outA, convertAbs); err != nil {
		log.Fatalf("write project A: %v", err)
	}
	fmt.Printf("wrote project A at %s (%d elements: %s)\n",
		*outA, len(g.Elements), summarizeKinds(g))

	if *outB != "" {
		if err := writeProjectB(g, *outB); err != nil {
			log.Fatalf("write project B: %v", err)
		}
		fmt.Printf("wrote project B at %s\n", *outB)
	}
}

// loadGraph parses every .bst path in input order, then resolves
// `depends:` / `build-depends:` / `runtime-depends:` references
// into a topologically-sorted element list. Element keying:
//
//   - With project.conf found: element name is the path relative to
//     the project's element-root (project.conf dir + element-path),
//     minus ".bst". So a .bst at <project>/elements/foo/bar.bst keys
//     into the graph as "foo/bar", and a depends-list reference
//     "foo/bar.bst" resolves regardless of which subdir the
//     declaration lives in.
//   - With no project.conf: element name falls back to basename
//     minus ".bst". The pre-project.conf shape; covers single-fixture
//     trees and the existing testdata/meta-project fixtures that
//     don't declare a project.
//
// Unresolved deps surface as errors so typos and missing-from-loader
// elements both surface early.
//
// Project.conf is loaded once per invocation, walking up from the
// first .bst's directory. Multi-junction graphs (where different
// .bsts root different project.confs) aren't supported — they'd
// need a per-junction scope on top of this single-project shape.
//
// sourceCache is the pre-fetched-source-tree directory: when
// non-empty, non-kind:local sources whose source-key hits an
// entry under it stage as if they were kind:local at that path.
// Empty (the test-callsite default) leaves non-kind:local
// sources skipped at staging time.
func loadGraph(bstPaths []string, sourceCache string) (*graph, error) {
	g := &graph{ByName: map[string]*element{}}
	var info projectInfo
	if len(bstPaths) > 0 {
		var err error
		info, err = loadProjectInfoFromBst(bstPaths[0])
		if err != nil {
			return nil, fmt.Errorf("load project.conf: %w", err)
		}
		g.Options = info.Options
	}
	for _, p := range bstPaths {
		// Element-level (@): includes resolve against the project
		// root when one's known (BuildStream's contract). Without a
		// project.conf, fall back to the .bst's own directory —
		// covers self-contained fixtures with no project setup.
		includeBase := info.ProjectRoot
		if includeBase == "" {
			includeBase = filepath.Dir(p)
		}
		elem, err := loadElement(p, includeBase, sourceCache, info.Options)
		if err != nil {
			return nil, err
		}
		// Re-key the element by project-relative path when a
		// project.conf is in play. loadElement defaults Name to the
		// basename; here we widen it to the path-relative form.
		if info.ElementRoot != "" {
			absBst, err := filepath.Abs(p)
			if err != nil {
				return nil, err
			}
			rel, err := filepath.Rel(info.ElementRoot, absBst)
			if err != nil {
				return nil, fmt.Errorf("compute element-path-relative name for %s: %w", p, err)
			}
			if strings.HasPrefix(rel, "..") {
				return nil, fmt.Errorf("element %s lives outside the project's element-root %s", p, info.ElementRoot)
			}
			elem.Name = strings.TrimSuffix(rel, ".bst")
		}
		if existing, ok := g.ByName[elem.Name]; ok {
			return nil, fmt.Errorf("element %q declared twice (%s and %s)",
				elem.Name, existing.Name, p)
		}
		// Fold non-target_arch (?): branches statically against
		// staticDispatchVars (bootstrap_build_arch / host_arch /
		// build_arch). target_arch branches survive for the
		// pipeline-handler's per-arch select() lowering. Element-
		// level conditionals get the same treatment in
		// loadElement; project-level conditionals here.
		//
		// staticDispatchVars themselves also seed the variable
		// scope as defaults, so a `%{bootstrap_build_arch}`
		// reference resolves cleanly even before a (?): branch
		// would have set the variable.
		seeded := map[string]string{}
		for k, v := range staticDispatchVars {
			seeded[k] = v
		}
		for k, v := range info.Variables {
			seeded[k] = v
		}
		// Per-kind project-conf overrides
		// (`elements: <kind>: variables:`) layer on top of project-
		// wide variables. FDSDK uses this for autotools-conf.yml's
		// build-dir, conf-cmd, etc.
		if kv, ok := info.KindVars[elem.Bst.Kind]; ok {
			for k, v := range kv {
				seeded[k] = v
			}
		}
		foldedVars, foldedConds := foldStaticConditionals(seeded, info.Conditionals, staticDispatchVars, optionTypedSet(info.Options))
		elem.ProjectConfVars = foldedVars
		elem.ProjectConfConditionals = foldedConds
		elem.ProjectConfEnvironment = info.Environment
		elem.ProjectConfOptions = info.Options
		g.ByName[elem.Name] = elem
		g.Elements = append(g.Elements, elem)
	}
	// Resolve dependencies. All three lists (depends, build-depends,
	// runtime-depends) merge into element.Deps for v1 — write-a
	// doesn't yet distinguish build-only from runtime-only edges.
	// Duplicates (a dep listed in both `depends:` and
	// `build-depends:`, say) are tolerated: the dep's *element
	// pointer dedupes downstream (topo sort doesn't care about edge
	// multiplicity).
	for _, elem := range g.Elements {
		seen := map[*element]bool{}
		seenBuild := map[*element]bool{}
		// `depends:` is BuildStream's "both" shorthand — counts as
		// build AND runtime in the typed-output split. Treat as
		// build for the BuildDeps slice (kind:filter cares).
		// `build-depends:` is build-only. `runtime-depends:` is
		// runtime-only and does NOT contribute to BuildDeps.
		buildClasses := []struct {
			deps    []bstDep
			isBuild bool
		}{
			{elem.Bst.Depends, true},
			{elem.Bst.BuildDepends, true},
			{elem.Bst.RuntimeDepends, false},
		}
		for _, class := range buildClasses {
			for _, dep := range class.deps {
				// List-form deps (filename: [a.bst, b.bst]) expand to
				// N edges; the shared config: applies to each.
				for _, fn := range dep.expandedFilenames() {
					// Tolerate `depends: [- foo.bst]` style by
					// stripping the .bst suffix; also accept bare
					// element names.
					depName := strings.TrimSuffix(fn, ".bst")
					depElem, ok := g.ByName[depName]
					if !ok {
						return nil, fmt.Errorf("element %q depends on %q which is not in the graph", elem.Name, depName)
					}
					if !seen[depElem] {
						seen[depElem] = true
						elem.Deps = append(elem.Deps, depElem)
					}
					if class.isBuild && !seenBuild[depElem] {
						seenBuild[depElem] = true
						elem.BuildDeps = append(elem.BuildDeps, depElem)
					}
				}
			}
		}
	}
	// Topological sort (Kahn's algorithm). Stable secondary order on
	// element name so the rendered output is deterministic across
	// invocations regardless of input order.
	sorted, err := topoSort(g.Elements)
	if err != nil {
		return nil, err
	}
	g.Elements = sorted
	return g, nil
}

func topoSort(elems []*element) ([]*element, error) {
	indeg := map[*element]int{}
	for _, e := range elems {
		indeg[e] = 0
	}
	for _, e := range elems {
		for _, d := range e.Deps {
			indeg[e]++
			_ = d // edges are dep -> e; e's in-degree counts incoming edges.
		}
	}
	var ready []*element
	for _, e := range elems {
		if indeg[e] == 0 {
			ready = append(ready, e)
		}
	}
	sort.Slice(ready, func(i, j int) bool { return ready[i].Name < ready[j].Name })

	var out []*element
	for len(ready) > 0 {
		e := ready[0]
		ready = ready[1:]
		out = append(out, e)
		// Decrement in-degree of any element that depends on e.
		for _, other := range elems {
			for _, d := range other.Deps {
				if d == e {
					indeg[other]--
					if indeg[other] == 0 {
						ready = append(ready, other)
					}
				}
			}
		}
		sort.Slice(ready, func(i, j int) bool { return ready[i].Name < ready[j].Name })
	}
	if len(out) != len(elems) {
		return nil, fmt.Errorf("dependency cycle among %d elements", len(elems))
	}
	return out, nil
}

// loadElement parses one .bst into an *element. includeBase is the
// directory (@): include paths resolve against (the project root,
// matching BuildStream semantics). When no project.conf was found
// for this graph, callers pass filepath.Dir(bstPath) as a fallback
// — covers the existing self-contained fixtures that don't declare
// a project.
func loadElement(bstPath, includeBase, sourceCache string, options map[string]bstOption) (*element, error) {
	doc, err := loadAndComposeYAML(bstPath, includeBase, map[string]bool{})
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", bstPath, err)
	}
	// Extract (?): conditional branches from variables: BEFORE
	// the struct decode — yaml.v3 can't directly unmarshal the
	// list-of-mapping shape into our string-map Variables field.
	conditionals, err := extractConditionalsFromVariables(doc)
	if err != nil {
		return nil, fmt.Errorf("extract conditionals from %s: %w", bstPath, err)
	}
	configConditionals, err := extractConditionalsFromConfig(doc)
	if err != nil {
		return nil, fmt.Errorf("extract config conditionals from %s: %w", bstPath, err)
	}
	// Strip any (?): blocks that survived past the typed
	// extractors (variables: + config:). Branches under
	// environment: / public: aren't honored today; the strip
	// just keeps the loader from barfing on the list-of-mapping
	// shape landing in a strict-typed slot.
	stripRemainingConditionals(doc)
	var f bstFile
	if err := doc.Decode(&f); err != nil {
		return nil, fmt.Errorf("decode %s: %w", bstPath, err)
	}
	// Fold non-target_arch (?): branches statically — same shape
	// as loadGraph does for the project-level conditionals. Folds
	// matching overrides into f.Variables so the resolver doesn't
	// see those branches separately; target_arch branches survive
	// for select() lowering.
	foldedVars, foldedConds := foldStaticConditionals(f.Variables, conditionals, staticDispatchVars, optionTypedSet(options))
	f.Variables = foldedVars
	f.Conditionals = foldedConds
	// config: (?): branches don't have a "fold against static
	// vars" path today (no static overrides for config commands
	// — every per-arch override survives for the dispatch loop).
	// Stash on bstFile and let pipelineHandler consume.
	f.ConfigConditionals = configConditionals
	name := strings.TrimSuffix(filepath.Base(bstPath), ".bst")

	// Load <element>.read-paths.txt sibling if present. Absent
	// → nil patterns → "entire tree real" default in the cmake
	// handler.
	patterns, err := loadReadPathsPatterns(bstPath)
	if err != nil {
		return nil, fmt.Errorf("load read-paths patterns for %s: %w", bstPath, err)
	}

	elem := &element{Name: name, Bst: &f, Patterns: patterns}

	// Source resolution is per-kind. cmake / manual / autotools /
	// import / … pull a kind:local source tree from disk; stack /
	// filter / compose don't have their own sources. Phase 2's
	// supported kinds use kind:local where present.
	if h, ok := handlers[f.Kind]; ok && h.NeedsSources() {
		// Zero sources is valid for pipeline kinds that operate on
		// dep install trees only (e.g. kind:manual elements that
		// just stitch together build-depends outputs). Handlers
		// that can't function without sources surface that as a
		// render-phase error.
		for _, src := range f.Sources {
			rs := resolvedSource{
				Kind:      src.Kind,
				Directory: src.Directory,
				URL:       src.URL,
				Ref:       src.Ref,
				Track:     src.Track,
			}
			if src.Kind == "local" {
				// kind:local paths resolve project-root-relative.
				// includeBase is the project root (or the .bst's
				// own directory when no project.conf was found —
				// covers self-contained fixtures). Absolute paths
				// pass through unchanged. Matches BuildStream's
				// kind:local semantics: "the contents of a
				// directory rooted at the project."
				resolved := src.Path
				if !filepath.IsAbs(resolved) {
					resolved = filepath.Join(includeBase, resolved)
				}
				abs, err := filepath.Abs(resolved)
				if err != nil {
					return nil, err
				}
				rs.AbsPath = abs
			} else {
				// Non-kind:local source: try the cache. A hit
				// populates AbsPath so staging treats the entry as
				// kind:local-equivalent. A miss leaves AbsPath
				// empty; staging skips it (the BUILD still
				// renders, but bazel-build of the resulting
				// genrule would fail until the cache is
				// populated).
				resolveFromCache(sourceCache, &rs)
			}
			elem.Sources = append(elem.Sources, rs)
		}
	}
	return elem, nil
}

// writeProjectA renders the meta workspace project A: top-level files
// (MODULE.bazel, BUILD.bazel, rules/, tools/) shared across every
// element, then a per-element package under elements/<name>/ rendered
// by the element's kind handler.
func writeProjectA(g *graph, outDir, convertBin string) error {
	// Reset the per-invocation pyproject caches at the entrypoint.
	// The CLI's flag-parse-time reset (see main()) only catches
	// the CLI process boundary; in-process callers — tests,
	// library use — drive writeProjectA / writeProjectB directly
	// and would otherwise see stale entries from a previous run.
	// Resetting here matches the "once per write-a invocation"
	// contract documented on pyprojectShouldUseNative +
	// pyprojectNativeIncompatible regardless of entrypoint.
	// (writeProjectB intentionally does NOT reset — its RenderB
	// must see the cache populated by writeProjectA's RenderA so
	// the per-element refusal diagnostic prints once, not twice.)
	resetPyprojectCaches()
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return err
	}

	// Top-level files. Project A targets bazel >= 7 (bzlmod).
	// WORKSPACE.bazel was removed in bazel 8; MODULE.bazel is the
	// only module-declaration shape going forward. The meta workspace
	// has no external deps — only genrules — so the MODULE.bazel
	// here is just `module(...)` and bazel resolves nothing from
	// the registry beyond its built-in implicit deps (platforms,
	// rules_license, rules_java, etc., for toolchain bookkeeping).
	if err := writeFile(filepath.Join(outDir, "MODULE.bazel"), moduleBazelA(g)); err != nil {
		return err
	}
	if err := writeFile(filepath.Join(outDir, "BUILD.bazel"), "# project A root; per-element packages live under elements/<name>/.\n"); err != nil {
		return err
	}

	// Wire the zero_files rule by writing the embedded .bzl content
	// into project A's rules/ dir. The rule has no deps, so a flat
	// copy works; future iterations may expose it via a published
	// bazel module instead.
	if err := writeFile(filepath.Join(outDir, "rules", "zero_files.bzl"), zeroFilesBzl); err != nil {
		return err
	}
	if err := writeFile(filepath.Join(outDir, "rules", "sources.bzl"), renderSourcesBzl()); err != nil {
		return err
	}
	if traceConfig.round2Enabled || cmakeConfig.round2FallbackEnabled {
		if err := writeFile(filepath.Join(outDir, "rules", "traces.bzl"), renderTracesBzl()); err != nil {
			return err
		}
	}
	if err := writeFile(filepath.Join(outDir, "rules", "BUILD.bazel"), "# rules/ holds the starlark utilities project A's per-element BUILDs use.\n"); err != nil {
		return err
	}

	// Emit tools/sources.json — the data file the sources extension
	// reads to declare per-source repos. One entry per unique source
	// identity (kind + url + ref) across the graph; kind:local
	// sources are excluded since they don't need a CAS-backed repo.
	// When --source-cache resolved a tree for a given source,
	// populateDigests packs it and stamps the resulting CAS
	// Directory digest into the entry; entries without an
	// AbsPath get an empty Digest (the repo rule's empty-tree
	// fallback handles that without breaking load() resolution).
	rawSrcs := collectSources(g)
	withDigests, err := populateDigests(g, rawSrcs.Sources)
	if err != nil {
		return fmt.Errorf("compute source digests: %w", err)
	}
	srcs := sourcesJSON{Sources: withDigests}
	srcJSON, err := marshalSourcesJSON(srcs)
	if err != nil {
		return fmt.Errorf("marshal sources.json: %w", err)
	}
	if err := writeFile(filepath.Join(outDir, "tools", "sources.json"), string(srcJSON)); err != nil {
		return err
	}

	if traceConfig.round2Enabled || cmakeConfig.round2FallbackEnabled {
		traces, err := collectTraces(g)
		if err != nil {
			return fmt.Errorf("collect traces: %w", err)
		}
		traceJSON, err := marshalTracesJSON(traces)
		if err != nil {
			return fmt.Errorf("marshal traces.json: %w", err)
		}
		if err := writeFile(filepath.Join(outDir, "tools", "traces.json"), string(traceJSON)); err != nil {
			return err
		}
	}

	// Stage the convert-element binary into project A's tools/ so the
	// per-element genrule sees it as a hermetic input via tools = [...].
	// `exports_files` keeps Bazel's load() footprint minimal — no
	// sh_binary, no rules_cc dependency. Production wiring would
	// build convert-element via a go_binary rule.
	if err := os.MkdirAll(filepath.Join(outDir, "tools"), 0o755); err != nil {
		return err
	}
	stagedBin := filepath.Join(outDir, "tools", "convert-element")
	if err := copyFile(convertBin, stagedBin); err != nil {
		return fmt.Errorf("stage convert-element: %w", err)
	}
	if err := os.Chmod(stagedBin, 0o755); err != nil {
		return err
	}
	exports := []string{"convert-element", "sources.json"}
	if traceConfig.round2Enabled || cmakeConfig.round2FallbackEnabled {
		exports = append(exports, "traces.json")
	}
	// Also stage convert-element-trace + build-tracer when
	// the trace-driven kind:autotools path is configured. The
	// install genrule references both via tools = [...]; without
	// staging, the labels would resolve to nothing.
	autotoolsExports, err := stageAutotoolsTools(outDir)
	if err != nil {
		return err
	}
	exports = append(exports, autotoolsExports...)
	// fold-element only goes into project A's tools/: project A's
	// per-element fold genrule consumes it. Project B's install
	// genrules don't fold — they just run the build pipeline and
	// publish per-platform traces — so writeProjectB skips this.
	foldExports, err := stageFoldElement(outDir)
	if err != nil {
		return err
	}
	exports = append(exports, foldExports...)
	cmakeFileExport, err := stageCmakeConfigureFileTool(outDir)
	if err != nil {
		return err
	}
	if cmakeFileExport != "" {
		exports = append(exports, cmakeFileExport)
	}
	mesonExport, err := stageMesonConverter(outDir)
	if err != nil {
		return err
	}
	if mesonExport != "" {
		exports = append(exports, mesonExport)
	}
	pyprojectExport, err := stagePyprojectConverter(outDir)
	if err != nil {
		return err
	}
	if pyprojectExport != "" {
		exports = append(exports, pyprojectExport)
	}
	exportsList := ""
	for i, e := range exports {
		if i > 0 {
			exportsList += ", "
		}
		exportsList += fmt.Sprintf("%q", e)
	}
	if err := writeFile(filepath.Join(outDir, "tools", "BUILD.bazel"), fmt.Sprintf("exports_files([%s])\n", exportsList)); err != nil {
		return err
	}

	// Render //options/BUILD.bazel from project.conf options:
	// declarations. Skipped when no options exist (the package
	// stays absent rather than empty so downstream selects()
	// don't reference dangling labels).
	if err := writeOptionsPackage(outDir, g.Options); err != nil {
		return fmt.Errorf("render //options package: %w", err)
	}

	for _, elem := range g.Elements {
		h := handlers[elem.Bst.Kind]
		elemPkg := filepath.Join(outDir, "elements", elem.Name)
		if err := os.MkdirAll(elemPkg, 0o755); err != nil {
			return err
		}
		if err := h.RenderA(elem, elemPkg); err != nil {
			return fmt.Errorf("render project-A package for %q (kind %q): %w", elem.Name, elem.Bst.Kind, err)
		}
	}

	return nil
}

// stageCmakeConfigureFileTool copies cmake-configure-file into
// outDir/tools/ when --cmake-configure-file-bin was set, returning
// the exports_files entry the caller adds to tools/BUILD.bazel.
// Empty + nil when the lift is disabled (legacy shape stays
// active; nothing to stage).
//
// Symmetric staging in project A + project B keeps the
// per-element BUILD.bazel.out's `//tools:cmake-configure-file`
// label resolving regardless of which project hosts the
// genrule. The convert-element action that runs in project A
// doesn't itself invoke this binary — only project B's
// downstream Bazel build does — but both projects keep a
// staged copy so the meta-driver script doesn't have to know
// which side is which.
func stageCmakeConfigureFileTool(outDir string) (string, error) {
	if cmakeConfig.configureFileBin == "" {
		return "", nil
	}
	if err := os.MkdirAll(filepath.Join(outDir, "tools"), 0o755); err != nil {
		return "", err
	}
	stagedAt := filepath.Join(outDir, "tools", "cmake-configure-file")
	if err := copyFile(cmakeConfig.configureFileBin, stagedAt); err != nil {
		return "", fmt.Errorf("stage cmake-configure-file: %w", err)
	}
	if err := os.Chmod(stagedAt, 0o755); err != nil {
		return "", err
	}
	return "cmake-configure-file", nil
}

// stageMesonConverter copies convert-element-meson into outDir/tools/
// when --convert-element-meson was set, returning the exports_files
// entry the caller adds to tools/BUILD.bazel. Empty + nil when the
// native path is disabled (the kind:meson handler then renders the
// pipeline-shape fallback; nothing to stage).
//
// Staged in both project A and project B so the
// `//tools:convert-element-meson` label resolves regardless of
// which project the per-element BUILD ends up in. Project A is the
// only side that *runs* the binary today; project B's staged copy
// keeps the symmetry the cmake/autotools tools already maintain
// (lets the driver-script staging flow re-run write-a against
// either project without touching the tool wiring).
func stageMesonConverter(outDir string) (string, error) {
	if mesonConfig.convertBin == "" {
		return "", nil
	}
	if err := os.MkdirAll(filepath.Join(outDir, "tools"), 0o755); err != nil {
		return "", err
	}
	stagedAt := filepath.Join(outDir, "tools", "convert-element-meson")
	if err := copyFile(mesonConfig.convertBin, stagedAt); err != nil {
		return "", fmt.Errorf("stage convert-element-meson: %w", err)
	}
	if err := os.Chmod(stagedAt, 0o755); err != nil {
		return "", err
	}
	return "convert-element-meson", nil
}

// stagePyprojectConverter copies convert-element-pyproject into
// outDir/tools/ when --convert-element-pyproject was set,
// returning the exports_files entry the caller adds to
// tools/BUILD.bazel. Mirrors stageMesonConverter — staged in
// both project A and project B so the
// `//tools:convert-element-pyproject` label resolves
// regardless of which project the per-element BUILD ends up
// in. Project A is the only side that *runs* the binary today;
// project B's copy keeps the symmetry the other tools maintain.
func stagePyprojectConverter(outDir string) (string, error) {
	if pyprojectConfig.convertBin == "" {
		return "", nil
	}
	if err := os.MkdirAll(filepath.Join(outDir, "tools"), 0o755); err != nil {
		return "", err
	}
	stagedAt := filepath.Join(outDir, "tools", "convert-element-pyproject")
	if err := copyFile(pyprojectConfig.convertBin, stagedAt); err != nil {
		return "", fmt.Errorf("stage convert-element-pyproject: %w", err)
	}
	if err := os.Chmod(stagedAt, 0o755); err != nil {
		return "", err
	}
	return "convert-element-pyproject", nil
}

// stageAutotoolsTools copies the trace-pipeline binaries into
// outDir/tools/. The set staged depends on which paths are
// enabled:
//
//   - kind:autotools trace-driven path active (both convertBin
//     and tracerBin set on traceConfig): stages
//     convert-element-trace + build-tracer; round-2 also
//     stages trace-publish + trace-lookup.
//   - kind:cmake round-2 fallback active
//     (cmakeConfig.round2FallbackEnabled set, with
//     --build-tracer-bin + --trace-publish-bin + --trace-
//     lookup-bin on the CLI): stages build-tracer + trace-
//     publish + trace-lookup (no convert-element-trace —
//     kind:cmake has its own converter). The trace-lookup
//     wiring is staged today; convert-element doesn't yet
//     CONSUME the trace bytes for refusal-refinement (that
//     follow-on is converter-side only — the staging here
//     is part of what makes that future change small).
//
// Returns the additional exports_files entries the caller
// needs to add to its tools/BUILD.bazel; nil + nil when no
// staging path is enabled. Used by both writeProjectA and
// writeProjectB so the install genrule can resolve
// //tools:build-tracer + //tools:convert-element-trace
// regardless of which project hosts it.
//
// Used by both writeProjectA and writeProjectB so the
// install genrule can resolve //tools:build-tracer +
// //tools:convert-element-trace regardless of which
// project hosts it. The trailing "AutotoolsTools" name is
// historical (this used to be autotools-only); kind:cmake
// fallback now reuses the same staging primitive.
// Foundation for the architectural move of the install
// genrule from project A's BUILD into project B's BUILD
// (see docs/three-pass-flow.md "1 → 2 → 3 → 2′ → 3′" loop).
func stageAutotoolsTools(outDir string) ([]string, error) {
	autotoolsActive := traceConfig.convertBin != "" && traceConfig.tracerBin != ""
	cmakeFallbackActive := cmakeConfig.round2FallbackEnabled
	if !autotoolsActive && !cmakeFallbackActive {
		return nil, nil
	}
	if err := os.MkdirAll(filepath.Join(outDir, "tools"), 0o755); err != nil {
		return nil, err
	}
	var exports []string
	if autotoolsActive {
		stagedAt := filepath.Join(outDir, "tools", "convert-element-trace")
		if err := copyFile(traceConfig.convertBin, stagedAt); err != nil {
			return nil, fmt.Errorf("stage convert-element-trace: %w", err)
		}
		if err := os.Chmod(stagedAt, 0o755); err != nil {
			return nil, err
		}
		exports = append(exports, "convert-element-trace")
	}
	// build-tracer is needed for both autotools round-{1,2}
	// (its install genrule wraps configure/make/install) and
	// for kind:cmake round-2 fallback (its install genrule wraps
	// cmake configure / ninja / install). The binary lives at
	// traceConfig.tracerBin regardless — both paths
	// resolved it via --build-tracer-bin.
	if traceConfig.tracerBin != "" {
		stagedTracer := filepath.Join(outDir, "tools", "build-tracer")
		if err := copyFile(traceConfig.tracerBin, stagedTracer); err != nil {
			return nil, fmt.Errorf("stage build-tracer: %w", err)
		}
		if err := os.Chmod(stagedTracer, 0o755); err != nil {
			return nil, err
		}
		exports = append(exports, "build-tracer")
	}
	// trace-publish + trace-lookup land for both round-2
	// paths. kind:autotools' round-2 needs publish in B's
	// install genrule + lookup in A's converter genrule via
	// the _trace_repo rule. kind:cmake fallback wires the
	// same shape — Project B publishes via inline
	// trace-publish; Project A pulls @trace_<elem>//:trace at
	// load time via trace-lookup. The converter doesn't yet
	// CONSUME the trace bytes for refusal-refinement (that's
	// the trace-driven convergence research follow-on), but
	// the wiring is staged today so the follow-on is purely a
	// converter-side change.
	publishNeeded := traceConfig.round2Enabled || cmakeFallbackActive
	lookupNeeded := traceConfig.round2Enabled || cmakeFallbackActive
	if publishNeeded {
		stagedPub := filepath.Join(outDir, "tools", "trace-publish")
		if err := copyFile(traceConfig.publishBin, stagedPub); err != nil {
			return nil, fmt.Errorf("stage trace-publish: %w", err)
		}
		if err := os.Chmod(stagedPub, 0o755); err != nil {
			return nil, err
		}
		exports = append(exports, "trace-publish")
	}
	if lookupNeeded {
		stagedLk := filepath.Join(outDir, "tools", "trace-lookup")
		if err := copyFile(traceConfig.lookupBin, stagedLk); err != nil {
			return nil, fmt.Errorf("stage trace-lookup: %w", err)
		}
		if err := os.Chmod(stagedLk, 0o755); err != nil {
			return nil, err
		}
		exports = append(exports, "trace-lookup")
	}
	return exports, nil
}

// stageFoldElement copies fold-element into the given project's
// tools/, returning a one-element exports list when staged. Only
// project A's per-element fold genrule consumes it (project B's
// install genrules just publish per-platform traces; they don't
// fold), so writeProjectB doesn't call this. No-op when
// --fold-element-bin / --platforms-json weren't supplied.
func stageFoldElement(outDir string) ([]string, error) {
	if traceConfig.foldBin == "" {
		return nil, nil
	}
	if err := os.MkdirAll(filepath.Join(outDir, "tools"), 0o755); err != nil {
		return nil, err
	}
	stagedFold := filepath.Join(outDir, "tools", "fold-element")
	if err := copyFile(traceConfig.foldBin, stagedFold); err != nil {
		return nil, fmt.Errorf("stage fold-element: %w", err)
	}
	if err := os.Chmod(stagedFold, 0o755); err != nil {
		return nil, err
	}
	return []string{"fold-element"}, nil
}

// writeProjectB renders the consumer workspace project B reads
// against project A's outputs.
func writeProjectB(g *graph, outDir string) error {
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return err
	}

	if err := writeFile(filepath.Join(outDir, "MODULE.bazel"), moduleBazelB(g)); err != nil {
		return err
	}
	if err := writeFile(filepath.Join(outDir, "BUILD.bazel"),
		"# project B root; per-element packages live under elements/<name>/.\n",
	); err != nil {
		return err
	}

	// Project B reads the same sources extension + JSON as project
	// A so @src_<key>// repos resolve to the same CAS Directories
	// in both workspaces.
	if err := writeFile(filepath.Join(outDir, "rules", "sources.bzl"), renderSourcesBzl()); err != nil {
		return err
	}
	if traceConfig.round2Enabled || cmakeConfig.round2FallbackEnabled {
		if err := writeFile(filepath.Join(outDir, "rules", "traces.bzl"), renderTracesBzl()); err != nil {
			return err
		}
	}
	if err := writeFile(filepath.Join(outDir, "rules", "BUILD.bazel"), "# rules/ holds the starlark utilities project B's per-element BUILDs use.\n"); err != nil {
		return err
	}
	rawSrcs := collectSources(g)
	withDigests, err := populateDigests(g, rawSrcs.Sources)
	if err != nil {
		return fmt.Errorf("compute source digests: %w", err)
	}
	srcs := sourcesJSON{Sources: withDigests}
	srcJSON, err := marshalSourcesJSON(srcs)
	if err != nil {
		return fmt.Errorf("marshal sources.json: %w", err)
	}
	if err := writeFile(filepath.Join(outDir, "tools", "sources.json"), string(srcJSON)); err != nil {
		return err
	}
	if traceConfig.round2Enabled || cmakeConfig.round2FallbackEnabled {
		traces, err := collectTraces(g)
		if err != nil {
			return fmt.Errorf("collect traces: %w", err)
		}
		traceJSON, err := marshalTracesJSON(traces)
		if err != nil {
			return fmt.Errorf("marshal traces.json: %w", err)
		}
		if err := writeFile(filepath.Join(outDir, "tools", "traces.json"), string(traceJSON)); err != nil {
			return err
		}
	}
	// Stage convert-element-trace + build-tracer when the
	// trace-driven kind:autotools path is configured. Project B
	// hosts the install genrule (see docs/three-pass-flow.md);
	// without these tools the //tools:build-tracer +
	// //tools:convert-element-trace labels resolve to
	// nothing in the B-side BUILD.
	exports := []string{"sources.json"}
	if traceConfig.round2Enabled || cmakeConfig.round2FallbackEnabled {
		exports = append(exports, "traces.json")
	}
	autotoolsExports, err := stageAutotoolsTools(outDir)
	if err != nil {
		return err
	}
	exports = append(exports, autotoolsExports...)
	cmakeFileExport, err := stageCmakeConfigureFileTool(outDir)
	if err != nil {
		return err
	}
	if cmakeFileExport != "" {
		exports = append(exports, cmakeFileExport)
	}
	mesonExport, err := stageMesonConverter(outDir)
	if err != nil {
		return err
	}
	if mesonExport != "" {
		exports = append(exports, mesonExport)
	}
	pyprojectExport, err := stagePyprojectConverter(outDir)
	if err != nil {
		return err
	}
	if pyprojectExport != "" {
		exports = append(exports, pyprojectExport)
	}
	exportsList := ""
	for i, e := range exports {
		if i > 0 {
			exportsList += ", "
		}
		exportsList += fmt.Sprintf("%q", e)
	}
	if err := writeFile(filepath.Join(outDir, "tools", "BUILD.bazel"), fmt.Sprintf("# tools/ holds the JSON inputs the sources extension reads + the\n# trace-driven autotools binaries (build-tracer / convert-element-\n# autotools) the install genrule references.\nexports_files([%s])\n", exportsList)); err != nil {
		return err
	}

	for _, elem := range g.Elements {
		h := handlers[elem.Bst.Kind]
		elemPkg := filepath.Join(outDir, "elements", elem.Name)
		if err := os.RemoveAll(elemPkg); err != nil {
			return err
		}
		if err := os.MkdirAll(elemPkg, 0o755); err != nil {
			return err
		}
		if err := h.RenderB(elem, elemPkg); err != nil {
			return fmt.Errorf("render project-B package for %q (kind %q): %w", elem.Name, elem.Bst.Kind, err)
		}
	}
	return nil
}

func moduleBazelA(g *graph) string {
	var b strings.Builder
	b.WriteString(`module(name = "meta_project_a", version = "0.0.0")

# Project A only runs genrules (one per element invoking the
# per-kind translator). It declares the minimum bazel_deps the
# rendered tree actually loads:
`)
	if len(g.Options) > 0 {
		b.WriteString(`#   - bazel_skylib for the string_flag rule the //options
#     package declares (one rule per project.conf options:
#     entry).
bazel_dep(name = "bazel_skylib", version = "1.7.1")
`)
	} else {
		b.WriteString(`# (No bazel_deps — only genrules; bazel's standard implicit
# modules (platforms / rules_license / rules_java / etc.) cover
# toolchain bookkeeping.)
`)
	}
	b.WriteString(renderSourcesUseExtension(collectSources(g)))
	if traceConfig.round2Enabled || cmakeConfig.round2FallbackEnabled {
		traces, err := collectTraces(g)
		if err == nil {
			b.WriteString(renderTracesUseExtension(traces))
		}
	}
	return b.String()
}

// moduleBazelB declares rules_cc so project A's converted
// BUILD.bazel.out (which loads cc_library from @rules_cc//cc:defs.bzl)
// resolves cleanly in project B.
func moduleBazelB(g *graph) string {
	var b strings.Builder
	b.WriteString(`module(name = "meta_project_b", version = "0.0.0")

# rules_cc is what the cmake-converter emits load() lines against
# (load("@rules_cc//cc:defs.bzl", "cc_library")). Pin a recent stable
# release; this is downloaded from bcr.bazel.build the first time
# project B's bazel build runs.
bazel_dep(name = "rules_cc", version = "0.0.17")
`)
	if pyprojectConfig.convertBin != "" && hasKind(g, "pyproject") {
		// kind:pyproject's native render emits py_library /
		// py_binary rules that load() against
		// @rules_python//python:defs.bzl. Pin a stable release;
		// the version follows rules_python's own bzlmod release
		// cadence and is downloaded from bcr.bazel.build at
		// project B's first bazel build.
		//
		// Added whenever --convert-element-pyproject is set and
		// the graph contains any kind:pyproject element — even if
		// every such element is forced to the pipeline shape by
		// pyprojectNativeIncompatible (multi-source / Directory
		// set) or by Phase B's per-element fallback. The
		// alternative (gating on whether at least one element
		// will actually render natively) would require running
		// the per-element structural / probe checks here at
		// MODULE.bazel rendering time, which is well outside this
		// helper's scope. The extra registry fetch for
		// rules_python is a small cost vs the architectural
		// complexity of threading per-element render decisions
		// through the module-deps emit; ROADMAP notes the gating
		// as a follow-up if it ever becomes load-bearing.
		b.WriteString(`bazel_dep(name = "rules_python", version = "0.40.0")
`)
	}
	b.WriteString(renderSourcesUseExtension(collectSources(g)))
	if traceConfig.round2Enabled {
		traces, err := collectTraces(g)
		if err == nil {
			b.WriteString(renderTracesUseExtension(traces))
		}
	}
	return b.String()
}

// hasKind reports whether the graph has any element of the
// given kind. Used to gate optional bazel_deps in project B's
// MODULE.bazel (e.g. rules_python only when kind:pyproject is
// active and rendered natively).
func hasKind(g *graph, kind string) bool {
	for _, elem := range g.Elements {
		if elem != nil && elem.Bst != nil && elem.Bst.Kind == kind {
			return true
		}
	}
	return false
}

// renderSourcesUseExtension emits the use_extension + use_repo
// block for the sources module extension. Both project A and
// project B include the same block so the @src_<key>// repos
// resolve identically across the two workspaces.
//
// When the graph has no non-kind:local sources the block is
// omitted entirely — declaring an extension with zero repos is
// legal but noisy in MODULE.bazel review.
func renderSourcesUseExtension(s sourcesJSON) string {
	if len(s.Sources) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString(`
sources = use_extension("//rules:sources.bzl", "sources")
sources.from_json(path = "//tools:sources.json")
use_repo(
    sources,
`)
	for _, e := range s.Sources {
		fmt.Fprintf(&b, "    %q,\n", "src_"+e.Key)
	}
	b.WriteString(")\n")
	return b.String()
}

// summarizeKinds is for the startup log line: "kind:cmake×2, kind:stack×1".
func summarizeKinds(g *graph) string {
	counts := map[string]int{}
	for _, e := range g.Elements {
		counts[e.Bst.Kind]++
	}
	keys := make([]string, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("kind:%s×%d", k, counts[k]))
	}
	return strings.Join(parts, ", ")
}

// supportedKinds is for the unknown-kind error message.
func supportedKinds() string {
	keys := make([]string, 0, len(handlers))
	for k := range handlers {
		keys = append(keys, "kind:"+k)
	}
	sort.Strings(keys)
	return strings.Join(keys, ", ")
}

// writeFile writes content to path, creating parent dirs.
func writeFile(path, content string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(content), 0o644)
}

// copyFile copies src to dst, creating parent dirs.
func copyFile(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	info, err := os.Stat(src)
	if err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	// Preserve mode (chiefly +x). autotools projects ship a
	// committed `configure` script + helper shell scripts; if
	// staging strips +x, the genrule's `if [ -x ./configure ]`
	// guard falls through to autoreconf -ivf which fails
	// without configure.ac. Same hazard for arbitrary kind:local
	// shell scripts (bootstrap, autogen.sh, etc.).
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, info.Mode().Perm())
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}

// copyTree recursively copies src to dst. Symlinks resolve to their
// targets (they're rare in kind:local trees and Phase 1 doesn't need
// to preserve them).
func copyTree(src, dst string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		// Preserve symlinks as symlinks rather than dereferencing
		// (copyFile via os.Open would follow). Some BuildStream
		// kind:import elements ship dangling-on-disk symlinks
		// whose targets exist only in the staged install tree
		// (e.g. FDSDK's bootstrap/symlinks: bin → usr/bin where
		// usr/bin doesn't exist in the source dir but will after
		// staging). Re-creating the symlink as-is matches the
		// element's intent.
		if info.Mode()&os.ModeSymlink != 0 {
			linkTarget, err := os.Readlink(path)
			if err != nil {
				return fmt.Errorf("readlink %s: %w", path, err)
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			// Remove any pre-existing entry at target — re-runs
			// of write-a against the same output dir hit this.
			_ = os.Remove(target)
			return os.Symlink(linkTarget, target)
		}
		if info.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		return copyFile(path, target)
	})
}

// stageAllSources copies every kind:local source in elem.Sources
// into dstRoot, honoring each entry's Directory subpath. Used by
// handlers whose staging is "all sources, flat or under their
// declared subdir": kind:cmake's project-B copy, kind:import's
// filegroup root, the pipeline-handler's project-A source mount.
//
// Non-kind:local sources (kind:git_repo / kind:tar / kind:patch
// / kind:remote / etc.) hit one of two paths during loadElement:
//
//   - --source-cache hit: AbsPath got populated from the
//     pre-fetched cache directory; staging treats them
//     identically to kind:local.
//   - cache miss (or no --source-cache): AbsPath stays empty;
//     staging skips them. The rendered BUILD still includes the
//     element's package, but its source set is incomplete —
//     bazel-build would fail at action-input merkle time until
//     the cache gets populated.
//
// AbsPath != "" is the canonical "stageable" predicate; the kind
// itself isn't checked here.
func stageAllSources(elem *element, dstRoot string) error {
	for i, src := range elem.Sources {
		if src.AbsPath == "" {
			continue
		}
		dst := dstRoot
		if src.Directory != "" {
			dst = filepath.Join(dstRoot, src.Directory)
			if err := os.MkdirAll(dst, 0o755); err != nil {
				return fmt.Errorf("element %q source[%d]: prepare directory %q: %w", elem.Name, i, src.Directory, err)
			}
		}
		if err := copyTree(src.AbsPath, dst); err != nil {
			return fmt.Errorf("element %q source[%d]: stage %s → %s: %w", elem.Name, i, src.AbsPath, dst, err)
		}
	}
	return nil
}

// hasNonLocalSources reports whether any of elem.Sources is not
// kind:local. Handlers that need actual source bytes at render
// time (kind:cmake's narrowing) check this and either error out
// or fall back to a no-narrowing path.
func hasNonLocalSources(elem *element) bool {
	for _, s := range elem.Sources {
		if s.Kind != "local" {
			return true
		}
	}
	return false
}
