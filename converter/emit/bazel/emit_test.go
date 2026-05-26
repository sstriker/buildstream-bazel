package bazel_test

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/emit/bazel"
	"github.com/sstriker/buildstream-bazel/converter/internal/cmakerun"
	"github.com/sstriker/buildstream-bazel/converter/internal/fileapi"
	"github.com/sstriker/buildstream-bazel/converter/internal/lower"
	"github.com/sstriker/buildstream-bazel/converter/ir"
	"github.com/sstriker/buildstream-bazel/internal/manifest"
)

var update = flag.Bool("update", false, "overwrite *.golden files instead of comparing")

func TestEmit_HelloWorld_Golden(t *testing.T) {
	src, err := filepath.Abs("../../testdata/sample-projects/hello-world")
	if err != nil {
		t.Fatalf("abs: %v", err)
	}
	r, err := fileapi.Load("../../testdata/fileapi/hello-world")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	pkg, err := lower.ToIR(r, nil, lower.Options{HostSourceRoot: src})
	if err != nil {
		t.Fatalf("ToIR: %v", err)
	}
	got, err := bazel.Emit(pkg)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}

	// Scrub the absolute SourceRoot from the header so the golden is
	// machine-portable. Emit writes "Source: <abs path>" in the header
	// comment; replace it with a stable token before comparison.
	got = scrubSourceLine(got, src)

	goldenPath := filepath.Join("..", "..", "testdata", "golden", "hello-world", "BUILD.bazel.golden")
	if *update {
		if err := os.MkdirAll(filepath.Dir(goldenPath), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(goldenPath, got, 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("updated %s", goldenPath)
		return
	}
	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read golden (run with -update?): %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("BUILD.bazel mismatch\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// TestEmit_SubdirLibrary_Golden exercises the multi-CMakeLists.txt
// shape: top-level CMakeLists pulls in src/util/CMakeLists.txt via
// add_subdirectory; both define cc_library targets. The codemodel
// reply has two targets defined across two source files; the
// emitter should produce one cc_library per target without
// flattening or losing the toplib→util dep edge.
//
// Known deltas captured by the golden (recorded as bugs to fix in
// follow-ups; the golden documents current behaviour so future
// converter changes against this fixture surface as visible diffs):
//   - `includes = ["include", "include"]` on toplib has duplicate
//     entries when a target's include path is repeated by both its
//     own target_include_directories and a transitive PUBLIC dep.
//     Should dedupe at IR-build time.
//   - `hdrs` on both targets enumerates every .h file in the
//     project rather than partitioning by which target's
//     target_include_directories owns the path. Hdrs detection is
//     over-inclusive across a multi-CMakeLists project.
func TestEmit_SubdirLibrary_Golden(t *testing.T) {
	src, err := filepath.Abs("../../testdata/sample-projects/subdir-library")
	if err != nil {
		t.Fatalf("abs: %v", err)
	}
	r, err := fileapi.Load("../../testdata/fileapi/subdir-library")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	pkg, err := lower.ToIR(r, nil, lower.Options{HostSourceRoot: src})
	if err != nil {
		t.Fatalf("ToIR: %v", err)
	}
	got, err := bazel.Emit(pkg)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	got = scrubSourceLine(got, src)

	goldenPath := filepath.Join("..", "..", "testdata", "golden", "subdir-library", "BUILD.bazel.golden")
	if *update {
		if err := os.MkdirAll(filepath.Dir(goldenPath), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(goldenPath, got, 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("updated %s", goldenPath)
		return
	}
	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read golden (run with -update?): %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("BUILD.bazel mismatch\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// scrubSourceLine replaces any literal occurrence of the absolute source root
// with the token <SOURCE_ROOT>. That's enough to make the header line stable
// across machines; Emit does not embed src elsewhere in M1.
func scrubSourceLine(b []byte, src string) []byte {
	abs := []byte(src)
	tok := []byte("<SOURCE_ROOT>")
	out := make([]byte, 0, len(b))
	for i := 0; i < len(b); {
		if i+len(abs) <= len(b) && string(b[i:i+len(abs)]) == string(abs) {
			out = append(out, tok...)
			i += len(abs)
			continue
		}
		out = append(out, b[i])
		i++
	}
	return out
}

// TestEmit_WithSourceKey_PrefixesLabels asserts the FUSE-sources
// emit path: when Options.SourceKey is set, every src/hdr in
// emitted cc_library/cc_binary/cc_test rules is prefixed with
// @src_<key>//: so project B's compile actions reference source
// bytes by digest-stable Bazel label rather than by relative
// filesystem path.
func TestEmit_WithSourceKey_PrefixesLabels(t *testing.T) {
	src, err := filepath.Abs("../../testdata/sample-projects/hello-world")
	if err != nil {
		t.Fatal(err)
	}
	r, err := fileapi.Load("../../testdata/fileapi/hello-world")
	if err != nil {
		t.Fatal(err)
	}
	pkg, err := lower.ToIR(r, nil, lower.Options{HostSourceRoot: src})
	if err != nil {
		t.Fatal(err)
	}
	got, err := bazel.EmitWithOptions(pkg, bazel.Options{SourceKey: "abc123"})
	if err != nil {
		t.Fatalf("EmitWithOptions: %v", err)
	}
	body := string(got)

	// Every src reference should be a @src_abc123//:tree_dir/<path>
	// label (matching the repo rule's tree_dir/ layout). The
	// hello-world fixture has hello.c + include/hello.h.
	for _, want := range []string{
		`@src_abc123//:tree_dir/hello.c`,
		`@src_abc123//:tree_dir/include/hello.h`,
	} {
		if !contains(body, want) {
			t.Errorf("emitted BUILD missing %q; got:\n%s", want, body)
		}
	}
	// Sanity check: no bare unprefixed src filenames leaked from
	// the legacy path.
	if contains(body, `srcs = ["hello.c"]`) {
		t.Errorf("emitted BUILD has bare hello.c reference (legacy path); got:\n%s", body)
	}
}

// TestEmit_NoSourceKey_PreservesLegacyPaths asserts the default
// emit path (no SourceKey) emits relative paths as before — a
// regression guard against the new option leaking into the
// existing test fixtures.
func TestEmit_NoSourceKey_PreservesLegacyPaths(t *testing.T) {
	src, err := filepath.Abs("../../testdata/sample-projects/hello-world")
	if err != nil {
		t.Fatal(err)
	}
	r, err := fileapi.Load("../../testdata/fileapi/hello-world")
	if err != nil {
		t.Fatal(err)
	}
	pkg, err := lower.ToIR(r, nil, lower.Options{HostSourceRoot: src})
	if err != nil {
		t.Fatal(err)
	}
	got, err := bazel.Emit(pkg)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	body := string(got)
	if contains(body, "@src_") {
		t.Errorf("legacy emit (no SourceKey) should not produce @src_ references; got:\n%s", body)
	}
}

// contains is a tiny strings.Contains alias kept local so the
// import set stays minimal.
func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

// TestEmit_ConfigureFile_Golden exercises the cmake configure_file
// shape: a .h.in template gets expanded to a build-tree config.h
// at cmake-configure time. The cc_library compiles a .c that
// includes the generated header.
//
// Lower's recovery path: trace records configure_file calls'
// (input, output) pairs; the recording script stashes the
// rendered output bytes in the fixture mirroring the build-dir
// layout. lower reads template + rendered, recovers the values
// dict via configurefile.Extract, and emits a genrule with
// .h.in as a real srcs input (so .h.in edits invalidate the
// genrule directly through Bazel's source graph) +
// //tools:cmake-configure-file as the tool that runs cmake's
// substitution rules at Bazel build time. The fallback shape
// (legacy base64-of-rendered cmd, no srcs) takes over when
// Extract can't recover values for a template.
func TestEmit_ConfigureFile_Golden(t *testing.T) {
	src, err := filepath.Abs("../../testdata/sample-projects/configure-file")
	if err != nil {
		t.Fatal(err)
	}
	replyDir, err := filepath.Abs("../../testdata/fileapi/configure-file")
	if err != nil {
		t.Fatal(err)
	}
	r, err := fileapi.Load(replyDir)
	if err != nil {
		t.Fatal(err)
	}
	traceRaw, err := os.ReadFile(filepath.Join(replyDir, "trace.jsonl"))
	if err != nil {
		t.Fatalf("read trace: %v", err)
	}
	pkg, err := lower.ToIR(r, nil, lower.Options{
		HostSourceRoot:    src,
		BuildDir:          replyDir,
		TraceRaw:          traceRaw,
		LiftConfigureFile: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := bazel.Emit(pkg)
	if err != nil {
		t.Fatal(err)
	}
	got = scrubSourceLine(got, src)

	goldenPath := filepath.Join("..", "..", "testdata", "golden", "configure-file", "BUILD.bazel.golden")
	if *update {
		_ = os.MkdirAll(filepath.Dir(goldenPath), 0o755)
		if err := os.WriteFile(goldenPath, got, 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("updated %s", goldenPath)
		return
	}
	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("BUILD.bazel mismatch\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// TestEmit_FileGenerate_Golden round-trips the file-generate
// sample project through Lower + bazel.Emit and compares to
// the captured BUILD.bazel.golden. Exercises three shapes the
// lifter covers:
//
//   - INPUT-form, genex-free: srcs anchors the template,
//     cmake-configure-file invocation in cmd, cmake-codegen-
//     lifted tag set.
//   - CONTENT-form, genex-free: --content-base64 inline body
//     in cmd, no srcs entry, cmake-codegen-lifted tag set.
//   - CONTENT-form with $<CONFIG>: legacy bytes-embedded shape,
//     cmake-codegen-file-generate-genex audit tag, no lifted
//     tag.
func TestEmit_FileGenerate_Golden(t *testing.T) {
	src, err := filepath.Abs("../../testdata/sample-projects/file-generate")
	if err != nil {
		t.Fatal(err)
	}
	replyDir, err := filepath.Abs("../../testdata/fileapi/file-generate")
	if err != nil {
		t.Fatal(err)
	}
	r, err := fileapi.Load(replyDir)
	if err != nil {
		t.Fatal(err)
	}
	traceRaw, err := os.ReadFile(filepath.Join(replyDir, "trace.jsonl"))
	if err != nil {
		t.Fatalf("read trace: %v", err)
	}
	// Load the captured cmake variable namespace so the (a)
	// genex evaluator can resolve $<CONFIG> / $<PLATFORM_ID:>
	// against the fixture's recorded Release/Linux/GNU values.
	// Mirrors the offline branch of convert-element-cmake/main
	// which does the same opportunistic load.
	cmakeVars, err := cmakerun.ReadVarsDumpFromReplyDir(replyDir)
	if err != nil {
		t.Fatalf("read vars dump: %v", err)
	}
	pkg, err := lower.ToIR(r, nil, lower.Options{
		HostSourceRoot:    src,
		BuildDir:          replyDir,
		TraceRaw:          traceRaw,
		LiftConfigureFile: true,
		CMakeVars:         cmakeVars,
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := bazel.Emit(pkg)
	if err != nil {
		t.Fatal(err)
	}
	got = scrubSourceLine(got, src)

	goldenPath := filepath.Join("..", "..", "testdata", "golden", "file-generate", "BUILD.bazel.golden")
	if *update {
		_ = os.MkdirAll(filepath.Dir(goldenPath), 0o755)
		if err := os.WriteFile(goldenPath, got, 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("updated %s", goldenPath)
		return
	}
	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("BUILD.bazel mismatch\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// TestEmit_ExecuteProcess_Fallback_Shape locks in the
// rendered shape of the kind:cmake round-2 fallback
// placeholder: extract genrule with install_tree.tar src and
// per-file outs, plus per-target stubs (cc_import +
// static_library / shared_library, sh_binary + srcs). The
// test constructs a synthetic ir.Package matching what
// emitFallbackPlaceholder produces and asserts the literal
// rendered bytes (no separate golden file — keeps the
// invariant close to the test for easy review).
func TestEmit_ExecuteProcess_Fallback_Shape(t *testing.T) {
	pkg := &ir.Package{
		Name: "demo",
		Targets: []ir.Target{
			{
				Name:        "_install_tree_extract",
				Kind:        ir.KindGenrule,
				Srcs:        []string{"install_tree.tar"},
				GenruleOuts: []string{"install_tree/lib/libthelib.a", "install_tree/bin/thetool"},
				GenruleCmd:  `mkdir -p "$(RULEDIR)/install_tree" && tar -C "$(RULEDIR)/install_tree" -xf "$(location install_tree.tar)"`,
				Tags:        []string{"cmake-codegen-execute-process-fallback", "cmake-codegen-execute-process-fallback-extract"},
				Visibility:  []string{"//visibility:private"},
			},
			{
				Name:          "thelib",
				Kind:          ir.KindCCImport,
				StaticLibrary: "install_tree/lib/libthelib.a",
				Tags:          []string{"cmake-codegen-execute-process-fallback"},
				Visibility:    []string{"//visibility:public"},
			},
			{
				Name:       "thetool",
				Kind:       ir.KindShBinary,
				Srcs:       []string{"install_tree/bin/thetool"},
				Tags:       []string{"cmake-codegen-execute-process-fallback"},
				Visibility: []string{"//visibility:public"},
			},
		},
	}
	got, err := bazel.Emit(pkg)
	if err != nil {
		t.Fatal(err)
	}
	gotStr := string(got)

	// Load line: cc_import comes from rules_cc; cc_library
	// / cc_binary aren't used in the placeholder so the load
	// should only mention cc_import. sh_binary is built-in
	// (no load needed).
	if !strings.Contains(gotStr, `load("@rules_cc//cc:defs.bzl", "cc_import")`) {
		t.Errorf("expected cc_import load; got:\n%s", gotStr)
	}
	if strings.Contains(gotStr, `"cc_library"`) {
		t.Errorf("placeholder should not load cc_library; got:\n%s", gotStr)
	}

	// Extract genrule rendering: srcs + outs + cmd + tags.
	for _, want := range []string{
		`name = "_install_tree_extract"`,
		`srcs = ["install_tree.tar"]`,
		`"install_tree/lib/libthelib.a"`,
		`"install_tree/bin/thetool"`,
		`tar -C`,
	} {
		if !strings.Contains(gotStr, want) {
			t.Errorf("extract genrule missing %q; got:\n%s", want, gotStr)
		}
	}

	// cc_import rendering: static_library attribute.
	for _, want := range []string{
		`cc_import(`,
		`name = "thelib"`,
		`static_library = "install_tree/lib/libthelib.a"`,
	} {
		if !strings.Contains(gotStr, want) {
			t.Errorf("cc_import shape missing %q; got:\n%s", want, gotStr)
		}
	}

	// sh_binary rendering: srcs attribute.
	for _, want := range []string{
		`sh_binary(`,
		`name = "thetool"`,
		`srcs = ["install_tree/bin/thetool"]`,
	} {
		if !strings.Contains(gotStr, want) {
			t.Errorf("sh_binary shape missing %q; got:\n%s", want, gotStr)
		}
	}
}

// TestEmit_CCImport_SharedLibrary asserts the SHARED_LIBRARY
// path of cc_import: shared_library attribute populated
// instead of static_library. Mirrors the
// fallback placeholder's SHARED/MODULE_LIBRARY → cc_import +
// shared_library dispatch.
func TestEmit_CCImport_SharedLibrary(t *testing.T) {
	pkg := &ir.Package{
		Targets: []ir.Target{{
			Name:          "shared",
			Kind:          ir.KindCCImport,
			SharedLibrary: "install_tree/lib/libshared.so.1",
			Visibility:    []string{"//visibility:public"},
		}},
	}
	got, err := bazel.Emit(pkg)
	if err != nil {
		t.Fatal(err)
	}
	gotStr := string(got)
	if !strings.Contains(gotStr, `shared_library = "install_tree/lib/libshared.so.1"`) {
		t.Errorf("expected shared_library attribute; got:\n%s", gotStr)
	}
	if strings.Contains(gotStr, `static_library`) {
		t.Errorf("static_library should not appear; got:\n%s", gotStr)
	}
}

// TestEmit_ExecuteProcess_CMakeE_Golden exercises the
// converter's execute_process recovery for the BucketCMakeE
// path: a CMakeLists.txt calls
// `execute_process(COMMAND ${CMAKE_COMMAND} -E touch ...)` at
// configure time. The trace records the call; the bucket
// classifier flags it as cmake-e/touch; the lifter emits a
// genrule with cmd="touch $@", outs=[marker.stamp], and the
// cmake-codegen-execute-process tag set. cc_library compile
// of the unrelated source file is unaffected.
func TestEmit_ExecuteProcess_CMakeE_Golden(t *testing.T) {
	src, err := filepath.Abs("../../testdata/sample-projects/execute-process-cmake-e")
	if err != nil {
		t.Fatal(err)
	}
	replyDir, err := filepath.Abs("../../testdata/fileapi/execute-process-cmake-e")
	if err != nil {
		t.Fatal(err)
	}
	r, err := fileapi.Load(replyDir)
	if err != nil {
		t.Fatal(err)
	}
	traceRaw, err := os.ReadFile(filepath.Join(replyDir, "trace.jsonl"))
	if err != nil {
		t.Fatalf("read trace: %v", err)
	}
	pkg, err := lower.ToIR(r, nil, lower.Options{
		HostSourceRoot: src,
		BuildDir:       replyDir,
		TraceRaw:       traceRaw,
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := bazel.Emit(pkg)
	if err != nil {
		t.Fatal(err)
	}
	got = scrubSourceLine(got, src)

	goldenPath := filepath.Join("..", "..", "testdata", "golden", "execute-process-cmake-e", "BUILD.bazel.golden")
	if *update {
		_ = os.MkdirAll(filepath.Dir(goldenPath), 0o755)
		if err := os.WriteFile(goldenPath, got, 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("updated %s", goldenPath)
		return
	}
	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("BUILD.bazel mismatch\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// TestEmit_ExecuteProcess_FileProducing_Golden exercises the
// converter's execute_process recovery for the
// BucketFileProducing path: a CMakeLists.txt runs a Python
// generator at configure time and redirects its stdout to a
// build-tree header via OUTPUT_FILE. The lifter hoists the
// call to a build-time genrule with scripts/gen.py + spec.txt
// as Bazel-tracked srcs; the host-resolved python3 path is
// stripped to basename for portability across executors. The
// cmake-codegen-execute-process-hoisted tag flags the
// configure-time → build-time move for audit queries.
func TestEmit_ExecuteProcess_FileProducing_Golden(t *testing.T) {
	src, err := filepath.Abs("../../testdata/sample-projects/execute-process-file-producing")
	if err != nil {
		t.Fatal(err)
	}
	replyDir, err := filepath.Abs("../../testdata/fileapi/execute-process-file-producing")
	if err != nil {
		t.Fatal(err)
	}
	r, err := fileapi.Load(replyDir)
	if err != nil {
		t.Fatal(err)
	}
	traceRaw, err := os.ReadFile(filepath.Join(replyDir, "trace.jsonl"))
	if err != nil {
		t.Fatalf("read trace: %v", err)
	}
	pkg, err := lower.ToIR(r, nil, lower.Options{
		HostSourceRoot: src,
		BuildDir:       replyDir,
		TraceRaw:       traceRaw,
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := bazel.Emit(pkg)
	if err != nil {
		t.Fatal(err)
	}
	got = scrubSourceLine(got, src)

	goldenPath := filepath.Join("..", "..", "testdata", "golden", "execute-process-file-producing", "BUILD.bazel.golden")
	if *update {
		_ = os.MkdirAll(filepath.Dir(goldenPath), 0o755)
		if err := os.WriteFile(goldenPath, got, 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("updated %s", goldenPath)
		return
	}
	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("BUILD.bazel mismatch\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// TestEmit_MultiTarget_Golden exercises the cc_library / cc_binary
// rule-kind dispatch + the static / shared distinction. Three
// targets in one project: a STATIC library (linkstatic = True), a
// SHARED library (no linkstatic; -fPIC + <name>_EXPORTS define), a
// binary (cc_binary, hdrs folded into srcs per Bazel 9). All emit
// in one BUILD with the right rule kind per target.
func TestEmit_MultiTarget_Golden(t *testing.T) {
	src, err := filepath.Abs("../../testdata/sample-projects/multi-target")
	if err != nil {
		t.Fatal(err)
	}
	r, err := fileapi.Load("../../testdata/fileapi/multi-target")
	if err != nil {
		t.Fatal(err)
	}
	pkg, err := lower.ToIR(r, nil, lower.Options{HostSourceRoot: src})
	if err != nil {
		t.Fatal(err)
	}
	got, err := bazel.Emit(pkg)
	if err != nil {
		t.Fatal(err)
	}
	got = scrubSourceLine(got, src)

	goldenPath := filepath.Join("..", "..", "testdata", "golden", "multi-target", "BUILD.bazel.golden")
	if *update {
		_ = os.MkdirAll(filepath.Dir(goldenPath), 0o755)
		if err := os.WriteFile(goldenPath, got, 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("updated %s", goldenPath)
		return
	}
	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("BUILD.bazel mismatch\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// TestEmit_FindPackage_Golden exercises the imports-manifest
// rewrite path: cmake's find_package(ZLIB) + target_link_libraries
// with ZLIB::ZLIB produces a codemodel link fragment for the
// system libz path; the imports manifest maps that to a Bazel
// label, and the emitter substitutes the link path with the
// label in the cc_library's deps. Confirms the synth-prefix /
// imports-manifest plumbing surfaces real out-of-tree deps as
// stable Bazel labels rather than absolute /usr/lib paths.
func TestEmit_FindPackage_Golden(t *testing.T) {
	src, err := filepath.Abs("../../testdata/sample-projects/find-package")
	if err != nil {
		t.Fatal(err)
	}
	r, err := fileapi.Load("../../testdata/fileapi/find-package")
	if err != nil {
		t.Fatal(err)
	}
	imports, err := manifest.Load(filepath.Join(src, "imports.json"))
	if err != nil {
		t.Fatalf("load imports manifest: %v", err)
	}
	pkg, err := lower.ToIR(r, nil, lower.Options{HostSourceRoot: src, Imports: imports})
	if err != nil {
		t.Fatal(err)
	}
	got, err := bazel.Emit(pkg)
	if err != nil {
		t.Fatal(err)
	}
	got = scrubSourceLine(got, src)

	goldenPath := filepath.Join("..", "..", "testdata", "golden", "find-package", "BUILD.bazel.golden")
	if *update {
		_ = os.MkdirAll(filepath.Dir(goldenPath), 0o755)
		if err := os.WriteFile(goldenPath, got, 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("updated %s", goldenPath)
		return
	}
	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("BUILD.bazel mismatch\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// TestEmit_FindPackageStatic_Golden exercises the STATIC
// IMPORTED-dep recovery path. For STATIC archives cmake's
// codemodel records no `dependencies` and no Link
// (no link step for an .a), so an IMPORTED target like
// ZLIB::ZLIB used via target_link_libraries is invisible
// from the codemodel alone. Lower's STATIC fallback
// consults the trace's target_link_libraries call to
// surface the dep.
func TestEmit_FindPackageStatic_Golden(t *testing.T) {
	src, err := filepath.Abs("../../testdata/sample-projects/find-package-static")
	if err != nil {
		t.Fatal(err)
	}
	r, err := fileapi.Load("../../testdata/fileapi/find-package-static")
	if err != nil {
		t.Fatal(err)
	}
	imports, err := manifest.Load(filepath.Join(src, "imports.json"))
	if err != nil {
		t.Fatalf("load imports manifest: %v", err)
	}
	traceRaw, err := os.ReadFile("../../testdata/fileapi/find-package-static/trace.jsonl")
	if err != nil {
		t.Fatalf("read trace: %v", err)
	}
	pkg, err := lower.ToIR(r, nil, lower.Options{HostSourceRoot: src, Imports: imports, TraceRaw: traceRaw})
	if err != nil {
		t.Fatal(err)
	}
	got, err := bazel.Emit(pkg)
	if err != nil {
		t.Fatal(err)
	}
	got = scrubSourceLine(got, src)

	goldenPath := filepath.Join("..", "..", "testdata", "golden", "find-package-static", "BUILD.bazel.golden")
	if *update {
		_ = os.MkdirAll(filepath.Dir(goldenPath), 0o755)
		if err := os.WriteFile(goldenPath, got, 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("updated %s", goldenPath)
		return
	}
	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("BUILD.bazel mismatch\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// TestEmit_PkgConfig_Golden covers the pkg-config IMPORTED_TARGET
// shape: pkg_check_modules(... IMPORTED_TARGET ...) synthesizes
// a PkgConfig::<NAME> IMPORTED interface target carrying the
// pkg-config-derived cflags/ldflags. Consumers
// target_link_libraries against it; cmake records the dep via
// the link-fragment path same as find_package's IMPORTED
// targets. Imports manifest maps the PkgConfig::<NAME> name to
// the same Bazel label that ZLIB::ZLIB would resolve to —
// alternative names, same underlying system lib.
func TestEmit_PkgConfig_Golden(t *testing.T) {
	src, err := filepath.Abs("../../testdata/sample-projects/pkg-config")
	if err != nil {
		t.Fatal(err)
	}
	r, err := fileapi.Load("../../testdata/fileapi/pkg-config")
	if err != nil {
		t.Fatal(err)
	}
	imports, err := manifest.Load(filepath.Join(src, "imports.json"))
	if err != nil {
		t.Fatalf("load imports manifest: %v", err)
	}
	traceRaw, err := os.ReadFile("../../testdata/fileapi/pkg-config/trace.jsonl")
	if err != nil {
		t.Fatalf("read trace: %v", err)
	}
	pkg, err := lower.ToIR(r, nil, lower.Options{HostSourceRoot: src, Imports: imports, TraceRaw: traceRaw})
	if err != nil {
		t.Fatal(err)
	}
	got, err := bazel.Emit(pkg)
	if err != nil {
		t.Fatal(err)
	}
	got = scrubSourceLine(got, src)

	goldenPath := filepath.Join("..", "..", "testdata", "golden", "pkg-config", "BUILD.bazel.golden")
	if *update {
		_ = os.MkdirAll(filepath.Dir(goldenPath), 0o755)
		if err := os.WriteFile(goldenPath, got, 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("updated %s", goldenPath)
		return
	}
	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("BUILD.bazel mismatch\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// TestEmit_AliasTarget_Golden covers cmake's
// add_library(... ALIAS ...) shape. cmake's File API
// codemodel records ONLY the underlying real target in
// configurations[].targets[] — the alias is invisible.
// When a consumer does target_link_libraries(t
// aliaslib::aliaslib), cmake resolves the alias and emits
// the dep edge as if it were against the real target.
// Lower's dep resolution sees the resolved id and produces
// a clean :aliaslib reference. No special-case code in lower
// — this test is a regression guard against future changes.
func TestEmit_AliasTarget_Golden(t *testing.T) {
	src, err := filepath.Abs("../../testdata/sample-projects/alias-target")
	if err != nil {
		t.Fatal(err)
	}
	r, err := fileapi.Load("../../testdata/fileapi/alias-target")
	if err != nil {
		t.Fatal(err)
	}
	traceRaw, err := os.ReadFile("../../testdata/fileapi/alias-target/trace.jsonl")
	if err != nil {
		t.Fatalf("read trace: %v", err)
	}
	pkg, err := lower.ToIR(r, nil, lower.Options{HostSourceRoot: src, TraceRaw: traceRaw})
	if err != nil {
		t.Fatal(err)
	}
	got, err := bazel.Emit(pkg)
	if err != nil {
		t.Fatal(err)
	}
	got = scrubSourceLine(got, src)

	goldenPath := filepath.Join("..", "..", "testdata", "golden", "alias-target", "BUILD.bazel.golden")
	if *update {
		_ = os.MkdirAll(filepath.Dir(goldenPath), 0o755)
		if err := os.WriteFile(goldenPath, got, 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("updated %s", goldenPath)
		return
	}
	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("BUILD.bazel mismatch\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// TestEmit_ObjectLibrary_Golden covers cmake's
// add_library(... OBJECT). cmake compiles the sources to
// .o files without archiving; consumers reference
// $<TARGET_OBJECTS:t> in their srcs to inline the objects
// into a downstream artifact. Codemodel emits an
// OBJECT_LIBRARY target with CompileGroups but no Link.
// Lower maps it to cc_library with alwayslink=True; the
// downstream archive's deps reference it, and the inlined
// objects flow naturally via Bazel's link-once-per-consumer
// shape.
func TestEmit_ObjectLibrary_Golden(t *testing.T) {
	src, err := filepath.Abs("../../testdata/sample-projects/object-library")
	if err != nil {
		t.Fatal(err)
	}
	r, err := fileapi.Load("../../testdata/fileapi/object-library")
	if err != nil {
		t.Fatal(err)
	}
	traceRaw, err := os.ReadFile("../../testdata/fileapi/object-library/trace.jsonl")
	if err != nil {
		t.Fatalf("read trace: %v", err)
	}
	pkg, err := lower.ToIR(r, nil, lower.Options{HostSourceRoot: src, TraceRaw: traceRaw})
	if err != nil {
		t.Fatal(err)
	}
	got, err := bazel.Emit(pkg)
	if err != nil {
		t.Fatal(err)
	}
	got = scrubSourceLine(got, src)

	goldenPath := filepath.Join("..", "..", "testdata", "golden", "object-library", "BUILD.bazel.golden")
	if *update {
		_ = os.MkdirAll(filepath.Dir(goldenPath), 0o755)
		if err := os.WriteFile(goldenPath, got, 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("updated %s", goldenPath)
		return
	}
	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("BUILD.bazel mismatch\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// TestEmit_InterfaceLibrary_Golden exercises a consumer of
// an INTERFACE_LIBRARY. cmake's codemodel doesn't emit
// pure-INTERFACE targets in configurations[].targets[]
// (verified against cmake 3.28 — INTERFACE libs without
// buildable artifacts are invisible to the File API). What
// IS visible: the consumer's CompileGroups[].Includes
// records the flattened INTERFACE_INCLUDE_DIRECTORIES the
// INTERFACE lib propagated. So the test covers "consumer
// gets the right includes after cmake flattens the
// INTERFACE chain", not "INTERFACE lib emitted as a
// standalone Bazel target".
//
// Cross-element exposure of an INTERFACE library (via
// install(EXPORT)) is a separate concern — the
// orchestrator's cmakecfg-bundle path covers that.
func TestEmit_InterfaceLibrary_Golden(t *testing.T) {
	src, err := filepath.Abs("../../testdata/sample-projects/interface-library")
	if err != nil {
		t.Fatal(err)
	}
	r, err := fileapi.Load("../../testdata/fileapi/interface-library")
	if err != nil {
		t.Fatal(err)
	}
	traceRaw, err := os.ReadFile("../../testdata/fileapi/interface-library/trace.jsonl")
	if err != nil {
		t.Fatalf("read trace: %v", err)
	}
	pkg, err := lower.ToIR(r, nil, lower.Options{HostSourceRoot: src, TraceRaw: traceRaw})
	if err != nil {
		t.Fatal(err)
	}
	got, err := bazel.Emit(pkg)
	if err != nil {
		t.Fatal(err)
	}
	got = scrubSourceLine(got, src)

	goldenPath := filepath.Join("..", "..", "testdata", "golden", "interface-library", "BUILD.bazel.golden")
	if *update {
		_ = os.MkdirAll(filepath.Dir(goldenPath), 0o755)
		if err := os.WriteFile(goldenPath, got, 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("updated %s", goldenPath)
		return
	}
	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("BUILD.bazel mismatch\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// TestEmit_HeaderOnlyDummyShim_Golden exercises the
// build-dir-rooted absolute-source elision from #143 against a
// real recorded fileapi reply. The sample project's
// CMakeLists.txt uses the yasm/libpng-style shim pattern —
// `file(WRITE ${CMAKE_BINARY_DIR}/dummy.cpp "")` followed by
// `add_library(foo STATIC ${CMAKE_BINARY_DIR}/dummy.cpp)` — so
// cmake records one source with an absolute build-dir path
// without IsGenerated set. Lower should drop that source from
// the rendered cc_library.srcs and tag the target with
// `cmake-elided-build-dir-source`.
//
// The golden BUILD.bazel pins the expected shape: a cc_library
// with empty srcs (header-only), the FILE_SET headers in hdrs,
// and the elision audit tag. A regression that re-emitted the
// absolute /tmp path would change the rendered srcs and trip
// the golden diff.
func TestEmit_HeaderOnlyDummyShim_Golden(t *testing.T) {
	src, err := filepath.Abs("../../testdata/sample-projects/header-only-dummy-shim")
	if err != nil {
		t.Fatal(err)
	}
	r, err := fileapi.Load("../../testdata/fileapi/header-only-dummy-shim")
	if err != nil {
		t.Fatal(err)
	}
	pkg, err := lower.ToIR(r, nil, lower.Options{HostSourceRoot: src})
	if err != nil {
		t.Fatal(err)
	}
	got, err := bazel.Emit(pkg)
	if err != nil {
		t.Fatal(err)
	}
	got = scrubSourceLine(got, src)

	goldenPath := filepath.Join("..", "..", "testdata", "golden", "header-only-dummy-shim", "BUILD.bazel.golden")
	if *update {
		_ = os.MkdirAll(filepath.Dir(goldenPath), 0o755)
		if err := os.WriteFile(goldenPath, got, 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("updated %s", goldenPath)
		return
	}
	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("BUILD.bazel mismatch\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// TestEmit_MacroFromImport_Golden exercises the
// macro-from-import case: the consumer cmake project
// includes a macro shipped by a producer element. The
// macro lives outside the consumer source root (in the
// fixture's `producer/` sibling, simulating a producer
// element's installed cmake module). When the macro calls
// target_link_libraries(consumer_target ...) cmake records
// the call with `file` pointing at the producer module —
// outside the consumer source root. lower's trace filter
// keeps the call via the known-target rescue path
// (knownTargets[args[0]]), so STATIC IMPORTED dep recovery
// still fires.
//
// Golden expectation: the cc_library has a `deps` entry
// for ZLIB even though the target_link_libraries call lives
// in the producer's Helpers.cmake.
func TestEmit_MacroFromImport_Golden(t *testing.T) {
	src, err := filepath.Abs("../../testdata/sample-projects/macro-from-import/consumer")
	if err != nil {
		t.Fatal(err)
	}
	r, err := fileapi.Load("../../testdata/fileapi/macro-from-import")
	if err != nil {
		t.Fatal(err)
	}
	importsPath, err := filepath.Abs("../../testdata/sample-projects/macro-from-import/imports.json")
	if err != nil {
		t.Fatal(err)
	}
	imports, err := manifest.Load(importsPath)
	if err != nil {
		t.Fatalf("load imports manifest: %v", err)
	}
	traceRaw, err := os.ReadFile("../../testdata/fileapi/macro-from-import/trace.jsonl")
	if err != nil {
		t.Fatalf("read trace: %v", err)
	}
	pkg, err := lower.ToIR(r, nil, lower.Options{HostSourceRoot: src, Imports: imports, TraceRaw: traceRaw})
	if err != nil {
		t.Fatal(err)
	}
	got, err := bazel.Emit(pkg)
	if err != nil {
		t.Fatal(err)
	}
	got = scrubSourceLine(got, src)

	goldenPath := filepath.Join("..", "..", "testdata", "golden", "macro-from-import", "BUILD.bazel.golden")
	if *update {
		_ = os.MkdirAll(filepath.Dir(goldenPath), 0o755)
		if err := os.WriteFile(goldenPath, got, 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("updated %s", goldenPath)
		return
	}
	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("BUILD.bazel mismatch\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// TestEmit_GeneratorExpressions_Golden exercises cmake's $<...>
// generator expressions. The codemodel resolves them at
// configure time, so what surfaces in CompileGroups[].Includes
// / Defines / Compile-fragments is the resolved-for-this-config
// values, not generator-expression literals. Confirms
// convert-element-cmake doesn't trip on the expressions and emits
// the resolved values cleanly. Known clean — no gap.
func TestEmit_GeneratorExpressions_Golden(t *testing.T) {
	src, err := filepath.Abs("../../testdata/sample-projects/generator-expressions")
	if err != nil {
		t.Fatal(err)
	}
	r, err := fileapi.Load("../../testdata/fileapi/generator-expressions")
	if err != nil {
		t.Fatal(err)
	}
	pkg, err := lower.ToIR(r, nil, lower.Options{HostSourceRoot: src})
	if err != nil {
		t.Fatal(err)
	}
	got, err := bazel.Emit(pkg)
	if err != nil {
		t.Fatal(err)
	}
	got = scrubSourceLine(got, src)
	goldenPath := filepath.Join("..", "..", "testdata", "golden", "generator-expressions", "BUILD.bazel.golden")
	if *update {
		_ = os.MkdirAll(filepath.Dir(goldenPath), 0o755)
		if err := os.WriteFile(goldenPath, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("BUILD.bazel mismatch\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// TestEmit_MultiLanguage_Golden exercises C+C++ in a single
// cc_library target. cmake codemodel emits one CompileGroup per
// language; lower's "at most one language per target"
// assumption (cg := t.CompileGroups[0]) drops the second
// language's flags entirely.
//
// Known delta captured by the golden:
//   - copts emitted = first compile group's only.
//     `cxx_part.cpp` would be compiled with `-std=c11` (the C
//     std flag), failing as C++ in C dialect.
//
// Fix shape (deferred): split multi-language targets into one
// cc_library per language. See docs/cmake-conversion-deltas.md.
func TestEmit_MultiLanguage_Golden(t *testing.T) {
	src, err := filepath.Abs("../../testdata/sample-projects/multi-language")
	if err != nil {
		t.Fatal(err)
	}
	r, err := fileapi.Load("../../testdata/fileapi/multi-language")
	if err != nil {
		t.Fatal(err)
	}
	pkg, err := lower.ToIR(r, nil, lower.Options{HostSourceRoot: src})
	if err != nil {
		t.Fatal(err)
	}
	got, err := bazel.Emit(pkg)
	if err != nil {
		t.Fatal(err)
	}
	got = scrubSourceLine(got, src)
	goldenPath := filepath.Join("..", "..", "testdata", "golden", "multi-language", "BUILD.bazel.golden")
	if *update {
		_ = os.MkdirAll(filepath.Dir(goldenPath), 0o755)
		if err := os.WriteFile(goldenPath, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("BUILD.bazel mismatch\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// TestEmit_Visibility_Golden exercises target_include_directories'
// PUBLIC vs PRIVATE distinction. The codemodel doesn't tag
// individual include entries with visibility — both arms flatten
// into compileGroups[].includes[]. lower recovers the keyword
// arms from cmake's --trace-expand output (parsed in
// internal/shadow). PUBLIC dirs flow into cc_library.includes
// (consumer-visible); PRIVATE dirs flow into copts as
// `-I<dir>` (compile-only, not propagated). PRIVATE-only
// headers don't surface in `hdrs` because discoverHeaders
// only walks the public include set.
func TestEmit_Visibility_Golden(t *testing.T) {
	src, err := filepath.Abs("../../testdata/sample-projects/visibility")
	if err != nil {
		t.Fatal(err)
	}
	r, err := fileapi.Load("../../testdata/fileapi/visibility")
	if err != nil {
		t.Fatal(err)
	}
	traceRaw, err := os.ReadFile("../../testdata/fileapi/visibility/trace.jsonl")
	if err != nil {
		t.Fatalf("read trace: %v", err)
	}
	pkg, err := lower.ToIR(r, nil, lower.Options{HostSourceRoot: src, TraceRaw: traceRaw})
	if err != nil {
		t.Fatal(err)
	}
	got, err := bazel.Emit(pkg)
	if err != nil {
		t.Fatal(err)
	}
	got = scrubSourceLine(got, src)
	goldenPath := filepath.Join("..", "..", "testdata", "golden", "visibility", "BUILD.bazel.golden")
	if *update {
		_ = os.MkdirAll(filepath.Dir(goldenPath), 0o755)
		if err := os.WriteFile(goldenPath, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("BUILD.bazel mismatch\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// TestEmit_ImplementationDeps covers Phase 4's
// implementation_deps split. The IR field is populated
// directly here (the CMake codemodel-lowering path has no
// signal source for PUBLIC/PRIVATE scope today; the test
// exercises the emit-side rendering against a hand-built
// Target). Buildifier's NamePriority places
// implementation_deps after deps in the rendered attribute
// order — both at priority 0, with deps drifting to priority
// 4 ahead of alwayslink. Confirms the new IR field renders
// when set without disrupting the deps-only case.
func TestEmit_ImplementationDeps(t *testing.T) {
	pkg := &ir.Package{
		Targets: []ir.Target{
			{
				Name:               "lib",
				Kind:               ir.KindCCLibrary,
				Srcs:               []string{"lib.cc"},
				Hdrs:               []string{"lib.h"},
				Deps:               []string{"//public:dep"},
				ImplementationDeps: []string{"//internal:helper"},
			},
		},
	}
	got, err := bazel.Emit(pkg)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	for _, marker := range []string{
		`deps = ["//public:dep"]`,
		`implementation_deps = ["//internal:helper"]`,
	} {
		if !strings.Contains(string(got), marker) {
			t.Errorf("missing marker %q\n--body--\n%s", marker, got)
		}
	}
	// implementation_deps appears BEFORE deps in the buildifier-
	// canonical attribute order: implementation_deps lives in
	// the priority-0 alpha block while `deps` has its own
	// NamePriority slot (4) that floats it later in the rule.
	// Confirms our emit lands the canonical shape gazelle_cc
	// users expect on a cc_library with both attributes set.
	implIdx := strings.Index(string(got), "implementation_deps =")
	// `deps =` is a substring of `implementation_deps =`; match
	// the bare `\n    deps =` to find the standalone occurrence.
	depsIdx := strings.Index(string(got), "\n    deps =")
	if implIdx < 0 || depsIdx < 0 || implIdx > depsIdx {
		t.Errorf("expected implementation_deps before deps; impl=%d deps=%d", implIdx, depsIdx)
	}
}

// TestEmit_NoImplementationDeps_OmitsAttribute confirms the
// rendered output stays byte-stable when ImplementationDeps
// is empty — the field's mere presence on ir.Target must not
// emit an empty `implementation_deps = []` attribute that
// would noise up every existing cc_library golden.
func TestEmit_NoImplementationDeps_OmitsAttribute(t *testing.T) {
	pkg := &ir.Package{
		Targets: []ir.Target{
			{
				Name: "lib",
				Kind: ir.KindCCLibrary,
				Srcs: []string{"lib.cc"},
				Deps: []string{"//public:dep"},
			},
		},
	}
	got, err := bazel.Emit(pkg)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if strings.Contains(string(got), "implementation_deps") {
		t.Errorf("expected implementation_deps attribute to be omitted when empty:\n%s", got)
	}
}

// TestEmit_GazelleCcSearch_UnionDedupSort confirms the Phase 7d
// `# gazelle:cc_search "" <pkgpath>/<dir>` file-head directives
// mirror the union of every target's `includes`, deduped across
// targets and emitted in sorted order. Two targets share
// "include"; one adds "src". The directives must render once
// each, sorted, above the load() line — the conventional
// gazelle-directive placement — with the package-rooted form
// gazelle_cc's repo-root-relative resolver expects.
func TestEmit_GazelleCcSearch_UnionDedupSort(t *testing.T) {
	pkg := &ir.Package{
		Targets: []ir.Target{
			{
				Name:     "b",
				Kind:     ir.KindCCLibrary,
				Srcs:     []string{"b.cc"},
				Includes: []string{"src", "include"},
			},
			{
				Name:     "a",
				Kind:     ir.KindCCLibrary,
				Srcs:     []string{"a.cc"},
				Includes: []string{"include"},
			},
		},
	}
	got, err := bazel.EmitWithOptions(pkg, bazel.Options{BazelPackagePath: "elements/multi"})
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	body := string(got)
	incLine := `# gazelle:cc_search "" elements/multi/include`
	srcLine := `# gazelle:cc_search "" elements/multi/src`
	incIdx := strings.Index(body, incLine)
	srcIdx := strings.Index(body, srcLine)
	loadIdx := strings.Index(body, "load(")
	if incIdx < 0 || srcIdx < 0 {
		t.Fatalf("missing cc_search directive(s):\n%s", body)
	}
	// Sorted: "include" before "src".
	if incIdx > srcIdx {
		t.Errorf("cc_search directives not sorted: include=%d src=%d\n%s", incIdx, srcIdx, body)
	}
	// File-head: above the load() line.
	if loadIdx >= 0 && srcIdx > loadIdx {
		t.Errorf("cc_search directives must precede load(): src=%d load=%d\n%s", srcIdx, loadIdx, body)
	}
	// Deduped: "include" appears exactly once despite two targets
	// declaring it.
	if n := strings.Count(body, incLine+"\n"); n != 1 {
		t.Errorf("cc_search include directive emitted %d times, want 1\n%s", n, body)
	}
}

// TestEmit_NoGazelleCcSearch_OmitsDirective confirms a package
// whose targets carry no `includes` emits no `# gazelle:cc_search`
// line at all — even with a BazelPackagePath set — keeping
// includes-free goldens byte-stable.
func TestEmit_NoGazelleCcSearch_OmitsDirective(t *testing.T) {
	pkg := &ir.Package{
		Targets: []ir.Target{
			{
				Name: "lib",
				Kind: ir.KindCCLibrary,
				Srcs: []string{"lib.cc"},
			},
		},
	}
	got, err := bazel.EmitWithOptions(pkg, bazel.Options{BazelPackagePath: "elements/lib"})
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if strings.Contains(string(got), "# gazelle:cc_search") {
		t.Errorf("expected no cc_search directive when no target has includes:\n%s", got)
	}
}

// TestEmit_GazelleCcSearch_RequiresBazelPackagePath confirms that
// omitting BazelPackagePath suppresses the cc_search directive
// even when targets do carry `includes`. gazelle_cc interprets
// the directive's path argument repo-root relative, so emitting
// it without knowing the package frame would point gazelle_cc at
// the wrong place. Zero-Options unit tests therefore emit
// nothing rather than wrong bytes.
func TestEmit_GazelleCcSearch_RequiresBazelPackagePath(t *testing.T) {
	pkg := &ir.Package{
		Targets: []ir.Target{
			{
				Name:     "lib",
				Kind:     ir.KindCCLibrary,
				Srcs:     []string{"lib.cc"},
				Includes: []string{"include"},
			},
		},
	}
	got, err := bazel.Emit(pkg)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if strings.Contains(string(got), "# gazelle:cc_search") {
		t.Errorf("expected no cc_search directive without BazelPackagePath:\n%s", got)
	}
}

// TestEmit_ConstraintViolation_PreEmitGuard wires that
// bazelconstraints.ValidatePackage fires through Emit and
// surfaces the typed-error prefix the wrap adds. Drives the
// three hazard shapes the constraints package covers (#193
// empty cmd, #194 duplicate deps, malformed name) at the
// emit boundary so a future refactor that decouples emit
// from validation fails loudly here.
func TestEmit_ConstraintViolation_PreEmitGuard(t *testing.T) {
	cases := []struct {
		name string
		pkg  *ir.Package
		want string // substring expected in the wrapped error
	}{
		{
			name: "empty genrule cmd (issue #193 shape)",
			pkg: &ir.Package{
				Targets: []ir.Target{{
					Name:        "gen_empty",
					Kind:        ir.KindGenrule,
					GenruleCmd:  "",
					GenruleOuts: []string{"out.h"},
				}},
			},
			want: "empty or whitespace-only",
		},
		{
			name: "duplicate deps (issue #194 shape)",
			pkg: &ir.Package{
				Targets: []ir.Target{{
					Name: "lib",
					Kind: ir.KindCCLibrary,
					Srcs: []string{"lib.cc"},
					Deps: []string{":foo", ":foo"},
				}},
			},
			want: `"deps"`,
		},
		{
			name: "malformed name (whitespace)",
			pkg: &ir.Package{
				Targets: []ir.Target{{
					Name: "bad name",
					Kind: ir.KindCCLibrary,
				}},
			},
			want: "valid Bazel identifier",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body, err := bazel.Emit(tc.pkg)
			if err == nil {
				t.Fatalf("Emit succeeded; want validator-wrapped error. body=%q", body)
			}
			msg := err.Error()
			if !strings.HasPrefix(msg, "bazel constraint violation:") {
				t.Errorf("err %q missing 'bazel constraint violation:' prefix", msg)
			}
			if !strings.Contains(msg, tc.want) {
				t.Errorf("err %q does not contain %q", msg, tc.want)
			}
		})
	}
}

// TestEmit_Filegroup_Basic covers KindFilegroup emission — Phase 1
// task 2's foundation for install(FILES) / install(DIRECTORY)
// lowering at convert time.
func TestEmit_Filegroup_Basic(t *testing.T) {
	pkg := &ir.Package{
		Targets: []ir.Target{{
			Name:       "headers",
			Kind:       ir.KindFilegroup,
			Srcs:       []string{"include/foo.h", "include/bar.h"},
			Visibility: []string{"//visibility:public"},
		}},
	}
	got, err := bazel.Emit(pkg)
	if err != nil {
		t.Fatal(err)
	}
	gotStr := string(got)
	if !strings.Contains(gotStr, `filegroup(`) {
		t.Errorf("expected filegroup rule; got:\n%s", gotStr)
	}
	if !strings.Contains(gotStr, `name = "headers"`) {
		t.Errorf("expected name attribute; got:\n%s", gotStr)
	}
	// Bazel sorts srcs entries; both should appear.
	if !strings.Contains(gotStr, `"include/bar.h"`) || !strings.Contains(gotStr, `"include/foo.h"`) {
		t.Errorf("expected srcs entries; got:\n%s", gotStr)
	}
}

// TestEmit_Provenance_RendersWhenEnabled covers Phase 1 task 1:
// when EmitProvenance is on, each rule with a non-zero Provenance
// gets a leading `# Source: <file>:<line> (<command>)` comment.
func TestEmit_Provenance_RendersWhenEnabled(t *testing.T) {
	pkg := &ir.Package{
		Targets: []ir.Target{{
			Name: "foo",
			Kind: ir.KindCCLibrary,
			Srcs: []string{"foo.c"},
			Provenance: ir.Provenance{
				File:    "CMakeLists.txt",
				Line:    42,
				Command: "add_library",
			},
		}},
	}
	got, err := bazel.EmitWithOptions(pkg, bazel.Options{EmitProvenance: true})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "# Source: CMakeLists.txt:42 (add_library)") {
		t.Errorf("expected provenance comment; got:\n%s", got)
	}
}

// TestEmit_Provenance_OmittedWhenDisabled confirms the comment is
// suppressed when the flag is off, keeping existing goldens
// byte-stable.
func TestEmit_Provenance_OmittedWhenDisabled(t *testing.T) {
	pkg := &ir.Package{
		Targets: []ir.Target{{
			Name: "foo",
			Kind: ir.KindCCLibrary,
			Srcs: []string{"foo.c"},
			Provenance: ir.Provenance{
				File:    "CMakeLists.txt",
				Line:    42,
				Command: "add_library",
			},
		}},
	}
	got, err := bazel.Emit(pkg)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(got), "# Source:") {
		t.Errorf("provenance should be suppressed when flag is off; got:\n%s", got)
	}
}

// TestEmit_Provenance_OmittedForZeroValue confirms the comment is
// suppressed when Provenance.IsZero() — lowerers that don't have
// backtrace data leave the field zero, and emit shouldn't render
// "# Source: :0 ()" garbage.
func TestEmit_Provenance_OmittedForZeroValue(t *testing.T) {
	pkg := &ir.Package{
		Targets: []ir.Target{{
			Name: "foo",
			Kind: ir.KindCCLibrary,
			Srcs: []string{"foo.c"},
			// no Provenance
		}},
	}
	got, err := bazel.EmitWithOptions(pkg, bazel.Options{EmitProvenance: true})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(got), "# Source:") {
		t.Errorf("zero-value Provenance should suppress comment; got:\n%s", got)
	}
}

// TestEmit_Provenance_OmitsLineAndCommandWhenEmpty handles the
// partial-population case: a lowerer that has only the file path
// shouldn't emit `:0 ()`.
func TestEmit_Provenance_OmitsLineAndCommandWhenEmpty(t *testing.T) {
	pkg := &ir.Package{
		Targets: []ir.Target{{
			Name: "foo",
			Kind: ir.KindCCLibrary,
			Srcs: []string{"foo.c"},
			Provenance: ir.Provenance{
				File: "CMakeLists.txt",
				// Line = 0, Command = ""
			},
		}},
	}
	got, err := bazel.EmitWithOptions(pkg, bazel.Options{EmitProvenance: true})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "# Source: CMakeLists.txt\n") {
		t.Errorf("expected bare-file comment without :0 ()  garbage; got:\n%s", got)
	}
}

// TestEmit_Filegroup_NoLoad confirms filegroup doesn't trigger a
// load() statement — it's in Bazel's global namespace.
func TestEmit_Filegroup_NoLoad(t *testing.T) {
	pkg := &ir.Package{
		Targets: []ir.Target{{
			Name: "data",
			Kind: ir.KindFilegroup,
			Srcs: []string{"data/foo.dat"},
		}},
	}
	got, err := bazel.Emit(pkg)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(got), `load("`) {
		t.Errorf("filegroup should not require a load(); got:\n%s", got)
	}
}

func TestEmit_FeaturesAttribute(t *testing.T) {
	pkg := &ir.Package{
		Targets: []ir.Target{{
			Name:     "foo",
			Kind:     ir.KindCCLibrary,
			Srcs:     []string{"foo.c"},
			Features: []string{"lto"},
		}},
	}
	got, err := bazel.Emit(pkg)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), `features = ["lto"]`) {
		t.Errorf("expected features attribute; got:\n%s", got)
	}
}

func TestEmit_NoFeaturesAttributeWhenEmpty(t *testing.T) {
	pkg := &ir.Package{
		Targets: []ir.Target{{
			Name: "foo",
			Kind: ir.KindCCLibrary,
			Srcs: []string{"foo.c"},
		}},
	}
	got, err := bazel.Emit(pkg)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(got), `features =`) {
		t.Errorf("features attribute should be omitted when empty; got:\n%s", got)
	}
}
