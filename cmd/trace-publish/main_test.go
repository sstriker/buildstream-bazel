package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"

	"github.com/sstriker/buildstream-bazel/internal/cas"
	"github.com/sstriker/buildstream-bazel/internal/tracenorm"
)

// TestPublish_RoundtripThroughLocalStore is the load-bearing
// trace-publish test: write a synthetic trace + make-db, publish
// against an in-memory store, then re-derive the synthetic key
// from the same srckey and assert the AC entry is reachable AND
// the trace blob is in CAS. This is the contract trace-lookup
// will read on the consumer side.
func TestPublish_RoundtripThroughLocalStore(t *testing.T) {
	ctx := context.Background()
	store, err := cas.NewLocalStore(t.TempDir())
	if err != nil {
		t.Fatalf("local store: %v", err)
	}

	src := t.TempDir()
	tracePath := filepath.Join(src, "trace.log")
	if err := os.WriteFile(tracePath,
		[]byte(`1234  execve("/usr/bin/cc", ["cc", "-c", "x.c"], 0x0) = 0`+"\n"),
		0o644); err != nil {
		t.Fatal(err)
	}
	makeDBPath := filepath.Join(src, "make-db.txt")
	if err := os.WriteFile(makeDBPath,
		[]byte("# Make data base, printed on Mon Jan  6 14:23:01 2026\nx: y\n\t@echo build\n"),
		0o644); err != nil {
		t.Fatal(err)
	}

	if err := publish(ctx, store, "srckey-aaa", "", tracePath, makeDBPath, ""); err != nil {
		t.Fatalf("publish: %v", err)
	}

	// Lookup by recomputing the synthetic key.
	key, err := tracenorm.SyntheticActionDigest("srckey-aaa", "")
	if err != nil {
		t.Fatal(err)
	}
	ar, err := store.GetActionResult(ctx, key)
	if err != nil {
		t.Fatalf("get-ar: %v", err)
	}
	if len(ar.OutputDirectories) != 1 {
		t.Fatalf("expected one output dir; got %d", len(ar.OutputDirectories))
	}
	rootDigest := ar.OutputDirectories[0].RootDirectoryDigest
	if rootDigest == nil {
		t.Fatalf("ActionResult missing root_directory_digest")
	}

	// The Directory proto referenced by rootDigest must be in CAS.
	rootBody, err := store.GetBlob(ctx, rootDigest)
	if err != nil {
		t.Fatalf("get directory blob: %v", err)
	}
	if len(rootBody) == 0 {
		t.Errorf("root directory blob is empty")
	}
	// Sanity: the directory proto names the two files the
	// publisher staged. A weak check — full proto deser is
	// covered by cas.PackDir tests.
	if !strings.Contains(string(rootBody), "trace.log") || !strings.Contains(string(rootBody), "make-db.txt") {
		t.Errorf("root directory blob does not name trace.log + make-db.txt:\n%s", rootBody)
	}
}

