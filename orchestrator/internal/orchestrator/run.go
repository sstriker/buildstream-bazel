// Package orchestrator drives per-element conversions.
//
// The Run function walks the element graph in dependency-first topo order
// and invokes the converter once per kind:cmake element. M3a uses os/exec
// against a real `convert-element` binary; M3b will swap the same call
// shape for a REAPI Action submission.
//
// Outputs land under <Out>/elements/<elem-name>/. A successful conversion
// produces:
//
//	BUILD.bazel
//	cmake-config/<Pkg>{Config,Targets,Targets-release}.cmake
//	read_paths.json
//
// A Tier-1 failure produces failure.json under the same directory and the
// element is recorded in the global failures registry. Tier-2/3 failures
// (converter crashed, infrastructure error) propagate as Go errors that
// abort the whole orchestrator.
package orchestrator

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"

	"github.com/sstriker/cmake-to-bazel/internal/cas"
	"github.com/sstriker/cmake-to-bazel/internal/manifest"
	"github.com/sstriker/cmake-to-bazel/internal/reapi"
	"github.com/sstriker/cmake-to-bazel/internal/shadow"
	"github.com/sstriker/cmake-to-bazel/internal/synthprefix"
	"github.com/sstriker/cmake-to-bazel/orchestrator/internal/allowlistreg"
	"github.com/sstriker/cmake-to-bazel/orchestrator/internal/element"
	"github.com/sstriker/cmake-to-bazel/orchestrator/internal/exports"
	"github.com/sstriker/cmake-to-bazel/orchestrator/internal/sourcecheckout"
)

// Options configures one Run call.
type Options struct {
	// Project / Graph are the parsed FDSDK input. Run filters to
	// kind:cmake elements internally; the caller passes the full graph.
	Project *element.Project
	Graph   *element.Graph

	// Out is the output root; the canonical layout
	// (docs/m3-plan.md, "Output layout") is built underneath it.
	Out string

	// SourcesBase, when non-empty, takes precedence over per-element
	// `sources[].path` resolution. The orchestrator looks for each
	// element's source tree at <SourcesBase>/<element-name>/. Used by
	// the test fixture and by orchestrators that pre-stage sources.
	SourcesBase string

	// ConverterBinary is the path to the convert-element binary. Defaults
	// to "convert-element" (PATH lookup).
	ConverterBinary string

	// Store is the CAS+ActionCache backing the per-element conversion
	// cache. When nil, Run constructs a LocalStore at <Out>/cache so
	// existing tests and offline runs work unchanged. Pass a GRPCStore
	// to share cache hits across orchestrator instances.
	Store cas.Store

	// Executor, when non-nil, takes over per-element execution on AC
	// miss: instead of running convert-element locally via os/exec,
	// the orchestrator submits the Action to Executor.Execute and
	// uses the returned ActionResult. M3b's GRPCExecutor goes through
	// REAPI's Execution service. Nil = local exec (M5 default).
	Executor reapi.Executor

	// Platform encodes the action's platform properties (OS family,
	// arch, tool versions). When empty, Run uses defaultPlatform —
	// linux/x86_64 with the cmake/ninja/bwrap pins from the Makefile.
	// Two orchestrators must use the same Platform to share cache hits.
	Platform []reapi.PlatformProperty

	// Concurrency caps how many elements process in parallel. <=0 falls
	// back to runtime.NumCPU(). Topological ordering is preserved —
	// dependents wait for their deps to finish — but independent
	// subgraphs run in parallel. Set to 1 for sequential, deterministic
	// log output (tests + dev loops).
	Concurrency int

	// PerElementTimeout caps how long any single element's pipeline
	// (resolve source, build action, AC lookup, convert/execute,
	// publish) may take. Zero = defaultPerElementTimeout. The same
	// value is set as Action.Timeout for remote execution so workers
	// enforce the limit symmetrically.
	//
	// On timeout, processElement returns a Tier-2/3 error that
	// cancels the run via ctx; in-flight goroutines abort.
	PerElementTimeout time.Duration

	// SourceAsset, when non-nil, enables `kind: remote-asset` source
	// resolution: the orchestrator looks up sources by uri+qualifiers
	// against the Remote Asset endpoint and materializes the resulting
	// Directory from Store. Used for FDSDK sources published via
	// `bst source push`. Local + git checkouts work without it.
	SourceAsset *cas.RemoteAsset

	// ToolchainCMakeFile, when non-empty, is the path to a CMake
	// toolchain file (typically derive-toolchain's toolchain.cmake)
	// passed to every per-element converter invocation via
	// --toolchain-cmake-file. Lets cmake skip its compiler-detection
	// probe — a measurable per-conversion latency win at project
	// scale. The orchestrator validates the file exists at startup.
	ToolchainCMakeFile string

	// CollectToolchainSignal, when true, asks each element's
	// converter to copy its cmake File API reply directory into
	// <Out>/elements/<name>/toolchain-signal/. Off by default:
	// existing flows that don't unify toolchains pay nothing for
	// the extra directory copy. Today's behaviour is capture-only:
	// the on-disk shape is the input format for the future
	// unifier-side consumer queued under ROADMAP.md Next as
	// "Element-signal consumption in the unifier", which will
	// fold per-element builtin-include / sysroot facts into each
	// platform's ResolvedToolchain.Base.
	CollectToolchainSignal bool

	// PlatformsJSON, when non-empty, points at a JSON manifest
	// declaring the target platform matrix the orchestrator
	// should fan out per-element conversions across. Each entry
	// pairs a Bazel platform name with its constraint_value
	// labels and the REAPI Action.Platform properties to route
	// the matching action to. With a one-platform manifest the
	// orchestrator's behaviour is byte-identical to today's
	// single-platform flow (the per-element fold's N=1
	// degenerate case); with more, each kind:cmake element
	// produces a unified BUILD.bazel whose attributes carry
	// `select()` blocks for the per-platform divergence.
	//
	// Manifest schema (one entry per platform):
	//
	//	{
	//	  "name": "linux_x86_64",
	//	  "constraints": [
	//	    "@platforms//os:linux",
	//	    "@platforms//cpu:x86_64"
	//	  ],
	//	  "reapi_properties": [
	//	    {"name": "OSFamily", "value": "linux"},
	//	    {"name": "Arch",     "value": "x86_64"}
	//	  ]
	//	}
	//
	// reapi_properties is required because there's no canonical
	// cross-walk from constraint_value labels to REAPI property
	// names; the operator declares them explicitly. Common
	// conventions (OSFamily, Arch) compose with whatever the
	// REAPI worker pool advertises. The reapi_properties slice
	// effectively replaces the orchestrator's hardcoded
	// defaultPlatform when this manifest is set.
	PlatformsJSON string

	// Log is a back-compat shim: when set and Logger is nil, the
	// orchestrator builds a slog.NewTextHandler(Log) and uses that
	// for structured progress records. Per-element converter
	// stdout/stderr capture (merged) also flows here. Defaults to
	// os.Stderr when both Log and Logger are nil.
	Log io.Writer

	// Logger, when non-nil, takes precedence over Log for structured
	// progress records. The CLI wires either a TextHandler (default)
	// or JSONHandler (--log-format=json) here. Per-element converter
	// stdout/stderr still flow through Log (or Stderr as fallback)
	// since subprocess output is line-oriented bytes, not records.
	Logger *slog.Logger
}

