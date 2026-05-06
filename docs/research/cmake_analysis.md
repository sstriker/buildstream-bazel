# CMake Configure Phase — Notes for a Starlark/Bazel Reimplementation

A single reference combining (a) what the configure phase actually does, source-level, and (b) where it fundamentally breaks under Starlark's evaluation model. Each section ends with a **→ Conversion** note giving the recommended transpilation strategy.

---

## 0. The two phases (and why they matter)

CMake runs in two passes:

1. **Configure phase.** Parse `CMakeLists.txt` top-to-bottom, executing each command immediately, mutating an in-memory model: directory tree, variable scopes, target registry, install rules, custom commands, cache. Source: `Source/cmMakefile.cxx`, `Source/cmGlobalGenerator.cxx`.
2. **Generation phase.** A second pass (`cmGlobalGenerator::Generate`) walks that model, evaluates generator expressions (`$<...>`), and writes Ninja/Make/MSBuild/Xcode files plus `cmake_install.cmake`.

**Critical fact: generator expressions are NOT evaluated at configure time.** `message("$<TARGET_FILE:foo>")` prints the literal string. Property values stored at configure time may contain unevaluated genex strings — that's intentional and load-bearing.

`cmake -P script.cmake` is *script mode*: no cache, no generation, target-defining commands are illegal.

→ **Conversion.** A faithful port needs a configure-phase interpreter producing an in-memory graph, then a separate genex-aware lowering pass that emits BUILD.bazel content. You cannot map CMake's two passes onto Starlark loading alone.

---

## 1. Parsing and execution model

CMake has **no AST**. `Source/cmListFileLexer.{c,in}` produces a flat token stream interpreted directly as command invocations. Three argument token types:

- **Bracket** `[=[ ... ]=]` — no expansion.
- **Quoted** `"..."` — expands `${}`, `$ENV{}`, `$CACHE{}`, `\` escapes.
- **Unquoted** — expands variable refs, then **list-splits on unescaped `;`**.

Unquoted argument expansion is the source of most surprises:

```cmake
set(x "a;b")
foo(${x})    # foo called with TWO args
foo("${x}")  # foo called with ONE arg
```

Commands are case-insensitive. There's no parse-time validation; every command parses its own argument shape in `Source/cm*Command.cxx`.

Execution is strictly source-order, top-down, including across `add_subdirectory()` (descends immediately) and `include()` (inlines into caller's scope).

→ **Conversion.** You need a lexer that preserves token type (bracket/quoted/unquoted) because `if()`, `foreach()`, and `${}` expansion semantics depend on it.

---

## 2. Variable system & scope

**Three namespaces, three syntaxes.** `${X}` = function/macro local → directory scope → cache (in that priority). `$CACHE{X}` = cache only. `$ENV{X}` = process env. References nest inside-out: `${${name}}`. Source: `cmMakefile::ExpandVariablesInString`.

**Directory scope.** Each `add_subdirectory()` snapshots parent's normal-variable bindings (copy-on-write). Mutations don't propagate up unless via `set(... PARENT_SCOPE)`. The cache is global.

**Function vs macro — the worst footgun in CMake.**

- `function()` opens a new variable scope; only `set(... PARENT_SCOPE)` survives return.
- `macro()` is **textual substitution**: no scope, `${ARGN}/${ARGV}/${ARGC}` are pre-substituted into the body before evaluation. Consequences:
  - `if(DEFINED ARGV2)` always false in macros.
  - `foreach(x IN LISTS ARGN)` only works in functions.
  - `return()` inside a macro returns from the **caller**.
  - Caller variables shadowing macro params silently rebind.

```cmake
macro(append_flag f)
  list(APPEND CFLAGS ${f})    # mutates CALLER's CFLAGS
endmacro()
function(append_flag_fn f)
  list(APPEND CFLAGS ${f})    # local; lost on return
endfunction()
```

**`set` variants.** `set(X v)` — normal in current scope. `set(X v PARENT_SCOPE)` — **parent scope only, current unchanged**. `set(X v CACHE TYPE "doc")` — only writes if absent. `FORCE` always overwrites. `set(ENV{X} v)` — mutates this CMake process's env only.

**Cache vs normal shadowing.** When both exist, `${X}` returns the **normal** variable. **CMP0126**: under OLD `set(... CACHE ...)` removed the same-named normal var; under NEW the normal var keeps shadowing. Use `$CACHE{X}` to disambiguate.

**`block()`** (3.25+): scope without function call overhead. `PROPAGATE v1 v2` lifts vars out on exit.

**Double-evaluation in `if()`** — see §3.

→ **Conversion.** Lower `${X}` to a name lookup against an explicit three-layer mapping `(scope_normal, scope_cache, env_snapshot)`. Refuse `set(... CACHE ...)` shared writes; require explicit declarative cache overrides via repository rules. **Detect `macro(...)` syntactically** and either inline-expand at every call site or emit a function returning a "scope deltas" dict the caller merges. Refuse macros that use `return()` or read undeclared caller variables.

---

## 3. `if()` — the type-coerced auto-deref nightmare

`if(<word>)` first checks for a defined variable named `<word>` and substitutes its value, then re-runs constant evaluation. Truthiness: case-insensitive `1/ON/YES/TRUE/Y` or non-zero number is true; `0/OFF/NO/FALSE/N/IGNORE/NOTFOUND/""` or `*-NOTFOUND` is false. There are **six** truthiness regimes: number, version-string, boolean-keyword, defined-name, target-name, test-name.

**Operators**: parens, then unary tests (`COMMAND`, `POLICY`, `TARGET`, `TEST`, `EXISTS`, `IS_DIRECTORY`, `IS_SYMLINK`, `DEFINED`, `DEFINED CACHE{...}`, `DEFINED ENV{...}`); binary (`EQUAL/LESS/GREATER`, `STREQUAL`, `VERSION_*`, `MATCHES`, `IN_LIST`, `IS_NEWER_THAN`); `NOT`; `AND`/`OR` left-to-right with **no short-circuit** — `if(DEFINED X AND ${X} STREQUAL "foo")` dereferences `${X}` even when undefined.

**CMP0054** (NEW since 3.1): quoted/bracket arguments are NOT dereferenced. Always require NEW.

```cmake
set(A B)
set(B "")
if(A)            # dereferences A->"B", then B->"" -> false  (NEW)
if(${A})         # expands to if(B) -> if("") -> false
if("${A}")       # quoted: literal "B" -> nonzero -> true under CMP0054 NEW
                 # but under OLD: dereferences again -> "B" -> empty -> false
