package lower

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/sstriker/buildstream-bazel/converter/ir"
	"github.com/sstriker/buildstream-bazel/internal/convmode"
)

// CodegenCommand is a recovered codegen custom-command a CodegenRecognizer
// inspects. It's the converter's authoritative view of what the build ran — the
// DRIVER tool + its argv — which is the decisive signal that lets the converter
// recognize a generator by tool (not by ambiguous file extension) and emit the
// idiomatic native Bazel rule. Populated by the codegen-lowering dispatch.
type CodegenCommand struct {
	// Driver is the basename of the generator tool (argv[0]), e.g. "protoc".
	Driver string
	// Args is the tool's argv after the driver — flags + positional inputs.
	Args []string
	// Srcs are the recovered input sources (package-relative), e.g. the .proto.
	Srcs []string
	// InputFiles, when set, is the COMPLETE recovered input set — source-tree AND
	// generated/build-dir inputs (a tool whose input is itself produced by another
	// rule) — used to identify the invocation for dedup. Srcs alone is
	// source-tree-only on the execute_process path, so two calls on DISTINCT
	// generated inputs would otherwise share an (empty) source-src set and collapse.
	// Empty on paths where Srcs already lists every input (the custom-command
	// genrule srcs include generated inputs), where the key falls back to Srcs.
	InputFiles []string
	// Outs are cmake's RECORDED outputs (package-relative) — the cross-check
	// the recognizer validates its derived output set against.
	Outs []string
	// DiscoveredOutputs is the converter's best discovery of what the tool
	// ACTUALLY produced — the output set the GENERIC genrule fallback would
	// declare: the genrule's `outs` on the custom-command / standalone paths, or
	// the on-disk enumeration under the tool's output directory on the
	// execute_process path. Unlike Outs (a convention cross-check), it carries no
	// derivation assumption — feed it to a recognizer whose tool derives its
	// outputs from the INPUT CONTENTS (so no fixed naming convention predicts
	// them): Lower can return this set verbatim, a subset, or a transformed set
	// as DerivedOutputs / its rule's outs. It's in the SAME frame as this path's
	// outputs (package-relative on the custom-command path, output-dir-relative
	// on the execute_process path — matching Outs / DerivedOutputs respectively).
	// Empty when the front-end couldn't discover an output set.
	DiscoveredOutputs []string
	// Pkg is the Bazel package path the codegen lives in.
	Pkg string
	// ProtoDeps are the already-resolved proto_library labels for this input's
	// transitive `import`s (computed by the dispatch via protoImportClosure +
	// the package layout). The proto recognizer threads them onto proto_library
	// deps; empty for an import-free proto.
	ProtoDeps []string
	// SiblingCppProto is set by the dispatch when ANOTHER protoc --cpp_out call
	// for this same .proto exists in the package — so a grpc-ONLY call can
	// reference the proto_library + cc_proto_library that sibling emits instead
	// of re-emitting (and double-producing) them. See grpcOnlyRecognizer.
	SiblingCppProto bool
}

// CodegenResult is what a recognizer emits in place of the generic genrule: the
// native rule target(s), and the label(s) a consumer of the generated outputs
// should depend on (so the dispatch can rewire `#include`-driven consumer deps).
type CodegenResult struct {
	Targets      []ir.Target
	ConsumerDeps []string
	// DerivedOutputs is the package-relative output set the recognizer derives
	// from the tool convention (protoc `foo.proto` → `foo.pb.{cc,h}`). The
	// custom-command paths already have cmake's recorded outputs and ignore
	// this; the execute_process path — where the outputs aren't recorded in the
	// argv (protoc `--cpp_out=DIR`) — uses it as the OUTPUT AUTHORITY's supplied
	// set, corroborated against on-disk + codemodel evidence by the caller.
	DerivedOutputs []string
	// SubPackage is the element-relative directory the native rule(s) should be
	// placed in (the package owning the codegen output). "" lets the dispatch
	// fall back to the output's dir. The protoc recognizer sets it to the
	// .proto's own directory so basename srcs resolve AND a rebased
	// `--proto_path` (proto under a sub-dir, output named relative to the root)
	// places the rule where the .proto actually lives, paired with
	// strip_import_prefix so the import path stays proto_path-relative.
	SubPackage string
}

