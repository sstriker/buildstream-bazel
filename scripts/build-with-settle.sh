#!/bin/sh
# build-with-settle.sh — invoke `bazel build` twice each on project A
# and project B so round-2's trace publish/lookup cycle settles in one
# command.
#
# Why: round-2's chicken-and-egg shape (B publishes the trace; A
# re-runs to consume it; B then picks up the new fine-grained rules)
# takes two passes (four `bazel build` invocations) to fully settle
# when an element's srckey moved. See docs/design/rendezvous.md
# (round-2 mechanism). Rather than instrument and conditionally
# re-invoke, just run the four-step loop unconditionally — Bazel's
# persistent daemons + action cache make redundant invocations
# cheap when nothing changed (each subsequent step is a no-op for
# elements whose trace AC was already warm).
#
# Usage:
#   build-with-settle.sh -A <projectA-dir> -B <projectB-dir>
#                        -a <bazel-targets-A> -b <bazel-targets-B>
#                        [-- <extra bazel build args>]
#
# Example:
#   build-with-settle.sh -A /tmp/A -B /tmp/B \
#                        -a //elements/...:all -b //consumer:all \
#                        -- --config=ci
#
# Exit status: non-zero on the first invocation that fails. The
# script does not attempt partial-failure recovery; a real failure
# means the source tree, toolchain, or dep graph is broken and
# re-invoking won't fix it.

set -eu

usage() {
    cat <<'EOF' >&2
build-with-settle.sh: settle round-2's publish/lookup cycle in one command.

Required:
  -A DIR    project A workspace root (the dir containing MODULE.bazel)
  -B DIR    project B workspace root
  -a SPEC   bazel targets to build in project A (e.g. //...)
  -b SPEC   bazel targets to build in project B (e.g. //...)

Optional, after `--`:
  any additional flags forwarded verbatim to every `bazel build`
  invocation (e.g. --config=ci, --remote_cache=...).
EOF
    exit 64
}

A=""; B=""; A_TARGETS=""; B_TARGETS=""
while getopts "A:B:a:b:h" opt; do
    case "$opt" in
        A) A="$OPTARG" ;;
        B) B="$OPTARG" ;;
        a) A_TARGETS="$OPTARG" ;;
        b) B_TARGETS="$OPTARG" ;;
        h|*) usage ;;
    esac
done
shift $((OPTIND - 1))
# Anything past `--` is forwarded to bazel build via "$@". Don't
# flatten into a single string — that would lose argument boundaries
# and require unsafe word-splitting on expansion (a flag like
# --copt='-DFOO=bar baz' would break apart at the embedded space).

if [ -z "$A" ] || [ -z "$B" ] || [ -z "$A_TARGETS" ] || [ -z "$B_TARGETS" ]; then
    usage
fi
# Verify the workspace dirs exist before kicking off any pass.
# Without this, a typo in -A/-B falls through to a `cd` failure
# under `set -e` and the operator sees a cryptic "no such file or
# directory" attributed to the cd, not to the bad flag value.
for dir in "$A" "$B"; do
    if [ ! -d "$dir" ]; then
        echo "build-with-settle: workspace directory not found: $dir" >&2
        exit 64
    fi
done

# Each pass is one bazel invocation per workspace. The second pass
# is the settle: A picks up traces B published in pass-1; B picks up
# the new BUILD.bazel.out A emitted in pass-2-A.
#
# $1 is the pass number; "$@" after the shift carries the
# forwarded-to-bazel extra args with their argument boundaries
# intact. $A_TARGETS / $B_TARGETS stay unquoted on purpose: the -a /
# -b flags accept a space-separated target list as a single value
# (e.g. -a "//foo //bar"), and word-splitting that into multiple
# args is the desired behaviour. Globbing of the same expansion is
# NOT — bazel target patterns like `//pkg:*` and `@platforms//cpu:*`
# would otherwise expand against the cwd if matching files happened
# to exist there. `set -f` inside the subshell disables glob
# expansion while leaving word-splitting on, which is exactly what
# we want for target lists.
run_pass() {
    pass="$1"
    shift
    echo "build-with-settle: pass $pass — project A ($A_TARGETS)" >&2
    # shellcheck disable=SC2086
    (cd "$A" && set -f && bazel build "$@" -- $A_TARGETS)
    echo "build-with-settle: pass $pass — project B ($B_TARGETS)" >&2
    # shellcheck disable=SC2086
    (cd "$B" && set -f && bazel build "$@" -- $B_TARGETS)
}

run_pass 1 "$@"
run_pass 2 "$@"

echo "build-with-settle: settled in 2 passes" >&2
