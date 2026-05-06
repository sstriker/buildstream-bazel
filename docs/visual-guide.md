# Visual guide to cmake-to-bazel

A diagram-first tour of the codebase.  
For prose depth see [`docs/overview.md`](overview.md),
[`docs/three-pass-flow.md`](three-pass-flow.md), and
[`docs/build-structure.md`](build-structure.md).

---

## 1. What the tool does in one picture

```mermaid
flowchart LR
    BST["📦 .bst element graph\n(BuildStream project)"]
    WA["🔧 write-a\n(static renderer)"]
    A["🏗️ Project A\n(meta workspace)"]
    B["📐 Project B\n(consumer workspace)"]
    ART["🎯 Binaries / Libraries\n(native Bazel build)"]

    BST --> WA
    WA --> A
    WA --> B
    A -- "bazel build A\n(per-element conversions)" --> B
    B -- "bazel build B\n(real cc_library / cc_binary)" --> ART
```

`write-a` never builds anything itself — it only writes files.
Both workspaces are then built by ordinary `bazel build //...`.

---

## 2. Repository module map

```mermaid
graph TD
    subgraph Binaries["🛠 Binaries (cmd/)"]
        WA["cmd/write-a\nStatic renderer — reads .bst graph,\nwrites project A + B"]
        CE["converter/cmd/convert-element\nkind:cmake converter\ncmake File API → cc rules"]
        BT["cmd/build-tracer\nProcess tracer\nptrace / strace wrapper"]
        CEA["cmd/convert-element-autotools\nkind:autotools converter\nTrace + make-db → cc rules"]
        SP["cmd/source-push\nPushes source content to CAS"]
        TL["cmd/trace-lookup\nLooks up cached trace from registry"]
        TP["cmd/trace-publish\nPublishes trace to registry"]
    end

    subgraph SharedLibs["📚 Shared packages (internal/)"]
        CAS["internal/cas\nLocal content-addressable store\n+ REAPI CAS interface"]
        REAPI["internal/reapi\nREAPI Action submission\n(GRPCExecutor)"]
        MANIFEST["internal/manifest\nPer-element + run-level\nJSON schemas"]
        SHADOW["internal/shadow\nShadow-tree creator\n+ trace path parser"]
        SYNTH["internal/synthprefix\nPer-element CMAKE_PREFIX_PATH\nstub tree builder"]
        TRACENORM["internal/tracenorm\nTrace canonicalization\n(pid strip, gcc temp paths)"]
        FIDELITY["internal/fidelity\nSymbol + behavioral diff\n(test gate only)"]
    end

    subgraph ConverterInternals["🔬 cmake converter internals (converter/internal/)"]
        LOWER["lower\nFile API + ninja → IR\n(the conversion brain)"]
        FILEAPI["fileapi\nParse cmake codemodel-v2\ntoolchains-v1 cmakeFiles-v1"]
        NINJA["ninja\nbuild.ninja parser\n(genrule recovery)"]
        EMIT["emit/bazel\nIR → BUILD.bazel"]
        IR["ir\nPackage / Target / Source\n/ Genrule / ImportedTarget"]
        CMAKERUN["cmakerun\nRuns cmake --trace-expand\n+ drops File API stamps"]
        HERMETIC["hermetic\nbwrap argv builder\n+ env scrubbing"]
    end

    WA --> CAS
    WA --> MANIFEST
    WA --> SHADOW
    WA --> SYNTH
    CE --> LOWER
    CE --> FILEAPI
    CE --> NINJA
    CE --> EMIT
    CE --> CMAKERUN
    CE --> HERMETIC
    CE --> SHADOW
    CE --> SYNTH
    LOWER --> IR
    CEA --> TRACENORM
    CEA --> MANIFEST
    BT --> TRACENORM
    SP --> CAS
    TL --> CAS
    TP --> CAS
```

---

## 3. Two-pass workspace generation (write-a)

