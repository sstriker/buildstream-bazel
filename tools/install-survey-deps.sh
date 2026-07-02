#!/bin/sh
# install-survey-deps.sh — host-install the find_package(...) dependencies the
# build-lens needs so `find_package(<Pkg> CONFIG)` SUCCEEDS at convert time.
#
# WHY: members like protobuf (and, ahead, grpc) call `find_package(absl)`.
# When the package isn't installed cmake either errors or the project silently
# FetchContent-downloads it into the build dir — sources the lens overlay can't
# stage, so the converted graph can't build. Installing the dep to a known
# prefix and pointing CMAKE_PREFIX_PATH there (the .conf files do) makes
# find_package resolve against a real install; the imports-manifest then maps
# the imported targets onto hermetic @bcr labels (see scripts/build-lens/).
#
# Idempotent: each install is skipped when its sentinel artifact already
# exists, so re-running (every SessionStart) is cheap. Sources come from the
# Makefile's fetch-* targets (ABSEIL_DIR=/tmp/abseil-cpp, …); this script
# fetches them if missing.
#
# Reused by .claude/hooks/session-start.sh (provisioning) and runnable by hand.
set -eu

repo_root=$(cd "$(dirname "$0")/.." && pwd)
log() { printf '[install-survey-deps] %s\n' "$*" >&2; }

ABSEIL_DIR=${ABSEIL_DIR:-/tmp/abseil-cpp}
ABSL_INSTALL=${ABSL_INSTALL:-/tmp/absl-install}

install_abseil() {
  if [ -f "$ABSL_INSTALL/lib/libabsl_base.a" ]; then
    log "abseil already installed at $ABSL_INSTALL (skip)"
    return 0
  fi
  if [ ! -d "$ABSEIL_DIR" ]; then
    log "fetching abseil source"
    ( cd "$repo_root" && make fetch-abseil ) >&2 || { log "WARNING: fetch-abseil failed"; return 1; }
  fi
  command -v cmake >/dev/null 2>&1 || { log "WARNING: no cmake; cannot install abseil"; return 1; }
  log "installing abseil → $ABSL_INSTALL (PIC, C++17 — matches the lens convert)"
  if cmake -S "$ABSEIL_DIR" -B /tmp/absl-build -G Ninja \
        -DCMAKE_INSTALL_PREFIX="$ABSL_INSTALL" -DABSL_ENABLE_INSTALL=ON \
        -DCMAKE_POSITION_INDEPENDENT_CODE=ON -DCMAKE_CXX_STANDARD=17 \
        -DCMAKE_BUILD_TYPE=Release >&2 \
     && cmake --build /tmp/absl-build --target install >&2; then
    log "abseil installed: $(ls "$ABSL_INSTALL/lib" | grep -c '\.a$') static libs"
  else
    log "WARNING: abseil install failed — protobuf/grpc lens runs will not build"
    return 1
  fi
}

install_abseil || true

# Catch2: spdlog's test suite does find_package(Catch2 3 QUIET) with a
# FetchContent network fallback the lens overlay can't stage — so the
# host install is what lets SPDLOG_BUILD_TESTS=ON convert hermetically
# (spdlog.conf points CMAKE_PREFIX_PATH here; spdlog-imports.json maps
# the imported targets onto @catch2 BCR labels). Default-on like
# abseil: a small build, and the spdlog lens needs it every run.
CATCH2_DIR=${CATCH2_DIR:-/tmp/Catch2}
CATCH2_INSTALL=${CATCH2_INSTALL:-/tmp/catch2-install}

