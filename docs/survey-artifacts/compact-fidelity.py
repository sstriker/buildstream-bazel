#!/usr/bin/env python3
"""Signature-group an oversized compile-db fidelity.json.

The raw fidelity.json lists every translation unit individually, so a large
project (vtk ~4200 TUs, llvm) produces a 10MB+ file that's almost entirely the
SAME mismatch signature repeated per TU. That per-TU enumeration is not the
signal — the signal is the set of DISTINCT drift classes, how many TUs each
hits, and a few examples. This rewrites each per-TU *_mismatch map into a
signature histogram, preserving that signal losslessly while collapsing the
repetition. Scalar fields (matched, gen_root_missing, only_*) pass through.

Usage: compact-fidelity.py <in.json> <out.json>
A "_compacted" marker + the original TU total per category is recorded so the
file is self-describing. The full per-TU file is reproducible via the survey
recipe in README.md.
"""
import json, sys

PER_TU = ("define_mismatch", "std_mismatch", "include_mismatch",
          "copt_mismatch", "gen_root_missing")


def sig(v):
    """Stable signature string for one TU's mismatch value."""
    if isinstance(v, dict):
        mis = tuple(sorted(v.get("missing_in_bazel") or []))
        ext = tuple(sorted(v.get("extra_in_bazel") or []))
        return json.dumps({"missing_in_bazel": list(mis),
                           "extra_in_bazel": list(ext)}, sort_keys=True)
    if isinstance(v, list):
        return json.dumps(sorted(v))
    return json.dumps(v)


def group(m):
    """dict[TU] -> {signature: {count, examples[<=5]}} histogram."""
    buckets = {}
    for tu, v in m.items():
        s = sig(v)
        b = buckets.setdefault(s, {"count": 0, "examples": []})
        b["count"] += 1
        if len(b["examples"]) < 5:
            b["examples"].append(tu)
    # emit as a list sorted by descending count
    return [{"signature": json.loads(s), "tu_count": b["count"],
             "examples": b["examples"]}
            for s, b in sorted(buckets.items(),
                               key=lambda kv: -kv[1]["count"])]


def main():
    src, dst = sys.argv[1], sys.argv[2]
    d = json.load(open(src))
    out = {"_compacted": True,
           "_note": "per-TU *_mismatch maps signature-grouped; see README.md"}
    for k, v in d.items():
        if k in PER_TU and isinstance(v, dict) and v:
            out[k + "_total_tus"] = len(v)
            out[k + "_by_signature"] = group(v)
        else:
            out[k] = v
    json.dump(out, open(dst, "w"), indent=1)
    print(f"{src}: {len(open(src).read())}B -> {dst}: {len(open(dst).read())}B")


if __name__ == "__main__":
    main()
