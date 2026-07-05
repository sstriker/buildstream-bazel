package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/sstriker/buildstream-bazel/converter/emit/optionsettings"
	"github.com/sstriker/buildstream-bazel/converter/internal/cli"
	"github.com/sstriker/buildstream-bazel/converter/internal/cmakerun"
	"github.com/sstriker/buildstream-bazel/converter/internal/fileapi"
	"github.com/sstriker/buildstream-bazel/converter/internal/lower"
	"github.com/sstriker/buildstream-bazel/converter/ir"
)

// The option lift (--lift-options; stages a+b of the option-lift
// ROADMAP.md item) runs as the same LAUNCH/FINISH pair as the
// per-config bake: one cold flip configure per lifted option — the
// option's BOOL cache value inverted via -D — fired concurrently
// with lowering, joined once the final IR is known. The fold half is
// lower.ApplyOptionFold (attribute deltas → //options:<name>_{on,off}
// select() arms); the render half reuses the existing PerPlatform
// select() emit unchanged.
//
// Guards, in order, each degrading per option (never failing the
// convert):
//
//   - the option must be a BOOL cache entry in the primary reply
//     (an option() / bool cache declaration) — else skip + warn;
//   - the flip configure must succeed — else skip + warn with
//     cmake's own output;
//   - the flip reply's TARGET NAME SET must equal the primary's —
//     an option gating whole targets (if(FOO) add_executable(…))
//     can't fold into attribute select()s (that's the roadmap's
//     stage c: target-existence gating), so the option stays baked
//     with a breadcrumb;
//   - the fold must land at least one arm — a flip that changes
//     nothing attribute-shaped emits no flag (a toggle nobody reads
//     would be noise).
//
// Successfully lifted options get their bool_flag + config_setting
// pair in the --out-option-settings package and move out of the
// "values baked in; re-convert to change" header block
// (lower.AnnotateLiftedOptions).

// optionLiftResult is one option's in-flight (then completed) flip
// configure. Output is buffered and dumped only on failure, same as
// perConfigResult.
type optionLiftResult struct {
	name   string // cmake option() cache-entry name, verbatim
	baseOn bool   // the primary configure's resolved value
	err    error
	out    bytes.Buffer
}

// optionLiftJob is the handle for the speculatively-launched flip
// configures. A nil job means the pre-gate declined; every method is
// nil-safe.
type optionLiftJob struct {
	buildDir  string
	results   []optionLiftResult
	cancel    context.CancelFunc
	rec       *phaseRecorder
	done      chan struct{}
	liftWall  time.Duration
	recOnce   sync.Once
	cleanOnce sync.Once
}

func (j *optionLiftJob) scratch(option string) string {
	return j.buildDir + "-opt-" + sanitizeConfigName(option)
}

func (j *optionLiftJob) join() {
	if j == nil {
		return
	}
	<-j.done
	j.recOnce.Do(func() { j.rec.add(phaseOptionLift, j.liftWall) })
}

func (j *optionLiftJob) cleanup() {
	if j == nil {
		return
	}
	j.cleanOnce.Do(func() {
		if j.cancel != nil {
			j.cancel()
		}
		j.join()
		for i := range j.results {
			os.RemoveAll(j.scratch(j.results[i].name))
		}
	})
}

