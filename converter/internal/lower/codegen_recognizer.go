package lower

import (
	"fmt"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/sstriker/buildstream-bazel/converter/ir"
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
	// Outs are cmake's RECORDED outputs (package-relative) — the cross-check
	// the recognizer validates its derived output set against.
	Outs []string
	// Pkg is the Bazel package path the codegen lives in.
	Pkg string
	// ProtoDeps are the already-resolved proto_library labels for this input's
	// transitive `import`s (computed by the dispatch via protoImportClosure +
	// the package layout). The proto recognizer threads them onto proto_library
	// deps; empty for an import-free proto.
	ProtoDeps []string
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

// codegenRegistry is the ordered recognizer list; the first Match wins, and no
// match means the command stays on the generic (hermetic) genrule path.
var codegenRegistry = []CodegenRecognizer{
	protocCppRecognizer{},
}

// recognizeCodegen tries each registered recognizer in order. Returns
// (result, matched, err): matched=false → no recognizer claimed it (genrule
// fallback); matched=true with err!=nil → claimed but non-standard (the caller
// applies the fidelity policy).
func recognizeCodegen(cmd CodegenCommand) (CodegenResult, bool, error) {
	return recognizeCodegenWith(nil, cmd)
}

// recognizeCodegenWith is recognizeCodegen with operator-supplied recognizers
// (loaded from --recognizers Starlark files) appended after the built-ins:
// first-party recognizers win, operator ones extend. Same (result, matched,
// err) contract.
func recognizeCodegenWith(extra []CodegenRecognizer, cmd CodegenCommand) (CodegenResult, bool, error) {
	for _, r := range codegenRegistry {
		if r.Match(cmd) {
			res, err := r.Lower(cmd)
			return res, true, err
		}
	}
	for _, r := range extra {
		if r.Match(cmd) {
			res, err := r.Lower(cmd)
			return res, true, err
		}
	}
	return CodegenResult{}, false, nil
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
		return []ir.Target{fallback}, false
	}
	res, matched, err := recognizeCodegenWith(cc.ExtraRecognizers, cmd)
	if !matched || err != nil {
		return []ir.Target{fallback}, false
	}
	if cc.OutToNativeConsumerDep != nil && len(res.ConsumerDeps) > 0 {
		consumer := strings.TrimPrefix(res.ConsumerDeps[0], ":")
		for _, o := range cmd.Outs {
			cc.OutToNativeConsumerDep[o] = consumer
		}
	}
	return res.Targets, true
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

// protocCppRecognizer maps a `protoc … --cpp_out …` custom-command to a
// proto_library + cc_proto_library pair. (The gRPC service variant — --grpc_out
// → cc_grpc_library — is a sibling recognizer, added next.)
type protocCppRecognizer struct{}

func (protocCppRecognizer) Name() string { return "protoc-cpp" }

func (protocCppRecognizer) Match(cmd CodegenCommand) bool {
	if !strings.HasPrefix(filepath.Base(cmd.Driver), "protoc") {
		return false
	}
	return hasFlagPrefix(cmd.Args, "--cpp_out")
}

func (r protocCppRecognizer) Lower(cmd CodegenCommand) (CodegenResult, error) {
	proto := soleProtoInput(cmd.Srcs)
	if proto == "" {
		return CodegenResult{}, fmt.Errorf("protoc-cpp: no single .proto input in srcs %v", cmd.Srcs)
	}
	base := strings.TrimSuffix(filepath.Base(proto), ".proto")
	// Output AUTHORITY: protoc --cpp_out's convention is foo.proto -> foo.pb.{h,cc}.
	// When cmake RECORDED the outputs (the custom-command paths), cross-check the
	// derivation against them — a mismatch is a non-standard invocation this
	// recognizer must not claim. When the outputs WEREN'T recorded (the
	// execute_process `--cpp_out=DIR` shape passes empty Outs), the recognizer
	// SUPPLIES the derived set and the caller corroborates it against on-disk +
	// codemodel evidence (it can't be validated here).
	derived := []string{base + ".pb.cc", base + ".pb.h"}
	if len(cmd.Outs) > 0 {
		if err := derivedOutputsConsistent(cmd.Outs, derived); err != nil {
			return CodegenResult{}, fmt.Errorf("protoc-cpp: %w", err)
		}
	}
	protoName := base + "_proto"
	ccName := base + "_cc_proto"
	protoAttrs := []ir.NativeAttr{{Name: "srcs", List: []string{filepath.Base(proto)}}}
	if len(cmd.ProtoDeps) > 0 {
		deps := append([]string(nil), cmd.ProtoDeps...)
		sort.Strings(deps)
		protoAttrs = append(protoAttrs, ir.NativeAttr{Name: "deps", List: deps})
	}
	protoAttrs = append(protoAttrs, ir.NativeAttr{Name: "visibility", List: []string{"//visibility:public"}})
	protoLib := ir.Target{
		Name: protoName, Kind: ir.KindNativeRule,
		NativeRule: &ir.NativeRuleSpec{Kind: "proto_library", LoadFrom: "@protobuf//bazel:proto_library.bzl", Attrs: protoAttrs},
	}
	ccLib := ir.Target{
		Name: ccName, Kind: ir.KindNativeRule,
		NativeRule: &ir.NativeRuleSpec{Kind: "cc_proto_library", LoadFrom: "@protobuf//bazel:cc_proto_library.bzl", Attrs: []ir.NativeAttr{
			{Name: "deps", List: []string{":" + protoName}},
			{Name: "visibility", List: []string{"//visibility:public"}},
		}},
	}
	return CodegenResult{
		Targets:        []ir.Target{protoLib, ccLib},
		ConsumerDeps:   []string{":" + ccName},
		DerivedOutputs: derived,
	}, nil
}

// hasFlagPrefix reports whether any arg begins with the given flag (covering
// both `--cpp_out=dir` and a bare `--cpp_out` followed by its value).
func hasFlagPrefix(args []string, flag string) bool {
	for _, a := range args {
		if a == flag || strings.HasPrefix(a, flag+"=") {
			return true
		}
	}
	return false
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
