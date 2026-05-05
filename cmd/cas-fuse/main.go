// cas-fuse mounts a REAPI-CAS namespace at a local path so Bazel
// repo rules can ctx.symlink into source trees that bst source
// push deposited into CAS, without ever materialising the bytes
// on the dev's local disk.
//
// Status (legacy / fallback): On Bazel 9, bb_clientd is the
// production path for CAS-aware mounts. bb_clientd is a stable,
// upstream-maintained binary that serves both the FUSE mount and
// the RemoteOutputService gRPC surface Bazel 9 needs to trust the
// daemon's reported digests without re-hashing inputs (see
// docs/design/bazel9-cas-fs.md). cas-fuse remains in-tree as a
// flag-only fallback for setups that don't want the bb_clientd
// dependency (e.g. air-gapped CI runners, simple dev fixtures, or
// the in-process casfuse / hello-fuse tests). The xattr fast-path
// cas-fuse implements (the `user.bazel.cas.digest` attribute that
// satisfied --unix_digest_hash_attribute_name) only matters on
// Bazel < 9; the flag was dropped in Bazel 9 without replacement,
// and the equivalent guarantee comes from bb_clientd's
// RemoteOutputService instead.
//
// Default mode (multi-digest, "root" mount):
//
//	cas-fuse mount --cas=<grpc-addr> --at=<mount-point> [--instance=<name>]
//
// The mount serves the bb_clientd-style hierarchy
//
//	<mount>/[<instance>/]blobs/directory/<hash>-<size>/...
//
// where each <hash>-<size> path component lazily resolves to the
// CAS Directory at that digest. One mount serves every source
// repo a Bazel build references.
//
// Single-digest mode (pre-#58 shape, kept for tests / debugging):
//
//	cas-fuse mount-one --cas=<...> --digest=<hash>-<size> --at=<...>
//
// Subsequent PRs add macOS NFSv4 support (per buildbarn's
// bb_clientd pattern).
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/sstriker/cmake-to-bazel/internal/casfuse"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "mount":
		cmdMount(os.Args[2:])
	case "mount-one":
		cmdMountOne(os.Args[2:])
	case "-h", "--help", "help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand %q\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `cas-fuse — FUSE-mount REAPI CAS namespaces.

Usage:
  cas-fuse mount     --cas=<grpc-addr> --at=<path> [--instance=<name>] [--allow-other]
  cas-fuse mount-one --cas=<grpc-addr> --digest=<hash>-<size> --at=<path> [--instance=<name>] [--allow-other]
  cas-fuse help
`)
}

func cmdMount(args []string) {
	fs := flag.NewFlagSet("mount", flag.ExitOnError)
	cas := fs.String("cas", "", "gRPC address of the CAS endpoint (host:port)")
	instance := fs.String("instance", "", "REAPI instance name (often empty)")
	at := fs.String("at", "", "mount point (must exist and be empty)")
	allowOther := fs.Bool("allow-other", false, "let other UIDs read the mount (needs user_allow_other in /etc/fuse.conf)")
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}
	if *cas == "" || *at == "" {
		fmt.Fprintln(os.Stderr, "--cas and --at are required")
		fs.Usage()
		os.Exit(2)
	}

	conn, err := grpc.NewClient(*cas, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		log.Fatalf("dial CAS %q: %v", *cas, err)
	}
	defer conn.Close()

	client := casfuse.NewCASClient(conn, *instance)
	root := casfuse.NewRoot(client)

	server, err := casfuse.MountRoot(root, *at, casfuse.MountOptions{AllowOther: *allowOther})
	if err != nil {
		log.Fatalf("mount %q: %v", *at, err)
	}
	log.Printf("cas-fuse mounted root at %s (cas=%s, instance=%q)", *at, *cas, *instance)

	waitForSignal(*at, server)
}

func cmdMountOne(args []string) {
	fs := flag.NewFlagSet("mount-one", flag.ExitOnError)
	cas := fs.String("cas", "", "gRPC address of the CAS endpoint (host:port)")
	instance := fs.String("instance", "", "REAPI instance name (often empty)")
	digest := fs.String("digest", "", `root Directory digest in "<hash>-<size>" form`)
	at := fs.String("at", "", "mount point (must exist and be empty)")
	allowOther := fs.Bool("allow-other", false, "let other UIDs read the mount (needs user_allow_other in /etc/fuse.conf)")
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}
	if *cas == "" || *digest == "" || *at == "" {
		fmt.Fprintln(os.Stderr, "--cas, --digest, --at are required")
		fs.Usage()
		os.Exit(2)
	}

	d, err := casfuse.ParseDigest(*digest)
	if err != nil {
		log.Fatalf("parse --digest: %v", err)
	}

	conn, err := grpc.NewClient(*cas, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		log.Fatalf("dial CAS %q: %v", *cas, err)
	}
	defer conn.Close()

	client := casfuse.NewCASClient(conn, *instance)
	tree := casfuse.NewTree(client, d)

	server, err := casfuse.Mount(tree, *at, casfuse.MountOptions{AllowOther: *allowOther})
	if err != nil {
		log.Fatalf("mount %q: %v", *at, err)
	}
	log.Printf("cas-fuse mounted %s at %s (instance=%q)", d, *at, *instance)

	waitForSignal(*at, server)
}

// waitForSignal blocks until SIGINT/SIGTERM, then triggers a clean
// unmount of server. Wrapped here so both cmdMount and cmdMountOne
// share the same lifecycle code (and the platform-specific Server
// type stays behind the serverUnmount/serverWait shims).
func waitForSignal(at string, server serverShim) {
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sig
		log.Printf("cas-fuse unmounting %s", at)
		_ = serverUnmount(server)
	}()
	serverWait(server)
}
