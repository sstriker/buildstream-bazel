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
