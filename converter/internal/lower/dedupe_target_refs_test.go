package lower

import (
	"reflect"
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/internal/fileapi"
)

// TestDedupeTargetRefsByID pins the fix for cmake's File API listing
// the same target twice in one configuration's targets[] (observed
// for Eigen's eigen_blas_static under Ninja Multi-Config). A repeated
// id must collapse to a single ref, preserving first-occurrence order,
// so the per-target walk doesn't emit two ir.Targets with the same
// name (which the bazelconstraints duplicate-name check would reject).
func TestDedupeTargetRefsByID(t *testing.T) {
	in := []fileapi.ConfigTargetRef{
		{Name: "a", Id: "a::@1", JsonFile: "ta.json"},
		{Name: "eigen_blas_static", Id: "ebs::@9", JsonFile: "tebs.json"},
		{Name: "eigen_blas_static", Id: "ebs::@9", JsonFile: "tebs.json"}, // exact dup
		{Name: "b", Id: "b::@2", JsonFile: "tb.json"},
	}
	got := dedupeTargetRefsByID(in)
	want := []fileapi.ConfigTargetRef{
		{Name: "a", Id: "a::@1", JsonFile: "ta.json"},
		{Name: "eigen_blas_static", Id: "ebs::@9", JsonFile: "tebs.json"},
		{Name: "b", Id: "b::@2", JsonFile: "tb.json"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("dedupe = %+v\nwant %+v", got, want)
	}
}

// Distinct ids that happen to share a name are NOT deduped — id is the
// unique key, and two genuinely different targets must both survive
// (their name collision is a separate concern handled elsewhere).
func TestDedupeTargetRefsByID_DistinctIDsSameName(t *testing.T) {
	in := []fileapi.ConfigTargetRef{
		{Name: "dup", Id: "x::@1"},
		{Name: "dup", Id: "y::@2"},
	}
	got := dedupeTargetRefsByID(in)
	if len(got) != 2 {
		t.Fatalf("distinct ids must both survive; got %d: %+v", len(got), got)
	}
}

// Empty ids are never treated as duplicates of each other (a missing
// id is not a key); both pass through.
func TestDedupeTargetRefsByID_EmptyIDs(t *testing.T) {
	in := []fileapi.ConfigTargetRef{
		{Name: "a", Id: ""},
		{Name: "b", Id: ""},
	}
	got := dedupeTargetRefsByID(in)
	if len(got) != 2 {
		t.Fatalf("empty-id refs must not collapse; got %d", len(got))
	}
}

// The result must be a fresh slice, not an alias of the input's
// backing array — the caller reassigns a value-copy's Targets and must
// not mutate the shared codemodel.
func TestDedupeTargetRefsByID_FreshSlice(t *testing.T) {
	in := []fileapi.ConfigTargetRef{
		{Name: "a", Id: "a::@1"},
		{Name: "a", Id: "a::@1"},
	}
	got := dedupeTargetRefsByID(in)
	if len(got) != 1 {
		t.Fatalf("want 1 after dedupe; got %d", len(got))
	}
	// Mutating the result must not touch the input.
	got[0].Name = "mutated"
	if in[0].Name != "a" {
		t.Fatalf("dedupe aliased the input backing array; in[0].Name=%q", in[0].Name)
	}
}
