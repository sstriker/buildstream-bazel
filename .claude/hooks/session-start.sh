#!/bin/bash
# SessionStart hook — provision the toolchains the survey corpus + gates
# need that the base Claude-Code-on-the-web container doesn't ship. The
# base image already has go / ninja / bazel / git, plus an older system
# cmake that the cmake step bumps to the repo's CMAKE_VERSION pin.
#
# Provisions:
#   - cmake (default)               — bump to the Makefile CMAKE_VERSION pin
#                                     (cmake 4.x); the base image's system
#                                     cmake is older than production's.
#   - gfortran (default)            — OpenBLAS/eigen real Fortran path.
#   - buildifier (default)          — lens-2 canonical-form check.
#   - bazelisk (default)            — the repo-pinned bazel launcher the
#                                     gates expect, fetched from GitHub
#                                     releases (releases.bazel.build is
#                                     blocked in this sandbox; GitHub
#                                     isn't — so BAZELISK_BASE_URL points
#                                     there).
#   - CUDA toolkit (BSB_PROVISION_CUDA=1)  — cutlass/cuda-samples; opt-in
#                                            (multi-GB).
#   - gazelle_cc warm (BSB_WARM_GAZELLE=1) — pre-build gazelle_cc into the
#                                            persistent survey cache;
#                                            opt-in (~2min cold + BCR).
#
# Web-only (gated on $CLAUDE_CODE_REMOTE), idempotent, non-interactive,
# synchronous (correctness over latency: the survey + go test paths
# shouldn't race a half-installed toolchain).
set -euo pipefail

if [ "${CLAUDE_CODE_REMOTE:-}" != "true" ]; then
  exit 0
fi

log() { echo "session-start: $*" >&2; }

# CLAUDE_PROJECT_DIR is normally exported by Claude Code for hooks; default it
# from this script's own location if unset, so `set -u` can't abort the whole
# hook (this cmake step + the bazelisk step both reference it) on a stray
# missing var — provisioning should degrade gracefully, not hard-fail.
CLAUDE_PROJECT_DIR="${CLAUDE_PROJECT_DIR:-$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)}"

# Bazel releases are served from GitHub here (releases.bazel.build 403s in
# this sandbox; github.com/bazelbuild/bazel/releases is reachable). The
# repo's install-bazelisk.sh symlinks `bazel` -> bazelisk, so EVERY `bazel`
# invocation goes through bazelisk and needs this base URL — not just the
# ones that source CLAUDE_ENV_FILE. Set it BOTH places: a global
# /etc/profile.d default (so a bare `bazel` in any shell resolves) and the
# session env file (so the agent loop inherits it immediately).
BAZELISK_GH_BASE="https://github.com/bazelbuild/bazel/releases/download"
export BAZELISK_BASE_URL="$BAZELISK_GH_BASE"
if [ -w /etc/profile.d ] || [ "$(id -u)" -eq 0 ]; then
  echo "export BAZELISK_BASE_URL=\"$BAZELISK_GH_BASE\"" > /etc/profile.d/bazelisk-base-url.sh 2>/dev/null \
    || log "note: could not write /etc/profile.d/bazelisk-base-url.sh (bare-shell bazel may need BAZELISK_BASE_URL set manually)"
fi
if [ -n "${CLAUDE_ENV_FILE:-}" ]; then
  echo "export BAZELISK_BASE_URL=\"$BAZELISK_GH_BASE\"" >> "$CLAUDE_ENV_FILE"
fi

apt_sudo() {
  # apt-get with sudo when not root (the web container runs as root).
  if [ "$(id -u)" -ne 0 ]; then
    DEBIAN_FRONTEND=noninteractive sudo "$@"
  else
    DEBIAN_FRONTEND=noninteractive "$@"
  fi
}

# --- gfortran (default) --------------------------------------------------
if command -v gfortran >/dev/null 2>&1; then
  log "gfortran already present ($(gfortran -dumpversion)); skipping"
elif command -v apt-get >/dev/null 2>&1; then
  log "installing gfortran (unlocks OpenBLAS/eigen Fortran path; see docs/survey-corpus.md)"
  apt_sudo apt-get update -qq || log "apt-get update failed (continuing)"
  if apt_sudo apt-get install -y --no-install-recommends gfortran; then
    log "gfortran installed: $(gfortran -dumpversion)"
  else
    log "WARNING: gfortran install failed — OpenBLAS/eigen fall back to -DNOFORTRAN=1 -DC_LAPACK=1"
  fi
