package lower_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/internal/fileapi"
	"github.com/sstriker/buildstream-bazel/converter/internal/lower"
	"github.com/sstriker/buildstream-bazel/converter/internal/ninja"
	"github.com/sstriker/buildstream-bazel/converter/ir"
)

const helloWorldFixture = "../../testdata/fileapi/hello-world"

func TestToIR_HelloWorld(t *testing.T) {
	r, err := fileapi.Load(helloWorldFixture)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	// The codemodel records an absolute source-root path that may not exist
	// at test time (the fixture was recorded on a different machine). Override
	// to the on-disk hello-world sample so header discovery works.
	src, err := filepath.Abs("../../testdata/sample-projects/hello-world")
	if err != nil {
		t.Fatalf("abs: %v", err)
	}

	pkg, err := lower.ToIR(r, nil, lower.Options{HostSourceRoot: src})
	if err != nil {
		t.Fatalf("ToIR: %v", err)
	}

	if pkg.Name != "hello" {
		t.Errorf("Package.Name = %q, want hello", pkg.Name)
	}
	if got := len(pkg.Targets); got != 1 {
		t.Fatalf("Targets = %d, want 1", got)
	}

	tgt := pkg.Targets[0]
	if tgt.Name != "hello" {
		t.Errorf("Target.Name = %q, want hello", tgt.Name)
	}
	if tgt.Kind != ir.KindCCLibrary {
		t.Errorf("Target.Kind = %v, want KindCCLibrary", tgt.Kind)
	}
	if !tgt.Linkstatic {
		t.Errorf("Linkstatic = false; STATIC_LIBRARY should set linkstatic=True")
	}
	if want := []string{"hello.c"}; !equal(tgt.Srcs, want) {
		t.Errorf("Srcs = %v, want %v", tgt.Srcs, want)
	}
	if want := []string{"include/hello.h"}; !equal(tgt.Hdrs, want) {
		t.Errorf("Hdrs = %v, want %v", tgt.Hdrs, want)
	}
	if want := []string{"include"}; !equal(tgt.Includes, want) {
		t.Errorf("Includes = %v, want %v", tgt.Includes, want)
	}
	// Release flags from CMAKE_C_FLAGS_RELEASE are "-O3 -DNDEBUG"; we split
	// them into copts=["-O3"] and defines=["NDEBUG"].
	if !contains(tgt.Copts, "-O3") {
		t.Errorf("Copts = %v, want to contain -O3", tgt.Copts)
	}
	if !contains(tgt.Defines, "NDEBUG") {
		t.Errorf("Defines = %v, want to contain NDEBUG", tgt.Defines)
	}
	for _, c := range tgt.Copts {
		if c == "-DNDEBUG" {
			t.Errorf("Copts contains -DNDEBUG; should be lifted to Defines")
		}
	}
	if tgt.InstallDest != "lib" {
		t.Errorf("InstallDest = %q, want lib", tgt.InstallDest)
	}
	if want := []string{"//visibility:public"}; !equal(tgt.Visibility, want) {
		t.Errorf("Visibility = %v, want %v", tgt.Visibility, want)
	}
}

