.PHONY: all converter diff history bst-translate derive-toolchain build-tracer convert-element-trace run-manifest test test-race cover test-e2e e2e-hello-world e2e-fmt e2e-meta-bst-wrapper \
        e2e-cmake-consumer e2e-toolchain-skip e2e-fidelity e2e-fidelity-fmt e2e-fidelity-compare-zlib e2e-fidelity-compare-catch2 e2e-fidelity-compare-libpng e2e-fidelity-compare-spdlog e2e-fidelity-compare-fmt e2e-fidelity-compare-zlib-consumer e2e-fidelity-compare-fmt-consumer e2e-fidelity-compare-spdlog-consumer e2e-fidelity-compare-nlohmann-json-consumer \
        e2e-meta-hello e2e-meta-stack e2e-meta-manual e2e-meta-make e2e-meta-make-round2 e2e-meta-trace-round2-fold e2e-meta-autotools-round2-multiplatform e2e-meta-cmake-round2-fallback-multiplatform e2e-meta-meson e2e-meta-meson-round2-fallback e2e-meta-meson-round2-fallback-multiplatform e2e-meta-converge e2e-meta-finalize-b e2e-meta-cross-kind e2e-meta-pyproject e2e-meta-pyproject-fallback e2e-meta-vars e2e-meta-gazelle-roundtrip e2e-meta-render-project-a e2e-meta-unify-toolchains e2e-meta-toolchain-build e2e-meta-kits-build \
        e2e-meta-compose e2e-meta-filter e2e-meta-import e2e-meta-autotools e2e-meta-cross-cmake e2e-meta-cmake-cross-package-target-file e2e-meta-cmake-split-build e2e-meta-cmake-split-multiconfig e2e-meta-cmake-split-gazelle e2e-meta-cmake-vcs-stamp e2e-meta-cmake-vcs-stamp-indirect e2e-meta-cmake-vcs-stamp-function e2e-meta-cmake-render-gates \
        e2e-meta-bazel-passthrough e2e-meta-bazel-override \
        e2e-meta-autotools-native e2e-meta-autotools-round2 e2e-meta-autotools-round2-live e2e-meta-autotools-multitarget e2e-meta-autotools-tu-optflags e2e-meta-autotools-libtool-pic e2e-meta-autotools-libtool-shared e2e-meta-autotools-determinism e2e-meta-autotools-subdirs e2e-meta-autotools-config-h e2e-meta-autotools-asm \
        e2e-meta-conditional e2e-meta-script e2e-meta-buildbarn-re e2e-meta-regression e2e-audit-narrowing fdsdk-reality-check \
        buildbarn-up buildbarn-down bb-clientd-up bb-clientd-down e2e-hello-bbclientd install-bazelisk install-cmake \
        fetch-fmt fetch-zlib fetch-spdlog fetch-nlohmann-json fetch-catch2 fetch-libpng fetch-abseil fetch-protobuf fetch-googletest fetch-eigen fetch-llvm fetch-vtk fetch-survey \
        fetch-re2 fetch-boost-core fetch-zstd fetch-libevent fetch-libxml2 fetch-brotli fetch-mbedtls fetch-cutlass fetch-cuda-samples fetch-openblas fetch-sdl fetch-curl fetch-grpc fetch-buildbox fetch-glog fetch-glm fetch-cryptoauthlib fetch-survey-regression \
        survey-gazelle survey-multiplatform update-golden record-fixtures lint vet fmt staticcheck check-cmake-toolchain clean

# Pinned external tool versions. Hard-failed at runtime by the converter,
# enforced softly here for dev-loop visibility.
CMAKE_VERSION  ?= 4.3.3
NINJA_VERSION  ?= 1.11.1

# M2 acceptance-package version. Bumping requires a re-run of
# TestE2E_Fmt_Converts since the *_test count assertion has a floor.
FMT_VERSION    ?= 11.0.2
FMT_DIR        ?= /tmp/fmt
ZLIB_VERSION   ?= v1.3.1
ZLIB_DIR       ?= /tmp/zlib
SPDLOG_VERSION ?= v1.14.1
SPDLOG_DIR     ?= /tmp/spdlog
JSON_VERSION   ?= v3.11.3
JSON_DIR       ?= /tmp/json
CATCH2_VERSION ?= v3.5.3
CATCH2_DIR     ?= /tmp/Catch2
LIBPNG_VERSION ?= v1.6.43
LIBPNG_DIR     ?= /tmp/libpng

# Diagnostic-survey corpus (see docs/codemodel-consumption-audit.md).
# Cloned out-of-band, then run through the converter in
# --ignore-rejections-for-diagnostics mode to enumerate the
# codemodel-consumption + idiom gap surface. NOT fidelity gates —
# best-effort BUILD output, not guaranteed to build.
ABSEIL_VERSION    ?= 20260107.1
ABSEIL_DIR        ?= /tmp/abseil-cpp
PROTOBUF_VERSION  ?= v6.31.1
PROTOBUF_DIR      ?= /tmp/protobuf
RE2_VERSION       ?= 2024-07-02
RE2_DIR           ?= /tmp/re2
GTEST_VERSION     ?= v1.17.0
GTEST_DIR         ?= /tmp/googletest
EIGEN_VERSION     ?= 3.4.1
EIGEN_DIR         ?= /tmp/eigen
# llvm + vtk are the large stress-test members (ENABLE_EXPORTS, PCH,
# TableGen / cmake -P codegen, forward-declared includes). Not in the
# default fetch-survey set — fetch explicitly. See docs/survey-corpus.md.
# Survey llvm's `llvm/` SUBDIR, not the monorepo root (the root inflates
# the missing-include-dir count with sibling dirs a shallow clone omits).
LLVM_VERSION      ?= llvmorg-20.1.8
LLVM_DIR          ?= /tmp/llvm-project
# VTK's canonical home (gitlab.kitware.com) is blocked by the CI/sandbox
# network allowlist; the github.com/Kitware/VTK mirror is allowlisted.
VTK_VERSION       ?= v9.4.2
VTK_DIR           ?= /tmp/vtk
# Regression corpus (each surfaced a converter bug now fixed, or is a
# clean control). See docs/survey-corpus.md for the per-project bug +
# fixing-PR table and the faithful-survey caveats (zstd's build/cmake
# subdir, mbedtls's framework submodule, cutlass/cuda needing a CUDA
# toolkit to configure).
BOOSTCORE_VERSION ?= boost-1.90.0
BOOSTCORE_DIR     ?= /tmp/boost-core
ZSTD_VERSION      ?= v1.5.7
ZSTD_DIR          ?= /tmp/zstd
LIBEVENT_VERSION  ?= release-2.1.12-stable
LIBEVENT_DIR      ?= /tmp/libevent
LIBXML2_VERSION   ?= v2.15.3
LIBXML2_DIR       ?= /tmp/libxml2
BROTLI_VERSION    ?= v1.2.0
BROTLI_DIR        ?= /tmp/brotli
MBEDTLS_VERSION   ?= v3.6.2
MBEDTLS_DIR       ?= /tmp/mbedtls
CUTLASS_VERSION   ?= v4.5.1
CUTLASS_DIR       ?= /tmp/cutlass
CUDASAMPLES_VERSION ?= v13.3
CUDASAMPLES_DIR   ?= /tmp/cuda-samples
OPENBLAS_VERSION  ?= v0.3.28
OPENBLAS_DIR      ?= /tmp/OpenBLAS
SDL_VERSION       ?= release-3.2.10
SDL_DIR           ?= /tmp/SDL
CURL_VERSION      ?= curl-8_11_1
CURL_DIR          ?= /tmp/curl
GRPC_VERSION      ?= v1.74.0
GRPC_DIR          ?= /tmp/grpc
BUILDBOX_VERSION  ?= 1.4.8
BUILDBOX_DIR      ?= /tmp/buildbox
GLOG_VERSION      ?= v0.7.1
GLOG_DIR          ?= /tmp/glog
GLM_VERSION       ?= 1.0.1
GLM_DIR           ?= /tmp/glm
# cryptoauthlib: real-world recursive cmake via configure-time execute_process.
CRYPTOAUTHLIB_VERSION ?= v3.8.0
CRYPTOAUTHLIB_DIR     ?= /tmp/cryptoauthlib

GO        ?= go
GOFLAGS   ?=
BUILD_DIR ?= build
BIN_DIR   := $(BUILD_DIR)/bin

CONVERTER    := $(BIN_DIR)/convert-element-cmake
DIFF         := $(BIN_DIR)/orchestrate-diff
HISTORY      := $(BIN_DIR)/orchestrate-history
BST_TRANSLATE := $(BIN_DIR)/bst-translate
DERIVE_TOOLCHAIN := $(BIN_DIR)/derive-toolchain
WRITE_A      := $(BIN_DIR)/write-a
SOURCE_PUSH  := $(BIN_DIR)/source-push
BUILD_TRACER := $(BIN_DIR)/build-tracer
CONVERT_ELEMENT_TRACE := $(BIN_DIR)/convert-element-trace
RUN_MANIFEST := $(BIN_DIR)/run-manifest

# Every Go binary target lists GO_SRC as a prerequisite. Coarse (any
# .go change rebuilds every binary) but correct — the alternative,
# prerequisite-free file targets, meant `make all` treated an existing
# binary as up-to-date forever and never rebuilt stale code, so
# `tools/bst`'s ensure_binaries silently ran whatever was last built.
# `go build`'s own incremental cache keeps the rebuild near-instant
# when nothing actually changed.
GO_SRC := $(shell find . -name '*.go' -not -path './$(BUILD_DIR)/*') go.mod go.sum

all: converter diff history bst-translate derive-toolchain write-a source-push build-tracer convert-element-trace run-manifest

converter: $(CONVERTER)

diff: $(DIFF)

history: $(HISTORY)

bst-translate: $(BST_TRANSLATE)

derive-toolchain: $(DERIVE_TOOLCHAIN)

write-a: $(WRITE_A)

source-push: $(SOURCE_PUSH)

build-tracer: $(BUILD_TRACER)

convert-element-trace: $(CONVERT_ELEMENT_TRACE)

run-manifest: $(RUN_MANIFEST)

$(CONVERTER): $(GO_SRC)
	@mkdir -p $(BIN_DIR)
	CGO_ENABLED=0 $(GO) build $(GOFLAGS) -o $(CONVERTER) ./converter/cmd/convert-element-cmake

$(DIFF): $(GO_SRC)
	@mkdir -p $(BIN_DIR)
	CGO_ENABLED=0 $(GO) build $(GOFLAGS) -o $(DIFF) ./cmd/orchestrate-diff

$(HISTORY): $(GO_SRC)
	@mkdir -p $(BIN_DIR)
	CGO_ENABLED=0 $(GO) build $(GOFLAGS) -o $(HISTORY) ./cmd/orchestrate-history

$(BST_TRANSLATE): $(GO_SRC)
	@mkdir -p $(BIN_DIR)
	CGO_ENABLED=0 $(GO) build $(GOFLAGS) -o $(BST_TRANSLATE) ./cmd/bst-translate

$(DERIVE_TOOLCHAIN): $(GO_SRC)
	@mkdir -p $(BIN_DIR)
	CGO_ENABLED=0 $(GO) build $(GOFLAGS) -o $(DERIVE_TOOLCHAIN) ./converter/cmd/derive-toolchain

$(WRITE_A): $(GO_SRC)
	@mkdir -p $(BIN_DIR)
	CGO_ENABLED=0 $(GO) build $(GOFLAGS) -o $(WRITE_A) ./cmd/write-a

$(SOURCE_PUSH): $(GO_SRC)
	@mkdir -p $(BIN_DIR)
	CGO_ENABLED=0 $(GO) build $(GOFLAGS) -o $(SOURCE_PUSH) ./cmd/source-push

