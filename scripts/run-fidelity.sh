#!/bin/sh
# run-fidelity.sh — drive a full convert-and-rebuild fidelity
# comparison for one fixture.
#
# Steps, all in a fresh scratch tree:
#   1. cmake configure + build the project (the oracle, project C in
#      docs/fidelity-deltas.md's A-B-C harness terminology).
#   2. convert-element-cmake against the same build dir → produce
#      BUILD.bazel.
#   3. Stage a Bazel workspace: the project sources + the converted
#      BUILD.bazel + a bzlmod MODULE.bazel declaring the converter's
#      load() deps (rules_cc for the cc_* rules, rules_pkg for the
#      install pkg_files) as bazel_deps. We keep the converter's real
#      emitted BUILD — no load-stripping — since Bazel 9 removed the
#      native cc rules, so `load("@rules_cc//...")` must resolve.
#      bazel build the same target.
#   4. Run cmd/fidelity-compare against the two artifacts. Report
#      benign / impactful deltas and exit non-zero on any impactful.
#
# Bazel-availability gating: the cmake configure + convert + classifier
# half always runs. The bazel build half self-skips when bazel is not on
# PATH (operators without it see a "skipped (no bazel)" line and exit 0
# on the configure + convert half). The bazel half needs a reachable
# Bazel Central Registry to fetch rules_cc / rules_pkg, the same as the
# repo's meta-* gates.
#
# Usage:
#   scripts/run-fidelity.sh \
#       --project-name <name> \
#       --source-root <abs-path-to-cmake-source-root> \
#       --target <cmake-and-bazel-target-name> \
#       --artifact-pattern <e.g. libfoo.a> \
#       [--allowlist <abs-path>] \
#       [--cmake-flags '-DFOO=BAR ...'] \
#       [--bazel-target-label '//:foo']

set -eu

# CLI parsing.
project_name=""
source_root=""
target=""
artifact_pattern=""
cmake_artifact_pattern=""
bazel_artifact_pattern=""
allowlist=""
cmake_flags=""
convert_flags=""
bazel_external=""
bazel_target_label=""
consumer_file=""
consumer_bazel_dep=""
consumer_cmake_cflags=""

usage() {
    echo "usage: $0 --project-name <name> --source-root <abs> --target <name>" >&2
    echo "          --artifact-pattern <libfoo.a>" >&2
    echo "          [--cmake-artifact-pattern <libfoo.a>] [--bazel-artifact-pattern <libfoo.a>]" >&2
    echo "          [--allowlist <abs>] [--cmake-flags '...']" >&2
    echo "          [--bazel-target-label '//:foo']" >&2
    echo "          [--consumer-file <abs.c|.cpp>] [--consumer-bazel-dep '//:foo']" >&2
    echo "" >&2
    echo "  --artifact-pattern sets a default for both sides; the" >&2
    echo "  per-side --cmake-artifact-pattern / --bazel-artifact-pattern" >&2
    echo "  overrides apply when cmake and bazel emit different names" >&2
    echo "  (e.g. zlib's cmake zlibstatic target emits libz.a; Bazel" >&2
    echo "  emits libzlibstatic.a from the same target)." >&2
    echo "" >&2
    echo "  --consumer-file switches to consumer-side fidelity: the" >&2
    echo "  given .c/.cpp source is compiled twice (once against" >&2
    echo "  cmake's installed prefix, once via Bazel as a cc_library" >&2
    echo "  depending on --consumer-bazel-dep) and the two .o files" >&2
    echo "  are diffed instead of the static libraries. Useful for" >&2
    echo "  header-only / INTERFACE libraries with no static-archive" >&2
    echo "  artifact, and as an extra signal for library projects." >&2
    echo "" >&2
    echo "  --consumer-cmake-cflags replays a target's PUBLIC compile" >&2
    echo "  definitions on the cmake-side consumer compile (the bare" >&2
    echo "  -I<install>/include compile doesn't carry them, but the" >&2
    echo "  Bazel-side consumer inherits them transitively from the" >&2
    echo "  converted target's defines). Without this the two sides" >&2
    echo "  diverge on a mode toggle like spdlog's SPDLOG_COMPILED_LIB" >&2
    echo "  (header-only vs compiled-lib codegen), which is a harness" >&2
    echo "  asymmetry, not a converter delta." >&2
}

