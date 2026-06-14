package lower

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"go.starlark.net/starlark"
	"go.starlark.net/starlarkstruct"
	"go.starlark.net/syntax"

	"github.com/sstriker/buildstream-bazel/converter/ir"
)

// This file lets operators add codegen recognizers WITHOUT recompiling the
// converter: a recognizer is a Starlark file (`*.star`) defining two
// functions, loaded at startup via --recognizers and appended to the registry.
//
//	def match(cmd):  # cmd.driver, cmd.args, cmd.srcs, cmd.outs, cmd.pkg, cmd.proto_deps
//	    return cmd.driver.startswith("protoc") and \
//	           any([a.startswith("--cpp_out") for a in cmd.args])
//
//	def lower(cmd):
//	    base = cmd.srcs[0].rsplit("/", 1)[-1][:-len(".proto")]
//	    return result(
//	        targets = [
//	            native_rule("proto_library", base + "_proto",
//	                        load_from = "@protobuf//bazel:proto_library.bzl",
//	                        attrs = {"srcs": [base + ".proto"], "visibility": ["//visibility:public"]}),
//	            native_rule("cc_proto_library", base + "_cc_proto",
//	                        load_from = "@protobuf//bazel:cc_proto_library.bzl",
//	                        attrs = {"deps": [":" + base + "_proto"], "visibility": ["//visibility:public"]}),
//	        ],
//	        consumer_deps = [":" + base + "_cc_proto"],
//	        derived_outputs = [base + ".pb.cc", base + ".pb.h"],
//	    )
//
// Starlark is deterministic and I/O-free, so a recognizer can't read the
// filesystem or otherwise break hermeticity — it can only inspect the command
// it's handed and return rule data. The output-authority cross-check stays
// FIRST-PARTY: the script declares derived_outputs and the Go host validates
// them against cmake's recorded outputs (derivedOutputsConsistent), so a buggy
// or non-standard script declines to the genrule fallback, never corrupts the
// build. The native_rule(...) and result(...) builtins are the entire API.

