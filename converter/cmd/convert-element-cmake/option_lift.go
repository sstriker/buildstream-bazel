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

	"github.com/sstriker/buildstream-bazel/converter/emit/optionsettings"
	"github.com/sstriker/buildstream-bazel/converter/internal/cli"
	"github.com/sstriker/buildstream-bazel/converter/internal/cmakerun"
	"github.com/sstriker/buildstream-bazel/converter/internal/fileapi"
	"github.com/sstriker/buildstream-bazel/converter/internal/lower"
	"github.com/sstriker/buildstream-bazel/converter/ir"
	"github.com/sstriker/buildstream-bazel/internal/sliceutil"
)

// The option lift (--lift-options; the option-lift ROADMAP.md item)
// runs as the same LAUNCH/FINISH pair as the per-config bake: cold
// flip configures — the option's cache value changed via -D — fired
// concurrently with lowering, joined once the final IR is known. A
// BOOL option() gets ONE flip (the inverted value); an enum option
// (STRING cache entry with a STRINGS allowed-value list) gets one
// flip per non-configured value. The fold half is
// lower.ApplyOptionFold (attribute deltas → //options select() arms
// keyed by lower.OptionCellLabel / lower.OptionValueCellLabel) plus
// lower.ApplyContentBakes (option-derived configure_file bodies →
// write_file content select() arms — the per-config bake extended to
// the option axis); the render half reuses the existing PerPlatform
// select() emit unchanged.
//
// Guards, in order, each degrading per option (never failing the
// convert):
//
//   - the option must be a BOOL cache entry or a STRING cache entry
//     with a STRINGS property in the primary reply — else skip+warn;
//   - an enum's configured value must be in its STRINGS list, the
//     list needs >=2 entries, and no two values may sanitize to the
//     same label suffix — else skip + warn;
//   - every flip configure must succeed — else skip + warn with
//     cmake's own output;
//   - the fold must land at least one attribute arm, one
//     target_compatible_with gate, OR one content bake — an option
//     that changes nothing emits no flag (a toggle nobody reads
//     would be noise).
//
// Target existence is expressed, not guarded: a target the primary
// configure declares but a flip value doesn't gains a
// target_compatible_with select() arm pointing at
// @platforms//:incompatible under that arm (lower.
// GateTargetExistence) — builds under that option value skip it. The
// inverse (a target declared ONLY under a flip value) can't be
// emitted from this convert — the primary lower never saw it — and
// surfaces as a breadcrumb suggesting a re-convert with that value
// configured.
//
// Successfully lifted options get their flag + config_settings in
// the --out-option-settings package and move out of the "values
// baked in; re-convert to change" header block
// (lower.AnnotateLiftedOptions).

// optionSpec is one --lift-options entry resolved against the
// primary reply's cache.
type optionSpec struct {
	name      string // cache-entry name, verbatim
	enum      bool
	baseValue string   // configured cache value, verbatim
	baseOn    bool     // BOOL options: cmakeTruthy(baseValue)
	values    []string // enum options: the STRINGS list, verbatim
	baseLabel string   // arm label of the primary configure's cell
}

// cliDefault renders the spec's configured value the way the emitted
// flag accepts it on the CLI: true/false for a bool_flag (the raw
// cache value is ON/OFF, which --//options:<name>= would reject),
// the cache string quoted (%q) for an enum string_flag so values
// with spaces read unambiguously in the breadcrumb.
func (s *optionSpec) cliDefault() string {
	if s.enum {
		return fmt.Sprintf("%q", s.baseValue)
	}
	if s.baseOn {
		return "true"
	}
	return "false"
}

// optionFlip is one in-flight (then completed) flip configure.
// Output is buffered and dumped only on failure, same as
// perConfigResult.
type optionFlip struct {
	spec     int    // index into optionLiftJob.specs
	setValue string // the -D value this flip configures with
	armLabel string // the select() arm this flip's deltas land under
	err      error
	out      bytes.Buffer
	// reply is the flip's loaded File API reply, stored by
	// collectOption for the 2D fold's (config x value) grid.
	reply *fileapi.Reply
}

