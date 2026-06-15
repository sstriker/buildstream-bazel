package lower

import (
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/sstriker/buildstream-bazel/converter/internal/cmakerun"
	"github.com/sstriker/buildstream-bazel/converter/internal/failure"
	"github.com/sstriker/buildstream-bazel/converter/internal/ninja"
	"github.com/sstriker/buildstream-bazel/converter/internal/todos"
	"github.com/sstriker/buildstream-bazel/converter/ir"
	"github.com/sstriker/buildstream-bazel/internal/convmode"
	"github.com/sstriker/buildstream-bazel/internal/shadow"

	"github.com/sstriker/buildstream-bazel/internal/manifest"
)

// codegenContext carries state from genrule recovery and CTest
// classification back into the consuming target's lowering.
type codegenContext struct {
	// Imports is the cross-element imports manifest (nil-safe resolver;
	// may be nil in unit-shaped callers). The genrule tool lift uses it
	// to rewrite absolute IMPORTED_LOCATION tool paths to
	// $(execpath <label>) + tools entries (rewriteToolFromTarget).
	Imports *manifest.Resolver
	// HostPrefixDir is the on-disk synth-prefix root (Options.
	// HostPrefixDir). Orchestrator-emitted manifests key link_paths in
	// the ANCHORED ManifestPrefixAnchor form; the tool lift remaps
	// hostPrefix-rooted cmd tokens onto the anchor before LookupLinkPath,
	// mirroring the link-fragment channel's pre-lookup rewrite.
	HostPrefixDir string
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

	// SubParent maps a per-language sub-library's name (cc.Subs) to its
	// parent wrapper target's name, so the package-assignment pass can
	// co-locate each sub in its parent's sub-package. Without this the subs
	// default to the root package while the parent (and the sub's srcs +
	// the wrapper that deps on it) live in a sub-package — a cross-package +
	// private-visibility analysis error (LLVM's BLAKE3 _asm/_c splits).
	SubParent map[string]string

	// OutToGenrule maps a package-relative output path to the genrule
	// name that produces it. Used by the consumer side to add
	// has-cmake-codegen and to reference outputs by label.
	OutToGenrule map[string]string

	// OutToNativeConsumerDep maps a package-relative generated output to the
	// NAME of the native rule's CONSUMER target a #include of it should depend
	// on (e.g. foo.pb.h -> "foo_cc_proto"). Distinct from OutToGenrule: the
	// output is produced INSIDE the native rule (a cc_proto_library compiles
	// foo.pb.{cc,h} itself), not as a standalone genrule out — so a consumer
	// wires a DIRECT rule deps edge to this label, NOT the file-oriented
	// generated_includes textual_hdrs wrapper. Populated by the
	// codegen-recognizer dispatch (--recognize-codegen).
	OutToNativeConsumerDep map[string]string

	// OutToNativeConsumerPkg maps the same generated output to the element-
	// relative PACKAGE its producing native rule lands in (e.g. a/msg.pb.cc ->
	// "a"). Recorded alongside OutToNativeConsumerDep so rewriteNativeRuleConsumers
	// can PACKAGE-QUALIFY the consumer dep when the rule NAME is ambiguous — two
	// same-basename protos (a/msg.proto, b/msg.proto) both yield a "msg_cc_proto",
	// so a bare ":msg_cc_proto" relabels via the name-keyed map (clobbered) to the
	// wrong package. With the producer package known, the ambiguous case emits
	// //<base>/<pkg>:msg_cc_proto. The unambiguous (single-package) case keeps the
	// bare relative label, byte-identical.
	OutToNativeConsumerPkg map[string]string

	// GendirMarkedOuts records baked file(GENERATE) outputs whose
	// content embeds the @BSB_GENDIR@ marker (build-dir paths
	// re-anchored by reanchorResponseContent). A consuming genrule
	// must substitute the marker with $(GENDIR) at action time —
	// rewriteGeneratedSrcRefs emits the sed preamble for srcs in
	// this set.
	GendirMarkedOuts map[string]bool

	// RespfileHdrGroups memoizes the per-include-root header
	// filegroups responseFileSourceHdrGroups synthesizes (source-root
	// relative dir → filegroup name), so the ~40 wrap-hierarchy-style
	// genrules that all -I the same module dirs share one filegroup
	// per root instead of each listing hundreds of headers.
	RespfileHdrGroups map[string]string

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

	// bakeTodoDisposition lets a lift site override the conversion-todos
	// disposition for a baked target (keyed by target name), so two targets
	// carrying the same bake tag can differ — e.g. a hoisted VCS/identity/date
	// stamp (non-hermetic, wrong on rebuild) is todos.Actionable while a baked
	// check-probe stays the default todos.Improvement. Consumed by
	// emitBakeTodos. Empty unless a site sets an override.
	bakeTodoDisposition map[string]todos.Disposition

	// SeenBuilds dedupes recovered builds when multiple targets reference
	// the same generated source.
	SeenBuilds map[*ninja.Build]string

	// ScriptBakeRuns is the parallel pre-warm's per-build cmake -P
	// execution result (nil = ran clean), filled by prewarmScriptBakes
	// and consulted by bakeCmakeScriptGenrule so a pre-warmed script
	// isn't re-executed serially. A build absent from the map runs
	// serially (the historical path).
	ScriptBakeRuns map[*ninja.Build]error

	// ConsumedBuildRel is the File-API demand side for the
	// unspecified-output execute_process lift: every codemodel target
	// source anchored under the build dir, build-relative. NinjaOuts is
	// the matching exclusion: every path a ninja edge produces (build-time
	// codegen, already recovered elsewhere). A consumed build file with no
	// ninja producer is an orphan only configure-time codegen can explain.
	// Both populated by ToIR before recoverExecuteProcess runs; nil maps
	// degrade to "no orphans" (the lift declines).
	ConsumedBuildRel map[string]bool
	NinjaOuts        map[string]bool

	// Nested-cmake lift state (see nested_cmake.go). NestedConfigureSink
	// maps each detected nested build dir (outer-build-relative) to its
	// trace-recorded source dir; recoverExecuteProcess fills it, the
	// driver stages File API queries there for the warm second pass.
	// NestedArtifactDeps maps `<nestedBuildRel>/<artifact>` to the merged
	// nested target's label so outer link fragments naming the nested
	// archive wire to it. NestedLifted records the builds the second pass
	// actually merged, gating the not-lifted warning/todo.
	NestedConfigureSink map[string]string
	NestedArtifactDeps  map[string]string
	NestedLifted        map[string]bool

	// Dead-capture analysis state (see execute_process_dead_capture.go):
	// CaptureRefusalSink collects capture-bearing refusals' variable
	// names for the driver; DeadCaptureVars are the proven-dead ones the
	// re-lower clears before classification.
	CaptureRefusalSink map[string]bool
	DeadCaptureVars    map[string]bool

	// FileWriterIndex maps build-dir-relative paths to their traced
	// file() writer calls (trace order preserved); the build-dir
	// recovery consults it before the on-disk byte-bake. See
	// build_dir_writer_lift.go.
	FileWriterIndex map[string][]shadow.FileWriterCall

	// CMakeVars is the configure's variable-namespace dump (the
	// dump-vars hook). The writer-index stamp lift uses it as the
	// pickValues fallback when extracting frozen values from a
	// stamp-bearing file(WRITE) template. LiftConfigureFile mirrors
	// --lift-configure-file: the file(WRITE) stamp lift emits a
	// cmake_configure_file rule (needs the staged tool), so it only
	// fires when the operator opted into the lift tier — otherwise the
	// frozen write_file bake stands, exactly like configure_file.
	CMakeVars         map[string]string
	LiftConfigureFile bool

	// LiftDownload mirrors --lift-download (Options.LiftDownload): when
	// true, bakeBuildDirFile rewires a recovered file(DOWNLOAD) producer
	// to a genrule sourcing @<repo>//file from an http_file repo instead
	// of byte-baking the fetched bytes. See build_dir_source_bake.go.
	LiftDownload bool

	// RecognizeCodegen opts into the codegen-recognizer registry (Options.
	// RecognizeCodegen): when true, lowerStandaloneCustomCommands routes a
	// recovered codegen command a recognizer claims to its native rule(s)
	// instead of a genrule.
	RecognizeCodegen bool

	// ExtraRecognizers are operator-supplied codegen recognizers loaded from
	// --recognizers Starlark files (Options.ExtraCodegenRecognizers). Consulted
	// after the built-ins (first-party wins), and only when RecognizeCodegen.
	ExtraRecognizers []CodegenRecognizer

	// NativeRuleSubPackage maps a recognized native rule's target name to the
	// element-relative directory it should land in — the package owning the
	// codegen output (e.g. foo_proto -> "pkg/a" for pkg/a/a.pb.cc). The
	// recognizer names the rule + sets srcs by BASENAME, so it must be placed in
	// the output's package for that basename to resolve and for cross-package
	// proto imports to line up (//pkg/a:a_proto). Merged into Package.SubPackages
	// after the native targets are emitted.
	NativeRuleSubPackage map[string]string

	// recognizedConsumerByInput dedups recognized native rules by EMISSION
	// identity (codegenRuleKey: driver + sorted input set + the produced rule
	// names). The SAME input run into different output dirs yields the same rule
	// names → ONE canonical native rule (the out-dir duplication collapses); a
	// repeat wires its outputs to the already-emitted rule via the keyed
	// consumer-dep label. The same input through DIFFERENT, complementary
	// recognizers (protoc --cpp_out vs a sibling --grpc_out) produces DIFFERENT
	// names → distinct keys → both emit. DIFFERENT inputs get distinct keys too.
	recognizedConsumerByInput map[string]string
	// recognizedNameOwner maps a placed native-rule identity (subpackage + name)
	// to the input key that owns it, so a DIFFERENT input that would emit a
	// colliding target name (a recognizer naming bug — e.g. a fixed rule name
	// across distinct inputs) is detected and falls back to the generic genrule
	// rather than emitting a load-breaking duplicate.
	recognizedNameOwner map[string]string

	// LiftDerivedCodegen opts the derived-name stem-match recovery into a live
	// genrule re-run (cd $(RULEDIR)) instead of the convert-time byte-bake, when
	// placement is sound (Options.LiftDerivedCodegen / --lift-derived-codegen).
	// Off by default — the bake is the safe fallback.
	LiftDerivedCodegen bool

	// Fidelity is the operator dial ("strict"/"best-effort") governing how a
	// non-faithful recovery is handled (Options.Fidelity). Today it gates the
	// recognizer cross-check mismatch: strict refuses (loud stub), best-effort
	// (or "") falls back to the generic genrule.
	Fidelity convmode.Fidelity

	// FileWriterTemplates maps a build-dir-relative path to the
	// NON-EXPANDED composed content of its file(WRITE/APPEND) chain —
	// the warm-pass harvest where a `${GIT_SHA}` reference survives
	// verbatim (--trace-expand would substitute it away). A
	// stamp-bearing file(WRITE) is a configure_file in disguise:
	// non-expanded content is the template, expanded content the
	// rendered output. The writer lift routes those through the
	// configure_file stamp_values machinery (live workspace-status
	// re-read) instead of baking the frozen revision. Empty when no
	// non-expanded trace was captured (offline / no warm pass) → the
	// frozen bake stands.
	FileWriterTemplates map[string]string

	// DownloadLifts records each file(DOWNLOAD) output baked at convert
	// time (the hermetic default) so emitToIRDiagnostics can surface a
	// structured `download` todo carrying the ready-to-paste http_file
	// MODULE stanza (url + integrity from the traced EXPECTED_HASH) for
	// an operator who wants the repo-rule form. See download_lift.go.
	DownloadLifts []downloadLiftRecord

	// HostCodegenTools records each recovered genrule whose driver is an
	// un-hermeticized HOST codegen tool — a generator with no native rule that
	// wasn't swapped to $(execpath)/$(location) and isn't a benign cmake -E /
	// shell builtin. emitHostCodegenToolTodos surfaces a structured
	// `host-codegen-tool` todo (grouped by driver) telling the operator which
	// imports-manifest `tools` entry to author. See host_codegen_tool_todo.go.
	HostCodegenTools []hostCodegenToolNote

	// No-silent-drops accounting + the build-dir on-disk bake (see
	// build_dir_source_bake.go). ElidedSources records every
	// codemodel-referenced source the lowering dropped WITHOUT recovery
	// (surfaced as one stderr aggregate + source-elided todos);
	// BuildDirHdrWalked / BuildDirBakedHdrs cache the consumed-include
	// header walk and its baked rel → rule-name registrations.
	ElidedSources     []elidedSourceRecord
	BuildDirHdrWalked map[string]bool
	BuildDirBakedHdrs map[string]string

	// HeaderWalkCache memoizes filesystem walks of include directories
	// across targets within one lower-element invocation. Keyed on the
	// absolute include-dir path; value is the package-relative header
	// list for that dir. Multiple targets in a project commonly share
	// include roots (`include/`, `src/`); without the cache each
	// target re-walks every shared dir.
	HeaderWalkCache map[string][]string

	// FilteredInternalCmds collects the cmake command edges the
	// standalone-genrule pass drops (install / uninstall / regen / cpack /
	// clean / dashboard / ide-stub, create_symlink tool/SONAME/manpage aliases,
	// and source-less cmake -E copy edges) — keyed by the edge's first output,
	// valued by category. These
	// have no Bazel analogue so dropping is correct, but ToIR emits one
	// aggregated stderr breadcrumb at the end (alongside MissingIncludeDirs)
	// so an operator auditing a conversion sees WHAT was filtered rather than
	// the drop being silent.
	FilteredInternalCmds map[string]string

	// UnreadableConfigureOutputs collects configure_file outputs (keyed by
	// build-relative path) the recovery couldn't read back in a LIVE convert
	// — the configure ran but the rendered file isn't readable, so we'd
	// silently drop an output a consumer needs. These note an uncertain skip
	// (emitExecuteProcessRefusalTodos's configure_file sibling); the OFFLINE
	// no-build-dir degradation (a fixture stash that doesn't include every
	// output) is NOT recorded here, since that's a by-design trace-only skip.
	UnreadableConfigureOutputs map[string]string

	// UnresolvedRecoveryInputs collects configure-time recovery inputs the
	// converter expected to resolve but couldn't (an unanchorable
	// configure_file output, an unreadable declared .proto or nested header) —
	// uncertain drops surfaced as conversion-todos + a stderr breadcrumb via
	// warnUnresolvedRecoveryInputs. The cross-family sibling of
	// UnreadableConfigureOutputs. See unresolved_recovery_inputs.go.
	UnresolvedRecoveryInputs []unresolvedRecoveryInput

	// OutOfTreeExecNotes collects the uncertain out-of-tree execute_process
	// calls partitionOutOfTreeExec set aside to NOTE (rather than lift): a
	// build-dir location with no codemodel sources to anchor a lift, or a
	// find_package prefix-tree probe. recoverConfigureTimeArtifacts fills it
	// (the codemodel-source-backed calls are spliced into the lift instead);
	// emitToIRDiagnostics surfaces it via warnOutOfTreeExecuteProcess. See
	// out_of_tree_execute_process.go.
	OutOfTreeExecNotes []outOfTreeExecNote

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

	// ExecArtifacts is the subset of ArtifactToName keys whose target is an
	// EXECUTABLE. The `VAR=<artifact-path>` tool lift (e.g. VTK's
	// -DEXE_SQLITE3=bin/Debug/sqlitebin-9.4) is gated on this so a library
	// artifact embedded in an arg (a linker flag, a data path) isn't
	// mis-lifted into the genrule's `tools` as if it were a runnable tool.
	ExecArtifacts map[string]bool

	// BazelPackagePath is the element's Bazel package path (e.g.
	// "elements/curl") — the exec-root prefix a genrule cmd's
	// source-tree inputs / `-I` roots need so they resolve at the
	// Bazel exec root rather than as bare project-relative paths.
	// Threaded here (like ArtifactToName) so recoverGenrule's ninja
	// path can hand it to rewriteGenruleCmd without a signature
	// change — the standalone path receives it as a parameter. Empty
	// for offline-replay / convert-at-root callers.
	BazelPackagePath string

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

// newCodegenContextFor wraps newCodegenContext with the Options-derived
// fields the genrule recovery paths read (the imports manifest + the
// synth-prefix root for the anchored tool-path remap).
func newCodegenContextFor(opts Options) *codegenContext {
	cc := newCodegenContext()
	cc.Imports = opts.Imports
	cc.HostPrefixDir = opts.HostPrefixDir
	return cc
}

func newCodegenContext() *codegenContext {
	return &codegenContext{
		OutToGenrule:               map[string]string{},
		OutToNativeConsumerDep:     map[string]string{},
		OutToNativeConsumerPkg:     map[string]string{},
		NativeRuleSubPackage:       map[string]string{},
		recognizedConsumerByInput:  map[string]string{},
		recognizedNameOwner:        map[string]string{},
		GendirMarkedOuts:           map[string]bool{},
		RespfileHdrGroups:          map[string]string{},
		CcEmbedSourceToHeader:      map[string]string{},
		StampVars:                  map[string]string{},
		bakeTodoDisposition:        map[string]todos.Disposition{},
		SeenBuilds:                 map[*ninja.Build]string{},
		HeaderWalkCache:            map[string][]string{},
		MissingIncludeDirs:         map[string]bool{},
		FilteredInternalCmds:       map[string]string{},
		UnreadableConfigureOutputs: map[string]string{},
		SubParent:                  map[string]string{},
		NestedConfigureSink:        map[string]string{},
		NestedArtifactDeps:         map[string]string{},
		NestedLifted:               map[string]bool{},
		BuildDirHdrWalked:          map[string]bool{},
		BuildDirBakedHdrs:          map[string]string{},
	}
}

// recoverCmakeScriptGenrule handles the `cmake -P <script>` custom-command case
// of recoverGenrule (selected by usesCmakeScriptMode). The operator can opt into
// the cmake-P lift by staging the runner tool and passing
// --cmake-script-runner=<label>. Soundness caveats apply: scripts that hardcode
// absolute paths (configure_file-derived scripts with `set(SRCDIR "/abs/path")`)
// won't resolve under Bazel's sandbox; parameter-driven scripts (VTK's
// vtkHashSource shape) work cleanly. See docs/design/generator-parity-gaps.md's
// "cmake -P lift" entry for the limitation details.
//
// It tries, in order: the native cc_embed / cc_hash recognizers (opt-in), the
// convert-time bake lift, and the runner lift — returning the consumed source's
// build-rel output (relOut) + the emitted target name on success. When none
// apply it returns a typed Tier-1 UnsupportedCustomCommandScript failure naming
// the script + every opt-in path.
func (cc *codegenContext) recoverCmakeScriptGenrule(b *ninja.Build, cmd, cmakeSrc, buildDir, relOut string, g *ninja.Graph) (string, string, error) {
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
	// Trace-recurse codegen recovery (opt-in --recognize-codegen +
	// --cmake-script-trace): the real tool may live in an execute_process INSIDE
	// the script. Re-trace, recognize it, and lower to the native rule — higher
	// fidelity than the bake/runner genrule, so it runs first. Declines (offline /
	// flags off / output not recovered) fall through to bake/runner/refuse. See
	// recoverCmakeScriptCodegen (P2 of the wrapper-codegen coverage).
	if name, ok := cc.recoverCmakeScriptCodegen(b, cmd, script, cmakeSrc, buildDir, relOut); ok {
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
		name, reason, ok := bakeCmakeScriptGenrule(cc, b, cmd, script, buildDir, g)
		if ok {
			// Return the REQUESTED output, not the bake's primary out:
			// a multi-output script (vtkEncodeString writes a .h + the
			// symbol-defining .cxx) is consumed once per output, and
			// handing every consumer the primary out misroutes the
			// .cxx to hdrs (header extension) — the shader-string
			// definitions then compile nowhere and every referencing
			// link fails. Same contract as the cc_embed/cc_hash
			// branches above; the sibling outputs — implicit
			// (BYPRODUCTS) ones included, per genruleOuts — are all
			// registered in OutToGenrule by the bake.
			return relOut, name, nil
		}
		liftReason = reason
	}
	if cc.CMakeScriptRunner != "" {
		name, reason, ok := liftCmakeScriptGenrule(cc, b, cmd, script, cmakeSrc, buildDir)
		if ok {
			// Same requested-output contract as the bake branch.
			return relOut, name, nil
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
		return cc.recoverCmakeScriptGenrule(b, cmd, cmakeSrc, buildDir, relOut, g)
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

	// Exec-root anchoring: a genrule cmd runs at the Bazel exec root, so a
	// source-tree input / `-I` root referenced bare (cmakeSrc-relative) must
	// carry the element's package prefix (cc.BazelPackagePath, e.g.
	// elements/curl/include/curl/curl.h) to resolve — mirroring the
	// standalone-custom-command path. Empty BazelPackagePath (offline-replay /
	// convert-at-root) leaves the bare shape unchanged. umbrellaPrefix stays ""
	// here: the per-target recovery path isn't reached under the workspace-root
	// umbrella promotion (that surfaces on the standalone path).
	rewrittenCmd := rewriteGenruleCmd(cmd, cmakeSrc, buildDir, "", cc.BazelPackagePath)
	// Pre-tool-swap cmd for the recognizer dispatch: the swap can rewrite the
	// DRIVER to $(execpath <label>) (--tool-conventions / manifest tools map),
	// which would hide the driver the recognizer matches on (e.g. protoc). The
	// native rule is higher-fidelity than the swapped genrule, so the recognizer
	// keys on the pre-swap driver. See standalone_genrules.go for the same guard.
	preToolSwapCmd := rewrittenCmd
	rewrittenCmd, tools := rewriteToolFromTarget(rewrittenCmd, cc.ArtifactToName, cc.ExecArtifacts, cc.Imports, cc.HostPrefixDir)
	// Anchor declared outputs to $(RULEDIR)/<out> so a cmd that names its
	// output as a literal arg (curl's `perl mk-lib1521.pl < curl.h lib1521.c`,
	// where the script writes to argv) writes under bazel-out rather than a
	// bare exec-root path bazel rejects as a missing output. anchorGenrule-
	// OutputsToRuledir only rewrites literal occurrences, so a cmd that emits
	// via `> $@` (no literal output token) is a no-op — root-level outputs are
	// now anchored too (was subdir-only), which the literal-occurrence scoping
	// keeps churn-free for the stdout-redirect recovered genrules.
	if len(outs) > 0 {
		rewrittenCmd = anchorGenruleOutputsToRuledir(rewrittenCmd, outs)
	}
	// Build-dir staging-copy reanchor: when the raw command ran in a build-dir
	// subdir of CONFIGURE-time-copied inputs (`cd <buildDir>/<sub> && tool -I .
	// <rel>` — grpc copies its .protos into <build>/protos/ at configure time
	// and runs protoc from there), stripping the cd stranded the cwd-relative
	// reads. Re-anchor them to the byte-identical source-tree inputs the genrule
	// already carries, drop the producerless copies, and anchor the output-root
	// dir. See reanchorBuildDirCopyGenrule.
	var copyTools []string
	rewrittenCmd, srcs, copyTools = reanchorBuildDirCopyGenrule(cmd, rewrittenCmd, srcs, outs, cmakeSrc, buildDir, cc.BazelPackagePath, cc)
	tools = append(tools, copyTools...)
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
	// Route the final emit through the shared recognizer chokepoint: a consumed
	// codegen custom-command a recognizer claims (a protoc add_custom_command
	// whose .pb.cc is a target src) lowers to its native rule instead of this
	// genrule. The ninja edge RECORDED the outputs, so the recognizer's output
	// cross-check validates (no supply mode needed here). On a match the sink
	// registers OutToNativeConsumerDep, and rewriteNativeRuleConsumers later
	// strips the generated src from the consuming target + wires the deps edge;
	// OutToGenrule is registered only in the genrule fallback. Flag-off / no-match
	// → the genrule unchanged.
	recoCmd := codegenCommandFrom(preToolSwapCmd, srcs, outs, cc.BazelPackagePath)
	recoCmd.ProtoDeps = protoImportLabels(recoCmd.Srcs, recoCmd.Outs, cmakeSrc, cc.BazelPackagePath)
	tgts, recognized := recognizeOrGenrule(cc, recoCmd, gen)
	cc.Genrules = append(cc.Genrules, tgts...)
	cc.SeenBuilds[b] = name
	if !recognized {
		for _, o := range outs {
			cc.OutToGenrule[o] = name
		}
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
	return "gen_" + sanitizePathToNameStem(first)
}

// sanitizePathToNameStem normalizes a path into a Bazel-target-name-safe stem:
// ToSlash, drop a leading "./", then map every byte outside [A-Za-z0-9_] to '_'.
// Callers prepend their own collision-avoidance prefix (gen_ / exec_ / …); the
// distinct prefixes are deliberate, keeping the per-pass name spaces disjoint.
func sanitizePathToNameStem(rel string) string {
	rel = filepath.ToSlash(rel)
	rel = strings.TrimPrefix(rel, "./")
	var sb strings.Builder
	for i := 0; i < len(rel); i++ {
		c := rel[i]
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

// genruleOuts returns build statement outputs — explicit AND implicit — as
// package-relative paths. Implicit outs matter because BuildFor indexes
// them too: a consumer can reference a BYPRODUCTS file (ninja's
// `build out | byproduct :` shape), and a recovery that omits it from the
// emitted genrule's outs hands that consumer a path no target produces.
// Entries carrying unexpanded ninja variable references are dropped —
// cmake's Ninja generator shadows every real custom-command output with a
// `${cmake_ninja_workdir}<name>` implicit out, which has no meaning at
// Bazel emission time (same filtering as the standalone pass's
// filterOutVarRefs).
func genruleOuts(b *ninja.Build, buildDir string) []string {
	seen := map[string]struct{}{}
	var out []string
	for _, list := range [][]string{b.Outputs, b.ImplicitOuts} {
		for _, o := range list {
			if strings.Contains(o, "${") {
				continue
			}
			if rel, ok := relativeIfInsideRelaxed(buildDir, o); ok {
				if _, dup := seen[rel]; !dup {
					seen[rel] = struct{}{}
					out = append(out, rel)
				}
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

	tokens := stripWrapperPrefix(splitShellTokens(cmd))
	if len(tokens) == 0 {
		return "unknown"
	}
	if base := filepath.Base(tokens[0]); base != "" {
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

// stripWrapperPrefix drops leading shell-wrapper invocations (env, sh, bash,
// taskset, nice, ionice) and the KEY=VAL / -flag tokens an env-style wrapper
// carries, returning the tokens beginning at the real argv0. Shared by
// extractDriver and usesCmakeScriptMode so they agree on the driver.
func stripWrapperPrefix(tokens []string) []string {
	wrappers := map[string]bool{
		"env": true, "sh": true, "bash": true,
		"taskset": true, "nice": true, "ionice": true,
	}
	for len(tokens) > 0 {
		base := filepath.Base(tokens[0])
		if !wrappers[base] {
			break
		}
		// `sh -c "<script>"` / `bash -lc "<script>"` run an unparsed quoted
		// command string; keep the shell as argv0 rather than drilling into
		// the script, so the driver facet stays a stable binary name and not a
		// spaced command. Command-mode only — taskset's `-c <cpulist>` is
		// unrelated and must still strip.
		if (base == "sh" || base == "bash") && len(tokens) > 1 &&
			(tokens[1] == "-c" || tokens[1] == "-lc") {
			break
		}
		// A wrapper carries KEY=VAL pairs (env) and flags before the real
		// command; skip them until a clean argv0 appears. A flag that takes a
		// SEPARATE-token argument (taskset/ionice `-c <cpulist>`, nice/ionice
		// `-n <prio>`, env `-u <KEY>`) must skip its argument too — otherwise
		// the argument ("0"/"5"/"FOO") is mistaken for argv0, which both
		// mis-tags the driver and (worse) makes usesCmakeScriptMode miss a
		// wrapped `cmake -P`. No-argument flags (env `-i`) skip just the flag.
		// argFlags is exact-match, so attached forms (`-c0`, `--cpu-list=0`)
		// already carry their value in one token and need no peek.
		argFlags := map[string]bool{"-c": true, "-n": true, "-u": true}
		tokens = tokens[1:]
		for len(tokens) > 0 {
			t := tokens[0]
			if strings.Contains(t, "=") { // env KEY=VAL pair
				tokens = tokens[1:]
				continue
			}
			if strings.HasPrefix(t, "-") {
				tokens = tokens[1:]
				if argFlags[t] && len(tokens) > 0 {
					tokens = tokens[1:] // skip the flag's separate-token argument
				}
				continue
			}
			break
		}
	}
	return tokens
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
	// Skip env-style wrappers (KEY=VAL ... cmake -P) so the two detectors
	// agree on what counts as the real driver.
	tokens = stripWrapperPrefix(tokens)
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

// reanchorBuildDirCopyGenrule fixes the genrule shape where the raw custom
// command ran inside a build-dir subdir of CONFIGURE-time-copied inputs:
//
//	cd <buildDir>/<sub> && <tool> --out=<buildDir>/<R> -I . <rel...>
//
// grpc copies its src/proto/**.proto into <build>/protos/ at configure time and
// runs `cd protos && protoc --cpp_out=gens -I . src/proto/.../x.proto`. The
// copies have no ninja producer, so genruleSrcs carries BOTH the build-dir copy
// (`<sub>/<rel>`) and the byte-identical source-tree input (`<rel>`); after the
// cd-strip the cwd-relative reads (`-I .`, `<rel>`) point nowhere under Bazel's
// exec-root cwd.
//
// This re-anchors the command to read the SOURCE-TREE inputs the genrule already
// declares: `-I .` → `-I <pkg>`, each cwd-relative source `<rel>` → `<pkg>/<rel>`,
// a build-tool src referenced bare (an extension-less binary like
// grpc_cpp_plugin) → `$(execpath <tool>)`, the shared output-root dir `<R>` →
// `$(RULEDIR)/<R>`; and it drops the producerless `<sub>/<rel>` copies from srcs.
//
// Gated tightly on the `cd <buildDir>/<sub>` shape: a genrule whose raw command
// had no such cd (the corpus norm) is returned untouched, as is the pkg=="" /
// no-twin case.
func reanchorBuildDirCopyGenrule(rawCmd, cmd string, srcs, outs []string, cmakeSrc, buildDir, pkg string, cc *codegenContext) (string, []string, []string) {
	if pkg == "" || !strings.HasPrefix(rawCmd, "cd ") {
		return cmd, srcs, nil
	}
	end := strings.Index(rawCmd, " && ")
	if end < 0 {
		return cmd, srcs, nil
	}
	cdTarget := strings.TrimSpace(strings.TrimPrefix(rawCmd[:end], "cd "))
	sub, ok := relativeIfInside(buildDir, cdTarget)
	if !ok || sub == "" {
		return cmd, srcs, nil // not a cd into a build-dir subdir
	}
	sub = filepath.ToSlash(sub)
	var protocTool string

	srcSet := map[string]bool{}
	for _, s := range srcs {
		srcSet[s] = true
	}
	// Only act when at least one `<sub>/<rel>` copy has a source-tree twin also
	// declared — the signature of the configure-time-copy shape. Otherwise leave
	// the command alone (don't perturb unrelated cd-into-build-dir genrules).
	copyPrefix := sub + "/"
	hasTwin := false
	var kept []string
	for _, s := range srcs {
		if rel := strings.TrimPrefix(s, copyPrefix); rel != s && srcSet[rel] {
			hasTwin = true
			continue // drop the producerless copy
		}
		kept = append(kept, s)
	}
	if !hasTwin {
		return cmd, srcs, nil
	}

	// `-I .` / `-I.` (the cd dir) → the package root.
	cmd = strings.ReplaceAll(cmd, "-I . ", "-I "+pkg+" ")
	cmd = strings.ReplaceAll(cmd, "-I.", "-I"+pkg+"/")

	// Reanchor kept srcs referenced bare in the command. Longest-first so a path
	// that prefixes another isn't half-rewritten. A source FILE (has extension)
	// → its exec path under the package; an extension-less build tool → execpath.
	ordered := append([]string(nil), kept...)
	sort.Slice(ordered, func(i, j int) bool { return len(ordered[i]) > len(ordered[j]) })
	toolSet := map[string]bool{}
	for _, s := range ordered {
		if filepath.Ext(s) == "" {
			// An extension-less in-tree build tool (grpc_cpp_plugin): reference it
			// via $(execpath) and move it to the genrule's `tools` — keeping it in
			// `srcs` would make the emitter exports_files() a name that's already a
			// cc_binary rule ("source file conflicts with existing rule").
			cmd = replaceBareToken(cmd, s, "$(execpath "+s+")")
			toolSet[s] = true
		} else {
			cmd = replaceBareToken(cmd, s, pkg+"/"+s)
		}
	}

	// Anchor the shared output-root dir (e.g. "gens") to $(RULEDIR)/<root>.
	if root := commonRootDir(outs); root != "" {
		cmd = anchorOutputRootDir(cmd, root)
	}

	// Hermetic protoc: the recovered command drives a HOST protoc by absolute
	// path (find_package(Protobuf) → `/tmp/.../protoc-31.1.0`). The Bazel build
	// links the BCR protobuf RUNTIME, which MVS resolves independently (rules_cc
	// pulls it to 33.4), so host-protoc gencode fails its PROTOBUF_VERSION guard
	// against the runtime headers. Generate with the BCR protoc instead
	// (`$(execpath @protobuf//:protoc)`) so gencode matches the linked runtime —
	// and the genrule needs no host protoc at action time. Only fires on this
	// proto-codegen shape (a `protoc`-named absolute driver).
	protocBasename := ""
	if drv, rest, found := strings.Cut(cmd, " "); found {
		if filepath.IsAbs(drv) && strings.HasPrefix(filepath.Base(drv), "protoc") {
			protocBasename = filepath.Base(drv)
			cmd = "$(execpath @protobuf//:protoc) " + rest
			protocTool = "@protobuf//:protoc"
		}
	}

	// Drop host-tool phantom srcs: a bare-basename src (no "/") that the command
	// references only as the basename of an ABSOLUTE host path (e.g. genruleSrcs'
	// basename-fallback of `/tmp/protobuf-install/bin/protoc-31.1.0` → a
	// `protoc-31.1.0` src). Bazel can't stage a basename-only label, and the tool
	// is invoked by absolute path, so the src is a dangling input. A real in-tree
	// tool ref (grpc_cpp_plugin, now `$(execpath grpc_cpp_plugin)`) is preceded by
	// a space, not a `/`, so it's preserved.
	// also move the execpath'd in-tree tools out of srcs into the returned tools.
	kept2 := kept[:0:0]
	var tools []string
	if protocTool != "" {
		tools = append(tools, protocTool)
	}
	for _, s := range kept {
		if toolSet[s] {
			tools = append(tools, s)
			continue
		}
		// Host-tool phantom basename: the genruleSrcs basename-fallback of the
		// host-absolute driver. Dropped whether the driver was swapped to the BCR
		// protoc (protocBasename) or left as an absolute host path (cmd carries
		// `/<s>`).
		if s == protocBasename {
			continue
		}
		if !strings.Contains(s, "/") && strings.Contains(cmd, "/"+s) {
			continue
		}
		kept2 = append(kept2, s)
	}
	kept = kept2

	// Proto import closure: cmake doesn't record a proto's `import "x.proto"`
	// dependencies as ninja inputs, so a service proto that imports a sibling
	// (grpc's service.proto imports channelz/v2/channelz.proto) is missing the
	// imported file from the genrule's staged inputs. Walk each kept `.proto`
	// src's transitive imports (resolved under cmakeSrc, the `-I <pkg>` root) and
	// add any that exist on disk and aren't already declared.
	kept = append(kept, protoImportClosure(kept, cmakeSrc, cc)...)

	return cmd, kept, tools
}

// protoImportClosure returns the source-tree-relative paths of the protos
// transitively `import`ed by the `.proto` entries in srcs, that exist under
// cmakeSrc and aren't already in srcs. Imports are resolved relative to the
// source root (the `-I <pkg>` include root the reanchored command uses), which
// is grpc's `import "src/proto/..."` convention.
func protoImportClosure(srcs []string, cmakeSrc string, cc *codegenContext) []string {
	if cmakeSrc == "" {
		return nil
	}
	have := map[string]bool{}
	for _, s := range srcs {
		have[s] = true
	}
	var queue []string
	for _, s := range srcs {
		if strings.HasSuffix(s, ".proto") {
			queue = append(queue, s)
		}
	}
	var added []string
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		data, err := os.ReadFile(filepath.Join(cmakeSrc, cur))
		if err != nil {
			// A declared .proto src we're walking for transitive imports
			// can't be read: its imports may now be missing from the genrule
			// srcs. Note the uncertain drop rather than skipping silently.
			// `cur` is source-tree-relative — leak-safe for the report. (The
			// well-known-type Stat miss below stays a confident silent skip:
			// that proto genuinely lives under a -I include root, not the
			// source tree.)
			cc.noteUnresolvedRecoveryInput(unresolvedProtoImportUnreadable, cur)
			continue
		}
		for _, imp := range parseProtoImports(string(data)) {
			if have[imp] {
				continue
			}
			if _, err := os.Stat(filepath.Join(cmakeSrc, imp)); err != nil {
				continue // not a source-tree proto (a well-known type from -I include)
			}
			have[imp] = true
			added = append(added, imp)
			queue = append(queue, imp)
		}
	}
	sort.Strings(added)
	return added
}

// parseProtoImports extracts the paths from `import "path";` / `import public
// "path";` / `import weak "path";` statements in a .proto file's text.
func parseProtoImports(text string) []string {
	var out []string
	for _, line := range strings.Split(text, "\n") {
		t := strings.TrimSpace(line)
		if !strings.HasPrefix(t, "import") {
			continue
		}
		i := strings.IndexByte(t, '"')
		if i < 0 {
			continue
		}
		j := strings.IndexByte(t[i+1:], '"')
		if j < 0 {
			continue
		}
		out = append(out, t[i+1:i+1+j])
	}
	return out
}

// replaceBareToken replaces every whole-token occurrence of tok in s with repl.
// "Whole token" = the byte before tok is start-of-string, space, '=', or ':',
// and the byte after is end-of-string or space. Avoids mangling tok as a
// substring of a longer path/flag.
func replaceBareToken(s, tok, repl string) string {
	if tok == "" {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); {
		if strings.HasPrefix(s[i:], tok) {
			leftOK := i == 0 || s[i-1] == ' ' || s[i-1] == '=' || s[i-1] == ':'
			rEnd := i + len(tok)
			rightOK := rEnd == len(s) || s[rEnd] == ' '
			if leftOK && rightOK {
				b.WriteString(repl)
				i = rEnd
				continue
			}
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

// commonRootDir returns the first path component shared by every out, or "" if
// the outs don't all share one (so nothing single-component is anchored by
// accident).
func commonRootDir(outs []string) string {
	root := ""
	for _, o := range outs {
		o = filepath.ToSlash(o)
		i := strings.IndexByte(o, '/')
		if i <= 0 {
			return "" // a root-level out: no shared subdir to anchor
		}
		r := o[:i]
		if root == "" {
			root = r
		} else if root != r {
			return ""
		}
	}
	return root
}

// anchorOutputRootDir rewrites a bare output-root dir token (root) to
// $(RULEDIR)/root where it sits as an argument value — preceded by '=' or ':'
// (e.g. `--cpp_out=gens`, `--grpc_out=...:gens`) and followed by space or
// end-of-string. The already-$(RULEDIR)-prefixed and `/`-followed forms are left
// alone (anchorGenruleOutputsToRuledir handles full output paths).
func anchorOutputRootDir(cmd, root string) string {
	var b strings.Builder
	for i := 0; i < len(cmd); {
		if strings.HasPrefix(cmd[i:], root) && i > 0 && (cmd[i-1] == '=' || cmd[i-1] == ':') {
			rEnd := i + len(root)
			if rEnd == len(cmd) || cmd[rEnd] == ' ' {
				b.WriteString("$(RULEDIR)/")
				b.WriteString(root)
				i = rEnd
				continue
			}
		}
		b.WriteByte(cmd[i])
		i++
	}
	return b.String()
}
