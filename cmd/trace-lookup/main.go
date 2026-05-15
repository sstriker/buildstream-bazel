// trace-lookup is the consumer side of the round-2 rendezvous.
// Two modes:
//
//  1. Legacy load-time print mode (current callers in pre-existing
//     rendered projects). Given a srckey and an optional platform
//     tag, computes the synthetic Action digest, queries the REAPI
//     ActionCache, verifies the trace blob is still in CAS, and
//     prints the trace's root Directory digest on stdout. Used by
//     the legacy `_trace_repo` Bazel repository rule (which
//     symlinks `<mount>/blobs/directory/<digest>` and emits a
//     filegroup). See cmd/write-a/traces_bzl.go.
//
//  2. Action-time materialize mode (the action-time `trace_load`
//     rule). When any of --out-trace / --out-make-db /
//     --out-empty-marker is set, the tool runs the same lookup
//     but instead of printing the digest, it MATERIALIZES the
//     trace's Directory contents into the caller-supplied output
//     files. On AC hit it writes trace.log / make-db.txt to the
//     declared paths; on AC miss it writes an empty stamp file
//     to --out-empty-marker. The action's declared outputs are
//     always produced, so Bazel's normal dependency tracking
//     works. Replaces the load-time repo-rule lookup with an
//     action-cache-respecting action.
//
// Usage (legacy print mode):
//
//	trace-lookup --cas=<grpc-addr> --srckey=<hex> \
//	    [--platform=<tag>] [--instance=<name>]
//
// Usage (action-time materialize mode):
//
//	trace-lookup --cas=<grpc-addr> --srckey=<hex> \
//	    --out-trace=<path/to/trace.log> \
//	    [--out-make-db=<path/to/make-db.txt>] \
//	    --out-empty-marker=<path/to/marker> \
//	    [--platform=<tag>] [--instance=<name>]
//
// --platform: optional, same shape as legacy mode.
//
// --out-empty-marker: a stamp file that always exists post-action,
// regardless of hit/miss. The marker's contents are "hit\n" on AC
// hit and "miss\n" on AC miss, so downstream rules / drivers can
// discover the convergence frontier by reading the marker.
//
// Exit codes:
//
//	0 — successful (hit or miss; the marker / digest reports which).
//	1 — hard error (gRPC dial failure, AC backend error, materialize
//	    failure, etc.).
//	2 — usage error (missing required flag).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/sstriker/buildstream-bazel/internal/cas"
	"github.com/sstriker/buildstream-bazel/internal/tracenorm"
)

