package main

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
)

// pipelineDefaults is the per-kind default phase command set. Each
// "coarse-grained install pipeline" kind (manual, make, autotools,
// pyproject, …) is a pipelineHandler instance with its own defaults;
// .bst-supplied commands override per phase.
//
// nil vs empty-list semantics: a kind that supplies a default for a
// phase but the .bst doesn't override gets the default; a .bst that
// explicitly sets `phase-commands: []` gets nothing for that phase.
// pipelineCfg uses pointer-to-slice fields to distinguish.
type pipelineDefaults struct {
	Configure []string
	Build     []string
	Install   []string
	Strip     []string
}

// pipelineHandler is the generic coarse-grained "install pipeline"
// handler implementation. Its identity (kindName), default phase
// commands, and default per-kind variables come from the registered
// instance; everything else — source staging, BUILD rendering,
// project-B placeholder — is shared.
//
// Concretely, a single source file per kind looks like:
//
//	func init() {
//	    registerHandler(pipelineHandler{
//	        kindName: "make",
//	        defaultVars: map[string]string{
//	            "make-args":         "",
//	            "make-install-args": `DESTDIR="%{install-root}" install`,
//	        },
//	        defaults: pipelineDefaults{
//	            Build:   []string{"make %{make-args}"},
//	            Install: []string{"make -j1 %{make-install-args}"},
//	        },
//	    })
//	}
//
// The element's `variables:` block overrides defaultVars per
// element; project-level defaults sit one layer below (see
// variables.go).
type pipelineHandler struct {
	kindName    string
	defaultVars map[string]string
	defaults    pipelineDefaults

	// extension is an optional hook the kind can install to
	// transform the rendered pipeline cmd into a wider shape.
	// Used by the trace-driven kind:autotools path: it wraps
	// the configure/build/install commands in build-tracer and
	// appends a convert-element-trace step that emits
	// BUILD.bazel.out alongside install_tree.tar.
	//
	// Nil = no transformation; the existing single-genrule
	// install_tree.tar shape renders unchanged.
	extension *pipelineExtension

	// traceDrivenSrckeyPatterns: when non-nil AND the trace-driven
	// CLI is configured (traceConfig.convertBin set +
	// traceConfig.round2Enabled true), this kind opts into
	// the round-2 trace-driven shape. Project A hosts a
	// per-element converter genrule consuming @trace_<elem>//:trace
	// (via cmd/trace-lookup at Bazel load time); project B hosts
	// the coarse install genrule wrapped in build-tracer with an
	// inline cmd/trace-publish step. The patterns drive srckey
	// computation — paths matching include rules contribute their
	// CONTENT bytes to srckey; everything else contributes by
	// path only (matches autotoolsSrckeyPatterns' semantics —
	// see srckey.go and handler_autotools_native.go).
	//
	// Default (nil) keeps the legacy install-genrule-in-A shape
	// from pipelineHandler.RenderA. kind:autotools doesn't go
	// through this field — it has its own autotoolsHandler
	// wrapping pipelineHandler with a more complex round-1 vs
	// round-2 dispatch (see handler_autotools_native.go).
	traceDrivenSrckeyPatterns *readPathsPatterns
}

// pipelineExtension is the small surface a pipeline-kind
// extension uses to widen the rendered genrule. Each field is
// optional; the empty value leaves rendering unchanged.
//
//   - WrapPipelineCmds: rewrites the resolved configure/build/
//     install/strip commands block. The string passed in is the
//     already-rendered shell snippet (with comments and the
//     `# === <phase> ===` markers); the returned string replaces
//     it. Used to inject a tracer wrapper around the build.
//   - AppendCmd: shell snippet inserted between the pipeline
//     commands and the `tar -cf install_tree.tar ...` step.
//     Used to run convert-element-trace against the
//     trace before the install tree is tarred.
//   - ExtraOuts: additional Bazel `outs` filenames the genrule
//     produces (e.g. "BUILD.bazel.out").
//   - ExtraTools: additional `//tools:X` labels the genrule
//     depends on (e.g. "//tools:build-tracer",
//     "//tools:convert-element-trace").
type pipelineExtension struct {
	WrapPipelineCmds func(cmds string) string
	AppendCmd        string
	ExtraSrcs        []string // extra entries in the genrule's srcs (e.g. ["imports.json"])
	ExtraOuts        []string
	ExtraTools       []string

	// DepLabels lists Bazel labels (typically per-file
	// outputs of upstream `<dep>_install` genrules, e.g.
	// `//elements/foo:install_tree.tar`) added to the
	// install genrule's srcs. Used by kinds whose build
	// pipeline consumes upstream install trees — autotools'
	// configure / make need dep .h / .a from upstream
	// elements.
	DepLabels []string

	// DepExtractCmd is a shell snippet spliced into the
	// install genrule's cmd, between the source-tree staging
	// step and the user-provided pipeline cmds. Sets up
	// $DEP_PREFIX with extracted dep install trees and
	// exports build flags (CPPFLAGS / LDFLAGS for autotools)
	// so the pipeline can find the deps' headers and
	// libraries. No-op when DepLabels is empty.
	DepExtractCmd string

	// Multi-platform install-genrule knobs. All three are zero-
	// valued in the single-platform legacy path (preserving byte-
	// stable goldens) and populated by the project-B per-platform
	// fan-out — one pipelineExtension per (element, platform)
	// cell.
	//
	//   - OutputPrefix prefixes every declared output (install_tree.tar
	//     and ExtraOuts) with "<platform>/" so the N per-platform
	//     genrules don't collide on output paths. Cmd-side
	//     $(location <out>) references compose with the prefix so
	//     the genrule's shell sees the correct exec-root-relative
	//     path at action time.
	//
	//   - NameSuffix appends to the genrule's name (e.g.
	//     "<elem>_install_linux_x86_64"). Required when N install
	//     genrules coexist in one BUILD.bazel.
	//
	//   - ExecCompatibleWith is the constraint_value label set
	//     passed through to the genrule's exec_compatible_with
	//     attribute. Routes the action to a matching executor
	//     pool so the linux build doesn't try to run on a darwin
	//     worker.
	OutputPrefix       string
	NameSuffix         string
	ExecCompatibleWith []string
}

