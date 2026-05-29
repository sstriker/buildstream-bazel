# rules_buildstream_bazel cmake_split_convert rule.
#
# Orchestrator delivery for `convert-element-cmake --split-packages`.
# One target per --split-packages-mode kind:cmake element. The action
# runs the converter against a shadow source-root and emits the
# discovered-at-action-time per-sub-package BUILD tree as a single
# TreeArtifact directory (`ctx.actions.declare_directory`), so each
# generated BUILD file is content-addressed individually — no opaque
# `build-packages.tar` whose digest churns on any one BUILD changing.
#
# This replaces the prior genrule that tarred a temp PKGTREE into a
# declared `build-packages.tar` output (a genrule can't statically
# declare the sub-package set as `outs`). The TreeArtifact lets Bazel
# track the directory's contents per file while still letting the
# action discover the package set at runtime. stage-b merges the
# materialized directory into project B by per-file content compare.
#
# The action mirrors the old genrule bash exactly: build a shadow
# source-root by merging the `srcs` inputs (real sources + zero stubs)
# under their post-`sources/` suffix, extract each kind:cmake dep's
# cmake-config-bundle.tar into a shared $PREFIX, run the converter in
# --split-packages mode writing BUILDs under the TreeArtifact, then tar
# the synthesized config bundle from the converter's bundle dir.
#
# Flag LOGIC stays out of Starlark: write-a assembles the per-element
# converter flag string (lift / fallback / fidelity / bake-in /
# diagnostics / exports-in / imports-manifest / prefix presence) and
# passes it through `converter_args`. The rule only knows the
# mechanical shadow-build + dep-extract + convert + bundle-tar steps.

def _cmake_split_convert_impl(ctx):
    # TreeArtifact: the per-sub-package BUILD tree. The converter writes
    # one BUILD.bazel per discovered directory beneath this directory
    # (rooted at <packages>/BUILD.bazel for the element-root package).
    # Bazel content-addresses each materialized file individually.
    packages = ctx.actions.declare_directory(ctx.label.name + "/packages")

    # Scalar converter outputs, unchanged from the genrule shape.
    read_paths = ctx.actions.declare_file(ctx.label.name + "/read_paths.json")
    bundle = ctx.actions.declare_file(ctx.label.name + "/cmake-config-bundle.tar")
    exports = ctx.actions.declare_file(ctx.label.name + "/exports.json")

    srcs = ctx.files.srcs
    dep_bundles = ctx.files.dep_bundles
    aux = ctx.files.aux

    # Build the shadow source-root the same way the genrule did: merge
    # real srcs (workspace paths under elements/<name>/sources/) and
    # zero stubs (under bazel-bin/.../sources/) into a fresh shadow dir.
    # Both share a "sources/" segment; strip up to the last one to
    # recover the source-relative suffix. Skip dep artifacts (the
    # bundle tars / imports.json / exports.json) that ride in via srcs
    # in the genrule — here those live in dep_bundles / aux instead, so
    # the case guards are belt-and-suspenders.
    lines = [
        "set -euo pipefail",
        "SHADOW=\"$(mktemp -d)\"",
    ]
    for src in srcs:
        path = src.path
        lines.append("src=%s" % _shquote(path))
        lines.append("case \"$src\" in")
        lines.append("    *cmake-config-bundle.tar) ;;")
        lines.append("    */imports.json) ;;")
        lines.append("    */exports.json) ;;")
        lines.append("    *)")
        lines.append("        rel=\"${src##*sources/}\"")
        lines.append("        mkdir -p \"$SHADOW/$(dirname \"$rel\")\"")
        lines.append("        cp -L \"$src\" \"$SHADOW/$rel\"")
        lines.append("        ;;")
        lines.append("esac")

    # Stage each kind:cmake dep's synth bundle under $PREFIX so
    # find_package(<Pkg> CONFIG) in this consumer's CMakeLists resolves
    # against it. The basename + non-empty filter mirrors the genrule:
    # a dep label can expand to multiple outputs and AC-miss bundles are
    # zero-byte. No-op when the element has no kind:cmake deps.
    prefix_flag = ""
    if dep_bundles:
        lines.append("PREFIX=\"$(mktemp -d)\"")
        for dep in dep_bundles:
            lines.append("tar=%s" % _shquote(dep.path))
            lines.append("if [ \"$(basename \"$tar\")\" = \"cmake-config-bundle.tar\" ] && [ -s \"$tar\" ]; then")
            lines.append("    tar -xf \"$tar\" -C \"$PREFIX\"")
            lines.append("fi")
        prefix_flag = " --prefix-dir=\"$PREFIX\""

    # The TreeArtifact directory is created by declare_directory, but
    # mkdir -p is cheap insurance before the converter writes into it.
    lines.append("BUNDLE_DIR=\"$(mktemp -d)\"")
    lines.append("mkdir -p %s" % _shquote(packages.path))

    convert_cmd = [
        _shquote(ctx.executable.converter.path),
        "--source-root=\"$SHADOW\"",
        "--split-packages",
        "--out-build=%s/BUILD.bazel" % _shquote(packages.path),
        "--out-bundle-dir=\"$BUNDLE_DIR\"",
        "--out-read-paths=%s" % _shquote(read_paths.path),
        "--out-exports=%s" % _shquote(exports.path),
        "--bazel-package-path=%s" % _shquote(ctx.attr.bazel_package_path),
    ]
    convert_line = " ".join(convert_cmd) + prefix_flag
    if ctx.attr.converter_args:
        convert_line += " " + ctx.attr.converter_args
    lines.append(convert_line)

    # Pack the synthesized config bundle the converter wrote into
    # BUNDLE_DIR, same as the genrule's trailing tar step.
    lines.append("tar -cf %s -C \"$BUNDLE_DIR\" ." % _shquote(bundle.path))

    cmd = "\n".join(lines) + "\n"

    ctx.actions.run_shell(
        outputs = [packages, read_paths, bundle, exports],
        inputs = depset(srcs + dep_bundles + aux),
        tools = [ctx.executable.converter],
        command = cmd,
        use_default_shell_env = True,
        mnemonic = "CmakeSplitConvert",
        progress_message = "cmake-split-convert %{label}",
    )
    return [DefaultInfo(files = depset([packages, read_paths, bundle, exports]))]

