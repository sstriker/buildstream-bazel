#!/bin/sh
# meta-converge.sh — render-half acceptance gate for the
# convergence driver loop (tools/converge.sh).
#
# The driver itself is a shell script; this gate exercises its
# logic without standing up a REAPI endpoint or bazel by stubbing
# the bazel + stage-b binaries with shell scripts that simulate
# the convergence behavior the driver expects.
#
# Specifically the gate stages:
#   - A fake bazel binary that writes "miss" markers on round 1
#     for one element, "hit" markers on round 2 onwards
#   - A fake stage-b binary that no-ops
#   - Empty project A / project B directories
#
# Then runs tools/converge.sh and asserts:
#   - The loop runs at least 2 rounds (round 1 sees the miss,
#     round 2 sees the hit)
#   - The CONVERGE_GENERATION env var increments per round
#   - The loop terminates with "fixpoint reached"
#
# Bazel-build-half exercise is intentionally out of scope —
# tools/e2e-meta-autotools-round2-live.sh covers the live
# REAPI half end-to-end; this gate covers the driver's control
# flow + ordering contract.

set -eu

repo_root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$repo_root"

work_dir="$(mktemp -d)"
trap 'rm -rf "$work_dir"' EXIT

# Fake bazel: simulates "round 1 miss → round 2 hit" for one
# element. On any invocation, reads CONVERGE_GENERATION from env
# and writes a marker file accordingly. The marker path mirrors
# the trace_load rule's declared output:
#   <bazel-bin>/elements/<elem>/<name>/marker
mkdir -p "$work_dir/bin"
cat > "$work_dir/bin/bazel" <<'EOF'
#!/bin/sh
# Args: "build" [...flags] [targets]
# Parses --action_env=CONVERGE_GENERATION=<n> from its own argv —
# real bazel reads this flag and passes the value through to
# action envs at execution time, but for this stub we extract it
# directly to decide which marker contents to write.
GEN=""
for arg in "$@"; do
    case "$arg" in
        --action_env=CONVERGE_GENERATION=*)
            GEN="${arg#--action_env=CONVERGE_GENERATION=}"
            ;;
    esac
done
case "$1" in
    build)
        if [ -n "$GEN" ]; then
            mkdir -p "bazel-bin/elements/demo/demo_trace_load"
            if [ "$GEN" = "1" ]; then
                printf 'miss\n' > bazel-bin/elements/demo/demo_trace_load/marker
            else
                printf 'hit\n' > bazel-bin/elements/demo/demo_trace_load/marker
            fi
        fi
        exit 0
        ;;
    *)
        exit 0
        ;;
esac
EOF
chmod +x "$work_dir/bin/bazel"

# Fake stage-b: no-op (the gate doesn't care about staging
# correctness — that's stage-b's own unit tests).
cat > "$work_dir/bin/stage-b" <<'EOF'
#!/bin/sh
exit 0
EOF
chmod +x "$work_dir/bin/stage-b"

# Set up empty project A + B.
mkdir -p "$work_dir/A" "$work_dir/B"

# Run the driver.
log="$work_dir/converge.log"
"$repo_root/tools/converge.sh" \
    --project-a "$work_dir/A" \
    --project-b "$work_dir/B" \
    --bazel "$work_dir/bin/bazel" \
    --stage-b "$work_dir/bin/stage-b" \
    --max-rounds 5 > "$log" 2>&1
rc=$?
if [ "$rc" -ne 0 ]; then
    echo "meta-converge: driver exited with rc=$rc" >&2
    cat "$log" >&2
    exit "$rc"
fi

# Assertions.
for marker in \
    "=== converge: round 1 ===" \
    "1 trace_build target(s) on the frontier:" \
    "//elements/demo:demo_trace_build" \
    "=== converge: round 2 ===" \
    "fixpoint reached after 2 round(s)" \
    "converge: done."; do
    if ! grep -qF -- "$marker" "$log"; then
        echo "meta-converge: log missing marker: $marker" >&2
        cat "$log" >&2
        exit 1
    fi
done

# The driver MUST terminate before --max-rounds; verify it ran
# exactly 2 rounds (round 1 miss → round 2 hit).
n_rounds=$(grep -c "=== converge: round" "$log")
if [ "$n_rounds" -ne 2 ]; then
    echo "meta-converge: expected 2 rounds; got $n_rounds" >&2
    cat "$log" >&2
    exit 1
fi

# Multi-round termination check: max-rounds=5 with a fake bazel
# that keeps writing "miss" should fail with the max-rounds
# diagnostic.
cat > "$work_dir/bin/bazel" <<'EOF'
#!/bin/sh
GEN=""
for arg in "$@"; do
    case "$arg" in
        --action_env=CONVERGE_GENERATION=*)
            GEN="${arg#--action_env=CONVERGE_GENERATION=}"
            ;;
    esac
done
case "$1" in
    build)
        if [ -n "$GEN" ]; then
            mkdir -p "bazel-bin/elements/demo/demo_trace_load"
            printf 'miss\n' > bazel-bin/elements/demo/demo_trace_load/marker
        fi
        exit 0
        ;;
    *)
        exit 0
        ;;