// TestToIR_ElidesAbsoluteBuildDirSource covers the
// header-only-shim pattern where a project writes a placeholder
// source under ${CMAKE_BINARY_DIR} (e.g. via `file(WRITE
// ${CMAKE_BINARY_DIR}/dummy.cpp "")`) and adds it to an
// otherwise-header-only library. cmake's codemodel records the
// absolute build-dir path verbatim but doesn't flag it as
// IsGenerated (file(WRITE) outputs aren't marked generated
// unless the project explicitly sets the property). Without
// filtering, the absolute /tmp/<convert-element-build>/...
// path lands in irt.Srcs and the rendered BUILD.bazel refers
// to a file that's gone before Bazel ever runs the rule.
//
// Expected behaviour: the build-dir-rooted source is dropped
// from srcs and the target picks up the audit tag
// `cmake-elided-build-dir-source` so operators can query for
// affected targets.
func TestToIR_ElidesAbsoluteBuildDirSource(t *testing.T) {
	r := &fileapi.Reply{
		Codemodel: fileapi.Codemodel{
			Paths: fileapi.CodemodelPaths{
				Source: "/src",
				Build:  "/tmp/convert-element-build-abc123",
			},
			Configurations: []fileapi.Configuration{{
				Name:    "Release",
				Targets: []fileapi.ConfigTargetRef{{Name: "foo", Id: "foo::@1"}},
			}},
		},
		Targets: map[string]fileapi.Target{
			"foo::@1": {
				Name: "foo",
				Type: "STATIC_LIBRARY",
				Sources: []fileapi.TargetSource{
					{Path: "real.c", CompileGroupIndex: 0},
					{Path: "/tmp/convert-element-build-abc123/dummy.cpp", CompileGroupIndex: 0},
				},
				CompileGroups: []fileapi.CompileGroup{{
					Language:      "C",
					SourceIndexes: []int{0, 1},
				}},
			},
		},
	}
	pkg, err := lower.ToIR(r, nil, lower.Options{HostSourceRoot: "/src"})
	if err != nil {
		t.Fatalf("ToIR: %v", err)
	}
	if len(pkg.Targets) != 1 {
		t.Fatalf("Targets = %d, want 1", len(pkg.Targets))
	}
	tgt := pkg.Targets[0]
	for _, s := range tgt.Srcs {
		if filepath.IsAbs(s) {
			t.Errorf("Srcs contains absolute path %q; expected build-dir source to be dropped", s)
		}
		if strings.HasPrefix(s, "/tmp/") || strings.Contains(s, "convert-element-build-") {
			t.Errorf("Srcs leaked the build-dir tmp path: %q", s)
		}
	}
	if !contains(tgt.Srcs, "real.c") {
		t.Errorf("Srcs = %v, want to contain real.c (the non-build-dir source)", tgt.Srcs)
	}
	if !contains(tgt.Tags, "cmake-elided-build-dir-source") {
		t.Errorf("Tags = %v, want to contain cmake-elided-build-dir-source", tgt.Tags)
	}
}

// TestToIR_NoElidedTagWhenAllSourcesClean is a no-regression
// guard: the elision tag only fires when at least one
// build-dir-rooted source was actually dropped. Clean targets
// keep their existing tag set.
func TestToIR_NoElidedTagWhenAllSourcesClean(t *testing.T) {
	r := &fileapi.Reply{
		Codemodel: fileapi.Codemodel{
			Paths: fileapi.CodemodelPaths{
				Source: "/src",
				Build:  "/tmp/convert-element-build-abc123",
			},
			Configurations: []fileapi.Configuration{{
				Name:    "Release",
				Targets: []fileapi.ConfigTargetRef{{Name: "foo", Id: "foo::@1"}},
			}},
		},
		Targets: map[string]fileapi.Target{
			"foo::@1": {
				Name: "foo",
				Type: "STATIC_LIBRARY",
				Sources: []fileapi.TargetSource{
					{Path: "real.c", CompileGroupIndex: 0},
				},
				CompileGroups: []fileapi.CompileGroup{{
					Language:      "C",
					SourceIndexes: []int{0},
				}},
			},
		},
	}
	pkg, err := lower.ToIR(r, nil, lower.Options{HostSourceRoot: "/src"})
	if err != nil {
		t.Fatalf("ToIR: %v", err)
	}
	tgt := pkg.Targets[0]
	if contains(tgt.Tags, "cmake-elided-build-dir-source") {
		t.Errorf("Tags = %v, did not expect cmake-elided-build-dir-source", tgt.Tags)
	}
}

