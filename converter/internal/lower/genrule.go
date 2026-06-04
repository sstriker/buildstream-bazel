package lower

import (
	"fmt"
	"io"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/sstriker/buildstream-bazel/converter/internal/cmakerun"
	"github.com/sstriker/buildstream-bazel/converter/internal/failure"
	"github.com/sstriker/buildstream-bazel/converter/internal/ninja"
	"github.com/sstriker/buildstream-bazel/converter/ir"
)

// codegenContext carries state from genrule recovery and CTest
// classification back into the consuming target's lowering.
type codegenContext struct {
	// Genrules is the list of synthesized ir.Target{Kind: KindGenrule}
	// entries to append to the package.
	Genrules []ir.Target

	// Tests is the list of synthesized ir.Target{Kind: KindCCTest}
	// entries; one per add_test() registration whose COMMAND target
	// matched an EXECUTABLE in this codemodel. Appended to the package
	// after the main target loop alongside Genrules.
	Tests []ir.Target

	// Subs is the list of per-language sub-libraries lower
	// synthesizes when a multi-language target is split into a
	// wrapper + per-language cc_library shape (multi-language
	// structural delta). The wrapper carries the original
	// target's name and Bazel-public surface; each sub-library
	// carries one language's srcs + per-language copts/defines.
	// Appended after Genrules in target-walk order so the
	// rendered BUILD groups them naturally with their parent.
	Subs []ir.Target

	// OutToGenrule maps a package-relative output path to the genrule
	// name that produces it. Used by the consumer side to add
	// has-cmake-codegen and to reference outputs by label.
	OutToGenrule map[string]string

	// CcEmbedSourceToHeader maps a cc_embed lift's generated SOURCE output
	// (the .cxx, which lands in the consuming target's srcs) to its sibling
	// generated HEADER output (the .h). A target that compiles the source
	// also needs the header as a declared hdr — they're a pair and the
	// generated .cxx #includes the .h. Populated by recognizeCcEmbed,
	// consumed in lowerTarget's per-source wiring.
	CcEmbedSourceToHeader map[string]string

	// StampVars maps a cmake variable written by a VCS-stamp
	// execute_process (BucketStamp: git/hg/svn rev-parse / describe,
	// OUTPUT_VARIABLE) to the Bazel workspace-status key a downstream
	// configure_file should read it from at build time (STABLE_<var>,
	// stable-status.txt, populated by --workspace_status_command under
	// --stamp). recoverExecuteProcess populates it; the configure_file
	// lift (which runs later over the same cc) consults it so a
	// `@GIT_SHA@` template marker re-reads the live revision instead of
	// baking the convert-time value into srckey. Empty when the project
	// has no VCS-stamp probe.
	StampVars map[string]string

	// SeenBuilds dedupes recovered builds when multiple targets reference
	// the same generated source.
	SeenBuilds map[*ninja.Build]string

	// HeaderWalkCache memoizes filesystem walks of include directories
	// across targets within one lower-element invocation. Keyed on the
	// absolute include-dir path; value is the package-relative header
	// list for that dir. Multiple targets in a project commonly share
	// include roots (`include/`, `src/`); without the cache each
	// target re-walks every shared dir.
	HeaderWalkCache map[string][]string

	// FilteredInternalCmds collects the cmake-internal command edges the
	// standalone-genrule pass drops (install / regen / cpack / dashboard /
	// ide-stub) — keyed by the edge's first output, valued by category. These
	// have no Bazel analogue so dropping is correct, but ToIR emits one
	// aggregated stderr breadcrumb at the end (alongside MissingIncludeDirs)
	// so an operator auditing a conversion sees WHAT was filtered rather than
	// the drop being silent.
	FilteredInternalCmds map[string]string

	// MissingIncludeDirs collects absolute include-directory paths
	// referenced by the codemodel that don't exist on disk. cmake
	// permits these (LLVM's llvm-mca declares
	// `target_include_directories(... include)` for forward-
	// declared headers); the converter skips them silently per-dir
	// but ToIR aggregates the set and emits one stderr warning at
	// the end so the operator sees the cmake oddity. Keyed for
	// dedup across the multiple targets that typically share an
	// include root.
	MissingIncludeDirs map[string]bool

	// CMakeScriptRunner is the operator-supplied Bazel label of a
	// target that, when invoked, IS cmake (or behaves like
	// `cmake -P`). When non-empty, recoverGenrule lifts
	// `add_custom_command(... COMMAND cmake -P <script> ...)`
	// shapes to a genrule that calls the runner instead of
	// refusing with UnsupportedCustomCommandScript. Empty (the
	// default) preserves the historical refusal — operators who
	// don't stage the tool see no behaviour change.
	//
	// Same operator-plumbing shape as the cmake-configure-file
	// flag (cli.Args.LiftConfigureFile + write-a's
	// --cmake-configure-file-bin stages the tool into project A
	// and project B); the script runner is the same idea for
	// arbitrary cmake-script-language drivers.
	CMakeScriptRunner string

	// CMakeScriptBake, when true (and CMakeBinary is set), runs
	// the cmake -P script at convert time, captures the declared
	// output bytes, and emits genrules that materialize them
	// via base64-decode. Closes the "script's hardcoded
	// absolute paths don't survive Bazel's sandbox" gap by
	// resolving the paths at convert time (where they exist).
	// Trade-off: outputs are convert-time-baked and don't
	// auto-refresh when upstream inputs change — the operator
	// re-runs convert. Same trade-off + warning shape as the
	// legacy configure_file capture; the
	// cmake-codegen-cmake-script-bake tag funnels into the
	// existing warnConvertTimeBaking post-pass.
	//
	// Off by default. Independent of CMakeScriptTrace (bake
	// captures bytes; trace captures dep paths — different
	// closures of the cmake-P gap).
	CMakeScriptBake bool

	// LiftCCEmbed, when true, recognizes a custom command running a known
	// file-embedding cmake -P encoder (VTK's vtkEncodeString) and lowers
	// it to the native cc_embed rule (//tools:cc-embed) instead of the
	// runner/bake/refuse path — so the converted project needs no cmake at
	// build time. Off by default (the consuming project must stage
	// //tools:cc-embed, like the runner); the operator opts in.
	LiftCCEmbed bool

	// LiftCCHash, when true, recognizes a custom command running a known
	// file-hashing cmake -P script (VTK's vtkHashSource) and lowers it to
	// the native cc_hash rule (//tools:cc-hash) instead of the
	// runner/bake/refuse path — so the converted project needs no cmake at
	// build time and the digest recomputes on input change. Off by default
	// (the consuming project must stage //tools:cc-hash); the operator opts in.
	LiftCCHash bool

	// CMakeScriptTrace, when true, asks the cmake -P lift to
	// actually run the script under `cmake --trace --trace-format=
	// json-v1 -P <script>` at convert time. The trace's read
	// paths drive auto-augmentation of the genrule's srcs and a
	// structured refusal diagnostic when the script touches
	// paths Bazel's sandbox can't reproduce. Off by default
	// because the trace step is convert-time-execution of
	// arbitrary cmake-script-language; operators opt in via
	// --cmake-script-trace after acknowledging the side-effect
	// risk. Requires CMakeBinary to point at a usable cmake.
	CMakeScriptTrace bool

	// CMakeBinary is the path to the convert-host cmake binary
	// the trace step uses for `cmake --trace -P`. Set by the
	// caller (main.go); empty disables the trace step.
	CMakeBinary string

	// Warnings is the io.Writer the script lift's sysroot-path
	// notice writes to. Mirrors lower.Options.Warnings; the
	// codegenContext copies it so the lift can fire warnings
	// without re-threading the Options struct through every
	// helper.
	Warnings io.Writer

	// ArtifactToName maps each codemodel target's artifact paths
	// (build-dir-relative, e.g. `bin/llvm-min-tblgen`) to the
	// target's name. Used by recoverGenrule's tool-from-target
	// rewrite — same role as the value lowerStandaloneCustomCommands
	// receives as a parameter, but threaded through the
	// codegenContext so the per-target recovery path can lift
	// bare-tool-path references in the same way without changing
	// the recoverGenrule signature.
	ArtifactToName map[string]string

	// LiteralProbeSink and LiteralResolutions thread the
	// generalized-genex two-pass through the codegen helpers
	// (mirrors how Warnings / ArtifactToName ride here rather than
	// re-threading lower.Options). LiteralProbeSink is the pass-1
	// collector (nil disables collection); LiteralResolutions holds
	// the pass-2 results keyed by request hash. resolveLiteral
	// consults both. See probe_literals.go.
	LiteralProbeSink   *LiteralProbeSink
	LiteralResolutions map[string]cmakerun.LiteralResolution
}