while [ $# -gt 0 ]; do
    case "$1" in
        --project-name) project_name="$2"; shift 2 ;;
        --source-root) source_root="$2"; shift 2 ;;
        --target) target="$2"; shift 2 ;;
        --artifact-pattern) artifact_pattern="$2"; shift 2 ;;
        --cmake-artifact-pattern) cmake_artifact_pattern="$2"; shift 2 ;;
        --bazel-artifact-pattern) bazel_artifact_pattern="$2"; shift 2 ;;
        --allowlist) allowlist="$2"; shift 2 ;;
        --cmake-flags) cmake_flags="$2"; shift 2 ;;
        --convert-flags) convert_flags="$2"; shift 2 ;;
        --bazel-external) bazel_external="$2"; shift 2 ;;
        --bazel-target-label) bazel_target_label="$2"; shift 2 ;;
        --consumer-file) consumer_file="$2"; shift 2 ;;
        --consumer-bazel-dep) consumer_bazel_dep="$2"; shift 2 ;;
        --consumer-cmake-cflags) consumer_cmake_cflags="$2"; shift 2 ;;
        -h|--help) usage; exit 0 ;;
        *) echo "unknown arg: $1" >&2; usage; exit 64 ;;
    esac
done

if [ -z "$project_name" ] || [ -z "$source_root" ] || [ -z "$target" ]; then
    echo "missing required arg" >&2
    usage
    exit 64
fi
# Per-side patterns fall back to the shared --artifact-pattern.
if [ -z "$cmake_artifact_pattern" ]; then
    cmake_artifact_pattern="$artifact_pattern"
fi
if [ -z "$bazel_artifact_pattern" ]; then
    bazel_artifact_pattern="$artifact_pattern"
fi
# Header-only / INTERFACE-only projects (nlohmann-json, boost-core
# shape) don't produce a static-archive artifact — the fidelity
# check has to ride on the consumer .o pair only. Allow missing
# --artifact-pattern when --consumer-file is set; the library-
# build + artifact-find blocks self-skip on the empty pattern.
if [ -n "$consumer_file" ] && [ -z "$cmake_artifact_pattern" ] && [ -z "$bazel_artifact_pattern" ]; then
    no_library=true
else
    no_library=false
    if [ -z "$cmake_artifact_pattern" ] || [ -z "$bazel_artifact_pattern" ]; then
        echo "missing --artifact-pattern (or per-side overrides)" >&2
        usage
        exit 64
    fi
fi
if [ -z "$bazel_target_label" ]; then
    bazel_target_label="//:$target"
fi

repo_root="$(cd "$(dirname "$0")/.." && pwd)"
bin_dir="$repo_root/build/bin"
mkdir -p "$bin_dir"

echo "fidelity[$project_name]: building required converters + classifier" >&2
make -C "$repo_root" converter >/dev/null
CGO_ENABLED=0 go build -C "$repo_root" -o "$bin_dir/fidelity-compare" ./cmd/fidelity-compare

work_dir="$(mktemp -d)"
trap "rm -rf '$work_dir'" EXIT
cmake_build="$work_dir/cmake-build"
bazel_ws="$work_dir/bazel-ws"
# Staging prefix for the consumer-mode `cmake --install`. Defined up front
# (not just where consumer mode uses it) because it has to be baked into the
# cmake configure below via -DCMAKE_INSTALL_PREFIX: projects like zlib compute
# their install destinations as ABSOLUTE paths at configure time (e.g.
# INSTALL_LIB_DIR = ${CMAKE_INSTALL_PREFIX}/lib defaulting to /usr/local/lib),
# and a later `cmake --install --prefix` can only redirect *relative*
# destinations — it cannot override a destination already baked absolute. So
# point the prefix at the scratch tree at configure time; the CI runner can't
# write /usr/local. Harmless for library-only fixtures that never install.
install_stage="$work_dir/cmake-install"
mkdir -p "$cmake_build"

