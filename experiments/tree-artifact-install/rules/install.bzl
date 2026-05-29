"""Spike: model the install root as a Bazel TreeArtifact.

This is the `declare_directory` analog of project B's per-element
install genrule, whose output today is an opaque `install_tree.tar`
(see cmd/write-a/handler_autotools_native.go). Here the install root
is a TreeArtifact instead: `ctx.actions.declare_directory(...)`.

Why it matters for the install_tree.tar question:

  * A TreeArtifact is carried through Bazel / REAPI as a `Directory`
    merkle tree, so identical files dedup in CAS at file granularity
    — the duplication the ROADMAP flags for the round-2 fallback
    (`tar_bytes + Σ extract-genrule bytes`) collapses.
  * Unlike the rejected repo-rule alternative, TreeArtifact actions
    run on RBE like any other action — no loading-time work, no
    startup block, no host-toolchain hermeticity hole.

Two rules live here:
  install_tree  — produce the install root as a TreeArtifact.
  pick_file     — project one file out of the tree as a plain File
                  label (the mechanism a round-2 fallback cc_import
                  stub would use in place of an extract genrule).
"""

def _install_tree_impl(ctx):
    # The install root. A TreeArtifact: its contents aren't known at
    # analysis time; the action populates them. This single line is
    # the whole substitution for `outs = ["install_tree.tar"]`.
    install_root = ctx.actions.declare_directory(ctx.label.name)

    srcs = ctx.files.srcs

    # Mimic `./configure && make && make install --prefix=/usr`:
    # compile .c sources into objects, archive them into
    # usr/lib/lib<name>.a, and stage .h headers into usr/include.
    # Everything lands *inside* the TreeArtifact directory.
    cmd = """
set -euo pipefail
ROOT="{root}"
mkdir -p "$ROOT/usr/include" "$ROOT/usr/lib"
objs=""
for src in {srcs}; do
  case "$src" in
    *.c)
      obj="$(mktemp).o"
      cc -c "$src" -o "$obj"
      objs="$objs $obj"
      ;;
    *.h)
      cp "$src" "$ROOT/usr/include/"
      ;;
  esac
done
ar rcs "$ROOT/usr/lib/lib{lib}.a" $objs
""".format(
        root = install_root.path,
        srcs = " ".join([s.path for s in srcs]),
        lib = ctx.attr.libname,
    )

    ctx.actions.run_shell(
        inputs = srcs,
        outputs = [install_root],
        command = cmd,
        use_default_shell_env = True,
        mnemonic = "InstallTree",
        progress_message = "install-tree (TreeArtifact) %{label}",
    )
    return [DefaultInfo(files = depset([install_root]))]

install_tree = rule(
    implementation = _install_tree_impl,
    attrs = {
        "srcs": attr.label_list(allow_files = [".c", ".h"]),
        "libname": attr.string(mandatory = True),
    },
    doc = "Build + install sources into a TreeArtifact install root (usr/include + usr/lib): the declare_directory analog of install_tree.tar.",
)

def _pick_file_impl(ctx):
    tree = ctx.file.tree
    out = ctx.actions.declare_file(ctx.label.name + "/" + ctx.attr.rel.rsplit("/", 1)[-1])

    # Copy one entry out of the TreeArtifact into a regular File. On
    # RBE the input is the dep's Directory tree (already in CAS), so
    # this is a single-file materialization, not a re-tar of the
    # whole install root the way the extract genrule re-materializes
    # a subset today.
    ctx.actions.run_shell(
        inputs = [tree],
        outputs = [out],
        command = 'cp "{tree}/{rel}" "{out}"'.format(
            tree = tree.path,
            rel = ctx.attr.rel,
            out = out.path,
        ),
        mnemonic = "PickFile",
        progress_message = "pick %{label}",
    )
    return [DefaultInfo(files = depset([out]))]

pick_file = rule(
    implementation = _pick_file_impl,
    attrs = {
        "tree": attr.label(allow_single_file = True, mandatory = True),
        "rel": attr.string(
            mandatory = True,
            doc = "Path inside the TreeArtifact, e.g. usr/lib/libfoo.a.",
        ),
    },
    doc = "Project one file out of a TreeArtifact into a plain File label — the cc_import-stub mechanism that replaces the round-2 extract genrule.",
)