// defaultPerElementTimeout is the cap when Options.PerElementTimeout
// is unset. 30 minutes accommodates large cmake configures (libdrm-
// class projects with thousands of TUs) without being so generous
// that a hung action holds the queue indefinitely. Operators can
// raise it via --element-timeout when needed.
const defaultPerElementTimeout = 30 * time.Minute

// defaultPlatform mirrors the toolchain pins enforced by the Makefile
// and by cmakerun.AssertVersion. Bumping any pin invalidates every
// element's cache by changing the Action digest — that's the contract.
var defaultPlatform = []reapi.PlatformProperty{
	{Name: "Arch", Value: "x86_64"},
	{Name: "OSFamily", Value: "linux"},
	{Name: "bwrap-version", Value: "0.8.0"},
	{Name: "cmake-version", Value: "3.28.3"},
	{Name: "ninja-version", Value: "1.11.1"},
}

// Result summarizes a Run.
type Result struct {
	Converted   []string
	Failed      []FailureRecord
	CacheHits   []string // elements whose outputs came from the action cache
	CacheMisses []string // elements that ran convert-element this pass

	// Timings aggregates per-phase wall-clock durations across all
	// elements that ran the converter this pass (cache-hits skip the
	// converter and contribute nothing). The configure / translation
	// ratio answers "where does conversion time go?" — a high ratio
	// means cmake configure dominates and caching wins big; a low
	// ratio means the converter's IR / emit pipeline is the
	// bottleneck.
	Timings TimingsSummary
}

// TimingsSummary is the cumulative + per-element view the orchestrator
// emits at the end of a Run.
type TimingsSummary struct {
	// Per-element timings keyed by element name. Only elements that
	// ran the converter this pass appear; cache-hits are absent.
	PerElement map[string]ElementTimings `json:"per_element,omitempty"`

	// Cumulative wall-clock seconds across all converter runs this
	// pass. Sum of the per-element values.
	TotalCMakeConfigureSecs float64 `json:"total_cmake_configure_seconds"`
	TotalTranslationSecs    float64 `json:"total_translation_seconds"`
	TotalConverterSecs      float64 `json:"total_converter_seconds"`

	// ConfigureToTranslationRatio = TotalCMakeConfigureSecs /
	// TotalTranslationSecs. Zero when TranslationSecs is zero.
	ConfigureToTranslationRatio float64 `json:"configure_to_translation_ratio"`
}

// ElementTimings is one element's per-phase wall-clock seconds, as
// reported by convert-element.
type ElementTimings struct {
	CMakeConfigureSecs float64 `json:"cmake_configure_seconds"`
	TranslationSecs    float64 `json:"translation_seconds"`
	TotalSecs          float64 `json:"total_seconds"`
}

