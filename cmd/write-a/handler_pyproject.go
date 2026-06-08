package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
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

	// fallbackEnabled toggles per-element auto-detection (Phase
	// B install-plan fallback's option-A shape — see
	// docs/architecture.md). Without it, every
	// kind:pyproject element renders natively when convertBin
	// is set; refused-by-Phase-A elements Tier-1 fail at
	// bazel-build time. With it, write-a probes each element's
	// pyproject.toml at render time (running the converter
	// binary with --probe) and emits the pipeline shape for
	// elements that would refuse, the native genrule for
	// elements that would succeed. Operators flip the flag once
	// and have every kind:pyproject element render correctly
	// regardless of per-element backend / metadata shape.
	fallbackEnabled bool

	// fidelity / diagnostics are the operator-facing dial values
	// resolved by deriveModes (see modes.go) and threaded from
	// main.go into the pyproject-converter genrule cmd. Both flags
	// are pass-through no-ops at the converter today (see
	// convert-element-pyproject/main.go for why); they're threaded
	// for CLI uniformity.
	fidelity    string
	diagnostics bool
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
		// The pipeline handler's stagePipelineSources copies files
		// into elements/<name>/sources/ but doesn't remove stale
		// entries from previous renders. The native path below
		// rm -rf's the stage dir; so does this branch, so toggling
		// --convert-element-pyproject or --pyproject-fallback
		// between runs (or routing a Directory-set / multi-source
		// element to pipeline) can't leave files from one shape's
		// staging polluting the other's inputs.
		if err := os.RemoveAll(filepath.Join(elemPkg, "sources")); err != nil {
			return err
		}
		return pyprojectPipelineHandler().RenderA(elem, elemPkg)
	}
	if pyprojectConfig.fallbackEnabled && !pyprojectShouldUseNative(elem) {
		// Probe says this element would refuse Phase A — render
		// the pipeline shape instead. Operator-visible: the
		// element still ships its install-root TreeArtifact;
		// downstream consumers reference //elements/<elem>:<elem>_install.
		// Same stale-sources concern as the convertBin="" branch
		// above — clear the stage dir before delegating.
		if err := os.RemoveAll(filepath.Join(elemPkg, "sources")); err != nil {
			return err
		}
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
	if pyprojectConfig.fallbackEnabled && !pyprojectShouldUseNative(elem) {
		return pyprojectPipelineHandler().RenderB(elem, elemPkg)
	}
	if err := stageAllSources(elem, elemPkg); err != nil {
		return err
	}
	placeholder := projectBPlaceholder(elem.Name, " (kind:pyproject native)")
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

// pyprojectShouldUseNative consults the convert-element-pyproject
// binary in --probe mode against the element's first source
// tree's AbsPath. Returns true when the probe exits 0 (native
// render would succeed); false on any non-zero exit. A non-zero
// exit covers both typed Tier-1 refusals (exit 1) and untyped /
// infrastructure failures — CLI usage errors (exit 64), untyped
// Tier-2 errors (exit 65: filesystem issues, malformed imports
// manifest, unhandled converter paths), or genuine spawn
// failures (binary not executable, wrong arch, ENOENT, signal
// during spawn). See pyprojectFallbackCause's doc-comment for
// how those cases are surfaced to the operator-facing
// diagnostic.
//
// Per-element invocation cost: ~one process spawn per kind:
// pyproject element at write-a time. FDSDK has 115 of them;
// total render-time overhead is well under a second on a
// modest dev machine.
//
// Forced-fallback shapes handled here (don't even run the
// probe — the probe would see a different tree than the
// converter, so a "go native" result can't be trusted):
//   - No on-disk source tree (Sources[0].AbsPath empty). e.g.
//     kind:remote-asset / kind:tar that hasn't been resolved
//     by source-checkout; the converter wouldn't be able to
//     read pyproject.toml at action time either.
//
// Multi-source elements and Sources[0].Directory!="" are
// structurally incompatible with the native render itself
// (not just the probe), and so they're handled one level up by
// pyprojectNativeIncompatible — invoked from RenderA / RenderB
// BEFORE this function runs. Owning that decision in a single
// place keeps the diagnostic + cache consistent. That helper
// routes those elements to pipeline shape regardless of
// --pyproject-fallback's state.
//
// Caching: results are memoized on pyprojectProbeCache by the
// element's NAME so the cache can't cross-contaminate elements
// that share a source directory but declare different deps
// (the probe outcome depends on elem.Deps via the temp
// imports.json passed to --imports-manifest, so source-path
// alone isn't a safe key). Element names are unique within a
// write-a invocation. The per-element refusal diagnostic prints
// exactly once per write-a invocation (on the cache-miss path);
// cache hits are silent so RenderA + RenderB don't each print
// the same line. Re-runs of write-a start with a fresh process
// and a fresh cache, so the diagnostic surfaces again on the
// next invocation.
//
// Probe-vs-genrule input parity: the native genrule stages its
// source tree via `cp -L` (symlink dereferencing), while the
// probe operates directly on elem.Sources[0].AbsPath, so a
// pathological source layout (e.g. directory symlinks pointing
// at trees containing a Cargo.toml or *.c the probe wouldn't
// recurse into) could in theory let probe-pass / genrule-fail.
// BuildStream source-checkout typically produces real files
// (not exotic symlink layouts), so this is a theoretical edge
// rather than a practical one in FDSDK; a future hardening
// would be to build the shadow tree at probe time too.
func pyprojectShouldUseNative(elem *element) bool {
	// Cache lookup first so the three forced-fallback paths
	// below (which populate the cache before returning) don't
	// print their reason twice per element across the RenderA
	// + RenderB call pair. Cache hits are intentionally silent
	// — see the function-doc above for the once-per-invocation
	// contract.
	if cached, ok := pyprojectProbeCache[elem.Name]; ok {
		return cached.useNative
	}
	if len(elem.Sources) == 0 || elem.Sources[0].AbsPath == "" {
		// No on-disk source tree to probe (kind:remote-asset /
		// kind:tar elements that haven't been resolved by
		// source-checkout). Fall back to pipeline shape — the
		// converter wouldn't be able to read pyproject.toml at
		// action time either if the source wasn't materialized
		// somewhere reachable.
		return cacheAndLogPyprojectFallback(elem.Name, pyprojectFallbackCauseForced, "no on-disk source tree to probe (Sources[0].AbsPath empty)")
	}
	// Multi-source / Directory!="" forced-fallback is owned by
	// pyprojectNativeIncompatible, which runs BEFORE this
	// function in RenderA/RenderB. Both paths used to check the
	// same shapes; we drop the duplicate here so the single-
	// owner diagnostic + cache stays consistent.
	srcRoot := elem.Sources[0].AbsPath
	importsManifestPath := ""
	if len(elem.Deps) > 0 {
		// The probe runs the full Lower(), which checks
		// [project.dependencies] against the imports manifest.
		// Without a manifest, dep-bearing elements would
		// falsely refuse here, even when the native genrule
		// (which gets imports.json staged whenever
		// writePyprojectImportsManifest writes a non-empty
		// manifest — see pyprojectElementBuildA's hasImports
		// branch) would succeed. Render a temp imports.json
		// via the same writer; when it returns wrote=true the
		// probe gets --imports-manifest and matches the
		// genrule, when it returns wrote=false neither path
		// passes a manifest and both see Imports=nil.
		tmp, err := os.MkdirTemp("", "pyproject-probe-imports-*")
		if err != nil {
			return cacheAndLogPyprojectFallback(elem.Name, pyprojectFallbackCauseForced, "imports-manifest staging failed: "+err.Error())
		}
		defer os.RemoveAll(tmp)
		wrote, err := writePyprojectImportsManifest(elem, tmp)
		if err != nil {
			return cacheAndLogPyprojectFallback(elem.Name, pyprojectFallbackCauseForced, "imports-manifest write failed: "+err.Error())
		}
		if wrote {
			importsManifestPath = filepath.Join(tmp, "imports.json")
		}
	}
	out, err := pyprojectProbe(pyprojectConfig.convertBin, srcRoot, importsManifestPath)
	useNative := err == nil
	if !useNative {
		cause, reason := classifyPyprojectProbeFallback(err, strings.TrimSpace(out))
		return cacheAndLogPyprojectFallback(elem.Name, cause, reason)
	}
	pyprojectProbeCache[elem.Name] = pyprojectProbeResult{useNative: true, reason: ""}
	return true
}

// classifyPyprojectProbeFallback maps a non-nil pyproject-probe error (and the
// probe's trimmed stdout, passed as reason) to the fallback cause + an
// operator-facing reason string. The three arms:
//
//   - context.DeadlineExceeded: the probe hung past pyprojectProbeTimeout; the
//     pipeline shape is a safer landing than letting write-a hang.
//   - *exec.ExitError: the probe ran but exited non-zero. Exit 1 is a typed
//     Tier-1 refusal (stderr carries the message verbatim); 64 is a CLI usage
//     error; 65 is any other untyped/Tier-2 error; anything else is a
//     probe/infrastructure problem. Only exit 1 keeps the plain Probe cause —
//     everything else is ProbeUntyped — so operators can tell "the element
//     refuses by the pyproject taxonomy" from "the probe returned an untyped
//     error".
//   - anything else: a genuine spawn failure (binary not executable, wrong
//     arch, ENOENT, signal …); the cause points at the executor, not the
//     converter's behavior.
func classifyPyprojectProbeFallback(err error, reason string) (pyprojectFallbackCause, string) {
	cause := pyprojectFallbackCauseProbe
	if errors.Is(err, context.DeadlineExceeded) {
		cause = pyprojectFallbackCauseProbeTimeout
		deadlineMsg := fmt.Sprintf("probe exceeded %s timeout", pyprojectProbeTimeout)
		if reason == "" {
			reason = deadlineMsg
		} else {
			reason = deadlineMsg + " — partial output: " + reason
		}
	} else if exitErr, ok := err.(*exec.ExitError); ok {
		code := exitErr.ExitCode()
		if code != 1 {
			cause = pyprojectFallbackCauseProbeUntyped
		}
		if reason == "" {
			reason = fmt.Sprintf("probe exited %d with no output", code)
		} else {
			reason = fmt.Sprintf("[exit %d] %s", code, reason)
		}
	} else {
		cause = pyprojectFallbackCauseProbeSpawn
		if reason == "" {
			reason = err.Error()
		} else {
			reason = reason + " (" + err.Error() + ")"
		}
	}
	return cause, reason
}

// cacheAndLogPyprojectFallback memoizes a forced-fallback /
// refused-probe outcome on pyprojectProbeCache and prints the
// per-element refusal diagnostic to stderr in a single, uniform
// shape. Always returns false (the useNative value to bubble out
// to the caller). Centralizing this in one helper means every
// fallback path goes through the same once-per-invocation
// print contract — the cache lookup at the top of
// pyprojectShouldUseNative skips re-printing on subsequent
// RenderA / RenderB calls within the same write-a run.
//
// `cause` distinguishes five scenarios so operators reading
// the log can tell at a glance whether they need to look at the
// converter's Tier-1 taxonomy, the source-checkout / element-
// graph state, or their executor environment:
//
//	pyprojectFallbackCauseProbe        → the probe ran and exited
//	                                     1 (typed Tier-1 refusal).
//	pyprojectFallbackCauseProbeUntyped → the probe ran and exited
//	                                     with a non-Tier-1 code
//	                                     (64 CLI usage / 65 untyped
//	                                     Tier-2 / other). Diagnostic
//	                                     points at the converter's
//	                                     output, not at spawn
//	                                     failure.
//	pyprojectFallbackCauseProbeSpawn   → the probe binary couldn't
//	                                     be spawned at all (ENOENT,
//	                                     wrong arch, missing exec
//	                                     bit, signal during spawn).
//	                                     Diagnostic points at the
//	                                     executor environment.
//	pyprojectFallbackCauseProbeTimeout → the probe exceeded
//	                                     pyprojectProbeTimeout
//	                                     (converter hung / huge
//	                                     tree / filesystem stall);
//	                                     context-cancellation
//	                                     killed it.
//	pyprojectFallbackCauseForced       → we never ran the probe —
//	                                     the element's shape made
//	                                     the probe's view diverge
//	                                     from the genrule's.
//
// `reason` is the operator-facing summary. For probe-ran cases
// (CauseProbe / CauseProbeUntyped) the caller has already
// prefixed `[exit N]` onto the converter's stderr; for spawn
// failures it carries the exec.Error message; for forced
// fallbacks it carries write-a's structural-shape explanation.
// The cache keeps the full text verbatim; the printed
// diagnostic collapses any embedded newlines to " | " so each
// element's fallback line stays grep-friendly even when the
// converter wrote a multi-line stderr (Tier-1 message + hint,
// or flag.Usage() output on exit 64).
func cacheAndLogPyprojectFallback(elemName string, cause pyprojectFallbackCause, reason string) bool {
	pyprojectProbeCache[elemName] = pyprojectProbeResult{useNative: false, reason: reason}
	var verb string
	switch cause {
	case pyprojectFallbackCauseProbeSpawn:
		verb = "probe binary failed to spawn"
	case pyprojectFallbackCauseProbeUntyped:
		verb = "probe exited with untyped error"
	case pyprojectFallbackCauseProbeTimeout:
		verb = "probe timed out"
	case pyprojectFallbackCauseForced:
		verb = "forced pipeline-shape fallback (probe skipped)"
	default:
		verb = "probe refuses native render"
	}
	fmt.Fprintf(os.Stderr, "kind:pyproject %s: %s (%s); falling back to pipeline shape\n", elemName, verb, oneLine(reason))
	return false
}

// oneLine collapses any embedded newlines / carriage returns in
// `s` into " | " separators and trims surrounding whitespace,
// so multi-line converter stderr (e.g. a Tier-1 message
// followed by a hint, or flag.Usage() output) renders as a
// single stderr line in the fallback diagnostic.
func oneLine(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.TrimSpace(s)
	if !strings.Contains(s, "\n") {
		return s
	}
	parts := strings.Split(s, "\n")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, " | ")
}