// TestToIR_ElidesMissingOnDiskSource covers #209: cmake's
// target model can list source files that aren't actually in
// the source tree the converter sees (e.g. the producer's
// tarball pruned the tests/playground subtree but kept the
// add_executable(test_x tests/playground/x.cpp) entry). cmake
// configure succeeds because add_executable doesn't validate
// file existence; Bazel would then fail at build time with
// "missing input file". The lowering should skip the missing
// source and tag the surviving target so audit queries find
// affected libraries.
func TestToIR_ElidesMissingOnDiskSource(t *testing.T) {
	hostSrc := t.TempDir()
	// Only real.c exists on disk; the tests/playground/x.cpp
	// path cmake's target model names is absent (mirrors a
	// pruned-tarball scenario).
	if err := os.WriteFile(filepath.Join(hostSrc, "real.c"), []byte(""), 0o644); err != nil {
		t.Fatalf("write real.c: %v", err)
	}

	r := &fileapi.Reply{
		Codemodel: fileapi.Codemodel{
			Paths: fileapi.CodemodelPaths{
				Source: hostSrc,
				Build:  "/tmp/convert-element-build-abc123",
			},
			Configurations: []fileapi.Configuration{{
				Name:    "Release",
				Targets: []fileapi.ConfigTargetRef{{Name: "foo", Id: "foo::@1"}},
			}},
		},
		Targets: map[string]fileapi.Target{
			"foo::@1": {
				Name: "foo",
				Type: "STATIC_LIBRARY",
				Sources: []fileapi.TargetSource{
					{Path: "real.c", CompileGroupIndex: 0},
					{Path: "tests/playground/x.cpp", CompileGroupIndex: 0},
				},
				CompileGroups: []fileapi.CompileGroup{{
					Language:      "C",
					SourceIndexes: []int{0, 1},
				}},
			},
		},
	}
	pkg, err := lower.ToIR(r, nil, lower.Options{HostSourceRoot: hostSrc})
	if err != nil {
		t.Fatalf("ToIR: %v", err)
	}
	if len(pkg.Targets) != 1 {
		t.Fatalf("Targets = %d, want 1", len(pkg.Targets))
	}
	tgt := pkg.Targets[0]
	if contains(tgt.Srcs, "tests/playground/x.cpp") {
		t.Errorf("Srcs = %v, missing-on-disk source leaked through", tgt.Srcs)
	}
	if !contains(tgt.Srcs, "real.c") {
		t.Errorf("Srcs = %v, want to contain real.c (the on-disk source)", tgt.Srcs)
	}
	if !contains(tgt.Tags, "cmake-elided-missing-source") {
		t.Errorf("Tags = %v, want to contain cmake-elided-missing-source", tgt.Tags)
	}
}

// TestToIR_NoMissingTagWhenAllSourcesPresent ensures the new
// elision tag only fires when at least one source was actually
// missing — targets whose sources are all on disk keep their
// existing tag set unchanged.
func TestToIR_NoMissingTagWhenAllSourcesPresent(t *testing.T) {
	hostSrc := t.TempDir()
	if err := os.WriteFile(filepath.Join(hostSrc, "real.c"), []byte(""), 0o644); err != nil {
		t.Fatalf("write real.c: %v", err)
	}

	r := &fileapi.Reply{
		Codemodel: fileapi.Codemodel{
			Paths: fileapi.CodemodelPaths{
				Source: hostSrc,
				Build:  "/tmp/convert-element-build-abc123",
			},
			Configurations: []fileapi.Configuration{{
				Name:    "Release",
				Targets: []fileapi.ConfigTargetRef{{Name: "foo", Id: "foo::@1"}},
			}},
		},
		Targets: map[string]fileapi.Target{
			"foo::@1": {
				Name: "foo",
				Type: "STATIC_LIBRARY",
				Sources: []fileapi.TargetSource{
					{Path: "real.c", CompileGroupIndex: 0},
				},
				CompileGroups: []fileapi.CompileGroup{{
					Language:      "C",
					SourceIndexes: []int{0},
				}},
			},
		},
	}
	pkg, err := lower.ToIR(r, nil, lower.Options{HostSourceRoot: hostSrc})
	if err != nil {
		t.Fatalf("ToIR: %v", err)
	}
	tgt := pkg.Targets[0]
	if contains(tgt.Tags, "cmake-elided-missing-source") {
		t.Errorf("Tags = %v, did not expect cmake-elided-missing-source", tgt.Tags)
	}
}