func (h pipelineHandler) Kind() string                                 { return h.kindName }
func (h pipelineHandler) NeedsSources() bool                           { return true }
func (h pipelineHandler) HasProjectABuild() bool                       { return true }
func (h pipelineHandler) DefaultReadPathsPatterns() *readPathsPatterns { return nil }

// pipelineCfg is the .bst `config:` block shape every pipeline-kind
// element shares. Pointer-to-slice so the renderer can distinguish
// "not set in .bst, fall back to handler defaults" (nil) from
// "explicitly cleared in .bst" (non-nil empty slice).
//
// Commands is the kind:script shape: a single flat list of
// shell commands run in order. When set, it takes the place of
// install-commands (and the other phases stay empty); kind:script
// is the only kind that reads it. Mutually exclusive with the
// per-phase fields per BuildStream's contract.
type pipelineCfg struct {
	ConfigureCommands *[]string `yaml:"configure-commands"`
	BuildCommands     *[]string `yaml:"build-commands"`
	InstallCommands   *[]string `yaml:"install-commands"`
	StripCommands     *[]string `yaml:"strip-commands"`
	Commands          *[]string `yaml:"commands"`
}

// pipelinePhases is a set of resolved phase command lists ready for
// rendering. One per arch for conditional elements; one total
// otherwise. Env carries the per-action environment-variable
// bindings the cmd's prelude emits as `export K=V` lines —
// project.conf-level + element-level environments composed and
// variable-resolved (runtime sentinels mapped to their shell-var
// form so `GOPATH: %{build-root}` becomes `export
// GOPATH="$BUILD_ROOT"`, working under shell-time expansion the
// same way phase commands do).
type pipelinePhases struct {
	Configure, Build, Install, Strip []string
	Env                              [][2]string // ordered K, V pairs
}

// shouldUseRound2 reports whether this pipelineHandler is opted
// in to the trace-driven round-2 shape AND the runtime CLI flags
// activated round-2 globally. Both gates have to pass:
//
//   - traceDrivenSrckeyPatterns set on the handler (kind opts in).
//   - traceConfig.convertBin / round2Enabled set on write-a
//     (operator passed --convert-element-trace etc, didn't
//     pass --trace-round1 to opt out).
//
// When false, RenderA / RenderB fall through to the legacy
// install-genrule-in-A + placeholder-in-B shape; existing
// fixtures and gates that don't enable round-2 keep working.
func (h pipelineHandler) shouldUseRound2() bool {
	return h.traceDrivenSrckeyPatterns != nil &&
		traceConfig.convertBin != "" &&
		traceConfig.round2Enabled
}

func (h pipelineHandler) RenderA(elem *element, elemPkg string) error {
	if h.shouldUseRound2() {
		// Round-2: project A hosts the per-element converter
		// genrule. Reads @trace_<elem>//:trace from the AC at
		// load time; emits BUILD.bazel.out (cc_library /
		// cc_binary on AC hit; placeholder on miss). The
		// install genrule itself moves to project B (see
		// RenderB below).
		return renderTraceDrivenRound2A(elem, elemPkg, h.kindName, h.traceDrivenSrckeyPatterns)
	}
	return h.renderInstallGenrule(elem, elemPkg)
}

// renderInstallGenrule writes the legacy single-platform install-
// genrule BUILD.bazel for an element to elemPkg. Thin wrapper over
// renderInstallGenruleBody, which returns the rendered string so
// the project-B per-platform fan-out can compose N bodies plus a
// top-level select()-filegroup in one file.
func (h pipelineHandler) renderInstallGenrule(elem *element, elemPkg string) error {
	body, err := h.renderInstallGenruleBody(elem, elemPkg)
	if err != nil {
		return err
	}
	return writeFile(filepath.Join(elemPkg, "BUILD.bazel"), body)
}

