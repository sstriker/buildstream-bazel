package verify

import "testing"

// Unit coverage for FormatMismatches (was 0%) — the operator-facing rendering of
// a verify Report's mismatch list.
func TestFormatMismatches(t *testing.T) {
	if got := FormatMismatches(nil); got != "" {
		t.Errorf("nil report: got %q, want empty", got)
	}
	if got := FormatMismatches(&Report{}); got != "" {
		t.Errorf("no mismatches: got %q, want empty", got)
	}
	rep := &Report{Mismatches: []Mismatch{
		{File: "src/foo.c", Kind: "missing-define", Detail: "NDEBUG", Target: "foo"},
		{File: "gen.c", Kind: "orphan-source", Detail: "no IR target claims it"},
	}}
	want := "verify: src/foo.c [missing-define] NDEBUG (target foo)\n" +
		"verify: gen.c [orphan-source] no IR target claims it\n"
	if got := FormatMismatches(rep); got != want {
		t.Errorf("FormatMismatches:\n got %q\nwant %q", got, want)
	}
}