// CodegenRecognizer maps a recovered codegen custom-command to the idiomatic
// native Bazel rule(s) — the registry's extensibility seam (ROADMAP #632).
// Adding a tool (flatc/thrift/moc/…) is implementing one of these and
// registering it; no native ir.Kind / emit path per tool (that's KindNativeRule).
type CodegenRecognizer interface {
	// Name identifies the recognizer (diagnostics).
	Name() string
	// Match reports whether this recognizer handles the command, keyed on the
	// DRIVER tool + argv shape (not the input file extension).
	Match(cmd CodegenCommand) bool
	// Lower emits the native rule(s). A non-nil error means "recognized the tool
	// but this invocation is non-standard" (e.g. the derived outputs don't match
	// what cmake recorded) — the dispatch then REFUSES under --fidelity=strict or
	// FALLS BACK to the generic genrule under best-effort (additive-only).
	Lower(cmd CodegenCommand) (CodegenResult, error)
}

// codegenRegistry returns the ordered recognizer list for a dispatch: the
// embedded built-in .star recognizers (protoc / grpc), with operator-supplied
// `extra` recognizers (loaded from --recognizers) applied as OVERRIDES — an
// operator recognizer with the same Name() REPLACES the built-in in place,
// otherwise it appends after. First Match wins; no match → the generic
// (hermetic) genrule path. (The built-in matches are mutually exclusive, so
// their relative order is immaterial.)
func codegenRegistry(extra []CodegenRecognizer) []CodegenRecognizer {
	out := append([]CodegenRecognizer(nil), builtinRecognizers()...)
	byName := map[string]int{}
	for i, r := range out {
		byName[r.Name()] = i
	}
	for _, e := range extra {
		if i, ok := byName[e.Name()]; ok {
			out[i] = e // operator shadows the built-in of the same name
		} else {
			byName[e.Name()] = len(out)
			out = append(out, e)
		}
	}
	return out
}

// recognizeCodegen tries each registered recognizer in order. Returns
// (result, matched, err): matched=false → no recognizer claimed it (genrule
// fallback); matched=true with err!=nil → claimed but non-standard (the caller
// applies the fidelity policy).
func recognizeCodegen(cmd CodegenCommand) (CodegenResult, bool, error) {
	return recognizeCodegenWith(nil, cmd)
}

// recognizeCodegenWith is recognizeCodegen over the built-ins plus operator
// overrides (see codegenRegistry). Same (result, matched, err) contract.
func recognizeCodegenWith(extra []CodegenRecognizer, cmd CodegenCommand) (CodegenResult, bool, error) {
	for _, r := range codegenRegistry(extra) {
		if r.Match(cmd) {
			res, err := r.Lower(cmd)
			return res, true, err
		}
	}
	return CodegenResult{}, false, nil
}

// codegenRecognizerMatches reports whether any registered (or operator-extra)
// recognizer CLAIMS the command's tool — the location-independent "is this a
// known codegen tool?" signal, without running Lower. Used by the out-of-tree
// execute_process partition to route a recognized codegen call to the lift
// (native-rule) path instead of a vague note.
func codegenRecognizerMatches(extra []CodegenRecognizer, cmd CodegenCommand) bool {
	for _, r := range codegenRegistry(extra) {
		if r.Match(cmd) {
			return true
		}
	}
	return false
}

