#!/bin/sh
# converge.sh — uniform convergence driver for the round-2 AC
# rendezvous.
#
# Runs the fixpoint loop the ROADMAP cross-element bootstrap item
# describes:
#
#   1. bazel build project A's :*_trace_load targets with a
#      bumped CONVERGE_GENERATION (forces trace-lookup re-queries).
#   2. Build project A's converter genrules (consume trace_load
#      outputs; emit BUILD.bazel.out — placeholder on AC miss,
#      fine cc rules on hit).
#   3. Stage project A's BUILD.bazel.out files into project B
#      (cmd/stage-b).
#   4. Read the trace_load markers to find the convergence
#      frontier — elements whose trace_load returned "miss".
#   5. For each missed element, bazel build the matching
#      :*_trace_build target in project B (publishes trace +
#      config bundle to the AC).
#   6. Bump CONVERGE_GENERATION; loop.
#
# Termination: when the frontier is empty (every trace_load
# returned "hit") OR when CONVERGE_MAX_ROUNDS is exceeded. The
# DAG bound guarantees termination at a depth equal to the
# longest configure-needing chain in the .bst graph.
#
# Usage:
#
#   tools/converge.sh --project-a <path> --project-b <path> \
#       --stage-b <path-to-stage-b-bin> \
#       [--cas-grpc-addr <host:port>] [--max-rounds N] [--bazel <bin>]
#
# Required flags: --project-a, --project-b, --stage-b.
#
# Optional flags:
#   --cas-grpc-addr — REAPI gRPC endpoint trace-lookup queries.
#       Empty / omitted ⇒ offline mode: trace_load short-circuits
#       to miss, trace_builds run inline trace-publish as no-ops,
#       the loop terminates by --max-rounds (equivalent to the
#       legacy one-pass "build A, stage, build B" shape).
#   --max-rounds N — fixpoint iteration cap (default 10).
#   --bazel <bin>  — bazel binary to invoke (default "bazel";
#                    BAZEL env var also honored).
#
# Env vars (alternatives to flags):
#   STAGE_B_BIN — fallback when --stage-b isn't passed.
#   BAZEL       — fallback when --bazel isn't passed.

set -eu

PROJECT_A=""
PROJECT_B=""
CAS_ADDR=""
MAX_ROUNDS=10
BAZEL="${BAZEL:-bazel}"
STAGE_B_BIN="${STAGE_B_BIN:-}"

while [ $# -gt 0 ]; do
    case "$1" in
        --project-a)
            PROJECT_A="$2"
            shift 2
            ;;
        --project-b)
            PROJECT_B="$2"
            shift 2
            ;;
        --cas-grpc-addr)
            CAS_ADDR="$2"
            shift 2
            ;;
        --max-rounds)
            MAX_ROUNDS="$2"
            shift 2
            ;;
        --stage-b)
            STAGE_B_BIN="$2"
            shift 2
            ;;
        --bazel)
            BAZEL="$2"
            shift 2
            ;;
        -h|--help)
            # Strip the leading "#" / "# " from the file's
            # comment block. The script body starts at the
            # first non-comment line, so awk reads until then.
            awk '/^[^#]/{exit} {sub(/^# ?/, ""); print}' "$0"
            exit 0
            ;;
        *)
            echo "converge: unknown arg: $1" >&2
            exit 2
            ;;
    esac
done

if [ -z "$PROJECT_A" ] || [ -z "$PROJECT_B" ]; then
    echo "converge: --project-a and --project-b are required" >&2
    exit 2
fi

if [ -z "$STAGE_B_BIN" ]; then
    echo "converge: --stage-b is required (path to cmd/stage-b binary)" >&2
    exit 2
fi

# CAS_ADDR can be empty for offline / no-remote runs. The
# trace_load actions short-circuit to miss when CAS_GRPC_ADDR is
# empty; in that case the loop runs the trace_builds (which
# short-circuit their inline trace-publish too), then re-queries
# (still empty), then terminates by max-rounds. Operator-friendly
# offline mode: every element ends up with a placeholder
# BUILD.bazel.out + a built trace_build target — equivalent to
# the legacy "one-pass build A, stage, build B" shape.

