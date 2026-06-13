# protoc.star — an operator codegen recognizer, loaded via
#   convert-element-cmake --recognize-codegen --recognizers 'recognizers/*.star'
# without recompiling the converter. This mirrors the built-in protoc-cpp
# recognizer as a copy-me template; the built-in already claims `protoc
# --cpp_out`, so this file's value is as the canonical example. To support a
# NEW generator, copy this and change match() + the rule shape in lower().
#
# A recognizer defines two top-level functions. `cmd` is a struct with:
#   cmd.driver      generator basename, e.g. "protoc"
#   cmd.args        argv after the driver (flags + inputs)
#   cmd.srcs        recovered input sources (package-relative)
#   cmd.outs        outputs cmake recorded (the output-authority cross-check)
#   cmd.pkg         the Bazel package path
#   cmd.proto_deps  pre-resolved proto_library labels for the input's imports

def _basename(p):
    return p.rsplit("/", 1)[-1]

def _sole_proto(srcs):
    protos = [s for s in srcs if s.endswith(".proto")]
    if len(protos) != 1:
        return None
    return protos[0]

# match keys on the TOOL + a flag — the decisive signal a source-file-dispatched
# tool lacks. Return True to claim the command.
def match(cmd):
    if not cmd.driver.startswith("protoc"):
        return False
    return any([a == "--cpp_out" or a.startswith("--cpp_out=") for a in cmd.args])

# lower returns the native rule(s) + the consumer dep label. It is the OUTPUT
# AUTHORITY: declare derived_outputs from the input convention; the converter
# cross-checks them against cmake's recorded outputs and falls back to the
# genrule on a mismatch (so a non-standard invocation never regresses).
def lower(cmd):
    proto = _sole_proto(cmd.srcs)
    if proto == None:
        fail("protoc-cpp: expected exactly one .proto in srcs, got %s" % cmd.srcs)
    base = _basename(proto)[:-len(".proto")]
    proto_name = base + "_proto"
    cc_name = base + "_cc_proto"

    proto_attrs = {"srcs": [_basename(proto)]}
    if cmd.proto_deps:
        proto_attrs["deps"] = sorted(cmd.proto_deps)
    proto_attrs["visibility"] = ["//visibility:public"]

    return result(
        targets = [
            native_rule(
                "proto_library",
                proto_name,
                load_from = "@protobuf//bazel:proto_library.bzl",
                attrs = proto_attrs,
            ),
            native_rule(
                "cc_proto_library",
                cc_name,
                load_from = "@protobuf//bazel:cc_proto_library.bzl",
                attrs = {"deps": [":" + proto_name], "visibility": ["//visibility:public"]},
            ),
        ],
        consumer_deps = [":" + cc_name],
        derived_outputs = [base + ".pb.cc", base + ".pb.h"],
    )