func main() {
	log.SetFlags(0)
	addr := flag.String("cas", "", "REAPI gRPC address (host:port). Empty/unset falls back to the CAS_GRPC_ADDR environment variable (the action-time `trace_load` Bazel rule reads it from --action_env=CAS_GRPC_ADDR rather than passing it as a flag — keeps the rule's argv free of operator-supplied values that would otherwise need shell interpolation). When both --cas and CAS_GRPC_ADDR are empty the lookup short-circuits to miss.")
	instance := flag.String("instance", "", "REAPI instance name; matches the AC endpoint's multi-tenancy prefix.")
	srckey := flag.String("srckey", "", "per-element srckey hex string (seeds the synthetic AC key). The `trace_load` Bazel rule passes this as a string attr directly — the rule's design choice to keep srckey out of the input file set is intentional and documented in rules/traces.bzl.")
	platform := flag.String("platform", "", "optional platform tag (e.g. linux_x86_64) partitioning the synthetic AC keyspace. Must match the publishing side's --platform.")
	outTrace := flag.String("out-trace", "", "action-time materialize mode: destination path for the trace.log file (extracted from the AC-resolved Directory). When set, the tool runs in materialize mode instead of legacy stdout-print mode.")
	outMakeDB := flag.String("out-make-db", "", "action-time materialize mode: destination path for the make-db.txt file. When set, the tool extracts make-db.txt from the resolved Directory; on AC miss OR Directory without make-db.txt entry, a zero-byte file is written so the declared Bazel output exists. Trace-driven kinds (autotools / make / makemaker / modulebuild / manual / script) set this; cmake round-2 fallback omits it (the cmake converter derives IR from the trace + cmake File API, no make-db).")
	outMarker := flag.String("out-empty-marker", "", "action-time materialize mode: destination path for the hit/miss stamp file. The file always exists post-action; its contents are \"hit\\n\" on AC hit, \"miss\\n\" on AC miss. The driver loop reads markers to compute the frontier of elements still needing a trace_build.")
	flag.Parse()

	if *srckey == "" {
		flag.Usage()
		os.Exit(2)
	}
	materializeMode := *outTrace != "" || *outMakeDB != "" || *outMarker != ""
	if materializeMode && *outMarker == "" {
		fmt.Fprintln(os.Stderr, "trace-lookup: materialize mode requires --out-empty-marker (the action needs a declared output that exists on both hit and miss)")
		os.Exit(2)
	}
	// --cas falls back to CAS_GRPC_ADDR so the action-time rule
	// can pass the endpoint via --action_env rather than as an
	// argv flag (avoids needing a shell to expand a variable
	// reference in the action's command line). Legacy callers
	// that pass --cas explicitly keep working unchanged.
	if *addr == "" {
		*addr = os.Getenv("CAS_GRPC_ADDR")
	}

	// Empty CAS address ⇒ no remote configured. In legacy mode emit
	// empty stdout (miss). In materialize mode write the miss-side
	// marker + zero-byte trace files so the action's declared outs
	// exist and downstream rules can branch on the marker.
	if *addr == "" {
		if materializeMode {
			if err := writeMissOutputs(*outTrace, *outMakeDB, *outMarker); err != nil {
				fmt.Fprintf(os.Stderr, "trace-lookup: %v\n", err)
				os.Exit(1)
			}
		}
		return
	}
	ctx := context.Background()
	store, err := cas.NewGRPCStore(ctx, cas.GRPCConfig{
		Endpoint:     *addr,
		InstanceName: *instance,
		Insecure:     true,
	})
	if err != nil {
		// gRPC dial errors at this layer are "we couldn't even
		// reach the cache" — same operational story as "no cache
		// configured." Distinguish miss-with-warning from hard
		// failure: the action-time mode emits a miss marker (the
		// build proceeds via the round-1 build path); legacy mode
		// keeps the prior behaviour (stderr + exit 1).
		fmt.Fprintf(os.Stderr, "trace-lookup: dial cas %s: %v\n", *addr, err)
		if materializeMode {
			if werr := writeMissOutputs(*outTrace, *outMakeDB, *outMarker); werr != nil {
				fmt.Fprintf(os.Stderr, "trace-lookup: %v\n", werr)
			}
			return
		}
		os.Exit(1)
	}
	defer store.Close()

	digest, err := lookup(ctx, store, *srckey, *platform)
	if err != nil {
		fmt.Fprintf(os.Stderr, "trace-lookup: %v\n", err)
		os.Exit(1)
	}
	if digest == nil {
		// Miss. Materialize-mode: write empty outputs + miss marker.
		// Legacy mode: empty stdout, exit 0.
		if materializeMode {
			if werr := writeMissOutputs(*outTrace, *outMakeDB, *outMarker); werr != nil {
				fmt.Fprintf(os.Stderr, "trace-lookup: %v\n", werr)
				os.Exit(1)
			}
		}
		return
	}
	if !materializeMode {
		fmt.Printf("%s/%d\n", digest.Hash, digest.SizeBytes)
		return
	}
	// Hit + materialize mode: fetch the Directory and extract the
	// requested files. trace-publish always publishes a flat
	// Directory (trace.log + optional make-db.txt at the root —
	// see cmd/trace-publish/main.go's OutputDirectories shape), so
	// the consumer just maps known names to declared output paths.
	if err := materializeHit(ctx, store, digest, *outTrace, *outMakeDB, *outMarker); err != nil {
		fmt.Fprintf(os.Stderr, "trace-lookup: %v\n", err)
		os.Exit(1)
	}
}

