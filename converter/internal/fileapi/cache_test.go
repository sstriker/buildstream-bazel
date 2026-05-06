package fileapi

import "testing"

func TestCacheGet_UsesBuiltIndex(t *testing.T) {
	c := Cache{
		Entries: []CacheEntry{
			{Name: "A", Value: "first"},
			{Name: "B", Value: "second"},
		},
	}

	c.buildIndex()

	if c.index == nil {
		t.Fatal("buildIndex did not populate index")
	}
	if got := c.Get("B"); got == nil || got.Value != "second" {
		t.Fatalf("Get(B) = %#v, want value second", got)
	}
}

func TestCacheGet_DuplicateNamesKeepFirstEntry(t *testing.T) {
	c := Cache{
		Entries: []CacheEntry{
			{Name: "DUP", Value: "first"},
			{Name: "DUP", Value: "second"},
		},
	}

	c.buildIndex()

	got := c.Get("DUP")
	if got == nil {
		t.Fatal("Get(DUP) returned nil")
	}
	if got.Value != "first" {
		t.Fatalf("Get(DUP).Value = %q, want first", got.Value)
	}
}