// renderInstallGenruleBody is the legacy install-genrule rendering —
// the shape pipelineHandler.RenderA emits when the kind isn't
// opted into round-2 (or when round-2 is globally disabled). The
// genrule's outs include install_tree.tar; cmd stages sources,
// runs configure/build/install/strip phases, and tars
// %{install-root}.
//
// Returns the body as a string so a caller composing multiple
// install genrules in one BUILD.bazel (the project-B per-platform
// fan-out) can stitch them together without writing intermediate
// files.
func (h pipelineHandler) renderInstallGenruleBody(elem *element, elemPkg string) (string, error) {
	var cfg pipelineCfg
	// Decode the .bst's config: only when it's actually present;
	// otherwise leave cfg zero (all phases nil → use defaults).
	if !elem.Bst.Config.IsZero() {
		if err := elem.Bst.Config.Decode(&cfg); err != nil {
			return "", fmt.Errorf("element %q (kind:%s): parse config: block: %w", elem.Name, h.kindName, err)
		}
	}
	// Per-phase fallback: nil pointer → handler default; non-nil
	// pointer (even empty slice) → use what the .bst declared.
	rawConfigure := mergeWithDefault(cfg.ConfigureCommands, h.defaults.Configure)
	rawBuild := mergeWithDefault(cfg.BuildCommands, h.defaults.Build)
	rawInstall := mergeWithDefault(cfg.InstallCommands, h.defaults.Install)
	rawStrip := mergeWithDefault(cfg.StripCommands, h.defaults.Strip)
	// kind:script's flat config:commands list — when present, it
	// takes the install-commands slot (other phases stay empty).
	// BuildStream's script plugin doesn't have configure / build /
	// strip phases.
	if cfg.Commands != nil {
		rawInstall = *cfg.Commands
	}

	dispatch, err := dispatchSpaceForElement(elem, elem.ProjectConfOptions)
	if err != nil {
		return "", err
	}

	// Resolution helper: variable-resolve + substitute every phase
	// command for a specific tuple (one entry per dispatch
	// variable). Empty tuple = unconditional resolution.
	resolveAt := func(tuple map[string]string) (pipelinePhases, error) {
		// Per-element built-ins. BuildStream reserves a small set of
		// names that resolve to the element's own metadata —
		// `element-name` is the one v1 actually sees in FDSDK
		// (bootstrap/base-sdk/perl uses it in flags.yml). Lowest
		// precedence (any user var with the same name overrides).
		elemBuiltins := map[string]string{
			"element-name": elem.Name,
		}
		var vars map[string]string
		var err error
		if len(tuple) == 0 {
			vars, err = resolveVars(elemBuiltins, elem.ProjectConfVars, h.defaultVars, elem.Bst.Variables)
		} else {
			vars, err = resolveVarsForTuple(elemBuiltins, elem.ProjectConfVars, h.defaultVars, elem.Bst.Variables,
				tuple, elem.ProjectConfConditionals, elem.Bst.Conditionals)
		}
		if err != nil {
			return pipelinePhases{}, fmt.Errorf("element %q (kind:%s) resolve variables%s: %w",
				elem.Name, h.kindName, tupleSuffix(tuple), err)
		}
		// Apply config: (?): per-arch overrides for the matching
		// branches. Each branch's Overrides is a partial pipelineCfg
		// shape (e.g. just configure-commands); decode and replace
		// fields where the override pointer is non-nil.
		tupleConfigure := rawConfigure
		tupleBuild := rawBuild
		tupleInstall := rawInstall
		tupleStrip := rawStrip
		for _, b := range elem.Bst.ConfigConditionals {
			if !branchMatchesTuple(b, tuple) {
				continue
			}
			var override pipelineCfg
			if err := b.Overrides.Decode(&override); err != nil {
				return pipelinePhases{}, fmt.Errorf("element %q (kind:%s) decode config:(?): branch%s: %w",
					elem.Name, h.kindName, tupleSuffix(tuple), err)
			}
			if override.ConfigureCommands != nil {
				tupleConfigure = *override.ConfigureCommands
			}
			if override.BuildCommands != nil {
				tupleBuild = *override.BuildCommands
			}
			if override.InstallCommands != nil {
				tupleInstall = *override.InstallCommands
			}
			if override.StripCommands != nil {
				tupleStrip = *override.StripCommands
			}
			if override.Commands != nil {
				tupleInstall = *override.Commands
			}
		}
		var p pipelinePhases
		p.Configure, err = substituteCmds(tupleConfigure, vars, elem.Name, h.kindName, "configure-commands")
		if err != nil {
			return pipelinePhases{}, err
		}
		p.Build, err = substituteCmds(tupleBuild, vars, elem.Name, h.kindName, "build-commands")
		if err != nil {
			return pipelinePhases{}, err
		}
		p.Install, err = substituteCmds(tupleInstall, vars, elem.Name, h.kindName, "install-commands")
		if err != nil {
			return pipelinePhases{}, err
		}
		p.Strip, err = substituteCmds(tupleStrip, vars, elem.Name, h.kindName, "strip-commands")
		if err != nil {
			return pipelinePhases{}, err
		}
		// Compose env: project.conf-level (defaults) + element-level
		// (overrides). Substitute %{...} references against the
		// resolved variable map. Result is ordered K-V pairs so the
		// rendered `export K=V` lines are deterministic across runs.
		composedEnv := composeEnvironment(elem.ProjectConfEnvironment, elem.Bst.Environment)
		p.Env, err = substituteEnv(composedEnv, vars, elem.Name, h.kindName)
		if err != nil {
			return pipelinePhases{}, err
		}
		return p, nil
	}

	// FUSE-sources mode: skip on-disk staging when the element
	// has a single non-kind:local source with no Directory subpath
	// — the genrule will pull from @src_<key>//:tree (symlinked
	// into the cas-fuse mount by the rules/sources.bzl repo
	// rule). Multi-source / Directory / kind:local elements still
	// stage; same shape as cmakeHandler's gating.
	fuseKey := pipelineFuseEligible(elem)
	if fuseKey == "" {
		if err := stagePipelineSources(elem, elemPkg); err != nil {
			return "", err
		}
	}

	if len(dispatch) == 0 {
		// No (?): dispatch (or branches were folded into static
		// vars at graph-load time): single-string cmd.
		phases, err := resolveAt(nil)
		if err != nil {
			return "", err
		}
		return renderPipelineBuild(elem, dispatch, []dispatchGroup{{Phases: phases}}, fuseKey, h.extension), nil
	}

	// Cross-product of all dispatch variables' values. Each tuple
	// resolves to one phases set; group tuples by identical
	// resolution so the emitted select() doesn't duplicate identical
	// branches.
	//
	// Soft-skip semantics: when a tuple's resolution fails with a
	// "variable referenced but not defined" error (the FDSDK
	// pattern where flags.yml has (?): branches for
	// {x86_64, aarch64, ppc64le, riscv64} but not loongarch64,
	// while the option declaration enumerates loongarch64), log
	// the skip and omit the tuple from the rendered select().
	// Bazel surfaces the missing platform at build time as
	// "no matching select() arm" rather than write-a aborting
	// the whole element render. Preserves render robustness on
	// real-world graphs where dispatch values overshoot what
	// the (?): branches actually cover.
	type groupKey [4]string
	groupIdx := map[groupKey]int{}
	var groups []dispatchGroup
	var lastSkipErr error
	for _, tuple := range cartesianTuples(dispatch) {
		phases, err := resolveAt(tuple)
		if err != nil {
			if strings.Contains(err.Error(), "referenced but not defined") {
				// Tuple silently dropped from the rendered
				// select(); bazel surfaces the missing platform
				// at build time as "no matching select() arm,"
				// which is louder than write-a aborting render.
				// Future --verbose flag can echo the skip to
				// stderr; for now the BUILD content is the
				// canonical record of which dispatch arms exist.
				lastSkipErr = err
				continue
			}
			return "", err
		}
		key := groupKey{
			strings.Join(phases.Configure, "\x00"),
			strings.Join(phases.Build, "\x00"),
			strings.Join(phases.Install, "\x00"),
			strings.Join(phases.Strip, "\x00") + "\x01" + envKey(phases.Env),
		}
		if idx, ok := groupIdx[key]; ok {
			groups[idx].Tuples = append(groups[idx].Tuples, tuple)
		} else {
			groupIdx[key] = len(groups)
			groups = append(groups, dispatchGroup{
				Tuples: []map[string]string{tuple},
				Phases: phases,
			})
		}
	}
	// All tuples skipped → nothing to emit. Surface as error so
	// the operator knows nothing's buildable rather than silently
	// rendering an empty select().
	if len(groups) == 0 {
		// All tuples skipped — surface the last underlying
		// resolution error so the operator sees which variable
		// is the culprit.
		return "", fmt.Errorf("element %q (kind:%s): every dispatch tuple was unresolvable; check (?): branch coverage vs option values; last error: %v",
			elem.Name, h.kindName, lastSkipErr)
	}
	// Dedup-collapse: if every dispatch tuple resolves identically,
	// the (?): block didn't actually affect the rendered cmd. Emit
	// the single-string form to keep the BUILD readable.
	if len(groups) == 1 {
		groups[0] = dispatchGroup{Phases: groups[0].Phases}
		return renderPipelineBuild(elem, nil, groups, fuseKey, h.extension), nil
	}
	return renderPipelineBuild(elem, dispatch, groups, fuseKey, h.extension), nil
}