$(BUILD_TRACER): $(GO_SRC)
	@mkdir -p $(BIN_DIR)
	CGO_ENABLED=0 $(GO) build $(GOFLAGS) -o $(BUILD_TRACER) ./cmd/build-tracer

$(CONVERT_ELEMENT_TRACE): $(GO_SRC)
	@mkdir -p $(BIN_DIR)
	CGO_ENABLED=0 $(GO) build $(GOFLAGS) -o $(CONVERT_ELEMENT_TRACE) ./cmd/convert-element-trace

$(RUN_MANIFEST): $(GO_SRC)
	@mkdir -p $(BIN_DIR)
	CGO_ENABLED=0 $(GO) build $(GOFLAGS) -o $(RUN_MANIFEST) ./cmd/run-manifest

# Unit tests: pre-recorded File API fixtures, no cmake required.
test:
	$(GO) test ./...

# Race-detector run of the unit suite. Not part of the fast `test` loop —
# the detector roughly halves throughput and the converter is mostly
# single-threaded (the only concurrency lives in internal/cas/fakecas) — so
# run it before changes that touch goroutine/channel code or the fake CAS.
test-race:
	$(GO) test -race ./...

# Coverage lens (measurement only — NOT a gate). Writes a per-statement
# profile and prints the total plus an annotated HTML view, so under-tested
# code is visible without a noisy threshold gate. Scoped to packages that
# actually have tests: no-test packages would otherwise show as 0% clutter
# (enumerate them with `go test ./...` instead) and, more practically, drive
# `go test -coverprofile` down the covdata merge path that a stripped Go
# toolchain (the web session's host SDK ships `cover` but not `covdata`) can't
# satisfy. `make cover` for the total; open coverage.html for the line view.
cover:
	$(GO) test -coverprofile=coverage.out \
		$$($(GO) list -f '{{if or .TestGoFiles .XTestGoFiles}}{{.ImportPath}}{{end}}' ./...)
	@$(GO) tool cover -func=coverage.out | tail -1
	@$(GO) tool cover -html=coverage.out -o coverage.html
	@echo "wrote coverage.out (profile) and coverage.html (annotated source)"

# End-to-end tests: real cmake invocation. Gated behind build tag.
test-e2e: check-cmake-toolchain converter
	$(GO) test -tags=e2e ./converter/...

e2e-hello-world: check-cmake-toolchain converter
	$(GO) test -tags=e2e -run TestE2E_HelloWorld ./converter/...

e2e-fmt: check-cmake-toolchain converter fetch-fmt
	$(GO) test -tags=e2e -run TestE2E_Fmt ./converter/...

# Phase 1 acceptance gate for the meta-project (Bazel-as-orchestrator)
# shape. Renders project A and project B
# from the hello-world fixture via cmd/write-a, then drives the full
# two-pass pipeline: bazel build A (runs convert-element-cmake via genrule)
# -> stage A's BUILD.bazel.out into B -> bazel build + run B's smoke
# binary linking against the converted cc_library. Skips the bazel
# phases cleanly when bazel >= 7 isn't on PATH; the rendering checks
# alone are still a useful regression gate.
e2e-meta-hello: converter
	scripts/meta-hello.sh

# Render-half smoke test for tools/bst (the BuildStream-style CLI
# wrapper around write-a). Exercises three render shapes (kind:cmake,
# kind:autotools, multi-element graph with flush-left bare-name
# deps) plus the workspace open/close round-trip. Bazel-build half
# is out of scope — the wrapper shells out to whatever bazel is on
# PATH, and that side is covered by the per-kind render + bazel-side
# e2e gates.
e2e-meta-bst-wrapper: all
	scripts/meta-bst-wrapper.sh

# Narrowing-undercoverage audit gate (soft launch). Renders a
# small meta-project with write-a, invokes convert-element-cmake
# offline to populate the cmake oracle (cmake-reads.json per
# kind:cmake element), then runs scripts/audit-narrowing-walk.sh
# to accumulate per-element drift. The script exits non-zero
# when drift is detected — the make target inherits that exit
# code so this make invocation fails just like any other check
# target. The soft-vs-blocking dial lives in the CI step that
# calls this target: `continue-on-error: true` keeps the job
# green while the allowlist mechanism stabilizes against a
# representative fixture set; flipping it to false promotes
# the gate to blocking (a real one-line YAML change because
# the script's exit code already discriminates). Recipe:
# docs/design/narrowing-audit.md.
e2e-audit-narrowing: check-cmake-toolchain converter
	scripts/meta-audit-narrowing.sh

# Phase 2 acceptance gate for the meta-project. Multi-element fixture
# (testdata/meta-project/two-libs/) — two kind:cmake elements + one
# kind:stack bundling them. Validates write-a's per-kind dispatch +
# graph-shape rendering + the kind:stack handler's filegroup
# composition end-to-end through both projects, with a smoke binary
# linking against both cmake elements. Same bazel-availability
# gating as e2e-meta-hello.
e2e-meta-stack: converter
	scripts/meta-stack.sh

# kind:bazel passthrough acceptance gate. Element's source tree
# already contains a BUILD.bazel; write-a stages it verbatim into
# project B and runs no translator. The gate asserts the staged
# BUILD is byte-identical to the source's authored one and the
# resulting cc_binary builds + runs end-to-end.
e2e-meta-bazel-passthrough: converter
	scripts/meta-bazel-passthrough.sh

# --build-files-dir override acceptance gate. A kind:manual .bst
# paired with an out-of-band <name>.BUILD.bazel under the
# directory passed to --build-files-dir gets re-stamped to
# kind:bazel and emits the operator's BUILD verbatim into
# project B; kind:local sources still stage alongside so the
# override's srcs = [...] resolves at bazel-build time.
e2e-meta-bazel-override: converter
	scripts/meta-bazel-override.sh

# Cross-element kind:cmake dep gate. Two kind:cmake elements where
# the consumer (cons) depends on the producer (prod) via
# find_package(prod CONFIG REQUIRED) + target_link_libraries(prod::prod).
# Asserts: write-a renders the cross-element bundle staging + an
# imports.json synthesis file; bazel build of cons resolves the
# dep against the staged bundle; convert-element-cmake's STATIC IMPORTED
# dep recovery emits deps = ["//elements/prod:prod"] in the
# converted output.
e2e-meta-cross-cmake: converter
	scripts/meta-cross-cmake.sh

# --split-packages orchestrator gate: render a multi-CMakeLists element
# with write-a --split-packages, bazel-build the cmake_split_convert
# TreeArtifact in project A, stage-b it into B, and bazel-build the
# split tree there (the synthesized header library carries the
# cross-package include path). See docs/design/cmake-split-packages.md.
e2e-meta-cmake-split-build: converter
	scripts/meta-cmake-split-build.sh

e2e-meta-cmake-split-multiconfig: converter
	scripts/meta-cmake-split-multiconfig.sh

# "gazelle_cc owns the layout" gate (Phase-8b continuous conversion):
# render the --split-packages output with --gazelle-cc, run
# `bazel run //:gazelle` over it, and assert the build still works, the
# install-export cc_imports survive (the whole-rule # keep fix), and a
# second gazelle pass is a fixpoint. See docs/design/cmake-split-packages.md.
e2e-meta-cmake-split-gazelle: converter
	scripts/meta-cmake-split-gazelle.sh

# VCS-stamp lift gate: convert a fixture whose execute_process(git rev-parse)
# feeds a configure_file(@GIT_SHA@), assert the emitted cmake_configure_file
# carries stamp_values, then bazel-build version.h WITH --stamp (re-reads the
# live workspace-status revision) and WITHOUT (falls back to the baked one).
# See docs on the stamp lift in ROADMAP.md.
e2e-meta-cmake-vcs-stamp: converter
	scripts/meta-cmake-vcs-stamp.sh

# VCS-stamp set()-indirection gate: convert a Benchmark-shaped fixture
# (git rev-parse -> set(VERSION ${GIT_SHA}) -> configure_file(@VERSION@))
# and assert the warm second --trace pass lifts the COPIED variable to
# stamp_values. Exercises the full two-pass pipeline (the unit tests cover
# the extractor + propagation on synthetic input).
e2e-meta-cmake-vcs-stamp-indirect: converter
	scripts/meta-cmake-vcs-stamp-indirect.sh

# VCS-stamp helper-function gate: convert a git_describe()-shaped fixture
# (function-local OUTPUT_VARIABLE forwarded via set(${_var} "${out}"
# PARENT_SCOPE)) and assert the pass-1 stamp abort is recovered so the clean
# convert completes instead of aborting. The recovered value bakes (the
# ${_var} return name isn't resolved to the parent var); lifting it to
# stamp_values is a follow-up.
e2e-meta-cmake-vcs-stamp-function: converter
	scripts/meta-cmake-vcs-stamp-function.sh

# Cmake render gates — each converts a sample project and asserts the
# rendered BUILD; several also bazel-build the result (the load-bearing
# half the Go-level unit tests + goldens don't cover). They run locally via
# `make`; this aggregate promotes them into CI so their
# convert -> render -> bazel-build contracts guard regressions on every PR.
# The aggregate target guards on cmake + ninja up front (and each gate
# self-skips its bazel-build half), so it's safe to invoke unconditionally.
RENDER_GATES = \
	scripts/meta-cmake-genex-probe.sh \
	scripts/meta-cmake-genclass-textual-impl.sh \
	scripts/meta-elf-fidelity.sh \
	scripts/meta-file-generate.sh \
	scripts/meta-cmake-genex-literal-twopass.sh \
	scripts/meta-cmake-fileset-compiled-lib.sh \
	scripts/meta-cmake-stamp-volatile.sh \
	scripts/meta-cmake-vcs-stamp.sh \
	scripts/meta-cmake-file-write-stamp.sh \
	scripts/meta-cmake-vcs-stamp-indirect.sh \
	scripts/meta-cmake-vcs-stamp-function.sh \
	scripts/meta-cmake-genrule-inplace-rewrite.sh \
	scripts/meta-cc-embed.sh \
	scripts/meta-cc-embed-recognize.sh \
	scripts/meta-cmake-export-header.sh \
	scripts/meta-cmake-pch.sh \
	scripts/meta-cmake-per-config-bake.sh \
	scripts/meta-cmake-custom-binary-dir.sh \
	scripts/meta-cmake-defer-execute-process.sh \
	scripts/meta-cmake-execute-process-argv-codegen.sh \
	scripts/meta-cmake-execute-process-unspecified-outs.sh \
	scripts/meta-cmake-execute-process-dead-capture.sh \
	scripts/meta-cmake-execute-process-pipeline-wd.sh \
	scripts/meta-cmake-execute-process-cmake-e-wrappers.sh \
	scripts/meta-cmake-e-tar-create.sh \
	scripts/meta-cmake-nested-cmake.sh \
	scripts/meta-cmake-nested-cmake-workdir.sh \
	scripts/meta-cmake-build-dir-source-bake.sh \
	scripts/meta-cmake-file-download-http.sh \
	scripts/meta-cmake-cc-hash.sh \
	scripts/meta-cmake-todos-coverage.sh \
	scripts/meta-intent-capture-lens.sh \
	scripts/meta-cmake-comment-carrying.sh \
	scripts/meta-cmake-cross-package-target-file.sh \
	scripts/meta-cmake-enable-exports.sh \
	scripts/meta-cmake-execute-process-rescue.sh \
	scripts/meta-cmake-find-package-variable-form.sh \
	scripts/meta-cmake-install-export-declarative.sh \
	scripts/meta-cmake-install-files-pkg.sh \
	scripts/meta-cmake-interface-genex-defines.sh \
	scripts/meta-cmake-platform-partition-tier2.sh \
	scripts/meta-cmake-probe-genex-duplicate-subdir.sh \
	scripts/meta-cmake-probe-genex-compile-language.sh \
	scripts/meta-cmake-probe-genex-object-library.sh \
	scripts/meta-cmake-probe-genex-utility.sh \
	scripts/meta-cmake-round2-fallback.sh \
	scripts/meta-cmake-sanitizer-features.sh \
	scripts/meta-cmake-split-packages.sh \
	scripts/meta-cmake-standalone-custom-command.sh \
	scripts/meta-cmake-workspace-root.sh