// recognizeOrGenrule is the single emit chokepoint every codegen recovery
// front-end routes its final emit through: given the recovered command and the
// genrule the site would otherwise emit, it returns either the recognizer's
// native rule(s) or the genrule fallback. The bool reports whether a recognizer
// claimed the command (true → native targets; false → []fallback), so a caller
// can do its genrule-only bookkeeping (e.g. registering OutToGenrule) only in
// the fallback case.
//
// Off by default (cc.RecognizeCodegen) and on any no-match / non-standard claim
// (output cross-check mismatch → err) it returns the fallback unchanged, so
// flag-off is byte-identical at every call site. On a match it registers the
// native rule's CONSUMER LABEL against each output in cc.OutToNativeConsumerDep,
// so a #include of a generated header wires a direct deps edge to the rule
// (resolveCodegenHeaderConsumers + split) rather than the genrule's file
// wrapper — OutToGenrule is deliberately NOT set for the native case.
func recognizeOrGenrule(cc *codegenContext, cmd CodegenCommand, fallback ir.Target) ([]ir.Target, bool) {
	if cc == nil || !cc.RecognizeCodegen {
		noteHostCodegenTool(cc, fallback)
		return []ir.Target{fallback}, false
	}
	// Feed the recognizer the output set the genrule would otherwise declare —
	// the discovery a content-derived-output tool needs (it can't predict outputs
	// from a naming convention). A recognizer that doesn't care ignores it.
	if cmd.DiscoveredOutputs == nil {
		cmd.DiscoveredOutputs = fallback.GenruleOuts
	}
	res, matched, err := recognizeCodegenWith(cc.ExtraRecognizers, cmd)
	if matched && err != nil {
		// A recognizer claimed the tool but the invocation is non-standard (its
		// derived outputs disagree with cmake's recorded ones). The --fidelity
		// dial decides: strict REFUSES — emit a loud build-time stub rather than
		// a generic genrule whose output set we couldn't validate; best-effort
		// FALLS BACK to the genrule. The CLI's product default is strict (it
		// canonicalizes "" → strict before threading); the lower-package zero
		// value "" here is the best-effort fall-back for a direct caller.
		if cc.Fidelity == convmode.FidelityStrict {
			return []ir.Target{recognizerRefusalStub(fallback, cmd, err)}, false
		}
		noteHostCodegenTool(cc, fallback)
		return []ir.Target{fallback}, false
	}
	if !matched {
		noteHostCodegenTool(cc, fallback)
		return []ir.Target{fallback}, false
	}
	emit, consumer, ok := cc.dedupRecognizedRule(cmd, res, nativeRuleSubPkg(res, cmd))
	if !ok {
		// A DIFFERENT input already owns one of these target names in this package
		// — emitting would be a duplicate. Fall back to the generic genrule.
		noteHostCodegenTool(cc, fallback)
		return []ir.Target{fallback}, false
	}
	if cc.OutToNativeConsumerDep != nil && consumer != "" {
		for _, o := range cmd.Outs {
			cc.OutToNativeConsumerDep[o] = consumer
		}
	}
	if len(emit) == 0 {
		// Deduped against the same input's earlier invocation (a different output
		// dir): outputs wired above; the rule's already emitted + placed.
		return nil, true
	}
	recordNativeRulePlacement(cc, res, cmd)
	return emit, true
}

// recognizerRefusalStub turns the genrule fallback into a loud build-time
// refusal: same name + declared outs (so consumers still resolve the label),
// but a cmd that prints the reason and exits non-zero. Used under
// --fidelity=strict when a recognizer matched the tool but the invocation is
// non-standard — "faithful native rule, or a loud failure; never a genrule
// whose outputs we couldn't validate."
func recognizerRefusalStub(fallback ir.Target, cmd CodegenCommand, err error) ir.Target {
	stub := fallback
	stub.Kind = ir.KindGenrule
	stub.GenruleTools = nil
	msg := fmt.Sprintf("convert-element-cmake: --fidelity=strict refused a non-standard %q codegen invocation: %v; re-run with --fidelity=best-effort to fall back to a generic genrule.", cmd.Driver, err)
	stub.GenruleCmd = "echo " + shellQuoteArg(msg) + " >&2; exit 1"
	stub.Tags = append(append([]string(nil), fallback.Tags...), "cmake-codegen-recognizer-strict-refusal")
	sort.Strings(stub.Tags)
	return stub
}

