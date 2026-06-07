#!/bin/sh
# meta-cmake-todos-coverage.sh — render gate for conversion-todos.json FULL
# coverage of the refusal/bake surfaces + the `disposition` qualifier.
#
# The fixture's execute_process(date … OUTPUT_FILE) is a Tier-1 refusal (a
# non-hermetic stamp driver writing a file). Converted in diagnostic mode
# (--ignore-rejections-for-diagnostics), the generic rejection producer mirrors
# it into conversion-todos.json as a `rejection:unsupported-execute-process`
# entry tagged `disposition: actionable` — proving the refusal surface is
# covered and dispositions flow end to end. (The bake/genex producers and the
# per-site disposition override are covered by the Go unit tests in
# converter/internal/lower/todos_coverage_test.go.)
#
# Also asserts: the preamble carries the disposition guidance (rule 6), and the
# report is byte-identical across two runs (determinism).
#
# Gating: skips cleanly when cmake / ninja / go / make / python3 are absent.
set -eu

repo_root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$repo_root"

command -v cmake >/dev/null 2>&1 || { echo "skip: cmake not on PATH"; exit 0; }
command -v ninja >/dev/null 2>&1 || { echo "skip: ninja not on PATH"; exit 0; }
command -v go >/dev/null 2>&1 || { echo "skip: go not on PATH"; exit 0; }
command -v make >/dev/null 2>&1 || { echo "skip: make not on PATH"; exit 0; }
command -v python3 >/dev/null 2>&1 || { echo "skip: python3 not on PATH (used to assert the JSON)"; exit 0; }

fixture_src="$repo_root/converter/testdata/sample-projects/todos-coverage"
work_dir="$(mktemp -d)"
trap 'rm -rf "$work_dir"' EXIT
ws="$work_dir/ws"
mkdir -p "$ws"
cp -R "$fixture_src/." "$ws/"

bin_dir="$repo_root/build/bin"
mkdir -p "$bin_dir"
make converter >/dev/null

# Convert in diagnostic mode so the refusal is collected (not aborted) and the
# rejection producer can mirror it into the todos report.
run() { # $1 = report path
  "$bin_dir/convert-element-cmake" \
    --source-root "$ws" \
    --ignore-rejections-for-diagnostics \
    --conversion-todos-report "$1" \
    --out-build "$ws/BUILD.bazel" \
    >"$work_dir/convert.out" 2>"$work_dir/convert.err" || true
}

run "$work_dir/todos.json"
if [ ! -s "$work_dir/todos.json" ]; then
  echo "FAIL: no conversion-todos.json written"
  sed 's/^/   stderr: /' "$work_dir/convert.err"
  exit 1
fi

python3 - "$work_dir/todos.json" <<'PY'
import json, sys
d = json.load(open(sys.argv[1]))
todos = d.get("todos", [])
# (1) the refusal is mirrored with the right kind + disposition.
match = [t for t in todos
         if t["kind"] == "rejection:unsupported-execute-process"
         and t["disposition"] == "actionable"]
if not match:
    print("FAIL: no rejection:unsupported-execute-process todo with disposition=actionable")
    print(json.dumps(todos, indent=2))
    sys.exit(1)
# (2) every todo carries a disposition from the known set.
ok = {"actionable", "improvement", "informational"}
for t in todos:
    if t.get("disposition") not in ok:
        print("FAIL: todo missing/invalid disposition:", t)
        sys.exit(1)
# (3) the preamble carries the disposition guidance (rule 6).
rules = d.get("preamble", {}).get("rules", "")
if "disposition" not in rules:
    print("FAIL: preamble rules missing disposition guidance")
    sys.exit(1)
# (3b) the preamble states the project's Bazel environment/conventions so the
# agent doesn't rediscover them (target version + canonical rule providers).
env = d.get("preamble", {}).get("environment", "")
for want in ("Bazel 9", "@rules_shell", "@rules_cc"):
    if want not in env:
        print("FAIL: preamble environment missing", want)
        sys.exit(1)
print("ok  meta-cmake-todos-coverage: refusal mirrored as rejection todo (disposition=actionable); preamble carries disposition guidance")
PY

# (4) determinism: a second run is byte-identical.
run "$work_dir/todos2.json"
if ! cmp -s "$work_dir/todos.json" "$work_dir/todos2.json"; then
  echo "FAIL: conversion-todos.json not byte-identical across runs"
  diff "$work_dir/todos.json" "$work_dir/todos2.json" | sed 's/^/   /' | head
  exit 1
fi
echo "ok  meta-cmake-todos-coverage: report is byte-identical across runs"
