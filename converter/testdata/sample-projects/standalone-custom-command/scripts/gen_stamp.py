#!/usr/bin/env python3
"""Tiny stamp-writing script driving the Phase 4 standalone-genrule
emission render gate.

Writes a single-line marker to argv[1] with argv[2] as the literal
marker payload. The fixture's add_custom_command output is
consumed only by an add_custom_target — no cc_library / cc_binary
references it — so the converter's recoverGenrule path skips the
edge and only the standalone-genrule walker (Phase 4) emits a
genrule for it.
"""
import sys

dst, payload = sys.argv[1], sys.argv[2]
with open(dst, "w") as f:
    f.write(payload + "\n")
