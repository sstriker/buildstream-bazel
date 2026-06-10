"""fortran_library — compile Fortran sources into a linkable archive exposed as
CcInfo, so plain `cc_*` targets can `deps` on it transparently.

Bazel has no canonical Fortran ruleset (rules_fortran is experimental and not
in the BCR), which is why the converter otherwise parks a cmake Fortran
target's sources in a labeled `filegroup` (see lower's partitionFortranSources)
— honest, but unbuildable. This rule is the converter's self-contained Fortran
lowering, the analogue of retagCudaTargets' cuda_library for Fortran:

  - It drives the cc toolchain's OWN compiler executable on the Fortran
    sources. The GNU driver (`gcc` / `gfortran` share the driver) dispatches
    `.f` / `.f90` / ... to the Fortran frontend by file extension, so no
    separate Fortran toolchain has to be discovered or registered — the rule
    rides whatever cc toolchain the build already resolves.
  - It archives the objects through cc_common (a real static library + a
    proper CcLinkingContext) and threads the gfortran runtime (`-lgfortran`)
    as a user link flag, so a consuming `cc_*` target's link resolves the
    Fortran symbols (and the intrinsics libgfortran provides) without the
    operator wiring anything.

Faithfulness + scope: this targets the GNU toolchain (gfortran via the gcc
driver) — the toolchain the converter's survey corpus builds under. A
non-GNU Fortran (flang/ifx) would need its own driver/runtime; that's a
toolchain swap, not a rule change. Module-bearing Fortran (`.mod` produced by
one source and `USE`d by another in the same library) is compiled per-source
here, which resolves intra-library module deps only when the codemodel orders
the sources after their module providers; the netlib reference LAPACK
OpenBLAS ships is fixed-form FORTRAN 77 (module-free) where per-source
compilation is fully parallel. See docs/survey-corpus.md (OpenBLAS rows).
"""

load("@bazel_skylib//lib:shell.bzl", "shell")
load("@rules_cc//cc:action_names.bzl", "ACTION_NAMES")
load("@rules_cc//cc:find_cc_toolchain.bzl", "find_cc_toolchain", "use_cc_toolchain")
load("@rules_cc//cc/common:cc_common.bzl", "cc_common")
load("@rules_cc//cc/common:cc_info.bzl", "CcInfo")