# No `converter` prerequisite: each gate builds convert-element-cmake itself,
# so the skip branches below truly skip (no forced converter build). Some
# gates only guard on cmake, but every gate runs `--source-root` which
# configures with -G Ninja (converter/internal/cmakerun/run.go), so guard the
# aggregate on BOTH cmake and ninja up front. (bazel absence is handled
# per-gate, which self-skip just their build halves.) One recipe shell so the
# skip `else` reaches the whole loop.
e2e-meta-cmake-render-gates:
	@if ! command -v cmake >/dev/null 2>&1; then \
		echo "skip: cmake not on PATH (cmake render gates)"; \
	elif ! command -v ninja >/dev/null 2>&1; then \
		echo "skip: ninja not on PATH (render gates configure with -G Ninja)"; \
	else \
		set -e; for g in $(RENDER_GATES); do echo "=== $$g ==="; sh "$$g"; done; \
	fi

# Cross-package $<TARGET_FILE:t> resolved-lift gate. Renders a
# producer + consumer kind:cmake pair where the consumer's
# CMakeLists.txt has `file(GENERATE)` referencing
# $<TARGET_FILE:producer::producer>; asserts the lifted genrule
# resolves producer::producer to //elements/producer:producer
# via the imports.json manifest (PR 2 of cross-package
# TARGET_FILE). The PR 1 refusal-stub gate (the
# cmake-codegen-file-generate-genex-cross-package tag) is
# asserted absent — the resolved-lift path supersedes it for
# manifest-hit cases.
e2e-meta-cmake-cross-package-target-file: converter
	scripts/meta-cmake-cross-package-target-file.sh

# Run-vs-run regression gate. Renders + builds project A from the
# cross-cmake fixture twice, snapshots each build with cmd/run-manifest,
# and diffs the two with orchestrate-diff: asserts a content edit cmake
# never reads doesn't shift BUILD.bazel.out (no-drift invariant) and a
# real CMakeLists change does (drift detection). The write-a + Bazel
# path's replacement for the (now-deleted) orchestrator's regression
# e2e.
e2e-meta-regression: converter
	scripts/meta-regression.sh

# Phase 3 first-cut acceptance gate. Single kind:manual element
# (testdata/meta-project/manual-greet/) whose install-commands
# stage a greeting file under %{install-root}%{prefix}/share/.
# Drives bazel build A → extracts the resulting install_tree.tar →
# asserts content. Validates write-a's manual handler + variable
# substitution end-to-end. Same bazel-availability gating as
# e2e-meta-hello / e2e-meta-stack.
e2e-meta-manual: converter
	scripts/meta-manual.sh

# Phase 3 sibling acceptance gate. Single kind:make element
# (testdata/meta-project/make-greet/) with a Makefile that builds
# a tiny `greet` binary and installs it. Drives bazel build A →
# extracts the resulting install_tree.tar → asserts usr/bin/greet
# exists, runs, and prints the expected message. Validates that
# kind:make's pipelineHandler defaults (`make` / `make install`)
# resolve correctly without an explicit config: in the .bst.
e2e-meta-make: converter
	scripts/meta-make.sh

# kind:make joins the trace-driven round-2 architecture
# (handler_make.go opts in via traceDrivenSrckeyPatterns).
# This gate asserts the round-2 rendered shape: project A hosts
# a per-element converter genrule consuming @trace_<elem>//:trace;
# project B hosts the coarse install genrule wrapped in
# build-tracer with an inline trace-publish step. Mirror of
# e2e-meta-autotools-round2 for the kind:make path.
#
# Render-half only. The kind-agnostic bazel-side e2e (publish →
# AC hit on fresh render → fine cc rules) lives in
# tools/e2e-meta-autotools-round2-live.sh and applies to any
# trace-driven kind including kind:make.
e2e-meta-make-round2: converter
	scripts/meta-make-round2.sh

# Per-platform fold acceptance gate for round-2 trace-driven kinds.
# Exercises kind:make with a two-platform manifest passed via
# --platforms-json. Verifies project A renders N per-platform
# converter genrules + one fold-element genrule composing them;
# tools/traces.json + MODULE.bazel use_repo() block reflect the
# per-platform repo names. Render-half only; the live-AC contract
# is exercised by tools/e2e-meta-autotools-round2-live.sh.
e2e-meta-trace-round2-fold: converter
	scripts/meta-trace-round2-fold.sh

# Sibling of e2e-meta-trace-round2-fold for kind:autotools.
# Same per-platform install-fan-out shape (project A converter +
# project B install genrules + top-level select()-filegroup), at
# the autotoolsHandler dispatch site. Render-half only.
e2e-meta-autotools-round2-multiplatform: converter
	scripts/meta-autotools-round2-multiplatform.sh

# Sibling for kind:cmake Phase B execute_process round-2
# fallback. Project B's install fan-out under --platforms-json
# emits N install genrules + top-level select()-filegroup.
# Project A's converter genrule under cmake round-2 fallback
# uses the orchestrator's existing multi-platform fan-out
# (PR #112) at orchestrate time; the write-a-side output is
# the same shape meta-cmake-round2-fallback already verifies.
e2e-meta-cmake-round2-fallback-multiplatform: converter
	scripts/meta-cmake-round2-fallback-multiplatform.sh

# kind:meson native render acceptance gate. Single kind:meson element
# (testdata/meta-project/meson-greet/) — a static_library + executable
# pair. write-a renders project A with --convert-element-meson set so
# the per-element BUILD invokes //tools:convert-element-meson against
# the staged source tree, producing BUILD.bazel.out with native
# cc_library / cc_binary rules from `meson introspect`. The render
# half always runs; the bazel-build half self-skips unless BOTH
# bazel >= 7 AND meson are on PATH (the genrule needs meson on the
# executor; bazel < 7 lacks bzlmod).
e2e-meta-meson: converter
	scripts/meta-meson.sh

# kind:meson Phase B fallback acceptance gate. Render-only: asserts
# A's converter genrule threads --unsupported-target-fallback=true,
# A's MODULE.bazel pulls in the traces module extension, B's per-
# element BUILD is the real install genrule (meson setup + ninja +
# meson install --destdir under build-tracer + inline trace-publish)
# replacing the placeholder. When meson is on PATH, also runs the
# standalone converter against a refusal-triggering fixture and
# verifies strict mode refuses while the fallback emits the
# install-plan-driven placeholder shape.
e2e-meta-meson-round2-fallback: converter
	scripts/meta-meson-round2-fallback.sh

# kind:meson Phase B round-2 fallback per-platform install
# fan-out. Sibling of e2e-meta-cmake-round2-fallback-multiplatform
# and e2e-meta-autotools-round2-multiplatform — drives write-a with
# --meson-round2-fallback + --platforms-json against the meson-greet
# fixture and asserts the rendered project B carries N install
# genrules (one per platform), per-platform outputs under
# <platform>/, exec_compatible_with constraints, and the
# top-level :install_tree.tar select()-filegroup that routes
# downstream label refs to the matching per-platform tarball.
e2e-meta-meson-round2-fallback-multiplatform: converter
	scripts/meta-meson-round2-fallback-multiplatform.sh

# kind:cmake round-2 fallback storage-cost signal. Renders the
# `execute-process-unliftable-fallback` fixture under
# --unsupported-execute-process-fallback=true and reports the
# number of paths the _install_tree_extract genrule duplicates
# out of install_tree.tar — the render-time signal for the
# ROADMAP's "Repo-rule install for kind:cmake round-2 fallback"
# entry. Doesn't fix anything; makes the cost measurable so a
# maintainer can re-evaluate the repo-rule alternative against
# real numbers instead of "roughly 2×" estimates.
e2e-meta-cmake-round2-fallback-storage-cost: check-cmake-toolchain converter
	scripts/meta-cmake-round2-fallback-storage-cost.sh

# Convergence driver loop acceptance gate. Render-only: stubs
# bazel + stage-b as shell scripts that simulate the
# "round 1 miss → round 2 hit" trace_load behaviour, runs
# tools/converge.sh, and asserts the loop ran exactly 2 rounds
# and terminated with "fixpoint reached". Also asserts the
# max-rounds failure path. The bazel-build half (driver against
# a real REAPI endpoint) is covered by the live-AC gate at
# tools/e2e-meta-autotools-round2-live.sh once it grows
# convergence-driver wiring; this gate covers the driver's
# control flow + ordering contract.
e2e-meta-converge: converter
	scripts/meta-converge.sh

# Cross-kind dependency render gate: kind:cmake consumer +
# kind:autotools producer fixture, asserts the cmake handler's
# cmakeDepBundleLabels stops filtering to kind=cmake and stages
# the producer's :*_trace_load (the AC-published bundle source)
# in the consumer's converter genrule srcs. The end-to-end live
# proof (find_package(autoprod CONFIG) actually resolving) is in
# tools/e2e-meta-cross-kind-live.sh.
e2e-meta-cross-kind: converter
	scripts/meta-cross-kind.sh

# finalize-b acceptance gate. Builds cmd/finalize-b, runs it on
# a synthetic converged project B (mix of converged-fine elements
# and unconverged-only-trace_build elements), and asserts the
# converged-element scaffolding gets pruned, the unconverged
# element stays verbatim, MODULE.bazel's rules_buildstream_bazel
# bazel_dep is pruned IFF no surviving BUILD references it,
# idempotence holds (finalize-b(finalize-b(x)) == finalize-b(x)),
# and the tool refuses to overwrite a non-empty --out.
e2e-meta-finalize-b: converter
	scripts/meta-finalize-b.sh

# kind:pyproject native render acceptance gate. Single kind:pyproject
# element (testdata/meta-project/pyproject-greet/) — a setuptools
# layout with a [project.scripts] entry. write-a renders project A
# with --convert-element-pyproject set so the per-element BUILD
# invokes //tools:convert-element-pyproject against the staged
# source tree, producing BUILD.bazel.out with native py_library +
# py_binary rules. The render half always runs; the bazel-build
# half self-skips unless BOTH bazel >= 7 AND python3 are on PATH
# (the rendered py_binary needs python3 at run time; bazel < 7
# lacks bzlmod).
e2e-meta-pyproject: converter
	scripts/meta-pyproject.sh

# Phase 7c/7d/8b acceptance gate for the gazelle-roundtrip story.
# Renders hello-world, runs project A's bazel build, then stage-b
# stages the converted BUILD.bazel into project B and reports the
# changed-element set; asserts the staged BUILD carries the Phase 7d
# `# gazelle:cc_search` directive; runs build-cc-index to populate
# tools/cc_index.json + tools/python_modules.json; runs the Phase 8b
# tail (relax-keeps + targeted `bazel run //:gazelle`, the latter
# guarded on the //:gazelle target existing); and (when buildifier
# is on PATH) asserts the Phase 3 no-op contract still holds.
# Bazel + buildifier are both optional — render assertions always run.
e2e-meta-gazelle-roundtrip: converter
	scripts/meta-gazelle-roundtrip.sh

# Stage 4 acceptance gate for the unified multi-platform Bazel
# toolchain plan: drives render-project-a against the canonical
# CMakePresets.json fixture + a 2-platform manifest and validates
# the rendered BUILD.bazel's per-(variant, platform) genrule cells
# + aggregating filegroup. Render-only (no cmake / bazel invoked);
# the contract is the rendering shape downstream stages consume.
e2e-meta-render-project-a: converter
	scripts/meta-render-project-a.sh