// FailureRecord is the per-element entry the orchestrator collects for the
// global failures.json registry. Mirrors converter Tier-1 failure.json
// schema with an added Element field.
type FailureRecord struct {
	Element string `json:"element"`
	Tier    int    `json:"tier"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

// sandboxPrefix is the canonical, host-independent anchor the imports
// manifest uses for cross-element link paths. The token is virtual: no
// filesystem path of that name exists. lower remaps real prefix-dir
// paths recorded in cmake's codemodel onto this anchor before the
// LookupLinkPath call (see lower.manifestPrefixAnchor — the two must
// match). The token shape is a holdover from the bwrap era when the
// converter actually mounted --prefix-dir at /opt/prefix; we keep the
// shape rather than churn the manifest schema.
const sandboxPrefix = "/opt/prefix/"

// Run drives the conversion. Returns a populated Result on success even if
// some elements failed Tier-1; only Tier-2/3 (or orchestrator-level)
// errors return non-nil err.
func Run(ctx context.Context, opts Options) (*Result, error) {
	if opts.Project == nil || opts.Graph == nil {
		return nil, errors.New("orchestrator: Project and Graph required")
	}
	if opts.Out == "" {
		return nil, errors.New("orchestrator: Out required")
	}
	conv := opts.ConverterBinary
	if conv == "" {
		conv = "convert-element"
	}
	convAbs, err := exec.LookPath(conv)
	if err != nil {
		return nil, fmt.Errorf("orchestrator: converter binary %q not on PATH: %w", conv, err)
	}

	order, err := opts.Graph.TopoSort()
	if err != nil {
		return nil, err
	}
	cmakeOrder := opts.Graph.FilterByKind(order, "cmake")

	if err := os.MkdirAll(filepath.Join(opts.Out, "elements"), 0o755); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(opts.Out, "manifest"), 0o755); err != nil {
		return nil, err
	}

	// depRecords accumulates per-element data needed to write a downstream
	// element's imports manifest:
	//   raw   — manifest.Export values with CMakeTarget set (no labels yet)
	//   relLinkPaths — cmake_target -> prefix-relative IMPORTED_LOCATION paths
	//   pkg   — the bundle's <Pkg> (drives lib/cmake/<Pkg>/ in the
	//           consumer's synth-prefix)
	//
	// LinkPaths in the consumer's manifest are absolute paths under the
	// consumer's prefix root, so they have to be stitched per-consumer
	// rather than stored once per dep. The runner owns this map.
	importsRoot := filepath.Join(opts.Out, "imports")
	prefixRoot := filepath.Join(opts.Out, "synth-prefix")
	shadowRoot := filepath.Join(opts.Out, "shadow")
	registryRoot := filepath.Join(opts.Out, "registry", "allowlists")
	if err := os.MkdirAll(importsRoot, 0o755); err != nil {
		return nil, err
	}

	registry := allowlistreg.New(registryRoot)

	store := opts.Store
	if store == nil {
		ls, err := cas.NewLocalStore(filepath.Join(opts.Out, "cache"))
		if err != nil {
			return nil, fmt.Errorf("orchestrator: init local cas: %w", err)
		}
		store = ls
	}
	platform := opts.Platform
	if len(platform) == 0 {
		platform = defaultPlatform
	}

	timeout := opts.PerElementTimeout
	if timeout <= 0 {
		timeout = defaultPerElementTimeout
	}

	executor := opts.Executor
	if executor == nil {
		executor = reapi.NewLocalExecutor()
	}

	platformsMatrix, err := loadPlatformsManifest(opts.PlatformsJSON)
	if err != nil {
		return nil, err
	}

	r := &runner{
		opts:            opts,
		conv:            conv,
		convAbs:         convAbs,
		store:           store,
		executor:        executor,
		platform:        platform,
		platformsMatrix: platformsMatrix,
		resolver:        newResolver(opts, store),
		timeout:         timeout,
		importsRoot:     importsRoot,
		prefixRoot:      prefixRoot,
		shadowRoot:      shadowRoot,
		registry:        registry,
		depRecords:      map[string]*depRecord{},
		res:             &Result{},
		logger:          loggerFor(opts),
	}

	if err := r.driveElements(ctx, cmakeOrder); err != nil {
		return nil, err
	}

	// Topo-order driver may complete elements out of input order under
	// concurrency; sort the result lists for stable diffs.
	sort.Strings(r.res.Converted)
	sort.Strings(r.res.CacheHits)
	sort.Strings(r.res.CacheMisses)
	sort.Slice(r.res.Failed, func(i, j int) bool { return r.res.Failed[i].Element < r.res.Failed[j].Element })

	// Aggregate timings across elements that ran the converter this
	// pass. Cache hits don't re-emit timings.json so they're absent
	// from the per-element map and contribute zero to the totals.
	r.res.Timings = aggregateTimings(opts.Out, r.res.CacheMisses)
	logTimingsSummary(r.logger, r.res)

	if err := writeManifest(opts.Out, r.res); err != nil {
		return nil, err
	}
	if err := writeBzlmodProject(opts.Out); err != nil {
		return nil, err
	}
	return r.res, nil
}

// rulesCCVersion is the rules_cc bzlmod dep that the generated MODULE.bazel
// pins. The per-element BUILD.bazel emitted by the converter loads
// `@rules_cc//cc:defs.bzl`, so this version must be ≥ what those rules
// require. Bumping the pin invalidates `bazel build` against the
// orchestrator output and is a coordinated change with the converter.
const rulesCCVersion = "0.2.17"

// writeBzlmodProject emits a self-contained MODULE.bazel at <out>/ so the
// orchestrator's per-element BUILD.bazel files form a directly-buildable
// bzlmod project. Cross-element deps stamped by exports.AsElement use
// `//elements/<name>:<target>` labels which resolve against this module
// root.
func writeBzlmodProject(out string) error {
	body := []byte(`module(name = "converted", version = "0.0.0")

bazel_dep(name = "rules_cc", version = "` + rulesCCVersion + `")
`)
	return os.WriteFile(filepath.Join(out, "MODULE.bazel"), body, 0o644)
}

// aggregateTimings reads each cache-miss element's timings.json and
// collects per-element + cumulative numbers. Errors reading individual
// files are silent — timings are observability, not correctness.
func aggregateTimings(out string, ranElements []string) TimingsSummary {
	per := make(map[string]ElementTimings, len(ranElements))
	var sumCfg, sumXlate, sumTotal float64
	for _, name := range ranElements {
		t, ok, err := loadTimings(filepath.Join(out, "elements", name, "timings.json"))
		if err != nil || !ok {
			continue
		}
		per[name] = ElementTimings{
			CMakeConfigureSecs: t.CMakeConfigureSecs,
			TranslationSecs:    t.TranslationSecs,
			TotalSecs:          t.TotalSecs,
		}
		sumCfg += t.CMakeConfigureSecs
		sumXlate += t.TranslationSecs
		sumTotal += t.TotalSecs
	}
	ratio := 0.0
	if sumXlate > 0 {
		ratio = sumCfg / sumXlate
	}
	return TimingsSummary{
		PerElement:                  per,
		TotalCMakeConfigureSecs:     sumCfg,
		TotalTranslationSecs:        sumXlate,
		TotalConverterSecs:          sumTotal,
		ConfigureToTranslationRatio: ratio,
	}
}

// logTimingsSummary emits a structured run-summary record plus a
// converter-wall-clock record (when timings populated). Operator-facing;
// the full per-element breakdown is in r.res.Timings for callers (and
// writeManifest persists it).
func logTimingsSummary(logger *slog.Logger, res *Result) {
	logger.Info("run summary",
		"converted", len(res.Converted),
		"cache_hits", len(res.CacheHits),
		"cache_misses", len(res.CacheMisses),
		"failed", len(res.Failed),
	)
	if res.Timings.TotalConverterSecs > 0 {
		logger.Info("converter wall-clock",
			"total_seconds", res.Timings.TotalConverterSecs,
			"cmake_configure_seconds", res.Timings.TotalCMakeConfigureSecs,
			"translation_seconds", res.Timings.TotalTranslationSecs,
			"configure_to_translation_ratio", res.Timings.ConfigureToTranslationRatio,
		)
	}
}

// runner holds the per-Run state shared across element-processing
// goroutines. The mutex protects the maps and slices written by
// completing elements (depRecords, res) and the registry's persistent
// state.
type runner struct {
	opts            Options
	conv            string
	convAbs         string
	store           cas.Store
	executor        reapi.Executor
	platform        []reapi.PlatformProperty
	platformsMatrix []convertPlatform // empty = single-platform path
	resolver        *sourcecheckout.Resolver
	timeout         time.Duration // per-element cap; zero = none

	importsRoot, prefixRoot, shadowRoot string
	registry                            *allowlistreg.Registry

	mu         sync.Mutex
	depRecords map[string]*depRecord
	res        *Result
	logger     *slog.Logger
}

// driveElements fans the cmake-only topo-ordered slice across a
// goroutine pool. Each element waits for its deps' done channels
// before processing; the pool size caps in-flight work without
// reshaping topology. Tier-1 failures stay local to the element;
// Tier-2/3 cancel the run via ctx.
func (r *runner) driveElements(ctx context.Context, cmakeOrder []string) error {
	concurrency := r.opts.Concurrency
	if concurrency <= 0 {
		concurrency = runtime.NumCPU()
	}

	// One done channel per element so dependents can wait without
	// polling. Pre-allocate so racy readers don't see a missing entry.
	done := make(map[string]chan struct{}, len(cmakeOrder))
	for _, n := range cmakeOrder {
		done[n] = make(chan struct{})
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	var (
		errMu    sync.Mutex
		firstErr error
	)
	recordErr := func(e error) {
		errMu.Lock()
		if firstErr == nil {
			firstErr = e
			cancel()
		}
		errMu.Unlock()
	}

	// Concurrency cap. Independent subgraphs share the slot pool.
	sem := make(chan struct{}, concurrency)

	var wg sync.WaitGroup
	for _, name := range cmakeOrder {
		name := name
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Always close the done channel — even on error or abort —
			// so dependents that haven't picked up runCtx cancellation
			// yet aren't stuck waiting.
			defer close(done[name])

			// Wait for cmake-kind deps to land. Non-cmake deps don't
			// have done channels; skip them.
			for _, dep := range r.opts.Graph.Edges[name] {
				ch, ok := done[dep]
				if !ok {
					continue
				}
				select {
				case <-ch:
				case <-runCtx.Done():
					return
				}
			}
			if runCtx.Err() != nil {
				return
			}

			// Acquire a concurrency slot.
			select {
			case sem <- struct{}{}:
			case <-runCtx.Done():
				return
			}
			defer func() { <-sem }()

			elemCtx := runCtx
			if r.timeout > 0 {
				var cancelElem context.CancelFunc
				elemCtx, cancelElem = context.WithTimeout(runCtx, r.timeout)
				defer cancelElem()
			}
			if err := r.processElement(elemCtx, name); err != nil {
				recordErr(err)
			}
		}()
	}
	wg.Wait()
	return firstErr
}

// processElement runs the full per-element pipeline. Returns nil on
// success or Tier-1 (failure recorded in r.res); error on Tier-2/3.
func (r *runner) processElement(ctx context.Context, name string) error {
	// Short-circuit dependents of failed elements with a synthetic
	// Tier-1: trying to convert them would fail at find_package
	// (synth-prefix is missing the failed dep's bundle) with a less
	// informative configure-failed code. dep-failed surfaces the root
	// cause cleanly.
	if failedDep, ok := r.firstFailedDep(name); ok {
		r.appendFailure(FailureRecord{
			Element: name,
			Tier:    1,
			Code:    "dep-failed",
			Message: fmt.Sprintf("transitive cmake dep %s failed Tier-1; skipped to surface root cause", failedDep),
		})
		r.logger.Info("element skipped", "name", name, "reason", "dep_failed", "failed_dep", failedDep)
		return nil
	}

	el := r.opts.Project.Elements[name]
	resolved, err := r.resolver.Resolve(ctx, el)
	if err != nil {
		return fmt.Errorf("element %s: %w", name, err)
	}
	realSrcRoot := resolved.Path
	if resolved.Digest != nil {
		r.logger.Info("element start", "name", name, "source_digest", resolved.Digest.Hash)
	} else {
		r.logger.Info("element start", "name", name)
	}

	// allowlist registry is shared persistent state — serialize.
	r.mu.Lock()
	if err := r.registry.Load(name); err != nil {
		r.mu.Unlock()
		return fmt.Errorf("element %s: load allowlist registry: %w", name, err)
	}
	matcher := r.registry.Matcher(name)
	r.mu.Unlock()

	shadowSrc, err := buildShadowForElement(r.shadowRoot, name, realSrcRoot, matcher)
	if err != nil {
		return fmt.Errorf("element %s: shadow tree: %w", name, err)
	}

	// depRecords is populated by completed dependents. Snapshot under
	// the lock and let buildPrefixForElement / writeImportsForElement
	// work on the immutable copy.
	r.mu.Lock()
	depSnapshot := make(map[string]*depRecord, len(r.depRecords))
	for k, v := range r.depRecords {
		depSnapshot[k] = v
	}
	r.mu.Unlock()

	prefixPath, err := buildPrefixForElement(r.prefixRoot, name, r.opts.Graph, depSnapshot, r.opts.Out)
	if err != nil {
		return fmt.Errorf("element %s: synth-prefix: %w", name, err)
	}
	importsPath, err := writeImportsForElement(r.importsRoot, name, r.opts.Graph, depSnapshot, prefixPath)
	if err != nil {
		return fmt.Errorf("element %s: imports manifest: %w", name, err)
	}

	elemOut := filepath.Join(r.opts.Out, "elements", name)

	// Multi-platform path: when --platforms-json is set, fan
	// out one Action per platform and fold per-platform IRs into
	// one BUILD.bazel via elementfold. Single-platform path
	// (the empty-matrix case) flows through the reapi.Build /
	// tryActionCacheHit path below — byte-identical behaviour.
	// Both paths converge at the depRecord-stitching tail that
	// reads elemOut/cmake-config + elemOut/read_paths.json.
	failed, err := r.runConvertActions(ctx, name, realSrcRoot, elemOut, shadowSrc, importsPath, prefixPath)
	if err != nil {
		return err
	}
	if failed {
		return nil
	}

	readPaths, err := loadReadPaths(filepath.Join(r.opts.Out, "elements", name, "read_paths.json"))
	if err != nil {
		return fmt.Errorf("element %s: load read_paths: %w", name, err)
	}

	bundleDir := filepath.Join(r.opts.Out, "elements", name, "cmake-config")
	raw, err := exports.FromBundle(bundleDir)
	if err != nil {
		return fmt.Errorf("element %s: extract exports: %w", name, err)
	}
	relPaths, err := exports.PrefixRelativeLinkPaths(bundleDir)
	if err != nil {
		return fmt.Errorf("element %s: link paths: %w", name, err)
	}
	pkg, err := synthprefix.PkgFromBundle(bundleDir)
	if err != nil {
		return fmt.Errorf("element %s: pkg-from-bundle: %w", name, err)
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.registry.Update(name, readPaths); err != nil {
		return fmt.Errorf("element %s: update allowlist registry: %w", name, err)
	}
	r.depRecords[name] = &depRecord{
		ElementName:        name,
		Pkg:                pkg,
		RawExports:         raw,
		PrefixRelLinkPaths: relPaths,
	}
	// Stage source-file symlinks at the per-element package root so
	// `bazel build //elements/<name>:<target>` resolves srcs/hdrs the
	// converter emitted as relative paths (e.g. `srcs = ["src/hello.c"]`).
	// Without this, Bazel reports "missing input file" — elemOut only
	// carries the generated BUILD.bazel + cmake-config bundle. Re-stage
	// on every successful pass (cache-hit or miss) since the source
	// symlinks aren't part of the cached Action output.
	if err := stageSourceSymlinks(filepath.Join(r.opts.Out, "elements", name), realSrcRoot); err != nil {
		return fmt.Errorf("element %s: stage source symlinks: %w", name, err)
	}
	r.res.Converted = append(r.res.Converted, name)
	return nil
}

// stageSourceSymlinks creates one symlink per top-level entry of srcRoot
// directly under pkgDir (the element's package root, alongside BUILD.bazel
// and cmake-config/) so Bazel can resolve the converter's relative
// srcs/hdrs paths. Existing symlinks at colliding paths are replaced;
// non-symlinks (e.g. the generated BUILD.bazel) are left untouched and
// the conflicting srcRoot entry is skipped. The staged links are
// excluded from determinism.json on purpose since per-host absolute
// targets would make the SHA-by-tree comparison flap across machines.
func stageSourceSymlinks(pkgDir, srcRoot string) error {
	if err := os.MkdirAll(pkgDir, 0o755); err != nil {
		return err
	}
	// Drop any existing symlinks first so a renamed or deleted top-level
	// source-root entry doesn't leave a stale symlink behind on re-runs.
	// Non-symlinks (BUILD.bazel, cmake-config/) stay.
	existing, err := os.ReadDir(pkgDir)
	if err != nil {
		return err
	}
	for _, e := range existing {
		if e.Type()&os.ModeSymlink == 0 {
			continue
		}
		if err := os.Remove(filepath.Join(pkgDir, e.Name())); err != nil {
			return err
		}
	}
	entries, err := os.ReadDir(srcRoot)
	if err != nil {
		return err
	}
	for _, e := range entries {
		target, err := filepath.Abs(filepath.Join(srcRoot, e.Name()))
		if err != nil {
			return err
		}
		link := filepath.Join(pkgDir, e.Name())
		// Non-symlink (the converter's BUILD.bazel, cmake-config/, etc.) —
		// skip rather than clobber.
		if info, err := os.Lstat(link); err == nil && info.Mode()&os.ModeSymlink == 0 {
			continue
		}
		if err := os.Symlink(target, link); err != nil {
			return err
		}
	}
	return nil
}

func (r *runner) appendCacheHit(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.res.CacheHits = append(r.res.CacheHits, name)
}

func (r *runner) appendCacheMiss(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.res.CacheMisses = append(r.res.CacheMisses, name)
}

func (r *runner) appendFailure(fr FailureRecord) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.res.Failed = append(r.res.Failed, fr)
}

// firstFailedDep walks name's transitive cmake-deps closure and returns
// the first dep already recorded in r.res.Failed. Concurrency-safe;
// reads r.res.Failed under the mutex.
//
// Topological scheduling guarantees deps complete before dependents,
// so this snapshot is always consistent: a missing failed-record means
// the dep succeeded (or was non-cmake), never "still in flight".
func (r *runner) firstFailedDep(name string) (string, bool) {
	closure := transitiveDeps(r.opts.Graph, name)
	r.mu.Lock()
	defer r.mu.Unlock()
	failed := make(map[string]bool, len(r.res.Failed))
	for _, fr := range r.res.Failed {
		failed[fr.Element] = true
	}
	for _, d := range closure {
		if failed[d] {
			return d, true
		}
	}
	return "", false
}

// newResolver builds a sourcecheckout.Resolver wired to the
// orchestrator's source cache (under <Out>/sources/) and respecting
// the --sources-base override when set. SourceAsset+Store hand the
// resolver the M3d remote-asset path.
func newResolver(opts Options, store cas.Store) *sourcecheckout.Resolver {
	return &sourcecheckout.Resolver{
		CacheDir:    filepath.Join(opts.Out, "sources"),
		SourcesBase: opts.SourcesBase,
		ElementSourceDir: func(el *element.Element) string {
			return filepath.Dir(el.SourcePath)
		},
		Asset: opts.SourceAsset,
		Store: store,
	}
}

func loadFailure(name, path string) (*FailureRecord, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var raw struct {
		Tier    int    `json:"tier"`
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil, err
	}
	return &FailureRecord{
		Element: name,
		Tier:    raw.Tier,
		Code:    raw.Code,
		Message: raw.Message,
	}, nil
}

// writeManifest writes converted.json + failures.json + determinism.json
// under <out>/manifest/.
//
// converted.json + failures.json entries are sorted by element name for
// stable diffs. determinism.json is a path -> sha256 map covering every
// per-element output file the orchestrator produced; M3a step 7's
// determinism test compares these across machines. Schema versioned via
// "version": 1 so M4's regression detector can fence on incompatible
// reads.
func writeManifest(out string, res *Result) error {
	conv := append([]string(nil), res.Converted...)
	sort.Strings(conv)
	type elemEntry struct {
		Name string `json:"name"`
	}
	convDoc := struct {
		Version  int         `json:"version"`
		Elements []elemEntry `json:"elements"`
	}{Version: 1}
	for _, n := range conv {
		convDoc.Elements = append(convDoc.Elements, elemEntry{Name: n})
	}
	if err := writeJSON(filepath.Join(out, "manifest", "converted.json"), convDoc); err != nil {
		return err
	}

	fails := append([]FailureRecord(nil), res.Failed...)
	sort.Slice(fails, func(i, j int) bool { return fails[i].Element < fails[j].Element })
	failDoc := struct {
		Version  int             `json:"version"`
		Elements []FailureRecord `json:"elements"`
	}{Version: 1, Elements: fails}
	if err := writeJSON(filepath.Join(out, "manifest", "failures.json"), failDoc); err != nil {
		return err
	}

	timingsDoc := struct {
		Version int            `json:"version"`
		Summary TimingsSummary `json:"summary"`
	}{
		Version: 1,
		Summary: res.Timings,
	}
	if err := writeJSON(filepath.Join(out, "manifest", "timings.json"), timingsDoc); err != nil {
		return err
	}

	return writeDeterminism(out)
}

// writeDeterminism walks every file under <out>/elements/ and hashes it,
// then writes a sorted path -> sha256 map under <out>/manifest/
// determinism.json. The file is what M3a step 7's three-tmpdir test
// compares for byte-equality across machines.
func writeDeterminism(out string) error {
	root := filepath.Join(out, "elements")
	type entry struct {
		Path   string `json:"path"`
		SHA256 string `json:"sha256"`
	}
	var entries []entry
	if err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			return err
		}
		if d.IsDir() || !d.Type().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		// timings.json carries wall-clock seconds, which legitimately
		// differ run-to-run; excluding it keeps the three-tmpdir
		// determinism test honest.
		if filepath.Base(rel) == "timings.json" {
			return nil
		}
		sum, err := hashRegularFile(p)
		if err != nil {
			return err
		}
		entries = append(entries, entry{
			Path:   filepath.ToSlash(rel),
			SHA256: sum,
		})
		return nil
	}); err != nil {
		return err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	doc := struct {
		Version int     `json:"version"`
		Files   []entry `json:"files"`
	}{Version: 1, Files: entries}
	return writeJSON(filepath.Join(out, "manifest", "determinism.json"), doc)
}

