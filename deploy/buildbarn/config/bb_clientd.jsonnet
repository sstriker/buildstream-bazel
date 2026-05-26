// bb_clientd configuration — Bazel-9 companion daemon that
// replaces the dropped `--unix_digest_hash_attribute_name` xattr
// fast-path. The daemon serves a FUSE mount (digest-addressed
// CAS contents + per-build output paths) and speaks the
// `RemoteOutputService` protocol to Bazel; Bazel trusts the
// daemon's reported digests instead of re-hashing input bytes.
//
// Critical framing: bb_clientd is a Bazel-9 *companion*, in the
// same sense `bazelisk` is a Bazel companion. Adopting it does
// NOT pull in the whole buildbarn ecosystem — bb_clientd talks
// plain REAPI to whatever CAS endpoint we point it at. The
// `make buildbarn-up` deployment in this directory just happens
// to be the CAS we exercise it against in CI / dev.
//
// See:
//   - docs/design/sources.md — design doc for the chosen direction
//   - CONTRIBUTING.md — bb_clientd install instructions for dev
//   - https://github.com/buildbarn/bb-clientd/releases — public
//     pre-built binaries (linux_amd64 + darwin / freebsd / windows)
//   - https://github.com/buildbarn/bb-deployments/blob/master/docker-compose/config/common.libsonnet
//     — canonical config patterns we mirror
//
// Schema reference for upgrades:
//   https://github.com/buildbarn/bb-clientd/blob/main/pkg/proto/configuration/bb_clientd/bb_clientd.proto
//
// This config runs on the dev's host (not in docker), so paths
// use $HOME-relative locations resolved by extVar substitutions
// from the lifecycle script (`make bb-clientd-up`).

local bbClientdRoot = std.extVar('BB_CLIENTD_ROOT');
local casAddress = std.extVar('BB_CLIENTD_CAS');
local mountPath = bbClientdRoot + '/mount';

{
  blobstore: {
    // CAS + AC backends both point at the same REAPI endpoint.
    // For our deploy/buildbarn/ stack that's bb-storage on
    // localhost:8980. For a non-buildbarn CAS (EngFlow,
    // BuildBuddy, NativeLink, …) just change `address` —
    // bb_clientd doesn't care which implementation is on the
    // other end of the gRPC wire.
    //
    // Schema: BlobAccessConfiguration.grpc is a
    // GrpcBlobAccessConfiguration whose `client` is a
    // ClientConfiguration with `address`. Mirrors the canonical
    // bb-deployments common.libsonnet shape.
    contentAddressableStorage: {
      grpc: { client: { address: casAddress } },
    },
    actionCache: {
      grpc: { client: { address: casAddress } },
    },
  },
  global: {
    diagnosticsHttpServer: {
      httpServers: [{
        listenAddresses: [':9988'],
        authenticationPolicy: { allow: {} },
      }],
      enablePrometheus: true,
    },
  },
  // gRPC server Bazel connects to via
  // --experimental_remote_output_service. Unix socket so
  // multi-user hosts don't collide on a shared TCP port and so
  // permissions stay scoped to the daemon's owner.
  grpcServers: [{
    listenPaths: [bbClientdRoot + '/grpc.sock'],
    authenticationPolicy: { allow: {} },
  }],
  // The FUSE mount where Bazel-visible content lives. Per the
  // bb_clientd proto's MountConfiguration, the daemon serves
  // its own well-known layout under the mount_path:
  //
  //   <mount>/cas/<instance>/blobs/<digest_function>/directory/<digest>/
  //                         /file/<digest>
  //                         /executable/<digest>
  //                         /tree/<digest>/
  //                         /command/<digest>
  //   <mount>/outputs/<workspace>/   — Bazel's per-build output
  //                                    paths (lazily materialized)
  //   <mount>/scratch/               — writable scratch namespace
  //
  // rules/sources.bzl needs to learn this layout (currently
  // it expects `<mount>/blobs/directory/<digest>/`). Fixing
  // that is a follow-up; for round 1 the path adjustments are
  // documented in docs/design/sources.md.
  mount: {
    mountPath: mountPath,
    fuse: {
      // Mount contents are content-addressed (digest-keyed),
      // so the kernel can cache directory entries / inode
      // attributes for a long time without correctness loss.
      // Recommended values from the proto comments.
      directoryEntryValidity: '300s',
      inodeAttributeValidity: '300s',
      // No allowOther: the dev's user owns the mount.
      allowOther: false,
    },
  },
  // The pool of per-build output-path directories. Bazel asks
  // bb_clientd to create one of these at StartBuild and clean
  // it up at FinalizeBuild. Persisting them across daemon
  // restarts means a stopped/started bb_clientd doesn't lose
  // in-flight build state.
  outputPathPersistency: {
    stateDirectoryPath: bbClientdRoot + '/outputs',
    maximumStateFileSizeBytes: 16 * 1024 * 1024,
    maximumStateFileAge: '86400s',
  },
  // Local on-disk cache backing the file pool (writable
  // outputs + scratch). Bounded; bb_clientd evicts older
  // blobs when full.
  filePool: {
    blockDevice: {
      file: {
        path: bbClientdRoot + '/cache/file_pool',
        sizeBytes: 1073741824,  // 1 GiB
      },
    },
  },
  // In-memory cache of fetched CAS Directory objects.
  // Tunable per the daemon's RAM budget.
  directoryCache: {
    maximumCount: 100000,
    maximumSizeBytes: 16 * 1024 * 1024,
    cacheReplacementPolicy: 'LEAST_RECENTLY_USED',
  },
  maximumMessageSizeBytes: 16 * 1024 * 1024,
  maximumTreeSizeBytes: 64 * 1024 * 1024,
}
