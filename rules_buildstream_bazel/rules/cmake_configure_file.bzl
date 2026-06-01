"""Re-renders a cmake configure_file / file(GENERATE) template at build time.

The configure-file **lift tier**. Instead of baking cmake's rendered bytes into
the BUILD (which couples the BUILD to template content and goes stale on edits),
this rule re-runs the cmake-configure-file tool at Bazel build time against the
original template plus the captured cmake variable namespace. Editing the
template re-renders through Bazel without re-running the converter.

It replaces the previous genrule-with-base64-in-shell lift shape. The
substitution inputs ride as readable Starlark attributes:

  * `values` / `genex_values` — string dicts,
  * `genex_context` — a JSON string (for the Go-side genex evaluator),
  * `content` — the inline CONTENT body, or `template` — an on-disk INPUT file.

The impl materializes the JSON sidecars with `ctx.actions.write` and passes an
argv array to the tool via `ctx.actions.run` — so there is no shell-quoting /
escaping surface and no base64 anywhere. `values` being a real dict is readable
at any size; nothing is routed by length.
"""

def _impl(ctx):
    # Fail fast on the mutually-exclusive attribute pairs the docstrings
    # describe, rather than letting the tool reject genex_values+genex_context
    # later with a less direct error or silently preferring template over a
    # meaningful content. The emitter only ever sets one of each, so these
    # guard hand-written / future callers. (content == "" is the default and
    # indistinguishable from unset, so only a non-empty content clashes with
    # a template label — that's the genuinely ambiguous case.)
    if ctx.attr.genex_values and ctx.attr.genex_context:
        fail("cmake_configure_file %s: genex_values and genex_context are mutually exclusive; set at most one" % ctx.label)
    if ctx.file.template and ctx.attr.content:
        fail("cmake_configure_file %s: template and content are mutually exclusive; set at most one" % ctx.label)

    # out is a predeclared output (attr.output), so the generated file is
    # addressable by its filename label (e.g. //pkg:config.h) — which is how
    # the emitted cc_library references it (hdrs = ["config.h"]), matching the
    # genrule outs=[...] shape this replaces.
    out = ctx.outputs.out
    args = ctx.actions.args()
    inputs = []

    # --values: always present (possibly the empty map for file(GENERATE)
    # COPYONLY lifts). Materialized to a JSON sidecar at build time so the
    # full cmake namespace never lands in the source tree.
    values_json = ctx.actions.declare_file(ctx.label.name + ".values.json")
    ctx.actions.write(values_json, json.encode(ctx.attr.values))
    inputs.append(values_json)
    args.add("--values", values_json)

    # Stamp values: re-read VCS-stamp template vars from the workspace
    # status at build time, overriding the baked `values` fallback. The
    # stable status file (ctx.info_file / stable-status.txt) is the source
    # — it is cache-keyed, so a revision change correctly re-renders. Under
    # `--stamp` + `--workspace_status_command` it carries the operator's
    # STABLE_* keys; otherwise it holds only the defaults and the tool
    # keeps the `values` fallback (a key the tool doesn't find is left
    # alone). One --stamp-value flag per (template var, status key) entry.
    if ctx.attr.stamp_values:
        inputs.append(ctx.info_file)
        args.add("--status-file", ctx.info_file.path)

        # Sorted keys → a stable action command line. A dict's iteration
        # order shouldn't leak into the argv (it would risk gratuitous
        # action-cache misses); sorting makes the --stamp-value sequence
        # deterministic regardless of how the attr dict was constructed.
        for tmpl_var in sorted(ctx.attr.stamp_values):
            args.add("--stamp-value", "%s=%s" % (tmpl_var, ctx.attr.stamp_values[tmpl_var]))

    if ctx.attr.at_only:
        args.add("--at-only")
    if ctx.attr.copy_only:
        args.add("--copy-only")
    if ctx.attr.escape_quotes:
        args.add("--escape-quotes")
    if ctx.attr.newline_style:
        args.add("--newline-style", ctx.attr.newline_style)

    # Genex replay shapes are mutually exclusive (the tool rejects both).
    if ctx.attr.genex_values:
        genex_values_json = ctx.actions.declare_file(ctx.label.name + ".genex_values.json")
        ctx.actions.write(genex_values_json, json.encode(ctx.attr.genex_values))
        inputs.append(genex_values_json)
        args.add("--genex-values", genex_values_json)

    if ctx.attr.genex_context:
        genex_context_json = ctx.actions.declare_file(ctx.label.name + ".genex_context.json")
        ctx.actions.write(genex_context_json, ctx.attr.genex_context)
        inputs.append(genex_context_json)
        args.add("--genex-context", genex_context_json)

    # $<TARGET_FILE:name> — each label resolves to exactly one file whose
    # build-time path feeds --target-file name=<path>. Label-keyed so Bazel
    # tracks the dependency and resolves the path here, with no genrule-style
    # $(location) shell expansion.
    for dep, name in ctx.attr.target_files.items():
        files = dep[DefaultInfo].files.to_list()
        if len(files) != 1:
            fail("cmake_configure_file %s: target_files entry %s must resolve to exactly one file, got %d" %
                 (ctx.label, dep.label, len(files)))
        inputs.append(files[0])
        args.add("--target-file", "%s=%s" % (name, files[0].path))

    # $<TARGET_OBJECTS:name> — the label's default outputs are the object
    # files; join their paths with ':' (the tool's wire delimiter, since
    # cmake's native ';' is also a statement terminator).
    for dep, name in ctx.attr.target_objects.items():
        objs = dep[DefaultInfo].files.to_list()
        if not objs:
            fail("cmake_configure_file %s: target_objects entry %s resolved to no files" %
                 (ctx.label, dep.label))
        inputs.extend(objs)
        args.add("--target-objects", "%s=%s" % (name, ":".join([o.path for o in objs])))

    # Template source. INPUT form: a real file label. CONTENT form: the
    # inline body, written to a file and fed positionally — so the tool always
    # reads a template file and --content-base64 is never used.
    if ctx.file.template:
        template = ctx.file.template
    else:
        template = ctx.actions.declare_file(ctx.label.name + ".content.in")
        ctx.actions.write(template, ctx.attr.content)
    inputs.append(template)
    args.add(template)
    args.add(out)

    ctx.actions.run(
        executable = ctx.executable.tool,
        arguments = [args],
        inputs = inputs,
        outputs = [out],
        mnemonic = "CMakeConfigureFile",
        progress_message = "Configuring %s" % out.short_path,
    )
    return [DefaultInfo(files = depset([out]))]