else
  log "WARNING: no apt-get; cannot install gfortran"
fi

# --- cmake (default) — match the production pin --------------------------
# The base image ships an older system cmake (3.28.x in /usr/bin), but
# production + CI run the Makefile's CMAKE_VERSION (cmake 4.x) via this same
# installer, and the converter's reactive CMAKE_POLICY_VERSION_MINIMUM=3.5
# retry (cmakerun) covers sub-3.5-floor projects under cmake 4. Without this a
# web session would survey on a *different* cmake than production, and modern
# projects (>=3.29 floors — e.g. double-conversion's `3.29...4.0.1`) fatal-
# error at configure before the converter even runs. PREFIX=/usr/local lands
# the pin in /usr/local/bin, shadowing /usr/bin. Idempotent (the installer
# stamps + version-checks, so re-runs are a no-op).
if [ -x "$CLAUDE_PROJECT_DIR/tools/install-pinned-cmake.sh" ]; then
  log "provisioning pinned cmake (Makefile CMAKE_VERSION; matches production/CI)"
  if PREFIX=/usr/local "$CLAUDE_PROJECT_DIR/tools/install-pinned-cmake.sh" >&2; then
    hash -r 2>/dev/null || true
    log "cmake provisioned: $(cmake --version 2>/dev/null | head -1)"
  else
    log "WARNING: pinned cmake install failed — staying on system $(cmake --version 2>/dev/null | head -1); >=3.29-floor projects (double-conversion) will fail configure"
  fi
else
  log "WARNING: tools/install-pinned-cmake.sh not found; web session stays on the older base-image cmake"
fi

# --- bazelisk (default) --------------------------------------------------
# The repo's bazel-tagged gates + the gazelle/multiplatform harnesses
# expect bazelisk (reads .bazelversion, matches CI). Reuse the in-repo
# pinned installer; with BAZELISK_BASE_URL set above it fetches bazel from
# GitHub releases.
if command -v bazelisk >/dev/null 2>&1; then
  log "bazelisk already present; skipping"
elif [ -x "$CLAUDE_PROJECT_DIR/tools/install-bazelisk.sh" ]; then
  log "installing bazelisk (repo-pinned; GitHub-releases base URL)"
  if PREFIX=/usr/local "$CLAUDE_PROJECT_DIR/tools/install-bazelisk.sh" >&2; then
    # Pre-fetch the pinned bazel so the first gate run isn't paying the
    # download. .bazelversion lives at the repo root.
    if BAZELISK_BASE_URL="$BAZELISK_GH_BASE" bazelisk --version >&2 2>/dev/null; then
      log "bazelisk installed + bazel prefetched: $(BAZELISK_BASE_URL="$BAZELISK_GH_BASE" bazelisk --version 2>/dev/null | head -1)"
    else
      log "bazelisk installed (bazel prefetch deferred to first use)"
    fi
  else
    log "WARNING: bazelisk install failed — gates fall back to the base 'bazel'"
  fi
else
  log "WARNING: tools/install-bazelisk.sh not found; skipping bazelisk"
fi

# --- bazel egress: BCR via GitHub mirror + JVM truststore (default) -------
# bzlmod can't reach the default registry out of the box in this sandbox:
#   - the egress proxy 403s bcr.bazel.build (and every *.bazel.build host),
#     while github.com is allowlisted — and BCR is mirrored on GitHub; and
#   - the proxy TLS-intercepts all HTTPS with an Anthropic egress CA that
#     bazel's bundled JVM doesn't trust, so its downloader PKIX-fails on
#     every fetch (curl/Go are fine — they use the system CA bundle).
# Both are fixed in ~/.bazelrc (home rc applies to every `bazel`, including
# the gates' staged workspaces and their --noworkspace_rc runs): point the
# registry at the GitHub BCR mirror and hand bazel's JVM a truststore that
# trusts the egress CA. Idempotent (managed block). No-op on
# a host without the egress CAs — i.e. one that can reach bcr.bazel.build
# directly — so this never degrades a normal environment.
egress_cas=$(ls /usr/local/share/ca-certificates/egress-gateway-ca-*.crt \
                /usr/local/share/ca-certificates/swp-ca-*.crt 2>/dev/null || true)
