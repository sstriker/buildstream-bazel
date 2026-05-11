// trace-publish takes a per-element trace.log (and optionally a
// make-db.txt) produced by a kind's coarse-build genrule, packs
// it as a REAPI Directory, uploads every blob to CAS, and writes
// an ActionResult into the action cache under a synthetic key
// derived from the element's srckey and an optional platform tag.
//
// File set published depends on the calling kind:
//
//   - autotools / make / makemaker / modulebuild / manual /
//     script: trace.log + make-db.txt (convert-element-trace
//     consumes both).
//   - cmake round-2 fallback: trace.log only (cmake's converter
//     derives its IR from the trace + the cmake File API, not
//     from make-db).
//
// The synthetic key is the rendezvous: cmd/trace-lookup, run by
// project A's _trace_repo Bazel rule at load time, computes the
// same key from the same (srckey, platform) and reads back the
// AC entry. AC hit + verified blobs ⇒ the lookup prints the
// trace's root Directory digest, which the repo rule symlinks
// under cas-fuse / bb_clientd's `<mount>/blobs/directory/<digest>`
// mount.
//
// Usage:
//
//	trace-publish \
//	    --cas=<grpc-addr> \
//	    --srckey=<hex>      \
//	    --trace=<path>      \
//	    [--make-db=<path>]  \
//	    [--platform=<tag>]  \
//	    [--instance=<name>]
//
// --make-db: optional. Empty / omitted → publish trace.log only
// (cmake round-2 shape). Non-empty → publish the 2-file
// autotools-family shape.
//
// --platform: optional. Empty / omitted preserves the historical
// single-keyspace shape (single-platform operators see no
// change; previously published AC entries stay reachable).
// Non-empty partitions the AC keyspace per target platform via
// REAPI's native Action.Platform mechanism so two platforms'
// traces against the same source content don't collide. The
// matching trace-lookup invocation must pass the same tag.
//
// Idempotent: republishing the same canonicalized trace is a no-op
// (CAS is content-addressable; AC update with an identical body is
// a no-op too).
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"

	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"

	"github.com/sstriker/cmake-to-bazel/internal/cas"
	"github.com/sstriker/cmake-to-bazel/internal/tracenorm"
)

func main() {
	log.SetFlags(0)
	addr := flag.String("cas", "", "REAPI gRPC address (host:port). Required.")
	instance := flag.String("instance", "", "REAPI instance name; matches the publishing CAS endpoint's multi-tenancy prefix.")
	srckey := flag.String("srckey", "", "per-element srckey hex (the content of srckey.txt); seeds the synthetic AC key.")
	tracePath := flag.String("trace", "", "path to the canonicalized trace.log produced by build-tracer.")
	makeDBPath := flag.String("make-db", "", "optional path to the filtered make-db.txt produced by the genrule's `make -np` post-step. Pass it for trace-driven kinds whose converter expects make-db.txt in the published Directory (autotools / make / makemaker / modulebuild / manual / script — convert-element-trace reads it); omit it for cmake round-2 fallback (cmake's converter derives its IR from the trace + the cmake File API). Empty/omitted publishes a trace.log-only Directory; downstream converters that look for make-db.txt will see it absent and should handle that as their kind dictates.")
	sourceRoot := flag.String("source-root", "", "absolute path to the element's source tree. Mirrors build-tracer's --source-root: when set, the defensive re-canonicalization filters openat lines to source-relative paths and strips the volatile fd return value (the trace doubles as a configure-time read oracle). When empty, openat lines drop entirely — preserves the legacy AC byte schema for elements not opted into the oracle.")
	platform := flag.String("platform", "", "optional platform tag (e.g. linux_x86_64) partitioning the synthetic AC keyspace for round-2 trace-driven kinds whose install layout / build graph diverges across target platforms. Empty preserves the historical single-keyspace shape — single-platform operators upgrading past this flag keep their previously published AC entries valid. The matching trace-lookup invocation in rules/traces.bzl must pass the same tag for the publish/lookup rendezvous to hit.")
	flag.Parse()

	if *addr == "" || *srckey == "" || *tracePath == "" {
		flag.Usage()
		os.Exit(2)
	}

	ctx := context.Background()
	store, err := cas.NewGRPCStore(ctx, cas.GRPCConfig{
		Endpoint:     *addr,
		InstanceName: *instance,
		Insecure:     true,
	})
	if err != nil {
		log.Fatalf("trace-publish: dial cas %s: %v", *addr, err)
	}
	defer store.Close()

	if err := publish(ctx, store, *srckey, *platform, *tracePath, *makeDBPath, *sourceRoot); err != nil {
		log.Fatalf("trace-publish: %v", err)
	}
}