# --- Step 1: cmake configure + build (project C, the oracle).
echo "fidelity[$project_name]: cmake configure + build" >&2
# Stage cmake File API query so convert can read codemodel-v2.
mkdir -p "$cmake_build/.cmake/api/v1/query"
for k in codemodel-v2 toolchains-v1 cmakeFiles-v1 cache-v2; do
    touch "$cmake_build/.cmake/api/v1/query/$k"
done
# shellcheck disable=SC2086
# Enable cmake's --trace-expand so convert-element-cmake's
# trace-driven lifts fire (INTERFACE-library cc_library
# synthesis, target_link_libraries STATIC IMPORTED dep
# recovery, PRIVATE/PUBLIC visibility on
# target_include_directories, etc.). The converter auto-
# detects trace.jsonl at <build>/trace.jsonl when present.
cmake -G Ninja -DCMAKE_BUILD_TYPE=Release \
    -DCMAKE_INSTALL_PREFIX="$install_stage" \
    -B "$cmake_build" -S "$source_root" \
    --trace-expand --trace-format=json-v1 \
    --trace-redirect="$cmake_build/trace.jsonl" \
    $cmake_flags > "$work_dir/cmake-configure.log" 2>&1
# For INTERFACE-only (no_library) projects there's nothing to
# build at this step; the install step in 1b is what stages
# headers for the consumer compile. Single --target call still
# runs to surface any configure-side errors.
if [ "$no_library" = false ]; then
    cmake --build "$cmake_build" --target "$target" > "$work_dir/cmake-build.log" 2>&1
fi

cmake_artifact=""
if [ "$no_library" = false ]; then
    cmake_artifact="$(find "$cmake_build" -name "$cmake_artifact_pattern" -type f | head -1)"
    if [ -z "$cmake_artifact" ]; then
        echo "fidelity[$project_name]: cmake build produced no artifact matching $cmake_artifact_pattern" >&2
        exit 1
    fi
    echo "fidelity[$project_name]:   cmake artifact: $cmake_artifact" >&2
fi

# --- Step 1b (consumer mode): cmake install + cmake-side consumer compile.
# The consumer.c/.cpp is compiled against the headers cmake's install
# step lays under <install>/include/ — exactly what a downstream
# package consuming the project via find_package() would see.
consumer_cmake_o=""
if [ -n "$consumer_file" ]; then
    # install_stage is defined up front (and baked into the cmake configure via
    # -DCMAKE_INSTALL_PREFIX) so absolute install destinations land in the
    # scratch tree, not /usr/local. The --prefix below stays as belt-and-
    # suspenders for fixtures whose install destinations are relative.
    # cmake's install target may depend on artifacts the single
    # --target build didn't produce (e.g. zlib's install lists
    # both the static and shared libs, but we only built static).
    # Re-run --build with no --target arg to build everything
    # install touches.
    cmake --build "$cmake_build" > "$work_dir/cmake-build-all.log" 2>&1 || {
        echo "fidelity[$project_name]: cmake-side full build (for install) FAILED — see $work_dir/cmake-build-all.log" >&2
        tail -20 "$work_dir/cmake-build-all.log" >&2
        exit 1
    }
    cmake --install "$cmake_build" --prefix "$install_stage" \
        > "$work_dir/cmake-install.log" 2>&1 || {
            echo "fidelity[$project_name]: cmake install FAILED — see $work_dir/cmake-install.log" >&2
            tail -20 "$work_dir/cmake-install.log" >&2
            exit 1
        }
    case "$consumer_file" in
        *.cpp|*.cc|*.cxx) consumer_cc="g++" ;;
        *) consumer_cc="gcc" ;;
    esac
    consumer_cmake_o="$work_dir/consumer.cmake.o"
    # shellcheck disable=SC2086 — consumer_cmake_cflags is an intentional
    # word-split list of -D defines (replays the converted target's
    # PUBLIC compile definitions the bare -I compile would otherwise miss).
    "$consumer_cc" -O2 -fPIC -c "$consumer_file" \
        -I"$install_stage/include" $consumer_cmake_cflags \
        -o "$consumer_cmake_o" \
        2> "$work_dir/consumer-cmake-compile.log" || {
            echo "fidelity[$project_name]: consumer cmake-side compile FAILED — see $work_dir/consumer-cmake-compile.log" >&2
            cat "$work_dir/consumer-cmake-compile.log" >&2
            exit 1
        }
    echo "fidelity[$project_name]:   consumer cmake-side .o: $consumer_cmake_o" >&2