if [ -n "$egress_cas" ]; then
  # Truststore selection, most-trustworthy first. bsb_trust_type/bsb_trust_pass
  # carry the store TYPE + password (a PKCS12 store needs the type passed).
  bsb_trust_type=""
  bsb_trust_pass="changeit"
  # (0) The egress proxy's OWN truststore, as it authoritatively declares it in
  # JAVA_TOOL_OPTIONS — the proxy builds a store with the egress CA already
  # imported and points every JVM tool at it via that env var. But bazel IGNORES
  # JAVA_TOOL_OPTIONS, so we re-pass the SAME store/password/type via
  # --host_jvm_args. Prefer it: it is GUARANTEED to trust the egress CA, whereas
  # the system store below only does if ca-certificates-java ran AFTER the egress
  # CA was installed — when it didn't (e.g. a stale base image), bazel PKIX-fails
  # on every github.com fetch, a "certificate_unknown" download error that
  # masquerades as a flake. Derived from the env (no hardcoded path), so it
  # tracks whatever the proxy set and is a clean no-op where JAVA_TOOL_OPTIONS
  # carries no trustStore.
  bsb_jto_ts=$(printf '%s\n' ${JAVA_TOOL_OPTIONS:-} | sed -n 's/^-Djavax\.net\.ssl\.trustStore=\(..*\)$/\1/p' | head -1)
  if [ -n "$bsb_jto_ts" ] && [ -r "$bsb_jto_ts" ]; then
    bsb_trust="$bsb_jto_ts"
    bsb_trust_type=$(printf '%s\n' ${JAVA_TOOL_OPTIONS:-} | sed -n 's/^-Djavax\.net\.ssl\.trustStoreType=\(..*\)$/\1/p' | head -1)
    bsb_jto_pass=$(printf '%s\n' ${JAVA_TOOL_OPTIONS:-} | sed -n 's/^-Djavax\.net\.ssl\.trustStorePassword=\(..*\)$/\1/p' | head -1)
    [ -n "$bsb_jto_pass" ] && bsb_trust_pass="$bsb_jto_pass"
  # (1) The system Java store. Debian's ca-certificates-java folds the egress CAs
  # into it while keeping the public roots, and it's the same store
  # scripts/run-fidelity.sh already points bazel at. Fall back to a minimal store
  # built from the egress CA files via keytool.
  elif [ -r /etc/ssl/certs/java/cacerts ]; then
    bsb_trust="/etc/ssl/certs/java/cacerts"
  elif command -v keytool >/dev/null 2>&1; then
    # Build into a temp keystore (same dir as the target, so the publish is an
    # atomic rename) and mv into place only after verifying it has cert
    # entries. So an interrupted/failed build never leaves a partial keystore
    # at the published path, and we don't delete a pre-existing file up front
    # for a build that might fail.
    bsb_trust="$HOME/.bazel-egress-trust.jks"
    bsb_tmp="$(mktemp "$HOME/.bsb-trust.XXXXXX" 2>/dev/null || true)"
    if [ -n "$bsb_tmp" ]; then
      rm -f "$bsb_tmp"  # keytool creates the keystore fresh at this name
      for ca in $egress_cas; do
        keytool -importcert -noprompt -trustcacerts -alias "bsb-$(basename "$ca")" \
          -file "$ca" -keystore "$bsb_tmp" -storepass changeit >/dev/null 2>&1 || true
      done
      # Publish only a keystore that actually has cert entries; otherwise drop
      # it and clear bsb_trust so the "no usable truststore" path logs below.
      if keytool -list -keystore "$bsb_tmp" -storepass changeit 2>/dev/null | grep -q trustedCertEntry; then
        mv -f "$bsb_tmp" "$bsb_trust" 2>/dev/null || { rm -f "$bsb_tmp"; bsb_trust=""; }
      else
        rm -f "$bsb_tmp"; bsb_trust=""
      fi
    else
      bsb_trust=""
    fi
  else
    bsb_trust=""
  fi
  if [ -n "$bsb_trust" ]; then
    bsb_rc="$HOME/.bazelrc"
    # Mirror rewrite for the egress sandbox. The egress policy lets BCR resolve
    # (raw.githubusercontent.com) but 403s the github RELEASE / codeload archives
    # the modules point at (rules_cc, abseil, …), so a bare `bazel build` fails to
    # fetch even with the registry+truststore set. The GCS `bazel-mirror` bucket
    # is allowed (200) and serves the SAME archives byte-for-byte (so bazel's
    # integrity hashes still verify), so a downloader-config `rewrite` retargets
    # every github/codeload download at the mirror. This is egress-ONLY (hence
    # inside the $bsb_trust guard): CI reaches github directly and must not be
    # rewritten. Written to a stable cache path; a write failure just omits the
    # downloader line below (egress registry+truststore still apply).
    bsb_mirror_cfg="${BSB_BAZEL_MIRROR_CFG:-$HOME/.cache/bazel-mirror-downloader.cfg}"
    mkdir -p "$(dirname "$bsb_mirror_cfg")" 2>/dev/null || true
    # Write atomically (temp in the same dir + mv), same idiom as the ~/.bazelrc
    # update below: an interrupted or disk-full write must never leave a partial
    # config at the published path, which would break every later bazel download.
    # ^-anchored so the github.com rule can't also match inside codeload.github.com
    # (which has its own rule). $1 is the rewrite backreference (literal here).
    bsb_mirror_tmp="$(mktemp "$(dirname "$bsb_mirror_cfg")/.mirror.XXXXXX" 2>/dev/null || true)"
    if [ -n "$bsb_mirror_tmp" ] \
       && { printf 'rewrite ^github.com/(.*) storage.googleapis.com/bazel-mirror/github.com/$1\n'
            printf 'rewrite ^codeload.github.com/(.*) storage.googleapis.com/bazel-mirror/codeload.github.com/$1\n'
          } > "$bsb_mirror_tmp" 2>/dev/null \
       && mv -f "$bsb_mirror_tmp" "$bsb_mirror_cfg" 2>/dev/null; then
      :
    else
      rm -f "$bsb_mirror_tmp" 2>/dev/null || true
      # Write failed (read-only HOME, disk full). If a previously published config
      # is still readable at the stable path, keep using it — don't drop the
      # rewrite on a transient failure when bazel could still read the old file.
      [ -r "$bsb_mirror_cfg" ] || bsb_mirror_cfg=""
    fi
    # Update ~/.bazelrc atomically. Build the new contents in a temp file in
    # the same dir (so publishing is an atomic rename): the current rc minus
    # any prior managed block, a trailing newline so the marker starts on its
    # own line, then a fresh managed block — and mv into place only after the
    # append succeeds. A failure (read-only HOME, full disk) thus leaves the
    # previous working ~/.bazelrc intact rather than a half-written one, and
    # never aborts the rest of the hook under `set -e`.
    bsb_rc_tmp="$(mktemp "$HOME/.bazelrc.XXXXXX" 2>/dev/null || true)"
    bsb_rc_ok=0
    if [ -n "$bsb_rc_tmp" ]; then
      bsb_rc_ok=1
      if [ -f "$bsb_rc" ]; then
        sed '/# >>> bsb-egress >>>/,/# <<< bsb-egress <<</d' "$bsb_rc" > "$bsb_rc_tmp" 2>/dev/null || bsb_rc_ok=0
      fi
      if [ "$bsb_rc_ok" = 1 ]; then
        { [ -s "$bsb_rc_tmp" ] && [ -n "$(tail -c1 "$bsb_rc_tmp" 2>/dev/null)" ] && printf '\n' >> "$bsb_rc_tmp"; } || true
        cat >> "$bsb_rc_tmp" <<RC && mv -f "$bsb_rc_tmp" "$bsb_rc" || bsb_rc_ok=0