# Stage 5 acceptance gate for unify-toolchains: drives the tool
# end-to-end against synthetic per-cell probe artifacts and
# validates the four tool-owned files
# (platforms/BUILD.bazel, toolchains/BUILD.bazel,
# toolchains/cc_toolchain_config.bzl, .bazelrc), determinism
# across re-runs, the MODULE.bazel setup-banner gating, and the
# --element-signal fold of per-element builtin include / link
# search roots into the platform toolchain. Render-only.
e2e-meta-unify-toolchains: converter
	scripts/meta-unify-toolchains.sh

# toolchain BUILD gate: derive a toolchain from a live cmake probe of the host,
# then bazel-build a C++ target *forcing* it — the regression test that the
# generated cc_toolchain layout actually compiles (catches the bazel-9
# Starlark-autoload load() requirements a render-only check can't).
e2e-meta-toolchain-build: converter
	scripts/meta-toolchain-build.sh

# kit (compiler-axis) multi-toolchain build gate: probe two compiler kits
# (gcc, clang) via live cmake, fold them through unify-toolchains into the
# kit-dimensioned Bazel layout, then bazel-build a C++ target forcing each
# kit's platform — proving the kit constraint_value disambiguates the right
# compiler. Self-skips without cmake / both compilers / bazel>=9.
e2e-meta-kits-build: converter
	scripts/meta-kits-build.sh

# kind:pyproject Phase B install-plan fallback (per-element
# auto-detection). Drives write-a against TWO kind:pyproject
# fixtures with --convert-element-pyproject + --pyproject-fallback
# set: a setuptools-based element (Phase A converts natively) +
# a pdm-backend element (Phase A refuses → pipeline shape).
# Asserts the dispatch routes each element to the correct
# rendered shape and surfaces the per-element refusal reason on
# write-a's stderr. Render-half only — both rendered shapes are
# already exercised by their respective gates (meta-pyproject.sh
# for native, the pipeline-shape gates for coarse).
e2e-meta-pyproject-fallback: converter
	scripts/meta-pyproject-fallback.sh

# Variable-resolver acceptance gate. Single kind:manual element
# (testdata/meta-project/vars-greet/) whose .bst overrides %{prefix}
# and defines a fresh %{greeting-dir} composing onto %{datadir} —
# exercises the project-default + element-override + custom-variable
# layers of variables.go's resolver. Asserts the rendered cmd carries
# the resolved path, no %{...} leaks through, and (when bazel >= 7
# is present) the install tarball lands the file at the overridden
# prefix.
e2e-meta-vars: converter
	scripts/meta-vars.sh

# kind:compose acceptance gate. Three elements (2 cmake + 1 compose)
# in testdata/meta-project/compose-greet/. compose renders the same
# filegroup-over-deps shape as kind:stack; this gate proves the
# rendering wiring works end-to-end and that compose's filegroup
# resolves through bazel against the staged cmake outputs.
e2e-meta-compose: converter
	scripts/meta-compose.sh

# kind:filter acceptance gate. Two elements (1 cmake parent + 1
# filter) in testdata/meta-project/filter-greet/. filter renders a
# single-dep filegroup with the .bst's `config: include / exclude /
# include-orphans` recorded as comments — domain-based slicing
# itself lands when the typed-filegroup wrapper for pipeline-kind
# outputs and the parent-public-data parser both arrive.
e2e-meta-filter: converter
	scripts/meta-filter.sh

# kind:import acceptance gate. Single import element with a
# kind:local source tree (testdata/meta-project/import-greet/);
# write-a stages the tree into project B verbatim and renders a
# filegroup over it. Validates the staged content is byte-identical
# to the fixture source and (when bazel >= 7 is present) the
# filegroup resolves through bazel.
e2e-meta-import: converter
	scripts/meta-import.sh

# kind:autotools acceptance gate. Single autotools element
# (testdata/meta-project/autotools-greet/) with a minimal ./configure
# script + Makefile.in template + greet.c. Drives bazel build A →
# extracts the install_tree.tar → asserts usr/bin/greet exists, runs,
# and prints the expected message. Validates the BuildStream autotools
# plugin's canonical %{autogen} / %{configure} / %{make} /
# %{make-install} chain expands correctly through the variable
# resolver under the project.conf prefix override.
e2e-meta-autotools: converter
	scripts/meta-autotools.sh

# Trace-driven kind:autotools native acceptance gate. Drives
# the autotools-greet fixture through write-a +
# --convert-element-trace + --build-tracer-bin; bazel build
# runs the tracer-wrapped install genrule + the native converter
# inline; the gate asserts the rendered BUILD.bazel.out contains
# native cc_binary targets recovered from the trace.
# Bazel's action cache (buildbarn in CI) handles cross-node
# convergence transparently — same action key, same outputs.
e2e-meta-autotools-native: converter
	scripts/meta-autotools-native.sh

# Trace-driven kind:autotools round-2 acceptance gate. Drives
# write-a with --autotools-round2 and asserts the new architectural
# shape: project A hosts a per-element converter genrule consuming
# @trace_<elem>//:trace (a load-time _trace_repo lookup against the
# REAPI ActionCache); project B's coarse install genrule no longer
# runs the converter inline — it ends with a trace-publish call
# that writes the AC entry the next pass-2 lookup will read.
#
# Render-half only (the full round-2 feedback loop has its own
# gate, e2e-meta-autotools-round2-live, that exercises the
# publish → lookup wire roundtrip against a real buildbarn +
# optionally bb_clientd). The publish/lookup wire contract is
# also covered in-process by go-test TestPublish_* + TestLookup_*
# in cmd/trace-{publish,lookup}.
e2e-meta-autotools-round2: converter
	scripts/meta-autotools-round2.sh

# Live-AC variant of the round-2 gate. Stands up buildbarn (and
# optionally bb_clientd + Bazel ≥ 9) and asserts the round-2
# rendezvous works through real REAPI:
#   - trace-publish writes an AC entry; trace-lookup reads it back.
#   - (When bb_clientd is on PATH) the published Directory blob
#     is mountable at the canonical bb_clientd layout
#     <mount>/cas/<instance>/blobs/<digest_function>/directory/<digest>/
#     — the same path the round-2 _trace_repo Bazel rule symlinks.
#   - (When bb_clientd + Bazel ≥ 9 are on PATH) a real
#     `bazel build //elements/greet:greet_build` against project A
#     resolves the AC-served trace through the parameterised
#     CAS_DIRECTORY_PREFIX and emits cc rules instead of the
#     placeholder shape. End-to-end coverage of the
#     RemoteOutputService-backed round-2 path.
#
# Skips cleanly when prereqs are missing (docker / bb_clientd /
# Bazel 9 each gate their respective half).
e2e-meta-autotools-round2-live:
	./tools/e2e-meta-autotools-round2-live.sh

# Remote-execution + build-without-the-bytes gate for the write-a +
# Bazel path against the real deploy/buildbarn/ stack. Renders project
# A, points it at bb-storage (:8980) + bb-scheduler (:8983) via an
# operator-style .bazelrc + platform(), and asserts the per-element
# convert-element-cmake genrule executes on a Buildbarn worker with
# the genrule output never materialised locally. This is the
# production-path replacement for the (now-deleted) orchestrator's
# Go-harness e2e-buildbarn / e2e-buildbarn-execute coverage. Self-
# skips when bazel >= 9 isn't on PATH; the script brings the stack
# up + down itself.
e2e-meta-buildbarn-re:
	./tools/e2e-meta-buildbarn-re.sh

# Multi-target trace-driven kind:autotools acceptance gate.
# Drives the autotools-multitarget fixture (multiple cc rules,
# multiple install dests, per-target CFLAGS) end-to-end through
# bazel build; asserts BUILD.bazel.out has the four expected
# rules and install-mapping.json captures all install dests
# with rule cross-references.
e2e-meta-autotools-multitarget: converter
	scripts/meta-autotools-multitarget.sh

# Per-target CFLAGS preservation gate. Fixture's
# `hotloop.o: CFLAGS += -O2` overrides global -O0 -g; the
# converter cross-references trace + make-db to keep -O2 in
# copts while stripping the global default flags.
e2e-meta-autotools-tu-optflags: converter
	scripts/meta-autotools-tu-optflags.sh

# Libtool dual-compile gate. Fixture's Makefile compiles foo.c
# twice: once with -fPIC -DPIC into .libs/foo.o (archived as
# libfoo_pic.a, the shared-prep lib) and once without PIC into
# foo.o (archived as libfoo.a, the static lib). Both .o paths
# collide on basename. The converter's exact-path correlation
# distinguishes them so the static lib's cc_library doesn't
# inherit -DPIC from the PIC compile.
e2e-meta-autotools-libtool-pic: converter
	scripts/meta-autotools-libtool-pic.sh

# Real-libtool emission gate. Same translation unit produces
# both libfoo.a (static, via ar/ranlib) AND libfoo.so.0.0.0
# (shared, via cc -shared) plus a libfoo.la text metadata
# file. Asserts the converter recovers ONLY the cc_library
# from the .a archive — the cc -shared event is filtered
# (Bazel's cc_library handles shared output on its own;
# emitting it as a cc_binary would be a duplicate / name
# collision) and the .la file participates in install-mapping
# but doesn't drive a rule.
e2e-meta-autotools-libtool-shared: converter
	scripts/meta-autotools-libtool-shared.sh

# Trace + make-db determinism gate. Drives the autotools-greet
# fixture through build-tracer twice (with different INSTALL_ROOT
# / BUILD_ROOT mktemp paths) and asserts the canonical trace +
# filtered make-db are byte-stable. Foundation for the 2-phase
# srckey design — without byte-stable foundation outputs, a
# registered trace can't be reused across builds.
e2e-meta-autotools-determinism: converter
	scripts/meta-autotools-determinism.sh

# Recursive-automake (SUBDIRS) collision gate. Two-subdir
# fixture where each subdir compiles its own source into
# parent.o (basename collision); the build-tracer's per-execve
# cwd capture lets the converter disambiguate the two compile
# events so each cc_library carries the right per-subdir
# defines / sources.
e2e-meta-autotools-subdirs: converter
	scripts/meta-autotools-subdirs.sh

# AC_CONFIG_HEADERS-style generated header gate. The fixture's
# configure step produces config.h from config.h.in; the
# pipeline's pre/post-configure header snapshot diff feeds
# convert-element-trace's --generated-headers flag so the
# emitted cc_library carries config.h in its hdrs.
e2e-meta-autotools-config-h: converter
	scripts/meta-autotools-config-h.sh

# Assembler-source (.S) recognition gate. libffi-style mixed
# C + arch-specific assembly: the converter must include .S
# files in cc_library srcs alongside .c. The fixture's sysv.S
# is x86_64; the build phase requires an x86_64 host.
e2e-meta-autotools-asm: converter
	scripts/meta-autotools-asm.sh

# Conditional-lowering acceptance gate. Single kind:manual element
# (testdata/meta-project/conditional-greet/) whose .bst declares
# (?): per-arch variable overrides. write-a lowers them into a
# project-B `cmd = select({...})` over @platforms//cpu:*; the
# driver asserts the select() shape + per-arch resolved paths.
e2e-meta-conditional: converter
	scripts/meta-conditional.sh

# kind:script acceptance gate. Single kind:script element
# (testdata/meta-project/script-greet/) with a flat config:commands
# list. Drives bazel build A → extracts the install_tree.tar →
# asserts usr/share/scripts/hello.txt has the expected content.
e2e-meta-script: converter
	scripts/meta-script.sh

# FDSDK reality check. Probes write-a against curated real
# freedesktop-sdk elements and reports which loader / handler gap
# each one hits first. Research / triage, not a gate. Per-kind
# coverage status: docs/fdsdk-coverage.md.
# Set FDSDK_DIR=/path/to/clone (or use the default /tmp/fdsdk).
fdsdk-reality-check:
	scripts/fdsdk-reality-check.sh

