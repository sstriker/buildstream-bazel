#!/usr/bin/env python3
# A stand-in for grpc's protoc: it takes a `-I <root>` include root (used by
# protoc to map the file to its import name) and an input path, then writes a
# generated C source to --out=<dir>/<stem>.gen.c. Like protoc, the input PATH
# argument is read as a filesystem path (resolved from the cwd), and `-I` is the
# import-root flag — both must be re-anchored when the cd into the staging dir is
# stripped.
#
# The point is the `cd <build>/<sub> && gen.py -I . data/x.txt` shape: under cmake
# the tool runs cd'd into the configure-time-copied staging dir, so `-I .` and
# `data/x.txt` are cwd-relative. After cd-strip those reads must re-anchor to the
# byte-identical SOURCE-tree inputs (`-I <pkg>`, `<pkg>/data/x.txt`), not the
# empty Bazel exec-root cwd.
import os
import sys


def main(argv):
    inc = "."
    out_dir = "."
    src = None
    i = 1
    while i < len(argv):
        a = argv[i]
        if a == "-I":
            inc = argv[i + 1]
            i += 2
            continue
        if a.startswith("-I"):
            inc = a[2:]
            i += 1
            continue
        if a.startswith("--out="):
            out_dir = a[len("--out="):]
            i += 1
            continue
        src = a
        i += 1
    if src is None:
        sys.exit("no input given")
    # protoc requires the input to be under one of its -I roots; mirror that
    # check so a mis-anchored -I (e.g. the stranded `-I .`) is a hard error.
    if not os.path.abspath(src).startswith(os.path.abspath(inc) + os.sep):
        sys.exit("input %r is not under the -I root %r" % (src, inc))
    with open(src) as f:
        payload = f.read().strip()
    stem = os.path.basename(src).rsplit(".", 1)[0]
    os.makedirs(out_dir, exist_ok=True)
    with open(out_dir.rstrip("/") + "/" + stem + ".gen.c", "w") as f:
        f.write("int %s_value(void) { return %s; }\n" % (stem, payload))


if __name__ == "__main__":
    main(sys.argv)