# >>> bsb-egress >>>
common --registry=https://raw.githubusercontent.com/bazelbuild/bazel-central-registry/main
startup --host_jvm_args=-Djavax.net.ssl.trustStore=$bsb_trust --host_jvm_args=-Djavax.net.ssl.trustStorePassword=$bsb_trust_pass${bsb_trust_type:+ --host_jvm_args=-Djavax.net.ssl.trustStoreType=$bsb_trust_type}
${bsb_mirror_cfg:+common --experimental_downloader_config=$bsb_mirror_cfg}
# <<< bsb-egress <<<
RC
      fi
    fi
    if [ "$bsb_rc_ok" = 1 ]; then
      if [ -n "$bsb_mirror_cfg" ]; then
        log "bazel egress configured: BCR (GitHub registry mirror) + GitHub→GCS download rewrite + JVM truststore ($bsb_trust)"
      else
        # No download rewrite (mirror-config write failed) — say so plainly rather
        # than imply the rewrite is active. Registry + truststore still apply; a
        # bare github/codeload archive fetch will 403 until the rewrite is back.
        log "bazel egress configured: BCR (GitHub registry mirror) + JVM truststore ($bsb_trust); download mirror rewrite NOT set (cfg write failed)"
      fi
    else
      rm -f "$bsb_rc_tmp" 2>/dev/null || true
      log "bazel egress: could not update $bsb_rc; left it unchanged"
    fi
  else
    log "bazel egress: egress CAs present but no usable truststore (no system cacerts, no keytool)"
  fi
