# Buildbarn validation deployment

A single-node Buildbarn instance for validating the write-a + Bazel
path's REAPI cache substrate (M5) and REAPI Execute (M3b) paths
against real Buildbarn code, vs the in-process fake the unit tests
use.

## Components

- `bb-storage`    — CAS + ActionCache (gRPC :8980)
- `bb-scheduler`  — queues Actions, dispatches to workers (gRPC :8983 client, :8984 worker)
- `bb-worker`     — pulls actions, materializes input roots, calls runner
- `bb-runner-bare` — exec's commands directly inside the shared build volume
- `bb_clientd` (host-side, see below) — Bazel-9 companion daemon
  serving a FUSE mount + RemoteOutputService gRPC; replaces the
  dropped `--unix_digest_hash_attribute_name` xattr fast-path.
  See [`docs/design/sources.md`](../../docs/design/sources.md).

No auth, localhost-only port mapping, file-backed blobstore at
1 GiB CAS / 64 MiB AC. Tear down with `docker compose down -v` to
wipe.

## Bring up

```sh
docker compose -f deploy/buildbarn/docker-compose.yml up -d
# wait for bb-storage healthcheck
until curl -fsS http://127.0.0.1:9980/-/healthy >/dev/null; do sleep 1; done
```

## Validate

Two acceptance gates depend on this stack:

```sh
make e2e-buildbarn         # M5 cache-share keystone (CAS+AC only)
make e2e-buildbarn-execute # M3b Execute keystone (full pipeline)
```

`e2e-buildbarn` runs the cache-share keystone twice against the
fdsdk-subset fixture, each pointed at `grpc://127.0.0.1:8980`; the
second pass must hit AC for every element and produce byte-identical
outputs.

`e2e-buildbarn-execute` submits a synthetic Action through
`grpc://127.0.0.1:8983` (the scheduler's client port) and verifies
the worker actually exec's it and returns a populated ActionResult.
The synthetic action is a `/bin/sh` script — it does NOT exercise
the converter end-to-end, since the bb-runner-bare image doesn't
have cmake/ninja installed. For full conversion through real
workers, build a custom worker image with the toolchain pre-baked.

## bb_clientd (Bazel-9 companion daemon)

bb_clientd runs **on the dev's host** (not in docker). It serves
a FUSE mount that lazily materialises CAS-resident bytes, plus a
`RemoteOutputService` gRPC endpoint Bazel 9 talks to via
`--remote_output_service=`. Together that restores the
"Bazel never re-hashes inputs the daemon already knows the digest
of" property the dropped `--unix_digest_hash_attribute_name` flag
used to provide on Bazel 7/8.

### Install

bb_clientd is a buildbarn project — the upstream repo
builds with **Bazel** (not the Go toolchain; `go install`
will not work). The dev loop doesn't need a source build:

```sh
# Recommended: pre-built binary from the bb-clientd repo.
# Statically linked, no runtime deps.
curl -fsSL -o /usr/local/bin/bb_clientd \
  https://github.com/buildbarn/bb-clientd/releases/latest/download/bb_clientd.linux_amd64
chmod +x /usr/local/bin/bb_clientd
```

Source build (only needed if you're modifying bb_clientd):

```sh
git clone https://github.com/buildbarn/bb-clientd && cd bb-clientd
bazel run --run_under cp //cmd/bb_clientd $PWD/bb_clientd
sudo install bb_clientd /usr/local/bin/
```

See `CONTRIBUTING.md`'s "Development install requirements"
section for the full set of host tools the dev loop uses.

Either way, point the lifecycle target at the binary if it's
not on `$PATH`:

```sh
make bb-clientd-up BB_CLIENTD_BIN=/path/to/bb_clientd
```

### Run

```sh
make buildbarn-up        # bring up the CAS bb_clientd talks to
make bb-clientd-up       # start the daemon (host-side, FUSE mount)
make e2e-hello-bbclientd # acceptance gate: full pipeline
make bb-clientd-down     # stop daemon, unmount
make buildbarn-down      # tear down the CAS
```

`bb-clientd-up` writes its mount / cache / output-path-state /
unix socket / pid / log under `$HOME/.cache/cmake-to-bazel/bb_clientd/`
by default; override with `BB_CLIENTD_ROOT=`. The daemon's log
tail is dumped if it doesn't become ready within 30s.

The acceptance gate `tools/e2e-hello-bbclientd.sh` skips cleanly
when bb_clientd or Bazel ≥ 9 isn't on PATH — bb_clientd install
is a per-host setup, not a default required step.

## Tear down

```sh
docker compose -f deploy/buildbarn/docker-compose.yml down -v
```

`-v` removes named volumes so a re-run starts cold. Omit `-v` to
inspect a populated CAS across runs.

## Schema drift

Image tags in `docker-compose.yml` are pinned. When bumping them,
reconcile each `.jsonnet` against:

- [`bb-storage` blobstore.proto + global.proto](https://github.com/buildbarn/bb-storage/tree/master/pkg/proto/configuration)
- [`bb-scheduler` scheduler/scheduler.proto](https://github.com/buildbarn/bb-remote-execution/blob/master/pkg/proto/configuration/bb_scheduler/bb_scheduler.proto)
- [`bb-worker` bb_worker.proto](https://github.com/buildbarn/bb-remote-execution/blob/master/pkg/proto/configuration/bb_worker/bb_worker.proto)
- [`bb-runner-bare` bb_runner.proto](https://github.com/buildbarn/bb-remote-execution/blob/master/pkg/proto/configuration/bb_runner/bb_runner.proto)
- [`bb_clientd` bb_clientd.proto](https://github.com/buildbarn/bb-storage/blob/master/pkg/proto/configuration/bb_clientd/bb_clientd.proto)

Test schema changes by running both make targets above against the
new images before merging.

## Production worker image (out of scope here)

bb-runner-bare exec's whatever command the action declares. For our
real conversion flow (`bin/convert-element-cmake ...`) the worker
container needs to provide:

- The bare runner binary (or any runner — bare is just easiest)
- `cmake`, `ninja` at the versions encoded in the orchestrator's
  `--platform` / `defaultPlatform` properties
- `/bin/sh` (already present in the official image)

A simple custom Dockerfile FROM the runner image, layered with `apt
install cmake ninja-build`, would close the loop for full
end-to-end conversion. We don't ship that here because the platform
properties + version pins cross too many deployment-specific
concerns; the documented path is "build your own runner image,
update worker.jsonnet's platform properties, point `bazel build`'s
`--remote_executor` at it".
