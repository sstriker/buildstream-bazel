"""Spike: a downstream element consuming a dep's install TreeArtifact.

This is the analog of autotoolsDepExtractCmd in
cmd/write-a/handler_autotools_native.go, which today untars each
`*/install_tree.tar` dep into a private $DEP_PREFIX before the build:

    for src in $(SRCS); do
      case "$src" in
        */install_tree.tar) tar -xf "$src" -C "$DEP_PREFIX" ;;
      esac
    done
    export CPPFLAGS="-I$DEP_PREFIX/usr/include ..."
    export LDFLAGS="-L$DEP_PREFIX/usr/lib ..."

With a TreeArtifact there is NO tar -xf and NO per-consumer
$DEP_PREFIX copy: the build compiles directly against the dep's
install-root directory in place. Two consumers of the same dep share
the one Directory; nothing is duplicated per edge.
"""

def _build_against_impl(ctx):
    dep_tree = ctx.file.dep
    main_src = ctx.file.src
    out = ctx.actions.declare_file(ctx.label.name)

    # Compile + link against include/ + lib/ INSIDE the dep's tree
    # artifact directory. The directory is an input as-is.
    cmd = 'cc "{src}" -I "{tree}/usr/include" -L "{tree}/usr/lib" -l{lib} -o "{out}"'.format(
        src = main_src.path,
        tree = dep_tree.path,
        lib = ctx.attr.libname,
        out = out.path,
    )

    ctx.actions.run_shell(
        inputs = [main_src, dep_tree],
        outputs = [out],
        command = cmd,
        use_default_shell_env = True,
        mnemonic = "BuildAgainst",
        progress_message = "build-against %{label}",
    )
    return [DefaultInfo(files = depset([out]), executable = out)]

build_against = rule(
    implementation = _build_against_impl,
    executable = True,
    attrs = {
        "src": attr.label(allow_single_file = [".c"], mandatory = True),
        "dep": attr.label(
            allow_single_file = True,
            mandatory = True,
            doc = "An install_tree TreeArtifact dep.",
        ),
        "libname": attr.string(mandatory = True),
    },
    doc = "Compile a source directly against a dep's install TreeArtifact — no tar -xf, no per-consumer $DEP_PREFIX copy.",
)
