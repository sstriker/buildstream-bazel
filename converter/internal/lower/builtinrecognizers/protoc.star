# Built-in recognizer: protoc --cpp_out → proto_library + cc_proto_library.
#
# Embedded default (go:embed). An operator can shadow it by providing a
# --recognizers file named protoc.star. The output-authority cross-check
# (derived_outputs vs cmake's recorded outputs) is enforced host-side.

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

def proto_library_attrs(proto, cmd):
    attrs = {"srcs": [proto.rsplit("/", 1)[-1]]}
    root = _proto_path_root(proto, cmd.outs)
    if root != "":
        prefix = (cmd.pkg + "/" + root) if cmd.pkg else root
        attrs["strip_import_prefix"] = "/" + prefix
    if cmd.proto_deps:
        attrs["deps"] = sorted(cmd.proto_deps)
    attrs["visibility"] = ["//visibility:public"]
    return attrs

def proto_library_for(proto, name, cmd):
    return native_rule("proto_library", name,
                       load_from = "@protobuf//bazel:proto_library.bzl",
                       attrs = proto_library_attrs(proto, cmd))

def cc_proto_library_for(name, proto_name):
    return native_rule("cc_proto_library", name,
                       load_from = "@protobuf//bazel:cc_proto_library.bzl",
                       attrs = {"deps": [":" + proto_name], "visibility": ["//visibility:public"]})

def match(cmd):
    return cmd.driver.rsplit("/", 1)[-1].startswith("protoc") and \
           _has_flag(cmd.args, "--cpp_out") and not _has_flag(cmd.args, "--grpc_out")

def lower(cmd):
    proto = _sole_proto(cmd.srcs)
    if proto == "":
        fail("protoc-cpp: no single .proto input in srcs %s" % cmd.srcs)
    base = _base(proto)
    proto_name = base + "_proto"
    cc_name = base + "_cc_proto"
    return result(
        targets = [proto_library_for(proto, proto_name, cmd), cc_proto_library_for(cc_name, proto_name)],
        consumer_deps = [":" + cc_name],
        derived_outputs = [base + ".pb.cc", base + ".pb.h"],
        sub_package = _dir(proto),
    )