// pyprojectFallbackCause discriminates the five reasons
// cacheAndLogPyprojectFallback gets called: Forced (probe
// skipped — structural shape mismatch), Probe (ran and Tier-1
// refused with exit 1), ProbeUntyped (ran and exited 64/65/
// other), ProbeSpawn (binary couldn't be spawned at all), and
// ProbeTimeout (probe exceeded pyprojectProbeTimeout).
type pyprojectFallbackCause int

const (
	// pyprojectFallbackCauseForced: we never ran the probe — the
	// element's shape (no on-disk source tree, multi-source,
	// Directory!="", failed imports-manifest staging) means the
	// probe's view would diverge from the genrule's view, so
	// the result couldn't be trusted even if we ran it.
	pyprojectFallbackCauseForced pyprojectFallbackCause = iota
	// pyprojectFallbackCauseProbe: the probe ran and exited
	// 1 — typed Tier-1 refusal. `reason` is the converter's
	// stderr (verbatim Tier-1 message) prefixed with `[exit N]`
	// by the caller so operators see the exit code without
	// having to grep the converter's output.
	pyprojectFallbackCauseProbe
	// pyprojectFallbackCauseProbeUntyped: the probe ran and
	// exited with a non-Tier-1 code (64 CLI usage / 65 untyped
	// Tier-2 / other). The diagnostic verb points at the
	// converter's output rather than at spawn failure so
	// operators don't misdiagnose "the probe binary ran but
	// returned 65" as a spawn issue.
	pyprojectFallbackCauseProbeUntyped
	// pyprojectFallbackCauseProbeSpawn: the probe binary couldn't
	// be spawned at all (ENOENT, wrong arch, missing exec bit,
	// signal during spawn). Distinct from the two CauseProbe
	// variants so the diagnostic points operators at their
	// executor environment instead of at the converter's
	// behavior.
	pyprojectFallbackCauseProbeSpawn
	// pyprojectFallbackCauseProbeTimeout: the probe exceeded
	// pyprojectProbeTimeout (the converter hung — deadlock,
	// pathological source tree, filesystem stall). exec.Cmd's
	// context-cancellation killed the process; we route the
	// element to the pipeline shape rather than letting write-a
	// hang. Distinct cause so the diagnostic verb tells
	// operators it's a hang rather than a refusal.
	pyprojectFallbackCauseProbeTimeout
)