// resolveLiteral attempts to resolve an arbitrary genex literal via
// the two-pass probe. On the second pass (LiteralResolutions
// populated) it returns the probe-captured value when present:
// (value, true) when every config agreed. When the literal diverged
// per config the value is dropped to ("", false) at this call site
// because the OUTPUT-path consumer needs a single static path (a
// per-config OUTPUT can't drive genrule outs). On the first pass
// (sink non-nil, no resolution yet) it records the request so the
// orchestrator runs the warm second pass, and returns ("", false)
// so the caller takes its normal drop/fallback path this round.
// Returns ("", false) for single-pass callers (both nil),
// preserving today's behavior.
//
// target is the cmake target context the literal evaluates in (""
// for project-scoped literals).
func (cc *codegenContext) resolveLiteral(literal, target string) (string, bool) {
	h := cc.LiteralProbeSink.Want(literal, target)
	if res, ok := cc.LiteralResolutions[h]; ok {
		if v, agreed := res.Unified(); agreed {
			return v, true
		}
		// Per-config divergence: no single static value the
		// OUTPUT-path consumer can use. A select()-capable
		// consumer reads res.PerConfig directly via
		// resolveLiteralPerConfig; here we fall through to the
		// drop path.
		return "", false
	}
	return "", false
}

// resolveLiteralPerConfig is the select()-capable sibling of resolveLiteral:
// it records the probe request (pass 1) and, on pass 2, returns the literal's
// per-config resolved values (cmakerun.LiteralResolution.PerConfig) even when
// they diverge across build configs — the caller lowers divergence to a
// select() rather than dropping to legacy. Returns (nil, false) when no
// resolution is available yet (pass 1) or the literal wasn't probed.
func (cc *codegenContext) resolveLiteralPerConfig(literal, target string) (map[string]string, bool) {
	h := cc.LiteralProbeSink.Want(literal, target)
	if res, ok := cc.LiteralResolutions[h]; ok && len(res.PerConfig) > 0 {
		return res.PerConfig, true
	}
	return nil, false
}

