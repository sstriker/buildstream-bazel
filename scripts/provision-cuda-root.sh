#!/bin/sh
# provision-cuda-root.sh — assemble a self-contained CUDA toolkit ROOT that
# rules_cuda's local-toolchain repo rule can mirror, from Debian's scattered
# `nvidia-cuda-toolkit` packaging.
#
# WHY THIS EXISTS
# ---------------
# cutlass / cuda-samples need `nvcc` two ways:
#   1. to pass `cmake configure` at all (they enable_language(CUDA) /
#      find_package(CUDAToolkit)) — the SessionStart hook's BSB_PROVISION_CUDA=1
#      path installs `nvidia-cuda-toolkit` for this; and
#   2. to actually COMPILE `.cu` device sources via the build lens, which now
#      emits rules_cuda's cuda_library / cuda_binary (see the converter's
#      KindCudaLibrary lowering). rules_cuda's `cuda.toolkit(toolkit_path=...)`
#      extension expects a MONOLITHIC CUDA root (canonical
#      bin/ + include/ + lib64/ + nvvm/ shape, like /usr/local/cuda from the
#      NVIDIA .run installer).
#
# Debian's `nvidia-cuda-toolkit` does NOT present that: `/usr/bin/nvcc` is a
# shell wrapper, the real binaries live under /usr/lib/nvidia-cuda-toolkit/bin,
# headers are in /usr/include, libraries in the multiarch dir
# (/usr/lib/x86_64-linux-gnu). Pointed at `/usr`, rules_cuda's repo rule hits a
# `/usr/bin/X11/X11/...` symlink loop while globbing `cuda/**`. So this script
# builds a clean root of SYMLINKS into the canonical layout.
#
# It also surfaces the gcc constraint: CUDA 12.0's nvcc rejects host gcc > 12.
# Install gcc-12/g++-12 (the hook does) and steer rules_cuda's `-ccbin` at it
# via CC — the build lens passes `--repo_env=CC=$BSB_CUDA_HOST_CC`.
#
# USAGE
#   sh scripts/provision-cuda-root.sh            # root at /opt/cuda-root
#   BSB_CUDA_ROOT=/path sh scripts/provision-cuda-root.sh
# Prints the assembled root path on success (consumers read $BSB_CUDA_ROOT).
# Idempotent: re-running refreshes the symlink farm. Needs nvcc already
# installed (run the hook's BSB_PROVISION_CUDA=1 first, or apt-get install
# nvidia-cuda-toolkit gcc-12 g++-12).
set -eu

root="${BSB_CUDA_ROOT:-/opt/cuda-root}"

if ! command -v nvcc >/dev/null 2>&1; then
  echo "provision-cuda-root: nvcc not on PATH; install the CUDA toolkit first" >&2
  echo "  (SessionStart hook BSB_PROVISION_CUDA=1, or: apt-get install -y nvidia-cuda-toolkit gcc-12 g++-12)" >&2
  exit 1
fi

mkdir -p "$root/bin" "$root/include" "$root/lib64/stubs" "$root/nvvm/libdevice"

# bin/: the REAL toolkit binaries (cicc, crt/, fatbinary, …). nvcc must be the
# real ELF, not the /usr/bin/nvcc shell wrapper — rules_cuda execs it directly
# under the sandbox where the wrapper's `exec /usr/lib/...` indirection and
# PATH assumptions don't hold.
if [ -d /usr/lib/nvidia-cuda-toolkit/bin ]; then
  for f in /usr/lib/nvidia-cuda-toolkit/bin/*; do
    ln -sf "$f" "$root/bin/$(basename "$f")"
  done
fi
for tool in ptxas fatbinary nvlink nvdisasm cuobjdump bin2c; do
  src="$(command -v "$tool" 2>/dev/null || true)"
  [ -n "$src" ] && ln -sf "$(readlink -f "$src")" "$root/bin/$tool" || true
done
[ -e /usr/lib/nvidia-cuda-toolkit/bin/nvcc ] && ln -sf /usr/lib/nvidia-cuda-toolkit/bin/nvcc "$root/bin/nvcc" || true

# include/: CUDA headers (cuda_runtime.h et al.) live in /usr/include on Debian.
for h in /usr/include/cuda*.h /usr/include/device_*.h /usr/include/driver_*.h \
         /usr/include/builtin_types.h /usr/include/sm_*.h /usr/include/host_*.h \
         /usr/include/vector_types.h /usr/include/vector_functions*.h \
         /usr/include/channel_descriptor.h /usr/include/texture_*.h \
         /usr/include/surface_*.h /usr/include/library_types.h \
         /usr/include/math_constants.h /usr/include/common_functions.h \
         /usr/include/cuComplex.h /usr/include/crt /usr/include/nv \
         /usr/include/cooperative_groups /usr/include/cooperative_groups.h; do
  [ -e "$h" ] && ln -sf "$h" "$root/include/$(basename "$h")" || true
done

# lib64/: runtime + the static libs rules_cuda's runtime target links
# (libculibos.a / libcudart_static.a / libcudadevrt.a) from the multiarch dir.
for l in /usr/lib/x86_64-linux-gnu/libcudart* /usr/lib/x86_64-linux-gnu/libcudadevrt* \
         /usr/lib/x86_64-linux-gnu/libculibos.a /usr/lib/x86_64-linux-gnu/libcupti* \
         /usr/lib/x86_64-linux-gnu/libnvToolsExt* /usr/lib/x86_64-linux-gnu/libnvrtc* \
         /usr/lib/x86_64-linux-gnu/libcublas*; do
  [ -e "$l" ] && ln -sf "$l" "$root/lib64/$(basename "$l")" || true
done
[ -e /usr/lib/x86_64-linux-gnu/stubs/libcuda.so ] && ln -sf /usr/lib/x86_64-linux-gnu/stubs/libcuda.so "$root/lib64/stubs/libcuda.so" || true

# nvvm/: libdevice bitcode + the nvvm lib dir (nvcc's device-IR backend).
if [ -d /usr/lib/nvidia-cuda-toolkit/libdevice ]; then
  for d in /usr/lib/nvidia-cuda-toolkit/libdevice/*; do
    [ -e "$d" ] && ln -sf "$d" "$root/nvvm/libdevice/$(basename "$d")" || true
  done
fi
[ -d /usr/lib/cuda/nvvm/lib64 ] && ln -sf /usr/lib/cuda/nvvm/lib64 "$root/nvvm/lib64" || true

if [ ! -e "$root/include/cuda_runtime.h" ] || [ ! -e "$root/bin/nvcc" ]; then
  echo "provision-cuda-root: assembly incomplete under $root (missing nvcc or cuda_runtime.h)" >&2
  exit 1
fi

echo "provision-cuda-root: assembled CUDA root at $root" >&2
echo "  point rules_cuda at it:  cuda.toolkit(toolkit_path = \"$root\")" >&2
[ -x /usr/bin/gcc-12 ] && echo "  nvcc host compiler:      CC=/usr/bin/gcc-12 (nvcc 12.0 caps host gcc at 12)" >&2
printf '%s\n' "$root"