// optionLiftJob is the handle for the speculatively-launched flip
// configures. A nil job means the pre-gate declined; every method is
// nil-safe.
type optionLiftJob struct {
	buildDir  string
	specs     []optionSpec
	flips     []optionFlip
	idToName  map[string]string // flip replies' codemodel id→name, filled at collect time
	cancel    context.CancelFunc
	rec       *phaseRecorder
	done      chan struct{}
	liftWall  time.Duration
	recOnce   sync.Once
	cleanOnce sync.Once
}

// scratch names flip i's build dir. The index prefix keeps distinct
// flips collision-free even when their sanitized names coincide
// (sanitizeConfigName maps every non-alphanumeric rune to '_', so
// FOO-BAR and FOO_BAR — or two enum values — could otherwise share
// a dir and the concurrent configures would race).
func (j *optionLiftJob) scratch(i int) string {
	return fmt.Sprintf("%s-opt-%d-%s", j.buildDir, i, sanitizeConfigName(j.specs[j.flips[i].spec].name))
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
		for i := range j.flips {
			os.RemoveAll(j.scratch(i))
		}
	})
}

// resolveOptionSpecs maps the --lift-options names onto specs +
// flips via the primary reply's cache, applying every per-option
// pre-gate (BOOL or STRING+STRINGS shape, enum value hygiene,
// lowercased-name collisions).
func resolveOptionSpecs(names []string, cache fileapi.Cache) ([]optionSpec, []optionFlip) {
	var specs []optionSpec
	var flips []optionFlip
	seenLower := map[string]string{}
	for _, name := range names {
		entry := cache.Get(name)
		if entry == nil {
			fmt.Fprintf(os.Stderr, "convert-element-cmake: warning: --lift-options %s: no cache entry in the configure's cache (not an option of this project?); keeping it baked.\n", name)
			continue
		}
		if prev, dup := seenLower[strings.ToLower(name)]; dup {
			fmt.Fprintf(os.Stderr, "convert-element-cmake: warning: --lift-options %s collides with %s after lowercasing; keeping it baked.\n", name, prev)
			continue
		}
		var spec optionSpec
		switch {
		case entry.Type == "BOOL":
			baseOn := cmakeTruthy(entry.Value)
			spec = optionSpec{name: name, baseValue: entry.Value, baseOn: baseOn, baseLabel: lower.OptionCellLabel(name, baseOn)}
		case entry.Type == "STRING":
			values := cacheStringsList(entry)
			if len(values) == 0 {
				fmt.Fprintf(os.Stderr, "convert-element-cmake: warning: --lift-options %s: STRING cache entry without a STRINGS allowed-value list (free-form strings have no finite arm set); keeping it baked.\n", name)
				continue
			}
			if !enumSpecOK(name, entry.Value, values) {
				continue
			}
			spec = optionSpec{name: name, enum: true, baseValue: entry.Value, values: values, baseLabel: lower.OptionValueCellLabel(name, entry.Value)}
		default:
			fmt.Fprintf(os.Stderr, "convert-element-cmake: warning: --lift-options %s: cache type %s isn't liftable (BOOL, or STRING with a STRINGS property); keeping it baked.\n", name, entry.Type)
			continue
		}
		seenLower[strings.ToLower(name)] = name
		specIdx := len(specs)
		specs = append(specs, spec)
		if spec.enum {
			for _, v := range spec.values {
				if v == spec.baseValue {
					continue
				}
				flips = append(flips, optionFlip{spec: specIdx, setValue: v, armLabel: lower.OptionValueCellLabel(name, v)})
			}
		} else {
			flip := "ON"
			if spec.baseOn {
				flip = "OFF"
			}
			flips = append(flips, optionFlip{spec: specIdx, setValue: flip, armLabel: lower.OptionCellLabel(name, !spec.baseOn)})
		}
	}
	return specs, flips
}

