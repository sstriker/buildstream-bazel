"""Embeds a file's bytes into a C source + header as a named symbol.

The Bazel-native lowering of the "embed a file as a C array" cmake -P idiom
(VTK's `vtkEncodeString.cmake`; the pattern also recurs across LLVM / Qt / game
engines). Instead of running cmake at build time (the `--cmake-script-runner`
bridge) or freezing bytes at convert time (`--cmake-script-bake`), this rule
runs a small hermetic tool that turns the input file into a `.h` + `.cxx`
exposing its bytes as a named symbol — so the converted project needs neither
cmake nor the converter at build time (the transition end-state for codegen;
see docs/research/codegen-idiom-coverage.md).

Faithfulness: the emitted symbol NAME is the `symbol` attr verbatim (so a
consumer that `#include`s the header and references the symbol is unchanged),
and the symbol's runtime value equals the input file's bytes. The generated-
source formatting is the tool's own (valid, deterministic C) — only the symbol
set and runtime value are load-bearing.

The two predeclared outputs (`out_header`, `out_source`) are referenceable
filename labels, so a downstream `cc_library` lists them in `hdrs` / `srcs`:

    cc_embed(
        name = "shader_gen",
        src = "shader.glsl",
        symbol = "shader_glsl",
        out_header = "shader_glsl.h",
        out_source = "shader_glsl.cxx",
        tool = "//tools:cc-embed",
    )
    cc_library(name = "shader", hdrs = ["shader_glsl.h"], srcs = ["shader_glsl.cxx"])
"""

def _impl(ctx):
    if ctx.attr.nul_terminate and not ctx.attr.binary:
        fail("cc_embed %s: nul_terminate only makes sense with binary = True" % ctx.label)
    if bool(ctx.attr.export_symbol) != bool(ctx.attr.export_header):
        fail("cc_embed %s: export_symbol and export_header must be set together" % ctx.label)

    # The generated source self-includes the header by basename, so the two
    # outputs must live in the same directory; fail fast here rather than as a
    # confusing missing-include compile error downstream.
    if ctx.outputs.out_header.dirname != ctx.outputs.out_source.dirname:
        fail("cc_embed %s: out_header (%s) and out_source (%s) must be in the same directory" %
             (ctx.label, ctx.outputs.out_header.short_path, ctx.outputs.out_source.short_path))

    args = ctx.actions.args()
    args.add("--input", ctx.file.src)
    args.add("--name", ctx.attr.symbol)
    args.add("--header-out", ctx.outputs.out_header)
    args.add("--source-out", ctx.outputs.out_source)
    if ctx.attr.binary:
        args.add("--binary")
    if ctx.attr.nul_terminate:
        args.add("--nul-terminate")
    if ctx.attr.export_symbol:
        args.add("--export-symbol", ctx.attr.export_symbol)
        args.add("--export-header", ctx.attr.export_header)

    ctx.actions.run(
        executable = ctx.executable.tool,
        arguments = [args],
        inputs = [ctx.file.src],
        outputs = [ctx.outputs.out_header, ctx.outputs.out_source],
        mnemonic = "CcEmbed",
        progress_message = "Embedding %s" % ctx.file.src.short_path,
    )
    return [DefaultInfo(files = depset([ctx.outputs.out_header, ctx.outputs.out_source]))]

cc_embed = rule(
    implementation = _impl,
    doc = "Embeds a file's bytes into a generated C source + header (.cxx + .h) " +
          "exposing them as a named symbol, via a hermetic tool — the Bazel-native " +
          "lowering of the \"embed a file as a C array\" cmake -P idiom " +
          "(vtkEncodeString). The predeclared out_header / out_source outputs feed a " +
          "downstream cc_library's hdrs / srcs. See the module docstring for the full " +
          "contract and an example.",
    attrs = {
        "src": attr.label(
            mandatory = True,
            allow_single_file = True,
            doc = "The file whose bytes are embedded.",
        ),
        "symbol": attr.string(
            mandatory = True,
            doc = "C symbol name for the embedded data (the consumer references this).",
        ),
        "out_header": attr.output(
            mandatory = True,
            doc = "Predeclared header output (e.g. shader_glsl.h) for a downstream cc_library hdrs.",
        ),
        "out_source": attr.output(
            mandatory = True,
            doc = "Predeclared source output (e.g. shader_glsl.cxx) for a downstream cc_library srcs.",
        ),
        "binary": attr.bool(
            default = False,
            doc = "Emit an `unsigned char[]` byte array instead of a `const char *` C string.",
        ),
        "nul_terminate": attr.bool(
            default = False,
            doc = "Append a trailing NUL byte (binary only) — e.g. to embed a file as a C string that exceeds compiler string-literal limits.",
        ),
        "export_symbol": attr.string(
            doc = "Visibility/export macro placed before the declaration (mirrors vtk_encode_string EXPORT_SYMBOL). Set with export_header.",
        ),
        "export_header": attr.string(
            doc = "Header included for the export macro (mirrors EXPORT_HEADER). Set with export_symbol.",
        ),
        "tool": attr.label(
            mandatory = True,
            executable = True,
            cfg = "exec",
            doc = "The cc-embed binary (e.g. //tools:cc-embed). Declared executable so Bazel validates the label is runnable, mirroring cmake_configure_file's tool attr.",
        ),
    },
)