func hashRegularFile(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func writeJSON(path string, v any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}

func logOf(opts Options) io.Writer {
	if opts.Log != nil {
		return opts.Log
	}
	return os.Stderr
}

// loggerFor returns the structured logger to use for orchestrator progress
// records. Precedence: Options.Logger > Options.Log (wrapped as
// TextHandler) > os.Stderr (TextHandler).
func loggerFor(opts Options) *slog.Logger {
	if opts.Logger != nil {
		return opts.Logger
	}
	return slog.New(slog.NewTextHandler(logOf(opts), nil))
}

// depRecord is the per-element bundle/exports state the orchestrator holds
// across the per-element loop.
type depRecord struct {
	ElementName        string
	Pkg                string
	RawExports         []*manifest.Export
	PrefixRelLinkPaths map[string][]string
}

// writeImportsForElement writes a per-element imports.json containing the
// exports of every element transitively reachable from `name` via deps.
// LinkPaths in each export are absolute paths under the consumer's
// prefixPath (the synth-prefix tree built earlier this iteration). When
// prefixPath is empty (no deps with bundles) LinkPaths are omitted.
//
// Returns the host path to the written file, or "" if there are no exports
// to declare (avoids passing --imports-manifest with an empty document).
func writeImportsForElement(importsRoot, name string, g *element.Graph, depRecords map[string]*depRecord, prefixPath string) (string, error) {
	closure := transitiveDeps(g, name)
	var elems []*manifest.Element
	for _, d := range closure {
		rec, ok := depRecords[d]
		if !ok || len(rec.RawExports) == 0 {
			continue
		}
		linkPathsFor := map[string][]string{}
		if prefixPath != "" {
			// CMake's codemodel records link.commandFragments at the real
			// prefix dir's absolute path. lower (see manifestPrefixAnchor
			// in converter/internal/lower/lower.go) remaps those onto the
			// canonical sandboxPrefix token before LookupLinkPath, so the
			// imports manifest stays host-independent regardless of where
			// the consumer's prefix tree happens to land on disk.
			for tgt, rels := range rec.PrefixRelLinkPaths {
				for _, rel := range rels {
					linkPathsFor[tgt] = append(linkPathsFor[tgt],
						sandboxPrefix+strings.TrimPrefix(rel, "/"))
				}
			}
		}
		elems = append(elems, exports.AsElement(d, rec.RawExports, linkPathsFor))
	}
	if len(elems) == 0 {
		return "", nil
	}
	sort.Slice(elems, func(i, j int) bool { return elems[i].Name < elems[j].Name })

	doc := &manifest.Imports{Version: 1, Elements: elems}
	out := filepath.Join(importsRoot, filepath.FromSlash(name)+".json")
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		return "", err
	}
	body, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(out, append(body, '\n'), 0o644); err != nil {
		return "", err
	}
	return out, nil
}