// publish does the work; factored out so the in-process roundtrip
// test (which uses cas.LocalStore) shares the upload + AC-update
// logic with the gRPC binary path.
func publish(ctx context.Context, store cas.Store, srckey, platform, tracePath, makeDBPath, sourceRoot string) error {
	traceBody, err := os.ReadFile(tracePath)
	if err != nil {
		return fmt.Errorf("read trace: %w", err)
	}
	// make-db.txt is optional: trace-driven autotools-family
	// kinds publish it (convert-element-trace consumes it
	// downstream), but cmake round-2 fallback has no equivalent
	// (cmake's converter reads the trace + File API directly).
	// Empty path → skip the read; the staged Directory below
	// omits the make-db.txt entry rather than landing an empty
	// file so a cmake publish doesn't tax the autotools
	// converter's parser with a zero-byte make-db.
	var makeDBBody []byte
	if makeDBPath != "" {
		makeDBBody, err = os.ReadFile(makeDBPath)
		if err != nil {
			return fmt.Errorf("read make-db: %w", err)
		}
		makeDBBody = tracenorm.FilterMakeDB(makeDBBody)
	}

	// Defensive re-canonicalization. The pipeline genrule already
	// produces canonical bytes (build-tracer applies pid + temp
	// path normalization; sed strips the make-db variant lines).
	// Re-applying here means a publisher running against an older
	// build-tracer or a custom genrule still lands a stable AC
	// entry across machines.
	//
	// sourceRoot is threaded through so the openat read-oracle
	// filter applies the same way it did at trace time. When
	// sourceRoot is empty, openat lines drop and the bytes match
	// the legacy execve-only schema.
	traceBody = tracenorm.CanonicalizeBytesWith(traceBody, tracenorm.Options{SourceRoot: sourceRoot})

	// Pack the staged trace dir (trace.log alone for cmake
	// round-2; trace.log + make-db.txt for the autotools-family
	// kinds) as a REAPI Directory and upload every blob (root
	// Directory + each file content) via FindMissing/PutBlob.
	// We do this as PackDir + manual
	// upload (rather than UploadDir) so we can also build +
	// upload the Tree proto: Buildbarn's bb-storage wraps its
	// AC backend in a completeness checker that walks the AR's
	// OutputDirectory[].TreeDigest at GetActionResult time and
	// rejects entries whose Tree (or its referenced files) are
	// missing from CAS. So we MUST publish the Tree blob, not
	// just the root Directory blob, for the AC entry to be
	// readable.
	stage, err := os.MkdirTemp("", "trace-publish-stage-*")
	if err != nil {
		return fmt.Errorf("stage tmpdir: %w", err)
	}
	defer os.RemoveAll(stage)
	if err := os.WriteFile(filepath.Join(stage, "trace.log"), traceBody, 0o644); err != nil {
		return fmt.Errorf("write staged trace: %w", err)
	}
	if makeDBPath != "" {
		if err := os.WriteFile(filepath.Join(stage, "make-db.txt"), makeDBBody, 0o644); err != nil {
			return fmt.Errorf("write staged make-db: %w", err)
		}
	}
	tree, err := cas.PackDir(stage)
	if err != nil {
		return fmt.Errorf("pack trace dir: %w", err)
	}
	if err := uploadTree(ctx, store, tree); err != nil {
		return fmt.Errorf("upload trace dir: %w", err)
	}

	// Build + upload the REAPI Tree proto. Tree.Root carries
	// the root Directory; Children is empty for our flat
	// 1-or-2-file layout. The Tree proto's bytes (digested
	// deterministically via cas.DigestProto) are what the AC
	// entry's TreeDigest references; the bb-storage
	// completeness checker walks this to verify CAS coverage.
	reapiTree := tree.AsReapiTree()
	treeDigest, treeBlob, err := cas.DigestProto(reapiTree)
	if err != nil {
		return fmt.Errorf("digest tree proto: %w", err)
	}
	if err := store.PutBlob(ctx, treeDigest, treeBlob); err != nil {
		return fmt.Errorf("upload tree proto: %w", err)
	}

	// Compute the synthetic Action digest and publish an
	// ActionResult whose single OutputDirectory carries both
	// the Tree digest (canonical REAPI shape, required by
	// bb-storage's completeness checker) and the root
	// Directory digest (consumed by trace-lookup directly so
	// cas-fuse / bb_clientd can serve `<mount>/blobs/directory/
	// <root>` without a Tree-proto round trip).
	actionDigest, err := tracenorm.SyntheticActionDigest(srckey, platform)
	if err != nil {
		return fmt.Errorf("synth-key: %w", err)
	}
	ar := &repb.ActionResult{
		OutputDirectories: []*repb.OutputDirectory{
			{
				Path:                "trace",
				TreeDigest:          treeDigest,
				RootDirectoryDigest: tree.RootDigest,
			},
		},
	}
	if err := store.UpdateActionResult(ctx, actionDigest, ar); err != nil {
		return fmt.Errorf("update ac: %w", err)
	}
	fmt.Printf("%s/%d\n", tree.RootDigest.Hash, tree.RootDigest.SizeBytes)
	return nil
}