// codegenInputKey identifies a recognizer invocation by its INPUTS — the driver
// plus the sorted source set. It's the dedup unit: the SAME input run into
// different output dirs is one canonical native rule (Bazel produces the output
// at a single location, so the cmake out-dir duplication collapses), while
// DIFFERENT inputs get distinct keys and stay separate rules.
func codegenInputKey(cmd CodegenCommand) string {
	ins := cmd.InputFiles
	if len(ins) == 0 {
		ins = cmd.Srcs
	}
	keyed := append([]string(nil), ins...)
	sort.Strings(keyed)
	return cmd.Driver + "\x00" + strings.Join(keyed, "\x00")
}

// dedupRecognizedRule decides whether a matched recognizer's targets should be
// EMITTED or DEDUPED against an identical earlier invocation. subPkg is the
// package the targets will be placed in (so the name-collision guard keys on the
// final identity). Returns:
//
//	emit     — the targets to append (nil when deduped: a repeat of the same input)
//	consumer — the consumer-dep label to wire the call's outputs to (the
//	           already-emitted rule's on a dedup, this result's otherwise)
//	ok       — false ONLY when a DIFFERENT input already owns one of these target
//	           names in subPkg (a recognizer naming bug); the caller falls back to
//	           the generic genrule rather than emit a load-breaking duplicate.
//
// Pure bookkeeping on the first call for a given input; existing
// single-invocation fixtures never hit the dedup or the guard, so behavior is
// unchanged there.
func (cc *codegenContext) dedupRecognizedRule(cmd CodegenCommand, res CodegenResult, subPkg string) (emit []ir.Target, consumer string, ok bool) {
	if len(res.ConsumerDeps) > 0 {
		consumer = strings.TrimPrefix(res.ConsumerDeps[0], ":")
	}
	key := codegenInputKey(cmd)
	if prevConsumer, dup := cc.recognizedConsumerByInput[key]; dup {
		// Same input as an earlier invocation — reuse that rule; wire this call's
		// outputs to it (consumer) instead of re-emitting a duplicate target.
		return nil, prevConsumer, true
	}
	// New input: a different input must not already own one of our names here.
	for _, t := range res.Targets {
		if owner, taken := cc.recognizedNameOwner[subPkg+"\x00"+t.Name]; taken && owner != key {
			return nil, "", false
		}
	}
	for _, t := range res.Targets {
		cc.recognizedNameOwner[subPkg+"\x00"+t.Name] = key
	}
	cc.recognizedConsumerByInput[key] = consumer
	return res.Targets, consumer, true
}

// nativeRuleSubPkg is the element-relative package a recognizer's targets land
// in: the recognizer's SubPackage (the .proto's own dir — correct even under a
// rebased --proto_path), else the output's dir. "" (the element root, where a
// bare name needs no placement) for an empty / "." result.
func nativeRuleSubPkg(res CodegenResult, cmd CodegenCommand) string {
	dir := res.SubPackage
	if dir == "" && len(cmd.Outs) > 0 {
		dir = path.Dir(cmd.Outs[0])
	}
	if dir == "." {
		return ""
	}
	return dir
}

// recordNativeRulePlacement notes the package each native target should land in
// (merged into Package.SubPackages later). The recognizer names the rule + sets
// srcs by basename, so the rule must land in the package owning the .proto for
// the basename to resolve and cross-package imports to line up. Prefers the
// recognizer's SubPackage (the .proto's own dir — correct even under a rebased
// --proto_path); falls back to the output's dir for recognizers that don't set
// it.
func recordNativeRulePlacement(cc *codegenContext, res CodegenResult, cmd CodegenCommand) {
	if cc.NativeRuleSubPackage == nil {
		return
	}
	dir := nativeRuleSubPkg(res, cmd)
	if dir == "" {
		return
	}
	for _, t := range res.Targets {
		cc.NativeRuleSubPackage[t.Name] = dir
	}
}