def _impl(ctx):
    cc_toolchain = find_cc_toolchain(ctx)
    feature_configuration = cc_common.configure_features(
        ctx = ctx,
        cc_toolchain = cc_toolchain,
        requested_features = ctx.features,
        unsupported_features = ctx.disabled_features,
    )

    # The cc toolchain's compiler driver. On GNU toolchains this is the gcc/
    # gfortran driver, which routes Fortran source extensions to the Fortran
    # frontend — so we get a Fortran compile without a separate Fortran
    # toolchain. (cc_common.compile dispatches by extension and has no Fortran
    # action, hence the direct driver invocation.)
    compiler = cc_common.get_tool_for_action(
        feature_configuration = feature_configuration,
        action_name = ACTION_NAMES.c_compile,
    )

    # deps' usage requirements: include dirs for INCLUDE / cpp `#include`
    # (preprocessed `.F*` sources) and the transitive linking context so a
    # Fortran library that deps on a C library links it through.
    dep_cc_info = cc_common.merge_cc_infos(
        cc_infos = [d[CcInfo] for d in ctx.attr.deps if CcInfo in d],
    )
    comp_ctx = dep_cc_info.compilation_context

    include_args = []
    # cc-rule `includes` semantics: each dir is PACKAGE-relative and searched
    # in BOTH the source tree and the generated (bin) tree; workspace_root
    # carries the `external/<repo>` prefix when the rendered project is consumed
    # as an external repo. (A bare `-I<inc>` would only resolve for a
    # root-package render of the main repo — wrong for the converter's
    # per-package split BUILD mode and for external consumption.)
    src_root = "/".join([p for p in [ctx.label.workspace_root, ctx.label.package] if p])
    bin_root = "/".join([p for p in [ctx.bin_dir.path, ctx.label.workspace_root, ctx.label.package] if p])
    for inc in ctx.attr.includes:
        rel = src_root + "/" + inc if src_root else inc
        include_args.append("-I" + rel)
        include_args.append("-I" + bin_root + "/" + inc)
    for inc in comp_ctx.includes.to_list():
        include_args.append("-I" + inc)
    for inc in comp_ctx.quote_includes.to_list():
        include_args.append("-iquote" + inc)
    for inc in comp_ctx.system_includes.to_list():
        include_args.append("-isystem" + inc)

    pic_objects = []

    # Module-defining sources (module_srcs) are compiled FIRST, in the order the
    # converter topologically sorted them, into a SHARED module directory: each
    # `module X` emits an `X.mod` gfortran writes with -J and later sources read
    # with -I. gfortran has no automatic source reordering — a `use` of a module
    # whose provider hasn't been compiled yet is a hard error — so these run in a
    # single ordered action that produces the module dir, and the parallel bulk
    # below depends on that dir. (Most Fortran defines no modules: module_srcs is
    # empty and this whole block is skipped.)
    mod_extra_inputs = []
    moddir = None
    if ctx.files.module_srcs:
        moddir = ctx.actions.declare_directory("_fmod/" + ctx.label.name)
        mod_outputs = [moddir]
        cmds = ["mkdir -p " + moddir.path]
        for i, src in enumerate(ctx.files.module_srcs):
            obj = ctx.actions.declare_file("_objs/%s/mod_%d_%s.pic.o" % (ctx.label.name, i, src.basename))
            mod_outputs.append(obj)
            pic_objects.append(obj)
            # shell.quote every interpolated value (paths, copts, include
            # flags) so a token with an embedded quote — e.g. a `-DVERSION='"x"'`
            # define folded into copts by normalizeFortranTarget — can't break
            # the command, matching the Args-based bulk path's robustness.
            parts = [
                shell.quote(compiler),
                "-fPIC",
                "-c",
                shell.quote(src.path),
                "-o",
                shell.quote(obj.path),
                "-J" + shell.quote(moddir.path),
                "-I" + shell.quote(moddir.path),
            ]
            parts += [shell.quote(c) for c in ctx.attr.copts]
            parts += [shell.quote(a) for a in include_args]
            cmds.append(" ".join(parts))
        ctx.actions.run_shell(
            command = " && ".join(cmds),
            inputs = depset(
                direct = ctx.files.module_srcs,
                transitive = [cc_toolchain.all_files, comp_ctx.headers],
            ),
            outputs = mod_outputs,
            mnemonic = "FortranModuleCompile",
            progress_message = "Compiling Fortran modules for %s" % ctx.label.name,
        )
        mod_extra_inputs = [moddir]

    for i, src in enumerate(ctx.files.srcs):
        obj = ctx.actions.declare_file("_objs/%s/%d_%s.pic.o" % (ctx.label.name, i, src.basename))
        args = ctx.actions.args()
        args.add("-fPIC")
        args.add("-c")
        args.add(src)
        args.add("-o", obj)
        # Search the module dir produced above so a `use LA_CONSTANTS` resolves
        # the provider's .mod (no-op when there are no module_srcs).
        if moddir != None:
            args.add("-I" + moddir.path)
        args.add_all(ctx.attr.copts)
        args.add_all(include_args)
        ctx.actions.run(
            executable = compiler,
            arguments = [args],
            inputs = depset(
                direct = [src] + mod_extra_inputs,
                transitive = [cc_toolchain.all_files, comp_ctx.headers],
            ),
            outputs = [obj],
            mnemonic = "FortranCompile",
            progress_message = "Compiling Fortran %{input}",
        )
        pic_objects.append(obj)

    # Archive the objects + build a linking context. Provide the PIC objects as
    # both `objects` and `pic_objects` so either link mode (static / pic)
    # resolves; PIC objects link correctly in a static archive too.
    compilation_outputs = cc_common.create_compilation_outputs(
        objects = depset(pic_objects),
        pic_objects = depset(pic_objects),
    )
    linking_context, linking_outputs = cc_common.create_linking_context_from_compilation_outputs(
        actions = ctx.actions,
        feature_configuration = feature_configuration,
        cc_toolchain = cc_toolchain,
        compilation_outputs = compilation_outputs,
        name = ctx.label.name,
        # libgfortran (and libm) carry the Fortran runtime + intrinsics the
        # objects reference; thread them to the consumer's link line.
        user_link_flags = ["-lgfortran", "-lm"] + ctx.attr.linkopts,
        linking_contexts = [dep_cc_info.linking_context],
    )

    # Expose the archive as the rule's default output so `bazel build //...`
    # (and `bazel build :this`) actually compiles + archives the Fortran. Without
    # a non-empty DefaultInfo the objects are only built on demand when a
    # consumer links the CcInfo — so a fortran_library nothing links would
    # silently never compile (a phantom-green //... build).
    out_files = []
    lib = linking_outputs.library_to_link
    if lib != None:
        for f in (lib.pic_static_library, lib.static_library, lib.dynamic_library):
            if f != None:
                out_files.append(f)

    return [
        DefaultInfo(files = depset(out_files)),
        CcInfo(
            compilation_context = comp_ctx,
            linking_context = linking_context,
        ),
    ]

fortran_library = rule(
    implementation = _impl,
    doc = "Compiles Fortran sources into a linkable archive exposed as CcInfo, " +
          "via the cc toolchain's own driver (GNU gfortran) + the gfortran " +
          "runtime — the converter's self-contained Fortran lowering for cmake " +
          "Fortran targets a cc_* rule can't compile. See the module docstring.",
    attrs = {
        "srcs": attr.label_list(
            allow_files = [
                ".f",
                ".F",
                ".f77",
                ".F77",
                ".f90",
                ".F90",
                ".f95",
                ".F95",
                ".f03",
                ".F03",
                ".f08",
                ".F08",
                ".for",
                ".FOR",
                ".ftn",
                ".fpp",
            ],
            doc = "Fortran sources (.f/.f90/...). Compiled with the cc toolchain's driver.",
        ),
        "module_srcs": attr.label_list(
            allow_files = [
                ".f",
                ".F",
                ".f77",
                ".F77",
                ".f90",
                ".F90",
                ".f95",
                ".F95",
                ".f03",
                ".F03",
                ".f08",
                ".F08",
                ".for",
                ".FOR",
                ".ftn",
                ".fpp",
            ],
            doc = "Fortran sources that DEFINE a module, in dependency order. " +
                  "Compiled first into a shared module dir the other srcs read.",
        ),
        "deps": attr.label_list(
            providers = [CcInfo],
            doc = "C/C++/Fortran libraries this library compiles and links against (CcInfo).",
        ),
        "copts": attr.string_list(doc = "Extra Fortran compile flags."),
        "linkopts": attr.string_list(doc = "Extra link flags threaded to consumers (after -lgfortran -lm)."),
        "includes": attr.string_list(doc = "Include dirs added as -I (mirrors cc_library includes)."),
    },
    toolchains = use_cc_toolchain(),
    fragments = ["cpp"],
)
