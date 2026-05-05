.PHONY: all converter orchestrator diff history bst-translate derive-toolchain test test-e2e e2e-hello-world e2e-fmt \
        e2e-orchestrate e2e-orchestrate-scale e2e-bazel-build e2e-cmake-consumer e2e-toolchain-skip e2e-fidelity e2e-fidelity-fmt e2e-buildbarn e2e-buildbarn-execute \
        e2e-meta-hello e2e-meta-stack e2e-meta-manual e2e-meta-make e2e-meta-vars \
        e2e-meta-compose e2e-meta-filter e2e-meta-import e2e-meta-autotools \
        e2e-meta-autotools-native e2e-meta-autotools-round2 e2e-meta-autotools-round2-live e2e-meta-autotools-multitarget e2e-meta-autotools-tu-optflags e2e-meta-autotools-libtool-pic e2e-meta-autotools-libtool-shared e2e-meta-autotools-determinism e2e-meta-autotools-subdirs e2e-meta-autotools-config-h e2e-meta-autotools-asm \
        e2e-meta-conditional e2e-meta-script fdsdk-reality-check \
        buildbarn-up buildbarn-down bb-clientd-up bb-clientd-down e2e-hello-bbclientd install-bazelisk install-cmake convert-and-build \
        fetch-fmt update-golden record-fixtures lint vet fmt check-tools clean

# Pinned external tool versions. Hard-failed at runtime by the converter,
# enforced softly here for dev-loop visibility.
CMAKE_VERSION  ?= 3.28.3
NINJA_VERSION  ?= 1.11.1
BWRAP_VERSION  ?= 0.8.0

# M2 acceptance-package version. Bumping requires a re-run of
# TestE2E_Fmt_Converts since the *_test count assertion has a floor.
FMT_VERSION    ?= 11.0.2
FMT_DIR        ?= /tmp/fmt

GO        ?= go
GOFLAGS   ?=
BUILD_DIR ?= build
BIN_DIR   := $(BUILD_DIR)/bin

CONVERTER    := $(BIN_DIR)/convert-element
ORCHESTRATOR := $(BIN_DIR)/orchestrate
DIFF         := $(BIN_DIR)/orchestrate-diff
HISTORY      := $(BIN_DIR)/orchestrate-history
BST_TRANSLATE := $(BIN_DIR)/orchestrate-bst-translate
DERIVE_TOOLCHAIN := $(BIN_DIR)/derive-toolchain
WRITE_A      := $(BIN_DIR)/write-a
CAS_FUSE     := $(BIN_DIR)/cas-fuse
SOURCE_PUSH  := $(BIN_DIR)/source-push

all: converter orchestrator diff history bst-translate derive-toolchain write-a cas-fuse source-push

converter: $(CONVERTER)

orchestrator: $(ORCHESTRATOR)

diff: $(DIFF)

history: $(HISTORY)

bst-translate: $(BST_TRANSLATE)

derive-toolchain: $(DERIVE_TOOLCHAIN)

write-a: $(WRITE_A)

cas-fuse: $(CAS_FUSE)

source-push: $(SOURCE_PUSH)

$(CONVERTER):
	@mkdir -p $(BIN_DIR)
	CGO_ENABLED=0 $(GO) build $(GOFLAGS) -o $(CONVERTER) ./converter/cmd/convert-element

$(ORCHESTRATOR):
	@mkdir -p $(BIN_DIR)
	CGO_ENABLED=0 $(GO) build $(GOFLAGS) -o $(ORCHESTRATOR) ./orchestrator/cmd/orchestrate

$(DIFF):
	@mkdir -p $(BIN_DIR)
	CGO_ENABLED=0 $(GO) build $(GOFLAGS) -o $(DIFF) ./orchestrator/cmd/orchestrate-diff

$(HISTORY):
	@mkdir -p $(BIN_DIR)
	CGO_ENABLED=0 $(GO) build $(GOFLAGS) -o $(HISTORY) ./orchestrator/cmd/orchestrate-history

$(BST_TRANSLATE):
	@mkdir -p $(BIN_DIR)
	CGO_ENABLED=0 $(GO) build $(GOFLAGS) -o $(BST_TRANSLATE) ./orchestrator/cmd/orchestrate-bst-translate

$(DERIVE_TOOLCHAIN):
	@mkdir -p $(BIN_DIR)
	CGO_ENABLED=0 $(GO) build $(GOFLAGS) -o $(DERIVE_TOOLCHAIN) ./converter/cmd/derive-toolchain