fi

# --- Step 2: convert.
# --convert-flags passes project-specific converter opt-ins through
# verbatim (e.g. Catch2 needs `--lift-configure-file=true` to recover
# catch_user_config.hpp from its configure_file template).
echo "fidelity[$project_name]: convert-element-cmake" >&2
# shellcheck disable=SC2086 # convert_flags is intentionally word-split.
"$bin_dir/convert-element-cmake" \
    --cmake-build-dir "$cmake_build" \
    --out-build "$bazel_ws/BUILD.bazel" \
    $convert_flags \
    > "$work_dir/convert.log" 2>&1 || {
        echo "fidelity[$project_name]: convert FAILED — see $work_dir/convert.log" >&2
        exit 1
    }

# --- Step 3: stage Bazel workspace + build (project B).
# Copy the project sources into the bazel workspace.
cp -r "$source_root/." "$bazel_ws/"
# Overwrite any pre-existing project BUILD.bazel with the converted one.
# shellcheck disable=SC2086 # convert_flags is intentionally word-split.
"$bin_dir/convert-element-cmake" \
    --cmake-build-dir "$cmake_build" \
    --out-build "$bazel_ws/BUILD.bazel" \
    $convert_flags \
    >> "$work_dir/convert.log" 2>&1
# Stage the cmake-configure-file build-time tool when the converted BUILD
# references it (the --lift-configure-file genrules invoke
# //tools:cmake-configure-file at Bazel build time to materialize the
# configured header from the .h.in template). Auto-detected so no caller
# has to remember to pair the convert flag with the tool — mirrors how
# write-a stages it into project B's tools/.
if grep -q "//tools:cmake-configure-file" "$bazel_ws/BUILD.bazel"; then
    echo "fidelity[$project_name]: staging //tools:cmake-configure-file" >&2
    CGO_ENABLED=0 go build -C "$repo_root" -o "$bin_dir/cmake-configure-file" ./cmd/cmake-configure-file
    mkdir -p "$bazel_ws/tools"
    # The cmake_configure_file rule's `tool` attr is executable=True, so the
    # label has to resolve to an executable TARGET, not a raw source file —
    # a plain exports_files() trips "source file is misplaced here (expected
    # no files)" at analysis. Wrap the binary in bazel_skylib's native_binary,
    # exactly as write-a (project B) and the meta-* gates stage it. (Staged as
    # .bin so native_binary's `out` can reuse the bare tool name.)
    cp "$bin_dir/cmake-configure-file" "$bazel_ws/tools/cmake-configure-file.bin"
    chmod 0755 "$bazel_ws/tools/cmake-configure-file.bin"
    cat > "$bazel_ws/tools/BUILD.bazel" <<'EOF'
load("@bazel_skylib//rules:native_binary.bzl", "native_binary")

native_binary(
    name = "cmake-configure-file",
    src = "cmake-configure-file.bin",
    out = "cmake-configure-file",
    visibility = ["//visibility:public"],
)
EOF
fi
# When the converted BUILD loads from @rules_buildstream_bazel — the converter's
# in-repo ruleset (cmake_configure_file from --lift-configure-file, pick_file
# from the install lift) — provide it via bazel_dep + local_path_override
# pointing at this repo's rules_buildstream_bazel/ subdir. That's exactly how
# write-a (project B) and the meta-* gates wire the ruleset; it's intentionally
# NOT on BCR (see rules_buildstream_bazel/MODULE.bazel), so a bazel_dep alone
# can't resolve it. Auto-detected so callers don't have to remember to pair the
# convert flag with the override.
rules_bsb_module=""
if grep -q "@rules_buildstream_bazel" "$bazel_ws/BUILD.bazel"; then
    echo "fidelity[$project_name]: wiring @rules_buildstream_bazel local_path_override" >&2
    rules_bsb_module="bazel_dep(name = \"rules_buildstream_bazel\", version = \"0.0.0\")
