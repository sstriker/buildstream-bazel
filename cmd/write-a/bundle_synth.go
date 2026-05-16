package main

// bundle_synth.go — shared shell snippet for the round-2
// install genrules' cmake-config bundle synthesis pass.
//
// The kind:cmake / kind:meson / pipeline (autotools / make /
// makemaker / modulebuild / manual / script) install genrules
// each end with the same shape: walk $INSTALL_ROOT for any
// pkg-config or cmake-config layout the element installed,
// copy what's found into $CONFIG_BUNDLE_DIR/lib/{pkgconfig,
// cmake}/, tar it up, publish.
//
// Pre-this-helper each handler inlined its own walk. The
// pipeline handler covered the autotools cases (usr/lib +
// usr/local/lib), the cmake / meson handlers only covered
// the canonical lib/. None covered:
//
//   - lib64/pkgconfig and lib64/cmake — RedHat-family 64-bit.
//   - usr/lib64 + usr/local/lib64 equivalents.
//   - usr/share/pkgconfig — architecture-independent .pc files
//     (autotools' default for noarch .pc files).
//   - Multiarch layouts (Debian / Ubuntu): usr/lib/<triplet>/
//     pkgconfig, e.g. usr/lib/x86_64-linux-gnu/pkgconfig.
//     FDSDK is Debian-shaped; this is the real-world miss case.
//
// The unified helper makes all three handlers walk the broader
// set. Cost is a handful of additional `[ -d ]` tests per
// install action (mostly false); benefit is no silent .pc /
// .cmake misses on FDSDK-shaped consumers downstream.
//
// Shell contract:
//   - $$INSTALL_ROOT and $$CONFIG_BUNDLE_DIR must be defined.
//   - $$ is Bazel genrule's escape for $; the snippet is meant
//     to be embedded in a `cmd =` template processed by Bazel.
//   - Errors from individual cp invocations are suppressed
//     (2>/dev/null || true) because the [ -d ] guard already
//     handles the common "doesn't exist" case; suppression
//     keeps stray-permission corner cases from breaking the
//     publish.

// bundleSynthShell returns the shell snippet that walks
// $INSTALL_ROOT for pkg-config and cmake-config layouts and
// stages them under $CONFIG_BUNDLE_DIR/lib/{pkgconfig,cmake}/.
// Caller is responsible for defining $$INSTALL_ROOT,
// $$CONFIG_BUNDLE_DIR, and for tarring $$CONFIG_BUNDLE_DIR
// after this snippet runs.
//
// The two kinds of inputs (fixed list + glob) handle the
// common-prefix and multiarch cases respectively. POSIX sh
// leaves an unmatched glob literal; the [ -d ] guard makes
// the unmatched case a no-op.
//
// Precedence when the same basename lives in multiple prefixes:
// later writes overwrite earlier (cp truncates), so the
// iteration order — lib → lib64 → usr/lib → usr/lib64 →
// usr/local/lib → usr/local/lib64 → usr/share → multiarch
// globs — means more-specific prefixes win over less-specific
// ones for the same file basename. Pathological in practice
// (well-formed installs don't ship duplicate .pc files at
// multiple prefixes); documented so a future operator who
// hits the case knows which copy ends up in the bundle.
func bundleSynthShell() string {
	return `        # Synthesize a cmake-config bundle from the install tree
        # for the cross-element configure-step bootstrap rendezvous
        # (see docs/design/cross-element-config-rendezvous.md).
        # Pkg-config files (*.pc) and cmake-config files (lib/cmake/
        # <Pkg>/) the element installed are copied verbatim.
        # Elements that install neither produce an empty bundle —
        # downstream consumers skip staging when the bundle's tar
        # is empty.
        #
        # Prefix coverage spans the common install layouts:
        #   - lib/{pkgconfig,cmake} — Debian/Ubuntu native, meson
        #     default, cmake default
        #   - lib64/{pkgconfig,cmake} — RedHat-family 64-bit
        #   - usr/{lib,lib64,share}/... — when --prefix=/usr
        #     (autotools default for some configure scripts)
        #   - usr/local/{lib,lib64}/... — when --prefix=/usr/local
        #     (autotools default for most configure scripts)
        #   - usr/lib/<triplet>/pkgconfig and lib/<triplet>/pkgconfig
        #     — Debian/Ubuntu multiarch (FDSDK is Debian-shaped;
        #     this is the real-world common case).
        #
        # The fixed-path probes use explicit [ -d ] guards. The
        # multiarch globs rely on POSIX sh leaving unmatched globs
        # literal, which then fail the [ -d ] guard cleanly.
        export CONFIG_BUNDLE_DIR="$$(mktemp -d)"
        for d in \
            lib/pkgconfig \
            lib64/pkgconfig \
            usr/lib/pkgconfig \
            usr/lib64/pkgconfig \
            usr/local/lib/pkgconfig \
            usr/local/lib64/pkgconfig \
            usr/share/pkgconfig; do
            if [ -d "$$INSTALL_ROOT/$$d" ]; then
                mkdir -p "$$CONFIG_BUNDLE_DIR/lib/pkgconfig"
                cp -r "$$INSTALL_ROOT/$$d"/. "$$CONFIG_BUNDLE_DIR/lib/pkgconfig/" 2>/dev/null || true
            fi
        done
        for d in "$$INSTALL_ROOT"/usr/lib/*/pkgconfig "$$INSTALL_ROOT"/lib/*/pkgconfig; do
            if [ -d "$$d" ]; then
                mkdir -p "$$CONFIG_BUNDLE_DIR/lib/pkgconfig"
                cp -r "$$d"/. "$$CONFIG_BUNDLE_DIR/lib/pkgconfig/" 2>/dev/null || true
            fi
        done
        for d in \
            lib/cmake \
            lib64/cmake \
            usr/lib/cmake \
            usr/lib64/cmake \
            usr/local/lib/cmake \
            usr/local/lib64/cmake \
            usr/share/cmake; do
            if [ -d "$$INSTALL_ROOT/$$d" ]; then
                mkdir -p "$$CONFIG_BUNDLE_DIR/lib/cmake"
                cp -r "$$INSTALL_ROOT/$$d"/. "$$CONFIG_BUNDLE_DIR/lib/cmake/" 2>/dev/null || true
            fi
        done
        for d in "$$INSTALL_ROOT"/usr/lib/*/cmake "$$INSTALL_ROOT"/lib/*/cmake; do
            if [ -d "$$d" ]; then
                mkdir -p "$$CONFIG_BUNDLE_DIR/lib/cmake"
                cp -r "$$d"/. "$$CONFIG_BUNDLE_DIR/lib/cmake/" 2>/dev/null || true
            fi
        done
        export CONFIG_BUNDLE_TAR="$$(mktemp)"
        tar --mtime=@0 --sort=name --owner=0 --group=0 --numeric-owner \
            -cf "$$CONFIG_BUNDLE_TAR" -C "$$CONFIG_BUNDLE_DIR" .`
}
