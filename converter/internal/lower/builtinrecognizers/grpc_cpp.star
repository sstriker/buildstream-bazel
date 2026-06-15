# Built-in recognizer: combined protoc --cpp_out --grpc_out → proto_library +
# cc_proto_library + cc_grpc_library(grpc_only=True). The common
# protobuf_generate(... PLUGIN grpc) shape: one invocation produces both the
# message classes and the C++ service stubs. Requires BOTH flags so it owns the
# whole self-contained set (a grpc-only call is grpc_only.star's).

def _base(proto):
    return proto.rsplit("/", 1)[-1][:-len(".proto")]

def _dir(p):
    return p.rsplit("/", 1)[0] if "/" in p else ""

def _sole_proto(srcs):
    protos = [s for s in srcs if s.lower().endswith(".proto")]
    return protos[0] if len(protos) == 1 else ""

def _has_flag(args, flag):
    return any([a == flag or a.startswith(flag + "=") for a in args])

def _canonical_proto_path(outs):
    for suffix in [".pb.cc", ".pb.h"]:
        for o in outs:
            if o.endswith(suffix):
                return o[:-len(suffix)] + ".proto"
    return ""

def _proto_path_root(proto, outs):
    canon = _canonical_proto_path(outs)
    if canon == "" or not proto.endswith(canon):
        return ""
    return proto[:-len(canon)].strip("/")

def _proto_library_attrs(proto, cmd):
    attrs = {"srcs": [proto.rsplit("/", 1)[-1]]}
    root = _proto_path_root(proto, cmd.outs)
    if root != "":
        prefix = (cmd.pkg + "/" + root) if cmd.pkg else root
        attrs["strip_import_prefix"] = "/" + prefix
    if cmd.proto_deps:
        attrs["deps"] = sorted(cmd.proto_deps)
    attrs["visibility"] = ["//visibility:public"]
    return attrs

def match(cmd):
    return cmd.driver.rsplit("/", 1)[-1].startswith("protoc") and \
           _has_flag(cmd.args, "--grpc_out") and _has_flag(cmd.args, "--cpp_out")

def lower(cmd):
    proto = _sole_proto(cmd.srcs)
    if proto == "":
        fail("protoc-grpc-cpp: no single .proto input in srcs %s" % cmd.srcs)
    base = _base(proto)
    proto_name = base + "_proto"
    cc_name = base + "_cc_proto"
    grpc_name = base + "_cc_grpc"
    return result(
        targets = [
            native_rule("proto_library", proto_name,
                        load_from = "@protobuf//bazel:proto_library.bzl",
                        attrs = _proto_library_attrs(proto, cmd)),
            native_rule("cc_proto_library", cc_name,
                        load_from = "@protobuf//bazel:cc_proto_library.bzl",
                        attrs = {"deps": [":" + proto_name], "visibility": ["//visibility:public"]}),
            native_rule("cc_grpc_library", grpc_name,
                        load_from = "@grpc//bazel:cc_grpc_library.bzl",
                        attrs = {"srcs": [":" + proto_name], "deps": [":" + cc_name],
                                 "grpc_only": True, "visibility": ["//visibility:public"]}),
        ],
        consumer_deps = [":" + grpc_name],
        derived_outputs = [base + ".pb.cc", base + ".pb.h", base + ".grpc.pb.cc", base + ".grpc.pb.h"],
        sub_package = _dir(proto),
    )