ROUND=0
while true; do
    ROUND=$((ROUND + 1))
    echo "=== converge: round $ROUND ==="

    # Step 1+2: build project A. The --action_env=CONVERGE_GENERATION
    # bump is what forces Bazel's ActionCache to re-run trace_load
    # actions even when their hermetic inputs haven't changed —
    # the AC view (what the REAPI ActionCache holds) can shift
    # between rounds even when nothing local did.
    echo "  bazel build //elements/...:* in project A"
    if [ -n "$CAS_ADDR" ]; then
        (cd "$PROJECT_A" && "$BAZEL" build \
            --action_env=CAS_GRPC_ADDR="$CAS_ADDR" \
            --action_env=CONVERGE_GENERATION="$ROUND" \
            //elements/...:* >/dev/null)
    else
        (cd "$PROJECT_A" && "$BAZEL" build \
            --action_env=CONVERGE_GENERATION="$ROUND" \
            //elements/...:* >/dev/null)
    fi

    # Step 3: stage project A's BUILD.bazel.out files into project B.
    # cmd/stage-b copies each element's converted BUILD.bazel.out
    # over the corresponding project B placeholder. Elements with
    # no project-A converted output (kind:stack / filter / import)
    # are skipped silently.
    echo "  stage A's BUILD.bazel.out files into B"
    "$STAGE_B_BIN" --in "$PROJECT_A" --out "$PROJECT_B" >/dev/null

    # Step 4: read the trace_load markers. Path shape:
    #   $PROJECT_A/bazel-bin/elements/<elem>/<name>/marker
    # where <name> is "<elem>_trace_load" (single-platform) or
    # "<elem>_trace_load_<platform>" (multi-platform).
    # 2>/dev/null on both find AND grep so a missing bazel-bin
    # (first round; bazel hasn't created it yet) or a marker
    # with no matching content doesn't leak diagnostics into
    # the captured stdout. POSIX `find ... -name marker` can
    # also emit "permission denied" on subdirs; the redirection
    # silences that too.
    miss_markers=$(find "$PROJECT_A/bazel-bin/elements" -name marker 2>/dev/null \
        | { xargs -r grep -l "^miss" 2>/dev/null || true; })

    if [ -z "$miss_markers" ]; then
        echo "  no trace_load misses; fixpoint reached after $ROUND round(s)"
        break
    fi

    # Map each marker path to its trace_build target name. The
    # name pattern matches: rename _trace_load → _trace_build.
    # Build the unique set so a multi-element pass doesn't re-list
    # the same element twice.
    trace_build_targets=""
    for marker in $miss_markers; do
        # marker path: .../elements/<elem>/<name>/marker
        name_dir=$(dirname "$marker")          # .../elements/<elem>/<name>
        elem_dir=$(dirname "$name_dir")        # .../elements/<elem>
        load_name=$(basename "$name_dir")      # <name>
        elem=$(basename "$elem_dir")           # <elem>
        # Substitute _trace_load with _trace_build in the name.
        build_name=$(echo "$load_name" | sed 's/_trace_load/_trace_build/')
        target="//elements/$elem:$build_name"
        # Append if not already in the list.
        case " $trace_build_targets " in
            *" $target "*) ;;
            *) trace_build_targets="$trace_build_targets $target" ;;
        esac
    done

    n_targets=$(echo $trace_build_targets | wc -w)
    echo "  $n_targets trace_build target(s) on the frontier:"
    for t in $trace_build_targets; do echo "    $t"; done

    if [ "$ROUND" -ge "$MAX_ROUNDS" ]; then
        echo "converge: FAILED to converge after $MAX_ROUNDS rounds; frontier still non-empty" >&2
        exit 1
    fi

    # Step 5: build the trace_build targets in project B. Each
    # one runs configure/build/install under build-tracer, writes
    # install_tree.tar + trace.log + (optionally) make-db.txt +
    # cmake-config-bundle.tar, then trace-publishes. CAS_GRPC_ADDR
    # threaded through via --action_env so trace-publish can
    # actually publish. Without CAS_GRPC_ADDR the publish step
    # short-circuits silently and the next round's trace_load will
    # still miss — the loop terminates by --max-rounds in that
    # case (operator's intent: build everything locally, no
    # cross-machine sharing).
    echo "  bazel build the frontier's trace_build targets in project B"
    # shellcheck disable=SC2086
    if [ -n "$CAS_ADDR" ]; then
        (cd "$PROJECT_B" && "$BAZEL" build \
            --action_env=CAS_GRPC_ADDR="$CAS_ADDR" \
            --action_env=CONVERGE_GENERATION="$ROUND" \
            $trace_build_targets >/dev/null)
    else
        (cd "$PROJECT_B" && "$BAZEL" build \
            --action_env=CONVERGE_GENERATION="$ROUND" \
            $trace_build_targets >/dev/null)
    fi

    # Loop: next round's bazel build A re-runs the trace_load
    # actions (CONVERGE_GENERATION bumped), now seeing the
    # just-published trace + bundle for the frontier elements.
done

# Final pass: build everything in project B with the converged
# BUILD.bazel.outs staged.
echo "  bazel build //... in project B (final)"
if [ -n "$CAS_ADDR" ]; then
    (cd "$PROJECT_B" && "$BAZEL" build \
        --action_env=CAS_GRPC_ADDR="$CAS_ADDR" \
        //... >/dev/null)
else
    (cd "$PROJECT_B" && "$BAZEL" build //... >/dev/null)
fi
echo "converge: done."
