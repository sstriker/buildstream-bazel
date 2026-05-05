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
//   - docs/bazel9-cas-fs.md — design doc for the chosen direction
//   - docs/sources-design.md — sources-route architecture
//   - https://github.com/buildbarn/bb-storage/blob/master/cmd/bb_clientd/main.go
//   - https://github.com/buildbarn/bb-deployments/tree/master/docker-compose
//     for the canonical bb-deployments example configs.
//
// Schema reference for upgrades:
//   https://github.com/buildbarn/bb-storage/blob/master/pkg/proto/configuration/bb_clientd/bb_clientd.proto
//
// This config is meant to run on the dev's host (not in
// docker), so paths use $HOME-relative locations resolved by
// the lifecycle script (`make bb-clientd-up`). Hard-coded paths
// in this file would surprise multi-user hosts; the lifecycle
// script writes a per-invocation config to a tempdir,
// substituting paths.

local bbClientdRoot = std.extVar('BB_CLIENTD_ROOT');
local casAddress = std.extVar('BB_CLIENTD_CAS');
local mountPath = bbClientdRoot + '/mount';
local cachePath = bbClientdRoot + '/cache';
local outputPath = bbClientdRoot + '/outputs';

{
  blobstore: {
    // CAS + AC backends both point at the same REAPI endpoint.
    // For our deploy/buildbarn/ stack that's bb-storage on
    // localhost:8980. For a non-buildbarn CAS (EngFlow,
    // BuildBuddy, NativeLink, …) just change `address` —
    // bb_clientd doesn't care which implementation is on the
    // other end of the gRPC wire.
    contentAddressableStorage: {
      grpc: { address: casAddress },
    },
    actionCache: {
      grpc: { address: casAddress },
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
  // gRPC server Bazel connects to via --remote_output_service.
  // Unix socket so multi-user hosts don't collide on a shared
  // TCP port and so permissions stay scoped to the daemon's
  // owner.
  grpcServers: [{
    listenAddresses: ['unix://' + bbClientdRoot + '/grpc.sock'],
    authenticationPolicy: { allow: {} },
  }],
  // The FUSE mount where Bazel-visible content lives:
  //   <mount>/cas/<digest>          — read-only CAS Directories
  //                                    addressed by digest
  //   <mount>/outputs/<workspace>/  — Bazel's per-build output
  //                                    paths (lazily materialized)
  // Bazel's --remote_output_service tells it to use this mount
  // as both input and output namespace.
  mount: {
    mountPath: mountPath,
    fuse: {
      // No allowOther: the dev's user owns the mount, no need
      // for cross-user visibility. Set true if you want Docker
      // containers (running as a different uid) to read through
      // the mount.
      allowOther: false,
      // directoryEntryValidity: how long the kernel may cache
      // dir-entry results before re-asking bb_clientd. Mount
      // contents are content-addressed (digest-keyed), so
      // they don't change underneath us — long validity is fine.
      directoryEntryValidity: '600s',
      inodeAttributeValidity: '600s',
    },
  },
  // The pool of per-build output-path directories. Bazel asks
  // bb_clientd to create one of these at StartBuild and clean
  // it up at FinalizeBuild. Persisting them across daemon
  // restarts means a stopped/started bb_clientd doesn't lose
  // in-flight build state.
  outputPathPersistency: {
    stateDirectoryPath: outputPath,
    maximumStateFileSizeBytes: 16 * 1024 * 1024,
    maximumStateFileAgeSeconds: 86400,
  },
  // Local on-disk cache of CAS blobs the daemon has fetched.
  // Bounded; bb_clientd evicts older blobs when full.
  filePool: {
    blockDevice: {
      file: {
        path: cachePath + '/file_pool',
        sizeBytes: 1073741824,  // 1 GiB
      },
    },
  },
  // Per-build identity hash. Lets bb_clientd disambiguate
  // concurrent builds running against the same daemon
  // (different shells, different repos).
  filesystemAccessCache: {
    inMemory: { cacheSize: 100000 },
  },
  maximumMessageSizeBytes: 16 * 1024 * 1024,
}