def _shquote(s):
    # Single-quote a literal for safe interpolation into the bash
    # command. Only the action-built paths above are passed through
    # here; they never legitimately contain single quotes, but quoting
    # keeps spaces / shell metacharacters inert.
    return "'" + s.replace("'", "'\\''") + "'"

cmake_split_convert = rule(
    implementation = _cmake_split_convert_impl,
    attrs = {
        "srcs": attr.label_list(
            allow_files = True,
            doc = "Shadow source-root inputs: the element's real sources (the :<name>_real glob filegroup) plus any zero stubs (:<name>_zero_stubs). Each entry carries a \"sources/\" path segment; the action strips up to the last one to recover the source-relative suffix and copies it into a fresh shadow dir the converter reads as --source-root.",
        ),
        "dep_bundles": attr.label_list(
            allow_files = True,
            default = [],
            doc = "Cross-element cmake-config bundle inputs: each kind:cmake dep's :cmake_config_bundle filegroup (or :<dep>_trace_load outputs). The action untars every basename-cmake-config-bundle.tar non-empty member into a shared $PREFIX so find_package(<Pkg> CONFIG) resolves; passing any entry adds --prefix-dir=$PREFIX to the converter invocation.",
        ),
        "aux": attr.label_list(
            allow_files = True,
            default = [],
            doc = "Auxiliary converter inputs referenced by flags in converter_args (imports.json, dep exports.json). These are staged into the action's input set but NOT shadowed; write-a must reference them in converter_args by their workspace-relative exec path. v1 wires the no-dep / simple case; richer imports/exports threading is documented as a follow-on (see docs/design/cmake-split-packages.md).",
        ),
        "bazel_package_path": attr.string(
            mandatory = True,
            doc = "The element's project-B package path (e.g. \"elements/demo\"), passed to the converter as --bazel-package-path so emitted sub-package labels are rooted correctly.",
        ),
        "converter_args": attr.string(
            default = "",
            doc = "Pre-assembled converter flag string built by write-a (lift / fallback / fidelity / bake-in / diagnostics / exports-in / imports-manifest, etc.). Keeping flag LOGIC in write-a means this rule only encodes the mechanical shadow-build + dep-extract + convert + bundle-tar steps. Appended verbatim to the converter invocation.",
        ),
        "converter": attr.label(
            mandatory = True,
            executable = True,
            cfg = "exec",
            allow_files = True,
            doc = "Label of the convert-element-cmake binary. Pass //tools:convert-element-cmake — the label resolves in the consuming project's repo (project A), matching how the trace_load rule wires //tools:trace-lookup.",
        ),
    },
    doc = "Runs convert-element-cmake in --split-packages mode and emits the discovered-at-action-time per-sub-package BUILD tree as a TreeArtifact directory (content-addressed per file, no tar), plus the scalar read_paths.json / cmake-config-bundle.tar / exports.json outputs. stage-b merges the materialized packages/ directory into project B by per-file content compare.",
)