// buildPrefixForElement assembles a CMAKE_PREFIX_PATH-shaped tree under
// <prefixRoot>/<element-name>/ holding the synthesized cmake-config
// bundles for every dep with exports plus zero-byte stubs for each
// IMPORTED_LOCATION_<CONFIG>. Returns the absolute prefix path the
// converter should mount, or "" if there's nothing to stage.
func buildPrefixForElement(prefixRoot, name string, g *element.Graph, depRecords map[string]*depRecord, outRoot string) (string, error) {
	closure := transitiveDeps(g, name)
	var deps []synthprefix.DepBundle
	for _, d := range closure {
		rec, ok := depRecords[d]
		if !ok || rec.Pkg == "" {
			continue // either non-cmake or had no Config.cmake
		}
		bundleDir := filepath.Join(outRoot, "elements", d, "cmake-config")
		if _, err := os.Stat(bundleDir); err != nil {
			continue
		}
		deps = append(deps, synthprefix.DepBundle{Pkg: rec.Pkg, SourceDir: bundleDir})
	}
	if len(deps) == 0 {
		return "", nil
	}
	dst := filepath.Join(prefixRoot, filepath.FromSlash(name))
	// Build is destructive on existing dst; M3a step 6's cache layer will
	// reuse prefix trees, but until then we recreate from scratch.
	if err := os.RemoveAll(dst); err != nil {
		return "", err
	}
	if err := synthprefix.Build(dst, deps); err != nil {
		return "", err
	}
	return dst, nil
}

