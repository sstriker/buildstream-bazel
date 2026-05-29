# rules_buildstream_bazel pipeline_install + pick_file rules.
#
# The TreeArtifact (`ctx.actions.declare_directory`) replacement for
# the coarse install transport that project B's per-element install
# step emitted as an opaque `install_tree.tar` (file output of a
# genrule). The install root becomes a Bazel TreeArtifact carried
# through Bazel / REAPI as a `Directory` merkle tree, so:
#
#   * identical files dedup in CAS at file granularity — the
#     duplication the ROADMAP flagged for the round-2 fallback
#     (`tar_bytes + Σ extract-genrule bytes`) collapses;
#   * downstream consumers build against the directory IN PLACE — no
#     `tar -xf */install_tree.tar -C $DEP_PREFIX` per-consumer copy
#     (cf. the old autotoolsDepExtractCmd untar loop);
#   * unlike the ruled-out repo-rule alternative, these are ordinary
#     Bazel actions: they run on RBE, do no loading-time work, and
#     stay as hermetic as any other action.
#
# The proof-of-concept lives at experiments/tree-artifact-install/;
# this file is the production port of its three mechanisms (install
# root as a Directory, build-against in place, pick_file projection).
#
# Division of labour (mirrors cmake_packages.bzl): write-a owns the
# per-kind orchestration command STRING; this rule is a mechanical
# host for the well-known tokens that string references. write-a
# builds the command with `@@INSTALL_DIR@@` / `@@SRCS@@` /
# `@@DEP_INSTALL_DIRS@@` / `@@TRACE_LOG@@` / `@@MAKE_DB@@` /
# `@@GENERATED_HEADERS@@` sentinel tokens; the rule `.replace()`s each
# with the action-time exec-root-relative path of the declared output
# / input. Sentinels (not Python `.format` braces) so the shell body
# — which is dense with `${var}` / `$(cmd)` / brace-expansion — passes
# through verbatim with no brace-doubling. No dial / per-kind LOGIC
# lives here.

# Sentinel tokens write-a embeds in the `command` string. Kept in one
# place so the producer (cmd/write-a) and this consumer stay in sync.
_TOK_INSTALL_DIR = "@@INSTALL_DIR@@"
_TOK_SRCS = "@@SRCS@@"
_TOK_DEP_INSTALL_DIRS = "@@DEP_INSTALL_DIRS@@"
_TOK_TRACE_LOG = "@@TRACE_LOG@@"
_TOK_MAKE_DB = "@@MAKE_DB@@"
_TOK_GENERATED_HEADERS = "@@GENERATED_HEADERS@@"