// TestToIR_MissingSourceCheckSkippedWithoutHostRoot ensures the
// validation is gated on HostSourceRoot being known: pure-offline
// callers (replay-against-fixture) that don't pass a host root
// should keep the pre-#209 behaviour of trusting the codemodel,
// since they can't resolve the path to a checkable location.
func TestToIR_MissingSourceCheckSkippedWithoutHostRoot(t *testing.T) {
	r := &fileapi.Reply{
		Codemodel: fileapi.Codemodel{
			Paths: fileapi.CodemodelPaths{
				Source: "/src",
				Build:  "/tmp/convert-element-build-abc123",
			},
			Configurations: []fileapi.Configuration{{
				Name:    "Release",
				Targets: []fileapi.ConfigTargetRef{{Name: "foo", Id: "foo::@1"}},
			}},
		},
		Targets: map[string]fileapi.Target{
			"foo::@1": {
				Name: "foo",
				Type: "STATIC_LIBRARY",
				Sources: []fileapi.TargetSource{
					{Path: "tests/playground/x.cpp", CompileGroupIndex: 0},
				},
				CompileGroups: []fileapi.CompileGroup{{
					Language:      "C",
					SourceIndexes: []int{0},
				}},
			},
		},
	}
	pkg, err := lower.ToIR(r, nil, lower.Options{}) // no HostSourceRoot
	if err != nil {
		t.Fatalf("ToIR: %v", err)
	}
	tgt := pkg.Targets[0]
	if !contains(tgt.Srcs, "tests/playground/x.cpp") {
		t.Errorf("Srcs = %v, want pass-through when no HostSourceRoot is set", tgt.Srcs)
	}
	if contains(tgt.Tags, "cmake-elided-missing-source") {
		t.Errorf("Tags = %v, did not expect cmake-elided-missing-source without HostSourceRoot", tgt.Tags)
	}
}

// TestToIR_ElidesCompilerObjectArtifact covers #206: cmake's
// target model can list a .o file as a generated source — e.g.
// for unity builds where the compiler-produced ub_*.cpp.o is
// surfaced as a source — and the producing ninja rule is a
// compile rule (CXX_COMPILER__<target>_*), not CUSTOM_COMMAND.
// Before #206 this surfaced as a Tier-1 unsupported-custom-command
// refusal; the file is a compile artifact already captured by the
// target's own compile group, so silently skipping with an audit
// tag is the right disposition.
func TestToIR_ElidesCompilerObjectArtifact(t *testing.T) {
	const buildDir = "/tmp/convert-element-build-abc123"

	g := &ninja.Graph{
		Vars:  map[string]string{},
		Rules: map[string]*ninja.Rule{},
		Pools: map[string]*ninja.Pool{},
	}
	g.Rules["CXX_COMPILER__foo_unscanned_Release"] = &ninja.Rule{
		Name: "CXX_COMPILER__foo_unscanned_Release",
		Bindings: map[string]string{
			"command": "/usr/bin/clang++ -c $in -o $out",
		},
		BindingOrder: []string{"command"},
	}
	g.Builds = []*ninja.Build{{
		Outputs: []string{"CMakeFiles/legacy_alias.dir/ub_file.cpp.o"},
		Rule:    "CXX_COMPILER__foo_unscanned_Release",
	}}

	r := &fileapi.Reply{
		Codemodel: fileapi.Codemodel{
			Paths: fileapi.CodemodelPaths{
				Source: "/src",
				Build:  buildDir,
			},
			Configurations: []fileapi.Configuration{{
				Name:    "Release",
				Targets: []fileapi.ConfigTargetRef{{Name: "foo", Id: "foo::@1"}},
			}},
		},
		Targets: map[string]fileapi.Target{
			"foo::@1": {
				Name: "foo",
				Type: "STATIC_LIBRARY",
				Sources: []fileapi.TargetSource{
					{Path: "real.c", CompileGroupIndex: 0},
					// Generated .o under a .dir whose name doesn't
					// match any known target — isTargetObjectsRef
					// would miss, so without the #206 fix this would
					// fall into recoverGenrule and refuse with
					// unsupported-custom-command.
					{Path: buildDir + "/CMakeFiles/legacy_alias.dir/ub_file.cpp.o", IsGenerated: true, CompileGroupIndex: 0},
				},
				CompileGroups: []fileapi.CompileGroup{{
					Language:      "C",
					SourceIndexes: []int{0, 1},
				}},
			},
		},
	}

	pkg, err := lower.ToIR(r, g, lower.Options{})
	if err != nil {
		t.Fatalf("ToIR: %v", err)
	}
	if len(pkg.Targets) != 1 {
		t.Fatalf("Targets = %d, want 1", len(pkg.Targets))
	}
	tgt := pkg.Targets[0]
	for _, s := range tgt.Srcs {
		if strings.HasSuffix(s, "ub_file.cpp.o") {
			t.Errorf("Srcs = %v, compiler artifact leaked through", tgt.Srcs)
		}
	}
	if !contains(tgt.Tags, "cmake-elided-compiler-artifact") {
		t.Errorf("Tags = %v, want to contain cmake-elided-compiler-artifact", tgt.Tags)
	}
}