$(WRITE_A):
	@mkdir -p $(BIN_DIR)
	CGO_ENABLED=0 $(GO) build $(GOFLAGS) -o $(WRITE_A) ./cmd/write-a

# cas-fuse links against go-fuse which uses cgo; leave CGO_ENABLED
# at the default. Daemon binary, not a hot path build.
$(CAS_FUSE):
	@mkdir -p $(BIN_DIR)
	$(GO) build $(GOFLAGS) -o $(CAS_FUSE) ./cmd/cas-fuse

$(SOURCE_PUSH):
	@mkdir -p $(BIN_DIR)
	CGO_ENABLED=0 $(GO) build $(GOFLAGS) -o $(SOURCE_PUSH) ./cmd/source-push

# Unit tests: pre-recorded File API fixtures, no cmake required.
test:
	$(GO) test ./...

# End-to-end tests: real cmake + bwrap. Gated behind build tag.
test-e2e: check-tools converter
	$(GO) test -tags=e2e ./converter/...

e2e-hello-world: check-tools converter
	$(GO) test -tags=e2e -run TestE2E_HelloWorld ./converter/...

e2e-fmt: check-tools converter fetch-fmt
	$(GO) test -tags=e2e -run TestE2E_Fmt ./converter/...

e2e-orchestrate: check-tools converter orchestrator
	$(GO) test -tags=e2e -run TestE2E_Orchestrate ./orchestrator/...

# Scale-fixture concurrency gate. Drives the orchestrator over a 50-element
# synthetic graph (orchestrator/testdata/fdsdk-scale/) at concurrency=1/8/32
# and asserts byte-identical per-element outputs across levels. Surfaces AC
# eviction races, queue-depth imbalances, and goroutine-pool bugs that the
# 3-element fdsdk-subset can't exercise. Uses the test binary's stub
# converter — no cmake/bwrap/ninja needed.
e2e-orchestrate-scale: orchestrator
	$(GO) test -run TestRun_Scale_DeterministicAcrossLevels -timeout 300s ./orchestrator/internal/orchestrator/...

# Phase 1 acceptance gate for the meta-project (Bazel-as-orchestrator)
# shape (docs/whole-project-plan.md). Renders project A and project B
# from the hello-world fixture via cmd/write-a, then drives the full
# two-pass pipeline: bazel build A (runs convert-element via genrule)
# -> stage A's BUILD.bazel.out into B -> bazel build + run B's smoke
# binary linking against the converted cc_library. Skips the bazel
# phases cleanly when bazel >= 7 isn't on PATH; the rendering checks
# alone are still a useful regression gate.
e2e-meta-hello: check-tools converter
	scripts/meta-hello.sh

# Phase 2 acceptance gate for the meta-project. Multi-element fixture
# (testdata/meta-project/two-libs/) — two kind:cmake elements + one
# kind:stack bundling them. Validates write-a's per-kind dispatch +
# graph-shape rendering + the kind:stack handler's filegroup
# composition end-to-end through both projects, with a smoke binary
# linking against both cmake elements. Same bazel-availability
# gating as e2e-meta-hello.
e2e-meta-stack: check-tools converter
	scripts/meta-stack.sh

# kind:bazel passthrough acceptance gate. Element's source tree
# already contains a BUILD.bazel; write-a stages it verbatim into
# project B and runs no translator. The gate asserts the staged
# BUILD is byte-identical to the source's authored one and the
# resulting cc_binary builds + runs end-to-end.
e2e-meta-bazel-passthrough: check-tools converter
	scripts/meta-bazel-passthrough.sh

# Cross-element kind:cmake dep gate. Two kind:cmake elements where
# the consumer (cons) depends on the producer (prod) via
# find_package(prod CONFIG REQUIRED) + target_link_libraries(prod::prod).
# Asserts: write-a renders the cross-element bundle staging + an
# imports.json synthesis file; bazel build of cons resolves the
# dep against the staged bundle; convert-element's STATIC IMPORTED
# dep recovery emits deps = ["//elements/prod:prod"] in the
# converted output.
e2e-meta-cross-cmake: check-tools converter
	scripts/meta-cross-cmake.sh