```mermaid
flowchart TB
    BST["🗂 .bst element files"]

    subgraph WA["cmd/write-a"]
        PARSE["Parse YAML\nbuild dep DAG"]
        DISPATCH["Per-kind dispatch\nkindHandler interface"]
        RA["RenderA(elem)\nwrites project-A package"]
        RB["RenderB(elem)\nwrites project-B package"]
    end

    subgraph PA["📁 Project A (meta workspace)"]
        PAMOD["MODULE.bazel"]
        PATOOLS["tools/\nconvert-element\nbuild-tracer\n..."]
        PABUILD["elements/<name>/BUILD.bazel\n(genrule invoking converter)"]
        PASRC["elements/<name>/sources/\n(staged source tree)"]
    end

    subgraph PB["📁 Project B (consumer workspace)"]
        PBMOD["MODULE.bazel\n(bazel_dep rules_cc)"]
        PBPH["elements/<name>/BUILD.bazel\n(placeholder)"]
        PBSRC["elements/<name>/\n(source files for cc_library)"]
    end

    BST --> PARSE
    PARSE --> DISPATCH
    DISPATCH --> RA & RB
    RA --> PA
    RB --> PB
```

For **kind:cmake** elements: after `bazel build //...` over project A, the
driver script stages each `BUILD.bazel.out` from project A into project B,
replacing the placeholder.  For **kind:autotools** round-2, project B's
install genrule is rendered in place from the start; project A's
`BUILD.bazel.out` is consumed differently — the operator stages or builds it
as a label once the round-2 trace is available from the AC.

---

## 4. Per-kind conversion paths

### 4a. kind:cmake — single pass

```mermaid
flowchart LR
    SRC["cmake source\n(staged in elements/<name>/sources/)"]
    STUB["Zero-byte stubs\n(narrowing — only real files\nare in action inputs)"]
    SHADOW_NODE["Shadow tree\n(real + zero stubs)"]
    CMAKECFG["cmake configure\n(File API stamps +\n--trace-expand)"]
    FILEAPI_NODE["File API reply\ncodemodel-v2\ntoolchains-v1\ncmakeFiles-v1"]
    TRACE["--trace-expand\nJSON trace"]
    NINJA_NODE["build.ninja\n(genrule recovery)"]
    LOWER_NODE["lower.go\n(IR construction)"]
    IR_NODE["IR: Package\nTarget Source\nGenrule ImportedTarget"]
    EMIT_NODE["emit/bazel\nBUILD.bazel.out"]
    BUNDLE["cmake-config-bundle.tar\n(synth prefix for consumers)"]

    SRC --> SHADOW_NODE
    STUB --> SHADOW_NODE
    SHADOW_NODE --> CMAKECFG
    CMAKECFG --> FILEAPI_NODE & TRACE & NINJA_NODE
    FILEAPI_NODE --> LOWER_NODE
    TRACE --> LOWER_NODE
    NINJA_NODE --> LOWER_NODE
    LOWER_NODE --> IR_NODE
    IR_NODE --> EMIT_NODE & BUNDLE
```

### 4b. kind:autotools — trace-driven (round 1 → round 2)

```mermaid
flowchart LR
    SRC2["autotools source"]
    BT2["build-tracer\n(ptrace / strace)"]
    CONFIGURE["./configure\nmake\nmake install"]
    INSTREE["install_tree.tar\n(DESTDIR install)"]
    TRACELOG["execve trace\n(text strace format,\ncanonicalized)"]
    MAKEDB["make -np output\n(make-db.txt)"]
    CEA2["convert-element-autotools\n(parse + correlate)"]
    BUILD_OUT["BUILD.bazel.out\n(cc_library / cc_binary)"]
    REG["AC entry under\nSyntheticActionDigest(srckey)\n(REAPI ActionCache)"]

    SRC2 --> BT2
    BT2 --> CONFIGURE
    CONFIGURE --> INSTREE
    BT2 --> TRACELOG
    CONFIGURE --> MAKEDB
    TRACELOG --> CEA2
    MAKEDB --> CEA2
    CEA2 --> BUILD_OUT
    BUILD_OUT --> REG
```

Round 2: on the next `write-a` render the registry hit means the
per-element action reads the stored trace instead of re-running
the full build — `.c`-only edits stay on the cheap path.

### 4c. Pipeline kinds (kind:make, kind:manual, kind:script, …)

```mermaid
flowchart LR
    PSRC["Source tree"]
    GR["install genrule\n(shell cmd from .bst variables)"]
    ITTAR["install_tree.tar\n(DESTDIR output)"]
    FG["Filegroup in project B\n(consumers unpack tar)"]

    PSRC --> GR --> ITTAR --> FG
```

No `BUILD.bazel.out` — consumers reference the install tree directly.

### 4d. Composition kinds (kind:stack, kind:filter, kind:compose, kind:import)

