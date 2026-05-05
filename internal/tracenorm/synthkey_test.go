package tracenorm

import "testing"

// TestSyntheticActionDigest_DeterministicForSameSrckey verifies
// two callers compute identical AC keys for the same srckey. This
// is the load-bearing property: trace-publish (round-1 publisher)
// and trace-lookup (round-2 consumer) must agree on the key
// without coordinating, possibly running on different machines.
func TestSyntheticActionDigest_DeterministicForSameSrckey(t *testing.T) {
	d1, err := SyntheticActionDigest("abc123")
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	d2, err := SyntheticActionDigest("abc123")
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if d1.Hash != d2.Hash || d1.SizeBytes != d2.SizeBytes {
		t.Errorf("same srckey produced different digests:\n  d1=%s/%d\n  d2=%s/%d",
			d1.Hash, d1.SizeBytes, d2.Hash, d2.SizeBytes)
	}
}

// TestSyntheticActionDigest_DistinctForDifferentSrckeys verifies
// the keyspace separates srckeys (no accidental collision). Without
// this, a graph-affecting source edit (which changes srckey)
// wouldn't get a fresh AC entry — it would clobber the prior key's
// trace, breaking older branches still building the prior srckey.
func TestSyntheticActionDigest_DistinctForDifferentSrckeys(t *testing.T) {
	d1, err := SyntheticActionDigest("aaa")
	if err != nil {
		t.Fatal(err)
	}
	d2, err := SyntheticActionDigest("bbb")
	if err != nil {
		t.Fatal(err)
	}
	if d1.Hash == d2.Hash {
		t.Errorf("distinct srckeys collided: both → %s/%d", d1.Hash, d1.SizeBytes)
	}
}

// TestSyntheticActionDigest_EmptySrckey is mostly a sanity check —
// an empty srckey is a degenerate input but the digest function
// should still resolve deterministically rather than panic.
func TestSyntheticActionDigest_EmptySrckey(t *testing.T) {
	d, err := SyntheticActionDigest("")
	if err != nil {
		t.Fatalf("empty srckey: %v", err)
	}
	if d == nil || d.Hash == "" {
		t.Errorf("empty srckey produced empty digest: %v", d)
	}
}