// buildShadowForElement creates <shadowRoot>/<element-name>/ as a path-only
// mirror of the real source tree, with file content preserved only for
// allowlisted paths. cmake's access(R_OK)-only configure-time semantics
// (docs/cmake_analysis.md §0) make a shadow tree configure-equivalent to the
// real one for every non-allowlisted file. The orchestrator points
// convert-element at the shadow tree so content-only edits to .c files
// are absorbed at the cache key (M3a step 6).
func buildShadowForElement(shadowRoot, name, realSrc string, m shadow.Matcher) (string, error) {
	dst := filepath.Join(shadowRoot, filepath.FromSlash(name))
	if err := os.RemoveAll(dst); err != nil {
		return "", err
	}
	if err := shadow.Build(realSrc, dst, m); err != nil {
		return "", err
	}
	return dst, nil
}

// tryActionCacheHit looks up the Action's digest in ActionCache. On
// hit, materializes every output blob to elemOut and returns
// (true, fr, nil) — fr is non-nil when the cached ActionResult
// represents a Tier-1 failure (failure.json present in outputs).
//
// Returns (false, nil, nil) on cache miss, blob-eviction (stale AC
// entry), or any other recoverable lookup failure — caller falls
// through to local execution.
func tryActionCacheHit(ctx context.Context, store cas.Store, built *reapi.BuiltAction, elemOut string) (bool, *FailureRecord, error) {
	ar, err := store.GetActionResult(ctx, built.ActionDigest)
	if errors.Is(err, cas.ErrNotFound) {
		return false, nil, nil
	}
	if err != nil {
		return false, nil, err
	}
	if err := os.RemoveAll(elemOut); err != nil {
		return false, nil, err
	}
	if err := os.MkdirAll(elemOut, 0o755); err != nil {
		return false, nil, err
	}
	if err := reapi.MaterializeResult(ctx, store, ar, elemOut); err != nil {
		// Stale AC entry (referenced blob evicted) — treat as miss
		// and fall through. Per the M5 plan's resilience case.
		var miss *reapi.ErrMissingBlob
		if errors.As(err, &miss) {
			_ = os.RemoveAll(elemOut)
			return false, nil, nil
		}
		return false, nil, err
	}
	if ar.ExitCode == 1 {
		fr, ferr := loadFailure("", filepath.Join(elemOut, "failure.json"))
		if ferr != nil {
			return false, nil, ferr
		}
		return true, fr, nil
	}
	return true, nil, nil
}