def _pipeline_install_impl(ctx):
    # The install root. A TreeArtifact: its contents aren't known at
    # analysis time; the action populates them. This single line is
    # the whole substitution for `outs = ["install_tree.tar"]`. The
    # command write-a builds installs directly INTO this directory
    # (replacing the old `-C $INSTALL_ROOT . | tar` step).
    install_root = ctx.actions.declare_directory(ctx.label.name + "/install")
    outputs = [install_root]

    command = ctx.attr.command
    command = command.replace(_TOK_INSTALL_DIR, install_root.path)

    # Scalar side outputs the round-2 extension emitted as extra
    # genrule `outs` (trace.log / make-db.txt / generated-headers.txt).
    # Each is declared only when the kind requests it via the matching
    # emit_* bool, mirroring traces.bzl's expect_* gating, so the
    # coarse (round-1 / legacy) path declares only the install root and
    # stays byte-minimal.
    if ctx.attr.emit_trace_log:
        trace_log = ctx.actions.declare_file(ctx.label.name + "/trace.log")
        outputs.append(trace_log)
        command = command.replace(_TOK_TRACE_LOG, trace_log.path)

    if ctx.attr.emit_make_db:
        make_db = ctx.actions.declare_file(ctx.label.name + "/make-db.txt")
        outputs.append(make_db)
        command = command.replace(_TOK_MAKE_DB, make_db.path)

    if ctx.attr.emit_generated_headers:
        generated_headers = ctx.actions.declare_file(ctx.label.name + "/generated-headers.txt")
        outputs.append(generated_headers)
        command = command.replace(_TOK_GENERATED_HEADERS, generated_headers.path)

    # @@SRCS@@: the element's staged sources, space-joined exec-root-
    # relative. The command iterates these the same way the old genrule
    # iterated $(SRCS) (stage into a fresh BUILD_ROOT by stripping the
    # leading sources/ segment).
    command = command.replace(_TOK_SRCS, " ".join([s.path for s in ctx.files.srcs]))

    # @@DEP_INSTALL_DIRS@@: space-joined TreeArtifact directory paths of
    # this element's deps' install roots. The command references each
    # dir IN PLACE (`-I<dir>/usr/include`, `-L<dir>/usr/lib`) — no
    # untar, no per-consumer $DEP_PREFIX. A dep target that provides
    # PipelineInstallInfo contributes only its install-root directory
    # (not its scalar side outputs); other deps contribute their files.
    dep_dirs = []
    dep_inputs = []
    for dep in ctx.attr.deps:
        if PipelineInstallInfo in dep:
            root = dep[PipelineInstallInfo].install_root
            dep_dirs.append(root.path)
            dep_inputs.append(root)
        else:
            for f in dep[DefaultInfo].files.to_list():
                dep_dirs.append(f.path)
                dep_inputs.append(f)
    command = command.replace(_TOK_DEP_INSTALL_DIRS, " ".join(dep_dirs))

    ctx.actions.run_shell(
        outputs = outputs,
        inputs = depset(ctx.files.srcs + dep_inputs),
        tools = ctx.files.tools,
        command = command,
        use_default_shell_env = True,
        mnemonic = "PipelineInstall",
        progress_message = "pipeline-install (TreeArtifact) %{label}",
    )

    # DefaultInfo carries every declared output (so the target builds
    # the install root + side outputs). The install_root is exposed
    # separately via PipelineInstallInfo + as the first DefaultInfo
    # file so consumers / pick_file can grab JUST the directory without
    # dragging the scalar side outputs into their action inputs.
    return [
        DefaultInfo(files = depset(outputs)),
        PipelineInstallInfo(install_root = install_root),
    ]

PipelineInstallInfo = provider(
    doc = "Exposes a pipeline_install target's install-root TreeArtifact so consumers (other pipeline_install deps, pick_file) resolve the directory directly without matching it out of the DefaultInfo fileset.",
    fields = {
        "install_root": "The install-root TreeArtifact File (declare_directory output).",
    },
)