// archSuffix shapes an arch identifier into a parenthetical for
// error messages: empty arch returns empty string, non-empty
// returns " (arch=<name>)".
func archSuffix(arch string) string {
	if arch == "" {
		return ""
	}
	return " (arch=" + arch + ")"
}

// tupleSuffix formats the dispatch tuple for error messages. Empty
// tuple → empty string; one entry → " (var=val)"; multiple entries
// → " (var1=val1, var2=val2, ...)" sorted by name.
func tupleSuffix(tuple map[string]string) string {
	if len(tuple) == 0 {
		return ""
	}
	keys := make([]string, 0, len(tuple))
	for k := range tuple {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+tuple[k])
	}
	return " (" + strings.Join(parts, ", ") + ")"
}

// composeEnvironment merges project.conf-level env (defaults) and
// element-level env (overrides), returning ordered K-V pairs sorted
// by key for stable rendering.
func composeEnvironment(projectEnv, elemEnv map[string]string) [][2]string {
	merged := map[string]string{}
	for k, v := range projectEnv {
		merged[k] = v
	}
	for k, v := range elemEnv {
		merged[k] = v
	}
	keys := make([]string, 0, len(merged))
	for k := range merged {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([][2]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, [2]string{k, merged[k]})
	}
	return out
}

// substituteEnv runs each env value through substituteCmd against
// the resolved variable map. Errors carry the env-key context so a
// stray %{typo} surfaces with enough context to locate it.
func substituteEnv(env [][2]string, vars map[string]string, elemName, kindName string) ([][2]string, error) {
	out := make([][2]string, len(env))
	for i, kv := range env {
		v, err := substituteCmd(kv[1], vars)
		if err != nil {
			return nil, fmt.Errorf("element %q (kind:%s) environment[%q]: %w", elemName, kindName, kv[0], err)
		}
		out[i] = [2]string{kv[0], v}
	}
	return out, nil
}

// envKey is a stable string serialization of an env-pair list,
// used by the per-arch dedup hash so two arches with identical env
// + identical phases share a select() group.
func envKey(env [][2]string) string {
	var b strings.Builder
	for _, kv := range env {
		b.WriteString(kv[0])
		b.WriteByte('=')
		b.WriteString(kv[1])
		b.WriteByte('\x00')
	}
	return b.String()
}

// dispatchGroup is one branch of the select() the pipeline handler
// emits when an element's (?): block dispatches over one or more
// variables. A group with empty Tuples is the "single-string cmd"
// shape (no select); a group with a non-empty Tuples list becomes
// one entry per tuple in the select() dict (each tuple maps to the
// same Phases body — dedup-collapse groups identical resolutions).
//
// Each tuple is a complete assignment of values across all
// dispatch dimensions in the element's dispatch space. With one
// dispatch var, tuples are single-key maps; with multiple, tuples
// have one entry per dimension and the renderer emits combined
// config_settings (constraint_values + flag_values).
type dispatchGroup struct {
	Tuples []map[string]string
	Phases pipelinePhases
}

// substituteCmds applies the resolved variable map to every command
// in a phase. The phase / kind / element labels feed the error
// message so a stray %{typo} surfaces with enough context to
// locate it in the .bst.
func substituteCmds(cmds []string, vars map[string]string, elemName, kindName, phase string) ([]string, error) {
	out := make([]string, len(cmds))
	for i, c := range cmds {
		s, err := substituteCmd(c, vars)
		if err != nil {
			return nil, fmt.Errorf("element %q (kind:%s) %s[%d]: %w", elemName, kindName, phase, i, err)
		}
		out[i] = s
	}
	return out, nil
}

func (h pipelineHandler) RenderB(elem *element, elemPkg string) error {
	if h.shouldUseRound2() {
		// Round-2: project B hosts the coarse install genrule
		// (build-tracer wrap + inline trace-publish). The
		// converter is gone from this side — it lives in project
		// A's per-element converter genrule (see RenderA above).
		// imports.json is NOT rendered in B: nothing here reads
		// it (the converter is in A); rendering would create a
		// dead input under the install action's merkle and
		// cause unnecessary cache invalidation when only the
		// imports manifest changes.
		if err := h.renderPipelineRound2B(elem, elemPkg); err != nil {
			return err
		}
		// srckey.txt feeds trace-publish (it derives the
		// synthetic AC key from the contents). Same file the
		// project-A converter genrule references; rendered in
		// both places to keep each project's BUILD self-contained.
		return renderSrckey(elem, elemPkg, h.traceDrivenSrckeyPatterns)
	}
	// Legacy / non-opted-in path: install_tree.tar lives in
	// project A; project B is a placeholder for the
	// typed-filegroup wrapper that future work lands.
	body := fmt.Sprintf(`# Generated by cmd/write-a. Do not edit by hand.
# kind:%[2]s — install tree produced by project A's genrule.
# The driver script overwrites this file with the typed-filegroup
# wrapper once that lands; until then, downstream consumers fetch
# install_tree.tar from project A directly.
filegroup(name = "BUILD_NOT_YET_STAGED", srcs = [])
`, elem.Name, h.kindName)
	return writeFile(filepath.Join(elemPkg, "BUILD.bazel"), body)
}