// TestPublish_DistinctSrckeysIsolate verifies two srckeys land at
// different AC entries. Without per-srckey isolation, a graph-
// affecting source edit (which changes srckey) would clobber
// the prior key's trace and break older branches.
func TestPublish_DistinctSrckeysIsolate(t *testing.T) {
	ctx := context.Background()
	store, err := cas.NewLocalStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	src := t.TempDir()
	tracePath := filepath.Join(src, "t.log")
	makeDBPath := filepath.Join(src, "db.txt")
	if err := os.WriteFile(tracePath, []byte("trace content\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(makeDBPath, []byte("makedb content\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := publish(ctx, store, "srckey-a", "", tracePath, makeDBPath, ""); err != nil {
		t.Fatal(err)
	}
	if err := publish(ctx, store, "srckey-b", "", tracePath, makeDBPath, ""); err != nil {
		t.Fatal(err)
	}
	keyA, _ := tracenorm.SyntheticActionDigest("srckey-a", "")
	keyB, _ := tracenorm.SyntheticActionDigest("srckey-b", "")
	arA, err := store.GetActionResult(ctx, keyA)
	if err != nil {
		t.Fatalf("get-ar A: %v", err)
	}
	arB, err := store.GetActionResult(ctx, keyB)
	if err != nil {
		t.Fatalf("get-ar B: %v", err)
	}
	// Same content under different srckeys → both ARs reachable.
	// (Their RootDirectoryDigest happens to match because we
	// staged the same bytes under both keys; that's correct
	// CAS behavior. The point of the test is that BOTH AC
	// keys round-trip — i.e. one didn't clobber the other.)
	if arA == nil || arB == nil {
		t.Errorf("per-srckey AC entries didn't round-trip: arA=%v arB=%v", arA, arB)
	}
}

// TestPublish_Idempotent verifies republishing the same trace is a
// no-op (CAS deduplicates by digest; AC update is overwrite-with-
// identical-body). Catches a regression where re-publish would
// fail or leave the store in a weird state.
func TestPublish_Idempotent(t *testing.T) {
	ctx := context.Background()
	store, err := cas.NewLocalStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	src := t.TempDir()
	tracePath := filepath.Join(src, "t.log")
	makeDBPath := filepath.Join(src, "db.txt")
	if err := os.WriteFile(tracePath, []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(makeDBPath, []byte("world\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := publish(ctx, store, "srckey-zzz", "", tracePath, makeDBPath, ""); err != nil {
			t.Fatalf("iteration %d: %v", i, err)
		}
	}
	key, _ := tracenorm.SyntheticActionDigest("srckey-zzz", "")
	if _, err := store.GetActionResult(ctx, key); err != nil {
		t.Errorf("AC entry missing after 3 publishes: %v", err)
	}
}

// TestPublish_OmitMakeDB: cmake round-2 fallback publishes a
// trace.log only — there's no make-db equivalent for cmake's
// converter to consume. Passing an empty --make-db path skips
// the read + stage step; the published Directory contains
// only trace.log. The AC entry round-trips the same way the
// 2-file shape does.
func TestPublish_OmitMakeDB(t *testing.T) {
	ctx := context.Background()
	store, err := cas.NewLocalStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	src := t.TempDir()
	tracePath := filepath.Join(src, "t.log")
	if err := os.WriteFile(tracePath, []byte("cmake-trace\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Empty make-db path is the cmake round-2 shape.
	if err := publish(ctx, store, "srckey-cmake", "", tracePath, "", ""); err != nil {
		t.Fatalf("publish without make-db: %v", err)
	}
	key, _ := tracenorm.SyntheticActionDigest("srckey-cmake", "")
	ar, err := store.GetActionResult(ctx, key)
	if err != nil {
		t.Fatalf("get-ac: %v", err)
	}
	if ar == nil || len(ar.OutputDirectories) != 1 {
		t.Fatalf("expected one OutputDirectory; got %v", ar)
	}
}

// TestPublish_NotFoundIsCleanError covers the consumer-side
// pre-condition: lookup of an unpublished srckey returns
// ErrNotFound, not an opaque gRPC error.
func TestPublish_NotFoundIsCleanError(t *testing.T) {
	ctx := context.Background()
	store, err := cas.NewLocalStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	key, _ := tracenorm.SyntheticActionDigest("never-published", "")
	_, err = store.GetActionResult(ctx, key)
	if !errors.Is(err, cas.ErrNotFound) {
		t.Errorf("expected ErrNotFound; got %v", err)
	}
	// Silence the unused import — repb may not appear in the
	// non-error path otherwise.
	_ = (*repb.ActionResult)(nil)
}

// TestPublish_PlatformPartitionsAC: publishing the same srckey
// under two different platform tags lands two distinct AC
// entries. Critical for round-2 trace-driven kinds whose
// install layout / build graph legitimately diverges across
// target platforms (.so vs .dylib, multiarch lib dirs); a
// shared keyspace would let the first platform's trace shadow
// the second's, producing converter stubs that don't match
// the second platform's install_tree.tar.
func TestPublish_PlatformPartitionsAC(t *testing.T) {
	ctx := context.Background()
	store, err := cas.NewLocalStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	src := t.TempDir()
	linuxTrace := filepath.Join(src, "linux.log")
	darwinTrace := filepath.Join(src, "darwin.log")
	makeDB := filepath.Join(src, "db.txt")
	// Distinct bodies so the platform partitioning is the only
	// thing keeping the two AC entries distinct (i.e. we're
	// testing keyspace behaviour, not CAS dedup).
	if err := os.WriteFile(linuxTrace, []byte("trace-linux\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(darwinTrace, []byte("trace-darwin\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(makeDB, []byte("db\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := publish(ctx, store, "srckey-shared", "linux_x86_64", linuxTrace, makeDB, ""); err != nil {
		t.Fatalf("publish linux: %v", err)
	}
	if err := publish(ctx, store, "srckey-shared", "darwin_arm64", darwinTrace, makeDB, ""); err != nil {
		t.Fatalf("publish darwin: %v", err)
	}

	linuxKey, _ := tracenorm.SyntheticActionDigest("srckey-shared", "linux_x86_64")
	darwinKey, _ := tracenorm.SyntheticActionDigest("srckey-shared", "darwin_arm64")
	if linuxKey.Hash == darwinKey.Hash {
		t.Fatalf("synth keys collapsed across platforms; the platform partition is the test's premise")
	}
	linuxAR, err := store.GetActionResult(ctx, linuxKey)
	if err != nil {
		t.Fatalf("get linux AR: %v", err)
	}
	darwinAR, err := store.GetActionResult(ctx, darwinKey)
	if err != nil {
		t.Fatalf("get darwin AR: %v", err)
	}
	if linuxAR == nil || darwinAR == nil {
		t.Fatalf("per-platform AC entries didn't round-trip: linux=%v darwin=%v", linuxAR, darwinAR)
	}
	if linuxAR.OutputDirectories[0].RootDirectoryDigest.Hash ==
		darwinAR.OutputDirectories[0].RootDirectoryDigest.Hash {
		t.Errorf("AC entries reference identical trace bodies — one platform's publish overwrote the other under a shared key (regression in the partition)")
	}

	// A platform-less lookup against the same srckey misses
	// both entries (it queries the legacy keyspace, which we
	// never published into); confirms back-compat goes the
	// other way too — an old-style lookup can't accidentally
	// route to a platform-tagged entry.
	legacyKey, _ := tracenorm.SyntheticActionDigest("srckey-shared", "")
	_, err = store.GetActionResult(ctx, legacyKey)
	if !errors.Is(err, cas.ErrNotFound) {
		t.Errorf("platform-less lookup against a srckey only published with platform tags should be ErrNotFound; got %v", err)
	}
}