// TestToIR_ElidesCompilerObjectArtifact_Subdirectory covers #212:
// the same compiler-artifact elision as the previous test but for
// the multi-directory cmake layout aws-lc and similar projects use.
// When an OBJECT library is defined in a subdirectory's
// CMakeLists.txt, cmake's ninja generator writes its outputs under
// `<subdir>/CMakeFiles/<target>.dir/...`, not the build-root
// CMakeFiles/. The pre-#212 isCompilerObjectArtifact gated on
// `HasPrefix(rel, "CMakeFiles/")` which silently missed those
// paths; isTargetObjectsRef had the same bug. Both now route
// through findCMakeFilesDir which matches the segment-aligned
// shape from any depth.
func TestToIR_ElidesCompilerObjectArtifact_Subdirectory(t *testing.T) {
	const buildDir = "/tmp/convert-element-build-abc123"

	g := &ninja.Graph{
		Vars:  map[string]string{},
		Rules: map[string]*ninja.Rule{},
		Pools: map[string]*ninja.Pool{},
	}
	g.Rules["C_COMPILER__crypto_objects_unscanned_Release"] = &ninja.Rule{
		Name: "C_COMPILER__crypto_objects_unscanned_Release",
		Bindings: map[string]string{
			"command": "/usr/bin/clang -c $in -o $out",
		},
		BindingOrder: []string{"command"},
	}
	// Matches the exact path shape from #212's aws-lc reproduction.
	const subPath = "crypto/CMakeFiles/crypto_objects.dir/asn1/a_bitstr.c.o"
	g.Builds = []*ninja.Build{{
		Outputs: []string{subPath},
		Rule:    "C_COMPILER__crypto_objects_unscanned_Release",
	}}

	r := &fileapi.Reply{
		Codemodel: fileapi.Codemodel{
			Paths: fileapi.CodemodelPaths{
				Source: "/src",
				Build:  buildDir,
			},
			Configurations: []fileapi.Configuration{{
				Name:    "Release",
				Targets: []fileapi.ConfigTargetRef{{Name: "crypto", Id: "crypto::@1"}},
			}},
		},
		Targets: map[string]fileapi.Target{
			"crypto::@1": {
				Name: "crypto",
				Type: "STATIC_LIBRARY",
				Sources: []fileapi.TargetSource{
					{Path: "real.c", CompileGroupIndex: 0},
					{Path: buildDir + "/" + subPath, IsGenerated: true, CompileGroupIndex: 0},
				},
				CompileGroups: []fileapi.CompileGroup{{
					Language:      "C",
					SourceIndexes: []int{0, 1},
				}},
			},
		},
	}

	pkg, err := lower.ToIR(r, g, lower.Options{})
	if err != nil {
		t.Fatalf("ToIR returned error (the #212 regression): %v", err)
	}
	if len(pkg.Targets) != 1 {
		t.Fatalf("Targets = %d, want 1", len(pkg.Targets))
	}
	tgt := pkg.Targets[0]
	for _, s := range tgt.Srcs {
		if strings.Contains(s, "a_bitstr.c.o") {
			t.Errorf("Srcs = %v, subdirectory compiler artifact leaked through", tgt.Srcs)
		}
	}
	if !contains(tgt.Tags, "cmake-elided-compiler-artifact") {
		t.Errorf("Tags = %v, want to contain cmake-elided-compiler-artifact", tgt.Tags)
	}
}