// writeMissOutputs writes the miss-side action outputs: an empty
// trace.log, an empty make-db.txt (when requested), and a marker
// file with "miss\n" contents. Used both for the no-CAS-configured
// case and the AC-miss case — both look identical to downstream
// consumers.
//
// Empty trace.log / make-db.txt outputs are intentional: the
// converters that consume them (convert-element-trace,
// convert-element-cmake under --unsupported-execute-process-fallback)
// already treat an empty trace as the "no trace yet" signal and
// emit the placeholder shape; pre-existing wiring stays correct.
func writeMissOutputs(outTrace, outMakeDB, outMarker string) error {
	if outTrace != "" {
		if err := writeEmptyFile(outTrace); err != nil {
			return fmt.Errorf("write %s: %w", outTrace, err)
		}
	}
	if outMakeDB != "" {
		if err := writeEmptyFile(outMakeDB); err != nil {
			return fmt.Errorf("write %s: %w", outMakeDB, err)
		}
	}
	return os.WriteFile(outMarker, []byte("miss\n"), 0o644)
}

// materializeHit fetches the AC-hit Directory blob, validates it
// has the expected entries, and copies them to the caller-supplied
// output paths. Writes the marker with "hit\n" to signal the
// outcome to downstream rules.
//
// The Directory layout is the one trace-publish writes: a flat
// Directory whose top-level entries are trace.log (always) and
// make-db.txt (when the publisher's --make-db was set). We don't
// recursively materialize — there are no subdirectories in a
// canonical publish — but if the AC ever grew a deeper layout we'd
// want to switch to MaterializeDirectory + a per-file copy.
func materializeHit(ctx context.Context, store cas.Store, d *cas.Digest, outTrace, outMakeDB, outMarker string) error {
	// Materialize into a scratch directory and then copy the
	// requested entries to their declared output paths. Using a
	// scratch dir keeps the action hermetic: declared outputs end
	// up at the exact paths Bazel expects, and any extra entries
	// the Directory carries (forward-compat) are dropped silently
	// rather than landing somewhere we'd have to clean up.
	scratch, err := os.MkdirTemp("", "trace-lookup-*")
	if err != nil {
		return fmt.Errorf("scratch: %w", err)
	}
	defer os.RemoveAll(scratch)
	if err := cas.MaterializeDirectory(ctx, store, d, scratch); err != nil {
		return fmt.Errorf("materialize: %w", err)
	}
	if outTrace != "" {
		if err := copyEntry(filepath.Join(scratch, "trace.log"), outTrace); err != nil {
			return fmt.Errorf("trace.log → %s: %w", outTrace, err)
		}
	}
	if outMakeDB != "" {
		src := filepath.Join(scratch, "make-db.txt")
		if _, statErr := os.Stat(src); statErr == nil {
			if err := copyEntry(src, outMakeDB); err != nil {
				return fmt.Errorf("make-db.txt → %s: %w", outMakeDB, err)
			}
		} else if errors.Is(statErr, os.ErrNotExist) {
			// AC hit but the publishing kind didn't emit make-db.
			// Write an empty file so Bazel's declared-outputs
			// contract holds; consumers treat empty make-db
			// equivalently to no make-db.
			if err := writeEmptyFile(outMakeDB); err != nil {
				return fmt.Errorf("empty make-db → %s: %w", outMakeDB, err)
			}
		} else {
			return fmt.Errorf("stat scratch make-db: %w", statErr)
		}
	}
	return os.WriteFile(outMarker, []byte("hit\n"), 0o644)
}

func copyEntry(src, dst string) error {
	body, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, body, 0o644)
}

func writeEmptyFile(path string) error {
	return os.WriteFile(path, nil, 0o644)
}

// lookup queries the AC for srckey's synthetic key, validates the
// referenced root Directory blob is still in CAS, and returns the
// digest. Returns (nil, nil) on a clean miss (AC entry absent OR
// blob evicted); both publisher "haven't built this yet" and
// CAS-eviction shapes route through the same coarse-fallback path.
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