// uploadTree mirrors cas.UploadDir's logic but takes a
// pre-packed *cas.Tree so the caller retains access to the
// tree's structure (root proto, Files map) for downstream
// uses — here, building the REAPI Tree proto whose digest
// goes into the AR's TreeDigest.
func uploadTree(ctx context.Context, store cas.Store, tree *cas.Tree) error {
	dirBodies := make(map[string][]byte, len(tree.Directories))
	digests := make([]*cas.Digest, 0, len(tree.Directories)+len(tree.Files))
	for h, d := range tree.Directories {
		body, err := cas.MarshalDeterministic(d)
		if err != nil {
			return fmt.Errorf("marshal directory %s: %w", h, err)
		}
		dirBodies[h] = body
		digests = append(digests, &cas.Digest{Hash: h, SizeBytes: int64(len(body))})
	}
	for h, p := range tree.Files {
		info, err := os.Stat(p)
		if err != nil {
			return fmt.Errorf("stat %s: %w", p, err)
		}
		digests = append(digests, &cas.Digest{Hash: h, SizeBytes: info.Size()})
	}
	missing, err := store.FindMissing(ctx, digests)
	if err != nil {
		return fmt.Errorf("findmissing: %w", err)
	}
	missingSet := make(map[string]bool, len(missing))
	for _, d := range missing {
		missingSet[d.Hash] = true
	}
	for h, body := range dirBodies {
		if !missingSet[h] {
			continue
		}
		if err := store.PutBlob(ctx, &cas.Digest{Hash: h, SizeBytes: int64(len(body))}, body); err != nil {
			return fmt.Errorf("put directory %s: %w", h, err)
		}
	}
	for h, p := range tree.Files {
		if !missingSet[h] {
			continue
		}
		body, err := os.ReadFile(p)
		if err != nil {
			return fmt.Errorf("read %s: %w", p, err)
		}
		if err := store.PutBlob(ctx, &cas.Digest{Hash: h, SizeBytes: int64(len(body))}, body); err != nil {
			return fmt.Errorf("put file %s: %w", h, err)
		}
	}
	return nil
}