// startOptionLift resolves each --lift-options name against the
// primary reply's cache and fires one flip configure per resolvable
// option, concurrently with the lowering the caller is about to do.
// Returns nil when nothing is liftable. The caller MUST
// finishOptionLift the job and should `defer job.cleanup()` as the
// early-error safety net.
func startOptionLift(ctx context.Context, a cli.Args, r *fileapi.Reply, hostBuildDir string, rec *phaseRecorder) *optionLiftJob {
	if hostBuildDir == "" || len(a.LiftOptions) == 0 || r == nil {
		return nil
	}
	var specs []optionLiftResult
	seenLower := map[string]string{}
	for _, name := range a.LiftOptions {
		entry := r.Cache.Get(name)
		if entry == nil || entry.Type != "BOOL" {
			fmt.Fprintf(os.Stderr, "convert-element-cmake: warning: --lift-options %s: no BOOL cache entry in the configure's cache (not an option() of this project?); keeping it baked.\n", name)
			continue
		}
		// The //options labels are lowercased; two spellings colliding
		// there would emit one flag for two distinct cache entries.
		if prev, dup := seenLower[strings.ToLower(name)]; dup {
			fmt.Fprintf(os.Stderr, "convert-element-cmake: warning: --lift-options %s collides with %s after lowercasing; keeping it baked.\n", name, prev)
			continue
		}
		seenLower[strings.ToLower(name)] = name
		specs = append(specs, optionLiftResult{name: name, baseOn: cmakeTruthy(entry.Value)})
	}
	if len(specs) == 0 {
		return nil
	}
	liftCtx, cancel := context.WithCancel(ctx)
	job := &optionLiftJob{
		buildDir: hostBuildDir,
		results:  specs,
		cancel:   cancel,
		rec:      rec,
		done:     make(chan struct{}),
	}
	fmt.Fprintf(os.Stderr, "convert-element-cmake: --lift-options: launching %d flip configure(s) concurrently with lowering.\n", len(specs))
	start := time.Now()
	var wg sync.WaitGroup
	for i := range job.results {
		wg.Add(1)
		go func(res *optionLiftResult) {
			defer wg.Done()
			flip := "ON"
			if res.baseOn {
				flip = "OFF"
			}
			extra := cmakeDefinesToMap(a.CmakeDefines)
			if extra == nil {
				extra = map[string]string{}
			}
			// The flip wins over an operator --cmake-define pinning the
			// same option — the whole point of the pass is the inverted
			// view.
			extra[res.name] = flip
			_, cfgErr := cmakerun.Configure(liftCtx, cmakerun.Options{
				SourceRoot:         a.SourceRoot,
				BuildDir:           job.scratch(res.name),
				PrefixDir:          a.PrefixDir,
				ToolchainCMakeFile: a.ToolchainCMakeFile,
				BuildType:          a.BuildType,
				ExtraCacheVars:     extra,
				Stdout:             &res.out,
				Stderr:             &res.out,
			})
			res.err = cfgErr
		}(&job.results[i])
	}
	go func() {
		wg.Wait()
		job.liftWall = time.Since(start)
		close(job.done)
	}()
	return job
}

// finishSpeculativeConfigures is the joint finish half of the two
// speculative-configure jobs runLowerPasses launches before lowering:
// the per-config bake fold (finishPerConfigBakes) then the option
// lift fold (finishOptionLift). Order is arbitrary — the jobs are
// independent (mutually exclusive today: bakes need --build-types,
// the lift rejects it) — but folding both before emit is not.
func finishSpeculativeConfigures(bakeJob *perConfigBakeJob, optJob *optionLiftJob, a cli.Args, hostBuildDir string, r *fileapi.Reply, pkg *ir.Package) {
	finishPerConfigBakes(bakeJob, a, hostBuildDir, pkg)
	finishOptionLift(optJob, a, hostBuildDir, r, pkg)
}

// finishOptionLift joins the flip configures and, per option: loads
// the flip reply, applies the target-set guard, canonicalizes the
// scratch build-dir paths onto the primary build dir, and folds the
// attribute deltas into //options select() arms. Then it writes the
// --out-option-settings package for the options that actually
// lifted and relocates them in the header-comment inventory. A nil
// job is a clean no-op; every per-option failure degrades to the
// baked value with a stderr breadcrumb.
func finishOptionLift(job *optionLiftJob, a cli.Args, hostBuildDir string, r *fileapi.Reply, pkg *ir.Package) {
	if job == nil {
		return
	}
	defer job.cleanup()
	if pkg == nil || r == nil {
		return
	}
	job.join()

	srcRoot := absSourceRoot(a.SourceRoot)
	baseNames := configTargetNameSet(r.Codemodel.Configurations)
	var lifted []optionsettings.Option
	liftedLabels := map[string]string{}
	for i := range job.results {
		res := &job.results[i]
		if res.err != nil {
			fmt.Fprintf(os.Stderr, "convert-element-cmake: warning: --lift-options %s: flip configure failed (%v); keeping it baked. cmake output:\n", res.name, res.err)
			_, _ = os.Stderr.Write(res.out.Bytes())
			continue
		}
		scratchDir := job.scratch(res.name)
		fr, err := fileapi.Load(filepath.Join(scratchDir, ".cmake", "api", "v1", "reply"))
		if err != nil {
			fmt.Fprintf(os.Stderr, "convert-element-cmake: warning: --lift-options %s: flip configure produced no loadable File API reply (%v); keeping it baked.\n", res.name, err)
			continue
		}
		// Target-set guard: an option that adds/removes targets can't
		// fold into attribute select()s — that's target-existence
		// gating (the option-lift roadmap item's stage c).
		flipNames := configTargetNameSet(fr.Codemodel.Configurations)
		if !stringSetsEqual(baseNames, flipNames) {
			fmt.Fprintf(os.Stderr, "convert-element-cmake: --lift-options %s: flipping changes the target set (%d -> %d targets); attribute select()s can't express conditional target existence, keeping it baked.\n", res.name, len(baseNames), len(flipNames))
			continue
		}
		baseLabel := lower.OptionCellLabel(res.name, res.baseOn)
		flipLabel := lower.OptionCellLabel(res.name, !res.baseOn)
		byCell := map[string]map[string]fileapi.Target{}
		for _, t := range r.Targets {
			byCell[t.Name] = map[string]fileapi.Target{baseLabel: t}
		}
		for _, t := range fr.Targets {
			canonicalizeFlipTarget(&t, scratchDir, hostBuildDir)
			if byCell[t.Name] == nil {
				byCell[t.Name] = map[string]fileapi.Target{}
			}
			byCell[t.Name][flipLabel] = t
		}
		idToName := map[string]string{}
		for _, cfgs := range [][]fileapi.Configuration{r.Codemodel.Configurations, fr.Codemodel.Configurations} {
			for _, cfg := range cfgs {
				for _, tr := range cfg.Targets {
					idToName[tr.Id] = tr.Name
				}
			}
		}
		armed := lower.ApplyOptionFold(pkg, byCell, []string{baseLabel, flipLabel}, srcRoot, hostBuildDir, idToName)
		if len(armed) == 0 {
			fmt.Fprintf(os.Stderr, "convert-element-cmake: --lift-options %s: flipping changes no foldable attribute; no flag emitted, keeping it baked.\n", res.name)
			continue
		}
		fmt.Fprintf(os.Stderr, "convert-element-cmake: --lift-options %s: lifted to --//options:%s (default %s); %d target(s) gained select() arms: %s\n",
			res.name, strings.ToLower(res.name), map[bool]string{true: "true", false: "false"}[res.baseOn], len(armed), strings.Join(armed, ", "))
		lifted = append(lifted, optionsettings.Option{Name: res.name, Default: res.baseOn})
		liftedLabels[res.name] = "//options:" + strings.ToLower(res.name)
	}
	if len(lifted) == 0 {
		return
	}
	lower.AnnotateLiftedOptions(pkg, liftedLabels)
	if a.OutOptionSettings != "" {
		if body := optionsettings.Emit(lifted); body != nil {
			if err := os.MkdirAll(filepath.Dir(a.OutOptionSettings), 0o755); err != nil {
				fmt.Fprintf(os.Stderr, "convert-element-cmake: warning: stage option-settings dir: %v\n", err)
				return
			}
			if err := os.WriteFile(a.OutOptionSettings, body, 0o644); err != nil {
				fmt.Fprintf(os.Stderr, "convert-element-cmake: warning: write option-settings: %v\n", err)
			}
		}
	}
}