cmake_configure_file = rule(
    implementation = _impl,
    attrs = {
        "out": attr.output(
            mandatory = True,
            doc = "Output file the rule produces. A predeclared output so the " +
                  "generated file is referenceable by its filename label " +
                  "(e.g. //pkg:config.h) from downstream cc_library hdrs/srcs.",
        ),
        "template": attr.label(
            allow_single_file = True,
            doc = "Template file (INPUT form). Leave unset to use the inline `content` (CONTENT form).",
        ),
        "content": attr.string(
            doc = "Inline template body (CONTENT form). Used only when `template` is unset.",
        ),
        "values": attr.string_dict(
            doc = "cmake variable -> value substitution map.",
        ),
        "stamp_values": attr.string_dict(
            doc = "template var -> Bazel workspace-status key (e.g. GIT_SHA -> " +
                  "STABLE_GIT_SHA). At build time the value is read from the stable " +
                  "workspace status (ctx.info_file) and overrides the baked `values` " +
                  "entry — the VCS-stamp lift, so a `@GIT_SHA@` header re-reads the " +
                  "live revision rather than the convert-time one. Populate the keys " +
                  "with --workspace_status_command and build with --stamp; an absent " +
                  "key keeps the `values` fallback. Empty for non-stamp configure_files.",
        ),
        "genex_values": attr.string_dict(
            doc = "Captured `$<...>` literal -> resolved bytes (structured-replay lift). " +
                  "Mutually exclusive with `genex_context`.",
        ),
        "genex_context": attr.string(
            doc = "cmake configure-time context JSON for the Go-side genex evaluator. " +
                  "Mutually exclusive with `genex_values`.",
        ),
        "target_files": attr.label_keyed_string_dict(
            allow_files = True,
            doc = "label -> cmake target name for `$<TARGET_FILE:name>` resolution.",
        ),
        "target_objects": attr.label_keyed_string_dict(
            allow_files = True,
            doc = "label -> cmake object-library name for `$<TARGET_OBJECTS:name>` resolution.",
        ),
        "tool": attr.label(
            mandatory = True,
            executable = True,
            cfg = "exec",
            doc = "The cmake-configure-file binary (e.g. //tools:cmake-configure-file). " +
                  "Declared executable so Bazel validates the label is runnable, mirroring " +
                  "trace_load's trace_lookup attr.",
        ),
        "at_only": attr.bool(doc = "Mirror configure_file's @ONLY."),
        "copy_only": attr.bool(doc = "Mirror configure_file's COPYONLY."),
        "escape_quotes": attr.bool(doc = "Mirror configure_file's ESCAPE_QUOTES."),
        "newline_style": attr.string(doc = "Mirror configure_file's NEWLINE_STYLE: '' (preserve), 'lf', or 'crlf'."),
    },
    doc = "Re-renders a cmake configure_file / file(GENERATE) template at build time.",
)