// renderPipelineRound2B emits project B's per-element BUILD for
// the round-2 trace-driven path. Two shapes:
//
//   - Single-platform legacy (traceConfig.platforms empty): one
//     install genrule named "<elem>_install" with the standard
//     trace-publish step appended. The trace-publish call reads
//     CMAKE_TO_BAZEL_PLATFORM from the action env (operators
//     pass --action_env to pin the publish-side platform tag).
//     Byte-stable with the pre-fan-out rendered goldens.
//
//   - Multi-platform fan-out (traceConfig.platforms non-empty):
//     N install genrules, one per platform, each with the
//     platform's constraint set in exec_compatible_with so Bazel
//     routes the action to a matching executor pool. Each
//     genrule's outputs land under "<platform>/" so the N
//     genrules don't collide, and each one's trace-publish step
//     bakes its platform tag literally into the --platform= argv
//     so each cell publishes under the matching AC partition.
//     A top-level filegroup at ":install_tree.tar" select()s the
//     right per-platform tarball so downstream
//     //elements/<dep>:install_tree.tar references resolve
//     correctly at the consumer's build platform.
func (h pipelineHandler) renderPipelineRound2B(elem *element, elemPkg string) error {
	if len(traceConfig.platforms) == 0 {
		h2 := h
		h2.extension = pipelineTraceExtensionRound2(elem, []string{h.kindName}, tracePlatform{})
		return h2.renderInstallGenrule(elem, elemPkg)
	}
	// Multi-platform path. Render N install-genrule bodies, then
	// stitch them together (sharing the top-of-file `package(...)`
	// header so the rendered BUILD.bazel is valid Bazel) plus a
	// top-level select()-filegroup at install_tree.tar.
	bodies := make([]string, 0, len(traceConfig.platforms))
	for _, plat := range traceConfig.platforms {
		h2 := h
		h2.extension = pipelineTraceExtensionRound2(elem, []string{h.kindName}, plat)
		body, err := h2.renderInstallGenruleBody(elem, elemPkg)
		if err != nil {
			return fmt.Errorf("element %q (kind:%s) platform %q: %w", elem.Name, h.kindName, plat.Name, err)
		}
		bodies = append(bodies, body)
	}
	return writeFile(filepath.Join(elemPkg, "BUILD.bazel"), composeMultiPlatformInstallBuild(elem, bodies, traceConfig.platforms))
}

// composeMultiPlatformInstallBuild stitches N per-platform install-
// genrule body strings into one BUILD.bazel: the first body's
// `package(...)` header survives as the file header, subsequent
// bodies have their header stripped, and a trailing top-level
// filegroup at ":install_tree.tar" select()s the matching
// per-platform tarball so downstream //elements/<dep>:install_tree.tar
// references stay valid.
func composeMultiPlatformInstallBuild(elem *element, bodies []string, platforms []tracePlatform) string {
	var b strings.Builder
	for i, body := range bodies {
		if i > 0 {
			// renderPipelineBuild prepends a fixed package(...)
			// header to every body. The first body's header
			// becomes the file header; strip the duplicate
			// headers from the rest by dropping everything
			// before the first `genrule(`.
			if idx := strings.Index(body, "genrule("); idx >= 0 {
				body = body[idx:]
			}
		}
		b.WriteString(body)
	}
	// Top-level filegroup: routes a consumer's
	// //elements/<dep>:install_tree.tar reference to the right
	// per-platform tarball. Per-platform select() arms key on
	// the platform's pre-resolved SelectKey (loadPlatformsManifest
	// ran PickSelectKeys at flag-parse time, so every platform's
	// SelectKey is populated here without any error path to
	// handle).
	b.WriteString("\nfilegroup(\n")
	fmt.Fprintf(&b, "    name = %q,\n", "install_tree.tar")
	b.WriteString("    srcs = select({\n")
	// Stable sort by SelectKey so the rendered output is
	// deterministic.
	sorted := make([]tracePlatform, len(platforms))
	copy(sorted, platforms)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].SelectKey < sorted[j].SelectKey })
	for _, p := range sorted {
		fmt.Fprintf(&b, "        %q: [%q],\n", p.SelectKey, p.Name+"/install_tree.tar")
	}
	b.WriteString("    }),\n")
	b.WriteString(")\n")
	return b.String()
}

// mergeWithDefault returns the user-supplied slice when non-nil,
// otherwise the default. The empty-slice case (user explicitly set
// `phase-commands: []`) is preserved as-is via the pointer check.
func mergeWithDefault(user *[]string, def []string) []string {
	if user == nil {
		return def
	}
	return *user
}

// stagePipelineSources copies the .bst's kind:local source trees
// into the project-A package so the genrule's
// `srcs = glob(["sources/**"])` picks them up. No narrowing:
// pipeline kinds' commands can read any path arbitrarily, so
// feedback-driven zero stubs don't apply. Multi-source elements
// honor each source's Directory subpath under sources/.
func stagePipelineSources(elem *element, elemPkg string) error {
	return stageAllSources(elem, filepath.Join(elemPkg, "sources"))
}

// pipelineFuseEligible reports whether a pipeline-shape element
// can take the FUSE-sources path: --use-fuse-sources flipped
// at startup, single source, no Directory subpath, and the
// source has a sourceKey (i.e. not kind:local). Returns the
// source key on success, "" otherwise.
//
// Same constraint envelope as cmakeHandler's gating; multi-
// source / Directory / kind:local cases fall back to staging.
// Repo-composition for multi-source elements is a follow-up.
func pipelineFuseEligible(elem *element) string {
	if !useFuseSourcesGlobal {
		return ""
	}
	if len(elem.Sources) != 1 {
		return ""
	}
	if elem.Sources[0].Directory != "" {
		return ""
	}
	return sourceKey(elem.Sources[0])
}