```

`MATCHES` populates `CMAKE_MATCH_<n>` (0–9) and `CMAKE_MATCH_COUNT`.

→ **Conversion.** **Do not lower to Starlark `if`.** Implement `cmake_if(tokens, scope, policies)` as an interpreter call. Carry policy state per call site. Refuse projects without `cmake_minimum_required(VERSION 3.1+)` — too many silent CMP0054 flips.

---

## 4. Control flow

**`foreach`**:
- `foreach(i RANGE stop)` — 0..stop **inclusive**.
- `foreach(i RANGE start stop [step])`.
- `foreach(i item1 item2 ...)`.
- `foreach(i IN LISTS A B [ITEMS x y])` — concatenates list-vars and items.
- `foreach(i IN ZIP_LISTS A B)` (3.17+) — multi-list zipped iteration; `i_0`, `i_1` per round.
- Loop var is loop-scoped under CMP0124.

**`while/break/continue`** — straightforward; condition uses `if()` evaluator.

**`return(PROPAGATE v1 v2)`** (3.25+) lifts named vars across enclosing `block()` scopes back to the function caller.

**`cmake_language`**:
- `EVAL CODE "..."` — string-eval CMake code in caller scope, immediate.
- `CALL <name> args...` — dynamic dispatch; cannot call control flow.
- `DEFER [DIRECTORY d] [ID id] CALL <name> args...` — schedules a call to run at end-of-directory. **Variable references in deferred args are evaluated at deferred-call time, not scheduling time.** `GET_CALL_IDS`/`GET_CALL`/`CANCEL_CALL` for introspection.

**`include()` vs `find_package()` vs `add_subdirectory()`**:
- `include(file)` — **inlines into caller scope** (dynamic scoping). Pushes a policy scope unless `NO_POLICY_SCOPE`.
- `find_package(P)` — see §6.
- `add_subdirectory(d)` — opens new directory scope (snapshot copy of vars, separate property layer).
- `include_guard([DIRECTORY|GLOBAL])` — variable-scope by default.

→ **Conversion.** `include()` ≠ Starlark `load()`. Lower to a function call that explicitly threads the mutable scope object. Honor `NO_POLICY_SCOPE`. **`cmake_language(DEFER)` has no Starlark analog** — refuse general use; permit only the trivial case (constant args, no inter-directory deferral).

---

## 5. Targets at configure time

**Kinds.** `add_executable`, `add_library [STATIC|SHARED|MODULE|OBJECT|INTERFACE|IMPORTED|ALIAS]`, `add_custom_target`. IMPORTED targets are directory-scoped by default; `GLOBAL` (or `find_package(... GLOBAL)` in 3.24+) hoists them. ALIAS is read-only and cannot be installed/exported.

**`target_*()` propagation.** `target_link_libraries`, `target_include_directories`, `target_compile_definitions`, `target_compile_options`, `target_compile_features`, `target_link_options`, `target_link_directories`, `target_precompile_headers`, `target_sources`. Each takes `PUBLIC | PRIVATE | INTERFACE`:

- `PRIVATE` → build-spec property only.
- `INTERFACE` → `INTERFACE_*` only (consumed by transitive users).
- `PUBLIC` → both.

For LINK_LIBRARIES, the linker line is computed by walking `INTERFACE_LINK_LIBRARIES` recursively with PRIVATE-cuts and de-duplication.

**Property layers** (lookup order in `get_property`): Target → Directory → Global, with explicit `INHERITED` opt-in. Source-file properties are stored on **directory scope keyed by file path** — same filename in another directory has independent properties.

**`BUILD_INTERFACE` vs `INSTALL_INTERFACE`**:

```cmake
target_include_directories(foo PUBLIC
  $<BUILD_INTERFACE:${CMAKE_CURRENT_SOURCE_DIR}/include>
  $<INSTALL_INTERFACE:include>)
