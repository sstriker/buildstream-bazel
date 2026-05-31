# Custom bb-runner-bare image with cmake / ninja pre-installed at the
# versions the orchestrator's defaultPlatform asserts. Without these
# tools on the runner, REAPI Action submissions of convert-element fail
# at exec time even though the protocol round-trip succeeds.
#
# Strategy: multi-stage. The upstream bb-runner-bare image is typically
# distroless (no apt-get, no shell). We extract the bb-runner-bare binary
# from it and install it onto a Debian base where we can layer the
# CMake-toolchain dependencies via apt.
#
# Versions MUST match deploy/buildbarn/config/worker.jsonnet's platform
# properties and the Makefile's CMAKE_VERSION / NINJA_VERSION pins.
# Bumping any pin requires updating all three.
#
# Build:
#   docker build -t cmake-to-bazel/bb-runner:local deploy/buildbarn/runner
#
# Or via docker compose (the bb-runner-bare service references this via
# its build: stanza).

# Stage 1: pull the upstream binary out of the pinned image.
FROM ghcr.io/buildbarn/bb-runner-bare:20251031T081715Z-4415657 AS upstream

# Stage 2: Debian base with the toolchain tools installed.
FROM debian:bookworm-slim AS runtime

ARG CMAKE_VERSION=4.3.3
ARG NINJA_VERSION=1.11.1

# bookworm ships:
#   cmake          3.25.1   (need 4.3.3 — download from Kitware GitHub releases)
#   ninja-build    1.11.1   (matches NINJA_VERSION pin exactly)
#   gcc            12.2.0   (whatever bookworm ships; convert-element only
#                            needs *a* C compiler reachable from cmake's
#                            project() probe — exact version is the
#                            converter's responsibility, not ours)
#
# build-essential pulls gcc + g++ + make + libc6-dev. We only need the C
# compiler for cmake's enable_language(C) probe, so pull `gcc` directly
# (smaller layer); add libc6-dev so #include <stdio.h> works in test
# fixtures that exercise compilation.
#
# For cmake we used to use Kitware's apt repo, but apt.kitware.com only
# carries one (rolling) cmake version, so once they bumped past 3.28.x
# our version-fence started failing. Switch to a direct tarball download
# from github.com/Kitware/CMake/releases — those are versioned forever.
RUN set -eux; \
    apt-get update; \
    apt-get install -y --no-install-recommends \
        ca-certificates curl \
        gcc libc6-dev \
        ninja-build \
        make \
    ; \
    arch="$(uname -m)"; \
    curl -fsSL --retry 3 \
      "https://github.com/Kitware/CMake/releases/download/v${CMAKE_VERSION}/cmake-${CMAKE_VERSION}-linux-${arch}.tar.gz" \
      -o /tmp/cmake.tgz; \
    tar -xzf /tmp/cmake.tgz -C /opt; \
    mv "/opt/cmake-${CMAKE_VERSION}-linux-${arch}" /opt/cmake; \
    rm /tmp/cmake.tgz; \
    ln -s /opt/cmake/bin/cmake /usr/local/bin/cmake; \
    ln -s /opt/cmake/bin/ctest /usr/local/bin/ctest; \
    ln -s /opt/cmake/bin/cpack /usr/local/bin/cpack; \
    apt-get purge -y --auto-remove curl; \
    rm -rf /var/lib/apt/lists/*; \
    # Hard version-fence: if the installed versions drift from
    # defaultPlatform pins, the build fails here instead of silently
    # making the worker incompatible with the orchestrator at exec time.
    cmake --version | head -n 1 | grep -F "${CMAKE_VERSION}" || (echo "cmake version mismatch (want ${CMAKE_VERSION})" >&2 ; exit 1); \
    ninja --version | grep -F "${NINJA_VERSION}" || (echo "ninja version mismatch (want ${NINJA_VERSION})" >&2 ; exit 1)

# bb-runner-bare's binary from the upstream layer. Buildbarn images are
# built via @com_github_buildbarn_bb_storage//tools:container.bzl's
# multiarch_go_image macro, which installs the binary at /app/<name>.
# The upstream image's source target is `bb_runner` (re-tagged as
# bb-runner-bare), hence /app/bb_runner here.
COPY --from=upstream /app/bb_runner /app/bb_runner

ENTRYPOINT ["/app/bb_runner"]