// canonicalProtoPath returns the proto-path-relative name of the proto a protoc
// command compiled, recovered from its output: foo.pb.cc -> foo.proto. (protoc
// names outputs by the input's path RELATIVE to --proto_path, so the output
// basename path IS the canonical import name.)
func canonicalProtoPath(outs []string) string {
	for _, suffix := range []string{".pb.cc", ".pb.h"} {
		for _, o := range outs {
			if strings.HasSuffix(o, suffix) {
				return strings.TrimSuffix(o, suffix) + ".proto"
			}
		}
	}
	return ""
}

// protoPathRoot recovers the element-relative `--proto_path` root from the
// mismatch between a .proto's source-tree path and its canonical (proto_path-
// relative) name: proto "proto/dep.proto" with canonical "dep.proto" -> root
// "proto". "" when the proto_path IS the source root (the common case:
// canonical == source path), i.e. no strip_import_prefix is needed.
func protoPathRoot(proto string, outs []string) string {
	canon := canonicalProtoPath(outs)
	if canon == "" || !strings.HasSuffix(proto, canon) {
		return ""
	}
	return strings.Trim(strings.TrimSuffix(proto, canon), "/")
}

// protoImportLabels resolves the DIRECT proto imports of the sole .proto in srcs
// to the proto_library labels the recognizer's deps need. Imports are written
// RELATIVE TO the command's --proto_path root (recovered from srcs+outs), so
// they're resolved under <cmakeSrc>/<protoPathRoot>: `import "pkg/a/a.proto"`
// with a source-root proto_path → //pkg/a:a_proto; `import "dep.proto"` with a
// rebased proto_path=proto → //proto:dep_proto (the imported proto lives at
// proto/dep.proto, placed in //proto, matching its own recognizer placement).
// Only in-tree protos are mapped (a well-known type from a -I root isn't an
// in-element dep). Empty when the proto has no in-tree imports.
func protoImportLabels(srcs, outs []string, cmakeSrc, bazelPackagePath string) []string {
	proto := soleProtoInput(srcs)
	if proto == "" || cmakeSrc == "" {
		return nil
	}
	data, err := os.ReadFile(filepath.Join(cmakeSrc, filepath.FromSlash(proto)))
	if err != nil {
		return nil
	}
	root := protoPathRoot(proto, outs) // element-relative --proto_path root ("" = source root)
	base := strings.Trim(bazelPackagePath, "/")
	var labels []string
	seen := map[string]bool{}
	for _, imp := range parseProtoImports(string(data)) {
		// imp is proto_path-relative; the imported file lives at <root>/<imp>.
		rel := path.Join(root, imp)
		if rel == proto {
			continue
		}
		if _, err := os.Stat(filepath.Join(cmakeSrc, filepath.FromSlash(rel))); err != nil {
			continue // not an in-element proto (well-known type from a -I root)
		}
		labelPkg := path.Dir(rel)
		name := strings.TrimSuffix(path.Base(rel), ".proto") + "_proto"
		if base != "" {
			labelPkg = path.Join(base, labelPkg)
		}
		label := "//" + labelPkg + ":" + name
		if !seen[label] {
			seen[label] = true
			labels = append(labels, label)
		}
	}
	sort.Strings(labels)
	return labels
}

// outputClaimed reports whether a package-relative output is already wired to a
// producer — a recovered genrule (OutToGenrule) or a recognized native rule
// (OutToNativeConsumerDep). It's the single "is this file already produced?"
// predicate every producer site consults before emitting, so two recoveries
// never claim the same output (the genrule/bake-vs-native-rule overlap, and
// duplicate trace calls).
func (cc *codegenContext) outputClaimed(rel string) bool {
	if cc == nil {
		return false
	}
	if _, ok := cc.OutToGenrule[rel]; ok {
		return true
	}
	_, ok := cc.OutToNativeConsumerDep[rel]
	return ok
}

