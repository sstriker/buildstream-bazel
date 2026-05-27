package classifier

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseNmOutput_SkipsArchiveMemberHeaders(t *testing.T) {
	// nm on an archive prints `<member>:` header lines between
	// each .o's symbol listing; these must not count as symbols.
	raw := []byte(`
adler32.c.o:
                 U __snprintf_chk
00000000 T adler32
00000000 T adler32_z

crc32.c.o:
                 U __memcpy_chk
00000000 T crc32
`)
	got := parseNmOutput(raw)
	want := map[string]bool{
		"__snprintf_chk": true,
		"adler32":        true,
		"adler32_z":      true,
		"__memcpy_chk":   true,
		"crc32":          true,
	}
	if len(got) != len(want) {
		t.Fatalf("parseNmOutput length: got %d (%v), want %d (%v)",
			len(got), got, len(want), want)
	}
	for k := range want {
		if !got[k] {
			t.Errorf("missing symbol %q in %v", k, got)
		}
	}
}

func TestClassifyUndefinedDeltas_FortifyAndStackProtector(t *testing.T) {
	cUndef := map[string]bool{
		"__snprintf_chk":   true,
		"__vsnprintf_chk":  true,
		"__stack_chk_fail": true,
		"printf":           true,
	}
	bUndef := map[string]bool{
		"snprintf":  true,
		"vsnprintf": true,
		"printf":    true,
	}
	rep := &Report{}
	classifyUndefinedDeltas(rep, cUndef, bUndef)

	// FORTIFY + stack-protector → benign, three entries.
	kinds := map[string]int{}
	for _, d := range rep.BenignDeltas {
		kinds[d.Kind]++
	}
	if kinds["fortify-symbol-only-in-cmake"] != 2 {
		t.Errorf("fortify benign count: got %d, want 2 (%v)", kinds["fortify-symbol-only-in-cmake"], rep.BenignDeltas)
	}
	if kinds["stack-protector-symbol-only-in-cmake"] != 1 {
		t.Errorf("stack-protector benign count: got %d, want 1 (%v)", kinds["stack-protector-symbol-only-in-cmake"], rep.BenignDeltas)
	}
	if len(rep.ImpactfulDeltas) != 0 {
		t.Errorf("undefined-deltas shouldn't surface impactful entries; got %v", rep.ImpactfulDeltas)
	}
}

func TestClassifyExportedDeltas_TemplateInstantiationPairing(t *testing.T) {
	// fmt-shape: same C++ template instantiated with slightly
	// different parameters across the two builds. The pair shares
	// a long mangled prefix; the heuristic pairs them and tags
	// both as benign.
	cExported := map[string]bool{
		"shared_symbol": true,
		// Template `format_decimal<char, short, ...>` on cmake side
		"_ZN3fmt3v106detail14format_decimalIcsNS0_8appenderELi0EEENS1_21format_decimal_resultIT1_EES5_T0_i": true,
	}
	bExported := map[string]bool{
		"shared_symbol": true,
		// Same template, different instantiation (long instead of short)
		"_ZN3fmt3v106detail14format_decimalIclNS0_8appenderELi0EEENS1_21format_decimal_resultIT1_EES5_T0_i": true,
	}
	rep := &Report{}
	classifyExportedDeltas(rep, cExported, bExported, nil)

	for _, d := range rep.BenignDeltas {
		if !strings.Contains(d.Kind, "template-instantiation") {
			t.Errorf("expected template-instantiation kind, got %q (%s)", d.Kind, d.Detail)
		}
	}
	if len(rep.ImpactfulDeltas) != 0 {
		t.Errorf("template pair should not be impactful; got %v", rep.ImpactfulDeltas)
	}
	if len(rep.BenignDeltas) != 2 {
		t.Errorf("expected 2 benign deltas (one per side); got %d (%v)", len(rep.BenignDeltas), rep.BenignDeltas)
	}
}

func TestClassifyExportedDeltas_AllowlistSuppression(t *testing.T) {
	c := map[string]bool{"shared": true, "cmake_only_known_benign": true}
	b := map[string]bool{"shared": true}
	allowed := map[string]bool{"cmake_only_known_benign": true}

	rep := &Report{}
	classifyExportedDeltas(rep, c, b, allowed)

	if len(rep.ImpactfulDeltas) != 0 {
		t.Errorf("allowlist should pre-empt impactful classification; got %v", rep.ImpactfulDeltas)
	}
	if len(rep.BenignDeltas) != 1 || rep.BenignDeltas[0].Kind != "allowlist-suppressed" {
		t.Errorf("expected one allowlist-suppressed benign delta; got %v", rep.BenignDeltas)
	}
}

