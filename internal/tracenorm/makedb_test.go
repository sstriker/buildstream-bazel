package tracenorm

import (
	"strings"
	"testing"
)

// TestFilterMakeDB_DropsVariantLines verifies the categories the
// shell sed filter drops are also dropped by the Go form. The
// pipeline genrule applies the sed filter inline; this is the
// trace-publish-side defense-in-depth re-application.
func TestFilterMakeDB_DropsVariantLines(t *testing.T) {
	in := `# Make data base, printed on Mon Jan  6 14:23:01 2026
# (device 1, inode 2): 3 files, 0 impossibilities in 1 directories
# 7 files, 0 impossibilities in 4 directories
#  Last modified 2026-01-06 14:23:01.000000000 +0000
target: prereq
	@echo build $@
.PHONY: all
# Finished Make data base on Mon Jan  6 14:23:02 2026
`
	got := string(FilterMakeDB([]byte(in)))
	for _, banned := range []string{
		"Make data base, printed on",
		"(device 1, inode 2): 3 files,",
		"7 files, 0 impossibilities in 4 directories",
		"Last modified",
		"Finished Make data base on",
	} {
		if strings.Contains(got, banned) {
			t.Errorf("filtered output still contains %q\n%s", banned, got)
		}
	}
	for _, kept := range []string{
		"target: prereq",
		"@echo build $@",
		".PHONY: all",
	} {
		if !strings.Contains(got, kept) {
			t.Errorf("filtered output dropped non-variant line %q\n%s", kept, got)
		}
	}
}

// TestFilterMakeDB_Idempotent guards against double-application
// drift. trace-publish may receive an already-filtered make-db
// (the shell genrule's sed step ran first); applying the filter
// again must be a no-op.
func TestFilterMakeDB_Idempotent(t *testing.T) {
	in := `target: prereq
	@echo build $@
`
	once := FilterMakeDB([]byte(in))
	twice := FilterMakeDB(once)
	if string(once) != string(twice) {
		t.Errorf("filter not idempotent\nonce:\n%s\ntwice:\n%s", once, twice)
	}
}