// renderPipelineBuild renders the per-element BUILD for a coarse-
// grained pipeline kind: a glob over staged sources + a genrule
// whose cmd stages the sources into a fresh work dir, runs each
// phase's commands in order, then tars %{install-root} as the
// element's primary output (install_tree.tar).
//
// Phase commands arrive here already variable-expanded (RenderA
// runs each through substituteCmd before getting here), so the
// only thing the genrule cmd binds at action time is the runtime
// sentinels: $$INSTALL_ROOT (the per-action mktemp dir tarred as
// install_tree.tar) and $$BUILD_ROOT (the staged source dir, also
// the cwd where phase commands run).
//
// groups carries one or more pre-resolved phase command sets:
//   - Single group with Arches==nil → renders cmd as a single
//     """...""" block (the no-conditional shape; covers every
//     v1 fixture and elements whose (?): blocks didn't actually
//     affect any rendered command).
//   - Multiple groups → renders cmd as `select({label: """...""",
//     ...})` over @platforms//cpu:* labels, one branch per arch
//     group. Lowering BuildStream's (?): per-arch overrides into
//     project-B Bazel-native multi-arch resolution rather than
//     baking write-a's host arch into the rendered cmd.
func renderPipelineBuild(elem *element, dispatch []dispatchVar, groups []dispatchGroup, fuseKey string, ext *pipelineExtension) string {
	var b strings.Builder
	fmt.Fprintf(&b, `# Generated by cmd/write-a. Do not edit by hand.

package(default_visibility = ["//visibility:public"])

`)
	if fuseKey == "" {
		fmt.Fprintf(&b, `filegroup(
    name = "%[1]s_sources",
    srcs = glob(["sources/**"]),
)

`, elem.Name)
	}

	// config_setting emission gates on the dispatch shape:
	//   - 0 dims (no dispatch) — none.
	//   - 1 dim, "platform" (target_arch only) — none; the cmd's
	//     select() arms reference @platforms//cpu:<v> directly.
	//   - 1 dim, "option" — one config_setting per option value.
	//   - 2+ dims (cross-product) — one config_setting per tuple,
	//     combining constraint_values (for platform dims) with
	//     flag_values (for option dims).
	if needsConfigSettings(dispatch) {
		b.WriteString(renderConfigSettings(dispatch, groups))
	}

	srcsBase := fmt.Sprintf(`":%s_sources"`, elem.Name)
	if fuseKey != "" {
		srcsBase = fmt.Sprintf(`"@src_%s//:tree"`, fuseKey)
	}
	srcsAttr := "[" + srcsBase
	if ext != nil {
		for _, extra := range ext.ExtraSrcs {
			srcsAttr += fmt.Sprintf(", %q", extra)
		}
		for _, label := range ext.DepLabels {
			srcsAttr += fmt.Sprintf(", %q", label)
		}
	}
	srcsAttr += "]"

	// Apply the per-platform OutputPrefix (set only by project-B's
	// per-platform install fan-out) so each (element, platform)
	// cell's outputs live under a distinct subdirectory and don't
	// collide. Empty prefix is the legacy single-platform shape;
	// outs render byte-identical to before.
	prefix := ""
	nameSuffix := ""
	var execCompatibleWith []string
	if ext != nil {
		prefix = ext.OutputPrefix
		nameSuffix = ext.NameSuffix
		execCompatibleWith = ext.ExecCompatibleWith
	}
	rawOuts := []string{"install_tree.tar"}
	var tools []string
	if ext != nil {
		rawOuts = append(rawOuts, ext.ExtraOuts...)
		tools = append(tools, ext.ExtraTools...)
	}
	outs := rawOuts
	if prefix != "" {
		outs = make([]string, len(rawOuts))
		for i, o := range rawOuts {
			outs[i] = prefix + "/" + o
		}
	}

	fmt.Fprintf(&b, `genrule(
    name = "%[1]s_install%[6]s",
    srcs = %[3]s,
    outs = %[4]s,
    cmd = %[2]s,
%[5]s%[7]s)
`, elem.Name,
		renderPipelineCmdAttr(dispatch, groups, fuseKey != "", ext),
		srcsAttr,
		strList(outs),
		toolsAttr(tools),
		nameSuffix,
		execCompatibleWithAttr(execCompatibleWith))
	return b.String()
}

// execCompatibleWithAttr renders the optional
// `exec_compatible_with = [...]` genrule attribute. Empty list
// returns the empty string so single-platform legacy goldens
// don't pick up a stray attribute.
func execCompatibleWithAttr(constraints []string) string {
	if len(constraints) == 0 {
		return ""
	}
	return fmt.Sprintf("    exec_compatible_with = %s,\n", strList(constraints))
}

