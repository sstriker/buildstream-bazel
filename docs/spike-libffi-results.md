# Spike: real autotools project (libffi) through the converter

**TL;DR**: the render layer accepts libffi cleanly; the actual build
isn't reachable on most sandboxes (no real bazel) but four
autotools-fidelity gaps are predictable from configure.ac /
Makefile.am static analysis. Listed below in priority order.

`tools/spike-libffi.sh` downloads libffi 3.4.6 on demand
(github releases; **source isn't vendored**, license stays
separate), generates a `.bst`, drives write-a + `bazel build`,
and prints a SUMMARY block keyed to whichever stage fails.
Run on a host with bazelisk + autoconf-chain to surface the
actual build failure; on minimal sandboxes the spike still
reports the static-analysis findings.

## What works

- **Source download**: github.com is reachable from sandboxed
  CI; ftp.gnu.org isn't. The spike pulls libffi from
  `github.com/libffi/libffi/releases/download/...`.
- **`.bst` generation + write-a render**: trivial. write-a
  doesn't care about source complexity — `kind:autotools` +
  `kind:local` + `path:` is enough.
- **B-side BUILD shape**: the `<element>_install` genrule
  renders correctly, with build-tracer + convert-element-
  autotools wired in, identical to the autotools-greet
  fixture.

## What breaks

In priority order (impact × likelihood):

### 1. Recursive automake (`SUBDIRS`) — high priority

libffi's top `Makefile.am`:

```
SUBDIRS = include testsuite man
SUBDIRS += doc
```

Top-level `make` recurses via `$(MAKE) -C include`; the inner
make's compile events have `cwd = $BUILD_ROOT/include`, and
the resulting `.o` paths are subdir-relative
(`../src/foo.o`-style or just `foo.o` from within the subdir).

Our converter's `objToCompile` map (`cmd/convert-element-autotools/main.go`)
keys by `ev.Out` (post our libtool-pic fix that switched from
basename to exact path). For sub-relative paths from
different `SUBDIRS`, the same `.o` basename can appear in
multiple subdirs, and we don't capture the `cwd` of each
event — so two `parent.o`s from `include/parent.o` and
`testsuite/parent.o` would collide.

**Fix sketch**: build-tracer's `emitExecve` doesn't currently
record `cwd`. Adding a `getcwd`-via-`/proc/<pid>/cwd` snapshot
per event lets the converter key by `(cwd, ev.Out)`.

### 2. Libtool wrapper — high priority

`LT_INIT` brings in `libtool` as a script wrapper; compile and
link go through

```
libtool --mode=compile cc -DHAVE_CONFIG_H -I... -c foo.c -o foo.lo
libtool --mode=link cc ... -o libffi.la foo.lo bar.lo ...
```

The trace will show `libtool` (sh script) → `cc -c foo.c -o
.libs/foo.o` (PIC compile, our existing libtool-pic fixture
covers this shape) **plus**:

- `.lo` files (libtool wrapper text files describing the
  PIC + non-PIC pair).
- `.la` files (libtool archive descriptors, listing real
  archive members + dep link order).
- Final shared object built via the `libtool --mode=link`
  driving `cc -shared` against `.libs/*.o`.

Our converter currently ignores `.lo` / `.la` because they're
not `.o` / `.a`. The cc rules it emits would link directly
against the underlying `.libs/*.o` (which works for static)
but won't reproduce libtool's shared-library output. For
elements where consumers `find_library(ffi)` expecting
`libffi.so`, this matters.

**Fix sketch**: detect libtool invocations in the trace
(execve of a script whose first line matches `^#!\s*/bin/sh.*libtool$`
or similar) and recognize that the cc/ar children are part of
a libtool-orchestrated build. Emit `cc_library(linkstatic = False)`
for `.la` outputs.

### 3. Generated headers (`config.h`) — medium priority

`AC_CONFIG_HEADERS([fficonfig.h])` makes configure emit
`fficonfig.h` from `fficonfig.h.in`. Compile commands look
like `cc -I. -DHAVE_CONFIG_H -include fficonfig.h ...`.

Our converter's `cc_library(srcs=[...])` handling treats `srcs`
as paths in the source tree. `fficonfig.h` only exists in the
build dir; if the converter looks it up by basename in the
source tree it'll miss. Today the source-tree-relative paths
are derived from compile event argv; for `-include
fficonfig.h` that resolves against `-I` paths, none of which
point at the source tree.

**Fix sketch**: the install-mapping.json output already
captures install-tree paths. Extend it (or a sibling output)
to capture build-dir-only generated files; the converter
includes them in `cc_library` `srcs` with a generated-file
genrule wiring upstream. **OR** treat them as "configure
output that's part of the static srckey" so they get baked
into project A and exposed to project B via the same staging
the install tree gets.

### 4. Arch-specific `.S` sources — medium priority

libffi has `src/<arch>/ffi.c` + `src/<arch>/sysv.S` selected
at configure time via `configure.host` based on `$host`. The
trace will show `cc -c src/x86/sysv.S -o src/x86/sysv.o`.

Our converter's `classifyCompilerDriver` accepts cc/gcc/clang
+ argv — it should already classify a `.S` compile correctly
(cc treats `.S` as "preprocess + assemble"). The `cc_library`
output gets `srcs = ["src/x86/sysv.S"]` which rules_cc
handles natively.

Likely-fine, but untested in our fixture corpus. Adding a
fixture with a `.S` source would close the loop.

### Smaller things probably-OK

- **`AC_CHECK_*` probes**: configure-time `AC_CHECK_HEADERS` /
  `AC_CHECK_FUNCS` runs `cc -E`/`cc -c` on host probes. These
  cc invocations show up in the trace but are configure-time,
  not build-time. `convert-element-autotools` should ignore
  them (they don't produce build artifacts). The current
  classifier filters by archive/link relationship — probes
  produce neither, so they fall through. Should-be-fine but
  worth verifying.
- **`pkg-config` exporter** (libffi.pc.in): libffi *generates*
  a `.pc` file but doesn't *consume* one. The generated `.pc`
  ends up in `$libdir/pkgconfig/`; install-mapping.json
  should capture it as a non-cc-rule install destination. No
  converter changes needed.

## Recommended next steps

In order:

1. **Add `cwd` to build-tracer's execve records.** Unblocks
   #1 (recursive automake). One-line addition to
   `emitExecve` + a few lines in the trace parser.
2. **Add a libffi e2e gate** that runs the spike script on
   a runner that has the autotools host chain. Wires the
   spike into the regular CI sweep so regressions surface.
3. **`.S` fixture.** Add `testdata/meta-project/autotools-asm/`
   that compiles a tiny `.S` alongside a `.c`. Closes #4
   without needing libffi.
4. **`config.h` fixture.** Add `testdata/meta-project/autotools-confh/`
   with a `config.h.in` and a `cc -DHAVE_CONFIG_H` compile.
   Closes #3.
5. **Libtool fixture using real libtool.** Today's
   autotools-libtool-pic mimics the dual-compile output but
   doesn't run libtool. Adding a fixture that invokes
   libtool for real surfaces #2 in a controlled way.

After 1 + 3 + 4 + 5 land, libffi end-to-end is a realistic
target.
