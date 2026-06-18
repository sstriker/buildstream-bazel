package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/sstriker/buildstream-bazel/converter/internal/cli"
	"github.com/sstriker/buildstream-bazel/converter/internal/cmakerun"
	"github.com/sstriker/buildstream-bazel/converter/internal/lower"
	"github.com/sstriker/buildstream-bazel/converter/ir"
	"github.com/sstriker/buildstream-bazel/internal/convmode"
	"github.com/sstriker/buildstream-bazel/internal/shadow"
)

// The per-build-type configure_file bake (--per-config-bake; the fold half is
// lower.ApplyPerConfigBakes, the render half emit's write_file content
// select()) runs as a LAUNCH/FINISH pair so the cold per-config configures —
// the conversion-latency profile's largest single block on
// CMAKE_BUILD_TYPE-reading projects, a full cold configure each — overlap
// pass-1 lowering and the warm pass instead of running serially after them.
//
// startPerConfigBakes fires them as soon as the fresh configure's trace is in
// hand (everything the trigger needs EXCEPT the post-lowering
// write_file-presence check); finishPerConfigBakes joins them once the final IR
// is known, applies that last gate, reads back the per-config bodies, and folds
// differing ones into content select() arms.
//
// Trigger: multi-config mode (≥2 --build-types), a live build dir
// (source-root mode), ≥1 recovered write_file bake, and — under the default
// "auto" — the trace showing the project's OWN files consulting
// CMAKE_BUILD_TYPE (shadow.ReadsBuildType); "on" skips the detection, "off"
// disables. The passes are COLD (one fresh single-config configure per build
// type into a sibling scratch dir): the multi-config build dir can't be
// reused warm because cmake refuses a generator switch in an existing build
// dir, and CMAKE_BUILD_TYPE-derived logic only runs under a single-config
// generator at all.
//
// Every failure path degrades to the pass-1 result (the multi-config body
// for all arms) with a stderr warning — exactly the output the convert
// produces without the feature.

// perConfigResult is one config's in-flight (then completed) cold configure.
// Its output is buffered (concurrent streaming to stderr would interleave into
// garbage) and dumped ONLY on failure — success stays silent, failure keeps
// cmake's own diagnostic alongside the Go-level error.
type perConfigResult struct {
	cfg string
	err error
	out bytes.Buffer
}

// perConfigBakeJob is the handle for the speculatively-launched per-config
// configures. A nil job means the cheap pre-gate declined (the common case);
// every method is nil-safe so finish/cleanup needn't guard.
type perConfigBakeJob struct {
	buildDir  string
	cfgs      []string
	results   []perConfigResult
	cancel    context.CancelFunc
	rec       *phaseRecorder
	done      chan struct{} // closed by the watcher once every configure returns
	bakeWall  time.Duration // launch → last-configure-done, set before done closes
	recOnce   sync.Once
	cleanOnce sync.Once
}

func (j *perConfigBakeJob) scratch(cfg string) string {
	return j.buildDir + "-cfg-" + sanitizeConfigName(cfg)
}

// join blocks until every per-config configure finishes and records the
// fan-out's true wall span once. The span is measured launch → last configure
// done by the watcher goroutine — NOT launch → join — so the overlapped
// lowering time the caller spends before joining is excluded: the bucket
// reflects the configures' own subprocess wall (≈ max(individual), since they
// run concurrently), which is exactly the headroom-vs-lowering signal.
func (j *perConfigBakeJob) join() {
	if j == nil {
		return
	}
	<-j.done
	j.recOnce.Do(func() { j.rec.add(phasePerConfigBake, j.bakeWall) })
}

// cleanup cancels any still-running configures, waits them out (never RemoveAll
// a dir a goroutine is mid-write on), and removes the scratch dirs. Idempotent
// and nil-safe so it doubles as the early-error-return safety net.
func (j *perConfigBakeJob) cleanup() {
	if j == nil {
		return
	}
	j.cleanOnce.Do(func() {
		if j.cancel != nil {
			j.cancel()
		}
		j.join()
		for _, cfg := range j.cfgs {
			os.RemoveAll(j.scratch(cfg))
		}
	})
}

