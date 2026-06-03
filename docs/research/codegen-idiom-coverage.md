# cmake codegen idiom coverage — recognize & natively lower the common set

Scope: which **codegen idioms** (custom commands / `cmake -P` scripts /
generator tools) recur often enough across cmake projects that the
converter should **recognize them and lower them to a native Bazel
shape**, rather than leaving them on the runner (`--cmake-script-runner`,
cmake-at-build-time) or bake (frozen) fallbacks.

This is bounded by the strategic North Star (`ROADMAP.md` "Now":
`cmake -G Bazel` / cmake-as-oracle). Native recognizers are worth
building only for **high-frequency** idioms; the long tail rides the
runner (the cheap early-transition bridge) until the oracle posture
subsumes it. Don't build a sprawling bespoke-recognizer library.

## Measuring frequency: the tags already exist

The converter already emits `cmake-codegen-*` tags for ~30 recognized
shapes. A **corpus-wide tag census** (run `scripts/run-survey.sh` over
the corpus + the two big stress members, tally tags + rejection `code`s)
is the empirical way to rank what to cover next — the instrumentation is
already in the output. Frequencies below are VTK-empirical +
corpus-informed; harden them with that census before committing build
order.

## Already natively lowered (don't reinvent)

These common idioms already have native lowerings — listed so the
"cover the common set" effort doesn't redo them:

- **`configure_file` / `file(GENERATE)`** → `cmake_configure_file` rule
  (the single most common codegen idiom). ✓
- **`install(EXPORT)`** → `cc_import` + `cmake_config_bundle`; **`install(FILES/DIRECTORY)`** → `pkg_files` (incl. RENAME + no-trailing-slash). ✓
- **`find_package`** → imports manifest / BCR module mapping. ✓
- **INTERFACE libraries** → `cc_library(hdrs, includes)`. ✓
- **TableGen-style generators** → genrule + the built tool, with the
  `.td`/depfile include closure and `file(GLOB)` threading (LLVM). ✓
- **Version stamps** (`git`/`hg`/`svn`/`date`/identity) → workspace-status
  stamp keys. ✓
- **Multi-language split, soversion, msvc-runtime, job-pool, sysroot,
  Fortran, language-override.** ✓

## The common idioms NOT yet natively lowered — the actionable set

Ranked by frequency × leverage. Each is currently runner/bake/tagged-only.

### 1. Embed file → C array / string  — **top priority**
- **What:** turn a file (shader, SQL, cert, template, resource) into a C
  string / `unsigned char[]` so it links into the binary. cmake projects
  do this with bespoke `cmake -P` encoders.
- **Seen as:** `vtkEncodeString.cmake` — **702** call sites in VTK alone
  (96% of VTK's whole rejection surface). The pattern recurs across LLVM,
  Qt (resources), game engines, embedded.
- **Today:** runner / bake only (no dedicated recognizer).
- **Native shape:** a repo `cc_embed` rule (Starlark + a tiny hermetic
  Go/C++ tool). Faithful where it matters — symbol name from the
  `-Doutput_name` arg, runtime string value = the input bytes; any correct
  escaping passes the nm fidelity gate. Parameters are already on the
  `cmake -P … -D…` command line. Must cover modes: string vs
  `BINARY`/hex, `NUL_TERMINATE`, export macros, ABI mangling (~50 lines).

### 2. Hash file → header  — **cheap, do alongside #1**
- **What:** MD5/SHA of a file emitted into a header constant.
- **Seen as:** `vtkHashSource.cmake` (VTK), version/integrity stamping.
- **Today:** runner / bake.
- **Native shape:** trivial `cc_hash` rule (hashlib in a tiny tool); the
  hash value is deterministic, so it's fully faithful.

### 3. Precompiled headers (`target_precompile_headers`)
- **What:** PCH for compile-time speedup.
- **Today:** recognized but **tagged only** (`cmake-codegen-pch`) — not
  lowered.
- **Frequency:** high in large C++ (VTK, Qt, KDE, LLVM-adjacent).
- **Native shape:** cc_library PCH via the toolchain's
  `parse_headers`/PCH features (operator cc_toolchain support) — or drop
  (PCH is a perf optimization; omitting it is correctness-neutral, so the
  cheap first move is to *not* refuse, just skip with the tag, which is
  the current state).

### 4. Qt AUTOMOC / AUTOUIC / AUTORCC
- **What:** Qt's meta-object / UI / resource compilers run by cmake's
  generator at build time.
- **Today:** **tagged only** ("Bazel cc_library has no native AUTOMOC
  equivalent"); operator must route via a kind:bazel override.
- **Frequency:** high within the Qt ecosystem (zero outside it).
- **Native shape:** moc/uic/rcc as host-tool genrules, or map to a
  rules_qt-style ruleset. Ecosystem-gated — do it when a Qt consumer
  lands, not speculatively.

### 5. Lex / Yacc / Bison / Flex (`.y` / `.l`)
- **Today:** partially recognized as generated sources
  (`genrule.go` handles `.y`/`.l` extensions).
- **Frequency:** medium (parsers/compilers/DSLs).
- **Native shape:** genrule invoking the bison/flex tool (or a dedicated
  rule); confirm the current `.y`/`.l` path actually wires the generator
  vs. just classifying the extension.

## Recommendation

1. Build **`cc_embed` (#1) + `cc_hash` (#2)** as one repo-rules PR — the
   highest-frequency, clearly-faithful, ecosystem-agnostic pair. This is
   the single biggest dent in the runner-served burn-down (VTK alone:
   705 → ~0 codegen sites off the runner).
2. Wire the **corpus-wide tag census** so the next idiom to cover is
   chosen by data, not guess.
3. Leave **PCH (#3)** as skip-with-tag (correctness-neutral) and **Qt
   (#4)** ecosystem-gated until a consumer needs them.
4. Hold the line per the North Star: cover the *common* set, runner the
   tail, don't gold-plate.
