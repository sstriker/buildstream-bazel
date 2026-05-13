// trace-lookup is the consumer side of the round-2 rendezvous.
// It runs at Bazel load time inside project A's _trace_repo
// repository rule (see cmd/write-a/traces_bzl.go). Given a
// srckey (and optionally a platform tag), it computes the
// synthetic Action digest, queries the REAPI ActionCache,
// verifies the trace blob is still in CAS, and prints the
// trace's root Directory digest on stdout.
//
// The repo rule reads stdout: empty ⇒ AC miss / blob missing /
// no CAS configured ⇒ empty trace fileset (the converter then
// emits a coarse pass-3 genrule). Non-empty stdout ⇒ AC hit ⇒
// rule symlinks `<mount>/blobs/directory/<digest>` and the
// converter emits fine cc_library / cc_binary.
//
// Usage:
//
//	trace-lookup --cas=<grpc-addr> --srckey=<hex> \
//	    [--platform=<tag>] [--instance=<name>]
//
// --platform: optional. Empty / omitted preserves the historical
// single-keyspace shape — single-platform operators see no
// behaviour change; AC entries published before the platform
// flag was added remain reachable. Non-empty partitions the AC
// keyspace per target platform (via REAPI Action.Platform), so
// the lookup hits only the trace the matching publish side
// tagged with the same value.
//
// Exit codes:
//
//	0 — successful lookup (hit or miss; stdout carries the result).
//	1 — hard error (gRPC dial failure, AC backend error, etc.).
//	2 — usage error (missing required flag).
//
// Lookup miss is NOT an error — it's the normal "haven't built
// this srckey yet" case the round-1 boot pipeline expects.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"

	"github.com/sstriker/buildstream-bazel/internal/cas"
	"github.com/sstriker/buildstream-bazel/internal/tracenorm"
)

func main() {
	log.SetFlags(0)
	addr := flag.String("cas", "", "REAPI gRPC address (host:port). Empty/unset ⇒ lookup miss (load-time fallback shape).")
	instance := flag.String("instance", "", "REAPI instance name; matches the AC endpoint's multi-tenancy prefix.")
	srckey := flag.String("srckey", "", "per-element srckey hex (the content of srckey.txt); seeds the synthetic AC key.")
	platform := flag.String("platform", "", "optional platform tag (e.g. linux_x86_64) partitioning the synthetic AC keyspace. Must match the tag the publishing side (trace-publish) used for the same srckey. Empty preserves the historical single-keyspace shape — single-platform operators upgrading past this flag keep their previously published entries reachable.")
	flag.Parse()

	if *srckey == "" {
		flag.Usage()
		os.Exit(2)
	}
	// Empty CAS address ⇒ no remote configured for this build;
	// emit empty stdout (lookup miss). The repo rule treats that
	// as "no trace yet" and the converter falls back to coarse.
	if *addr == "" {
		return
	}
	ctx := context.Background()
	store, err := cas.NewGRPCStore(ctx, cas.GRPCConfig{
		Endpoint:     *addr,
		InstanceName: *instance,
		Insecure:     true,
	})
	if err != nil {
		// gRPC dial errors at this layer are "we couldn't
		// even reach the cache" — same operational story as
		// "no cache configured." Emit miss + non-zero exit
		// so the repo rule's stderr capture surfaces the
		// reason, but the rule's err==nil path treats stdout
		// as authoritative (empty ⇒ miss).
		fmt.Fprintf(os.Stderr, "trace-lookup: dial cas %s: %v\n", *addr, err)
		os.Exit(1)
	}
	defer store.Close()

	digest, err := lookup(ctx, store, *srckey, *platform)
	if err != nil {
		fmt.Fprintf(os.Stderr, "trace-lookup: %v\n", err)
		os.Exit(1)
	}
	if digest == nil {
		// Miss: empty stdout, exit 0.
		return
	}
	fmt.Printf("%s/%d\n", digest.Hash, digest.SizeBytes)
}

// lookup queries the AC for srckey's synthetic key, validates
// the referenced root Directory blob is still in CAS, and
// returns the digest. Returns (nil, nil) on a clean miss
// (AC entry absent OR blob evicted); both publisher
// "haven't built this yet" and CAS-eviction shapes route
// through the same coarse-fallback path.
func lookup(ctx context.Context, store cas.Store, srckey, platform string) (*cas.Digest, error) {
	key, err := tracenorm.SyntheticActionDigest(srckey, platform)
	if err != nil {
		return nil, fmt.Errorf("synth-key: %w", err)
	}
	ar, err := store.GetActionResult(ctx, key)
	if err != nil {
		if errors.Is(err, cas.ErrNotFound) {
			return nil, nil
		}
		return nil, fmt.Errorf("get-ac: %w", err)
	}
	if len(ar.OutputDirectories) == 0 {
		return nil, nil
	}
	rootDigest := ar.OutputDirectories[0].RootDirectoryDigest
	if rootDigest == nil {
		return nil, nil
	}
	missing, err := store.FindMissing(ctx, []*cas.Digest{rootDigest})
	if err != nil {
		return nil, fmt.Errorf("findmissing: %w", err)
	}
	if len(missing) > 0 {
		// AC entry survived but the trace blob was evicted.
		// Treat as miss; round-1 will republish.
		return nil, nil
	}
	return rootDigest, nil
}