// enumSpecOK applies the enum-shape pre-gates: configured value in
// the list, >=2 values, and sanitized-suffix uniqueness (two values
// mapping to one suffix would collide on the config_setting name).
func enumSpecOK(name, baseValue string, values []string) bool {
	if len(values) < 2 {
		fmt.Fprintf(os.Stderr, "convert-element-cmake: warning: --lift-options %s: STRINGS lists %d value(s); nothing to toggle, keeping it baked.\n", name, len(values))
		return false
	}
	inList := false
	suffixes := map[string]string{}
	for _, v := range values {
		if v == baseValue {
			inList = true
		}
		s := lower.SanitizeOptionValue(v)
		if prev, dup := suffixes[s]; dup {
			fmt.Fprintf(os.Stderr, "convert-element-cmake: warning: --lift-options %s: values %q and %q both sanitize to config_setting suffix %q; keeping it baked.\n", name, prev, v, s)
			return false
		}
		suffixes[s] = v
	}
	if !inList {
		fmt.Fprintf(os.Stderr, "convert-element-cmake: warning: --lift-options %s: configured value %q is not in the STRINGS list %v; keeping it baked.\n", name, baseValue, values)
		return false
	}
	return true
}

// cacheStringsList parses a cache entry's STRINGS property
// (cmake's semicolon-joined allowed-value list for enum options).
func cacheStringsList(entry *fileapi.CacheEntry) []string {
	for _, p := range entry.Properties {
		if p.Name != "STRINGS" {
			continue
		}
		var values []string
		for _, v := range strings.Split(p.Value, ";") {
			if v = strings.TrimSpace(v); v != "" {
				values = append(values, v)
			}
		}
		return values
	}
	return nil
}