# Phase 3 first-cut acceptance gate. Single kind:manual element
# (testdata/meta-project/manual-greet/) whose install-commands
# stage a greeting file under %{install-root}%{prefix}/share/.
# Drives bazel build A → extracts the resulting install_tree.tar →
# asserts content. Validates write-a's manual handler + variable
# substitution end-to-end. Same bazel-availability gating as
# e2e-meta-hello / e2e-meta-stack.
e2e-meta-manual: check-tools converter
	scripts/meta-manual.sh

# Phase 3 sibling acceptance gate. Single kind:make element
# (testdata/meta-project/make-greet/) with a Makefile that builds
# a tiny `greet` binary and installs it. Drives bazel build A →
# extracts the resulting install_tree.tar → asserts usr/bin/greet
# exists, runs, and prints the expected message. Validates that
# kind:make's pipelineHandler defaults (`make` / `make install`)
# resolve correctly without an explicit config: in the .bst.
e2e-meta-make: check-tools converter
	scripts/meta-make.sh

# Variable-resolver acceptance gate. Single kind:manual element
# (testdata/meta-project/vars-greet/) whose .bst overrides %{prefix}
# and defines a fresh %{greeting-dir} composing onto %{datadir} —
# exercises the project-default + element-override + custom-variable
# layers of variables.go's resolver. Asserts the rendered cmd carries
# the resolved path, no %{...} leaks through, and (when bazel >= 7
# is present) the install tarball lands the file at the overridden
# prefix.
e2e-meta-vars: check-tools converter
	scripts/meta-vars.sh

# kind:compose acceptance gate. Three elements (2 cmake + 1 compose)
# in testdata/meta-project/compose-greet/. compose renders the same
# filegroup-over-deps shape as kind:stack; this gate proves the
# rendering wiring works end-to-end and that compose's filegroup
# resolves through bazel against the staged cmake outputs.
e2e-meta-compose: check-tools converter
	scripts/meta-compose.sh

# kind:filter acceptance gate. Two elements (1 cmake parent + 1
# filter) in testdata/meta-project/filter-greet/. filter renders a
# single-dep filegroup with the .bst's `config: include / exclude /
# include-orphans` recorded as comments — domain-based slicing
# itself lands when the typed-filegroup wrapper for pipeline-kind
# outputs and the parent-public-data parser both arrive.
e2e-meta-filter: check-tools converter
	scripts/meta-filter.sh

# kind:import acceptance gate. Single import element with a
# kind:local source tree (testdata/meta-project/import-greet/);
# write-a stages the tree into project B verbatim and renders a
# filegroup over it. Validates the staged content is byte-identical
# to the fixture source and (when bazel >= 7 is present) the
# filegroup resolves through bazel.
e2e-meta-import: check-tools converter
	scripts/meta-import.sh

# kind:autotools acceptance gate. Single autotools element
# (testdata/meta-project/autotools-greet/) with a minimal ./configure
# script + Makefile.in template + greet.c. Drives bazel build A →
# extracts the install_tree.tar → asserts usr/bin/greet exists, runs,
# and prints the expected message. Validates the BuildStream autotools
# plugin's canonical %{autogen} / %{configure} / %{make} /
# %{make-install} chain expands correctly through the variable
# resolver under the project.conf prefix override.
e2e-meta-autotools: check-tools converter
	scripts/meta-autotools.sh

# Trace-driven kind:autotools native acceptance gate. Drives
# the autotools-greet fixture through write-a +
# --convert-element-autotools + --build-tracer-bin; bazel build
# runs the tracer-wrapped install genrule + the native converter
# inline; the gate asserts the rendered BUILD.bazel.out contains
# native cc_binary targets recovered from the trace.
# Bazel's action cache (buildbarn in CI) handles cross-node
# convergence transparently — same action key, same outputs.
e2e-meta-autotools-native: check-tools converter
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
e2e-meta-autotools-round2: check-tools converter
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

# Multi-target trace-driven kind:autotools acceptance gate.
# Drives the autotools-multitarget fixture (multiple cc rules,
# multiple install dests, per-target CFLAGS) end-to-end through
# bazel build; asserts BUILD.bazel.out has the four expected
# rules and install-mapping.json captures all install dests
# with rule cross-references.
e2e-meta-autotools-multitarget: check-tools converter
	scripts/meta-autotools-multitarget.sh