local_path_override(module_name = \"rules_buildstream_bazel\", path = \"$repo_root/rules_buildstream_bazel\")"
fi
# bzlmod MODULE.bazel providing the converter's load() deps as bazel_deps
# from BCR — rules_cc backs the cc_* rules, rules_pkg backs the install
# pkg_files. We keep the converter's *real* output (no load-stripping):
# Bazel 9 removed the native cc rules, so `load("@rules_cc//...")` MUST
# resolve, and testing the actual emitted BUILD is more faithful than a
# rewritten one. Versions track write-a's project B (cmd/write-a/main.go),
# the proven-in-CI reference. Needs a reachable Bazel Central Registry,
# same as the meta-* gates. --bazel-external appends extra bzlmod lines
# (e.g. a `bazel_dep(name = "zlib", …)` for libpng's find_package(ZLIB)).
cat > "$bazel_ws/MODULE.bazel" <<EOF
module(name = "${project_name}_fidelity", version = "0.0.0")
bazel_dep(name = "rules_cc", version = "0.0.17")
bazel_dep(name = "rules_pkg", version = "1.0.1")
bazel_dep(name = "bazel_skylib", version = "1.8.2")
${rules_bsb_module}
${bazel_external}
EOF

if ! command -v bazel >/dev/null 2>&1; then
    echo "fidelity[$project_name]: bazel not on PATH; skipped — cmake + convert half ran clean" >&2
    exit 0
fi

bazel_jvm_args=""
if [ -f /etc/ssl/certs/java/cacerts ]; then
    # On hosts where the JVM's embedded cacerts doesn't trust the
    # system CA bundle (Bazel-on-Debian-with-corporate-CA), point
    # the JVM at the system store.
    bazel_jvm_args="--host_jvm_args=-Djavax.net.ssl.trustStore=/etc/ssl/certs/java/cacerts --host_jvm_args=-Djavax.net.ssl.trustStorePassword=changeit"
fi

# The bazel half fetches BCR deps (rules_cc / rules_pkg, + zlib for libpng)
# from GitHub releases, which intermittently 502 / TLS-timeout in CI. Two
# defenses so a blocking fidelity gate doesn't flake on that infra noise:
#
#   1. A persistent --repository_cache: bazel's repository cache is content-
#      addressed, so once an archive is fetched it's reused with no further
#      download. The path is STABLE (outside the per-run scratch tree) so it
#      survives across the run's fixtures AND, when CI restores it via
#      actions/cache, across CI runs — after the first cold populate there's
#      no re-download, hence no 502 exposure. Override with
#      FIDELITY_BAZEL_REPO_CACHE.
#   2. --experimental_repository_downloader_retries above bazel's default (5)
#      to ride out a transient burst during that first cold populate.
fidelity_repo_cache="${FIDELITY_BAZEL_REPO_CACHE:-$HOME/.cache/bazel-fidelity-repo}"
mkdir -p "$fidelity_repo_cache"
# The repo-cache flag carries a path (which may contain spaces), so it's passed
# as its own quoted argument at each build invocation below — NOT folded into
# the word-split $bazel_build_flags bag. $bazel_build_flags holds only
# space-free flags that are intentionally word-split.
bazel_build_flags="--experimental_repository_downloader_retries=10"
bazel_artifact=""
if [ "$no_library" = false ]; then
    echo "fidelity[$project_name]: bazel build $bazel_target_label" >&2
    # shellcheck disable=SC2086
    (cd "$bazel_ws" && bazel $bazel_jvm_args build "--repository_cache=$fidelity_repo_cache" $bazel_build_flags "$bazel_target_label") > "$work_dir/bazel.log" 2>&1 || {
        echo "fidelity[$project_name]: bazel build FAILED — see $work_dir/bazel.log" >&2
        tail -20 "$work_dir/bazel.log" >&2
        exit 1
    }
    # `bazel-bin` is a symlink into bazel's output base; `find -L`
    # follows it. Without -L the find walks the symlink's target as a
    # leaf and finds nothing.
    bazel_artifact="$(find -L "$bazel_ws/bazel-bin" -name "$bazel_artifact_pattern" -type f 2>/dev/null | head -1)"
    if [ -z "$bazel_artifact" ]; then
        echo "fidelity[$project_name]: bazel build produced no artifact matching $bazel_artifact_pattern" >&2
        exit 1
    fi
    echo "fidelity[$project_name]:   bazel artifact: $bazel_artifact" >&2
fi

# --- Step 3b (consumer mode): bazel-side consumer compile.
# Append a cc_library rule consuming the converted target's exported
# headers, then bazel-build it. The resulting .o is the bazel-side
# counterpart of the cmake-side consumer.cmake.o produced in step 1b.
consumer_bazel_o=""
if [ -n "$consumer_file" ]; then
    # Pick a default Bazel dep label when the caller didn't specify one.
    if [ -z "$consumer_bazel_dep" ]; then
        consumer_bazel_dep=":$target"
    fi
    case "$consumer_file" in
        *.cpp|*.cc|*.cxx) consumer_ext=".cpp" ;;
        *) consumer_ext=".c" ;;
    esac
    cp "$consumer_file" "$bazel_ws/_fidelity_consumer$consumer_ext"
    cat >> "$bazel_ws/BUILD.bazel" <<EOF