# M5 downstream-build acceptance gate — re-homed from the
# (now-deleted) orchestrator's TestE2E_BazelBuild onto the write-a +
# Bazel path: e2e-meta-cross-cmake renders a cross-element kind:cmake
# graph with write-a, builds project A, stage-b's into project B,
# and bazel-builds the consumer there (cross-element converted cc
# deps link end-to-end). See e2e-meta-cross-cmake above.

# M5 CMake-side acceptance gate. Configures a downstream find_package
# consumer against a convert-element-cmake-synthesized cmake-config
# bundle. No bazel required; just real cmake (already covered by
# check-cmake-toolchain).
e2e-cmake-consumer: check-cmake-toolchain converter
	$(GO) test -tags=e2e -run TestE2E_CMakeConsumer ./converter/cmd/convert-element-cmake/

# Toolchain plumbing e2e: runs cmake configure with a derived
# toolchain.cmake and asserts the file's CACHE INTERNAL vars
# survive into CMakeCache.txt (proves cmake loaded the file).
# Plus runs convert-element-cmake with --toolchain-cmake-file
# end to end to catch converter-side integration bugs. Validates
# the derive-toolchain -> toolchain.cmake -> cmakerun integration
# deterministically (the historical SkipReducesConfigureTime
# wall-clock variant was noise-bound and routinely flaked).
e2e-toolchain-skip: check-cmake-toolchain converter derive-toolchain
	$(GO) test -tags=e2e -run 'TestE2E_Toolchain_(LoadedByCmake|ConverterAcceptsToolchainFile)' ./converter/cmd/convert-element-cmake/

# Fidelity gate. Parameterized harness: hello-world is the smoke
# fixture; fmt (when fetched via `fetch-fmt`) is the real-world
# fixture. Each fixture builds the project two ways (cmake reference
# vs convert-element-cmake + bazel) and asserts symbol equivalence on the
# resulting library. Each new delta is recorded in
# docs/known-deltas.md.
e2e-fidelity: check-cmake-toolchain converter
	$(GO) test -tags=e2e -run TestE2E_Fidelity ./converter/cmd/convert-element-cmake/

# Same as e2e-fidelity but ensures the fmt fixture is fetched first
# so TestE2E_Fidelity_Fmt_SymbolEquivalent doesn't self-skip.
e2e-fidelity-fmt: check-cmake-toolchain converter fetch-fmt
	$(GO) test -tags=e2e -run TestE2E_Fidelity_Fmt ./converter/cmd/convert-element-cmake/

# Convert-and-rebuild fidelity gates per real-world fixture.
# Drives the full A-B-C cycle (cmake build → convert → bazel build
# → fidelity-compare) per docs/known-deltas.md's harness. Skips the
# bazel half cleanly when bazel isn't on PATH. Each project's
# benign-deltas allowlist lives at testdata/fidelity/<name>.allowlist.txt;
# operator-acknowledged benign entries (e.g. fmt's known template-
# inlining instantiations) pre-empt impactful classification.
e2e-fidelity-compare-zlib: check-cmake-toolchain converter fetch-zlib
	scripts/run-fidelity.sh \
		--project-name zlib \
		--source-root $(ZLIB_DIR) \
		--target zlibstatic \
		--cmake-artifact-pattern libz.a \
		--bazel-artifact-pattern libzlibstatic.a \
		--allowlist testdata/fidelity/zlib.allowlist.txt

e2e-fidelity-compare-spdlog: check-cmake-toolchain converter fetch-spdlog
	scripts/run-fidelity.sh \
		--project-name spdlog \
		--source-root $(SPDLOG_DIR) \
		--target spdlog \
		--artifact-pattern libspdlog.a \
		--allowlist testdata/fidelity/spdlog.allowlist.txt

e2e-fidelity-compare-fmt: check-cmake-toolchain converter fetch-fmt
	scripts/run-fidelity.sh \
		--project-name fmt \
		--source-root $(FMT_DIR) \
		--target fmt \
		--artifact-pattern libfmt.a \
		--allowlist testdata/fidelity/fmt.allowlist.txt

# Consumer-side fidelity gates. Compile a small consumer .c/.cpp
# twice — once against cmake's installed headers, once via Bazel as
# a cc_library depending on the converted target — and diff the
# resulting .o files. Catches converter regressions in
# INTERFACE_INCLUDE_DIRECTORIES exposure / strip_include_prefix /
# INTERFACE_COMPILE_DEFINITIONS propagation that the library-side
# nm-diff can't see (the library's own symbols are unaffected if a
# consumer can't reach a public header at all).
e2e-fidelity-compare-zlib-consumer: check-cmake-toolchain converter fetch-zlib
	scripts/run-fidelity.sh \
		--project-name zlib-consumer \
		--source-root $(ZLIB_DIR) \
		--target zlibstatic \
		--cmake-artifact-pattern libz.a \
		--bazel-artifact-pattern libzlibstatic.a \
		--consumer-file $(CURDIR)/testdata/fidelity/consumers/zlib_consumer.c \
		--consumer-bazel-dep :zlibstatic

e2e-fidelity-compare-fmt-consumer: check-cmake-toolchain converter fetch-fmt
	scripts/run-fidelity.sh \
		--project-name fmt-consumer \
		--source-root $(FMT_DIR) \
		--target fmt \
		--artifact-pattern libfmt.a \
		--consumer-file $(CURDIR)/testdata/fidelity/consumers/fmt_consumer.cpp \
		--consumer-bazel-dep :fmt \
		--allowlist testdata/fidelity/fmt-consumer.allowlist.txt

# spdlog consumer-side fidelity gate. spdlog is a *compiled* library
# (its CMake sets `target_compile_definitions(spdlog PUBLIC
# SPDLOG_COMPILED_LIB)`), so a consumer of the converted target compiles
# in compiled-lib mode (out-of-line refs into libspdlog.a). The cmake-side
# consumer compile is a bare `-I<install>/include` that wouldn't otherwise
# carry that PUBLIC define, so --consumer-cmake-cflags replays it to keep
# both sides in the same codegen mode (otherwise the cmake side compiles
# spdlog header-only and the two .o symbol sets are incomparable — a
# harness asymmetry, not a converter delta). 63/63 exported symbols match,
# 0 impactful deltas, no allowlist needed.
e2e-fidelity-compare-spdlog-consumer: check-cmake-toolchain converter fetch-spdlog
	scripts/run-fidelity.sh \
		--project-name spdlog-consumer \
		--source-root $(SPDLOG_DIR) \
		--target spdlog \
		--artifact-pattern libspdlog.a \
		--consumer-file $(CURDIR)/testdata/fidelity/consumers/spdlog_consumer.cpp \
		--consumer-bazel-dep :spdlog \
		--consumer-cmake-cflags '-DSPDLOG_COMPILED_LIB'

# nlohmann/json INTERFACE-only consumer-side fidelity gate.
# json has no static archive — library-mode comparison doesn't
# apply. Consumer-mode is the only meaningful signal: compile a
# small consumer.cpp against (a) cmake's installed json headers,
# (b) Bazel's converted :nlohmann_json cc_library, then diff
# the resulting .o pair. The harness's no_library mode (auto-
# detected when --consumer-file is set + no --artifact-pattern)
# skips the static-archive build + find for both sides.
#
# The :nlohmann_json cc_library is synthesized by the converter
# from the trace's add_library(nlohmann_json INTERFACE) call —
# shipped in PR #268's lowerInterfaceLibraries lift. Without
# that lift, the converter emitted only an install_directory__include
# filegroup, which a consumer cc_library can't depend on.
e2e-fidelity-compare-nlohmann-json-consumer: check-cmake-toolchain converter fetch-nlohmann-json
	scripts/run-fidelity.sh \
		--project-name nlohmann-json-consumer \
		--source-root $(JSON_DIR) \
		--target nlohmann_json \
		--cmake-flags '-DJSON_BuildTests=OFF' \
		--consumer-file $(CURDIR)/testdata/fidelity/consumers/json_consumer.cpp \
		--consumer-bazel-dep :nlohmann_json \
		--allowlist testdata/fidelity/nlohmann-json-consumer.allowlist.txt

# Catch2 library-side fidelity. Needs --lift-configure-file (the lib
# #includes the configure_file-generated catch2/catch_user_config.hpp);
# run-fidelity.sh auto-stages //tools:cmake-configure-file when the
# converted BUILD references it. The converter wires the genrule output's
# generated-includes/ dir into the cc_library's includes so the angle-
# bracket include resolves.
e2e-fidelity-compare-catch2: check-cmake-toolchain converter fetch-catch2
	scripts/run-fidelity.sh \
		--project-name catch2 \
		--source-root $(CATCH2_DIR) \
		--target Catch2 \
		--artifact-pattern libCatch2.a \
		--convert-flags '--lift-configure-file=true' \
		--allowlist testdata/fidelity/catch2.allowlist.txt

# libpng library-side fidelity. Exercises the full deferred-blocker set:
#   - cmake -E create_symlink install aliases (libpng16.pc -> libpng.pc)
#     skip via PR #350's install-compat-alias rule;
#   - the cmake -P script-generated headers (pnglibconf.h, pngprefix.h, …)
#     bake via --cmake-script-bake;
#   - find_package(ZLIB) resolves to @zlib via --imports-manifest
#     (libpng-imports.json maps ZLIB::ZLIB -> @zlib);
#   - --bazel-external adds the zlib BCR module so @zlib resolves.
# The cmake side needs zlib dev headers on the host (find_package(ZLIB)).
e2e-fidelity-compare-libpng: check-cmake-toolchain converter fetch-libpng
	scripts/run-fidelity.sh \
		--project-name libpng \
		--source-root $(LIBPNG_DIR) \
		--target png_static \
		--cmake-artifact-pattern libpng16.a \
		--bazel-artifact-pattern libpng_static.a \
		--bazel-target-label //:png_static \
		--cmake-flags '-DPNG_TESTS=OFF -DPNG_SHARED=OFF' \
		--convert-flags '--cmake-script-bake=true --imports-manifest=$(CURDIR)/testdata/fidelity/libpng-imports.json' \
		--bazel-external 'bazel_dep(name = "zlib", version = "1.3.1.bcr.5")' \
		--allowlist testdata/fidelity/libpng.allowlist.txt

# Real-Buildbarn validation. Brings up bb-storage via docker compose,
# runs the cache-share keystone test against grpc://127.0.0.1:8980,
# tears down. Replaces the in-process fake with actual Buildbarn code.
# Requires docker compose; no other toolchain bar.
BUILDBARN_COMPOSE := deploy/buildbarn/docker-compose.yml

buildbarn-up:
	@docker compose -f $(BUILDBARN_COMPOSE) up -d || ( \
		echo "buildbarn: compose up failed; dumping container state + logs:"; \
		docker compose -f $(BUILDBARN_COMPOSE) ps; \
		docker compose -f $(BUILDBARN_COMPOSE) logs --no-color --timestamps --tail=200; \
		exit 1; \
	)
	@echo "waiting for bb-storage + bb-scheduler + bb-worker HTTP /-/healthy..."
	@# Each container exposes /-/healthy on its diagnostics http port:
	@#   bb-storage   :9980
	@#   bb-scheduler :9982
	@#   bb-worker    :9981
	@# Polling only bb-storage masked schema-config crashes elsewhere;
	@# polling only storage + scheduler still missed bb-worker
	@# crashes (TestE2E_Buildbarn_Execute then saw "No workers exist
	@# for instance name prefix ..." because bb-worker had never
	@# managed to register against bb-scheduler:8984).
	@#
	@# Waiting for all three closes the visibility gap. Worker health
	@# also serves as a coarse "scheduler has at least seen the
	@# worker" signal — bb-worker only binds its diagnostics http
	@# AFTER its config parses and BEFORE registering with the
	@# scheduler, so a successful poll guarantees the config is
	@# valid; registration follows shortly after on the same process.
	@for i in $$(seq 1 180); do \
		if curl -fsS http://127.0.0.1:9980/-/healthy >/dev/null 2>&1 \
		   && curl -fsS http://127.0.0.1:9982/-/healthy >/dev/null 2>&1 \
		   && curl -fsS http://127.0.0.1:9981/-/healthy >/dev/null 2>&1; then \
			echo "ready in $${i}s"; exit 0; \
		fi; \
		sleep 1; \
	done; \
	echo "buildbarn stack did not become healthy within 180s; container logs:"; \
	docker compose -f $(BUILDBARN_COMPOSE) ps; \
	docker compose -f $(BUILDBARN_COMPOSE) logs --no-color --timestamps --tail=200; \
	exit 1