// pyprojectProbeResult is the cached probe outcome. The reason
// text is captured so future code paths can inspect WHY an
// element fell back (the diagnostic itself prints exactly once
// at cache-miss time, see cacheAndLogPyprojectFallback).
type pyprojectProbeResult struct {
	useNative bool
	reason    string
}

// pyprojectProbeCache memoizes probe results by element name.
// See pyprojectShouldUseNative's doc-comment for why the key is
// elem.Name and not the source-tree abspath.
var pyprojectProbeCache = map[string]pyprojectProbeResult{}

// resetPyprojectCaches clears the per-invocation caches that memoize
// pyprojectShouldUseNative + pyprojectNativeIncompatible results.
// Called from writeProjectA (the in-process entrypoint) and at
// --convert-element-pyproject flag-parse time (the CLI entrypoint)
// so the "once per write-a invocation" contract documented on both
// functions holds regardless of how write-a was driven — fresh CLI
// process, in-process test, or library caller invoking writeProjectA
// / writeProjectB across multiple runs.
func resetPyprojectCaches() {
	pyprojectProbeCache = map[string]pyprojectProbeResult{}
	pyprojectStructuralFallback = map[string]bool{}
}

// pyprojectProbe invokes convertBin with --probe + --source-root
// (+ --imports-manifest when importsManifestPath is non-empty)
// against srcRoot and returns (combined stdout+stderr, exec
// error). exec error is nil when the probe exited 0 (native
// render would succeed); non-nil otherwise. Exit-code meanings:
//
//	1   typed Tier-1 refusal (stderr carries the verbatim
//	    pyproject failure message).
//	64  CLI usage error.
//	65  any other untyped/Tier-2 error — filesystem issues,
//	    malformed imports manifest, an unhandled converter
//	    path; not necessarily a bug.
//
// Callers treat any non-zero as "would refuse" and dispatch to
// the pipeline-shape fallback. When the probe ran but exited
// non-zero (any code, including 1 / 64 / 65), the combined
// output is surfaced to write-a's stderr with a `[exit N]`
// prefix and a newline-collapse pass (see
// pyprojectShouldUseNative + oneLine). When the probe couldn't
// be spawned at all (binary missing, wrong arch, signal during
// spawn), there's no exit code to prefix; the exec error itself
// is appended to the diagnostic instead.
//
// The probe runs under a per-element timeout
// (pyprojectProbeTimeout). A converter that hangs (deadlock,
// huge tree, filesystem stall) would otherwise hang write-a
// itself at render time, which is a worse failure mode than a
// build-time refusal — the timeout makes probing fail fast and
// route to the pipeline shape. context.DeadlineExceeded is
// returned distinctly so the caller can label the diagnostic.
func pyprojectProbe(convertBin, srcRoot, importsManifestPath string) (string, error) {
	args := []string{"--probe", "--source-root", srcRoot}
	if importsManifestPath != "" {
		args = append(args, "--imports-manifest", importsManifestPath)
	}
	ctx, cancel := context.WithTimeout(context.Background(), pyprojectProbeTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, convertBin, args...)
	var combined bytes.Buffer
	cmd.Stdout = &combined
	cmd.Stderr = &combined
	err := cmd.Run()
	// Classify a timeout only when the process was actually killed
	// by signal (the SIGKILL that exec.CommandContext sends when
	// ctx fires) AND the context's deadline did expire. Checking
	// ctx.Err() alone is racy at the deadline boundary: a probe
	// that completed normally (zero or non-zero) right before the
	// timer fires would otherwise have its real exit error
	// (including a legitimate *exec.ExitError from a refusal exit
	// code) overwritten with context.DeadlineExceeded once ctx.Err()
	// flips. cmd.ProcessState.Exited() is true when the process
	// terminated via the exit() syscall (any exit code, including
	// the 1/64/65 that the converter returns) and false only when
	// it was killed by signal — pairing the two checks makes the
	// classification race-free.
	if err != nil &&
		cmd.ProcessState != nil &&
		!cmd.ProcessState.Exited() &&
		errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return combined.String(), ctx.Err()
	}
	return combined.String(), err
}