// hasSynthesizedTarget reports whether a target with the given name was
// already appended to cc.Genrules (the synthesized-target list). Used to dedup
// sibling targets — e.g. compilation_outputs filegroups multiple file(GENERATE)
// calls may each reference for the same OBJECT library.
func (cc *codegenContext) hasSynthesizedTarget(name string) bool {
	for _, g := range cc.Genrules {
		if g.Name == name {
			return true
		}
	}
	return false
}

func newCodegenContext() *codegenContext {
	return &codegenContext{
		OutToGenrule:          map[string]string{},
		CcEmbedSourceToHeader: map[string]string{},
		StampVars:             map[string]string{},
		SeenBuilds:            map[*ninja.Build]string{},
		HeaderWalkCache:       map[string][]string{},
		MissingIncludeDirs:    map[string]bool{},
		FilteredInternalCmds:  map[string]string{},
	}
}

// recoverGenrule looks up the ninja Build statement that produces the given
// generated source path and lowers it to an ir.Target{Kind: KindGenrule}.
// Returns the package-relative output path to use as the consuming target's
// input, plus the genrule name. If recovery isn't possible (no ninja graph,
// no producing build, refused command shape), returns a typed Tier-1 error.
//
// buildDir is the cmake-side build directory (r.Codemodel.Paths.Build);
// generated source paths in the File API are absolute under it, and ninja's
// build statements are relative to it.
func (cc *codegenContext) recoverGenrule(srcPath, cmakeSrc, buildDir string, g *ninja.Graph) (relOut, name string, err error) {
	relOut, ok := relativeIfInside(buildDir, srcPath)
	if !ok {
		// Generated source outside the build dir is unusual; bail out
		// with a clear failure.
		return "", "", failure.New(failure.UnsupportedCustomCommand,
			"generated source %q is outside the build dir %q", srcPath, buildDir)
	}

	if g == nil {
		// No ninja graph at all — the converter ran without a cmake
		// build dir, so there's nothing to recover the producing
		// command from. This is the common configure_file() symptom
		// (#215): version / config headers (config.h, *pubconf.h,
		// *_version.h) are configure-time outputs that only become
		// recoverable once cmake has been configured and build.ninja
		// captured. Name the fix in the message so the operator isn't
		// left guessing why a "generated source" silently refused.
		return "", "", failure.New(failure.UnsupportedCustomCommand,
			"target references generated source %q but no cmake build graph (build.ninja) was available to recover the producing custom command; "+
				"generated sources such as configure_file() outputs (version / config headers) can only be lifted with a build graph — "+
				"re-run convert-element-cmake with --source-root (configures cmake in a fresh build dir and captures build.ninja) "+
				"or --cmake-build-dir (reuses an existing build dir's build.ninja)",
			relOut)
	}

	b := g.BuildFor(relOut)
	if b == nil {
		// Try the explicit-output absolute form.
		b = g.BuildFor(srcPath)
	}
	if b == nil {
		return "", "", failure.New(failure.UnsupportedCustomCommand,
			"no ninja build statement produces generated source %q", relOut)
	}

	if b.Rule != "CUSTOM_COMMAND" {
		// Object files etc. — not a custom command. We don't lower these
		// to genrule; they're already in the cc_library compile graph.
		return "", "", failure.New(failure.UnsupportedCustomCommand,
			"generated source %q is produced by rule %q, not CUSTOM_COMMAND",
			relOut, b.Rule)
	}

	// Already recovered? Reuse.
	if existingName, ok := cc.SeenBuilds[b]; ok {
		return relOut, existingName, nil
	}

	cmd, ok := ninja.CommandFor(g, b)
	if !ok {
		return "", "", failure.New(failure.UnsupportedCustomCommand,
			"could not resolve command for generated source %q", relOut)
	}

	// Issue #193: CommandFor can return (cmd, ok=true) with cmd being
	// the empty string when the underlying rule's `command` binding
	// expands to nothing — e.g. cmake emitting a no-op CUSTOM_COMMAND
	// or an unrecognised pattern whose Expand resolves to "". Emitting
	// such a genrule produces `cmd = ""` in BUILD.bazel, which Bazel
	// rejects at build time with "declared output was not created by
	// genrule". Refuse here with a typed Tier-1 error so the broken
	// BUILD never lands. The issue's reproduction was a source-only
	// case (isSourceOnly(b) true), but the Bazel-side rejection is
	// general — any empty-cmd genrule fails — so the gate is on the
	// cmd alone, not narrowed to source-only.
	if strings.TrimSpace(cmd) == "" {
		return "", "", failure.New(failure.UnsupportedCustomCommand,
			"custom command for %q resolved to an empty string; cannot emit as a genrule (Bazel would reject `cmd = \"\"`)",
			relOut)
	}

	// CMake stuffs the actual command in $COMMAND on the build statement;
	// the rule's command is just `$COMMAND`. CommandFor handles that
	// transparently via scope chain. The literal "cd <dir> &&" prefix
	// gets handled at command translation time.
	if usesCmakeScriptMode(cmd) {
		// Operator can opt into the cmake-P lift by staging the
		// runner tool and passing --cmake-script-runner=<label>.
		// Soundness caveats apply: scripts that hardcode
		// absolute paths (configure_file-derived scripts with
		// `set(SRCDIR "/abs/path")`) won't resolve under
		// Bazel's sandbox; parameter-driven scripts (VTK's
		// vtkHashSource shape) work cleanly. See
		// docs/design/generator-parity-gaps.md's "cmake -P
		// lift" entry for the limitation details.
		script := extractCmakeScriptPath(cmd)
		// Native cc_embed recognizer (opt-in via --lift-cc-embed): a known
		// file-embedding encoder (vtkEncodeString) lowers to the cc_embed
		// rule, so the converted project needs no cmake at build time. Runs
		// before the runner/bake/refuse path; falls through when it declines.
		// Returns relOut (the build-rel form of THIS consumed source —
		// header or source) so the consumer maps to the output it actually
		// referenced; the sibling output reuses via the SeenBuilds check above.
		if name, ok := recognizeCcEmbed(cc, b, cmd, script, cmakeSrc, buildDir); ok {
			return relOut, name, nil
		}
		// Native cc_hash recognizer (opt-in via --lift-cc-hash): a known
		// file-hashing script (vtkHashSource) lowers to the cc_hash rule, so
		// the converted project needs no cmake at build time and the digest
		// recomputes on input change. Same fall-through contract as cc_embed.
		if name, ok := recognizeCcHash(cc, b, cmd, script, cmakeSrc, buildDir); ok {
			return relOut, name, nil
		}
		var liftReason string
		// Bake mode (convert-time execution + bytes capture)
		// runs first when opted in: it solves the
		// hardcoded-paths case the runner-only lift can't.
		// Falls through to the standard runner lift if the
		// bake declines (e.g. cmake not on PATH, script
		// produced no output).
		if cc.CMakeScriptBake {
			rel, name, reason, ok := bakeCmakeScriptGenrule(cc, b, cmd, script, buildDir, g)
			if ok {
				return rel, name, nil
			}
			liftReason = reason
		}
		if cc.CMakeScriptRunner != "" {
			rel, name, reason, ok := liftCmakeScriptGenrule(cc, b, cmd, script, cmakeSrc, buildDir)
			if ok {
				return rel, name, nil
			}
			// Lift declined; preserve its structured reason
			// for the refusal message below.
			if liftReason == "" {
				liftReason = reason
			} else if reason != "" {
				liftReason = liftReason + "; runner lift also declined: " + reason
			}
		}
		// Pull the actual `-P <script>` argument out of the
		// recovered command so the failure points operators at
		// the specific script to rewrite — not just at the
		// consuming target's output. #207.
		base := fmt.Sprintf("custom command for %q runs `cmake -P %s`",
			relOut, script)
		if liftReason != "" {
			base += "; lift declined: " + liftReason
		}
		msg := base + "; opt into the cmake-P lift via --cmake-script-runner=<label> (requires a staged runner tool), --cmake-script-trace=true to auto-augment srcs from the script's read paths (convert-time execution), --cmake-script-bake=true to bake the script's output bytes at convert time (closes hardcoded-paths but outputs don't auto-refresh on input change), rewrite the script in a real language (shell / python), override the element via write-a --build-files-dir, route the element through the per-element round-2 cmake fallback (--unsupported-execute-process-fallback equivalent for kind:cmake; see docs/design/rendezvous.md), OR pass --ignore-rejections-for-diagnostics to skip and survey the rest"
		return "", "", failure.New(failure.UnsupportedCustomCommandScript, "%s", msg)
	}

	// Sanitize a name from the build statement's first output.
	name = genruleNameFor(b, buildDir)

	outs := genruleOuts(b, buildDir)
	// recoverGenrule predates the umbrella promotion and has no
	// labelRoot in scope; pass "" so its source-relative srcs/cmd
	// shape is unchanged. The umbrella anchoring lives on the
	// standalone-custom-command path (lowerStandaloneCustomCommands),
	// which is where LLVM's tablegen genrules surface.
	srcs := genruleSrcs(b, cmakeSrc, buildDir, "")
	tags := genruleTags(cmd, b, g)

	rewrittenCmd := rewriteGenruleCmd(cmd, cmakeSrc, buildDir, "")
	rewrittenCmd, tools := rewriteToolFromTarget(rewrittenCmd, cc.ArtifactToName)
	gen := ir.Target{
		Name:         name,
		Kind:         ir.KindGenrule,
		GenruleCmd:   rewrittenCmd,
		GenruleOuts:  outs,
		GenruleTools: tools,
		Srcs:         srcs,
		Tags:         tags,
		Visibility:   []string{"//visibility:private"},
	}
	cc.Genrules = append(cc.Genrules, gen)
	cc.SeenBuilds[b] = name
	for _, o := range outs {
		cc.OutToGenrule[o] = name
	}
	return relOut, name, nil
}

