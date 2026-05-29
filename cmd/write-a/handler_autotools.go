package main

// autotoolsPipelineHandler returns the coarse install-pipeline
// shape of kind:autotools (./configure + make + make install
// running via the pipeline_install rule, output is the
// install-root TreeArtifact). Used as
// the cache-miss branch of nativeAutotoolsHandler. Defined as
// a function rather than a top-level var so init-order across
// handler files stays well-defined.
func autotoolsBasePipelineHandler() pipelineHandler {
	// kind:autotools is BuildStream's `autotools` plugin lowered onto
	// the pipelineHandler shape. The plugin's defaults (per
	// buildstream/src/buildstream/plugins/elements/autotools.py) layer
	// the canonical autoconf install pipeline:
	//
	//   configure-commands: ["%{configure}"]
	//   build-commands:     ["%{make}"]
	//   install-commands:   ["%{make-install}"]
	//
	// where %{configure} is itself a multi-line shell script chaining
	// %{autogen} (which detects autogen / autogen.sh / bootstrap /
	// autoreconf -ivf) and %{conf-cmd} %{conf-args} (./configure with
	// the canonical --prefix / --bindir / --libdir / ... flags).
	//
	// For v1 every per-element override path BuildStream supports is
	// already covered by the existing precedence chain (project.conf <
	// kind defaults < element variables): an element overriding
	// %{conf-local} adds extra flags to ./configure without re-stating
	// the surrounding shape; an element overriding %{configure}
	// outright replaces the whole shell script.
	//
	// Toolchain integration deferred. autotools' configure script
	// reads CC / CFLAGS / LDFLAGS from the environment to pick the
	// compiler. For now the genrule cmd inherits whatever cc is on
	// PATH — same path kind:make uses today. cc_toolchain integration
	// (toolchains = ["@bazel_tools//tools/cpp:current_cc_toolchain"]
	// + $(CC) make-vars piped into the cmd's exported CC / CFLAGS) is
	// a follow-up that affects every pipeline kind, not just
	// autotools, so it lands once the host-toolchain shape stops
	// being sufficient for FDSDK reality-check.
	return pipelineHandler{
		kindName: "autotools",
		defaultVars: map[string]string{
			// %{autogen} regenerates the configure script when missing.
			// First branch (configure already executable) is a no-op:
			// %{configure} below runs configure itself with the
			// canonical autoconf flags, so autogen here just needs to
			// ensure ./configure exists. NOCONFIGURE=1 tells the
			// autogen.sh / bootstrap variants to stop after generating
			// the configure script (don't run it themselves; that's
			// our job after autogen returns).
			"autogen": `export NOCONFIGURE=1;
if [ -x %{conf-cmd} ]; then
  true;
elif [ -x autogen ]; then
  command ./autogen;
elif [ -x autogen.sh ]; then
  command ./autogen.sh;
elif [ -x bootstrap ]; then
  command ./bootstrap;
elif [ -x bootstrap.sh ]; then
  command ./bootstrap.sh;
else
  autoreconf -ivf;
fi`,

			"conf-cmd": "./configure",

			// Canonical autoconf path-flag set; --prefix / --bindir /
			// etc. all flow through the variable resolver so an
			// element or project.conf overriding %{prefix} reshapes
			// every flag automatically. Trailing %{conf-local} is the
			// per-element extra-flags hook BuildStream documents.
			"conf-args": `--prefix=%{prefix} \
--exec-prefix=%{exec_prefix} \
--bindir=%{bindir} \
--sbindir=%{sbindir} \
--sysconfdir=%{sysconfdir} \
--datadir=%{datadir} \
--includedir=%{includedir} \
--libdir=%{libdir} \
--libexecdir=%{libexecdir} \
--localstatedir=%{localstatedir} \
--sharedstatedir=%{sharedstatedir} \
--mandir=%{mandir} \
--infodir=%{infodir} \
%{conf-local}`,

			"conf-local": "",

			"configure": `%{autogen}
%{conf-cmd} %{conf-args}`,

			"make":         "make",
			"make-install": `make -j1 DESTDIR="%{install-root}" install`,

			// Libtool .la file scrubbing — buildstream-plugins'
			// autotools plugin runs this in install-commands by
			// default, since stale .la files break downstream
			// linkers when the install layout differs from the
			// libtool-recorded paths. FDSDK references
			// %{delete-libtool-archives} from autotools-conf.yml's
			// install-commands tail; the per-element BUILD just
			// inlines the resolved value.
			"remove-libtool-modules":   "false",
			"remove-libtool-libraries": "false",
			"delete-libtool-archives": `if %{remove-libtool-modules} || %{remove-libtool-libraries}; then
    find "%{install-root}" -name "*.la" -print0 | while read -d '' -r file; do
        if grep '^shouldnotlink=yes$' "${file}" &>/dev/null; then
            if %{remove-libtool-modules}; then
                rm "${file}"
            fi
        else
            if %{remove-libtool-libraries}; then
                rm "${file}"
            fi
        fi
    done
fi`,
		},
		defaults: pipelineDefaults{
			Configure: []string{"%{configure}"},
			Build:     []string{"%{make}"},
			Install:   []string{"%{make-install}"},
		},
	}
}