cc_library(
    name = "_fidelity_consumer",
    srcs = ["_fidelity_consumer$consumer_ext"],
    deps = ["$consumer_bazel_dep"],
)
EOF
    # Match the cmake-side consumer compile's optimization level (-O2,
    # step 1b). Bazel's default fastbuild compiles at -O0, where g++
    # emits every template instantiation as a weak symbol instead of
    # eliding it — a methodology artifact that floods the diff with
    # unpaired weak symbols on template-heavy consumers (spdlog), not a
    # converter delta. Both sides at -O2 makes the symbol sets comparable.
    # shellcheck disable=SC2086
    (cd "$bazel_ws" && bazel $bazel_jvm_args build "--repository_cache=$fidelity_repo_cache" $bazel_build_flags --copt=-O2 :_fidelity_consumer) \
        > "$work_dir/bazel-consumer.log" 2>&1 || {
            echo "fidelity[$project_name]: consumer bazel-side build FAILED — see $work_dir/bazel-consumer.log" >&2
            tail -20 "$work_dir/bazel-consumer.log" >&2
            exit 1
        }
    # Look for the consumer's .o or .pic.o under bazel-bin.
    consumer_bazel_o="$(find -L "$bazel_ws/bazel-bin" \
        \( -name '_fidelity_consumer.pic.o' -o -name '_fidelity_consumer.o' \) \
        -type f 2>/dev/null | head -1)"
    if [ -z "$consumer_bazel_o" ]; then
        echo "fidelity[$project_name]: bazel produced no consumer .o" >&2
        exit 1
    fi
    echo "fidelity[$project_name]:   consumer bazel-side .o: $consumer_bazel_o" >&2
fi

# In consumer mode, swap the artifacts handed to fidelity-compare from
# the static-archive pair to the consumer .o pair.
if [ -n "$consumer_file" ]; then
    cmake_artifact="$consumer_cmake_o"
    bazel_artifact="$consumer_bazel_o"
fi

# --- Step 4: classify deltas.
echo "fidelity[$project_name]: fidelity-compare" >&2
allowlist_arg=""
if [ -n "$allowlist" ] && [ -f "$allowlist" ]; then
    allowlist_arg="--allowlist $allowlist"
fi
# shellcheck disable=SC2086
"$bin_dir/fidelity-compare" \
    --cmake-artifact "$cmake_artifact" \
    --bazel-artifact "$bazel_artifact" \
    --report "$work_dir/fidelity.json" \
    $allowlist_arg
echo "fidelity[$project_name]: ok" >&2