install_catch2() {
  if [ -f "$CATCH2_INSTALL/lib/libCatch2.a" ]; then
    log "catch2 already installed at $CATCH2_INSTALL (skip)"
    return 0
  fi
  if [ ! -d "$CATCH2_DIR" ]; then
    log "fetching catch2 source"
    ( cd "$repo_root" && make fetch-catch2 ) >&2 || { log "WARNING: fetch-catch2 failed"; return 1; }
  fi
  command -v cmake >/dev/null 2>&1 || { log "WARNING: no cmake; cannot install catch2"; return 1; }
  log "installing catch2 → $CATCH2_INSTALL (PIC, C++17 — matches the lens convert)"
  if cmake -S "$CATCH2_DIR" -B /tmp/catch2-build -G Ninja \
        -DCMAKE_INSTALL_PREFIX="$CATCH2_INSTALL" -DBUILD_TESTING=OFF \
        -DCMAKE_POSITION_INDEPENDENT_CODE=ON -DCMAKE_CXX_STANDARD=17 \
        -DCMAKE_BUILD_TYPE=Release >&2 \
     && cmake --build /tmp/catch2-build --target install >&2; then
    log "catch2 installed"
  else
    log "WARNING: catch2 install failed — spdlog's test lens run will not convert"
    return 1
  fi
}

install_catch2 || true

# grpc's deeper find_package deps: protobuf + re2, both built AGAINST the
# abseil install above (matching grpc.conf's CMAKE_PREFIX_PATH=/tmp/
# absl-install;/tmp/protobuf-install;/tmp/re2-install). Protobuf is a
# heavy build (~10+ min), so both are gated behind BSB_PROVISION_GRPC_DEPS=1
# — the same opt-in pattern the SessionStart hook uses for CUDA.
PROTOBUF_DIR=${PROTOBUF_DIR:-/tmp/protobuf}
PROTOBUF_INSTALL=${PROTOBUF_INSTALL:-/tmp/protobuf-install}
RE2_DIR=${RE2_DIR:-/tmp/re2}
RE2_INSTALL=${RE2_INSTALL:-/tmp/re2-install}

install_protobuf() {
  if [ -f "$PROTOBUF_INSTALL/lib/libprotobuf.a" ]; then
    log "protobuf already installed at $PROTOBUF_INSTALL (skip)"
    return 0
  fi
  [ -f "$ABSL_INSTALL/lib/libabsl_base.a" ] || { log "WARNING: abseil install missing; protobuf install needs it"; return 1; }
  if [ ! -d "$PROTOBUF_DIR" ]; then
    log "fetching protobuf source"
    ( cd "$repo_root" && make fetch-protobuf ) >&2 || { log "WARNING: fetch-protobuf failed"; return 1; }
  fi
  command -v cmake >/dev/null 2>&1 || { log "WARNING: no cmake; cannot install protobuf"; return 1; }
  log "installing protobuf → $PROTOBUF_INSTALL (against $ABSL_INSTALL; PIC, C++17)"
  if cmake -S "$PROTOBUF_DIR" -B /tmp/protobuf-build -G Ninja \
        -DCMAKE_INSTALL_PREFIX="$PROTOBUF_INSTALL" \
        -Dprotobuf_BUILD_TESTS=OFF -Dprotobuf_ABSL_PROVIDER=package \
        -DCMAKE_PREFIX_PATH="$ABSL_INSTALL" \
        -DCMAKE_POSITION_INDEPENDENT_CODE=ON -DCMAKE_CXX_STANDARD=17 \
        -DCMAKE_BUILD_TYPE=Release >&2 \
     && cmake --build /tmp/protobuf-build --target install >&2; then
    log "protobuf installed"
  else
    log "WARNING: protobuf install failed — the grpc lens will not convert"
    return 1
  fi
}

install_re2() {
  if [ -f "$RE2_INSTALL/lib/libre2.a" ]; then
    log "re2 already installed at $RE2_INSTALL (skip)"
    return 0
  fi
  [ -f "$ABSL_INSTALL/lib/libabsl_base.a" ] || { log "WARNING: abseil install missing; re2 install needs it"; return 1; }
  if [ ! -d "$RE2_DIR" ]; then
    log "fetching re2 source"
    ( cd "$repo_root" && make fetch-re2 ) >&2 || { log "WARNING: fetch-re2 failed"; return 1; }
  fi
  command -v cmake >/dev/null 2>&1 || { log "WARNING: no cmake; cannot install re2"; return 1; }
  log "installing re2 → $RE2_INSTALL (against $ABSL_INSTALL; PIC, C++17)"
  if cmake -S "$RE2_DIR" -B /tmp/re2-build -G Ninja \
        -DCMAKE_INSTALL_PREFIX="$RE2_INSTALL" -DRE2_BUILD_TESTING=OFF \
        -DCMAKE_PREFIX_PATH="$ABSL_INSTALL" \
        -DCMAKE_POSITION_INDEPENDENT_CODE=ON -DCMAKE_CXX_STANDARD=17 \
        -DCMAKE_BUILD_TYPE=Release >&2 \
     && cmake --build /tmp/re2-build --target install >&2; then
    log "re2 installed"
  else
    log "WARNING: re2 install failed — the grpc lens will not convert"
    return 1
  fi
}