// pyprojectProbeTimeout caps how long a single probe invocation
// can run before write-a gives up and routes the element to the
// pipeline shape. Sized to be much larger than a real probe's
// wall time (each runs the parse/discover/lower pipeline against
// one element's source tree — well under a second on a modest
// dev box; FDSDK's 115 pyproject elements total ~1s of probe
// time) but small enough that a hung probe can't lock up the
// whole write-a render.
const pyprojectProbeTimeout = 30 * time.Second

// pyprojectPipelineHandler returns the pipeline-shape handler
// used when the native path is disabled. Defaults mirror upstream
// buildstream-plugins-community's pyproject.{py,yaml} (see
// docs/architecture.md for the upstream
// snippet), with one shape difference: upstream installs via
// `python -m installer`, this repo's pipeline uses
// `python -m pip install` so existing operator scripts that
// pass extra `--pip-args=...` overrides keep working.
// `dist-dir` defaults to `_bst_dist` to avoid colliding with a
// project's own `./dist/` if its sources already ship one;
// `build-args` carries the default `--wheel --no-isolation` so
// an operator overriding `variables: build-args: ...` in their
// .bst element actually changes the rendered command.
// `installer-args` is kept as an empty no-op even though our
// pip-based install command doesn't consume it: variables.go's
// substituteCmd hard-fails on `%{undefined}` references, and
// existing kind:pyproject elements carried over from upstream
// templates commonly carry `%{installer-args}` references in
// their own commands or overrides. Keeping the default avoids
// breaking those at render time.
func pyprojectPipelineHandler() pipelineHandler {
	return pipelineHandler{
		kindName: "pyproject",
		defaultVars: map[string]string{
			"python":         "python3",
			"pip":            "pip",
			"python-prefix":  "%{prefix}/lib/python3",
			"pip-args":       `--no-build-isolation --no-deps --no-index --target="%{install-root}%{python-prefix}"`,
			"build-args":     "--wheel --no-isolation",
			"installer-args": "",
			"dist-dir":       "_bst_dist",
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
	// Operator-facing dial pass-through. The pyproject converter
	// treats both flags as no-ops today (its "best-effort" path is
	// write-a's --probe + pipeline-shape dispatch, not an internal
	// switch; rejection collection isn't wired). Threading them
	// anyway keeps the per-element cmd byte-identical with what
	// future converter changes will rely on.
	fidelityFlag := fidelityFlagFragment(pyprojectConfig.fidelity)
	diagnosticsFlag := diagnosticsFlagFragment(pyprojectConfig.diagnostics)

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
            --out-build="$(location BUILD.bazel.out)"%[3]s%[4]s%[5]s
    """,
    tools = ["//tools:convert-element-pyproject"],
)

filegroup(
    name = "build_bazel",
    srcs = ["BUILD.bazel.out"],
)
`, elem.Name, srcsList, importsFlag, fidelityFlag, diagnosticsFlag)

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