buildbarn-down:
	docker compose -f $(BUILDBARN_COMPOSE) down -v

# bb_clientd lifecycle. bb_clientd is the Bazel-9 companion daemon
# that replaces the dropped --unix_digest_hash_attribute_name
# fast-path; see docs/design/sources.md. Unlike the buildbarn
# executor stack, bb_clientd runs on the dev's host (not in
# docker) because it serves a host FUSE mount Bazel reads through.
#
# Configurable knobs (override on the make command line):
#   BB_CLIENTD_BIN  — path to the bb_clientd binary. If unset and
#                     not on PATH, the target prints install
#                     instructions and exits cleanly.
#   BB_CLIENTD_ROOT — host dir where the daemon writes mount /
#                     cache / outputs / grpc.sock. Default keeps
#                     dev users isolated under $HOME.
#   BB_CLIENTD_CAS  — REAPI gRPC address the daemon talks to.
#                     Defaults to the local make-buildbarn-up
#                     stack on 127.0.0.1:8980.
BB_CLIENTD_BIN  ?= $(shell command -v bb_clientd 2>/dev/null)
BB_CLIENTD_ROOT ?= $(HOME)/.cache/cmake-to-bazel/bb_clientd
BB_CLIENTD_CAS  ?= 127.0.0.1:8980
BB_CLIENTD_CONFIG := deploy/buildbarn/config/bb_clientd.jsonnet
BB_CLIENTD_PIDFILE := $(BB_CLIENTD_ROOT)/bb_clientd.pid

bb-clientd-up:
	@if [ -z "$(BB_CLIENTD_BIN)" ]; then \
		echo "bb-clientd-up: bb_clientd not found on PATH"; \
		echo ""; \
		echo "Install (recommended — pre-built binary):"; \
		echo "  curl -fsSL -o /usr/local/bin/bb_clientd \\"; \
		echo "    https://github.com/buildbarn/bb-clientd/releases/latest/download/bb_clientd.linux_amd64"; \
		echo "  chmod +x /usr/local/bin/bb_clientd"; \
		echo ""; \
		echo "Or build from source (requires Bazel; bb_clientd builds"; \
		echo "with Bazel, not the Go toolchain):"; \
		echo "  git clone https://github.com/buildbarn/bb-clientd && cd bb-clientd"; \
		echo "  bazel run --run_under cp //cmd/bb_clientd \\"; \
		echo "    \$$PWD/bb_clientd && sudo install bb_clientd /usr/local/bin/"; \
		echo ""; \
		echo "Then re-run with BB_CLIENTD_BIN pointing at the binary, or"; \
		echo "ensure bb_clientd is on \$$PATH. See CONTRIBUTING.md for the"; \
		echo "full development install requirements."; \
		exit 1; \
	fi
	@mkdir -p $(BB_CLIENTD_ROOT)/mount $(BB_CLIENTD_ROOT)/cache $(BB_CLIENTD_ROOT)/outputs
	@if [ -f $(BB_CLIENTD_PIDFILE) ] && kill -0 "$$(cat $(BB_CLIENTD_PIDFILE))" 2>/dev/null; then \
		echo "bb-clientd-up: already running (pid $$(cat $(BB_CLIENTD_PIDFILE)))"; \
		exit 0; \
	fi
	@echo "bb-clientd-up: starting against $(BB_CLIENTD_CAS), mount=$(BB_CLIENTD_ROOT)/mount"
	@BB_CLIENTD_ROOT=$(BB_CLIENTD_ROOT) BB_CLIENTD_CAS=$(BB_CLIENTD_CAS) \
	    nohup $(BB_CLIENTD_BIN) $(BB_CLIENTD_CONFIG) >$(BB_CLIENTD_ROOT)/bb_clientd.log 2>&1 & \
	    echo $$! > $(BB_CLIENTD_PIDFILE)
	@for i in $$(seq 1 30); do \
		if [ -S $(BB_CLIENTD_ROOT)/grpc.sock ] && mountpoint -q $(BB_CLIENTD_ROOT)/mount 2>/dev/null; then \
			echo "bb-clientd: ready (grpc=$(BB_CLIENTD_ROOT)/grpc.sock, mount=$(BB_CLIENTD_ROOT)/mount)"; \
			exit 0; \
		fi; \
		sleep 1; \
	done; \
	echo "bb-clientd-up: daemon failed to become ready within 30s"; \
	echo "log tail:"; tail -50 $(BB_CLIENTD_ROOT)/bb_clientd.log 2>/dev/null || true; \
	exit 1

bb-clientd-down:
	@if [ ! -f $(BB_CLIENTD_PIDFILE) ]; then \
		echo "bb-clientd-down: no pidfile at $(BB_CLIENTD_PIDFILE); nothing to do"; \
		exit 0; \
	fi
	@pid=$$(cat $(BB_CLIENTD_PIDFILE)); \
	if kill -0 "$$pid" 2>/dev/null; then \
		echo "bb-clientd-down: stopping pid $$pid"; \
		kill -TERM "$$pid"; \
		for i in 1 2 3 4 5; do \
			if ! kill -0 "$$pid" 2>/dev/null; then break; fi; \
			sleep 1; \
		done; \
		if kill -0 "$$pid" 2>/dev/null; then \
			echo "bb-clientd-down: forcing kill"; \
			kill -KILL "$$pid"; \
		fi; \
	fi; \
	rm -f $(BB_CLIENTD_PIDFILE); \
	if mountpoint -q $(BB_CLIENTD_ROOT)/mount 2>/dev/null; then \
		fusermount3 -u $(BB_CLIENTD_ROOT)/mount 2>/dev/null \
		  || fusermount  -u $(BB_CLIENTD_ROOT)/mount 2>/dev/null \
		  || true; \
	fi

# bb_clientd-backed hello-fuse e2e gate. Brings up buildbarn +
# bb_clientd, populates CAS via cmd/source-push, renders project A
# with --use-fuse-sources, and drives bazel build through the
# bb_clientd RemoteOutputService — Bazel's intended Bazel-9
# replacement for the dropped --unix_digest_hash_attribute_name
# xattr fast-path. Skips cleanly when bb_clientd or Bazel >= 9
# isn't on PATH. (cmd/cas-fuse + the in-process FUSE library it
# was built on were retired alongside this — bb_clientd is the
# production direction; cas-fuse was already legacy.)
e2e-hello-bbclientd: converter source-push write-a
	./tools/e2e-hello-bbclientd.sh

# Local-dev bazelisk bootstrap. Installs to ~/.local/bin by default
# (override with PREFIX=). The bazel-tagged e2e tests self-skip when
# bazel is missing; this gets them out of skip-mode without operators
# having to read README footnotes.
install-bazelisk:
	tools/install-bazelisk.sh

# Local-dev pinned-cmake bootstrap. Same pin CI installs and the
# worker image ships (CMAKE_VERSION above). Use this when your distro
# ships a different cmake than what defaultPlatform asserts; otherwise
# converter behavior on a newer cmake (e.g. ubuntu-24.04's 3.31.6)
# slips past local dev and only fires in CI.
install-cmake:
	tools/install-pinned-cmake.sh
# Fetch the M2 acceptance package out-of-band. Idempotent.
fetch-fmt:
	@if [ ! -d "$(FMT_DIR)" ]; then \
		git clone --depth 1 --branch $(FMT_VERSION) https://github.com/fmtlib/fmt.git "$(FMT_DIR)"; \
	else \
		echo "fmt already at $(FMT_DIR); rm -rf to refetch"; \
	fi

# Fetch zlib for the fidelity-compare gate. Idempotent.
fetch-zlib:
	@if [ ! -d "$(ZLIB_DIR)" ]; then \
		git clone --depth 1 --branch $(ZLIB_VERSION) https://github.com/madler/zlib.git "$(ZLIB_DIR)"; \
	else \
		echo "zlib already at $(ZLIB_DIR); rm -rf to refetch"; \
	fi

# Fetch spdlog for the fidelity-compare gate. Idempotent.
fetch-spdlog:
	@if [ ! -d "$(SPDLOG_DIR)" ]; then \
		git clone --depth 1 --branch $(SPDLOG_VERSION) https://github.com/gabime/spdlog.git "$(SPDLOG_DIR)"; \
	else \
		echo "spdlog already at $(SPDLOG_DIR); rm -rf to refetch"; \
	fi

# Fetch nlohmann/json for the consumer-mode fidelity gate. Idempotent.
fetch-nlohmann-json:
	@if [ ! -d "$(JSON_DIR)" ]; then \
		git clone --depth 1 --branch $(JSON_VERSION) https://github.com/nlohmann/json.git "$(JSON_DIR)"; \
	else \
		echo "nlohmann/json already at $(JSON_DIR); rm -rf to refetch"; \
	fi

# Fetch Catch2 for the configure_file-lift fidelity gate. Idempotent.
fetch-catch2:
	@if [ ! -d "$(CATCH2_DIR)" ]; then \
		git clone --depth 1 --branch $(CATCH2_VERSION) https://github.com/catchorg/Catch2.git "$(CATCH2_DIR)"; \
	else \
		echo "Catch2 already at $(CATCH2_DIR); rm -rf to refetch"; \
	fi

# Fetch libpng for the deferred-blocker fidelity gate. Idempotent.
fetch-libpng:
	@if [ ! -d "$(LIBPNG_DIR)" ]; then \
		git clone --depth 1 --branch $(LIBPNG_VERSION) https://github.com/pnggroup/libpng.git "$(LIBPNG_DIR)"; \
	else \
		echo "libpng already at $(LIBPNG_DIR); rm -rf to refetch"; \
	fi

# --- Diagnostic-survey corpus (docs/codemodel-consumption-audit.md) ---
# Each clone is idempotent. `fetch-survey` grabs the whole set.

# abseil-cpp: large modern target-based CMake; ships its own BUILD, so
# it doubles as a feature-flag idiom oracle.
fetch-abseil:
	@if [ ! -d "$(ABSEIL_DIR)" ]; then \
		git clone --depth 1 --branch $(ABSEIL_VERSION) https://github.com/abseil/abseil-cpp.git "$(ABSEIL_DIR)"; \
	else \
		echo "abseil-cpp already at $(ABSEIL_DIR); rm -rf to refetch"; \
	fi

# protobuf: protoc custom-command codegen + install(EXPORT) config-mode
# producer. Fills the cross-target-codegen / export-bundle shape.
fetch-re2:
	@if [ ! -d "$(RE2_DIR)" ]; then \
		git clone --depth 1 --branch $(RE2_VERSION) https://github.com/google/re2.git "$(RE2_DIR)"; \
	else \
		echo "re2 already at $(RE2_DIR); rm -rf to refetch"; \
	fi

fetch-protobuf:
	@if [ ! -d "$(PROTOBUF_DIR)" ]; then \
		git clone --depth 1 --branch $(PROTOBUF_VERSION) https://github.com/protocolbuffers/protobuf.git "$(PROTOBUF_DIR)"; \
	else \
		echo "protobuf already at $(PROTOBUF_DIR); rm -rf to refetch"; \
	fi

