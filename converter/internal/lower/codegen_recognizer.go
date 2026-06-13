package lower

import (
	"fmt"
	"path/filepath"
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
	for _, r := range codegenRegistry {
		if r.Match(cmd) {
			res, err := r.Lower(cmd)
			return res, true, err
		}
	}
	return CodegenResult{}, false, nil
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
	// Cross-check against what cmake recorded; a mismatch is a non-standard
	// invocation this recognizer must not claim.
	derived := []string{base + ".pb.cc", base + ".pb.h"}
	if err := derivedOutputsConsistent(cmd.Outs, derived); err != nil {
		return CodegenResult{}, fmt.Errorf("protoc-cpp: %w", err)
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
	return CodegenResult{Targets: []ir.Target{protoLib, ccLib}, ConsumerDeps: []string{":" + ccName}}, nil
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