// remoteExecute submits the Action to the configured Executor and
// materializes the returned ActionResult into elemOut. Both the local
// in-process LocalExecutor and remote GRPCExecutor publish the
// ActionResult to AC themselves; the orchestrator only consumes it.
//
// Tier-1 failure detection: the converter writes failure.json on
// exit-1, which the executor collects as an OutputFile.
func remoteExecute(ctx context.Context, store cas.Store, exec reapi.Executor, built *reapi.BuiltAction, elemOut, name string, log io.Writer) (*FailureRecord, error) {
	ar, err := exec.Execute(ctx, store, built)
	if err != nil {
		return nil, fmt.Errorf("element %s: remote execute: %w", name, err)
	}
	// Stream the worker's stdout/stderr to the orchestrator's log so
	// operators debugging Tier-1 failures don't have to bb-browser
	// the action result by hand. Errors fetching are non-fatal — the
	// action result itself is the source of truth.
	streamWorkerOutput(ctx, store, ar, log)

	if err := os.MkdirAll(elemOut, 0o755); err != nil {
		return nil, fmt.Errorf("element %s: mkdir elemOut: %w", name, err)
	}
	if err := reapi.MaterializeResult(ctx, store, ar, elemOut); err != nil {
		return nil, fmt.Errorf("element %s: materialize result: %w", name, err)
	}
	if ar.ExitCode == 1 {
		fr, ferr := loadFailure(name, filepath.Join(elemOut, "failure.json"))
		if ferr != nil {
			return nil, fmt.Errorf("element %s: remote action exit 1 but failure.json unreadable: %w", name, ferr)
		}
		return fr, nil
	}
	if ar.ExitCode != 0 {
		return nil, fmt.Errorf("element %s: remote action exit %d", name, ar.ExitCode)
	}
	return nil, nil
}

