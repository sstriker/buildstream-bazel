#!/usr/bin/env python3
# stage A: read input.txt, write the doubled value to the intermediate.
import sys
with open(sys.argv[1]) as f:
    n = int(f.read().strip())
with open(sys.argv[2], "w") as f:
    f.write(str(n * 2))
