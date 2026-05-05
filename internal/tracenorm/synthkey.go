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
// a given srckey.
//
// The Action is never executed — we only need its digest as a
// stable identifier the AC can store an ActionResult against.
// Recipe:
//
//	Command{ arguments: [synthKeyArgv0, srckey] }
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
// same key for the same srckey.
func SyntheticActionDigest(srckey string) (*cas.Digest, error) {
	cmd := &repb.Command{
		Arguments: []string{synthKeyArgv0, srckey},
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