// configTargetNameSet collects the target NAME set of a codemodel's
// primary configuration. Names (not ids) so the comparison is
// stable across independently-configured build dirs.
func configTargetNameSet(configs []fileapi.Configuration) map[string]bool {
	names := map[string]bool{}
	if len(configs) == 0 {
		return names
	}
	for _, tr := range configs[0].Targets {
		names[tr.Name] = true
	}
	return names
}

func stringSetsEqual(a, b map[string]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if !b[k] {
			return false
		}
	}
	return true
}

// canonicalizeFlipTarget rewrites the flip configure's scratch
// build-dir spellings onto the primary build dir in every string
// field the fold reads, so a fact differing ONLY by build-dir
// prefix (a generated-header -I, a build-dir-anchored link path)
// partitions as baseline instead of a spurious per-option delta —
// the option-axis instance of the reply-path canonicalization the
// multi-platform fold's design notes call out.
func canonicalizeFlipTarget(t *fileapi.Target, scratchDir, hostDir string) {
	if scratchDir == "" || hostDir == "" || scratchDir == hostDir {
		return
	}
	swap := func(s string) string { return strings.ReplaceAll(s, scratchDir, hostDir) }
	for i := range t.Sources {
		t.Sources[i].Path = swap(t.Sources[i].Path)
	}
	for gi := range t.CompileGroups {
		cg := &t.CompileGroups[gi]
		for i := range cg.Includes {
			cg.Includes[i].Path = swap(cg.Includes[i].Path)
		}
		for i := range cg.Defines {
			cg.Defines[i].Define = swap(cg.Defines[i].Define)
		}
		for i := range cg.CompileCommandFragments {
			cg.CompileCommandFragments[i].Fragment = swap(cg.CompileCommandFragments[i].Fragment)
		}
	}
	if t.Link != nil {
		for i := range t.Link.CommandFragments {
			t.Link.CommandFragments[i].Fragment = swap(t.Link.CommandFragments[i].Fragment)
		}
	}
}

// cmakeTruthy applies cmake's boolean-constant rule to a BOOL cache
// value: false is 0 / OFF / NO / FALSE / N / IGNORE / NOTFOUND /
// *-NOTFOUND / empty (case-insensitive); everything else is true.
func cmakeTruthy(v string) bool {
	switch strings.ToUpper(strings.TrimSpace(v)) {
	case "", "0", "OFF", "NO", "FALSE", "N", "IGNORE", "NOTFOUND":
		return false
	}
	return !strings.HasSuffix(strings.ToUpper(v), "-NOTFOUND")
}
