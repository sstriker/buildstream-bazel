// source-push uploads on-disk source trees to a REAPI CAS
// endpoint, indexed by sourceKey, so cas-aware FUSE daemons
// (bb_clientd in production; cmd/cas-fuse historically, now
// retired) can serve them to Bazel repo rules.
//
// Two modes:
//
//	source-push tree --cas=<grpc-addr> --src=<dir> [--instance=<name>]
//	  Pack a single directory tree, push every blob into CAS,
//	  print the root Directory digest. Useful for hello-world
//	  fixtures and dev workflows.
//
//	source-push graph --cas=<grpc-addr> --bst=<root.bst> --source-cache=<dir> [--instance=<name>]
//	  Walk a .bst graph, find each non-kind:local source's
//	  source-cache entry, pack + push each, and print a JSON
//	  manifest of {key → digest}. Used by make fdsdk-source-push.
//
// In production, BuildStream's `bst source push` is the canonical
// path — it knows how to fetch sources too. cmd/source-push
// covers the test/dev case where a populated --source-cache
// already exists and BuildStream isn't installed locally.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"

	"github.com/sstriker/cmake-to-bazel/internal/cas"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "tree":
		cmdTree(os.Args[2:])
	case "graph":
		cmdGraph(os.Args[2:])
	case "-h", "--help", "help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand %q\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `source-push — upload local source trees to REAPI CAS.

Usage:
  source-push tree  --cas=<addr> --src=<dir> [--instance=<name>]
  source-push graph --cas=<addr> --source-cache=<dir> [--instance=<name>]
  source-push help
`)
}

func cmdTree(args []string) {
	fs := flag.NewFlagSet("tree", flag.ExitOnError)
	addr := fs.String("cas", "", "gRPC address of the CAS endpoint")
	src := fs.String("src", "", "directory to pack and push")
	instance := fs.String("instance", "", "REAPI instance name")
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}
	if *addr == "" || *src == "" {
		fmt.Fprintln(os.Stderr, "--cas and --src are required")
		os.Exit(2)
	}

	ctx := context.Background()
	store := dial(ctx, *addr, *instance)
	defer store.Close()

	rootDigest, err := cas.UploadDir(ctx, store, *src)
	if err != nil {
		log.Fatalf("upload-dir %s: %v", *src, err)
	}
	fmt.Println(formatPathDigest(rootDigest))
}

func cmdGraph(args []string) {
	fs := flag.NewFlagSet("graph", flag.ExitOnError)
	addr := fs.String("cas", "", "gRPC address of the CAS endpoint")
	cache := fs.String("source-cache", "", "directory of pre-fetched source trees, indexed by source-key")
	instance := fs.String("instance", "", "REAPI instance name")
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}
	if *addr == "" || *cache == "" {
		fmt.Fprintln(os.Stderr, "--cas and --source-cache are required")
		os.Exit(2)
	}

	entries, err := os.ReadDir(*cache)
	if err != nil {
		log.Fatalf("read source-cache %s: %v", *cache, err)
	}
	ctx := context.Background()
	store := dial(ctx, *addr, *instance)
	defer store.Close()
	manifest := map[string]string{}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		key := e.Name()
		path := *cache + "/" + key
		rootDigest, err := cas.UploadDir(ctx, store, path)
		if err != nil {
			log.Fatalf("upload-dir %s: %v", path, err)
		}
		manifest[key] = formatPathDigest(rootDigest)
	}
	out, _ := json.MarshalIndent(manifest, "", "  ")
	fmt.Println(string(out))
}

func dial(ctx context.Context, addr, instance string) *cas.GRPCStore {
	store, err := cas.NewGRPCStore(ctx, cas.GRPCConfig{
		Endpoint:     addr,
		InstanceName: instance,
		Insecure:     true,
	})
	if err != nil {
		log.Fatalf("dial CAS %q: %v", addr, err)
	}
	return store
}

// formatPathDigest renders a cas.Digest in the `<hash>-<size>`
// form used by REAPI mount-path components (bb_clientd's
// `<mount>/cas/<instance>/blobs/<fn>/directory/<hash>-<size>/`)
// and by the `digest` field of cmd/write-a's tools/sources.json.
// Different from cas.DigestString (which emits `<hash>/<size>` for
// log lines / bb_browser URLs).
func formatPathDigest(d *cas.Digest) string {
	return fmt.Sprintf("%s-%d", d.Hash, d.SizeBytes)
}