else
  log "bazel egress: egress CAs absent; leaving bazel at defaults (direct bcr.bazel.build assumed)"
fi

# --- bazel disk cache (gate build reuse) ---------------------------------
# Every gate stages bazel in a FRESH mktemp workspace, so the per-workspace
# output_base (under --output_user_root) is unique per run: the analysis +
# action cache never carry over and each run recompiles heavy deps from
# scratch — e.g. a `cc_proto_library` gate rebuilds protobuf's protoc (~780
# actions, 3-4 min) on every invocation. A --disk_cache is content-addressed
# and workspace-path-independent, so it IS shared across those staged
# workspaces (and across re-runs): the first gate to need protoc compiles it,
# every later gate gets disk-cache hits and finishes in seconds. Its own
# managed block, written UNCONDITIONALLY (unlike the egress block it doesn't
# depend on the egress CA) so the home rc applies it to every `bazel` — gates
# (including their --noworkspace_rc runs) and survey alike. Override the
# location with BSB_BAZEL_DISK_CACHE.
#
# Tradeoff — great for gates, OPT OUT for large surveys: a disk cache stores
# action OUTPUTS, so it ~doubles peak disk (artifacts live in both bazel-out and
# the cache). For the gates that's a rounding error and the cross-workspace reuse
# is the whole point. But a survey marathon builds //... for HUNDREDS of members;
# there the doubling is real, so scripts/run-survey.sh explicitly disables it
# (an empty `--disk_cache=` on its build line overrides this common; re-enable
# with SURVEY_BAZEL_DISK_CACHE=<dir>). New large-build survey paths should opt
# out the same way rather than inherit this gate-tuned default.
bsb_disk_cache="${BSB_BAZEL_DISK_CACHE:-$HOME/.cache/bazel-disk}"
mkdir -p "$bsb_disk_cache" 2>/dev/null || true
# The repository cache is content-addressed (keyed on each download's declared
# hash) and, like the disk cache, workspace-path-independent — so a module archive
# fetched (through the egress mirror above) by one staged gate workspace is reused
# by every later one instead of re-downloaded. Unlike the disk cache it stores only
# the small SOURCE archives (not action outputs), so it carries no survey
# disk-doubling penalty and stays UNCONDITIONAL. Override with BSB_BAZEL_REPO_CACHE.
bsb_repo_cache="${BSB_BAZEL_REPO_CACHE:-$HOME/.cache/bazel-repos}"
mkdir -p "$bsb_repo_cache" 2>/dev/null || true
bsb_cache_rc="$HOME/.bazelrc"
# Same atomic update idiom as the egress block: rebuild in a temp file (drop
# any prior managed block, ensure a trailing newline, append a fresh block)
# and mv into place only on success, so a failure leaves the working rc intact.
bsb_cache_rc_tmp="$(mktemp "$HOME/.bazelrc.XXXXXX" 2>/dev/null || true)"
bsb_cache_rc_ok=0
if [ -n "$bsb_cache_rc_tmp" ]; then
  bsb_cache_rc_ok=1
  if [ -f "$bsb_cache_rc" ]; then
    sed '/# >>> bsb-cache >>>/,/# <<< bsb-cache <<</d' "$bsb_cache_rc" > "$bsb_cache_rc_tmp" 2>/dev/null || bsb_cache_rc_ok=0
  fi
  if [ "$bsb_cache_rc_ok" = 1 ]; then
    { [ -s "$bsb_cache_rc_tmp" ] && [ -n "$(tail -c1 "$bsb_cache_rc_tmp" 2>/dev/null)" ] && printf '\n' >> "$bsb_cache_rc_tmp"; } || true
    cat >> "$bsb_cache_rc_tmp" <<RC && mv -f "$bsb_cache_rc_tmp" "$bsb_cache_rc" || bsb_cache_rc_ok=0