// startOptionLift resolves each --lift-options name against the
// primary reply's cache and fires the flip configures concurrently
// with the lowering the caller is about to do. Returns nil when
// nothing is liftable. The caller MUST finishOptionLift the job and
// should `defer job.cleanup()` as the early-error safety net.
func startOptionLift(ctx context.Context, a cli.Args, r *fileapi.Reply, hostBuildDir string, rec *phaseRecorder) *optionLiftJob {
	if hostBuildDir == "" || len(a.LiftOptions) == 0 || r == nil {
		return nil
	}
	specs, flips := resolveOptionSpecs(a.LiftOptions, r.Cache)
	if len(flips) == 0 {
		return nil
	}
	liftCtx, cancel := context.WithCancel(ctx)
	job := &optionLiftJob{
		buildDir: hostBuildDir,
		specs:    specs,
		flips:    flips,
		cancel:   cancel,
		rec:      rec,
		done:     make(chan struct{}),
	}
	fmt.Fprintf(os.Stderr, "convert-element-cmake: --lift-options: launching %d flip configure(s) for %d option(s) concurrently with lowering.\n", len(flips), len(specs))
	start := time.Now()
	var wg sync.WaitGroup
	for i := range job.flips {
		wg.Add(1)
		go func(i int, flip *optionFlip) {
			defer wg.Done()
			extra := cmakeDefinesToMap(a.CmakeDefines)
			if extra == nil {
				extra = map[string]string{}
			}
			// The flip wins over an operator --cmake-define pinning the
			// same option — the whole point of the pass is the changed
			// view.
			extra[job.specs[flip.spec].name] = flip.setValue
			// Mirror the primary configure's generator shape: under
			// --build-types the flip must be the same multi-config
			// configure (the 2D fold diffs per (config, value) cell);
			// BuildType and BuildTypes are mutually exclusive in
			// cmakerun, so exactly one is set.
			copts := cmakerun.Options{
				SourceRoot:         a.SourceRoot,
				BuildDir:           job.scratch(i),
				PrefixDir:          a.PrefixDir,
				ToolchainCMakeFile: a.ToolchainCMakeFile,
				ExtraCacheVars:     extra,
				Stdout:             &flip.out,
				Stderr:             &flip.out,
			}
			if len(a.BuildTypes) > 0 {
				copts.BuildTypes = a.BuildTypes
			} else {
				copts.BuildType = a.BuildType
			}
			_, cfgErr := cmakerun.Configure(liftCtx, copts)
			flip.err = cfgErr
		}(i, &job.flips[i])
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

// liftedCells is one option's loaded flip data, ready to fold: the
// per-target cell views (each target under exactly the cells that
// declare it), the write_file bodies read from each flip's scratch
// dir, and the target-existence bookkeeping.
type liftedCells struct {
	cells  []string // baseLabel first, then each flip's arm label
	byCell map[string]map[string]fileapi.Target
	bakes  map[string]map[string][]byte // WriteFileOut rel → arm label → bytes
	// gates maps a base-declared target name → the arm labels it is
	// ABSENT under (an if(<option>) target gate); GateTargetExistence
	// renders these as target_compatible_with select() arms.
	gates map[string][]string
	// flipOnly maps a target name declared ONLY under some flip
	// value(s) → those values. The primary lower never saw it, so it
	// can't be emitted from this convert; surfaced as a breadcrumb
	// (re-convert with that value configured to emit it).
	flipOnly map[string][]string
}

// foldGroup is one presence-signature slice of an option's targets:
// every target in byCell is declared under exactly the same cells.
type foldGroup struct {
	cells  []string
	byCell map[string]map[string]fileapi.Target
}

// foldGroups splits byCell by presence signature: configfold.Project
// treats a missing declared cell as "every fact deltas onto the
// observing cells", so a target absent under some arm must fold over
// ONLY its present cells (its existence there is already expressed
// by the target_compatible_with gate, not by attribute arms).
// Groups are keyed by the cell signature; returned in sorted
// signature order for determinism.
func (lc *liftedCells) foldGroups() []foldGroup {
	bySig := map[string]*foldGroup{}
	var sigs []string
	for name, cells := range lc.byCell {
		// Present-cell list in lc.cells order (base first, then flips).
		var present []string
		for _, c := range lc.cells {
			if _, ok := cells[c]; ok {
				present = append(present, c)
			}
		}
		sig := strings.Join(present, "\x00")
		g, ok := bySig[sig]
		if !ok {
			g = &foldGroup{cells: present, byCell: map[string]map[string]fileapi.Target{}}
			bySig[sig] = g
			sigs = append(sigs, sig)
		}
		g.byCell[name] = cells
	}
	sort.Strings(sigs)
	out := make([]foldGroup, 0, len(bySig))
	for _, sig := range sigs {
		out = append(out, *bySig[sig])
	}
	return out
}

// finishOptionLift joins the flip configures and, per option: loads
// the flip replies, applies the target-set guard, canonicalizes the
// scratch build-dir paths onto the primary build dir, folds the
// attribute deltas into //options select() arms, and folds
// option-derived configure_file bodies into write_file content
// select() arms. Then it writes the --out-option-settings package
// for the options that actually lifted and relocates them in the
// header-comment inventory. A nil job is a clean no-op; every
// per-option failure degrades to the baked value with a stderr
// breadcrumb.
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
	writeFileOuts := packageWriteFileOuts(pkg)
	var lifted []optionsettings.Option
	var groups []optionsettings.Group
	liftedLabels := map[string]string{}
	// The 2D option x config grid needs >=2 non-sanitizer configs in a
	// multi-config reply; otherwise the single-axis fold applies.
	var nonFeatureConfigs []string
	if len(a.BuildTypes) >= 2 && len(r.TargetsByConfig) > 0 {
		var names []string
		for _, cfg := range r.Codemodel.Configurations {
			names = append(names, cfg.Name)
		}
		if nf := lower.NonFeatureConfigNames(names); len(nf) >= 2 {
			nonFeatureConfigs = nf
		}
	}
	for specIdx := range job.specs {
		spec := &job.specs[specIdx]
		lc := job.collectOption(specIdx, r, baseNames, writeFileOuts, hostBuildDir)
		if lc == nil {
			continue
		}
		// Register the option's arm labels under its flag's select
		// family FIRST: two options' arms — or an option arm next to a
		// //config:*/constraint arm — can match simultaneously, so the
		// emitter renders one select() per family, concatenated
		// (lower.RegisterOptionArms; ir.Package.SelectArmFamilies).
		flagLabel := "//options:" + strings.ToLower(spec.name)
		lower.RegisterOptionArms(pkg, flagLabel, spec.baseLabel)
		for i := range job.flips {
			if job.flips[i].spec == specIdx {
				lower.RegisterOptionArms(pkg, flagLabel, job.flips[i].armLabel)
			}
		}
		// Attribute fold. Under a multi-config configure the 2D fold
		// classifies each fact over the (config x option-value) grid —
		// pure-option facts land on //options arms, option x config-
		// conditional facts on config_setting_group AND-arms (and leave
		// the base fold's plain //config arms). Single-config runs keep
		// the presence-signature-grouped single-axis fold: a target
		// absent under some arm folds over only its present cells (its
		// existence there is the gate's job, not attribute arms').
		idToName := replyIDToName(r, job)
		var armed []string
		if nonFeatureConfigs != nil {
			byCell2, valueArms := job.cells2D(specIdx, r, nonFeatureConfigs, hostBuildDir)
			armed2, grps := lower.ApplyOptionFold2D(pkg, byCell2, nonFeatureConfigs, valueArms, srcRoot, hostBuildDir, idToName, flagLabel)
			armed = armed2
			for _, g := range grps {
				groups = append(groups, optionsettings.Group{Name: g.Name, MatchAll: g.MatchAll})
			}
		} else {
			for _, grp := range lc.foldGroups() {
				armed = append(armed, lower.ApplyOptionFold(pkg, grp.byCell, grp.cells, srcRoot, hostBuildDir, idToName)...)
			}
		}
		gated := lower.GateTargetExistence(pkg, lc.gates)
		baked, bakeSkipped := lower.ApplyContentBakes(pkg, lc.bakes, srcRoot, hostBuildDir, a.BazelPackagePath, "cmake-codegen-per-option-content", flagLabel)
		for _, name := range bakeSkipped {
			fmt.Fprintf(os.Stderr, "convert-element-cmake: --lift-options %s: write_file %s already carries content arms from another select family (a content select can't compose families); keeping its existing arms.\n", spec.name, name)
		}
		if len(lc.flipOnly) > 0 {
			names := sliceutil.SortedKeys(lc.flipOnly)
			for _, name := range names {
				values := append([]string(nil), lc.flipOnly[name]...)
				sort.Strings(values)
				for i, v := range values {
					values[i] = fmt.Sprintf("%q", v)
				}
				fmt.Fprintf(os.Stderr, "convert-element-cmake: --lift-options %s: target %s exists only under value(s) %s — the primary configure never declared it, so this convert can't emit it; re-convert with that value configured to include it.\n",
					spec.name, name, strings.Join(values, ", "))
			}
		}
		if len(armed) == 0 && len(gated) == 0 && len(baked) == 0 {
			fmt.Fprintf(os.Stderr, "convert-element-cmake: --lift-options %s: changing it folds no attribute delta, gates no target, and changes no configure_file body; no flag emitted, keeping it baked.\n", spec.name)
			continue
		}
		fmt.Fprintf(os.Stderr, "convert-element-cmake: --lift-options %s: lifted to --//options:%s (default %s); %d target(s) gained select() arms, %d gained target_compatible_with gates, %d write_file body(ies) gained content arms.\n",
			spec.name, strings.ToLower(spec.name), spec.cliDefault(), len(armed), len(gated), len(baked))
		lifted = append(lifted, specOption(spec))
		liftedLabels[spec.name] = flagLabel
	}
	if len(lifted) == 0 {
		return
	}
	lower.AnnotateLiftedOptions(pkg, liftedLabels)
	writeOptionSettings(a.OutOptionSettings, lifted, groups)
}

// collectOption loads one spec's flip replies and reads its
// write_file bodies, tracking per-target cell presence (gates /
// flipOnly). Returns nil (with a stderr breadcrumb) when any flip
// failed or produced no reply.
func (j *optionLiftJob) collectOption(specIdx int, r *fileapi.Reply, baseNames map[string]bool, writeFileOuts []string, hostBuildDir string) *liftedCells {
	spec := &j.specs[specIdx]
	lc := &liftedCells{
		cells:    []string{spec.baseLabel},
		byCell:   map[string]map[string]fileapi.Target{},
		bakes:    map[string]map[string][]byte{},
		gates:    map[string][]string{},
		flipOnly: map[string][]string{},
	}
	for _, t := range r.Targets {
		lc.byCell[t.Name] = map[string]fileapi.Target{spec.baseLabel: t}
	}
	flipCount := 0
	for i := range j.flips {
		flip := &j.flips[i]
		if flip.spec != specIdx {
			continue
		}
		flipCount++
		if flip.err != nil {
			fmt.Fprintf(os.Stderr, "convert-element-cmake: warning: --lift-options %s: flip configure (=%s) failed (%v); keeping it baked. cmake output:\n", spec.name, flip.setValue, flip.err)
			_, _ = os.Stderr.Write(flip.out.Bytes())
			return nil
		}
		scratchDir := j.scratch(i)
		fr, err := fileapi.Load(filepath.Join(scratchDir, ".cmake", "api", "v1", "reply"))
		if err != nil {
			fmt.Fprintf(os.Stderr, "convert-element-cmake: warning: --lift-options %s: flip configure (=%s) produced no loadable File API reply (%v); keeping it baked.\n", spec.name, flip.setValue, err)
			return nil
		}
		flip.reply = fr
		// Target-existence bookkeeping: a target the primary configure
		// declares but this flip value doesn't (an if(<option>) target
		// gate) can't be un-declared by a select() — it gets a
		// target_compatible_with arm pointing at
		// @platforms//:incompatible under this flip's label instead
		// (GateTargetExistence). A target declared ONLY under this flip
		// value was never lowered, so it can't be emitted from this
		// convert — recorded for the flip-only breadcrumb.
		flipNames := configTargetNameSet(fr.Codemodel.Configurations)
		for name := range baseNames {
			if !flipNames[name] {
				lc.gates[name] = append(lc.gates[name], flip.armLabel)
			}
		}
		for name := range flipNames {
			if !baseNames[name] {
				lc.flipOnly[name] = append(lc.flipOnly[name], flip.setValue)
			}
		}
		lc.cells = append(lc.cells, flip.armLabel)
		for _, t := range fr.Targets {
			if !baseNames[t.Name] {
				continue // flip-only: no lowered target to fold onto
			}
			canonicalizeFlipTarget(&t, scratchDir, hostBuildDir)
			if lc.byCell[t.Name] == nil {
				lc.byCell[t.Name] = map[string]fileapi.Target{}
			}
			lc.byCell[t.Name][flip.armLabel] = t
		}
		j.flipIDToName(fr)
		for _, rel := range writeFileOuts {
			body, readErr := os.ReadFile(filepath.Join(scratchDir, filepath.FromSlash(rel)))
			if readErr != nil {
				continue // not produced under this value; coverage check below drops it
			}
			// Canonicalize scratch-dir spellings onto the primary build
			// dir BEFORE the shared content fold: ApplyContentBakes'
			// re-anchor helpers strip the primary dir (and the per-config
			// bake's -cfg-<name> siblings) but know nothing about the
			// option lift's -opt-<i>-<name> dirs, so an uncanonicalized
			// body embedding CMAKE_BINARY_DIR would leak the throwaway
			// path — and fabricate a content select() whose only delta is
			// scratch-dir spelling.
			body = bytes.ReplaceAll(body, []byte(scratchDir), []byte(hostBuildDir))
			if lc.bakes[rel] == nil {
				lc.bakes[rel] = map[string][]byte{}
			}
			lc.bakes[rel][flip.armLabel] = body
		}
	}
	if flipCount == 0 {
		return nil
	}
	// An output missing under SOME flip can't form an honest select —
	// keep its single primary body instead (same coverage rule as the
	// per-config bake).
	for rel, m := range lc.bakes {
		if len(m) != flipCount {
			delete(lc.bakes, rel)
		}
	}
	return lc
}

// cells2D builds one spec's (config x option-value) grid for the 2D
// fold: target NAME → Cell2DKey(config, valueArm) → view. Only
// targets present in EVERY grid cell participate — value-level
// absences are the existence gate's job (collectOption already
// recorded them), and config-level absences are the base multi-config
// fold's pre-existing config-only residual. Flip views canonicalize
// their scratch-dir spellings onto the primary build dir, same as the
// 1D path. Returns the grid plus the value-arm list (base first,
// then each flip's arm in flip order).
func (j *optionLiftJob) cells2D(specIdx int, r *fileapi.Reply, configs []string, hostBuildDir string) (map[string]map[string]fileapi.Target, []string) {
	spec := &j.specs[specIdx]
	valueArms := []string{spec.baseLabel}
	byCell := map[string]map[string]fileapi.Target{}
	add := func(reply *fileapi.Reply, valueArm, scratchDir string) {
		for _, byCfg := range reply.TargetsByConfig {
			for _, cfg := range configs {
				t, ok := byCfg[cfg]
				if !ok {
					continue
				}
				if scratchDir != "" {
					canonicalizeFlipTarget(&t, scratchDir, hostBuildDir)
				}
				if byCell[t.Name] == nil {
					byCell[t.Name] = map[string]fileapi.Target{}
				}
				byCell[t.Name][lower.Cell2DKey(cfg, valueArm)] = t
			}
		}
	}
	add(r, spec.baseLabel, "")
	for i := range j.flips {
		flip := &j.flips[i]
		if flip.spec != specIdx || flip.reply == nil {
			continue
		}
		valueArms = append(valueArms, flip.armLabel)
		add(flip.reply, flip.armLabel, j.scratch(i))
	}
	// Drop partially-present targets: the 2D classifier would read a
	// missing cell as "fact absent" and route every fact of the
	// target onto AND arms.
	want := len(configs) * len(valueArms)
	for name, cells := range byCell {
		if len(cells) != want {
			delete(byCell, name)
		}
	}
	return byCell, valueArms
}

// flipIDToName accumulates a flip reply's id→name pairs into the
// job-level map replyIDToName consults for the deps relabel.
func (j *optionLiftJob) flipIDToName(fr *fileapi.Reply) {
	if j.idToName == nil {
		j.idToName = map[string]string{}
	}
	for _, cfg := range fr.Codemodel.Configurations {
		for _, tr := range cfg.Targets {
			j.idToName[tr.Id] = tr.Name
		}
	}
}

// replyIDToName merges the primary reply's and the collected flip
// replies' codemodel id→name maps for ApplyOptionFold's deps
// relabel.
func replyIDToName(r *fileapi.Reply, j *optionLiftJob) map[string]string {
	out := map[string]string{}
	for _, cfg := range r.Codemodel.Configurations {
		for _, tr := range cfg.Targets {
			out[tr.Id] = tr.Name
		}
	}
	for id, name := range j.idToName {
		out[id] = name
	}
	return out
}

// specOption maps a lifted spec onto its emit/optionsettings entry.
func specOption(spec *optionSpec) optionsettings.Option {
	if spec.enum {
		suffixes := make(map[string]string, len(spec.values))
		for _, v := range spec.values {
			suffixes[v] = lower.SanitizeOptionValue(v)
		}
		return optionsettings.Option{Name: spec.name, Default: spec.baseValue, Values: spec.values, ValueSuffixes: suffixes}
	}
	def := "False"
	if spec.baseOn {
		def = "True"
	}
	return optionsettings.Option{Name: spec.name, Default: def}
}

// writeOptionSettings emits the //options package to outPath (no-op
// when the flag is unset). Failures warn rather than failing the
// convert — the BUILD itself is already correct.
func writeOptionSettings(outPath string, lifted []optionsettings.Option, groups []optionsettings.Group) {
	if outPath == "" {
		return
	}
	body := optionsettings.Emit(lifted, groups)
	if body == nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "convert-element-cmake: warning: stage option-settings dir: %v\n", err)
		return
	}
	if err := os.WriteFile(outPath, body, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "convert-element-cmake: warning: write option-settings: %v\n", err)
	}
}

// packageWriteFileOuts collects the build-dir-relative output paths
// of the package's write_file targets — the read-back set for the
// per-option content bake.
func packageWriteFileOuts(pkg *ir.Package) []string {
	var outs []string
	for i := range pkg.Targets {
		if pkg.Targets[i].Kind == ir.KindWriteFile && pkg.Targets[i].WriteFileOut != "" {
			outs = append(outs, pkg.Targets[i].WriteFileOut)
		}
	}
	return outs
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
