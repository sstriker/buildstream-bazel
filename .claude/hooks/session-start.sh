#!/bin/bash
# SessionStart hook — provision the toolchains the survey corpus + gates
# need that the base Claude-Code-on-the-web container doesn't ship. The
# base image already has go / cmake / ninja / bazel / git.
#
# Provisions:
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

# --- CUDA toolkit (opt-in) -----------------------------------------------
if [ "${BSB_PROVISION_CUDA:-}" = "1" ]; then
  if command -v nvcc >/dev/null 2>&1; then
    log "CUDA toolkit already present; skipping"
  elif command -v apt-get >/dev/null 2>&1; then
    log "BSB_PROVISION_CUDA=1: installing CUDA toolkit (multi-GB) for cutlass/cuda-samples"
    apt_sudo apt-get update -qq || true
    if apt_sudo apt-get install -y --no-install-recommends nvidia-cuda-toolkit; then
      log "CUDA toolkit installed: $(nvcc --version | grep -oE 'release [0-9.]+' | head -1)"
    else
      log "WARNING: CUDA toolkit install failed — cutlass/cuda-samples stop at cmake configure"
    fi
  else
    log "WARNING: no apt-get; cannot install CUDA toolkit"
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
