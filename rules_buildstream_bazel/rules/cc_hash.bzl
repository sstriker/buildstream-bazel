"""Hashes a file's bytes into a generated header that `#define`s the digest.

The Bazel-native lowering of the "hash a file into a generated header" cmake -P
idiom (VTK's `vtkHashSource.cmake`: `vtk_hash_source()` computes a digest of an
input file and bakes it into a header consumers `#include`). Instead of running
cmake at build time (the `--cmake-script-runner` bridge) or freezing the digest
at convert time (`--cmake-script-bake`), this rule runs a small hermetic tool
that recomputes the digest and writes the header — so the converted project
needs neither cmake nor the converter at build time, and the digest
auto-refreshes when the input changes (the bake's gap; see
docs/research/codegen-idiom-coverage.md).

Faithfulness: the generated header is byte-for-byte what vtkHashSource.cmake
writes —

    #ifndef <define_name>
     #define <define_name> "<digest>"
    #endif

with `<define_name>` taken verbatim from the attr and `<digest>` the
lowercase-hex digest under `algorithm`, matching cmake's `file(<ALGO> …)`.

The predeclared `out_header` output is a referenceable filename label, so a
downstream `cc_library` lists it in `hdrs`:

    cc_hash(
        name = "sockcomm_hash",
        src = "vtkSocketCommunicator.cxx",
        define_name = "vtkSocketCommunicatorHash",
        algorithm = "SHA256",
        out_header = "vtkSocketCommunicatorHash.h",
        tool = "//tools:cc-hash",
    )
    cc_library(name = "core", hdrs = ["vtkSocketCommunicatorHash.h"], srcs = [...])
"""

_ALGORITHMS = ["MD5", "SHA1", "SHA224", "SHA256", "SHA384", "SHA512"]

def _impl(ctx):
    if ctx.attr.algorithm not in _ALGORITHMS:
        fail("cc_hash %s: algorithm = %r must be one of %s" % (ctx.label, ctx.attr.algorithm, _ALGORITHMS))

    args = ctx.actions.args()
    args.add("--input", ctx.file.src)
    args.add("--name", ctx.attr.define_name)
    args.add("--algorithm", ctx.attr.algorithm)
    args.add("--header-out", ctx.outputs.out_header)

    ctx.actions.run(
        executable = ctx.executable.tool,
        arguments = [args],
        inputs = [ctx.file.src],
        outputs = [ctx.outputs.out_header],
        mnemonic = "CcHash",
        progress_message = "Hashing %s" % ctx.file.src.short_path,
    )
    return [DefaultInfo(files = depset([ctx.outputs.out_header]))]

cc_hash = rule(
    implementation = _impl,
    doc = "Hashes a file's bytes into a generated header that `#define`s the digest " +
          "as a C string, via a hermetic tool — the Bazel-native lowering of the " +
          "\"hash a file into a generated header\" cmake -P idiom (vtkHashSource). " +
          "The predeclared out_header output feeds a downstream cc_library's hdrs. " +
          "See the module docstring for the full contract and an example.",
    attrs = {
        "src": attr.label(
            mandatory = True,
            allow_single_file = True,
            doc = "The file whose bytes are hashed.",
        ),
        "define_name": attr.string(
            mandatory = True,
            doc = "C #define name (and include guard) the digest is bound to (the consumer references this).",
        ),
        "algorithm": attr.string(
            default = "MD5",
            values = _ALGORITHMS,
            doc = "Digest algorithm: MD5, SHA1, SHA224, SHA256, SHA384 or SHA512 (mirrors vtk_hash_source ALGORITHM; default MD5, as cmake's).",
        ),
        "out_header": attr.output(
            mandatory = True,
            doc = "Predeclared header output (e.g. vtkSocketCommunicatorHash.h) for a downstream cc_library hdrs.",
        ),
        "tool": attr.label(
            mandatory = True,
            executable = True,
            cfg = "exec",
            doc = "The cc-hash binary (e.g. //tools:cc-hash). Declared executable so Bazel validates the label is runnable, mirroring cc_embed's tool attr.",
        ),
    },
)
