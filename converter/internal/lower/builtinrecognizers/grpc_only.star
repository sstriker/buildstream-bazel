# Built-in recognizer: grpc-only protoc --grpc_out (no --cpp_out) → a
# cc_grpc_library that REFERENCES the proto_library + cc_proto_library a sibling
# protoc --cpp_out call emits in the same package (the services are compiled in
# a separate invocation from the messages). Fires only when the host confirms
# that sibling (cmd.sibling_cpp_proto); without one the referenced
# :base_proto / :base_cc_proto would dangle, so it declines and the call stays a
# genrule.

def _base(proto):
    return proto.rsplit("/", 1)[-1][:-len(".proto")]

def _dir(p):
    return p.rsplit("/", 1)[0] if "/" in p else ""

def _sole_proto(srcs):
    protos = [s for s in srcs if s.lower().endswith(".proto")]
    return protos[0] if len(protos) == 1 else ""

def _has_flag(args, flag):
    return any([a == flag or a.startswith(flag + "=") for a in args])

def match(cmd):
    return cmd.driver.rsplit("/", 1)[-1].startswith("protoc") and cmd.sibling_cpp_proto and \
           _has_flag(cmd.args, "--grpc_out") and not _has_flag(cmd.args, "--cpp_out")

def lower(cmd):
    proto = _sole_proto(cmd.srcs)
    if proto == "":
        fail("protoc-grpc-only: no single .proto input in srcs %s" % cmd.srcs)
    base = _base(proto)
    proto_name = base + "_proto"
    cc_name = base + "_cc_proto"
    grpc_name = base + "_cc_grpc"
    return result(
        targets = [
            native_rule("cc_grpc_library", grpc_name,
                        load_from = "@grpc//bazel:cc_grpc_library.bzl",
                        attrs = {"srcs": [":" + proto_name], "deps": [":" + cc_name],
                                 "grpc_only": True, "visibility": ["//visibility:public"]}),
        ],
        consumer_deps = [":" + grpc_name],
        derived_outputs = [base + ".grpc.pb.cc", base + ".grpc.pb.h"],
        sub_package = _dir(proto),
    )