// startPerConfigBakes applies the cheap pre-gate — every per-config bake
// condition EXCEPT the write_file-presence check, which can't run until pass-1
// lowering has produced the IR — and, if it passes, fires the cold
// single-config configures concurrently. Returns nil when the pre-gate
// declines. The caller MUST finishPerConfigBakes the job (which cleans up), and
// should `defer job.cleanup()` as a safety net for early error returns.
func startPerConfigBakes(ctx context.Context, a cli.Args, hostBuildDir string, traceRaw []byte, rec *phaseRecorder) *perConfigBakeJob {
	if hostBuildDir == "" || len(a.BuildTypes) < 2 {
		return nil
	}
	// a.PerConfigBake is canonicalized by cli.Parse (parseValidate →
	// applyOperatorDials); re-parse here so a direct caller that bypassed Parse
	// still gets the alias handling.
	perConfigBake, _ := convmode.ParsePerConfigBake(a.PerConfigBake)
	switch perConfigBake {
	case convmode.PerConfigBakeOff:
		return nil
	case convmode.PerConfigBakeOn:
	default: // auto
		if len(traceRaw) == 0 || !shadow.ReadsBuildType(traceRaw, absSourceRoot(a.SourceRoot)) {
			return nil
		}
	}
	bakeCtx, cancel := context.WithCancel(ctx)
	job := &perConfigBakeJob{
		buildDir: hostBuildDir,
		cfgs:     a.BuildTypes,
		results:  make([]perConfigResult, len(a.BuildTypes)),
		cancel:   cancel,
		rec:      rec,
		done:     make(chan struct{}),
	}
	fmt.Fprintf(os.Stderr, "convert-element-cmake: multi-config + project consults CMAKE_BUILD_TYPE; launching %d per-config configure(s) concurrently with lowering to capture build-type-dependent configure_file bodies.\n", len(a.BuildTypes))
	start := time.Now()
	var wg sync.WaitGroup
	for i, cfg := range a.BuildTypes {
		job.results[i].cfg = cfg
		wg.Add(1)
		go func(i int, cfg string) {
			defer wg.Done()
			_, cfgErr := cmakerun.Configure(bakeCtx, cmakerun.Options{
				SourceRoot:         a.SourceRoot,
				BuildDir:           job.scratch(cfg),
				PrefixDir:          a.PrefixDir,
				ToolchainCMakeFile: a.ToolchainCMakeFile,
				BuildType:          cfg,
				ExtraCacheVars:     cmakeDefinesToMap(a.CmakeDefines),
				Stdout:             &job.results[i].out,
				Stderr:             &job.results[i].out,
			})
			job.results[i].err = cfgErr
		}(i, cfg)
	}
	// Watcher: capture the true fan-out wall (launch → last configure done)
	// independent of when the caller joins, then publish via the done channel
	// (its close happens-before any join's read of bakeWall/results).
	go func() {
		wg.Wait()
		job.bakeWall = time.Since(start)
		close(job.done)
	}()
	return job
}

// finishPerConfigBakes joins the speculative configures, applies the
// write_file-presence gate the launch couldn't, reads back each config's bodies,
// and folds differing bodies into content select() arms. A nil job (pre-gate
// declined) or a final IR with no write_file bakes is a clean no-op; the
// scratch dirs are cleaned either way.
func finishPerConfigBakes(job *perConfigBakeJob, a cli.Args, hostBuildDir string, pkg *ir.Package) {
	if job == nil {
		return
	}
	defer job.cleanup()
	if pkg == nil {
		return
	}
	var outs []string
	for i := range pkg.Targets {
		if pkg.Targets[i].Kind == ir.KindWriteFile && pkg.Targets[i].WriteFileOut != "" {
			outs = append(outs, pkg.Targets[i].WriteFileOut)
		}
	}
	if len(outs) == 0 {
		// No write_file bake to fold: the speculative configures had nothing
		// to capture. They overlapped lowering, so no wall was lost — cleanup
		// cancels whatever is still in flight.
		return
	}
	job.join()
	for i := range job.results {
		if job.results[i].err != nil {
			fmt.Fprintf(os.Stderr, "convert-element-cmake: warning: per-config configure (%s) failed (%v); keeping the multi-config body for all arms. cmake output:\n", job.results[i].cfg, job.results[i].err)
			_, _ = os.Stderr.Write(job.results[i].out.Bytes())
			return
		}
	}
	// Trace event paths are absolute; --source-root may be given relative
	// (the doc'd convention is absolute, but nothing normalizes it), so
	// absolutize before the re-anchor or scratch-dir spellings never match.
	srcRoot := absSourceRoot(a.SourceRoot)
	bakes := map[string]map[string][]byte{}
	for _, cfg := range a.BuildTypes {
		cfgDir := job.scratch(cfg)
		for _, rel := range outs {
			body, readErr := os.ReadFile(filepath.Join(cfgDir, filepath.FromSlash(rel)))
			if readErr != nil {
				// Not produced under this config (config-gated
				// configure_file); per-output coverage check below drops it.
				continue
			}
			if bakes[rel] == nil {
				bakes[rel] = map[string][]byte{}
			}
			bakes[rel][cfg] = body
		}
	}
	// An output missing under SOME config can't form an honest select — keep
	// its single multi-config body instead.
	for rel, m := range bakes {
		if len(m) != len(a.BuildTypes) {
			delete(bakes, rel)
		}
	}
	if applied := lower.ApplyPerConfigBakes(pkg, bakes, srcRoot, hostBuildDir, a.BazelPackagePath); len(applied) > 0 {
		sort.Strings(applied)
		fmt.Fprintf(os.Stderr, "convert-element-cmake: per-config bake: %d write_file body(ies) differ across build types; emitted content select() arms: %s\n", len(applied), strings.Join(applied, ", "))
	}
}

// absSourceRoot absolutizes --source-root (the doc'd convention is absolute,
// but nothing normalizes it), falling back to the input on error.
func absSourceRoot(srcRoot string) string {
	if abs, err := filepath.Abs(srcRoot); err == nil {
		return abs
	}
	return srcRoot
}

// sanitizeConfigName maps a cmake config name onto a filesystem-safe
// scratch-dir suffix.
func sanitizeConfigName(cfg string) string {
	var b strings.Builder
	for _, r := range cfg {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}
