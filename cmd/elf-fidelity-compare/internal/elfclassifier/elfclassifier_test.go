package elfclassifier

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// buildSO compiles a trivial shared object with the given linker flags and
// returns its path. Skips the whole test when no C compiler / readelf is on
// PATH (the unit suite stays green on a toolchain-less runner).
func buildSO(t *testing.T, dir, name string, ldflags ...string) string {
	t.Helper()
	cc := "cc"
	if _, err := exec.LookPath(cc); err != nil {
		t.Skip("cc not on PATH; skipping ELF fixture test")
	}
	if _, err := exec.LookPath("readelf"); err != nil {
		t.Skip("readelf not on PATH; skipping ELF fixture test")
	}
	src := filepath.Join(dir, name+".c")
	if err := os.WriteFile(src, []byte("int foo(void){return 7;}\nint bar(void){return 9;}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "lib"+name+".so")
	args := append([]string{"-shared", "-fPIC", "-o", out, src}, ldflags...)
	cmd := exec.Command(cc, args...)
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("cc %v failed: %v\n%s", args, err, b)
	}
	return out
}

// versionScript writes a symbol version-script defining one version node.
func versionScript(t *testing.T, dir, node string) string {
	t.Helper()
	p := filepath.Join(dir, node+".map")
	body := node + " {\n  global: foo; bar;\n  local: *;\n};\n"
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func hasDelta(deltas []Delta, kind string) bool {
	for _, d := range deltas {
		if d.Kind == kind {
			return true
		}
	}
	return false
}

// buildExe compiles a trivial dynamically-linked executable with the given
// linker flags and returns its path.
func buildExe(t *testing.T, dir, name string, ldflags ...string) string {
	t.Helper()
	cc := "cc"
	if _, err := exec.LookPath(cc); err != nil {
		t.Skip("cc not on PATH; skipping ELF fixture test")
	}
	if _, err := exec.LookPath("readelf"); err != nil {
		t.Skip("readelf not on PATH; skipping ELF fixture test")
	}
	src := filepath.Join(dir, name+".c")
	if err := os.WriteFile(src, []byte("int main(void){return 0;}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, name)
	args := append([]string{"-o", out, src}, ldflags...)
	cmd := exec.Command(cc, args...)
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("cc %v failed: %v\n%s", args, err, b)
	}
	return out
}

// TestCompare_Executables: the lens handles executables too. A PIE executable
// with a clean ABI surface compares clean; one with a host-leak RUNPATH trips
// the same hermeticity finding as a .so. SONAME / version-def checks are
// graceful no-ops (an exe carries neither).
func TestCompare_Executables(t *testing.T) {
	dir := t.TempDir()
	cmakeExe := buildExe(t, dir, "app_cmake", "-fPIE", "-pie")
	dir2 := t.TempDir()
	bazelGood := buildExe(t, dir2, "app", "-fPIE", "-pie")

	rep, err := Compare(cmakeExe, bazelGood, Allowlist{})
	if err != nil {
		t.Fatal(err)
	}
	if rep.HasImpactful() {
		t.Fatalf("matching executables should have no impactful deltas: %+v", rep.ImpactfulDeltas)
	}
	if rep.SonameMatch != "" {
		t.Errorf("executables carry no SONAME; SonameMatch should be empty, got %q", rep.SonameMatch)
	}

	dir3 := t.TempDir()
	bazelLeak := buildExe(t, dir3, "app", "-fPIE", "-pie",
		"-Wl,--enable-new-dtags", "-Wl,-rpath,/home/user/scratch")
	repLeak, err := Compare(cmakeExe, bazelLeak, Allowlist{})
	if err != nil {
		t.Fatal(err)
	}
	if !hasDelta(repLeak.ImpactfulDeltas, "rpath-host-leak-in-bazel") {
		t.Errorf("executable host-leak RUNPATH should be impactful; got %+v", repLeak.ImpactfulDeltas)
	}
}

// TestSonameBaseAndMajor locks the suffix-aware soname parsing: the qualifying
// `.so` is the version suffix, not a `.so` embedded mid-name.
func TestSonameBaseAndMajor(t *testing.T) {
	cases := []struct{ in, base, major string }{
		{"libfoo.so", "libfoo.so", "libfoo.so"},
		{"libfoo.so.1", "libfoo.so", "libfoo.so.1"},
		{"libfoo.so.1.2.3", "libfoo.so", "libfoo.so.1"},
		{"libc.so.6", "libc.so", "libc.so.6"},
		// `.so` embedded mid-name must not mis-base on the embedded occurrence.
		{"libfoo.software.so.1", "libfoo.software.so", "libfoo.software.so.1"},
		{"libsomething.so.2", "libsomething.so", "libsomething.so.2"},
		{"noext", "noext", "noext"},
	}
	for _, c := range cases {
		if got := sonameBase(c.in); got != c.base {
			t.Errorf("sonameBase(%q) = %q, want %q", c.in, got, c.base)
		}
		if got := sonameMajor(c.in); got != c.major {
			t.Errorf("sonameMajor(%q) = %q, want %q", c.in, got, c.major)
		}
	}
}

// TestCompare_IdenticalSharedObjects: two .so's built with the same soname +
// version node compare clean (no impactful deltas), with the shared facts
// reported.
func TestCompare_IdenticalSharedObjects(t *testing.T) {
	dir := t.TempDir()
	vs := versionScript(t, dir, "LIBFOO_1.0")
	a := buildSO(t, dir, "a", "-Wl,-soname,libfoo.so.1", "-Wl,--version-script,"+vs)
	// Build the "bazel" side in a sibling dir so the path differs but the ELF
	// facts match.
	dir2 := t.TempDir()
	vs2 := versionScript(t, dir2, "LIBFOO_1.0")
	b := buildSO(t, dir2, "a", "-Wl,-soname,libfoo.so.1", "-Wl,--version-script,"+vs2)

	rep, err := Compare(a, b, Allowlist{})
	if err != nil {
		t.Fatal(err)
	}
	if rep.HasImpactful() {
		t.Fatalf("identical .so's should have no impactful deltas: %+v", rep.ImpactfulDeltas)
	}
	if rep.SonameMatch != "libfoo.so.1" {
		t.Errorf("SonameMatch = %q, want libfoo.so.1", rep.SonameMatch)
	}
	if rep.VersionDefsBoth != 1 {
		t.Errorf("VersionDefsBoth = %d, want 1 (LIBFOO_1.0)", rep.VersionDefsBoth)
	}
}

// TestCompare_VersionNodeDropped: the bazel side lost the version node — the
// ABI-versioning break nm-set comparison can't see. Impactful.
func TestCompare_VersionNodeDropped(t *testing.T) {
	dir := t.TempDir()
	vs := versionScript(t, dir, "LIBFOO_1.0")
	cmakeSO := buildSO(t, dir, "a", "-Wl,-soname,libfoo.so.1", "-Wl,--version-script,"+vs)
	dir2 := t.TempDir()
	bazelSO := buildSO(t, dir2, "a", "-Wl,-soname,libfoo.so.1") // no version script

	rep, err := Compare(cmakeSO, bazelSO, Allowlist{})
	if err != nil {
		t.Fatal(err)
	}
	if !hasDelta(rep.ImpactfulDeltas, "version-node-only-in-cmake") {
		t.Errorf("expected impactful version-node-only-in-cmake; got %+v", rep.ImpactfulDeltas)
	}
	// And the allowlist suppresses it.
	rep2, _ := Compare(cmakeSO, bazelSO, Allowlist{Symbols: map[string]bool{"LIBFOO_1.0": true}})
	if rep2.HasImpactful() {
		t.Errorf("allowlisted version node should be benign: %+v", rep2.ImpactfulDeltas)
	}
}

// TestCompare_SonameMajorMismatchVsSuffix: a major bump (.so.1 vs .so.2) is
// impactful; a minor/patch suffix (.so.1 vs .so.1.2.3) is benign.
func TestCompare_SonameMajorMismatchVsSuffix(t *testing.T) {
	dir := t.TempDir()
	base := buildSO(t, dir, "a", "-Wl,-soname,libfoo.so.1")
	dirMaj := t.TempDir()
	major := buildSO(t, dirMaj, "a", "-Wl,-soname,libfoo.so.2")
	dirMin := t.TempDir()
	minor := buildSO(t, dirMin, "a", "-Wl,-soname,libfoo.so.1.2.3")

	repMaj, err := Compare(base, major, Allowlist{})
	if err != nil {
		t.Fatal(err)
	}
	if !hasDelta(repMaj.ImpactfulDeltas, "soname-mismatch") {
		t.Errorf("major bump .so.1 vs .so.2 should be impactful soname-mismatch; got %+v", repMaj.ImpactfulDeltas)
	}
	repMin, err := Compare(base, minor, Allowlist{})
	if err != nil {
		t.Fatal(err)
	}
	if repMin.HasImpactful() {
		t.Errorf("minor suffix .so.1 vs .so.1.2.3 should be benign; got %+v", repMin.ImpactfulDeltas)
	}
	if !hasDelta(repMin.BenignDeltas, "soname-version-suffix") {
		t.Errorf("expected benign soname-version-suffix; got %+v", repMin.BenignDeltas)
	}
}

// TestCompare_HostLeakRunpath: a /tmp or /home RUNPATH baked into the bazel
// artifact is a hermeticity break. Impactful.
func TestCompare_HostLeakRunpath(t *testing.T) {
	dir := t.TempDir()
	cmakeSO := buildSO(t, dir, "a", "-Wl,-soname,libfoo.so.1")
	dir2 := t.TempDir()
	bazelSO := buildSO(t, dir2, "a", "-Wl,-soname,libfoo.so.1",
		"-Wl,--enable-new-dtags", "-Wl,-rpath,/tmp/bazel-out/leak")

	rep, err := Compare(cmakeSO, bazelSO, Allowlist{})
	if err != nil {
		t.Fatal(err)
	}
	if !hasDelta(rep.ImpactfulDeltas, "rpath-host-leak-in-bazel") {
		t.Errorf("expected impactful rpath-host-leak-in-bazel; got %+v", rep.ImpactfulDeltas)
	}
}

// TestCompare_ExtraProjectNeeded: the bazel side links an extra PROJECT library
// (over-linking) — impactful; a distro-runtime NEEDED difference is benign.
func TestCompare_ExtraProjectNeeded(t *testing.T) {
	dir := t.TempDir()
	// Build a project dependency libdep.so to NEED.
	dep := buildSO(t, dir, "dep", "-Wl,-soname,libdep.so.1")
	_ = dep
	cmakeSO := buildSO(t, dir, "a", "-Wl,-soname,libfoo.so.1")
	// bazel side NEEDs libdep.so.1 (project lib) — over-linking.
	bazelSO := buildSO(t, dir, "b", "-Wl,-soname,libfoo.so.1",
		"-L"+dir, "-Wl,--no-as-needed", "-ldep")

	rep, err := Compare(cmakeSO, bazelSO, Allowlist{})
	if err != nil {
		t.Fatal(err)
	}
	if !hasDelta(rep.ImpactfulDeltas, "needed-only-in-bazel") {
		t.Errorf("expected impactful needed-only-in-bazel for libdep.so.1; got benign=%+v impactful=%+v", rep.BenignDeltas, rep.ImpactfulDeltas)
	}
	// Allowlisting the project dep makes it benign.
	rep2, _ := Compare(cmakeSO, bazelSO, Allowlist{Symbols: map[string]bool{"libdep.so.1": true}})
	if hasDelta(rep2.ImpactfulDeltas, "needed-only-in-bazel") {
		t.Errorf("allowlisted NEEDED should be benign: %+v", rep2.ImpactfulDeltas)
	}
}

func TestNormalizeSection(t *testing.T) {
	cases := map[string]string{
		".text._ZN3fmt6formatE":      ".text",             // -ffunction-sections
		".text":                      ".text",             // bare
		".rodata.str1.1":             ".rodata",           // string-merge
		".data.foo":                  ".data",             // -fdata-sections
		".data.rel.ro":               ".data.rel.ro",      // RELRO preserved
		".data.rel.ro.local":         ".data.rel.ro",      // RELRO variant preserved
		".gcc_except_table._ZN3fmtE": ".gcc_except_table", // per-function EH table
		".rela.text._ZN3fmtE":        ".rela",             // per-function reloc
		".rela.dyn":                  ".rela",             // dyn reloc
		".rel.plt":                   ".rel",              // rel form
		".init_array.001":            ".init_array",       // prioritized ctors
		".eh_frame":                  ".eh_frame",         // untouched
		".gnu.version_d":             ".gnu.version_d",    // untouched (benign handled elsewhere)
	}
	for in, want := range cases {
		if got := normalizeSection(in); got != want {
			t.Errorf("normalizeSection(%q) = %q want %q", in, got, want)
		}
	}
}

func TestIsBenignSection(t *testing.T) {
	benign := []string{".comment", ".debug_info", ".note.gnu.build-id", ".symtab",
		".strtab", ".gnu.hash", ".got", ".plt", ".rela", ".relro_padding",
		".tm_clone_table", ".gnu.version_d", ".group", ".llvm_addrsig"}
	for _, s := range benign {
		if !isBenignSection(s) {
			t.Errorf("%q should be benign (toolchain-determined)", s)
		}
	}
	impactful := []string{".text", ".data", ".rodata", ".bss", ".init_array",
		".fini_array", ".data.rel.ro", ".eh_frame", ".tdata", ".tbss", ".interp"}
	for _, s := range impactful {
		if isBenignSection(s) {
			t.Errorf("%q should NOT be benign (real link/runtime section)", s)
		}
	}
}

func TestParseSections(t *testing.T) {
	// Canned `readelf -W -S` output: a NULL section (blank name), real sections,
	// and per-function sections that must normalize to base names.
	out := []byte(`There are 5 section headers, starting at offset 0x100:

Section Headers:
  [Nr] Name              Type            Address          Off    Size   ES Flg Lk Inf Al
  [ 0]                   NULL            0000000000000000 000000 000000 00      0   0  0
  [ 1] .text._ZN3fmtE    PROGBITS        0000000000000000 000040 000010 00  AX  0   0 16
  [ 2] .rela.text._ZN3fmtE RELA          0000000000000000 000060 000018 18   I  7   1  8
  [ 3] .init_array       INIT_ARRAY      0000000000000000 000080 000008 08  WA  0   0  8
  [ 4] .debug_info       PROGBITS        0000000000000000 000090 000100 00      0   0  1
`)
	info := &ElfInfo{Sections: map[string]bool{}}
	parseSections(out, info)
	for _, want := range []string{".text", ".rela", ".init_array", ".debug_info"} {
		if !info.Sections[want] {
			t.Errorf("parseSections missing %q: %v", want, info.Sections)
		}
	}
	if len(info.Sections) != 4 { // NULL skipped; .text._ZN/.rela.text._ZN normalized
		t.Errorf("section set = %v, want exactly 4 normalized names", info.Sections)
	}
}

func TestClassifySections(t *testing.T) {
	rep := &Report{}
	cmake := map[string]bool{".text": true, ".data": true, ".init_array": true, ".comment": true}
	bazel := map[string]bool{".text": true, ".data": true, ".debug_info": true} // missing .init_array; extra .debug_info; cmake-only .comment
	classifySections(rep, cmake, bazel, Allowlist{})
	// .init_array only-in-cmake is a REAL section → impactful.
	foundInit := false
	for _, d := range rep.ImpactfulDeltas {
		if d.Kind == "section-only-in-cmake" && d.Detail == ".init_array" {
			foundInit = true
		}
	}
	if !foundInit {
		t.Errorf("missing .init_array should be impactful: %+v", rep.ImpactfulDeltas)
	}
	// .comment (cmake-only) + .debug_info (bazel-only) are toolchain → benign.
	if len(rep.ImpactfulDeltas) != 1 {
		t.Errorf("only .init_array should be impactful; got %+v", rep.ImpactfulDeltas)
	}
	// Allowlist suppresses a real section delta.
	rep2 := &Report{}
	classifySections(rep2, cmake, bazel, Allowlist{Symbols: map[string]bool{".init_array": true}})
	if len(rep2.ImpactfulDeltas) != 0 {
		t.Errorf("allowlisted section should be benign: %+v", rep2.ImpactfulDeltas)
	}
}