// TestToIR_TargetObjectsRef_Subdirectory covers the other #212
// arm: isTargetObjectsRef must also recognise the subdirectory
// `<sub>/CMakeFiles/<t>.dir/...*.o` shape so $<TARGET_OBJECTS:t>
// references where t is defined in a subdir get silently skipped
// (the consumer's deps already carry the OBJECT_LIBRARY edge with
// alwayslink=True). Pre-#212 the HasPrefix("CMakeFiles/") guard
// missed these and they fell through to isCompilerObjectArtifact —
// which would have also missed without its parallel fix, then
// recoverGenrule would refuse with unsupported-custom-command.
func TestToIR_TargetObjectsRef_Subdirectory(t *testing.T) {
	const buildDir = "/tmp/convert-element-build-abc123"

	// No ninja graph needed: isTargetObjectsRef should match the
	// path on the idToName lookup alone, BEFORE the function
	// hands off to recoverGenrule / isCompilerObjectArtifact.
	r := &fileapi.Reply{
		Codemodel: fileapi.Codemodel{
			Paths: fileapi.CodemodelPaths{
				Source: "/src",
				Build:  buildDir,
			},
			Configurations: []fileapi.Configuration{{
				Name: "Release",
				Targets: []fileapi.ConfigTargetRef{
					{Name: "consumer", Id: "consumer::@1"},
					{Name: "obj_lib", Id: "obj_lib::@2"},
				},
			}},
		},
		Targets: map[string]fileapi.Target{
			"consumer::@1": {
				Name: "consumer",
				Type: "STATIC_LIBRARY",
				Sources: []fileapi.TargetSource{
					{Path: "consumer.c", CompileGroupIndex: 0},
					// $<TARGET_OBJECTS:obj_lib> in a subdirectory
					// codebase shape.
					{Path: buildDir + "/sub/CMakeFiles/obj_lib.dir/source.c.o", IsGenerated: true, CompileGroupIndex: 0},
				},
				CompileGroups: []fileapi.CompileGroup{{
					Language:      "C",
					SourceIndexes: []int{0, 1},
				}},
			},
			"obj_lib::@2": {
				Name: "obj_lib",
				Type: "OBJECT_LIBRARY",
				Sources: []fileapi.TargetSource{
					{Path: "sub/source.c", CompileGroupIndex: 0},
				},
				CompileGroups: []fileapi.CompileGroup{{
					Language:      "C",
					SourceIndexes: []int{0},
				}},
			},
		},
	}

	pkg, err := lower.ToIR(r, nil, lower.Options{})
	if err != nil {
		t.Fatalf("ToIR returned error (the #212 regression for TARGET_OBJECTS): %v", err)
	}
	var consumer *ir.Target
	for i := range pkg.Targets {
		if pkg.Targets[i].Name == "consumer" {
			consumer = &pkg.Targets[i]
			break
		}
	}
	if consumer == nil {
		t.Fatalf("Targets = %+v, missing consumer", pkg.Targets)
	}
	for _, s := range consumer.Srcs {
		if strings.Contains(s, "source.c.o") {
			t.Errorf("consumer.Srcs = %v, subdirectory TARGET_OBJECTS ref leaked through", consumer.Srcs)
		}
	}
}