// placeNativeRuleSubPackages merges the dispatch-recorded native-rule placements
// (cc.NativeRuleSubPackage) into pkg.SubPackages, so the split transform lands
// each recognized proto_library/cc_proto_library in the package owning its
// codegen output.
func placeNativeRuleSubPackages(pkg *ir.Package, cc *codegenContext) {
	if pkg == nil || cc == nil || len(cc.NativeRuleSubPackage) == 0 {
		return
	}
	if pkg.SubPackages == nil {
		pkg.SubPackages = map[string]string{}
	}
	for name, dir := range cc.NativeRuleSubPackage {
		pkg.SubPackages[name] = dir
	}
}

// rewriteNativeRuleConsumers is the package-wide consumer rewrite for native
// rules: a cc target that lists a recognized codegen output as a SOURCE (or
// header) — e.g. an execute_process project that compiles the generated
// `foo.pb.cc` into a library — has that file STRIPPED from its srcs/hdrs and a
// DIRECT deps edge to the native rule's consumer label added in its place,
// because the native rule (cc_proto_library) compiles the generated source
// itself. Keyed on cc.OutToNativeConsumerDep (the recognizer's output → consumer
// label map), so a listed generated source IS the codemodel-demand signal that
// both confirms the output and attributes the consumer. Complements the
// #include-driven wiring (resolveCodegenHeaderConsumers + split), which handles
// consumers that include a generated header without listing it as a source.
func rewriteNativeRuleConsumers(pkg *ir.Package, cc *codegenContext) {
	if pkg == nil || cc == nil || len(cc.OutToNativeConsumerDep) == 0 {
		return
	}
	for i := range pkg.Targets {
		t := &pkg.Targets[i]
		switch t.Kind {
		case ir.KindCCLibrary, ir.KindCCBinary, ir.KindCCTest:
		default:
			continue
		}
		labels := map[string]bool{}
		strip := func(files []string) []string {
			kept := files[:0:0]
			for _, f := range files {
				if consumer, ok := cc.OutToNativeConsumerDep[f]; ok {
					labels[":"+consumer] = true
					continue
				}
				kept = append(kept, f)
			}
			return kept
		}
		srcs, hdrs := strip(t.Srcs), strip(t.Hdrs)
		if len(labels) == 0 {
			continue
		}
		t.Srcs, t.Hdrs = srcs, hdrs
		for l := range labels {
			if !slices.Contains(t.Deps, l) {
				t.Deps = append(t.Deps, l)
			}
		}
		sort.Strings(t.Deps)
	}
}

// soleProtoInput returns the single `.proto` entry in srcs, or "" when there
// isn't exactly one (the protoc_compile shape is one proto per command).
func soleProtoInput(srcs []string) string {
	var protos []string
	for _, s := range srcs {
		if strings.EqualFold(filepath.Ext(s), ".proto") {
			protos = append(protos, s)
		}
	}
	if len(protos) != 1 {
		return ""
	}
	return protos[0]
}

// derivedOutputsConsistent checks that the recognizer's derived output basenames
// match what cmake recorded (basename set equality). A mismatch — cmake recorded
// an output the tool's convention doesn't predict, or vice versa — signals a
// non-standard invocation the recognizer must decline.
func derivedOutputsConsistent(recorded, derived []string) error {
	rset := map[string]bool{}
	for _, o := range recorded {
		rset[filepath.Base(o)] = true
	}
	dset := map[string]bool{}
	for _, o := range derived {
		dset[filepath.Base(o)] = true
	}
	for d := range dset {
		if !rset[d] {
			return fmt.Errorf("derived output %q not in cmake-recorded outputs %v", d, recorded)
		}
	}
	for r := range rset {
		if !dset[r] {
			return fmt.Errorf("cmake-recorded output %q not predicted by the tool convention %v", r, derived)
		}
	}
	return nil
}