// genruleNameFor turns the first output path into a Bazel-rule-name-safe
// identifier. `version.h` -> `gen_version_h`; `dir/foo.cc` -> `gen_dir_foo_cc`.
//
// buildDir is the recording-machine build directory (the same one
// genruleOuts relativizes against). When cmake writes absolute paths into
// build.ninja, b.Outputs[0] is `<buildDir>/pkg/gen/output.cpp`; the rule
// name needs the SAME relativization the outs attribute gets, otherwise
// the buildDir's per-run temp suffix (e.g. `/tmp/convert-element-build-XXXX`)
// leaks into the rule name and makes BUILD.bazel non-deterministic across
// runs of convert-element-cmake on the same package (issue #192). Paths
// that don't relativize cleanly (genuinely outside the build dir) fall
// through verbatim — they're already path-shaped names but at least
// buildDir-independent.
func genruleNameFor(b *ninja.Build, buildDir string) string {
	first := "out"
	if len(b.Outputs) > 0 {
		first = b.Outputs[0]
		if rel, ok := relativeIfInsideRelaxed(buildDir, first); ok {
			first = rel
		}
	}
	first = filepath.ToSlash(first)
	first = strings.TrimPrefix(first, "./")
	var sb strings.Builder
	sb.WriteString("gen_")
	for i := 0; i < len(first); i++ {
		c := first[i]
		switch {
		case (c >= 'a' && c <= 'z'),
			(c >= 'A' && c <= 'Z'),
			(c >= '0' && c <= '9'),
			c == '_':
			sb.WriteByte(c)
		default:
			sb.WriteByte('_')
		}
	}
	return sb.String()
}

