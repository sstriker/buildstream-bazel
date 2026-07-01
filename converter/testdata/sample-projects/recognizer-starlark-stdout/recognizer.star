# Operator recognizer for `sgen`, a STDOUT generator with no native Bazel rule.
# The execute_process OUTPUT_FILE recognition feeds the captured output's basename
# as cmd.discovered_outputs; lower() returns a genrule (the Starlark genrule(...)
# builtin) that re-runs the tool at Bazel time, redirecting its stdout to $@.
def match(cmd):
    return cmd.driver == "sgen"

def lower(cmd):
    out = cmd.discovered_outputs[0]
    name = "sgen_" + out.replace(".", "_")
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
        consumer_deps = [":" + name],
        derived_outputs = [out],
    )
