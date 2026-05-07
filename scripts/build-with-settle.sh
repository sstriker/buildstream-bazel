#!/bin/sh
# build-with-settle.sh — invoke `bazel build` twice each on project A
# and project B so round-2's trace publish/lookup cycle settles in one
# command.
#
# Why: round-2's chicken-and-egg shape (B publishes the trace; A
# re-runs to consume it; B then picks up the new fine-grained rules)
# costs two bazel invocations to fully settle when an element's
# srckey moved. See docs/trace-driven-autotools.md "Re-conversion
# thrash". Rather than instrument and conditionally re-invoke, just
# run the four-step loop unconditionally — Bazel's persistent
# daemons + action cache make redundant invocations cheap when
# nothing changed (each subsequent step is a no-op for elements
# whose trace AC was already warm).
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
# Anything past `--` is forwarded to bazel build verbatim.
EXTRA="$*"

if [ -z "$A" ] || [ -z "$B" ] || [ -z "$A_TARGETS" ] || [ -z "$B_TARGETS" ]; then
    usage
fi

# Each pass is one bazel invocation per workspace. The second pass
# is the settle: A picks up traces B published in pass-1; B picks up
# the new BUILD.bazel.out A emitted in pass-2-A.
run_pass() {
    pass="$1"
    echo "build-with-settle: pass $pass — project A ($A_TARGETS)" >&2
    # shellcheck disable=SC2086
    (cd "$A" && bazel build $EXTRA -- $A_TARGETS)
    echo "build-with-settle: pass $pass — project B ($B_TARGETS)" >&2
    # shellcheck disable=SC2086
    (cd "$B" && bazel build $EXTRA -- $B_TARGETS)
}

run_pass 1
run_pass 2

echo "build-with-settle: settled in 2 passes" >&2