# googletest: enable_testing() + add_test / gtest_discover_tests — the
# real-world datapoint for the ctest edge-filtering path.
fetch-googletest:
	@if [ ! -d "$(GTEST_DIR)" ]; then \
		git clone --depth 1 --branch $(GTEST_VERSION) https://github.com/google/googletest.git "$(GTEST_DIR)"; \
	else \
		echo "googletest already at $(GTEST_DIR); rm -rf to refetch"; \
	fi

# Eigen: header-only INTERFACE library + config-mode export/components.
# Cheap; stresses the interface-lib + install-export path.
fetch-eigen:
	@if [ ! -d "$(EIGEN_DIR)" ]; then \
		git clone --depth 1 --branch $(EIGEN_VERSION) https://gitlab.com/libeigen/eigen.git "$(EIGEN_DIR)"; \
	else \
		echo "eigen already at $(EIGEN_DIR); rm -rf to refetch"; \
	fi

# llvm: the large stress test — ENABLE_EXPORTS, PCH, TableGen generated
# sources, forward-declared include dirs. Survey the llvm/ subdir (see
# docs/survey-corpus.md), not the monorepo root.
fetch-llvm:
	@if [ ! -d "$(LLVM_DIR)" ]; then \
		git clone --depth 1 --branch $(LLVM_VERSION) https://github.com/llvm/llvm-project.git "$(LLVM_DIR)"; \
	else \
		echo "llvm-project already at $(LLVM_DIR); rm -rf to refetch"; \
	fi

# VTK: heavy `cmake -P` codegen (vtkEncodeString) + target_precompile_headers.
# Fetched from the github.com/Kitware/VTK mirror because the canonical
# gitlab.kitware.com is blocked by the sandbox allowlist.
fetch-vtk:
	@if [ ! -d "$(VTK_DIR)" ]; then \
		git clone --depth 1 --branch $(VTK_VERSION) https://github.com/Kitware/VTK.git "$(VTK_DIR)"; \
	else \
		echo "vtk already at $(VTK_DIR); rm -rf to refetch"; \
	fi

# --- Regression corpus (docs/survey-corpus.md) ----------------------------
# Each member surfaced a real converter bug (now fixed) or is a clean
# control. Kept fetchable so a survey re-run guards against regressions.

# Boost.Core: alias-target lift ordering (#300). Modular boost lib; its
# CMakeLists configures standalone for the modular build.
fetch-boost-core:
	@if [ ! -d "$(BOOSTCORE_DIR)" ]; then \
		git clone --depth 1 --branch $(BOOSTCORE_VERSION) https://github.com/boostorg/core.git "$(BOOSTCORE_DIR)"; \
	else \
		echo "boost-core already at $(BOOSTCORE_DIR); rm -rf to refetch"; \
	fi

# zstd: workspace-root umbrella detection (#303). NOTE: the CMake root is
# the `build/cmake` SUBDIR, not the repo root — survey
# $(ZSTD_DIR)/build/cmake.
fetch-zstd:
	@if [ ! -d "$(ZSTD_DIR)" ]; then \
		git clone --depth 1 --branch $(ZSTD_VERSION) https://github.com/facebook/zstd.git "$(ZSTD_DIR)"; \
	else \
		echo "zstd already at $(ZSTD_DIR); rm -rf to refetch"; \
	fi

# libevent: pre-committed generated sources wrongly refused (#304).
fetch-libevent:
	@if [ ! -d "$(LIBEVENT_DIR)" ]; then \
		git clone --depth 1 --branch $(LIBEVENT_VERSION) https://github.com/libevent/libevent.git "$(LIBEVENT_DIR)"; \
	else \
		echo "libevent already at $(LIBEVENT_DIR); rm -rf to refetch"; \
	fi

# libxml2: clean control (no converter bugs found). Fetched from the
# github.com/GNOME/libxml2 mirror (canonical gitlab.gnome.org is fine too,
# but the mirror matches the rest of the corpus on github).
fetch-libxml2:
	@if [ ! -d "$(LIBXML2_DIR)" ]; then \
		git clone --depth 1 --branch $(LIBXML2_VERSION) https://github.com/GNOME/libxml2.git "$(LIBXML2_DIR)"; \
	else \
		echo "libxml2 already at $(LIBXML2_DIR); rm -rf to refetch"; \
	fi

# brotli: clean control (no converter bugs found).
fetch-brotli:
	@if [ ! -d "$(BROTLI_DIR)" ]; then \
		git clone --depth 1 --branch $(BROTLI_VERSION) https://github.com/google/brotli.git "$(BROTLI_DIR)"; \
	else \
		echo "brotli already at $(BROTLI_DIR); rm -rf to refetch"; \
	fi

# glog: an unresolved-genex include dir — the literal
# `$<TARGET_PROPERTY:glog,INCLUDE_DIRECTORIES>` on the glog_test INTERFACE
# library — aborted --split-packages with an invalid header-lib name; fixed
# (dropGenexIncludeDirs + planSplit backstop).
fetch-glog:
	@if [ ! -d "$(GLOG_DIR)" ]; then \
		git clone --depth 1 --branch $(GLOG_VERSION) https://github.com/google/glog.git "$(GLOG_DIR)"; \
	else \
		echo "glog already at $(GLOG_DIR); rm -rf to refetch"; \
	fi

# glm: a header-only INTERFACE lib (glm-header-only) whose include path is the
# source root, plus a compiled glm that PUBLIC-links it. The INTERFACE lib
# emitted empty (root include skipped) and the glm→glm-header-only edge was
# dropped — fixed (lowerInterfaceLibraries root-walk header ownership +
# routeTraceInterfaceLibDeps edge routing).
fetch-glm:
	@if [ ! -d "$(GLM_DIR)" ]; then \
		git clone --depth 1 --branch $(GLM_VERSION) https://github.com/g-truc/glm.git "$(GLM_DIR)"; \
	else \
		echo "glm already at $(GLM_DIR); rm -rf to refetch"; \
	fi

# cryptoauthlib: real-world recursive cmake via configure-time execute_process
# — the superbuild-at-configure idiom nested_cmake.go lifts. Its
# cmake/mbedtls.cmake does configure_file(third_party/CMakeLists-mbedtls.txt.in)
# → execute_process(${CMAKE_COMMAND} -G … .) → execute_process(${CMAKE_COMMAND}
# --build .) → file(GLOB) the result. NOTE the survey shape: the CMake root is
# the `lib/` subdir (project(cryptoauth C)), and the mbedtls integration is
# gated `option(ATCA_MBEDTLS … OFF)` — survey `$(CRYPTOAUTHLIB_DIR)/lib`
# with -DATCA_MBEDTLS=ON to actually hit the nested build.
#
# The nested build's "build" step is an ExternalProject_Add that DOWNLOADS an
# mbedtls tarball at configure time. So this fetch pre-fetches that tarball and
# repoints the ExternalProject URL at the local file:// copy (URL_HASH still
# verifies) — the configure-time nested cmake build then needs no network. The
# URL is read out of the pinned tag's .txt.in, so it tracks the pin. (This is
# also the seam where a repository-rule lift would hermetically vendor the
# dependency instead of cloning at configure time.)
fetch-cryptoauthlib:
	@if [ ! -d "$(CRYPTOAUTHLIB_DIR)" ]; then \
		git clone --depth 1 --branch $(CRYPTOAUTHLIB_VERSION) https://github.com/MicrochipTech/cryptoauthlib.git "$(CRYPTOAUTHLIB_DIR)"; \
		txtin="$(CRYPTOAUTHLIB_DIR)/third_party/CMakeLists-mbedtls.txt.in"; \
		url=$$(sed -n 's/.*URL[[:space:]]*"\(http[^"]*\)".*/\1/p' "$$txtin" | head -1); \
		if [ -n "$$url" ]; then \
			pfdir="$(CRYPTOAUTHLIB_DIR)/third_party/_prefetch"; \
			mkdir -p "$$pfdir"; \
			tarball="$$(cd "$$pfdir" && pwd)/$$(basename "$$url")"; \
			echo "pre-fetching configure-time mbedtls download: $$url"; \
			curl -fsSL "$$url" -o "$$tarball"; \
			sed -i "s|\"$$url\"|\"file://$$tarball\"|" "$$txtin"; \
			echo "repointed mbedtls ExternalProject URL at $$tarball (configure-time nested build is now network-free; URL_HASH still verifies)"; \
		else \
			echo "WARNING: no mbedtls download URL found in $$txtin; the configure-time nested build will fetch over the network"; \
		fi; \
	else \
		echo "cryptoauthlib already at $(CRYPTOAUTHLIB_DIR); rm -rf to refetch"; \
	fi

# mbedtls: wrapped `ctest -D Experimental` dashboard target wrongly lifted
# (fixed — isCMakeInternalCmd dashboard filter). NOTE: 3.6.x needs its
# `framework` git submodule, so this fetch recurses submodules.
fetch-mbedtls:
	@if [ ! -d "$(MBEDTLS_DIR)" ]; then \
		git clone --depth 1 --branch $(MBEDTLS_VERSION) --recurse-submodules --shallow-submodules https://github.com/Mbed-TLS/mbedtls.git "$(MBEDTLS_DIR)"; \
	else \
		echo "mbedtls already at $(MBEDTLS_DIR); rm -rf to refetch"; \
	fi

# cutlass + cuda-samples: NVIDIA CMake projects. NOTE: both need a CUDA
# toolkit (nvcc) on PATH to configure — without it the survey stops at
# cmake configure. Kept fetchable for environments that have CUDA.
fetch-cutlass:
	@if [ ! -d "$(CUTLASS_DIR)" ]; then \
		git clone --depth 1 --branch $(CUTLASS_VERSION) https://github.com/NVIDIA/cutlass.git "$(CUTLASS_DIR)"; \
	else \
		echo "cutlass already at $(CUTLASS_DIR); rm -rf to refetch"; \
	fi

fetch-cuda-samples:
	@if [ ! -d "$(CUDASAMPLES_DIR)" ]; then \
		git clone --depth 1 --branch $(CUDASAMPLES_VERSION) https://github.com/NVIDIA/cuda-samples.git "$(CUDASAMPLES_DIR)"; \
	else \
		echo "cuda-samples already at $(CUDASAMPLES_DIR); rm -rf to refetch"; \
	fi

# OpenBLAS: assembly kernels + Fortran/LAPACK + arch-conditional source
# selection + ~2460 targets — shapes nothing else in the corpus has. It
# surfaced the add_test/target name-collision robustness bug (an
# upstream `add_test(openblas_utest_ext <wrong binary>)` made the
# converter synthesize a cc_test colliding with the same-named
# executable; fixed via disambiguateTestNameCollisions). NOTE: survey
# with `-DNOFORTRAN=1 -DC_LAPACK=1` on hosts without gfortran (the
# converter's default source-root configure picks a working path, but
# the C_LAPACK route is the portable one); the asm/Fortran kernels
# themselves aren't Bazel-modelable, so the value is the C surface +
# codegen + scale.
fetch-openblas:
	@if [ ! -d "$(OPENBLAS_DIR)" ]; then \
		git clone --depth 1 --branch $(OPENBLAS_VERSION) https://github.com/OpenMathLib/OpenBLAS.git "$(OPENBLAS_DIR)"; \
	else \
		echo "openblas already at $(OPENBLAS_DIR); rm -rf to refetch"; \
	fi

# SDL: heavy platform-conditional source selection (37 if(WIN32/APPLE/
# LINUX/...) blocks) + Objective-C (.m) sources + target_precompile_headers.
# A standalone survey resolves to the one configured platform (so the
# platform-conditional select() arms come from the multi-platform fold,
# not here); the value is the platform-source-partition path + the objc
# language surface. Converts clean on Linux (1 benign execute_process
# rejection for `cmake -E make_directory`, 1 pch operator-action idiom).
fetch-sdl:
	@if [ ! -d "$(SDL_DIR)" ]; then \
		git clone --depth 1 --branch $(SDL_VERSION) https://github.com/libsdl-org/SDL.git "$(SDL_DIR)"; \
	else \
		echo "sdl already at $(SDL_DIR); rm -rf to refetch"; \
	fi