// genruleOuts returns build statement outputs as package-relative paths.
// Implicit outs that resolve to the same file as an explicit out (via the
// `${cmake_ninja_workdir}<name>` redundancy CMake emits) are filtered.
func genruleOuts(b *ninja.Build, buildDir string) []string {
	seen := map[string]struct{}{}
	var out []string
	for _, o := range b.Outputs {
		if rel, ok := relativeIfInsideRelaxed(buildDir, o); ok {
			if _, dup := seen[rel]; !dup {
				seen[rel] = struct{}{}
				out = append(out, rel)
			}
		}
	}
	return out
}

// genruleSrcs returns explicit and implicit inputs as package-relative
// paths. CMake records absolute paths in custom-command inputs; we
// relativize against the source root (cmakeSrc) so two inputs with the
// same basename in different subdirectories don't collide.
//
// Inputs that aren't under cmakeSrc fall back to basename — those are
// typically host-leak references the orchestrator's downstream layer
// will re-anchor (or refuse). The fallback is rare and noisy on
// purpose: anything resolving here points at a real concern.
func genruleSrcs(b *ninja.Build, cmakeSrc, buildDir, umbrellaPrefix string) []string {
	all := append([]string{}, b.Inputs...)
	all = append(all, b.ImplicitInputs...)

	seen := map[string]struct{}{}
	var out []string
	for _, in := range all {
		key := normalizeInput(in, cmakeSrc, buildDir, umbrellaPrefix)
		if key == "" {
			continue
		}
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

// normalizeInput picks the most-qualified package-relative representation
// of an input path for genrule srcs.
//
//  1. If `in` is under cmakeSrc, return cmakeSrc-relative slash form.
//  2. If under buildDir, return buildDir-relative — same shape genrule
//     outputs use, so an in-element generator binary's output is
//     matchable.
//  3. Otherwise basename, with a comment in the emitted BUILD.bazel
//     that flags the under-qualified entry. (Not implemented as a
//     comment yet; M4.x adds the audit hook.)
func normalizeInput(in, cmakeSrc, buildDir, umbrellaPrefix string) string {
	if !filepath.IsAbs(in) {
		return filepath.ToSlash(in)
	}
	if cmakeSrc != "" {
		if rel, ok := relativeIfInside(cmakeSrc, in); ok {
			// Under the workspace-root umbrella promotion (labelRoot
			// above cmakeSrc, e.g. LLVM's llvm-project/ over
			// llvm-project/llvm/), source-tree inputs must carry the
			// cmakeSrc-relative-to-labelRoot prefix so a BUILD at
			// labelRoot resolves them — consistent with the cc_library
			// src/hdr re-anchor. Empty in the non-promoted case.
			if umbrellaPrefix != "" && rel != "" {
				return filepath.ToSlash(filepath.Join(umbrellaPrefix, rel))
			}
			return rel
		}
	}
	if buildDir != "" {
		if rel, ok := relativeIfInsideRelaxed(buildDir, in); ok {
			return rel
		}
	}
	// Fallback: basename. Documented as a known under-qualification
	// site; M5's converted_pkg_repo layer will need to surface these.
	return filepath.Base(in)
}

// genruleTags computes the cmake-codegen-* tag set for one recovered build.
// See docs/codegen-tags.md for the taxonomy.
func genruleTags(cmd string, b *ninja.Build, g *ninja.Graph) []string {
	tags := []string{"cmake-codegen"}

	driver := extractDriver(cmd)
	tags = append(tags, "cmake-codegen-driver="+driver)

	if hasCmakeE(cmd) {
		tags = append(tags, "cmake-codegen-cmake-e")
	}

	if toolFromTarget(b, g) {
		tags = append(tags, "cmake-codegen-tool-from-target")
	}

	if isSourceOnly(b) {
		tags = append(tags, "cmake-codegen-source-only")
	}

	sort.Strings(tags)
	return tags
}

// extractDriver returns the binary name the command actually invokes. Strips
// `cd <dir> &&` prefix and a small recognizer list of wrappers.
//
// Falls back to "unknown" — never empty — so the driver= facet is always
// present in queries.
func extractDriver(cmd string) string {
	// Strip a leading `cd <dir> && `. ninja-emitted cmake commands almost
	// always start with this.
	if i := strings.Index(cmd, " && "); i > 0 && strings.HasPrefix(cmd, "cd ") {
		cmd = cmd[i+4:]
	}
	cmd = strings.TrimSpace(cmd)

	tokens := splitShellTokens(cmd)
	wrappers := map[string]bool{
		"env":     true,
		"sh":      true,
		"bash":    true,
		"taskset": true,
		"nice":    true,
		"ionice":  true,
	}
	for len(tokens) > 0 {
		first := tokens[0]
		base := filepath.Base(first)
		if wrappers[base] {
			// env may carry KEY=VAL pairs and -i/-u flags before the real
			// command; we strip lazily by skipping tokens starting with
			// '-' or containing '=' until a clean argv0 appears.
			tokens = tokens[1:]
			for len(tokens) > 0 {
				t := tokens[0]
				if strings.HasPrefix(t, "-") || strings.Contains(t, "=") {
					tokens = tokens[1:]
					continue
				}
				break
			}
			continue
		}
		// `sh -c "<cmd>"` is a special wrapper: we'd need to reparse the
		// quoted string. Keep "sh" as the driver to surface that we
		// didn't drill in; M2 audit can flag these.
		if base == "" {
			return "unknown"
		}
		return base
	}
	return "unknown"
}

// splitShellTokens is a small tokenizer for shell-style commands. Honors '
// and " quoting and \-escapes. Not POSIX-complete; sufficient for the
// command shapes CMake's CUSTOM_COMMAND emits.
func splitShellTokens(s string) []string {
	var out []string
	var cur strings.Builder
	quote := byte(0)
	for i := 0; i < len(s); i++ {
		c := s[i]
		if quote != 0 {
			if c == quote {
				quote = 0
				continue
			}
			if c == '\\' && i+1 < len(s) {
				cur.WriteByte(s[i+1])
				i++
				continue
			}
			cur.WriteByte(c)
			continue
		}
		switch c {
		case ' ', '\t':
			if cur.Len() > 0 {
				out = append(out, cur.String())
				cur.Reset()
			}
		case '\'', '"':
			quote = c
		case '\\':
			if i+1 < len(s) {
				cur.WriteByte(s[i+1])
				i++
			}
		default:
			cur.WriteByte(c)
		}
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}

// usesCmakeScriptMode reports whether the recovered custom-command runs
// cmake in script mode (`cmake [args ...] -P <script>`). cmake's script
// mode is the converter's hard refusal case: the script lives in the
// project's build dir (which is gone after convert-element-cmake exits),
// runs against cmake-specific variable state we can't reconstruct at
// Bazel time, and re-invokes cmake (no equivalent Bazel idiom). The
// audit recommendation is to rewrite the script in a real language so
// the genrule has a portable, sandbox-safe command.
//
// Detection tokenises the command (honouring `cd <dir> &&` prefixes and
// wrapper invocations like `env KEY=V cmake -DOUTPUT=... -P foo.cmake`)
// and reports true when the resolved driver is `cmake` and any
// subsequent argv token equals `-P`. The original substring match only
// caught the `<absolute-cmake-path> -P ` and `${CMAKE_COMMAND} -P `
// shapes; cmake invocations that pass `-D...` cache vars before `-P`
// (a common pattern for packages that pre-resolve the output basename
// inside the script — libpng's pnglibconf, etc.) slipped through and
// landed in BUILD.bazel as a genrule whose `cmd` referenced the
// build-dir's now-deleted absolute paths.
func usesCmakeScriptMode(cmd string) bool {
	tokens := splitShellTokens(cmd)
	// Strip a leading `cd <dir> && ` (the conventional ninja prefix
	// for cmake-emitted custom commands). splitShellTokens flattens
	// `&&` into a separator-style token, so we look for it and reset
	// the head if seen.
	for i, tok := range tokens {
		if tok == "&&" {
			tokens = tokens[i+1:]
			break
		}
	}
	// Skip env-style wrappers (KEY=VAL ... cmake -P) the same way
	// extractDriver does. Mirrors that helper's logic so the two
	// detectors agree on what counts as the real driver.
	wrappers := map[string]bool{
		"env":     true,
		"sh":      true,
		"bash":    true,
		"taskset": true,
		"nice":    true,
		"ionice":  true,
	}
	for len(tokens) > 0 {
		first := tokens[0]
		base := filepath.Base(first)
		if wrappers[base] {
			tokens = tokens[1:]
			for len(tokens) > 0 {
				t := tokens[0]
				if strings.HasPrefix(t, "-") || strings.Contains(t, "=") {
					tokens = tokens[1:]
					continue
				}
				break
			}
			continue
		}
		break
	}
	if len(tokens) == 0 {
		return false
	}
	driver := filepath.Base(tokens[0])
	// ${CMAKE_COMMAND} survives tokenization as a literal token —
	// CommandFor doesn't expand cmake's own variable references when
	// COMMAND is a verbatim substitution. Accept both the resolved
	// `cmake` driver and the unsubstituted form so neither variant
	// slips through.
	if driver != "cmake" && tokens[0] != "${CMAKE_COMMAND}" {
		return false
	}
	for _, t := range tokens[1:] {
		if t == "-P" {
			return true
		}
	}
	return false
}

// extractCmakeScriptPath returns the script-mode argument from a
// `cmake [args ...] -P <script> [extra ...]` command — i.e. the
// token immediately after `-P`. Returns "<unknown-script>" when no
// `-P` is present (the caller is responsible for only invoking this
// after usesCmakeScriptMode returned true; the fallback is a defensive
// guard, not the expected path). Used by the
// UnsupportedCustomCommandScript failure (#207) so operators see
// which script to rewrite — not just the consuming target.
func extractCmakeScriptPath(cmd string) string {
	tokens := splitShellTokens(cmd)
	for i, tok := range tokens {
		if tok == "-P" && i+1 < len(tokens) {
			return tokens[i+1]
		}
	}
	return "<unknown-script>"
}

// hasCmakeE returns true if the command invokes a cmake -E sub-tool that we
// translate to a native Bazel idiom.
func hasCmakeE(cmd string) bool {
	for _, tok := range []string{
		"/usr/bin/cmake -E ",
		"${CMAKE_COMMAND} -E ",
		" cmake -E ",
	} {
		if strings.Contains(cmd, tok) {
			return true
		}
	}
	return false
}

// toolFromTarget returns true if the command's driver tool is itself the
// output of another build statement in the graph (i.e. an in-codebase
// generator binary).
func toolFromTarget(b *ninja.Build, g *ninja.Graph) bool {
	cmd, ok := ninja.CommandFor(g, b)
	if !ok {
		return false
	}
	driver := extractDriver(cmd)
	if driver == "unknown" {
		return false
	}
	// Try the basename first (driver is a filename); look up any output
	// in the index whose basename matches.
	for out := range g.OutputIndex {
		if filepath.Base(out) == driver {
			return true
		}
	}
	return false
}

// isSourceOnly returns true if the build statement's outputs are all source-
// or header-shaped paths (used as srcs/hdrs by a downstream cc rule). The
// converter doesn't have full transitive consumer info at this point; we
// approximate by extension.
func isSourceOnly(b *ninja.Build) bool {
	if len(b.Outputs) == 0 {
		return false
	}
	for _, o := range b.Outputs {
		ext := strings.ToLower(path.Ext(o))
		switch ext {
		case ".c", ".cc", ".cpp", ".cxx",
			".h", ".hh", ".hpp", ".hxx", ".inl",
			".s", ".S",
			".y", ".l":
		default:
			return false
		}
	}
	return true
}

// relativeIfInsideRelaxed is like relativeIfInside but accepts equality (the
// path itself being the root) — useful for build-statement outputs that are
// sometimes the whole build dir's relative path.
func relativeIfInsideRelaxed(root, abs string) (string, bool) {
	if !filepath.IsAbs(abs) {
		// Already relative — assume relative to the build dir, which is
		// what ninja outputs are.
		return filepath.ToSlash(abs), true
	}
	rel, err := filepath.Rel(root, abs)
	if err != nil {
		return "", false
	}
	rel = filepath.ToSlash(rel)
	if strings.HasPrefix(rel, "../") {
		return "", false
	}
	return rel, true
}