# Per-target CFLAGS preservation gate. Fixture's
# `hotloop.o: CFLAGS += -O2` overrides global -O0 -g; the
# converter cross-references trace + make-db to keep -O2 in
# copts while stripping the global default flags.
e2e-meta-autotools-tu-optflags: check-tools converter
	scripts/meta-autotools-tu-optflags.sh

# Libtool dual-compile gate. Fixture's Makefile compiles foo.c
# twice: once with -fPIC -DPIC into .libs/foo.o (archived as
# libfoo_pic.a, the shared-prep lib) and once without PIC into
# foo.o (archived as libfoo.a, the static lib). Both .o paths
# collide on basename. The converter's exact-path correlation
# distinguishes them so the static lib's cc_library doesn't
# inherit -DPIC from the PIC compile.
e2e-meta-autotools-libtool-pic: check-tools converter
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
e2e-meta-autotools-libtool-shared: check-tools converter
	scripts/meta-autotools-libtool-shared.sh

# Trace + make-db determinism gate. Drives the autotools-greet
# fixture through build-tracer twice (with different INSTALL_ROOT
# / BUILD_ROOT mktemp paths) and asserts the canonical trace +
# filtered make-db are byte-stable. Foundation for the 2-phase
# srckey design — without byte-stable foundation outputs, a
# registered trace can't be reused across builds.
e2e-meta-autotools-determinism: check-tools converter
	scripts/meta-autotools-determinism.sh

# Recursive-automake (SUBDIRS) collision gate. Two-subdir
# fixture where each subdir compiles its own source into
# parent.o (basename collision); the build-tracer's per-execve
# cwd capture lets the converter disambiguate the two compile
# events so each cc_library carries the right per-subdir
# defines / sources.
e2e-meta-autotools-subdirs: check-tools converter
	scripts/meta-autotools-subdirs.sh

# AC_CONFIG_HEADERS-style generated header gate. The fixture's
# configure step produces config.h from config.h.in; the
# pipeline's pre/post-configure header snapshot diff feeds
# convert-element-autotools' --generated-headers flag so the
# emitted cc_library carries config.h in its hdrs.
e2e-meta-autotools-config-h: check-tools converter
	scripts/meta-autotools-config-h.sh

# Assembler-source (.S) recognition gate. libffi-style mixed
# C + arch-specific assembly: the converter must include .S
# files in cc_library srcs alongside .c. The fixture's sysv.S
# is x86_64; the build phase requires an x86_64 host.
e2e-meta-autotools-asm: check-tools converter
	scripts/meta-autotools-asm.sh

# Conditional-lowering acceptance gate. Single kind:manual element
# (testdata/meta-project/conditional-greet/) whose .bst declares
# (?): per-arch variable overrides. write-a lowers them into a
# project-B `cmd = select({...})` over @platforms//cpu:*; the
# driver asserts the select() shape + per-arch resolved paths.
e2e-meta-conditional: check-tools converter
	scripts/meta-conditional.sh

# kind:script acceptance gate. Single kind:script element
# (testdata/meta-project/script-greet/) with a flat config:commands
# list. Drives bazel build A → extracts the install_tree.tar →
# asserts usr/share/scripts/hello.txt has the expected content.
e2e-meta-script: check-tools converter
	scripts/meta-script.sh

# FDSDK reality check. Probes write-a against curated real
# freedesktop-sdk elements and reports which loader / handler gap
# each one hits first. Research / triage, not a gate — see
# docs/fdsdk-reality-check.md for the prioritized punch list.
# Set FDSDK_DIR=/path/to/clone (or use the default /tmp/fdsdk).
fdsdk-reality-check:
	scripts/fdsdk-reality-check.sh

# M5 downstream-build acceptance gate. Requires bazel/bazelisk on PATH
# in addition to the standard cmake/ninja/bwrap; if absent the test
# self-skips (runtime LookPath check). Spins the orchestrator against
# the FDSDK subset, then runs `bazel build //:smoke` against a
# downstream consumer that depends on a converted element.
e2e-bazel-build: check-tools converter orchestrator
	$(GO) test -tags=e2e -run TestE2E_BazelBuild ./orchestrator/...

# M5 CMake-side acceptance gate. Configures a downstream find_package
# consumer against the orchestrator's synth-prefix tree. No bazel
# required; just real cmake + bwrap (already covered by check-tools).
e2e-cmake-consumer: check-tools converter orchestrator
	$(GO) test -tags=e2e -run TestE2E_CMakeConsumer ./orchestrator/...