// streamWorkerOutput fetches stdout/stderr blobs from CAS and writes
// them to the orchestrator's log. Best-effort: a missing or
// unreadable blob is ignored so a remote-execute success isn't
// gated on log retrieval working.
func streamWorkerOutput(ctx context.Context, store cas.Store, ar *repb.ActionResult, log io.Writer) {
	if ar.StdoutDigest != nil && ar.StdoutDigest.SizeBytes > 0 {
		if body, err := store.GetBlob(ctx, ar.StdoutDigest); err == nil {
			_, _ = log.Write(body)
		}
	}
	if ar.StderrDigest != nil && ar.StderrDigest.SizeBytes > 0 {
		if body, err := store.GetBlob(ctx, ar.StderrDigest); err == nil {
			_, _ = log.Write(body)
		}
	}
}

// loadReadPaths reads the converter-emitted read_paths.json into a string
// slice. Missing file is not an error (older converter versions or runs
// without --out-read-paths produce no file); we just return an empty list.
func loadReadPaths(path string) ([]string, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []string
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return out, nil
}

// elementTimings captures one element's per-phase wall-clock timings,
// as written by convert-element's --out-timings flag. Missing file
// (cache hit, older converter, etc.) is silently skipped — timings
// are an operator UX feature, not a correctness requirement.
type elementTimings struct {
	CMakeConfigureSecs float64 `json:"cmake_configure_seconds"`
	TranslationSecs    float64 `json:"translation_seconds"`
	TotalSecs          float64 `json:"total_seconds"`
}

// loadTimings reads timings.json. Returns zero value when the file
// is absent — a cache-hit element doesn't re-emit timings, and the
// orchestrator's summary copes with that by counting only elements
// that ran the converter this pass.
func loadTimings(path string) (elementTimings, bool, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return elementTimings{}, false, nil
	}
	if err != nil {
		return elementTimings{}, false, err
	}
	var raw struct {
		Version int `json:"version"`
		elementTimings
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return elementTimings{}, false, fmt.Errorf("parse %s: %w", path, err)
	}
	return raw.elementTimings, true, nil
}

// transitiveDeps walks the graph from `name` collecting every reachable
// dep in dependency-first order (deps before dependents). Idempotent under
// repeated calls; returns deduplicated names.
func transitiveDeps(g *element.Graph, name string) []string {
	seen := map[string]bool{}
	var order []string
	var walk func(n string)
	walk = func(n string) {
		for _, d := range g.Edges[n] {
			if seen[d] {
				continue
			}
			seen[d] = true
			walk(d)
			order = append(order, d)
		}
	}
	walk(name)
	return order
}