// TestToIR_PlatformConditionalSrcs_Partitioned covers #217 Tier 1.
// When the trace shows a source was attached to a target inside a
// recognized `if(CMAKE_SYSTEM_NAME STREQUAL "<Name>")` block,
// lower must move that source from the flat irt.Srcs to
// irt.PerPlatform["srcs"][selectKey]. Sources outside the
// conditional stay in flat srcs — projects without platform
// conditionals are byte-stable.
func TestToIR_PlatformConditionalSrcs_Partitioned(t *testing.T) {
	r := &fileapi.Reply{
		Codemodel: fileapi.Codemodel{
			Paths: fileapi.CodemodelPaths{
				Source: "/src",
				Build:  "/tmp/convert-element-build-abc123",
			},
			Configurations: []fileapi.Configuration{{
				Name:    "Release",
				Targets: []fileapi.ConfigTargetRef{{Name: "foo", Id: "foo::@1"}},
			}},
		},
		Targets: map[string]fileapi.Target{
			"foo::@1": {
				Name: "foo",
				Type: "STATIC_LIBRARY",
				Sources: []fileapi.TargetSource{
					{Path: "shared.c", CompileGroupIndex: 0},
					{Path: "linux.c", CompileGroupIndex: 0},
				},
				CompileGroups: []fileapi.CompileGroup{{
					Language:      "C",
					SourceIndexes: []int{0, 1},
				}},
			},
		},
	}
	// Trace pinning: linux.c was added inside an
	// `if(CMAKE_SYSTEM_NAME STREQUAL "Linux")` block. shared.c
	// has no enclosing if; it stays flat.
	trace := `
{"args":["foo","PRIVATE","shared.c"],"cmd":"target_sources","file":"/src/CMakeLists.txt","line":3}
{"args":["CMAKE_SYSTEM_NAME","STREQUAL","Linux"],"cmd":"if","file":"/src/CMakeLists.txt","line":5}
{"args":["foo","PRIVATE","linux.c"],"cmd":"target_sources","file":"/src/CMakeLists.txt","line":6}
{"args":[],"cmd":"endif","file":"/src/CMakeLists.txt","line":7}
`
	pkg, err := lower.ToIR(r, nil, lower.Options{
		HostSourceRoot: "/src",
		TraceRaw:       []byte(trace),
	})
	if err != nil {
		t.Fatalf("ToIR: %v", err)
	}
	if len(pkg.Targets) != 1 {
		t.Fatalf("Targets = %d, want 1", len(pkg.Targets))
	}
	tgt := pkg.Targets[0]
	if !equal(tgt.Srcs, []string{"shared.c"}) {
		t.Errorf("flat Srcs = %v, want [shared.c] (linux.c should be partitioned out)", tgt.Srcs)
	}
	arm, ok := tgt.PerPlatform["srcs"]["@platforms//os:linux"]
	if !ok {
		t.Fatalf("PerPlatform[srcs][linux] missing; got %+v", tgt.PerPlatform)
	}
	if !equal(arm, []string{"linux.c"}) {
		t.Errorf("PerPlatform[srcs][linux] = %v, want [linux.c]", arm)
	}
}

// TestToIR_PlatformConditionalSrcs_NoTraceByteStable pins the
// no-regression guard: a project without trace data leaves
// PerPlatform empty and emits the same flat-srcs shape as
// before #217. Byte-stable single-platform contract holds for
// non-conditional projects.
func TestToIR_PlatformConditionalSrcs_NoTraceByteStable(t *testing.T) {
	r := &fileapi.Reply{
		Codemodel: fileapi.Codemodel{
			Paths: fileapi.CodemodelPaths{
				Source: "/src",
				Build:  "/tmp/convert-element-build-abc123",
			},
			Configurations: []fileapi.Configuration{{
				Name:    "Release",
				Targets: []fileapi.ConfigTargetRef{{Name: "foo", Id: "foo::@1"}},
			}},
		},
		Targets: map[string]fileapi.Target{
			"foo::@1": {
				Name: "foo",
				Type: "STATIC_LIBRARY",
				Sources: []fileapi.TargetSource{
					{Path: "linux.c", CompileGroupIndex: 0},
				},
				CompileGroups: []fileapi.CompileGroup{{
					Language:      "C",
					SourceIndexes: []int{0},
				}},
			},
		},
	}
	pkg, err := lower.ToIR(r, nil, lower.Options{HostSourceRoot: "/src"})
	if err != nil {
		t.Fatalf("ToIR: %v", err)
	}
	tgt := pkg.Targets[0]
	if !equal(tgt.Srcs, []string{"linux.c"}) {
		t.Errorf("Srcs = %v, want [linux.c] (no trace → no partition)", tgt.Srcs)
	}
	if len(tgt.PerPlatform) != 0 {
		t.Errorf("PerPlatform = %+v, want empty (no trace → no select arms)", tgt.PerPlatform)
	}
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func contains(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}