```

At `install(EXPORT)` / `export()`, `BUILD_INTERFACE` evaluates empty, `INSTALL_INTERFACE` expands relative to `CMAKE_INSTALL_PREFIX`. Inside the build tree the reverse holds.

→ **Conversion.** Map IMPORTED libraries to `cc_import` and STATIC/SHARED/INTERFACE to `cc_library` (with `linkstatic`/`alwayslink` knobs). Carry transitive usage requirements via providers, not Bazel string attributes — the closure walk needs explicit handling. `BUILD_INTERFACE`/`INSTALL_INTERFACE` requires emitting two distinct rule outputs (build-tree CcInfo vs an installable package config).

---

## 6. `find_package`

**Two modes**:
- **Module mode**: searches `CMAKE_MODULE_PATH` then bundled `Modules/Find<P>.cmake`. Find module does detection (typically `find_path`/`find_library`); not shipped by the package.
- **Config mode**: searches for `<P>Config.cmake` or `<p>-config.cmake` shipped by the package. Declarative imported targets.

Default: try module, fall back to config. `MODULE`/`CONFIG`/`NO_MODULE` force one mode. `CMAKE_FIND_PACKAGE_PREFER_CONFIG=ON` reverses default.

**Search path order** (config mode):
1. `<PackageName>_ROOT` var and env.
2. Cache: `CMAKE_PREFIX_PATH`, `CMAKE_FRAMEWORK_PATH`, `CMAKE_APPBUNDLE_PATH`.
3. Same as env vars.
4. `HINTS`.
5. System `PATH` (with `bin/sbin` stripped).
6. User package registry (`~/.cmake/packages/<P>` on Unix, registry on Windows).
7. `CMAKE_SYSTEM_PREFIX_PATH`, `CMAKE_INSTALL_PREFIX`.
8. System package registry.
9. `PATHS`.

Each prefix is searched at `<prefix>/`, `<prefix>/(cmake|CMake)/`, `<prefix>/(lib|lib64|share)/cmake/<name>*/`, framework/bundle dirs on macOS.

**`CMAKE_FIND_ROOT_PATH`** — restricts to a sysroot when cross-compiling. `CMAKE_FIND_ROOT_PATH_MODE_LIBRARY/INCLUDE/PROGRAM/PACKAGE` per-class: `ONLY`/`NEVER`/`BOTH`.

**Components.** `COMPONENTS Foo Bar OPTIONAL_COMPONENTS Baz` — sets `<P>_FIND_COMPONENTS` and `<P>_FIND_REQUIRED_<comp>`.

**Version selection.** `find_package(Foo 1.2.3)` or `find_package(Foo 1.2...<2.0)` (range, 3.19+). The package's `<P>ConfigVersion.cmake` is invoked with `PACKAGE_FIND_VERSION` set; must set `PACKAGE_VERSION_COMPATIBLE`/`_EXACT`/`_UNSUITABLE`.

**State left behind.** `<P>_FOUND`, `<P>_VERSION[_*]`, `<P>_CONFIG`, `<P>_DIR` (cached), plus imported targets like `Foo::Foo`.

**`find_library`/`find_program`/`find_path`/`find_file`** — all cache. Once cached, **subsequent calls do not re-search**; force with `unset(VAR CACHE)` first. `NAMES`, `PATHS`, `HINTS`, `PATH_SUFFIXES`, `NO_DEFAULT_PATH`, per-class `NO_*_PATH`, `REQUIRED` (3.18+).

→ **Conversion.** `find_package` is the canonical hermeticity violation: env reads, registry reads, filesystem walk, persistent caching. Three options: (a) replace each `find_package` with a `bazel_dep` + `MODULE.bazel` extension that vendors specific versions; (b) precompute a per-project package registry in a repo rule into a `.bzl` table; (c) for sources only built via foreign tools, use `rules_foreign_cc`. Default behavior of the transpiler: emit a stub that fails loudly with a TODO referencing the package.

---

## 7. `try_compile` / `try_run` / `check_*` — the structural blocker

**`try_compile`** spawns a sub-CMake. Generates a unique dir under `${CMAKE_BINARY_DIR}/CMakeFiles/CMakeScratch/TryCompile-XXXXXX/`, writes a synthetic `CMakeLists.txt`, runs a fresh CMake configure+build cycle. `--debug-trycompile` preserves the dirs. Source: `Source/cmCoreTryCompile.cxx`.

**State propagated** (3.24+): `CMAKE_TRY_COMPILE_PLATFORM_VARIABLES` list, `CMAKE_<LANG>_STANDARD/_REQUIRED/_EXTENSIONS` (CMP0067 NEW), `CMAKE_MSVC_RUNTIME_LIBRARY`, link flags, PIC. **`CMAKE_TRY_COMPILE_TARGET_TYPE=STATIC_LIBRARY`** skips link — required for cross-toolchains that can't link a host-side executable.

**Caching.** Result variable is a cache entry. Idiomatic guard:
```cmake
if(NOT DEFINED HAVE_FOO)
  try_compile(HAVE_FOO ...)
endif()
```
`NO_CACHE` (3.25+) makes it a normal variable.

**`try_run`** — try_compile + run-the-binary. Sets `<run_var>`, `<run_var>_OUTPUT`. When `CMAKE_CROSSCOMPILING` is true and no `CMAKE_CROSSCOMPILING_EMULATOR`, run is skipped → user must pre-seed cache values.

**`Check*`** — `CheckIncludeFile`, `CheckSymbolExists`, `CheckFunctionExists`, `CheckCSourceCompiles`, `CheckCSourceRuns`, `CheckCXXCompilerFlag`, `CheckTypeSize`, etc. All wrap try_compile/try_run, write a tiny test program, cache a boolean. Consume `CMAKE_REQUIRED_FLAGS/DEFINITIONS/INCLUDES/LIBRARIES/LINK_OPTIONS/QUIET`.

→ **Conversion. THIS IS THE STRUCTURAL BLOCKER.** Bazel's analysis phase forbids action execution; the analysis phase strictly precedes execution; **you cannot feed action outputs back into analysis.** Three workable patterns:

1. **Pre-compute and check in.** Repository rule runs the compiler probe once per `(host, target)` key, materialises the answer into a generated `config.h` or `.bzl` constants file. Analysis-phase code reads it as static input.
2. **Lower to `select()` with hand-curated keys.** If outcome is fully determined by `(os, cpu, libc, compiler)`, emit `select` over `config_setting`s with each branch's expected answer.
3. **Run a configure action.** Emit an action that runs `cmake --configure` and produces `config.h` as a build output. Downside: any **target-graph decisions** that hinged on the probe (which sources to compile, which deps to link) cannot be decided here.

**Refuse projects that key target-graph decisions on `try_compile` results.** For projects that only key `#define`s on probes, generate a per-platform repo rule that runs them once and writes a `.bzl` table. Cross-compile `try_run` is essentially impossible to reproduce hermetically.