// strList renders a Go []string as a Bazel string list.
func strList(items []string) string {
	if len(items) == 0 {
		return "[]"
	}
	parts := make([]string, len(items))
	for i, s := range items {
		parts[i] = fmt.Sprintf("%q", s)
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

// toolsAttr renders the optional `tools = [...]` genrule
// attribute. Empty list returns the empty string so the
// surrounding genrule template doesn't get a stray attribute.
func toolsAttr(tools []string) string {
	if len(tools) == 0 {
		return ""
	}
	return fmt.Sprintf("    tools = %s,\n", strList(tools))
}

// needsConfigSettings reports whether the dispatch shape requires
// emitting local config_setting rules. The single-dispatch-var
// "platform" case (target_arch only) doesn't — the cmd's select()
// arms can reference @platforms//cpu:<v> directly. Every other
// shape (option-typed, or any cross-product with 2+ dims)
// requires per-tuple config_settings.
func needsConfigSettings(dispatch []dispatchVar) bool {
	if len(dispatch) == 0 {
		return false
	}
	if len(dispatch) == 1 && dispatch[0].Kind == "platform" {
		return false
	}
	return true
}

// renderConfigSettings emits one `config_setting` per dispatch
// tuple. Each config_setting carries:
//   - constraint_values for "platform" dims (e.g. target_arch=x86_64
//     becomes constraint_values = ["@platforms//cpu:x86_64"]).
//   - flag_values for "option" dims (e.g. snap_grade=devel becomes
//     flag_values = {"//options:snap_grade": "devel"}).
//
// Names follow tupleConfigSettingName — a sorted-by-varname join
// of values with '_' so identical tuples produce identical names
// across runs.
func renderConfigSettings(dispatch []dispatchVar, groups []dispatchGroup) string {
	kinds := map[string]string{}
	for _, d := range dispatch {
		kinds[d.Name] = d.Kind
	}
	var b strings.Builder
	for _, g := range groups {
		for _, tuple := range g.Tuples {
			fmt.Fprintf(&b, "config_setting(\n")
			fmt.Fprintf(&b, "    name = %q,\n", tupleConfigSettingName(tuple))
			// Sort keys for deterministic rendering.
			keys := sortedKeys(tuple)
			var constraints, flagPairs []string
			for _, k := range keys {
				v := tuple[k]
				switch kinds[k] {
				case "platform":
					constraints = append(constraints, archConstraintLabel(v))
				case "option":
					flagPairs = append(flagPairs, fmt.Sprintf("%q: %q", "//options:"+k, v))
				}
			}
			if len(constraints) > 0 {
				fmt.Fprintf(&b, "    constraint_values = [\n")
				for _, c := range constraints {
					fmt.Fprintf(&b, "        %q,\n", c)
				}
				fmt.Fprintf(&b, "    ],\n")
			}
			if len(flagPairs) > 0 {
				fmt.Fprintf(&b, "    flag_values = {\n")
				for _, fp := range flagPairs {
					fmt.Fprintf(&b, "        %s,\n", fp)
				}
				fmt.Fprintf(&b, "    },\n")
			}
			fmt.Fprintf(&b, ")\n\n")
		}
	}
	return b.String()
}

// tupleConfigSettingName returns the local config_setting label
// name for a dispatch tuple. Sorts entries by varname and joins
// values with '_'; non-identifier characters in values become '_'.
func tupleConfigSettingName(tuple map[string]string) string {
	keys := sortedKeys(tuple)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, sanitizeIdent(tuple[k]))
	}
	return strings.Join(parts, "_")
}

