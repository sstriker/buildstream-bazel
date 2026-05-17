package ninja_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/internal/ninja"
)

// Batch D of the test-coverage plan in PR #199: cover the file-
// level entry points the unit tests currently skip in favor of
// the in-memory Parse(io.Reader, ...) variant. ParseFile is the
// public entry the converter uses; defaultFileResolver hands
// include paths to Parse; parsePoolStmt is a rarely-used ninja
// syntax our fixtures don't naturally exercise.

// TestParseFile_HappyPath writes a small build.ninja to a temp
// dir and parses it via the public ParseFile entry point. Pins
// that ParseFile correctly opens the file, derives the parent
// dir for include resolution, and returns a populated Graph.
// This is the "use the public entry the converter calls" gap
// the plan calls out.
func TestParseFile_HappyPath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "build.ninja")
	body := strings.Join([]string{
		"# top-level var",
		"cmake_ninja_workdir = ",
		"",
		"rule CXX_COMPILER",
		"  command = c++ -o $out $in",
		"  description = Compile $out",
		"",
		"build hello.o: CXX_COMPILER hello.cc",
		"  FLAGS = -O2",
		"",
		"default hello.o",
		"",
	}, "\n")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	g, err := ninja.ParseFile(path)
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	if _, ok := g.Rules["CXX_COMPILER"]; !ok {
		t.Errorf("CXX_COMPILER rule missing from graph: %v", g.Rules)
	}
	b := g.BuildFor("hello.o")
	if b == nil {
		t.Fatal("hello.o not in build index")
	}
	if b.Rule != "CXX_COMPILER" {
		t.Errorf("rule = %q, want CXX_COMPILER", b.Rule)
	}
	if got := b.Bindings["FLAGS"]; got != "-O2" {
		t.Errorf("FLAGS = %q, want -O2", got)
	}
	if len(g.Defaults) != 1 || g.Defaults[0] != "hello.o" {
		t.Errorf("Defaults = %v, want [hello.o]", g.Defaults)
	}
}

// TestParseFile_MissingFileReturnsError locks the error contract
// when the path doesn't exist on disk. The converter uses
// ParseFile via convert-element-cmake's read of
// `$buildDir/build.ninja`; a missing file is a recoverable
// "graph absent" signal for the caller, who treats it as
// "skip recovery" — but it must surface as a non-nil error,
// not a silent empty graph.
func TestParseFile_MissingFileReturnsError(t *testing.T) {
	dir := t.TempDir()
	g, err := ninja.ParseFile(filepath.Join(dir, "does-not-exist.ninja"))
	if err == nil {
		t.Fatalf("ParseFile on missing file returned nil err; graph=%v", g)
	}
	if g != nil {
		t.Errorf("ParseFile on missing file returned non-nil graph: %v", g)
	}
	if !os.IsNotExist(err) {
		t.Errorf("err is not an os.IsNotExist: %v", err)
	}
}

// TestParseFile_ResolvesIncludesRelativeToParentDir locks that
// ParseFile derives the right parent-dir for relative-path
// includes. The included file's `rule` must appear in the
// final Graph; if ParseFile passed `""` or the cwd as the
// parent dir, the resolver would look in the wrong place and
// the include would fail.
func TestParseFile_ResolvesIncludesRelativeToParentDir(t *testing.T) {
	dir := t.TempDir()
	subDir := filepath.Join(dir, "ninja-files")
	if err := os.Mkdir(subDir, 0o755); err != nil {
		t.Fatal(err)
	}
	rulesPath := filepath.Join(subDir, "rules.ninja")
	if err := os.WriteFile(rulesPath, []byte("rule CXX_COMPILER\n  command = c++ -o $out $in\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mainPath := filepath.Join(subDir, "build.ninja")
	if err := os.WriteFile(mainPath, []byte("include rules.ninja\n\nbuild hello.o: CXX_COMPILER hello.cc\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	g, err := ninja.ParseFile(mainPath)
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	if _, ok := g.Rules["CXX_COMPILER"]; !ok {
		t.Errorf("CXX_COMPILER from included file missing: %v", g.Rules)
	}
}

// TestParseFile_DefaultFileResolver_AbsolutePath locks the
// defaultFileResolver's absolute-path branch. ninja allows
// `include /abs/path/to/file`; the resolver must NOT prepend
// parentDir to an absolute path. Drive it through ParseFile's
// public surface since defaultFileResolver itself isn't
// exported.
func TestParseFile_DefaultFileResolver_AbsolutePath(t *testing.T) {
	dir := t.TempDir()
	otherDir := t.TempDir()
	includedPath := filepath.Join(otherDir, "extras.ninja")
	if err := os.WriteFile(includedPath, []byte("rule LINKER\n  command = ld -o $out $in\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mainPath := filepath.Join(dir, "build.ninja")
	body := "include " + includedPath + "\n"
	if err := os.WriteFile(mainPath, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	g, err := ninja.ParseFile(mainPath)
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	if _, ok := g.Rules["LINKER"]; !ok {
		t.Errorf("LINKER rule from absolute-path include missing: %v", g.Rules)
	}
}

// TestParse_PoolStmt covers parsePoolStmt — rare ninja syntax
// (`pool console\n  depth = 1`) that CMake almost never emits
// so our fixtures don't carry it. Defensive coverage: a
// future cmake version that DOES emit a pool must not crash
// the parser.
func TestParse_PoolStmt(t *testing.T) {
	body := "pool link_pool\n  depth = 4\n\nrule LINK\n  command = ld -o $out $in\n  pool = link_pool\n"
	g, err := ninja.Parse(strings.NewReader(body), "", nil)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	p, ok := g.Pools["link_pool"]
	if !ok {
		t.Fatalf("link_pool not in graph; pools=%v", g.Pools)
	}
	if p.Bindings["depth"] != "4" {
		t.Errorf("depth = %q, want 4", p.Bindings["depth"])
	}
}

// TestParse_PoolStmt_MissingName surfaces the parsePoolStmt
// error branch: `pool\n` with no name returns a typed parser
// error pointing at the offending line. Pins the contract so
// a future refactor that swallows the error fails loudly.
func TestParse_PoolStmt_MissingName(t *testing.T) {
	_, err := ninja.Parse(strings.NewReader("pool\n  depth = 1\n"), "", nil)
	if err == nil {
		t.Fatal("Parse accepted pool without name; want error")
	}
	if !strings.Contains(err.Error(), "pool without name") {
		t.Errorf("err = %q, want substring %q", err.Error(), "pool without name")
	}
}

// TestParseFile_DefaultResolver_FailingIncludeBubblesUp pins
// that defaultFileResolver's os.Open error path propagates up
// through ParseFile, decorated with the line number / directive
// the includeFile wrapper adds. Without this contract the
// converter would silently lose a missing rules.ninja and
// produce an empty rules map.
func TestParseFile_DefaultResolver_FailingIncludeBubblesUp(t *testing.T) {
	dir := t.TempDir()
	mainPath := filepath.Join(dir, "build.ninja")
	if err := os.WriteFile(mainPath, []byte("include missing.ninja\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := ninja.ParseFile(mainPath)
	if err == nil {
		t.Fatal("ParseFile succeeded; want include error")
	}
	if !strings.Contains(err.Error(), "missing.ninja") {
		t.Errorf("err %q does not mention missing.ninja", err.Error())
	}
}