pipeline_install = rule(
    implementation = _pipeline_install_impl,
    attrs = {
        "srcs": attr.label_list(
            allow_files = True,
            doc = "The element's staged source inputs (the :<name>_sources glob filegroup, or @src_<key>//:tree under FUSE-sources). Referenced via the @@SRCS@@ token (space-joined exec-root-relative paths) the command iterates to stage a fresh BUILD_ROOT.",
        ),
        "deps": attr.label_list(
            default = [],
            doc = "Other elements' pipeline_install install-root TreeArtifacts this element builds against. Each is referenced IN PLACE via the @@DEP_INSTALL_DIRS@@ token (no untar). Pass the dep's :<dep>_install target; the rule resolves the directory through PipelineInstallInfo when available (consuming ONLY the directory, not the dep's scalar side outputs), else the DefaultInfo fileset.",
        ),
        "command": attr.string(
            mandatory = True,
            doc = "The orchestration shell command, built by write-a, with well-known sentinel tokens the rule replaces with real paths: @@INSTALL_DIR@@ (the install-root TreeArtifact — the build installs directly into it), @@SRCS@@, @@DEP_INSTALL_DIRS@@, and @@TRACE_LOG@@/@@MAKE_DB@@/@@GENERATED_HEADERS@@ when the matching emit_* bool is set. Sentinels (not Python .format braces) so the shell body's ${var}/$(cmd)/brace-expansion passes through verbatim. Keeping the command in write-a means this rule holds no per-kind LOGIC, only the mechanical declare_directory + token substitution (the cmake_packages.bzl pattern).",
        ),
        "tools": attr.label_list(
            cfg = "exec",
            allow_files = True,
            default = [],
            doc = "Exec-configuration tool dependencies the command invokes (build-tracer / trace-publish / convert-element-trace / etc.). Threaded into the action's tools so they're built for the exec platform. write-a references them by their exec-root path inside the command string.",
        ),
        "emit_trace_log": attr.bool(
            default = False,
            doc = "Declare a trace.log side output and bind the @@TRACE_LOG@@ token. Set by the round-2 trace-driven path (the build-tracer wrap writes the trace there). Default False keeps the coarse path emitting only the install root.",
        ),
        "emit_make_db": attr.bool(
            default = False,
            doc = "Declare a make-db.txt side output and bind the @@MAKE_DB@@ token. Set by round-2 trace-driven kinds that also publish a make database alongside the trace.",
        ),
        "emit_generated_headers": attr.bool(
            default = False,
            doc = "Declare a generated-headers.txt side output and bind the @@GENERATED_HEADERS@@ token. Set when the round-2 extension emits the generated-headers manifest.",
        ),
    },
    provides = [PipelineInstallInfo],
    doc = "Build + install an element into a TreeArtifact install root (declare_directory) instead of an opaque install_tree.tar. The directory dedups in CAS at file granularity and is consumed in place by downstream pipeline_install deps + pick_file. write-a supplies the orchestration command via the `command` attr's tokens; this rule is the mechanical declare_directory host.",
)

def _pick_file_impl(ctx):
    # The install-root TreeArtifact to project a single file out of.
    # Resolve through PipelineInstallInfo when the src target provides
    # it (the common case — src is a pipeline_install target), else
    # fall back to the single declared file (allow_single_file).
    if PipelineInstallInfo in ctx.attr.src:
        tree = ctx.attr.src[PipelineInstallInfo].install_root
    else:
        tree = ctx.file.src

    # Declare the projected file under the rule's name, preserving the
    # basename of the requested path so the cc_import / sh_binary stub
    # consuming it sees the expected filename.
    basename = ctx.attr.path.rsplit("/", 1)[-1]
    out = ctx.actions.declare_file(ctx.label.name + "/" + basename)

    # Copy one entry out of the TreeArtifact into a regular File. On
    # RBE the input is the dep's Directory tree (already in CAS), so
    # this is a single-file materialization — NOT the whole-subset
    # re-materialization the old _install_tree_extract genrule did
    # (which re-emitted every referenced artifact per consumer).
    ctx.actions.run_shell(
        inputs = [tree],
        outputs = [out],
        command = 'cp "{tree}/{path}" "{out}"'.format(
            tree = tree.path,
            path = ctx.attr.path,
            out = out.path,
        ),
        use_default_shell_env = True,
        mnemonic = "PickFile",
        progress_message = "pick-file %{label}",
    )
    return [DefaultInfo(files = depset([out]))]

pick_file = rule(
    implementation = _pick_file_impl,
    attrs = {
        "src": attr.label(
            allow_single_file = True,
            mandatory = True,
            doc = "The pipeline_install target (or its install-root TreeArtifact label) to project a file out of. Resolved through PipelineInstallInfo when present, else the single declared file.",
        ),
        "path": attr.string(
            mandatory = True,
            doc = "Path inside the TreeArtifact to extract, e.g. \"usr/lib/libfoo.a\". The projected File keeps this path's basename.",
        ),
    },
    doc = "Project one file out of a pipeline_install TreeArtifact into a plain File label — the cc_import / sh_binary stub mechanism that replaces the round-2 fallback's _install_tree_extract genrule (no per-consumer re-materialization of the whole tree).",
)