```mermaid
flowchart LR
    DEP_A["//elements/lib-a:lib-a_install"]
    DEP_B["//elements/lib-b:lib-b_install"]
    FG2["filegroup(name = 'runtime', srcs = [dep_a, dep_b])"]

    DEP_A --> FG2
    DEP_B --> FG2
```

No genrule at all — just starlark filegroup composition of dep elements.

---

## 5. Cross-element data flow (cmake deps)

```mermaid
flowchart LR
    subgraph PROD["Producer element (kind:cmake)"]
        PCONV["convert-element\n(emits cmake-config-bundle.tar)"]
        PBUNDLE["lib/cmake/Pkg/\nPkgConfig.cmake\n+ zero-byte IMPORTED_LOCATION stubs"]
    end

    subgraph CONS["Consumer element (kind:cmake)"]
        EXTRACT["$PREFIX extract\n(tar -xf bundle)"]
        CFIND["cmake find_package(Pkg CONFIG)\nresolves in $PREFIX"]
        CCONV["convert-element\n(--prefix-dir=$PREFIX\n--imports-manifest)"]
        CDEPS["BUILD.bazel.out\ndeps = [//elements/producer:producer, ...]"]
    end

    PCONV --> PBUNDLE
    PBUNDLE -. "cmake-config-bundle.tar\nlisted in consumer's genrule srcs" .-> EXTRACT
    EXTRACT --> CFIND
    CFIND --> CCONV
    CCONV --> CDEPS
```

For **autotools consumers**: cross-element resolution uses `imports.json`
(`-l<name>` link flags → `//elements/<name>:<name>` Bazel labels via
`manifest.LookupLinkLibrary`).

---

## 6. The three-pass (and five-pass) build flow

```mermaid
flowchart LR
    BST2[".bst graph"]

    subgraph P1["Pass 1 (cheap — seconds)"]
        WA2["write-a\nrenders project A"]
    end

    subgraph P2["Pass 2 (Bazel build A)"]
        BAZA["per-element genrules\nrun converters"]
        BBOUT["BUILD.bazel.out\nfor each element"]
    end

    subgraph P3["Pass 3 (Bazel build B)"]
        BAZB["cc_library / cc_binary\nnative compilation"]
        ART2["Artifacts"]
    end

    subgraph LOOP["Round-2 loop (autotools only)"]
        REG2["srckey → trace\nregistry (ActionCache)"]
        WA3["Pass 1': write-a re-render\n(registry hit → fine-graph)"]
        BA2["Pass 2': converter reads\nregistered trace"]
        BB2["Pass 3': incremental\ncc compile"]
    end

    BST2 --> P1 --> P2 --> P3
    P3 -- "cmake: done" --> ART2
    P3 -- "autotools: trace\nregistered" --> REG2
    REG2 --> WA3 --> BA2 --> BB2 --> ART2
```

| Pass | What runs | Cache layer |
|---|---|---|
| 1 — write-a | Go binary writes files | none — always fast |
| 2 — bazel build A | per-element converter genrules | Bazel ActionCache (buildbarn) |
| 3 — bazel build B | cc_library / cc_binary compile+link | Bazel ActionCache |
| 1' — write-a (registry hit) | re-render with trace available | none |
| 2' — bazel build A' | converter reads trace, emits fine graph | Bazel ActionCache |
| 3' — bazel build B' (kind:autotools round-2) | incremental cc compile of changed .c | Bazel ActionCache |

The full autotools build (pass 3 coarse genrule) runs **once per srckey**.
After that, `.c`-only edits stay on the cheap path permanently.

---

## 7. Srckey narrowing — what triggers a cache miss

```mermaid
flowchart TD
    EDIT["Developer edits a file"]

    EDIT --> Q1{"File kind?"}
    Q1 -- ".c / .cpp / .cc\n(graph-irrelevant)" --> CHEAP["srckey UNCHANGED\nRegistry HIT\nPass 2' → fine-graph\nPass 3' → incremental cc"]
    Q1 -- "configure.ac / *.am\n*.in / *.m4 / *.h\n(graph-affecting)" --> EXPENSIVE["srckey CHANGES\nRegistry MISS\nFull coarse build\nre-runs (pass 3)"]
    Q1 -- "CMakeLists.txt\n(cmake elements)" --> MEDIUM["cmake re-conversion\n(pass 2 cache miss)\n+ affected cc rules recompile"]
```

