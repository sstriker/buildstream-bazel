// trace-publish takes a per-element {trace.log, make-db.txt} pair
// produced by the kind:autotools coarse pass-3 genrule, packs it
// as a REAPI Directory, uploads every blob to CAS, and writes an
// ActionResult into the action cache under a synthetic key derived
// from the element's srckey.
//
// The synthetic key is the rendezvous: cmd/trace-lookup, run by
// project A's _trace_repo Bazel rule at load time, computes the
// same key from the same srckey and reads back the AC entry. AC
// hit + verified blobs ⇒ the lookup prints the trace's root
// Directory digest, which the repo rule symlinks under cas-fuse /
// bb_clientd's `<mount>/blobs/directory/<digest>` mount.
//
// Usage (invoked from inside the autotools install genrule, after
// the build has produced trace.log + make-db.txt):
//
//	trace-publish \
//	    --cas=<grpc-addr> \
//	    --srckey=<hex>     \
//	    --trace=<path>     \
//	    --make-db=<path>   \
//	    [--instance=<name>]
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
	makeDBPath := flag.String("make-db", "", "path to the filtered make-db.txt produced by the genrule's `make -np` post-step.")
	flag.Parse()

	if *addr == "" || *srckey == "" || *tracePath == "" || *makeDBPath == "" {
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

	if err := publish(ctx, store, *srckey, *tracePath, *makeDBPath); err != nil {
		log.Fatalf("trace-publish: %v", err)
	}
}

// publish does the work; factored out so the in-process roundtrip
// test (which uses cas.LocalStore) shares the upload + AC-update
// logic with the gRPC binary path.
func publish(ctx context.Context, store cas.Store, srckey, tracePath, makeDBPath string) error {
	traceBody, err := os.ReadFile(tracePath)
	if err != nil {
		return fmt.Errorf("read trace: %w", err)
	}
	makeDBBody, err := os.ReadFile(makeDBPath)
	if err != nil {
		return fmt.Errorf("read make-db: %w", err)
	}

	// Defensive re-canonicalization. The pipeline genrule already
	// produces canonical bytes (build-tracer applies pid + temp
	// path normalization; sed strips the make-db variant lines).
	// Re-applying here means a publisher running against an older
	// build-tracer or a custom genrule still lands a stable AC
	// entry across machines.
	traceBody = tracenorm.CanonicalizeBytes(traceBody, nil)
	makeDBBody = tracenorm.FilterMakeDB(makeDBBody)

	// Pack the two-file trace dir as a REAPI Directory and upload
	// every blob (root directory + the two file contents) via
	// FindMissing/PutBlob. cas.UploadDir does both in one helper.
	stage, err := os.MkdirTemp("", "trace-publish-stage-*")
	if err != nil {
		return fmt.Errorf("stage tmpdir: %w", err)
	}
	defer os.RemoveAll(stage)
	if err := os.WriteFile(filepath.Join(stage, "trace.log"), traceBody, 0o644); err != nil {
		return fmt.Errorf("write staged trace: %w", err)
	}
	if err := os.WriteFile(filepath.Join(stage, "make-db.txt"), makeDBBody, 0o644); err != nil {
		return fmt.Errorf("write staged make-db: %w", err)
	}
	rootDigest, err := cas.UploadDir(ctx, store, stage)
	if err != nil {
		return fmt.Errorf("upload trace dir: %w", err)
	}

	// Compute the synthetic Action digest and publish an
	// ActionResult whose single OutputDirectory carries the
	// trace dir's root Directory digest. trace-lookup reads
	// RootDirectoryDigest back; cas-fuse / bb_clientd serve any
	// Directory blob in CAS at `<mount>/blobs/directory/<digest>`,
	// so the digest IS the FUSE-mountable identifier — no Tree
	// blob is needed for lookup. (TreeDigest stays nil; consumers
	// of this AC entry use RootDirectoryDigest only.)
	actionDigest, err := tracenorm.SyntheticActionDigest(srckey)
	if err != nil {
		return fmt.Errorf("synth-key: %w", err)
	}
	ar := &repb.ActionResult{
		OutputDirectories: []*repb.OutputDirectory{
			{
				Path:                "trace",
				RootDirectoryDigest: rootDigest,
			},
		},
	}
	if err := store.UpdateActionResult(ctx, actionDigest, ar); err != nil {
		return fmt.Errorf("update ac: %w", err)
	}
	fmt.Printf("%s/%d\n", rootDigest.Hash, rootDigest.SizeBytes)
	return nil
}
