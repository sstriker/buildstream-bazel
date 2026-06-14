# Operator recognizer for `gen_pb`, a project-specific protobuf compiler
# wrapper the converter has no built-in for. Loaded via --recognizers; teaches
# the converter to lower `gen_pb --cpp_out` to proto_library + cc_proto_library
# with no recompile. (A near-copy of recognizers/protoc.star, matching gen_pb
# instead of protoc — the gate uses a non-built-in tool so the operator script
# actually fires rather than being shadowed by the built-in protoc recognizer.)

def _basename(p):
    return p.rsplit("/", 1)[-1]

def _sole_proto(srcs):
    protos = [s for s in srcs if s.endswith(".proto")]
    if len(protos) != 1:
        return None
    return protos[0]

def match(cmd):
    if cmd.driver != "gen_pb":
        return False
    return any([a == "--cpp_out" or a.startswith("--cpp_out=") for a in cmd.args])

def lower(cmd):
    proto = _sole_proto(cmd.srcs)
    if proto == None:
        fail("gen_pb: expected exactly one .proto in srcs, got %s" % cmd.srcs)
    base = _basename(proto)[:-len(".proto")]
    proto_name = base + "_proto"
    cc_name = base + "_cc_proto"
    return result(
        targets = [
            native_rule(
                "proto_library",
                proto_name,
                load_from = "@protobuf//bazel:proto_library.bzl",
                attrs = {"srcs": [_basename(proto)], "visibility": ["//visibility:public"]},
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