---

## 8. Caching and convergence

```mermaid
flowchart LR
    SRC3["Source bytes\n(CAS digest)"]
    TOOL["Translator binary\n(convert-element version)"]
    KEY["Bazel ActionCache key\n= hash(inputs + tool)"]
    RESULT["ActionResult\n(BUILD.bazel.out, bundles, …)"]
    REMOTE["Remote cache\n(buildbarn in CI;\nBazel local cache for dev)"]

    SRC3 --> KEY
    TOOL --> KEY
    KEY -- "lookup" --> REMOTE
    REMOTE -- "HIT: reuse result" --> RESULT
    KEY -- "MISS: run action" --> RESULT
    RESULT --> REMOTE
```

No tool-specific srckey registry for cmake — Bazel's ActionCache **is** the
convergence point. Same source + same toolchain + same converter version →
same action key → same outputs, shared across all builders automatically.

Autotools adds a separate registry layer because the coarse pass-3 genrule's
trace output needs to flow back into pass 1'/2' write-time decisions, which
happen before the ActionCache for pass 2 is consulted.

---

## 9. bb_clientd / CAS-aware source mount (optional)

```mermaid
flowchart LR
    CAS3["CAS endpoint\n(REAPI)"]
    BBC["bb_clientd daemon\n(FUSE mount +\nRemoteOutputService)"]
    FUSE["FUSE mount\n/cas/<instance>/blobs/…"]
    BAZEL9["Bazel 9\n--experimental_remote_output_service="]
    ACTION["Bazel action sandbox\n(sources stream in on demand)"]

    CAS3 --> BBC
    BBC --> FUSE
    BBC -- "RemoteOutputService protocol" --> BAZEL9
    BAZEL9 -- "trusts daemon-reported\ndigests (no re-hash)" --> ACTION
    FUSE --> ACTION
```

With bb_clientd, source bytes never land on the developer's disk —
actions stream them lazily through the FUSE mount. Without it, sources are
staged into `elements/<name>/sources/` on disk (the default dev path).

---

## 10. End-to-end gate map

```mermaid
flowchart TB
    subgraph DEV["Developer / CI gates (make …)"]
        UNIT["make test\nunit tests (no cmake required)\npre-recorded fixtures"]
        E2E_HELLO["make e2e-meta-hello\nhello-world fixture\nkind:cmake smoke test"]
        E2E_AUTO["make e2e-meta-autotools-native\nfull trace-driven autotools path"]
        E2E_MAKE["make e2e-meta-make-round2\nkind:make trace-driven path"]
        E2E_BB["make e2e-meta-autotools-round2-live\n(needs buildbarn + optionally bb_clientd)"]
        FDSDK["make fdsdk-reality-check\nsurveys full FDSDK graph (1092 elements)"]
    end

    subgraph CI["GitHub Actions CI"]
        UNIT2["job: unit"]
        E2E2["job: e2e\n(cmake + bwrap)"]
        BAZELE2E["job: bazel-e2e\n(bazel build downstream)"]
        BBE2E["job: buildbarn-e2e\n(docker compose cluster)"]
    end

    UNIT --> UNIT2
    E2E_HELLO --> E2E2
    E2E_AUTO --> E2E2
    E2E_BB --> BBE2E
    E2E_MAKE --> BAZELE2E
```

The four CI jobs shown (`unit`, `e2e`, `bazel-e2e`, `buildbarn-e2e`) are the
always-run jobs triggered on push/PR.  A fifth job, `fdsdk-probe` (FDSDK
reality-check survey), runs only on manual `workflow_dispatch` and is not
wired to any `make` target above.

---

## Further reading

| Where | What |
|---|---|
| [`README.md`](../README.md) | Quick start + repository layout |
| [`docs/overview.md`](overview.md) | Architecture in five minutes |
| [`docs/three-pass-flow.md`](three-pass-flow.md) | Detailed pass model + scenario walkthroughs |
| [`docs/build-structure.md`](build-structure.md) | Generated workspace interop contract |
| [`docs/architecture.md`](architecture.md) | Binary-level pipeline and package map |
| [`ROADMAP.md`](../ROADMAP.md) | What's shipped vs. what's next |
| [`CONTRIBUTING.md`](../CONTRIBUTING.md) | Dev-loop commands and test map |
