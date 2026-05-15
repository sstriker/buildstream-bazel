package tracenorm

import (
	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"

	"github.com/sstriker/buildstream-bazel/internal/cas"
)

// synthKeyArgv0 is the first argv element of the Command proto whose
// digest seeds the synthetic Action. A neutral string that won't
// collide with any real action's argv[0]; the Command is never
// executed, only digested. Bumping this string rotates the AC
// keyspace if we ever need to invalidate published traces.
const synthKeyArgv0 = "cmake-to-bazel/trace-publish-marker/v1"

// configBundleKeyArgv0 namespaces the config-bundle keyspace
// separately from the trace keyspace. Same mechanism as
// synthKeyArgv0; the distinct argv0 means a published trace and
// a published config bundle for the same (srckey, platform) land
// at different AC keys. Required because the consumer side of
// the round-2 rendezvous can need either (or both) and looks them
// up independently — the trace_load action materializes the
// trace, the same trace_load action ALSO materializes the bundle
// via SyntheticConfigDigest.
const configBundleKeyArgv0 = "cmake-to-bazel/config-publish-marker/v1"

// synthKeyPlatformProperty is the REAPI Platform_Property name
// the synthetic Action carries when the caller supplies a
// non-empty platform tag. Distinct prefix so it can't collide
// with a real worker property the executor pool advertises;
// the synthetic Action is never dispatched, only digested.
const synthKeyPlatformProperty = "cmake-to-bazel/target-platform/v1"

// SyntheticActionDigest derives the REAPI Action digest both
// trace-publish and trace-lookup use as the AC rendezvous key for
// a given srckey, optionally partitioned by a platform tag.
//
// The Action is never executed — we only need its digest as a
// stable identifier the AC can store an ActionResult against.
// Recipe:
//
//	Command{ arguments: [synthKeyArgv0, srckey] }
//	Action {
//	  command_digest:    digest(Command)
//	  input_root_digest: digest(empty Directory)
//	  platform:          nil                            // platform == ""
//	  platform:          {properties: [{name: synthKeyPlatformProperty, value: platform}]}  // platform != ""
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
// under the same source-content identity. The mechanism is
// REAPI's native Action.Platform partitioning — the same axis
// real Actions are sharded along — rather than smuggling the
// tag into Command.Arguments.
//
// Empty platform preserves the historical keyspace exactly:
// Action.Platform stays nil so the serialized Action proto is
// byte-identical to today's, and the digest agrees with
// previously published AC entries. Single-platform operators
// upgrading past this revision see no change.
//
// The cmake Phase-A orchestrator path doesn't go through this
// AC rendezvous at all — its multi-platform fan-out partitions
// the action keyspace naturally via REAPI Action.Platform on
// the per-platform convert-element-cmake Actions themselves.
func SyntheticActionDigest(srckey, platform string) (*cas.Digest, error) {
	return syntheticDigest(synthKeyArgv0, srckey, platform)
}

// SyntheticConfigDigest is the config-bundle keyspace sibling of
// SyntheticActionDigest. Same shape (Command{argv0, srckey} +
// Action with optional Platform property), different argv0 marker
// (configBundleKeyArgv0). A trace and a config bundle for the
// same (srckey, platform) coexist at distinct AC keys.
//
// Used by trace-publish (with --config-bundle) and trace-lookup
// (with --out-config-bundle) to land / fetch the bundle alongside
// the trace from the round-2 install genrule. The convergence
// driver doesn't care which is which — both keys are populated by
// the same install action and consumed by the same trace_load
// action.
func SyntheticConfigDigest(srckey, platform string) (*cas.Digest, error) {
	return syntheticDigest(configBundleKeyArgv0, srckey, platform)
}

func syntheticDigest(argv0, srckey, platform string) (*cas.Digest, error) {
	cmd := &repb.Command{
		Arguments: []string{argv0, srckey},
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
	if platform != "" {
		action.Platform = &repb.Platform{
			Properties: []*repb.Platform_Property{
				{Name: synthKeyPlatformProperty, Value: platform},
			},
		}
	}
	actionDigest, _, err := cas.DigestProto(action)
	if err != nil {
		return nil, err
	}
	return actionDigest, nil
}
