#!/usr/bin/env bash
# Regenerate testdata/fileapi/<name>/ from testdata/sample-projects/<name>/.
#
# For each checked-in sample project, runs cmake with File API queries enabled
# and copies the reply directory into testdata for unit-test consumption.
#
# Idempotent: deletes the existing reply dir before regenerating.
#
# Usage: tools/fixtures/record-fileapi.sh [project-name ...]
#        With no args, regenerates every sample project.

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
SAMPLES_DIR="$REPO_ROOT/converter/testdata/sample-projects"
FIXTURES_DIR="$REPO_ROOT/converter/testdata/fileapi"

record_one() {
    local name="$1"
    local src="$SAMPLES_DIR/$name"
    local out="$FIXTURES_DIR/$name"

    if [[ ! -d "$src" ]]; then
        echo "no such sample project: $src" >&2
        return 1
    fi

    echo "==> recording fileapi for $name"
    local build
    build="$(mktemp -d)"

    # Request all four API kinds we consume.
    mkdir -p "$build/.cmake/api/v1/query"
    : >"$build/.cmake/api/v1/query/codemodel-v2"
    : >"$build/.cmake/api/v1/query/toolchains-v1"
    : >"$build/.cmake/api/v1/query/cmakeFiles-v1"
    : >"$build/.cmake/api/v1/query/cache-v2"

    # consumer/+producer/ convention: a fixture testing
    # cross-element behavior (e.g. macro-from-import) puts the
    # consumer cmake project in `consumer/` and producer-shipped
    # cmake macros in `producer/`. cmake `-S` points at
    # `consumer/`; `producer/` goes onto CMAKE_MODULE_PATH so
    # `include(<name>)` resolves to a path OUTSIDE the consumer
    # source root — which is the whole point: trace records
    # those calls with the producer module as `file`, exercising
    # lower's known-target rescue path.
    local cmake_src="$src"
    local extra_args=()
    if [[ -d "$src/consumer" && -f "$src/consumer/CMakeLists.txt" ]]; then
        cmake_src="$src/consumer"
        if [[ -d "$src/producer" ]]; then
            extra_args+=(-DCMAKE_MODULE_PATH="$src/producer")
        fi
    fi

    cmake -S "$cmake_src" -B "$build" -G Ninja \
        -DCMAKE_BUILD_TYPE=Release \
        -DCMAKE_EXPORT_COMPILE_COMMANDS=ON \
        "${extra_args[@]}" \
        --trace-expand --trace-format=json-v1 \
        --trace-redirect="$build/trace.jsonl" \
        >"$build/cmake.stdout" 2>"$build/cmake.stderr"

    rm -rf "$out"
    mkdir -p "$out"
    cp -R "$build/.cmake/api/v1/reply/." "$out/"

    # Also stash build.ninja and CMakeFiles/rules.ninja for genrule recovery
    # tests; the included rules file lives next door and is opaque to ninja
    # consumers but our parser exposes it via the include directive.
    cp "$build/build.ninja" "$out/build.ninja"
    if [[ -f "$build/CMakeFiles/rules.ninja" ]]; then
        mkdir -p "$out/CMakeFiles"
        cp "$build/CMakeFiles/rules.ninja" "$out/CMakeFiles/rules.ninja"
    fi
    # Capture cmake's --trace-expand output. Used by lower's
    # PUBLIC/PRIVATE-aware include partitioner, IMPORTED-target
    # dep recovery for static libs, and configure_file genrule
    # emission. See internal/shadow/trace_commands.go.
    #
    # Trace events embed absolute paths (the recording machine's
    # source + build dirs). Lower's inSourceTree check uses
    # codemodel's Paths.Source — also a recorded absolute path —
    # so the in-tree filter compares recorded-vs-recorded. Both
    # baked-in paths stay consistent across replays; tests on a
    # different checkout work because the same recorded prefix
    # is used to extract relative paths from both.
    if [[ -f "$build/trace.jsonl" ]]; then
        cp "$build/trace.jsonl" "$out/trace.jsonl"

        # Stash configure_file outputs into the fixture mirroring
        # the build-dir layout. lower's configure_file recovery
        # reads each output's bytes from the build dir at convert-
        # element time; offline test runs need those bytes
        # captured here. Outputs outside the build dir (rare —
        # configure_file with absolute non-build dest) are
        # skipped because lower can't anchor them anyway.
        while IFS= read -r abs_out; do
            [[ -z "$abs_out" ]] && continue
            [[ "$abs_out" == "$build/"* ]] || continue
            [[ -f "$abs_out" ]] || continue
            rel="${abs_out#"$build/"}"
            mkdir -p "$out/$(dirname "$rel")"
            cp "$abs_out" "$out/$rel"
        done < <(jq -r 'select(.cmd == "configure_file") | .args[1] // empty' "$build/trace.jsonl")

        # Same stash treatment for file(GENERATE) outputs.
        # cmake evaluates file(GENERATE) at generate-time (after
        # configure ends), so the rendered output exists in the
        # build dir after cmake exits. lower's file_generate
        # recovery reads each output's bytes from the build dir
        # at convert-element time; the offline fixture needs them
        # captured here. Trace records the call as cmd=="file"
        # with args[0]=="GENERATE"; the OUTPUT value follows the
        # "OUTPUT" keyword in the args list (variadic keyword
        # walk, so use jq to find it positionally).
        while IFS= read -r abs_out; do
            [[ -z "$abs_out" ]] && continue
            [[ "$abs_out" == "$build/"* ]] || continue
            [[ -f "$abs_out" ]] || continue
            rel="${abs_out#"$build/"}"
            mkdir -p "$out/$(dirname "$rel")"
            cp "$abs_out" "$out/$rel"
        done < <(jq -r '
            select(.cmd == "file" and (.args | length) >= 3 and (.args[0] | ascii_upcase) == "GENERATE")
            | .args as $a
            | range(1; ($a | length) - 1) as $i
            | select(($a[$i] | ascii_upcase) == "OUTPUT") | $a[$i+1]
        ' "$build/trace.jsonl")
    fi

    echo "    -> $(find "$out" -type f | wc -l) files in $out"
    rm -rf "$build"
}

main() {
    if [[ $# -eq 0 ]]; then
        for d in "$SAMPLES_DIR"/*/; do
            record_one "$(basename "$d")"
        done
    else
        for name in "$@"; do
            record_one "$name"
        done
    fi
}

main "$@"