// sanitizeIdent replaces non-identifier characters with '_' so a
// dispatch value like "1.2.3" or "my-option" produces a valid
// Bazel target name.
func sanitizeIdent(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_':
			b.WriteByte(c)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

// sortedKeys returns the keys of a map[string]string sorted
// alphabetically.
func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// renderPipelineCmdAttr emits the value of the genrule's cmd
// attribute. Empty dispatch + single group: a triple-quoted shell
// script string. Otherwise: select({...}) over per-tuple labels
// (either @platforms//cpu:<v> for the simple target_arch-only
// case, or local :<tuple-name> config_setting labels otherwise).
func renderPipelineCmdAttr(dispatch []dispatchVar, groups []dispatchGroup, fuseSources bool, ext *pipelineExtension) string {
	if len(groups) == 1 && len(groups[0].Tuples) == 0 {
		return renderPipelineCmdBody(groups[0].Phases, fuseSources, ext)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "select({\n")
	for _, g := range groups {
		body := renderPipelineCmdBody(g.Phases, fuseSources, ext)
		for _, tuple := range g.Tuples {
			fmt.Fprintf(&b, "        %q: %s,\n", tupleSelectLabel(dispatch, tuple), body)
		}
	}
	fmt.Fprintf(&b, "    })")
	return b.String()
}

// tupleSelectLabel returns the Bazel select() key for a dispatch
// tuple. Single-platform-dim case (target_arch only) uses
// @platforms//cpu:<v> directly without a config_setting wrapper.
// Every other shape references the local :<tuple-name>
// config_setting renderConfigSettings emitted.
func tupleSelectLabel(dispatch []dispatchVar, tuple map[string]string) string {
	if len(dispatch) == 1 && dispatch[0].Kind == "platform" {
		return archConstraintLabel(tuple[dispatch[0].Name])
	}
	return ":" + tupleConfigSettingName(tuple)
}

// renderPipelineCmdBody emits the triple-quoted shell-script body
// the genrule's cmd attribute consumes (or one branch of the
// select() dict in the multi-arch case). fuseSources picks the
// strip-prefix the cmd uses to recover source-relative paths
// from $(SRCS) entries: "sources/" for the staged-on-disk shape
// (the default), "tree_dir/" for the FUSE-symlinked
// @src_<key>//:tree shape (matches the symlink target the
// rules/sources.bzl repo rule creates).
func renderPipelineCmdBody(p pipelinePhases, fuseSources bool, ext *pipelineExtension) string {
	stripFrom := "sources/"
	if fuseSources {
		stripFrom = "tree_dir/"
	}
	// installTar names the install_tree.tar output the cmd's tar
	// step writes to. Single-platform legacy shape: bare
	// "install_tree.tar". Per-platform install fan-out: the
	// OutputPrefix-prefixed path so each cell's output lives at
	// its own location. $(location ...) takes the declared-output
	// path verbatim, so the prefix has to be embedded here.
	installTar := "install_tree.tar"
	if ext != nil && ext.OutputPrefix != "" {
		installTar = ext.OutputPrefix + "/install_tree.tar"
	}

	// Resolved configure/build/install/strip command block.
	// pipelineExtension.WrapPipelineCmds rewrites this when
	// the kind installs a wrapper (the trace-driven autotools
	// path wraps in build-tracer); nil hook = pass through.
	cmds := renderPipelineCommands(p.Configure, p.Build, p.Install, p.Strip)
	if ext != nil && ext.WrapPipelineCmds != nil {
		cmds = ext.WrapPipelineCmds(cmds)
	}

	// Optional shell snippet that runs after the pipeline cmds
	// but before the install-tree tar. Used to feed the trace
	// produced by the wrapper into convert-element-trace.
	appendCmd := ""
	if ext != nil && ext.AppendCmd != "" {
		appendCmd = "\n" + ext.AppendCmd + "\n"
	}

	// Optional shell snippet that runs after BUILD_ROOT setup
	// but before the user-provided pipeline cmds. Used by
	// kinds whose pipeline consumes upstream install trees
	// (autotools native: extracts dep tars under $DEP_PREFIX,
	// exports CPPFLAGS / LDFLAGS).
	depExtractCmd := ""
	if ext != nil && ext.DepExtractCmd != "" {
		depExtractCmd = "\n" + ext.DepExtractCmd + "\n"
	}

	return fmt.Sprintf(`"""
        # Snapshot the exec root before any cd. Bazel resolves
        # location expressions to exec-root-relative paths, and the
        # user-provided commands below cd into the staged work dir,
        # so we restore PWD before tarring the install tree.
        EXEC_ROOT="$$PWD"
        # Stage sources into a fresh work dir; honor the original
        # source-relative layout via the same shadow-merge pattern
        # the cmake handler uses (strip the leading "sources/" of
        # each $(SRCS) entry to recover the source-relative path).
        BUILD_ROOT="$$(mktemp -d)"
        for src in $(SRCS); do
            # Skip extension-supplied non-source files (imports.json
            # for the autotools native render path; install_tree.tar
            # entries from upstream deps, handled by DepExtractCmd
            # below). Their access happens via $$(location <name>) /
            # $(SRCS) iteration in extension snippets; copying them
            # into BUILD_ROOT would leak into the staged source tree.
            case "$$src" in
                */imports.json) continue ;;
                */install_tree.tar) continue ;;
            esac
            rel="$${src##*%[3]s}"
            mkdir -p "$$BUILD_ROOT/$$(dirname "$$rel")"
            cp -L "$$src" "$$BUILD_ROOT/$$rel"
        done
        cd "$$BUILD_ROOT"

        # Runtime variable bindings (every other %%{...} reference is
        # already expanded at codegen time by handler_pipeline's
        # substituteCmd):
        #   $$INSTALL_ROOT — DESTDIR-style staging dir; tarred as
        #                    the element's output below.
        #   $$BUILD_ROOT   — the staged source dir (set above).
        export INSTALL_ROOT="$$(mktemp -d)"
        export PATH=/usr/local/bin:/usr/bin:/bin
%[2]s%[5]s
%[1]s%[4]s
        # Tar the install tree as the element's primary output.
        # Deterministic options give byte-stable archives across
        # builds with byte-identical content: --mtime=@0 zeros out
        # mtimes; --sort=name removes filesystem-readdir-order
        # variation; --owner=0 --group=0 --numeric-owner removes
        # uid/gid drift across machines. Without these, an
        # upstream's tar would churn even when the install
        # contents didn't change, breaking downstream cache-narrow
        # transitivity (the consumer's _install action would
        # cache-miss on the changed tar input).
        cd "$$EXEC_ROOT"
        tar --mtime=@0 --sort=name --owner=0 --group=0 --numeric-owner \
            -cf "$(location %[6]s)" -C "$$INSTALL_ROOT" .
    """`, cmds, renderEnvExports(p.Env), stripFrom, appendCmd, depExtractCmd, installTar)
}

// renderEnvExports emits one `export K=V` line per env entry,
// indented to match the surrounding cmd-body lines. The values are
// already variable-resolved (substituteCmd in resolveAt); we just
// shell-quote them. Empty env yields the empty string so the
// surrounding template doesn't get a stray blank line.
func renderEnvExports(env [][2]string) string {
	if len(env) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("        # Project- + element-level environment, sourced from\n")
	b.WriteString("        # project.conf's `environment:` and the .bst's `environment:`\n")
	b.WriteString("        # blocks. Element-level entries override project-level on\n")
	b.WriteString("        # conflict; values are variable-resolved with runtime\n")
	b.WriteString("        # sentinels (%%{install-root} → $$INSTALL_ROOT etc.) mapped\n")
	b.WriteString("        # to their shell-var form so phase commands consume them\n")
	b.WriteString("        # consistently.\n")
	for _, kv := range env {
		fmt.Fprintf(&b, "        export %s=%s\n", kv[0], shellQuote(kv[1]))
	}
	return b.String()
}

// shellQuote wraps a value in double quotes, escaping any
// embedded $$ / " / \ so the resulting string is a valid
// double-quoted shell literal. Specifically: $$ stays as $$
// (Bazel's escape; the action runner sees $); a literal " becomes
// \"; a literal \ becomes \\.
func shellQuote(v string) string {
	var b strings.Builder
	b.WriteByte('"')
	for i := 0; i < len(v); i++ {
		c := v[i]
		switch c {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		default:
			b.WriteByte(c)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// renderPipelineCommands flattens the four phase command lists into
// the genrule's cmd, in canonical order. The commands arrive here
// already variable-expanded (RenderA → substituteCmd), so the only
// shaping this layer does is per-phase header comments and the
// "no commands at all" fallthrough. Each command runs under `set -e`
// (the genrule cmd block is a single shell script); failures at any
// step abort the action.
func renderPipelineCommands(configure, build, install, strip []string) string {
	var lines []string
	for _, phase := range []struct {
		label    string
		commands []string
	}{
		{"configure", configure},
		{"build", build},
		{"install", install},
		{"strip", strip},
	} {
		if len(phase.commands) == 0 {
			continue
		}
		lines = append(lines, "        # === "+phase.label+" ===")
		for _, c := range phase.commands {
			lines = append(lines, "        "+c)
		}
	}
	if len(lines) == 0 {
		// No commands at all (e.g., a kind:manual element with
		// empty config:). The genrule produces an empty install
		// tree — useful only as a degenerate fixture, but a real
		// element will always declare at least install-commands
		// or pull defaults from the kind handler.
		lines = append(lines, "        # (no pipeline commands declared; install tree will be empty)")
	}
	return strings.Join(lines, "\n")
}