# Toolchain configure-skip e2e: runs the orchestrator twice against the
# fdsdk-subset (without and with --toolchain-cmake-file) and asserts
# the second pass's cumulative cmake-configure wall-clock is shorter.
# Validates the derive-toolchain -> toolchain.cmake -> cmakerun
# integration end-to-end.
e2e-toolchain-skip: check-tools converter orchestrator derive-toolchain
	$(GO) test -tags=e2e -run TestE2E_Toolchain_SkipReducesConfigureTime ./orchestrator/...

# Fidelity gate. Parameterized harness: hello-world is the smoke
# fixture; fmt (when fetched via `fetch-fmt`) is the real-world
# fixture. Each fixture builds the project two ways (cmake reference
# vs convert-element + bazel) and asserts symbol equivalence on the
# resulting library. Each new delta is recorded in
# docs/fidelity-known-deltas.md.
e2e-fidelity: check-tools converter
	$(GO) test -tags=e2e -run TestE2E_Fidelity ./orchestrator/...

# Same as e2e-fidelity but ensures the fmt fixture is fetched first
# so TestE2E_Fidelity_Fmt_SymbolEquivalent doesn't self-skip.
e2e-fidelity-fmt: check-tools converter fetch-fmt
	$(GO) test -tags=e2e -run TestE2E_Fidelity_Fmt ./orchestrator/...

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
# fast-path; see docs/design/bazel9-cas-fs.md. Unlike the buildbarn
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

# bb_clientd-backed hello-fuse e2e gate. Mirrors
# tools/e2e-hello-fuse.sh's pipeline but uses bb_clientd's
# mount + --remote_output_service in place of cmd/cas-fuse +
# the dropped --unix_digest_hash_attribute_name flag. Skips
# cleanly when bb_clientd or Bazel >= 9 isn't on PATH.
e2e-hello-bbclientd: converter source-push write-a
	./tools/e2e-hello-bbclientd.sh

e2e-buildbarn: buildbarn-up
	$(GO) test -tags=buildbarn -run TestE2E_Buildbarn ./orchestrator/...
	$(MAKE) buildbarn-down

# M3b execution validation against real Buildbarn workers (scheduler +
# bb-worker + bb-runner-bare from the same docker-compose stack).
# Runs both the synthetic /bin/sh round-trip test and the real-converter
# test (which depends on the custom worker image at
# deploy/buildbarn/runner/Dockerfile having cmake/ninja/bwrap installed).
# `make converter` is a prerequisite of the real-converter test; if the
# binary isn't present that subtest skips with a clear message.
e2e-buildbarn-execute: converter buildbarn-up
	@# On test failure, dump per-container logs before tearing down so
	@# CI shows worker/scheduler/runner state when "No workers exist"
	@# or similar registration-side errors fire. /-/healthy says "the
	@# diagnostics http server bound" but says nothing about whether
	@# bb-worker has finished registering against bb-scheduler:8984;
	@# the logs disambiguate.
	@$(GO) test -tags=buildbarn -run TestE2E_Buildbarn_Execute ./internal/reapi/...; \
	  ec=$$?; \
	  if [ $$ec -ne 0 ]; then \
	    echo "=== container state ==="; \
	    docker compose -f $(BUILDBARN_COMPOSE) ps -a; \
	    echo "=== bb-runner-bare inspect (RestartCount, ExitCode, OOMKilled) ==="; \
	    docker inspect bb-runner-bare --format='RestartCount={{.RestartCount}} State.Status={{.State.Status}} ExitCode={{.State.ExitCode}} OOMKilled={{.State.OOMKilled}} Error="{{.State.Error}}"' || true; \
	    echo "=== bb-runner-bare healthcheck history ==="; \
	    docker inspect bb-runner-bare --format='{{range .State.Health.Log}}{{.Start}} ExitCode={{.ExitCode}} Output={{.Output | printf "%q"}}{{println}}{{end}}' || true; \
	    echo "=== bb-runner-bare procs ==="; \
	    docker top bb-runner-bare || true; \
	    echo "=== bb-runner-bare find for runner.sock anywhere ==="; \
	    docker compose -f $(BUILDBARN_COMPOSE) exec -T bb-runner-bare find / -name 'runner.sock' -o -name 'runner' -type s 2>/dev/null || true; \
	    echo "=== bb-runner-bare /worker tree ==="; \
	    docker compose -f $(BUILDBARN_COMPOSE) exec -T bb-runner-bare ls -lR /worker 2>&1 || true; \
	    echo "=== bb-runner-bare /tmp ==="; \
	    docker compose -f $(BUILDBARN_COMPOSE) exec -T bb-runner-bare ls -la /tmp 2>&1 || true; \
	    for svc in bb-worker bb-scheduler bb-runner-bare bb-storage; do \
	      echo "=== $$svc logs ==="; \
	      docker compose -f $(BUILDBARN_COMPOSE) logs --no-color --timestamps --tail=100 $$svc; \
	    done; \
	    echo "=== bb-runner-bare /worker/build view ==="; \
	    docker compose -f $(BUILDBARN_COMPOSE) exec -T bb-runner-bare ls -la /worker/build/ || true; \
	    echo "=== bb-worker /worker/build view ==="; \
	    docker compose -f $(BUILDBARN_COMPOSE) exec -T bb-worker ls -la /worker/build/ || true; \
	    echo "=== bb-worker uid/gid ==="; \
	    docker compose -f $(BUILDBARN_COMPOSE) exec -T bb-worker id || true; \
	    echo "=== bb-runner-bare uid/gid ==="; \
	    docker compose -f $(BUILDBARN_COMPOSE) exec -T bb-runner-bare id || true; \
	  fi; \
	  $(MAKE) buildbarn-down; \
	  exit $$ec

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