esac
EOF
chmod +x "$work_dir/bin/bazel"
mr_log="$work_dir/converge-mr.log"
set +e
"$repo_root/tools/converge.sh" \
    --project-a "$work_dir/A" \
    --project-b "$work_dir/B" \
    --bazel "$work_dir/bin/bazel" \
    --stage-b "$work_dir/bin/stage-b" \
    --max-rounds 3 > "$mr_log" 2>&1
mr_rc=$?
set -e
if [ "$mr_rc" -eq 0 ]; then
    echo "meta-converge: never-converging case unexpectedly succeeded" >&2
    cat "$mr_log" >&2
    exit 1
fi
if ! grep -qF "FAILED to converge after 3 rounds" "$mr_log"; then
    echo "meta-converge: never-converging case missing the max-rounds diagnostic" >&2
    cat "$mr_log" >&2
    exit 1
fi

# Depth-2 alternation case: simulates a cmake → trace → cmake →
# trace chain where convergence takes 3 rounds. The bottom
# element (depth0) converges round 1; the middle element
# (depth1) round 2; the top element (depth2) round 3. Each
# round's converged element's trace_load reports "hit" from
# that round on; unconverged elements report "miss" until
# their dep converges.
work_dir3="$(mktemp -d)"
mkdir -p "$work_dir3/bin" "$work_dir3/A" "$work_dir3/B"
cat > "$work_dir3/bin/bazel" <<'EOF'
#!/bin/sh
GEN=""
for arg in "$@"; do
    case "$arg" in
        --action_env=CONVERGE_GENERATION=*)
            GEN="${arg#--action_env=CONVERGE_GENERATION=}"
            ;;
    esac
done
case "$1" in
    build)
        if [ -n "$GEN" ]; then
            for depth in 0 1 2; do
                mkdir -p "bazel-bin/elements/depth$depth/depth${depth}_trace_load"
                # Element depthN converges when round > N.
                if [ "$GEN" -gt "$depth" ]; then
                    printf 'hit\n' > "bazel-bin/elements/depth$depth/depth${depth}_trace_load/marker"
                else
                    printf 'miss\n' > "bazel-bin/elements/depth$depth/depth${depth}_trace_load/marker"
                fi
            done
        fi
        exit 0
        ;;
    *) exit 0 ;;
esac
EOF
chmod +x "$work_dir3/bin/bazel"
cat > "$work_dir3/bin/stage-b" <<'EOF'
#!/bin/sh
exit 0
EOF
chmod +x "$work_dir3/bin/stage-b"

log3="$work_dir3/converge.log"
"$repo_root/tools/converge.sh" \
    --project-a "$work_dir3/A" \
    --project-b "$work_dir3/B" \
    --bazel "$work_dir3/bin/bazel" \
    --stage-b "$work_dir3/bin/stage-b" \
    --max-rounds 10 > "$log3" 2>&1

# Round 1: depth0 hits; depth1, depth2 miss → frontier = {depth1, depth2}.
# Wait actually: round=1, GEN=1, depth0 (depth=0) → GEN(1) > 0 ? yes → hit.
#                                  depth1 (depth=1) → GEN(1) > 1 ? no → miss.
#                                  depth2 (depth=2) → GEN(1) > 2 ? no → miss.
# Round 2: GEN=2. depth0 hit; depth1 hit; depth2 miss. Frontier = {depth2}.
# Round 3: GEN=3. all hit. Terminate.
n_rounds=$(grep -c "=== converge: round" "$log3")
if [ "$n_rounds" -ne 3 ]; then
    echo "meta-converge: depth-2 case expected 3 rounds; got $n_rounds" >&2
    cat "$log3" >&2
    exit 1
fi
# Round 1 frontier: depth1 + depth2 (2 targets).
if ! grep -qE "2 trace_build target\(s\) on the frontier:" "$log3"; then
    echo "meta-converge: depth-2 case round 1 frontier size != 2" >&2
    grep "trace_build target" "$log3" >&2
    exit 1
fi
# Round 2 frontier: depth2 (1 target).
# Round 3: fixpoint.
if ! grep -qF "fixpoint reached after 3 round(s)" "$log3"; then
    echo "meta-converge: depth-2 case did not report fixpoint at round 3" >&2
    cat "$log3" >&2
    exit 1
fi
# Every depth's trace_build appeared on the frontier in some round.
for elem in depth0 depth1 depth2; do
    # depth0 never lands on the frontier (it hits from round 1).
    if [ "$elem" = "depth0" ]; then
        if grep -qF "//elements/depth0:depth0_trace_build" "$log3"; then
            echo "meta-converge: depth0 unexpectedly appeared on the frontier" >&2
            cat "$log3" >&2
            exit 1
        fi
        continue
    fi
    if ! grep -qF "//elements/$elem:${elem}_trace_build" "$log3"; then
        echo "meta-converge: depth-2 case never listed $elem on a frontier" >&2
        cat "$log3" >&2
        exit 1
    fi
done
rm -rf "$work_dir3"

echo "meta-converge: ok"
