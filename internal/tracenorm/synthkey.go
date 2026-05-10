package tracenorm

import (
	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"

	"github.com/sstriker/cmake-to-bazel/internal/cas"
)

// synthKeyArgv0 is the first argv element of the Command proto whose
// digest seeds the synthetic Action. A neutral string that won't
// collide with any real action's argv[0]; the Command is never
// executed, only digested. Bumping this string rotates the AC
// keyspace if we ever need to invalidate published traces.
const synthKeyArgv0 = "cmake-to-bazel/trace-publish-marker/v1"

// SyntheticActionDigest derives the REAPI Action digest both
// trace-publish and trace-lookup use as the AC rendezvous key for
// a given srckey, optionally partitioned by a platform tag.
//
// The Action is never executed — we only need its digest as a
// stable identifier the AC can store an ActionResult against.
// Recipe:
//
//	Command{ arguments: [synthKeyArgv0, srckey] }              // platform == ""
//	Command{ arguments: [synthKeyArgv0, srckey, platform] }    // platform != ""
//	Action {
//	  command_digest:    digest(Command)
//	  input_root_digest: digest(empty Directory)
//	  do_not_cache:      false
//	}
//	digest(Action) → AC key
//
// Both sides compute it deterministically (cas.DigestProto
// serializes with proto.MarshalOptions{Deterministic: true}); a
// publisher and a lookup running on different machines hit the
// same key for the same (srckey, platform) pair.
//
// The platform argument is the partition tag for round-2
// trace-driven kinds whose install layout / build graph can
// legitimately diverge across target platforms (.so vs .dylib,
// multiarch lib dirs, arch-tagged binary names). When non-empty,
// different platforms' traces land at distinct AC keys for the
// same srckey, so a linux trace doesn't shadow a darwin one
// under the same source-content identity.
//
// Empty platform preserves the historical keyspace exactly:
// `Arguments` stays a 2-element slice so single-platform
// operators don't see their existing AC entries invalidated.
// The cmake Phase-A orchestrator path doesn't go through this
// AC rendezvous at all — its multi-platform fan-out partitions
// the action keyspace naturally via REAPI Action.Platform on
// the per-platform convert-element Actions themselves.
func SyntheticActionDigest(srckey, platform string) (*cas.Digest, error) {
	args := []string{synthKeyArgv0, srckey}
	if platform != "" {
		args = append(args, platform)
	}
	cmd := &repb.Command{
		Arguments: args,
	}
	cmdDigest, _, err := cas.DigestProto(cmd)
	if err != nil {
		return nil, err
	}

	emptyDir := &repb.Directory{}
	emptyDirDigest, _, err := cas.DigestProto(emptyDir)
	if err != nil {
		return nil, err
	}

	action := &repb.Action{
		CommandDigest:   cmdDigest,
		InputRootDigest: emptyDirDigest,
	}
	actionDigest, _, err := cas.DigestProto(action)
	if err != nil {
		return nil, err
	}
	return actionDigest, nil
}
