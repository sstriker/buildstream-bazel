# rules_buildstream_bazel trace_load rule.
#
# Action-time consumer side of the round-2 AC rendezvous. One
# trace_load target per round-2-using element. The action shells
# to the consuming project's //tools:trace-lookup; outputs are
# trace.log (+ make-db.txt for trace-driven kinds) and a hit/miss
# marker. AC miss produces zero-byte trace files, so converters
# route through their coarse-fallback path the same way the legacy
# load-time _trace_repo empty fileset did.
#
# The lookup needs CAS_GRPC_ADDR via --action_env. The convergence
# driver bumps CONVERGE_GENERATION between rounds (also via
# --action_env) to force action re-runs when the AC view shifts.
#
# Replaces the legacy load-time _trace_repo + traces module
# extension; the AC lookup is now Bazel-action-cached, ending
# the analysis-cache churn between driver passes.

def _trace_load_impl(ctx):
    out_trace = ctx.actions.declare_file(ctx.label.name + "/trace.log")
    out_marker = ctx.actions.declare_file(ctx.label.name + "/marker")
    outputs = [out_trace, out_marker]

    args = ctx.actions.args()
    args.add("--srckey", ctx.attr.srckey)
    args.add("--out-trace", out_trace)
    args.add("--out-empty-marker", out_marker)

    if ctx.attr.expect_make_db:
        out_make_db = ctx.actions.declare_file(ctx.label.name + "/make-db.txt")
        outputs.append(out_make_db)
        args.add("--out-make-db", out_make_db)

    if ctx.attr.expect_config_bundle:
        out_config_bundle = ctx.actions.declare_file(ctx.label.name + "/cmake-config-bundle.tar")
        outputs.append(out_config_bundle)
        args.add("--out-config-bundle", out_config_bundle)

    if ctx.attr.platform:
        args.add("--platform", ctx.attr.platform)

    # trace-lookup reads CAS_GRPC_ADDR from the action env when
    # --cas is unset; the operator passes it via
    # --action_env=CAS_GRPC_ADDR. CONVERGE_GENERATION is the lever
    # the convergence driver pulls between rounds — Bazel's
    # ActionCache tracks --action_env values, so a bump forces a
    # re-run. use_default_shell_env opts the action into seeing the
    # build's --action_env values without us having to enumerate
    # them explicitly.
    ctx.actions.run(
        outputs = outputs,
        executable = ctx.executable.trace_lookup,
        arguments = [args],
        use_default_shell_env = True,
        mnemonic = "TraceLoad",
        progress_message = "trace-load %{label}",
    )
    return [DefaultInfo(files = depset(outputs))]

trace_load = rule(
    implementation = _trace_load_impl,
    attrs = {
        "srckey": attr.string(
            mandatory = True,
            doc = "Hex-encoded srckey seed for the synthetic AC key. write-a renders this from the same value it writes to project B's srckey.txt; passing as a string avoids needing to render srckey.txt into project A and keeps the rule's input set minimal.",
        ),
        "platform": attr.string(
            default = "",
            doc = "Platform tag partitioning the synthetic AC keyspace. Matches the publishing side's --platform.",
        ),
        "expect_make_db": attr.bool(
            default = True,
            doc = "Declares whether the publishing kind emits make-db.txt alongside trace.log. Trace-driven kinds (autotools / make / makemaker / modulebuild / manual / script) set True; cmake / meson round-2 fallback set False.",
        ),
        "expect_config_bundle": attr.bool(
            default = False,
            doc = "Declares whether the publishing kind emits a cmake-config-bundle.tar alongside the trace. Set True for trace-driven kinds that publish a bundle synthesized from the install tree (cross-element configure-step bootstrap); the bundle materializes as <name>/cmake-config-bundle.tar — zero-byte on AC miss, real bytes on hit. The bundle is queried via SyntheticConfigDigest, a separate AC keyspace from the trace. Default False preserves the legacy trace-only shape.",
        ),
        "trace_lookup": attr.label(
            mandatory = True,
            executable = True,
            cfg = "exec",
            doc = "Label of the trace-lookup binary. Pass //tools:trace-lookup — the label resolves in the consuming project's repo (project A / project B), not rules_buildstream_bazel's, so each project's locally staged binary is used.",
        ),
    },
    doc = "Action-time consumer side of the round-2 rendezvous. Materializes the trace's Directory into the action's declared outputs on AC hit; writes zero-byte trace files plus a miss-marker on AC miss.",
)