func TestClassifyExportedDeltas_UnexplainedDropIsImpactful(t *testing.T) {
	c := map[string]bool{"shared": true, "regression_symbol": true}
	b := map[string]bool{"shared": true}

	rep := &Report{}
	classifyExportedDeltas(rep, c, b, nil)

	if len(rep.ImpactfulDeltas) != 1 {
		t.Fatalf("expected 1 impactful delta; got %d (%v)", len(rep.ImpactfulDeltas), rep.ImpactfulDeltas)
	}
	if rep.ImpactfulDeltas[0].Detail != "regression_symbol" {
		t.Errorf("expected detail=regression_symbol; got %q", rep.ImpactfulDeltas[0].Detail)
	}
}

func TestClassifyAbsolutePaths_BazelOnlyIsImpactful(t *testing.T) {
	c := map[string]bool{}
	b := map[string]bool{"/tmp/bazel-stage/foo": true, "/home/user/work/src/baz.h": true}

	rep := &Report{}
	classifyAbsolutePaths(rep, c, b)

	if len(rep.ImpactfulDeltas) != 2 {
		t.Errorf("expected 2 hermeticity-leak deltas; got %v", rep.ImpactfulDeltas)
	}
}

func TestClassifyAbsolutePaths_SharedPathNotFlagged(t *testing.T) {
	// Paths in both artifacts come from the toolchain (libgcc
	// debug info, etc.), not a converter leak.
	c := map[string]bool{"/tmp/shared/foo": true}
	b := map[string]bool{"/tmp/shared/foo": true}

	rep := &Report{}
	classifyAbsolutePaths(rep, c, b)

	if len(rep.ImpactfulDeltas) != 0 {
		t.Errorf("paths shared by both artifacts should not flag; got %v", rep.ImpactfulDeltas)
	}
}

func TestFilterHermeticityLeakPaths_KeepsLeakPrefixes_DropsToolchain(t *testing.T) {
	raw := []byte(`
/usr/lib/gcc/x86_64-linux-gnu/12/include
/lib/x86_64-linux-gnu/libc.so.6
/tmp/bazel-stage/zlib/zutil.c
/home/operator/src/proj/main.cpp
/etc/something-not-a-leak
not an absolute path
`)
	got := filterHermeticityLeakPaths(raw)
	want := map[string]bool{
		"/tmp/bazel-stage/zlib/zutil.c":    true,
		"/home/operator/src/proj/main.cpp": true,
	}
	if len(got) != len(want) {
		t.Fatalf("filterHermeticityLeakPaths: got %v, want %v", got, want)
	}
	for k := range want {
		if !got[k] {
			t.Errorf("missing %q in %v", k, got)
		}
	}
}

func TestLoadAllowlist_StripsCommentsAndBlanks(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "allowlist.txt")
	body := `# fmt's known template-inlining differences
_ZN3fmt3v106detail14format_decimalSpecific

# blank lines too:

_ZN3fmt3v106detail17do_write_floatVariant
`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	allowed, err := LoadAllowlist(path)
	if err != nil {
		t.Fatal(err)
	}
	if !allowed["_ZN3fmt3v106detail14format_decimalSpecific"] {
		t.Error("expected first entry to be loaded")
	}
	if !allowed["_ZN3fmt3v106detail17do_write_floatVariant"] {
		t.Error("expected second entry to be loaded")
	}
	if allowed["# fmt's known template-inlining differences"] {
		t.Error("comment line should not load as allowlist entry")
	}
	if len(allowed) != 2 {
		t.Errorf("expected 2 entries; got %d (%v)", len(allowed), allowed)
	}
}

func TestLoadAllowlist_EmptyPath(t *testing.T) {
	got, err := LoadAllowlist("")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("expected empty map for empty path; got %v", got)
	}
}

func TestHasPrefixPair_SharesFirst24Chars(t *testing.T) {
	a := "_ZN3fmt3v106detail14format_decimalIcsNS0_..."
	peers := map[string]bool{
		"_ZN3fmt3v106detail14format_decimalIclNS0_...": true, // shares 24-char prefix
	}
	if !hasPrefixPair(a, peers) {
		t.Error("expected pair to match on shared 24-char prefix")
	}

	peersUnrelated := map[string]bool{
		"_ZN3std6stringC1Ev": true, // different template
	}
	if hasPrefixPair(a, peersUnrelated) {
		t.Error("did not expect match against unrelated mangled symbol")
	}
}

func TestReport_HasImpactfulAndFormat(t *testing.T) {
	r := &Report{
		CMakeArtifact: "/tmp/cmake/libfoo.a",
		BazelArtifact: "/tmp/bazel/libfoo.a",
		ExportedBoth:  42,
		ImpactfulDeltas: []Delta{
			{Kind: "exported-symbol-only-in-cmake", Detail: "lost_function"},
		},
	}
	if !r.HasImpactful() {
		t.Error("HasImpactful should be true with non-empty ImpactfulDeltas")
	}
	out := r.FormatForOperator()
	if !strings.Contains(out, "lost_function") {
		t.Errorf("expected formatted output to mention lost_function: %s", out)
	}
	if !strings.Contains(out, "exported symbols in both: 42") {
		t.Errorf("expected exported-both count in output: %s", out)
	}
}
