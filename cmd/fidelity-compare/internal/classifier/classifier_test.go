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
	classifyUndefinedDeltas(rep, cUndef, bUndef, Allowlist{})

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
	classifyExportedDeltas(rep, cExported, bExported, Allowlist{})

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

func TestClassifyDeltas_StdlibInternalUnpaired(t *testing.T) {
	// Catch2-shape: large template-heavy library where the two
	// toolchains instantiate/inline DIFFERENT sets of std:: internals,
	// so the std symbols are unpaired (no matching prefix on the other
	// side). They must classify benign — the converter never controls
	// which std:: templates the compiler emits. A project-own unpaired
	// symbol (no std prefix) stays impactful.
	cExported := map[string]bool{
		"_ZNSt7__cxx1112basic_stringIcSt11char_traitsIcESaIcEE9push_backEc": true, // std::string::push_back
		"_ZN5Catch9SomethingRealDropEv":                                     true, // project-own → impactful
	}
	bExported := map[string]bool{
		"_ZNSt8__detail15_BracketMatcherINSt7__cxx1112regex_traitsIcEELb0ELb0EE13_M_make_rangeEcc": true, // std::__detail regex
	}
	rep := &Report{}
	classifyExportedDeltas(rep, cExported, bExported, Allowlist{})

	if len(rep.ImpactfulDeltas) != 1 || rep.ImpactfulDeltas[0].Detail != "_ZN5Catch9SomethingRealDropEv" {
		t.Errorf("expected only the project-own symbol impactful; got %v", rep.ImpactfulDeltas)
	}
	var stdBenign int
	for _, d := range rep.BenignDeltas {
		if strings.Contains(d.Kind, "stdlib-template-instantiation") {
			stdBenign++
		}
	}
	if stdBenign != 2 {
		t.Errorf("expected 2 stdlib-template-instantiation benign deltas; got %d (%v)", stdBenign, rep.BenignDeltas)
	}

	// Same for the undefined-set path (the Catch2 failure mode was
	// undefined std::string method refs cmake had but bazel inlined).
	repU := &Report{}
	classifyUndefinedDeltas(repU,
		map[string]bool{"_ZNKSt7__cxx1112basic_stringIcSt11char_traitsIcESaIcEE4findEcm": true},
		map[string]bool{}, Allowlist{})
	if len(repU.ImpactfulDeltas) != 0 {
		t.Errorf("unpaired std:: undefined ref should be benign; got impactful %v", repU.ImpactfulDeltas)
	}
}

func TestClassifyExportedDeltas_AllowlistSuppression(t *testing.T) {
	c := map[string]bool{"shared": true, "cmake_only_known_benign": true}
	b := map[string]bool{"shared": true}
	allowed := Allowlist{Symbols: map[string]bool{"cmake_only_known_benign": true}}

	rep := &Report{}
	classifyExportedDeltas(rep, c, b, allowed)

	if len(rep.ImpactfulDeltas) != 0 {
		t.Errorf("allowlist should pre-empt impactful classification; got %v", rep.ImpactfulDeltas)
	}
	if len(rep.BenignDeltas) != 1 || rep.BenignDeltas[0].Kind != "allowlist-suppressed" {
		t.Errorf("expected one allowlist-suppressed benign delta; got %v", rep.BenignDeltas)
	}
}

// TestClassifyExportedDeltas_AllowlistPrefixSuppression pins the
// `prefix:<mangled>` syntax that closes the
// huge-namespace-of-template-instantiations case (nlohmann/json's
// 1000+ basic_json template entries). One prefix entry covers
// every symbol in the namespace; the allowlist file stays small.
func TestClassifyExportedDeltas_AllowlistPrefixSuppression(t *testing.T) {
	c := map[string]bool{"shared": true}
	b := map[string]bool{
		"shared":                       true,
		"_ZN8nlohmann10basic_jsonXYZ":  true,
		"_ZN8nlohmann10basic_jsonABC":  true,
		"_ZN9otherpkg10basic_thingDEF": true,
	}
	allowed := Allowlist{
		Symbols:  map[string]bool{},
		Prefixes: []string{"_ZN8nlohmann"},
	}

	rep := &Report{}
	classifyExportedDeltas(rep, c, b, allowed)

	// nlohmann-prefixed entries → benign (allowlist-suppressed)
	// otherpkg-prefixed entry → still impactful
	gotImpactful := 0
	for _, d := range rep.ImpactfulDeltas {
		if d.Detail == "_ZN9otherpkg10basic_thingDEF" {
			gotImpactful++
		}
	}
	if gotImpactful != 1 {
		t.Errorf("expected non-prefix-matching symbol to remain impactful; got %v", rep.ImpactfulDeltas)
	}
	suppressed := 0
	for _, d := range rep.BenignDeltas {
		if d.Kind == "allowlist-suppressed" {
			suppressed++
		}
	}
	if suppressed != 2 {
		t.Errorf("expected 2 prefix-suppressed entries; got %v", rep.BenignDeltas)
	}
}

func TestClassifyExportedDeltas_UnexplainedDropIsImpactful(t *testing.T) {
	c := map[string]bool{"shared": true, "regression_symbol": true}
	b := map[string]bool{"shared": true}

	rep := &Report{}
	classifyExportedDeltas(rep, c, b, Allowlist{})

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
	if !allowed.Symbols["_ZN3fmt3v106detail14format_decimalSpecific"] {
		t.Error("expected first entry to be loaded")
	}
	if !allowed.Symbols["_ZN3fmt3v106detail17do_write_floatVariant"] {
		t.Error("expected second entry to be loaded")
	}
	if allowed.Symbols["# fmt's known template-inlining differences"] {
		t.Error("comment line should not load as allowlist entry")
	}
	if len(allowed.Symbols) != 2 {
		t.Errorf("expected 2 Symbols entries; got %d (%v)", len(allowed.Symbols), allowed.Symbols)
	}
}

// TestLoadAllowlist_PrefixEntries pins the `prefix:<mangled>`
// syntax that closes the huge-namespace case (e.g. nlohmann/json's
// _ZN8nlohmann*).
func TestLoadAllowlist_PrefixEntries(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "allowlist.txt")
	body := `
# header comment
exact_symbol_foo
prefix:_ZN8nlohmann
prefix:_ZTSN8nlohmann
not_a_prefix_entry
`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	allowed, err := LoadAllowlist(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(allowed.Symbols) != 2 {
		t.Errorf("expected 2 exact symbols; got %d (%v)", len(allowed.Symbols), allowed.Symbols)
	}
	if !allowed.Symbols["exact_symbol_foo"] || !allowed.Symbols["not_a_prefix_entry"] {
		t.Errorf("exact entries missing: %v", allowed.Symbols)
	}
	if len(allowed.Prefixes) != 2 {
		t.Errorf("expected 2 prefixes; got %d (%v)", len(allowed.Prefixes), allowed.Prefixes)
	}
	// Match should hit both shapes.
	if !allowed.Match("_ZN8nlohmann16json_abi_v3_11_3basic_jsonXYZ") {
		t.Error("expected prefix match for _ZN8nlohmann*")
	}
	if !allowed.Match("exact_symbol_foo") {
		t.Error("expected exact match for symbol")
	}
	if allowed.Match("_ZN9otherpkg") {
		t.Error("unexpected match for unrelated prefix")
	}
}

func TestLoadAllowlist_EmptyPath(t *testing.T) {
	got, err := LoadAllowlist("")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Symbols) != 0 || len(got.Prefixes) != 0 {
		t.Errorf("expected empty allowlist for empty path; got %v", got)
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