# >>> bsb-cache >>>
common --disk_cache=$bsb_disk_cache
common --repository_cache=$bsb_repo_cache
# <<< bsb-cache <<<
RC
  fi
fi
if [ "$bsb_cache_rc_ok" = 1 ]; then
  log "bazel caches configured: disk_cache=$bsb_disk_cache repository_cache=$bsb_repo_cache (shared across gate workspaces)"
else
  rm -f "$bsb_cache_rc_tmp" 2>/dev/null || true
  log "bazel caches: could not update $bsb_cache_rc; left it unchanged"
fi

# --- buildifier (default) ------------------------------------------------
if command -v buildifier >/dev/null 2>&1; then
  log "buildifier already present; skipping"
elif command -v go >/dev/null 2>&1; then
  log "installing buildifier (BUILD canonical-form check) via go install"
  if go install github.com/bazelbuild/buildtools/buildifier@latest >&2; then
    gobin="$(go env GOBIN)"
    [ -n "$gobin" ] || gobin="$(go env GOPATH)/bin"
    log "buildifier installed to $gobin"
    if [ -n "${CLAUDE_ENV_FILE:-}" ] && [ -n "$gobin" ]; then
      echo "export PATH=\"$gobin:\$PATH\"" >> "$CLAUDE_ENV_FILE"
    fi
  else
    log "WARNING: buildifier install failed — gates fall back to their per-run go install"
  fi
else
  log "WARNING: no go; cannot install buildifier"
fi

# --- find_package host-installs (survey deps) -----------------------------
# Abseil installs by default (cheap, idempotent — protobuf's lens needs it);
# protobuf + re2 (grpc's deeper graph) ride the BSB_PROVISION_GRPC_DEPS=1
# opt-in inside the script (heavy protobuf build). This makes the
# protobuf/grpc lens runs reproducible with no manual prep step.
if [ -f "$CLAUDE_PROJECT_DIR/tools/install-survey-deps.sh" ]; then
  sh "$CLAUDE_PROJECT_DIR/tools/install-survey-deps.sh" || log "WARNING: install-survey-deps.sh reported failures (see above)"
else
  log "WARNING: tools/install-survey-deps.sh missing; protobuf/grpc lens runs need manual prep"
fi

# --- CUDA toolkit (opt-in) -----------------------------------------------
if [ "${BSB_PROVISION_CUDA:-}" = "1" ]; then
  # Step 1 — ensure nvcc AND the gcc-12 host compiler. cutlass CONFIGURE needs
  # only nvcc, but cuda-samples' `.cu` COMPILE needs gcc-12 (nvcc 12.0 caps the
  # host compiler at 12; the base image's default gcc is newer → "unsupported GNU
  # version"). Install if EITHER is missing, so a partially provisioned host still
  # gets gcc-12. _cuda_ready gates the root assembly in step 2.
  _cuda_ready=0
  if command -v nvcc >/dev/null 2>&1 && command -v gcc-12 >/dev/null 2>&1; then
    log "CUDA toolkit + gcc-12 already present"
    _cuda_ready=1
  elif command -v apt-get >/dev/null 2>&1; then
    log "BSB_PROVISION_CUDA=1: installing CUDA toolkit (multi-GB) + gcc-12 for cutlass/cuda-samples"
    apt_sudo apt-get update -qq || true
    if apt_sudo apt-get install -y --no-install-recommends nvidia-cuda-toolkit gcc-12 g++-12; then
      # `|| true` on the pipeline: under `set -euo pipefail` a `grep` that finds no
      # match exits 1 and pipefail would propagate it out of the command
      # substitution — aborting the hook even though CUDA installed fine.
      log "CUDA toolkit installed: $(nvcc --version 2>/dev/null | grep -oE 'release [0-9.]+' | head -n 1 || true) (+ gcc-12 host compiler)"
      _cuda_ready=1
    else
      log "WARNING: CUDA toolkit install failed — cutlass/cuda-samples stop at cmake configure"
    fi
  else
    log "WARNING: no apt-get; cannot install CUDA toolkit"
  fi
  # Step 2 — assemble the single CUDA root rules_cuda's local toolchain wants
  # (Debian scatters it) and export BSB_CUDA_ROOT + BSB_CUDA_HOST_CC into the
  # session env, so cuda-samples.conf picks them up with no manual step. Runs
  # whether CUDA was just installed OR was ALREADY present (idempotent) — a
  # pre-provisioned host still needs the env exported. `|| true` on the pipeline:
  # root assembly is best-effort and must not abort the hook under pipefail (the
  # empty-`_cuda_root` note-path handles a failure). `tail -n 1` for portability.
  if [ "$_cuda_ready" = 1 ] && [ -x "$CLAUDE_PROJECT_DIR/scripts/provision-cuda-root.sh" ]; then
    _cuda_root="$(sh "$CLAUDE_PROJECT_DIR/scripts/provision-cuda-root.sh" 2>/dev/null | tail -n 1 || true)"
    if [ -n "$_cuda_root" ] && [ -d "$_cuda_root" ] && [ -n "${CLAUDE_ENV_FILE:-}" ]; then
      # %q: shell-quote so a root with whitespace/metacharacters can't break (or
      # inject into) the sourced env file. The hook is bash, so %q works.
      printf 'export BSB_CUDA_ROOT=%q\nexport BSB_CUDA_HOST_CC=/usr/bin/gcc-12\n' "$_cuda_root" >> "$CLAUDE_ENV_FILE"
      log "CUDA root assembled at $_cuda_root (BSB_CUDA_ROOT + BSB_CUDA_HOST_CC exported for the cuda-samples lens)"
    else
      log "note: CUDA root assembly incomplete — cuda-samples build needs BSB_CUDA_ROOT set manually (see scripts/provision-cuda-root.sh)"
    fi
  fi