# curl: heavy find_package consumer (OpenSSL + ZLIB linked across
# hundreds of targets). A standalone survey emits ~1248
# find-package-dep-unresolved findings — all the "external / resolves in
# a real element graph" class (like protobuf's ZLIB), inflated by the
# same few libs re-linked everywhere. 0 rejections, 0 coverage — a
# stress test of the find_package path, not a converter bug.
fetch-curl:
	@if [ ! -d "$(CURL_DIR)" ]; then \
		git clone --depth 1 --branch $(CURL_VERSION) https://github.com/curl/curl.git "$(CURL_DIR)"; \
	else \
		echo "curl already at $(CURL_DIR); rm -rf to refetch"; \
	fi

# grpc: deep transitive deps + many install(FILES) directives + bundled
# third_party (zlib submodule). Surfaced the install_files name-collision
# bug (include/grpc vs include/grpc++ sanitize to the same target name;
# fixed via the usedNames disambiguation in directory_installers.go).
# NOTE: needs --recurse-submodules (third_party/zlib etc.) to configure.
fetch-grpc:
	@if [ ! -d "$(GRPC_DIR)" ]; then \
		git clone --depth 1 --branch $(GRPC_VERSION) --recurse-submodules --shallow-submodules https://github.com/grpc/grpc.git "$(GRPC_DIR)"; \
	else \
		echo "grpc already at $(GRPC_DIR); rm -rf to refetch"; \
	fi

# BuildGrid/buildbox — a BuildStream-ecosystem REAPI tooling monorepo
# (gitlab, not github). Surveys the custom protoc_compile() codegen wrapper
# and the monorepo conditional-tool layout. See docs/survey-corpus.md.
fetch-buildbox:
	@if [ ! -d "$(BUILDBOX_DIR)" ]; then \
		git clone --depth 1 --branch $(BUILDBOX_VERSION) https://gitlab.com/BuildGrid/buildbox/buildbox.git "$(BUILDBOX_DIR)"; \
	else \
		echo "buildbox already at $(BUILDBOX_DIR); rm -rf to refetch"; \
	fi

# Convenience aggregate: fetch the default survey corpus (the cheap four;
# llvm + vtk are fetched explicitly via fetch-llvm / fetch-vtk).
fetch-survey: fetch-abseil fetch-protobuf fetch-googletest fetch-eigen

# Convenience aggregate: fetch the regression corpus (the projects that
# surfaced past bugs + the clean controls). cutlass / cuda-samples need a
# CUDA toolkit to actually survey; they're fetched so the corpus is whole.
fetch-survey-regression: fetch-boost-core fetch-zstd fetch-libevent fetch-libxml2 fetch-brotli fetch-mbedtls fetch-cutlass fetch-cuda-samples fetch-openblas fetch-sdl fetch-curl fetch-grpc fetch-glog fetch-glm fetch-cryptoauthlib

# survey-gazelle: the strongest lens-2 (structural idiom) check — run the
# gazelle_cc round-trip on wild corpus projects (see
# scripts/survey-gazelle-roundtrip.sh and docs/survey-corpus.md). Hard-
# fails only when the converted BUILDs don't load under gazelle_cc (a
# non-idiomatic emission); reports first-pass drift and non-convergence
# as idiom datapoints (SURVEY_GAZELLE_STRICT=1 escalates non-convergence
# to a failure). Needs bazel>=9 + cmake + ninja (skips cleanly otherwise);
# META_GAZELLE_USE_HOST_GO=1 uses the host Go toolchain when go.dev egress
# is blocked. Defaults to the small/fast members; override
# SURVEY_GAZELLE_PROJECTS for others.
SURVEY_GAZELLE_PROJECTS ?= googletest=$(GTEST_DIR) brotli=$(BROTLI_DIR)
survey-gazelle:
	scripts/survey-gazelle-roundtrip.sh $(SURVEY_GAZELLE_PROJECTS)

# survey-multiplatform: configure each project under linux(native) +
# synthetic windows/darwin toolchains, fold the per-platform IRs into one
# BUILD with real select() arms, and report how many targets gained a
# platform select(). Makes platform/arch intent observable on the corpus
# (see scripts/survey-multiplatform.sh + scripts/survey-toolchains/).
# Needs cmake + ninja (skips cleanly otherwise). Platforms a project can't
# configure are dropped from the matrix; SURVEY_MP_PLATFORMS overrides the
# set. Defaults to the platform-conditional members.
SURVEY_MP_PROJECTS ?= sdl=$(SDL_DIR) brotli=$(BROTLI_DIR)
survey-multiplatform:
	scripts/survey-multiplatform.sh $(SURVEY_MP_PROJECTS)

# Regenerate golden files. Re-runs the pipeline, overwrites *.golden.
update-golden:
	$(GO) test ./... -update

# Re-run cmake on each sample project, capture File API reply dirs into testdata.
record-fixtures: check-cmake-toolchain
	./tools/fixtures/record-fileapi.sh

lint: vet fmt staticcheck

vet:
	$(GO) vet ./...

fmt:
	@out="$$(gofmt -l .)"; if [ -n "$$out" ]; then \
		echo "gofmt diffs in:"; echo "$$out"; \
		echo "run 'gofmt -w .'"; exit 1; \
	fi

# staticcheck catches what `go vet` doesn't: unused code (U1000 —
# how the dead emitCCTarget / fakecas executor surfaced), small
# simplifications (S1xxx), and deprecations (SA1019). Pinned for
# reproducibility — bump deliberately. Run via `go run` so there's no
# separate install/PATH step; setup-go's module cache memoizes it.
STATICCHECK_VERSION ?= 2026.1
staticcheck:
	$(GO) run honnef.co/go/tools/cmd/staticcheck@$(STATICCHECK_VERSION) ./...

# lint-complexity is the code-complexity lens (cyclomatic / cognitive / nesting
# / length / maintainability) — the axis go vet / gofmt / staticcheck don't
# cover. Driven by golangci-lint with a complexity-only config (.golangci.yml);
# pinned + run via `go run` to match the staticcheck idiom (no install step).
# BLOCKING: the soft-launch burndown reached green, so the CI step gates like
# the other checks; the tracked complexity giants carry documented //nolint
# directives keyed to the ROADMAP burndown.
GOLANGCI_LINT_VERSION ?= v2.12.2
lint-complexity:
	$(GO) run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION) run

# Per-toolchain check targets. Only the gates that actually exec
# the corresponding tool gate on the matching check target; render-
# only gates (kind:bazel, kind:script, kind:manual, finalize-b,
# unify-toolchains, etc.) have no host-tool prereq beyond Go.
# kind:meson / kind:pyproject / kind:autotools gates self-check
# their respective tools (meson / python3 / autotools chain) inside
# the script and skip the bazel-build half cleanly when missing.
#
# Pinned versions live in CMAKE_VERSION / NINJA_VERSION above; the
# orchestrator's defaultPlatform asserts these and the worker image
# (deploy/buildbarn/runner/Dockerfile) installs them. bwrap used to
# be in this check too on the assumption that cmakerun.Configure
# wrapped cmake in bwrap for hermeticity — it doesn't. Configure
# invokes cmake directly with controlled env (empty HOME etc.) and
# relies on Bazel's own action sandbox at the genrule layer.
check-cmake-toolchain:
	@command -v cmake >/dev/null || (echo "cmake not on PATH"; exit 1)
	@command -v ninja >/dev/null || (echo "ninja not on PATH"; exit 1)
	@cmake --version | head -1
	@ninja --version

clean:
	rm -rf $(BUILD_DIR)

# source-push driver targets (PR #59).
#
# The production path is `bst source push` invoked against a
# BuildStream project's source-caches:-configured CAS endpoint.
# Run from inside the FDSDK checkout:
#
#   bst source push --deps all <element>.bst
#
# That requires BuildStream installed on the host, which has a
# heavy native dep chain (ostree, gobject-introspection, ...).
# For test + dev workflows where bst isn't available but a
# populated --source-cache directory is, the in-tree
# cmd/source-push graph subcommand achieves the same effect by
# packing each <cache>/<key>/ tree directly into REAPI CAS:
#
#   make source-push-graph SOURCE_CACHE=/tmp/sc
#
# CI uses this path against a stood-up buildbarn so the
# end-to-end "populate CAS from local trees, read through CAS"
# round-trip is exercised on every change.

CAS_ADDR ?= 127.0.0.1:8980

# source-push-graph: walk a populated source-cache dir and push
# each tree to CAS. Prints a JSON manifest {key → digest} on
# stdout. Used by the e2e-source-push CI job and by devs who
# already have a --source-cache from `make e2e-orchestrate-...`.
source-push-graph: source-push
	@if [ -z "$(SOURCE_CACHE)" ]; then \
		echo "error: SOURCE_CACHE=<path> is required"; exit 2; \
	fi
	$(SOURCE_PUSH) graph --cas=$(CAS_ADDR) --source-cache=$(SOURCE_CACHE)

# fdsdk-source-push: thin convenience wrapper for the FDSDK
# end-to-end workflow. Two paths:
#
#   FDSDK_SOURCE_CACHE=<dir>   → cmd/source-push graph
#       (BuildStream-free; uploads pre-fetched trees indexed by
#       sourceKey). Test/dev path; used when the operator already
#       has a populated --source-cache directory.
#
#   FDSDK_DIR=<dir>            → tools/bst-source-push.sh
#       (real `bst source push` against the FDSDK BuildStream
#       project). Production path; requires BuildStream installed
#       on the host or in the cached venv (see `make bst-venv`).
#
# Pass exactly one. If both are set, FDSDK_DIR (the bst path)
# wins — that's the canonical mechanism.
fdsdk-source-push:
	@if [ -n "$(FDSDK_DIR)" ]; then \
		echo "fdsdk-source-push via real \`bst source push\` ($(FDSDK_DIR))"; \
		./tools/bst-source-push.sh "$(FDSDK_DIR)"; \
	elif [ -n "$(FDSDK_SOURCE_CACHE)" ]; then \
		$(MAKE) source-push-graph SOURCE_CACHE=$(FDSDK_SOURCE_CACHE); \
	else \
		echo "error: pass either FDSDK_DIR=<bst-project-dir> (real bst path) or FDSDK_SOURCE_CACHE=<cache-dir> (in-tree Go uploader path)"; \
		exit 2; \
	fi

# bst-venv: install BuildStream into a hermetic venv at
# ~/.cache/cmake-to-bazel/bst-venv/. Run once; tools/bst-source-push.sh
# auto-picks the venv's bst when no host bst is on PATH. Useful
# when the operator's distro doesn't ship bst or ships a stale
# version.
bst-venv:
	@mkdir -p $$HOME/.cache/cmake-to-bazel
	@if [ ! -d $$HOME/.cache/cmake-to-bazel/bst-venv ]; then \
		python3 -m venv $$HOME/.cache/cmake-to-bazel/bst-venv; \
	fi
	$$HOME/.cache/cmake-to-bazel/bst-venv/bin/pip install --upgrade pip
	$$HOME/.cache/cmake-to-bazel/bst-venv/bin/pip install BuildStream
	@$$HOME/.cache/cmake-to-bazel/bst-venv/bin/bst --version

# e2e-source-push: stand up buildbarn, pack a tiny synthetic
# source tree, push it via cas.UploadDir, verify CAS contents
# round-trip through cas.GetBlob. Runs in seconds and gates the
# wire-format end of the pipeline. Replaces the legacy
# casfuse-based variant alongside cmd/cas-fuse's retirement.
e2e-source-push: source-push buildbarn-up
	@$(GO) test -tags=buildbarn -run TestE2E_SourcePush ./internal/cas/...; \
	  ec=$$?; \
	  $(MAKE) buildbarn-down; \
	  exit $$ec
