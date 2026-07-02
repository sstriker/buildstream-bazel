# Operator recognizer for `sgen`, a STDOUT generator with no native Bazel rule.
# The execute_process OUTPUT_FILE recognition feeds the captured output's basename
# as cmd.discovered_outputs; lower() returns a genrule (the Starlark genrule(...)
# builtin) that re-runs the tool at Bazel time, redirecting its stdout to $@.
def match(cmd):
    return cmd.driver == "sgen"

def lower(cmd):
    out = cmd.discovered_outputs[0]
    name = "sgen_" + out.replace(".", "_")
    # A genrule's outputs are consumed BY FILENAME — the consumer keeps the
    # generated file in its srcs and Bazel resolves it to this genrule. So a
    # genrule-emitting recognizer leaves consumer_deps EMPTY (unlike a native_rule
    # recognizer, whose CcInfo target the consumer depends on via a deps edge).
    return result(
        targets = [
            genrule(
                name = name,
                cmd = "$(location //tools:sgen) $(SRCS) > $@",
                outs = [out],
                srcs = cmd.srcs,
                tools = ["//tools:sgen"],
            ),
        ],
        derived_outputs = [out],
    )