# Full pipeline: convert an FDSDK-style root, then bazel build inside
# the resulting <out>/. Requires bazel or bazelisk on PATH (run
# `make install-bazelisk` first if missing). Override BAZEL_TARGET to
# build a specific label instead of //... .
#
#   make convert-and-build FDSDK_ROOT=/path/to/fdsdk OUT=/tmp/out
#   make convert-and-build FDSDK_ROOT=/path/to/fdsdk OUT=/tmp/out BAZEL_TARGET=//elements/components/foo:bar
convert-and-build: converter orchestrator
	@[ -n "$(FDSDK_ROOT)" ] || (echo "set FDSDK_ROOT=path/to/fdsdk-root"; exit 1)
	@[ -n "$(OUT)" ] || (echo "set OUT=output-dir"; exit 1)
	$(ORCHESTRATOR) --fdsdk-root $(FDSDK_ROOT) --out $(OUT) --converter $(CONVERTER) --bazel-build $(or $(BAZEL_TARGET),//...)

# Fetch the M2 acceptance package out-of-band. Idempotent.
fetch-fmt:
	@if [ ! -d "$(FMT_DIR)" ]; then \
		git clone --depth 1 --branch $(FMT_VERSION) https://github.com/fmtlib/fmt.git "$(FMT_DIR)"; \
	else \
		echo "fmt already at $(FMT_DIR); rm -rf to refetch"; \
	fi

# Regenerate golden files. Re-runs the pipeline, overwrites *.golden.
update-golden:
	$(GO) test ./... -update

# Re-run cmake on each sample project, capture File API reply dirs into testdata.
record-fixtures: check-tools
	./tools/fixtures/record-fileapi.sh

lint: vet fmt

vet:
	$(GO) vet ./...

fmt:
	@out="$$(gofmt -l .)"; if [ -n "$$out" ]; then \
		echo "gofmt diffs in:"; echo "$$out"; \
		echo "run 'gofmt -w .'"; exit 1; \
	fi

check-tools:
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
# end-to-end "populate CAS from local trees, mount with cas-fuse,
# read through" round-trip is exercised on every change.

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
# source-cache, push it via cmd/source-push, verify CAS contents
# round-trip through casfuse.NewTree. The fast cousin of the
# full project-B e2e (PR #60) — runs in seconds and gates the
# wire-format end of the pipeline.
e2e-source-push: source-push buildbarn-up
	@$(GO) test -tags=buildbarn -run TestE2E_SourcePush ./internal/casfuse/...; \
	  ec=$$?; \
	  $(MAKE) buildbarn-down; \
	  exit $$ec

# e2e-hello-fuse: PR #60's verification target. Walks the full
# pipeline (source-push → cas-fuse → write-a --use-fuse-sources
# → structural verification of the generated project A). The
# bazel-build step is gated behind RUN_BAZEL=1 — set it when the
# host has bazel + cmake + ninja + bwrap.
e2e-hello-fuse: converter cas-fuse source-push write-a
	./tools/e2e-hello-fuse.sh