---

## 8. `configure_file` & `file()`

**`configure_file(<input> <output> [options])`** — reads input, performs `@VAR@` and `${VAR}` substitution, writes output only if changed (preserves mtime). `#cmakedefine FOO` → `#define FOO` if truthy else `/* #undef FOO */`. `#cmakedefine01 FOO` → `#define FOO 1`/`0`. `@ONLY` restricts to `@VAR@` (preserves `${...}` for shell). `COPYONLY` skips substitution. `NEWLINE_STYLE UNIX|DOS|...`. **Triggers automatic rerun** on input change.

**`file(READ/WRITE/APPEND)`** — synchronous configure-time I/O. Don't use `WRITE` for build inputs (writes on every reconfigure, defeating change detection); use `configure_file` instead.

**`file(GLOB)` / `file(GLOB_RECURSE)`** — runs once at configure. New files are silently ignored until re-configure. `CONFIGURE_DEPENDS` (3.12+) re-globs at build time on Ninja/Makefiles, but [docs explicitly caution it "may not work reliably"](https://cmake.org/cmake/help/latest/command/file.html#filesystem). Idiomatic CMake: list sources explicitly.

**`file(GENERATE OUTPUT path CONTENT "..." [CONDITION ...] [TARGET t])`** — runs at **generation time**; full genex support; one file per config in multi-config. The output does not exist when the command returns at configure time.

**`file(CONFIGURE OUTPUT ... CONTENT "..." [@ONLY])`** (3.18+) — like `configure_file` with inline content; configure-time, no genexes.

**`file(DOWNLOAD url file [HASH ...] [TLS_VERIFY ON] [TIMEOUT n])`** — network at configure time.

**`file(REAL_PATH path out)`** — symlink resolution; hits filesystem.

Other configure-time `file()` ops: `COPY`, `INSTALL`, `RENAME`, `REMOVE`, `MAKE_DIRECTORY`, `TOUCH`, `SIZE`, `LOCK`, `STRINGS`, `TO_CMAKE_PATH`, `TO_NATIVE_PATH`.

→ **Conversion.** `file(GLOB)` → `glob([...])` at loading time (Bazel tracks inputs deterministically). Refuse globs mixing `${CMAKE_BINARY_DIR}` paths. Drop `CONFIGURE_DEPENDS`. `configure_file` → `expand_template` rule + a small `#cmakedefine` helper. `file(GENERATE)` with genexes → custom rule emitting files in actions. `file(DOWNLOAD)` → `repository_rule`.

---

## 9. `execute_process`

```
execute_process(COMMAND a... [COMMAND b...] [WORKING_DIRECTORY d] [TIMEOUT s]
  [RESULT_VARIABLE r] [RESULTS_VARIABLE rs] [OUTPUT_VARIABLE o] [ERROR_VARIABLE e]
  [INPUT_FILE/OUTPUT_FILE/ERROR_FILE f]
  [OUTPUT_QUIET/ERROR_QUIET]
  [OUTPUT_STRIP_TRAILING_WHITESPACE/ERROR_STRIP_TRAILING_WHITESPACE]
  [COMMAND_ERROR_IS_FATAL ANY|LAST]
  [COMMAND_ECHO STDERR|STDOUT|NONE]
  [ENCODING ...]
  [ENVIRONMENT k=v ...] [ENVIRONMENT_MODIFICATION ops])
```

Multiple `COMMAND` form a **pipeline** (concurrent, shared stderr). Sequential commands need separate calls. `RESULT_VARIABLE` = last process exit; `RESULTS_VARIABLE` = all. **Synchronous, configure-time.** For build-time, use `add_custom_command`/`add_custom_target` instead.

→ **Conversion.** Canonical hermeticity violation. Only legitimate analog: `repository_ctx.execute()` in a repo rule (loading/fetch time). Anything informing target-graph shape (e.g. `git rev-parse HEAD` for a version macro) must lift into a repo rule producing a generated `.bzl` or stamped header.

---

## 10. `project()` and toolchain detection

**`project(name [VERSION x.y.z] [DESCRIPTION ...] [LANGUAGES C CXX ...])`**, first call effects:

1. Reads `CMAKE_TOOLCHAIN_FILE` if not yet read.
2. Sets `CMAKE_HOST_SYSTEM_NAME/_PROCESSOR/_VERSION`.
3. Sets `CMAKE_SYSTEM_NAME/_PROCESSOR/_VERSION` from toolchain or host.
4. Sets `CMAKE_CROSSCOMPILING` if SYSTEM differs from HOST.
5. Sets `PROJECT_NAME`, `CMAKE_PROJECT_NAME`, `<name>_SOURCE_DIR/_BINARY_DIR`, `PROJECT_VERSION[_*]`, `PROJECT_IS_TOP_LEVEL`.
6. Includes `CMAKE_PROJECT_INCLUDE_BEFORE`, `CMAKE_PROJECT_<name>_INCLUDE_BEFORE`.
7. **Enables languages** (default C, CXX). For each:
   - `Modules/CMakeDetermine<LANG>Compiler.cmake` (locates compiler).
   - `Modules/CMakeTest<LANG>Compiler.cmake` (try_compile a tiny program).
   - `Modules/CMake<LANG>Information.cmake` (loads `Platform/<sys>.cmake`, `Compiler/<id>-<lang>.cmake`).
   - Populates `CMAKE_<LANG>_COMPILER_ID`, `_VERSION`, `_ARCHITECTURE_ID`, `_LOADED`, `_FLAGS`.
8. Includes `CMAKE_PROJECT_INCLUDE`, `CMAKE_PROJECT_<name>_INCLUDE`.

Compiler ID detection (`Modules/CMakeDetermineCompilerId.cmake`): compiles a tiny program peppered with `__GNUC__`/`_MSC_VER` macros emitting known string patterns; the binary is parsed for `INFO:compiler[GNU]` etc.

`enable_language(LANG)` runs the same language-enable step; idempotent.

**Cross-compilation** requires at minimum `CMAKE_SYSTEM_NAME` (any non-empty value sets `CMAKE_CROSSCOMPILING=TRUE`); typically also `_PROCESSOR`, `CMAKE_<LANG>_COMPILER`, `CMAKE_FIND_ROOT_PATH`, per-class `CMAKE_FIND_ROOT_PATH_MODE_*`.

**`cmake_minimum_required(VERSION 3.20)`** must precede `project()` — toolchain detection reads policies.

→ **Conversion.** Don't transpile compiler detection. Map to `toolchains = ["@bazel_tools//tools/cpp:toolchain_type"]` and read flags off `cc_common` in rule impls. Map `CMAKE_CXX_COMPILER_ID`, `CMAKE_SYSTEM_PROCESSOR` to `config_setting`s over `@platforms//cpu:*`, `@bazel_tools//tools/cpp:compiler`. If autodetection is genuinely needed, run it in a repo rule once and persist the resulting toolchain definitions.

---

## 11. CMakeCache.txt

Persistent k-v store at `<build>/CMakeCache.txt`, written by `cmGlobalGenerator::WriteCMakeCache()`, reloaded each configure run. Each entry: `KEY:TYPE=VALUE` with comment-line docstring.

**Types**: `BOOL`, `PATH`, `FILEPATH`, `STRING`, `INTERNAL`, `STATIC`, `UNINITIALIZED`. INTERNAL hidden from GUIs and forced. `mark_as_advanced` is a separate property.

**Population priority**:
1. `-DKEY=VALUE` on command line (creates `UNINITIALIZED` entry).
2. `-C initial-cache.cmake`.
3. Toolchain file.
4. `set(... CACHE ...)` in CMakeLists — only if absent unless `FORCE`.
5. Cache-writing commands (`option`, `find_*`, `try_compile`, `check_*`, `mark_as_advanced`).

**`-D` semantics.** Bare `-DFOO=bar` writes type `UNINITIALIZED`. Subsequent `set(FOO ... CACHE STRING "")` adopts the value but upgrades the type. `-DFOO:BOOL=ON` fixes type at command line.

**Reconfigure non-idempotency.** On every `cmake -B build`, the cache is loaded **first**, then CMakeLists runs. Therefore:
- `find_*` calls skip work because results are cached.
- `try_compile`/`check_*` skip if result var already in cache.
- `set(X v CACHE STRING "")` is a no-op if cache has X.
- `option(USE_FOO ON)` only sets cache on first run — flipping the source default is invisible later.
- Removing a CMakeLists line that wrote a cache var **does not unset the cache var**.
- `--fresh` (3.24+) deletes `CMakeCache.txt` and `CMakeFiles/`.

→ **Conversion.** Bazel has no analog. Treat user-editable cache as out-of-scope; require a declarative cache-overrides file driving a repo rule. Lower `option()` to `bool_flag` / `config_setting` if it actually drives `select()`; otherwise resolve once and bake.

---

## 12. Policies (CMP####)

Each policy: `NEW`, `OLD`, or **unset** (warns and behaves like OLD). Policy decisions are **scoped**: every directory, function, and `include()` (without `NO_POLICY_SCOPE`) pushes a stack frame, popped on exit. `cmake_policy(PUSH/POP/SET/VERSION/GET)`.

**`cmake_minimum_required(VERSION 3.20)`** is essentially `cmake_policy(VERSION 3.20)` plus a version assertion: sets every policy up to and including 3.20 to NEW.

**Per-policy command-line override**: `-DCMAKE_POLICY_DEFAULT_CMP0077=NEW`.

**Lifecycle**: introduced (OLD warns) → ≥2y deprecation on OLD → ≥6y / major bump: OLD removed, becomes a hard error. CMake 4.0 removed many pre-3.5 policies; setting them to OLD now errors.

**Most aggressively semantics-changing for a transpiler**:
- **CMP0054** — quoted args in `if()` not dereferenced. Flips truthiness silently.
- **CMP0057** — `IN_LIST` recognized. OLD: syntax error or string treatment.
- **CMP0067** — `try_compile` honors `CMAKE_<LANG>_STANDARD/_REQUIRED/_EXTENSIONS`.
- **CMP0077** — `option()` honours pre-existing normal vars. Critical for parent-project overrides on embedded subprojects (huge for `FetchContent`).
- **CMP0079** — cross-directory `target_link_libraries`.
- **CMP0099** — cross-config link interface dedup.
- **CMP0116** — `add_custom_command(DEPFILE)` paths rewritten relative to current binary dir. OLD silently produces wrong dependency tracking.
- **CMP0118** — source-file properties scoping.
- **CMP0126** — `set(CACHE)` no longer removes normal var.

→ **Conversion.** **Not a flat global flag** — frame-scoped, inheritable. Maintain a per-call-site policy version table during transpile; dispatch each command's implementation by active policy. Don't try to be "policy-agnostic" — `cmake_minimum_required` is load-bearing.

---

## 13. `install()` / `export()`

**Configure-time recording only.** `install(...)` appends to a per-directory install-rule list. `cmGlobalGenerator::Generate` materialises them into `<build>/cmake_install.cmake`. Actual file copies happen at **install time** (`cmake --install build`).

**`install(TARGETS t1 t2 ...)`** dispatches per artifact kind (RUNTIME, LIBRARY, ARCHIVE, OBJECTS, FRAMEWORK, BUNDLE, PUBLIC_HEADER, PRIVATE_HEADER, RESOURCE, FILE_SET, CXX_MODULES_BMI). Each kind has its own `DESTINATION`, `COMPONENT`, `PERMISSIONS`, `CONFIGURATIONS`, `EXCLUDE_FROM_ALL`, `OPTIONAL`, `NAMELINK_*`. Defaults from `GNUInstallDirs`.

**`install(EXPORT export-name DESTINATION ... [NAMESPACE NS::] [FILE name.cmake])`** — generates `<export-name>.cmake` (per-config files too) at install time, containing `add_library(NS::tgt IMPORTED)` and `set_target_properties(... INTERFACE_*)` reflecting target's interface properties. `$<INSTALL_INTERFACE:...>` resolved against `${_IMPORT_PREFIX}` (computed inside the generated file).

**`export(TARGETS|EXPORT ...)`** — same machinery for the **build tree** so consumers can `find_package` an un-installed build. `BUILD_INTERFACE` kept; `INSTALL_INTERFACE` dropped.

**`CMAKE_INSTALL_PREFIX`** default: `/usr/local` (Unix), `C:/Program Files/<project>` (Windows).

**`DESTDIR`** env var consulted only at install time — every destination prefixed by `$DESTDIR`. For staged packaging. Not used on Windows.

**`GNUInstallDirs`** — `CMAKE_INSTALL_BINDIR/SBINDIR/LIBEXECDIR/SYSCONFDIR/LIBDIR/INCLUDEDIR/DATAROOTDIR/DATADIR/MANDIR/DOCDIR` etc., with multiarch-aware LIBDIR (e.g. `lib/x86_64-linux-gnu`).

**Crucial pitfall: `*Config.cmake` files are programs.** They may set arbitrary variables, branch on `CMAKE_HOST_SYSTEM_NAME`, define imported targets with genexes in interface props, call `find_dependency` (recursively `find_package`), and check policies. Consuming a CMake-installed package therefore **requires executing CMake-language code at consumer configure time**.

→ **Conversion.** Bazel has no install concept; outputs live in `bazel-bin/`. Closest analog: `pkg_tar`/`pkg_files` for the artifact, plus separate generation of `<P>Targets.cmake`-equivalent files for downstream CMake consumers. For consumption: a hybrid converter runs real CMake against the published Config files in a repo rule and dumps resolved imported targets to JSON. A pure-Starlark converter must restrict consumption to a known whitelist of well-behaved Config files (basically: `CMakePackageConfigHelpers`-generated, no custom logic).

---

## 14. `ExternalProject_Add` and `FetchContent`

**`ExternalProject_Add`** — creates a custom target with sub-steps (mkdir → download → update → patch → configure → build → install → test), each `add_custom_command` writing a stamp file. **All steps run at build time, not configure time.** The configure step of the inner project is itself a build-time CMake invocation. → at the end of your top-level configure you don't yet know what the external project will export. Users typically combine with a second top-level `find_package` invocation in a separate project.

**`FetchContent`** sits on top of `ExternalProject` but downloads at **configure time** (sub-CMake invoked synchronously with everything except download/update/patch disabled). Flow:
- `FetchContent_Declare(name URL ... | GIT_REPOSITORY ... GIT_TAG ...)` — records details only; **first-to-declare wins**, so parents override children.
- `FetchContent_MakeAvailable(name1 name2 ...)` (3.14+) — tries dependency providers (3.24+) → `find_package` (with `FETCHCONTENT_TRY_FIND_PACKAGE_MODE`) → downloads via `ExternalProject_Add` (configure-time sub-build) → `add_subdirectory(<name>-src <name>-build)` so fetched targets become part of *your* configure model.
- Sets `<lowercaseName>_SOURCE_DIR/_BINARY_DIR/_POPULATED`.
- `FETCHCONTENT_BASE_DIR` defaults to `${CMAKE_BINARY_DIR}/_deps`.
- `FETCHCONTENT_SOURCE_DIR_<UPPERCASENAME>` skips download, points at developer tree.
- `FETCHCONTENT_FULLY_DISCONNECTED` / `_UPDATES_DISCONNECTED` for offline.

→ **Conversion.** Both perform network I/O. Bazel forbids network in actions and in analysis. Map:
- `FetchContent_Declare(URL ... HASH ...)` → `http_archive` in `MODULE.bazel` extension (loading phase, hermetic if hash given).
- `FetchContent_Declare(GIT_REPOSITORY)` → `git_repository`.
- `FetchContent_MakeAvailable` → loading the resulting external repo + its `BUILD.bazel`.
- `ExternalProject_Add` with non-CMake configure → `rules_foreign_cc` (`configure_make`, `cmake`, `meson`, `ninja_build`); copies sources into a sandbox, invokes the external build inside a Bazel action, with limited network access.

**Mismatches**: Bazel demands content hashes; FetchContent doesn't. Bazel forbids re-fetch based on configure logic, but FetchContent often computes URLs/tags from CMake variables (refuse those). Per-platform conditional fetches must move into `select()` over `http_archive` aliases or a `module_extension`. `FetchContent`'s `add_subdirectory` model means inner targets share the outer configure model — `rules_foreign_cc` cannot replicate that and produces opaque artifacts.

---

## 15. Generator selection & generator expressions

**`-G <generator>`** → `Ninja`, `Ninja Multi-Config`, `Unix Makefiles`, `MinGW Makefiles`, `Visual Studio 17 2022`, `Xcode`, etc. Sets `CMAKE_GENERATOR`. Some take `-A` (platform: Win32/x64/ARM) and `-T` (toolset).

**Single-config vs multi-config.** Single-config (Ninja, Make): `CMAKE_BUILD_TYPE` is meaningful at configure time, baked into generated build files. Multi-config (VS, Xcode, Ninja Multi-Config): `CMAKE_BUILD_TYPE` is **ignored**; active config selected at build time (`cmake --build . --config Release`). `CMAKE_CONFIGURATION_TYPES` lists allowed configs.

**Generator expression algebra** (evaluated at generation, not configure):

- **Logical**: `$<BOOL:str>`, `$<NOT:cond>`, `$<AND:c1,c2,...>`, `$<OR:...>`, `$<IF:cond,t,f>`, `$<EQUAL:n1,n2>`, `$<STREQUAL:s1,s2>`, `$<IN_LIST:item,list>`, `$<VERSION_LESS:v1,v2>`.
- **Variable queries**: `$<CONFIG>`, `$<CONFIG:Debug,Release>`, `$<PLATFORM_ID:Linux>`, `$<C_COMPILER_ID:GNU>`, `$<COMPILE_LANGUAGE:CXX>`, `$<LINK_LANGUAGE:C>`, `$<COMPILE_FEATURES:cxx_std_17>`.
- **Target queries**: `$<TARGET_FILE:tgt>`, `$<TARGET_FILE_NAME:tgt>`, `$<TARGET_FILE_DIR:tgt>`, `$<TARGET_LINKER_FILE:tgt>`, `$<TARGET_SONAME_FILE:tgt>`, `$<TARGET_PROPERTY:tgt,prop>`, `$<TARGET_OBJECTS:tgt>`, `$<TARGET_EXISTS:tgt>`, `$<GENEX_EVAL:str>`, `$<TARGET_GENEX_EVAL:tgt,str>`.
- **String ops**: `$<JOIN:list,sep>`, `$<LIST:GET|FILTER|TRANSFORM,...>`, `$<LOWER_CASE:s>`, `$<UPPER_CASE:s>`.
- **Path ops**: `$<PATH:GET_FILENAME,...>`, `$<MAKE_C_IDENTIFIER:s>`, `$<SHELL_PATH:p>`.
- **Output context**: `$<BUILD_INTERFACE:...>`, `$<INSTALL_INTERFACE:...>`, `$<INSTALL_PREFIX>`. `$<OUTPUT_CONFIG:...>` and `$<COMMAND_CONFIG:...>` valid only as outermost expression in `add_custom_command`/`target` for multi-config.

**Where genexes are valid**: most `target_*` properties, `add_custom_command/target`'s `COMMAND`, `install(... DESTINATION)`, `file(GENERATE ... CONTENT/OUTPUT/CONDITION)`. **Not valid** in: `if()`, `message()`, `set()`/cache values, `add_subdirectory()`, source filenames in `add_executable/library`, `find_package` arguments, `configure_file` template variables. Each property/command's docs state explicitly whether genexes are accepted — no general rule.

**Quoting subtlety.** Genex output strings can themselves contain `;` separators. Use `VERBATIM` and `COMMAND_EXPAND_LISTS` in custom commands to handle safely.

**No short-circuit.** `$<AND:c1,c2>` evaluates both even if `c1` is false.

→ **Conversion.** Classify each genex:
- **Statically resolvable** (`$<1:foo>`, `$<0:foo>`, `$<BOOL:literal>`, `$<IF:literal-cond,a,b>`) → eagerly fold to a Starlark string.
- **Configuration-dependent but flag-mappable** (`$<CONFIG:Debug>`, `$<PLATFORM_ID:Linux>`, `$<COMPILE_LANGUAGE:CXX>`, `$<C_COMPILER_ID:Clang>`) → Bazel `select({"@platforms//os:linux": ...})` plus user-defined `config_setting` targets.
- **Target-property-dependent** (`$<TARGET_PROPERTY:foo,INTERFACE_INCLUDE_DIRECTORIES>`, `$<TARGET_FILE:foo>`, `$<TARGET_OBJECTS:foo>`, `$<LINK_ONLY:...>`) → cannot flatten in BUILD; lower to fields on a custom provider read inside a Starlark **rule implementation**, not in loading-phase macros.

The combination of (a) genexes embedded in arbitrary strings, (b) per-config evaluation, (c) no-short-circuit `$<AND>` — means a lossless converter must keep genex strings as data and have its own evaluator. Bazel's `select()` will not stretch.

---

## 16. CMake list semantics

A CMake "list" is a semicolon-separated string. From [`list()` docs](https://cmake.org/cmake/help/latest/command/list.html), most list-constructing commands "do not escape `;` characters in list elements, thus flattening nested lists":

```cmake
set(X "a;b;c")
list(LENGTH X n)   # n = 3, X is ["a","b","c"]
set(Y "a\;b;c")
list(LENGTH Y n)   # n = 2, Y is ["a;b","c"]
list(APPEND Z "")
list(LENGTH Z n)   # n may be 0; empty element dropped
```

Empty elements are usually dropped. `\;` escapes a semicolon inside a variable reference. Starlark lists are real heterogeneous sequences; conversions in either direction lose information.

→ **Conversion.** Carry CMake "lists" as Starlark lists internally, serialize back to `;`-joined strings only at interop boundaries (e.g., emitting a flag for an action that exec's CMake itself). Detect literals containing `\;` and refuse to round-trip them through any helper that flattens. **Reject programs that rely on empty-element preservation.**

---

## Cross-cutting impedance mismatches (sorted by severity)

| # | Pitfall | Severity | Strategy |
|---|---------|----------|----------|
| 1 | `try_compile`/`try_run`/`check_*` whose result drives target-graph shape | **Structural blocker** | Refuse, or precompute in repo rule |
| 2 | `find_package` with non-trivial `*Config.cmake` | **Blocker for pure-Starlark** | Hybrid: run real CMake in repo rule, dump JSON |
| 3 | `cmake_language(EVAL)` and `DEFER` with late-binding args | **Blocker** | Refuse general use |
| 4 | Generator expressions on target properties (`$<TARGET_FILE>` etc.) | **High** | Carry as opaque, lower in rule impls |
| 5 | `macro()` mutating caller scope or calling `return()` | **High** | Inline-expand at call sites; refuse `return()` |
| 6 | `if()` auto-deref, double-eval, CMP0054 | **High** | Hand-rolled evaluator, never lower to Starlark `if` |
| 7 | Policy stack (per-frame, inheritable) | **High** | PolicyFrame data structure threaded through |
| 8 | `set(... CACHE ...)` cross-invocation persistence | **High** | One-shot resolve at port time |
| 9 | `set(... PARENT_SCOPE)` and `set_property(GLOBAL ...)` | **High** | Refuse cross-scope mutation; lift to pure functions returning tuples |
| 10 | `execute_process` informing target-graph | **High** | Move to repo rule generating `.bzl` |
| 11 | `FetchContent`/`ExternalProject` with computed URLs | **High** | `http_archive` if URL+hash literal; else refuse |
| 12 | `include()` dynamic scoping + side effects | **Medium** | Function call threading scope object explicitly |
| 13 | `file(GLOB)` without `CONFIGURE_DEPENDS` | **Medium** | Map to `glob([...])` (Bazel tracks inputs anyway) |
| 14 | `install(EXPORT)` and CMake `*Config.cmake` consumers | **Medium** | Generate stub Targets.cmake + pkg_tar |
| 15 | Compiler ID detection / `enable_language` | **Medium** | Skip; require explicit Bazel toolchain |
| 16 | List semicolon semantics, `\;` escapes, empty elements | **Low** | Real lists internally, serialize at boundaries |

---

## Recommended transpilation strategy

Three options:

**(a) Faithful CMake-language interpreter in Starlark.** Tempting but bounded by Starlark hermeticity. Once you hit `try_compile`, `find_package`, `execute_process`, or any non-trivial `*Config.cmake`, the interpreter must call out — which a `.bzl` interpreter cannot do. You'd have to put the interpreter in a repo rule and emit static `.bzl` from it, at which point you're really doing (b).

**(b) Run real CMake to capture the resolved target graph, then convert that.** What `gn`-style and `bazel_to_cmake` projects converge on. Run a real `cmake --configure` against a probe/host toolchain inside a repo rule, ask CMake to dump its target graph (via `--graphviz`, the file API server-mode JSON under `.cmake/api/v1/`, or a hand-rolled CMake script that walks build-system properties), feed that JSON into a Starlark generator that emits `cc_library`/`cc_binary`/`select()`/rule-impl shims. You sidestep all 16 pitfalls because CMake itself does the imperative part. Price: the host-evaluated graph is **one configuration**; you must re-run per cross-compile axis you care about and merge results into `select()`s, which is non-trivial for genex-laden projects.

**(c) Hybrid.** The realistic answer. Use (b) as the default path: run real CMake, capture the file API output, transpile to Starlark. But also ship a small Starlark-side runtime — a frozen-aware `scope` object, a list-preserving string helper, a policy-aware `if` evaluator, a `select()` synthesiser for the genex classes from §15 — so consumer-side transpiled snippets can re-evaluate the easy parts (constants, simple branches, list manipulation) without re-invoking CMake. Reserve `rules_foreign_cc` as the escape hatch for projects that violate too many rules to transpile.

**Recommendation: build (c) on top of (b)'s skeleton.** The faithful-interpreter ambition is a tar pit; CMake's semantics are defined by the C++ source of CMake itself, including dozens of policies whose interactions are documented only by behaviour. Let real CMake do the imperative work, then translate its **output** into the declarative model Bazel demands.

---

## Sources

- Docs: `Help/manual/cmake-language.7.rst`, `cmake-buildsystem.7.rst`, `cmake-generator-expressions.7.rst`, `cmake-policies.7.rst`, `cmake-toolchains.7.rst`, `cmake.1.rst`.
- Commands: `Help/command/{if,foreach,function,macro,set,return,block,project,cmake_minimum_required,cmake_policy,cmake_language,configure_file,file,execute_process,try_compile,try_run,include,include_guard,find_package,find_library,install,export}.rst`.
- Modules: `Modules/{FetchContent,ExternalProject,CMakeDetermineCompilerId,CheckSymbolExists,GNUInstallDirs}.cmake`.
- Source (Kitware/CMake): `Source/{cmListFileLexer,cmListFileCache,cmMakefile,cmCoreTryCompile,cmCMakeMinimumRequired,cmGlobalGenerator,cmake,cmakemain}.cxx`, `Source/cm*Command.cxx`.
- Bazel: `bazel.build/rules/language`, `bazel.build/external/{repo,extension}`, `bazel.build/docs/configurable-attributes`, `bazel.build/extending/toolchains`.
- Starlark spec: `github.com/bazelbuild/starlark/blob/master/spec.md`.
- `rules_foreign_cc`: `github.com/bazel-contrib/rules_foreign_cc/blob/main/foreign_cc/{private/framework,make}.bzl`.
- Key policies: CMP0054, CMP0057, CMP0067, CMP0077, CMP0079, CMP0099, CMP0116, CMP0118, CMP0124, CMP0126.

---

Want me to also commit this as `docs/cmake_configure_analysis.md` on the branch, or keep it inline only?