# System (apt) deps the grpc + buildbox lenses' cmake CONFIGURE needs, beyond the
# from-source abseil/protobuf/re2 installs above:
#   - grpc  find_package: c-ares, OpenSSL, zlib (gRPC_*_PROVIDER=package) come
#     from /usr; the .conf's CMAKE_PREFIX_PATH lists /usr for them.
#   - buildbox find_program/find_package + pkg_check_modules: the protoc +
#     grpc_cpp_plugin binaries, uuid, and tomlplusplus (a hard CORE dep —
#     BuildboxCommonConfig.cmake's pkg_check_modules(tomlplusplus REQUIRED)
#     fails configure without it).
# Gated behind the same BSB_PROVISION_GRPC_DEPS opt-in (they only matter for the
# grpc/buildbox lens runs). Idempotent: apt-get install is a no-op when present.
install_grpc_buildbox_system_deps() {
  command -v apt-get >/dev/null 2>&1 || { log "WARNING: no apt-get; grpc/buildbox system deps unavailable"; return 1; }
  log "installing grpc/buildbox system deps (c-ares, OpenSSL, zlib, protoc+grpc plugin, uuid, tomlplusplus)"
  _apt='apt-get'
  [ "$(id -u)" -eq 0 ] || _apt='sudo apt-get'
  DEBIAN_FRONTEND=noninteractive $_apt update -qq >&2 2>/dev/null || true
  if DEBIAN_FRONTEND=noninteractive $_apt install -y --no-install-recommends \
       libc-ares-dev libssl-dev zlib1g-dev protobuf-compiler protobuf-compiler-grpc \
       libgrpc++-dev uuid-dev libtomlplusplus-dev >&2; then
    log "grpc/buildbox system deps installed"
  else
    log "WARNING: some grpc/buildbox system deps failed to install (grpc/buildbox convert may fail)"
    return 1
  fi
}

if [ "${BSB_PROVISION_GRPC_DEPS:-}" = "1" ]; then
  install_grpc_buildbox_system_deps || true
  install_protobuf || true
  install_re2 || true
else
  log "grpc deps not requested (set BSB_PROVISION_GRPC_DEPS=1 for protobuf-install/re2-install + apt system deps)"
fi

# SDL: system OpenGL / GLES / EGL dev headers. SDL's cmake auto-enables the
# OpenGL, OpenGL ES (incl. the legacy GLES1 render backend whose
# <GLES/glplatform.h> SDL does NOT vendor — it ships GLES2/GLES3/EGL/KHR under
# src/video/khronos but not GLES1), and EGL backends; their sources #include
# system Khronos headers. X11 dev headers are already present in the base image.
# Installing the -dev packages provides both the headers (compile) and the .so
# stubs (the find_package(OpenGL)/-lGLESv1_CM host link). Idempotent: skip when
# the GLES1 header is already on disk.
install_sdl_gl() {
  if [ -f /usr/include/GLES/glplatform.h ]; then
    log "GLES/GL/EGL dev headers already present (skip)"
    return 0
  fi
  command -v apt-get >/dev/null 2>&1 || { log "WARNING: no apt-get; SDL GL backends won't build"; return 1; }
  log "installing libgles-dev (pulls GL/GLES/EGL/GLX dev headers + stubs) for SDL"
  if apt-get install -y --no-install-recommends libgles-dev >&2 2>/dev/null ||
     sudo apt-get install -y --no-install-recommends libgles-dev >&2 2>/dev/null; then
    log "GL/GLES/EGL dev headers installed"
  else
    log "WARNING: libgles-dev install failed — SDL's GL/GLES backends won't build"
    return 1
  fi
}

install_sdl_gl || true

log "survey-deps provisioning done"
