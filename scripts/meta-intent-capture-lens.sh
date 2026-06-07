#!/bin/sh
# meta-intent-capture-lens.sh — render gate for the intent-capture survey lens
# (scripts/intent-capture-lens.sh + converter/cmd/intent-lens).
#
# The LLM judge is non-deterministic and not available in CI, so the gate wires
# a STUB judge: a script that ignores the prompt and emits a fixed findings JSON
# referencing (a) a real todo-anchor file from the converted fixture — which
# triage MUST classify dup-todo — and (b) a file in no anchor — which MUST come
# back net-new. This proves the deterministic halves (prompt assembly + the
# dedup/triage) end to end through the real script pipeline.
#
# Gating: skips cleanly when cmake / ninja / go / python3 are absent.
set -eu

repo_root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$repo_root"

command -v cmake   >/dev/null 2>&1 || { echo "skip: cmake not on PATH";   exit 0; }
command -v ninja   >/dev/null 2>&1 || { echo "skip: ninja not on PATH";   exit 0; }
command -v go      >/dev/null 2>&1 || { echo "skip: go not on PATH";      exit 0; }
command -v python3 >/dev/null 2>&1 || { echo "skip: python3 not on PATH"; exit 0; }

work_dir="$(mktemp -d)"
trap 'rm -rf "$work_dir"' EXIT

# Convert a fixture so we get a real BUILD.bazel + conversion-todos.json with at
# least one anchored todo. The multi-disposition fixture has a cmake-p-test.
ws="$work_dir/ws"; mkdir -p "$ws"
cp -R "$repo_root/converter/testdata/sample-projects/todos-multi-disposition/." "$ws/"

bin_dir="$repo_root/build/bin"; mkdir -p "$bin_dir"
make converter >/dev/null
"$bin_dir/convert-element-cmake" \
  --source-root "$ws" \
  --ignore-rejections-for-diagnostics \
  --conversion-todos-report "$ws/conversion-todos.json" \
  --rejections-report "$ws/rejections.json" \
  --out-build "$ws/BUILD.bazel" \
  >"$work_dir/convert.out" 2>"$work_dir/convert.err" || true
[ -s "$ws/conversion-todos.json" ] || { echo "FAIL: no conversion-todos.json"; sed 's/^/  /' "$work_dir/convert.err"; exit 1; }

# A faithful MODULE.bazel (write-a project-B shape) so the bundle is complete.
cat > "$ws/MODULE.bazel" <<EOF
module(name = "todos_multi_disposition", version = "0.0.0")
bazel_dep(name = "rules_cc", version = "0.0.17")
bazel_dep(name = "rules_pkg", version = "1.0.1")
bazel_dep(name = "bazel_skylib", version = "1.8.2")
EOF

# Pull a real todo identity out of the report for the stub to cite (→ dup). The
# producers carry the unit's identity in group_key (a runner path / unit id);
# the cmake-p-test todo's group_key is the runner script the dedup matches on.
anchor_file="$(python3 -c "
import json
d=json.load(open('$ws/conversion-todos.json'))
for t in d['todos']:
    if t['kind'] == 'cmake-p-test':
        print(t['group_key']); raise SystemExit
print('')
")"
[ -n "$anchor_file" ] || { echo "FAIL: fixture produced no cmake-p-test todo to dedup against"; exit 1; }

# The STUB judge: ignore stdin (the prompt), emit two findings.
judge="$work_dir/stub-judge.sh"
cat > "$judge" <<EOF
#!/bin/sh
cat >/dev/null   # drain the prompt
cat <<JSON
{"findings":[
 {"category":"install","severity":"high","summary":"net-new install layout miss","evidence":"stub","cmake_ref":"NoSuchFile.cmake:1"},
 {"category":"test","severity":"medium","summary":"dup of a known todo","evidence":"stub","cmake_ref":"$anchor_file:99"}
]}
JSON
EOF
chmod +x "$judge"

out_dir="$work_dir/out"
INTENT_LENS_JUDGE="sh $judge" sh "$repo_root/scripts/intent-capture-lens.sh" \
  "$ws" "$ws" "$out_dir" todos-multi-disposition >"$work_dir/lens.out" 2>&1 || {
    echo "FAIL: lens pipeline errored"; sed 's/^/  /' "$work_dir/lens.out"; exit 1; }

[ -s "$out_dir/intent-capture.json" ] || { echo "FAIL: no intent-capture.json"; sed 's/^/  /' "$work_dir/lens.out"; exit 1; }

python3 - "$out_dir/intent-capture.json" "$anchor_file" <<'PY'
import json, sys
d = json.load(open(sys.argv[1]))
anchor = sys.argv[2]
s = d["summary"]
if s["total"] != 2 or s["net_new"] != 1 or s["already_flagged"] != 1:
    print("FAIL: summary counts wrong:", s); sys.exit(1)
by = {f["summary"]: f for f in d["findings"]}
nn = by.get("net-new install layout miss") or by.get("net-new install layout miss".strip())
nn = next((f for f in d["findings"] if f["category"] == "install"), None)
dup = next((f for f in d["findings"] if f["category"] == "test"), None)
if not nn or nn["status"] != "net-new":
    print("FAIL: install finding should be net-new:", nn); sys.exit(1)
if not dup or dup["status"] != "dup-todo" or not dup.get("matched_id"):
    print("FAIL: test finding should be dup-todo with a matched id:", dup); sys.exit(1)
print("ok  meta-intent-capture-lens: pipeline produced intent-capture.json; "
      "net-new + dup-todo classified correctly (anchor=%s)" % anchor)
PY