// LoadStarlarkRecognizers compiles each path into a CodegenRecognizer. Paths
// are .star files (resolve a glob/dir to file paths before calling). A compile
// error (syntax, or a missing match/lower function) is returned, not swallowed
// — an operator's broken recognizer should fail loudly at startup.
func LoadStarlarkRecognizers(paths []string) ([]CodegenRecognizer, error) {
	var out []CodegenRecognizer
	for _, p := range paths {
		src, err := os.ReadFile(p)
		if err != nil {
			return nil, fmt.Errorf("recognizer %q: %w", p, err)
		}
		r, err := compileStarlarkRecognizer(p, src)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
}

func compileStarlarkRecognizer(path string, src []byte) (CodegenRecognizer, error) {
	predeclared := starlark.StringDict{
		"native_rule": starlark.NewBuiltin("native_rule", starlarkNativeRule),
		"result":      starlark.NewBuiltin("result", starlarkResult),
	}
	thread := &starlark.Thread{Name: "recognizer-load:" + path}
	globals, err := starlark.ExecFileOptions(&syntax.FileOptions{}, thread, path, src, predeclared)
	if err != nil {
		return nil, fmt.Errorf("recognizer %q: %w", path, err)
	}
	matchFn, ok := globals["match"].(starlark.Callable)
	if !ok {
		return nil, fmt.Errorf("recognizer %q: missing a top-level match(cmd) function", path)
	}
	lowerFn, ok := globals["lower"].(starlark.Callable)
	if !ok {
		return nil, fmt.Errorf("recognizer %q: missing a top-level lower(cmd) function", path)
	}
	return &starlarkRecognizer{
		name:  strings.TrimSuffix(filepath.Base(path), ".star"),
		match: matchFn,
		lower: lowerFn,
	}, nil
}

// starlarkRecognizer adapts a compiled .star (its match/lower functions) to the
// Go CodegenRecognizer interface — the ONE internal contract both built-in and
// operator recognizers share.
type starlarkRecognizer struct {
	name  string
	match starlark.Callable
	lower starlark.Callable
}

func (r *starlarkRecognizer) Name() string { return "starlark:" + r.name }

func (r *starlarkRecognizer) Match(cmd CodegenCommand) bool {
	thread := &starlark.Thread{Name: "recognizer-match:" + r.name}
	v, err := starlark.Call(thread, r.match, starlark.Tuple{commandToStarlark(cmd)}, nil)
	if err != nil {
		// A script error in match declines (the safe, non-regressing choice):
		// the command falls through to the next recognizer / the genrule.
		return false
	}
	return bool(v.Truth())
}

func (r *starlarkRecognizer) Lower(cmd CodegenCommand) (CodegenResult, error) {
	thread := &starlark.Thread{Name: "recognizer-lower:" + r.name}
	v, err := starlark.Call(thread, r.lower, starlark.Tuple{commandToStarlark(cmd)}, nil)
	if err != nil {
		return CodegenResult{}, fmt.Errorf("starlark recognizer %q: lower: %w", r.name, err)
	}
	return resultFromStarlark(cmd, v)
}

// commandToStarlark exposes a CodegenCommand to the script as a struct.
func commandToStarlark(cmd CodegenCommand) starlark.Value {
	return starlarkstruct.FromStringDict(starlarkstruct.Default, starlark.StringDict{
		"driver":     starlark.String(cmd.Driver),
		"args":       stringsToStarlark(cmd.Args),
		"srcs":       stringsToStarlark(cmd.Srcs),
		"outs":       stringsToStarlark(cmd.Outs),
		"pkg":        starlark.String(cmd.Pkg),
		"proto_deps": stringsToStarlark(cmd.ProtoDeps),
	})
}

func stringsToStarlark(xs []string) *starlark.List {
	elems := make([]starlark.Value, len(xs))
	for i, x := range xs {
		elems[i] = starlark.String(x)
	}
	return starlark.NewList(elems)
}

// starlarkNativeRule is the `native_rule(kind, name, load_from=, load_symbol=,
// attrs={})` builtin — a typed constructor returning a struct the host converts
// to an ir.Target{Kind: KindNativeRule}.
func starlarkNativeRule(_ *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var kind, name, loadFrom, loadSymbol string
	var attrs starlark.Value
	if err := starlark.UnpackArgs(b.Name(), args, kwargs,
		"kind", &kind, "name", &name,
		"load_from?", &loadFrom, "load_symbol?", &loadSymbol, "attrs?", &attrs); err != nil {
		return nil, err
	}
	if attrs == nil {
		attrs = starlark.NewDict(0)
	}
	return starlarkstruct.FromStringDict(starlarkstruct.Default, starlark.StringDict{
		"kind":        starlark.String(kind),
		"name":        starlark.String(name),
		"load_from":   starlark.String(loadFrom),
		"load_symbol": starlark.String(loadSymbol),
		"attrs":       attrs,
	}), nil
}

// starlarkResult is the `result(targets, consumer_deps=, derived_outputs=)`
// builtin returning the struct a recognizer's lower() must return.
func starlarkResult(_ *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var targets, consumerDeps, derivedOutputs starlark.Value
	if err := starlark.UnpackArgs(b.Name(), args, kwargs,
		"targets", &targets, "consumer_deps?", &consumerDeps, "derived_outputs?", &derivedOutputs); err != nil {
		return nil, err
	}
	norm := func(v starlark.Value) starlark.Value {
		if v == nil {
			return starlark.NewList(nil)
		}
		return v
	}
	return starlarkstruct.FromStringDict(starlarkstruct.Default, starlark.StringDict{
		"targets":         norm(targets),
		"consumer_deps":   norm(consumerDeps),
		"derived_outputs": norm(derivedOutputs),
	}), nil
}

// resultFromStarlark converts a lower() return value into a CodegenResult and
// runs the FIRST-PARTY output-authority cross-check: the script's
// derived_outputs must agree with cmake's recorded outputs, else this is a
// non-standard invocation and we return an error (→ genrule fallback / strict
// refusal), exactly as a built-in recognizer does.
func resultFromStarlark(cmd CodegenCommand, v starlark.Value) (CodegenResult, error) {
	st, ok := v.(*starlarkstruct.Struct)
	if !ok {
		return CodegenResult{}, fmt.Errorf("lower() must return result(...), got %s", v.Type())
	}
	targetsV, err := st.Attr("targets")
	if err != nil {
		return CodegenResult{}, fmt.Errorf("result missing targets: %w", err)
	}
	targets, err := starlarkTargets(targetsV)
	if err != nil {
		return CodegenResult{}, err
	}
	if len(targets) == 0 {
		return CodegenResult{}, fmt.Errorf("result declared no targets")
	}
	consumer, err := goStringList(attrOrNone(st, "consumer_deps"))
	if err != nil {
		return CodegenResult{}, fmt.Errorf("consumer_deps: %w", err)
	}
	derived, err := goStringList(attrOrNone(st, "derived_outputs"))
	if err != nil {
		return CodegenResult{}, fmt.Errorf("derived_outputs: %w", err)
	}
	if len(derived) == 0 {
		return CodegenResult{}, fmt.Errorf("result must declare derived_outputs for the output-authority cross-check")
	}
	// Same output-authority policy as the built-in recognizers: when cmake
	// RECORDED the outputs (the custom-command paths), validate the script's
	// derived set against them; when it didn't (the execute_process
	// `--cpp_out=DIR` shape passes empty Outs), the script SUPPLIES the set and
	// the caller corroborates it against on-disk + codemodel evidence.
	if len(cmd.Outs) > 0 {
		if err := derivedOutputsConsistent(cmd.Outs, derived); err != nil {
			return CodegenResult{}, err
		}
	}
	return CodegenResult{Targets: targets, ConsumerDeps: consumer, DerivedOutputs: derived}, nil
}

func starlarkTargets(v starlark.Value) ([]ir.Target, error) {
	items, err := goValueList(v)
	if err != nil {
		return nil, fmt.Errorf("targets: %w", err)
	}
	out := make([]ir.Target, 0, len(items))
	for _, item := range items {
		t, err := starlarkTarget(item)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, nil
}

func starlarkTarget(v starlark.Value) (ir.Target, error) {
	st, ok := v.(*starlarkstruct.Struct)
	if !ok {
		return ir.Target{}, fmt.Errorf("each target must be native_rule(...), got %s", v.Type())
	}
	kind, err := structStr(st, "kind")
	if err != nil {
		return ir.Target{}, err
	}
	name, err := structStr(st, "name")
	if err != nil {
		return ir.Target{}, err
	}
	if kind == "" || name == "" {
		return ir.Target{}, fmt.Errorf("native_rule requires a non-empty kind and name")
	}
	loadFrom, _ := structStr(st, "load_from")
	loadSymbol, _ := structStr(st, "load_symbol")
	attrs, err := attrsFromStarlark(attrOrNone(st, "attrs"))
	if err != nil {
		return ir.Target{}, fmt.Errorf("native_rule %q: %w", name, err)
	}
	return ir.Target{
		Name: name, Kind: ir.KindNativeRule,
		NativeRule: &ir.NativeRuleSpec{Kind: kind, LoadFrom: loadFrom, LoadSymbol: loadSymbol, Attrs: attrs},
	}, nil
}

// attrsFromStarlark converts a Starlark dict (attr name -> string | list of
// strings) to []ir.NativeAttr, preserving insertion order for byte-stability.
func attrsFromStarlark(v starlark.Value) ([]ir.NativeAttr, error) {
	if isNoneOrNil(v) {
		return nil, nil
	}
	d, ok := v.(*starlark.Dict)
	if !ok {
		return nil, fmt.Errorf("attrs must be a dict, got %s", v.Type())
	}
	var out []ir.NativeAttr
	for _, item := range d.Items() {
		key, ok := starlark.AsString(item[0])
		if !ok {
			return nil, fmt.Errorf("attr name must be a string, got %s", item[0].Type())
		}
		if s, ok := stringValue(item[1]); ok {
			out = append(out, ir.NativeAttr{Name: key, Str: s})
			continue
		}
		list, err := goStringList(item[1])
		if err != nil {
			return nil, fmt.Errorf("attr %q must be a string or list of strings: %w", key, err)
		}
		out = append(out, ir.NativeAttr{Name: key, List: list})
	}
	return out, nil
}

// --- small Starlark<->Go helpers ---

func attrOrNone(st *starlarkstruct.Struct, field string) starlark.Value {
	v, err := st.Attr(field)
	if err != nil {
		return starlark.None
	}
	return v
}

func structStr(st *starlarkstruct.Struct, field string) (string, error) {
	v, err := st.Attr(field)
	if err != nil {
		return "", err
	}
	s, ok := starlark.AsString(v)
	if !ok {
		return "", fmt.Errorf("%s must be a string, got %s", field, v.Type())
	}
	return s, nil
}

func isNoneOrNil(v starlark.Value) bool {
	if v == nil {
		return true
	}
	_, none := v.(starlark.NoneType)
	return none
}

// stringValue returns the Go string of a Starlark STRING (not other stringable
// values), so attrsFromStarlark can distinguish a scalar attr from a list.
func stringValue(v starlark.Value) (string, bool) {
	s, ok := v.(starlark.String)
	return string(s), ok
}

func goValueList(v starlark.Value) ([]starlark.Value, error) {
	if isNoneOrNil(v) {
		return nil, nil
	}
	iter, ok := v.(starlark.Iterable)
	if !ok {
		return nil, fmt.Errorf("expected a list, got %s", v.Type())
	}
	it := iter.Iterate()
	defer it.Done()
	var out []starlark.Value
	var x starlark.Value
	for it.Next(&x) {
		out = append(out, x)
	}
	return out, nil
}

func goStringList(v starlark.Value) ([]string, error) {
	items, err := goValueList(v)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(items))
	for _, x := range items {
		s, ok := starlark.AsString(x)
		if !ok {
			return nil, fmt.Errorf("expected string, got %s", x.Type())
		}
		out = append(out, s)
	}
	return out, nil
}