else
  log "CUDA toolkit not requested (set BSB_PROVISION_CUDA=1 for cutlass/cuda-samples)"
fi

# --- gazelle_cc toolchain warm (opt-in) ----------------------------------
# `make survey-gazelle` builds gazelle_cc per project via `bazel run
# //:gazelle` (gazelle_cc 0.5.0 BCR module + transitive Go SDK), ~2min
# cold. Pre-warm the persistent survey cache so the first run is fast.
# Opt-in: needs BCR egress + ~2min. go_sdk.host() dodges the (blocked)
# go.dev SDK download.
if [ "${BSB_WARM_GAZELLE:-}" = "1" ]; then
  if ! command -v bazel >/dev/null 2>&1 && ! command -v bazelisk >/dev/null 2>&1; then
    log "BSB_WARM_GAZELLE=1 but no bazel/bazelisk; skipping gazelle_cc warm"
  else
    bzl="bazel"; command -v bazel >/dev/null 2>&1 || bzl="bazelisk"
    log "BSB_WARM_GAZELLE=1: warming gazelle_cc into the survey cache (~2min cold)"
    warm_cache="${SURVEY_GAZELLE_BZL_CACHE:-$HOME/.cache/survey-gazelle-bazel}"
    warm_dir="$(mktemp -d)"
    cat > "$warm_dir/MODULE.bazel" <<'MOD'
module(name = "gazelle_cc_warm", version = "0.0.0")
bazel_dep(name = "rules_cc", version = "0.0.17")
bazel_dep(name = "rules_pkg", version = "1.0.1")
bazel_dep(name = "rules_go", version = "0.59.0")
bazel_dep(name = "gazelle", version = "0.46.0")
bazel_dep(name = "gazelle_cc", version = "0.5.0")
go_sdk = use_extension("@rules_go//go:extensions.bzl", "go_sdk")
go_sdk.host()
MOD
    cat > "$warm_dir/BUILD.bazel" <<'BLD'
load("@gazelle//:def.bzl", "gazelle", "gazelle_binary")

gazelle_binary(
    name = "gazelle_cc_bin",
    languages = ["@gazelle_cc//language/cc"],
)

gazelle(
    name = "gazelle",
    gazelle = ":gazelle_cc_bin",
)
BLD
    if ( cd "$warm_dir" && BAZELISK_BASE_URL="$BAZELISK_GH_BASE" "$bzl" \
           --output_user_root="$warm_cache" --noworkspace_rc \
           build //:gazelle --lockfile_mode=off ) >&2; then
      log "gazelle_cc toolchain warmed into $warm_cache"
    else
      log "WARNING: gazelle_cc warm failed — survey-gazelle will build it on first run"
    fi
    rm -rf "$warm_dir"
  fi
else
  log "gazelle_cc warm skipped (set BSB_WARM_GAZELLE=1 to pre-build it into the survey cache)"
fi

log "provisioning done"
