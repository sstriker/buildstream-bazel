"""Materializes zero-length stub files at declared paths.

Used by the meta-project (project A) to compose per-element shadow
trees: a convert-element-cmake genrule's `srcs` is the union of real source
files (referenced via `glob(...)` or labels) plus zero-length stubs
for paths cmake's `file(GLOB)` walks would naturally see but cmake
configure doesn't actually open.

Why two reasons converge on the same primitive:

1. **CMake glob shape.** cmake's `file(GLOB)` records directory
   entries during configure. Hiding paths cmake's globs would walk
   shifts the generated graph (a glob that was supposed to match
   N files matches fewer). Zero stubs preserve the shape; cmake
   sees the entry, can't read content, but content was never
   relevant for a pure walk.

2. **Cache-key stability.** A convert-element-cmake action's input
   merkle includes the content of every declared srcs entry.
   Stubbing files cmake never opens means edits to those files'
   real content don't reshape the action key — cache hits across
   semantically-irrelevant edits.

The set of stubbed paths is determined per-element by inverting the
converter's `read_paths.json` output against the element's source
glob. The writer-of-A renders the resulting stub list into the
generated BUILD.bazel.
"""

def _zero_files_impl(ctx):
    outs = []
    for p in ctx.attr.paths:
        f = ctx.actions.declare_file(p)
        ctx.actions.write(output = f, content = "")
        outs.append(f)
    return [DefaultInfo(files = depset(outs))]

zero_files = rule(
    implementation = _zero_files_impl,
    attrs = {
        "paths": attr.string_list(
            doc = "Package-relative paths to materialize as zero-length stubs.",
            mandatory = True,
        ),
    },
    doc = "Materializes zero-length stub files at the given package-relative paths.",
)
