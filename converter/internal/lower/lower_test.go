package lower_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/internal/failure"
	"github.com/sstriker/buildstream-bazel/converter/internal/fileapi"
	"github.com/sstriker/buildstream-bazel/converter/internal/lower"
	"github.com/sstriker/buildstream-bazel/converter/internal/ninja"
	"github.com/sstriker/buildstream-bazel/converter/internal/rejection"
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
	// Four targets:
	//   - the cc_library (the producer rule for `hello`),
	//   - the install_directory__include pkg_files mirroring
	//     install(DIRECTORY include ...) (Phase 1 slice 1b),
	//   - the cmake_config_bundle filegroup synthesizing the
	//     install(EXPORT) bundle script (Phase 6 declarative
	//     projection — codemodel-only EmitInputs slice),
	//   - the hello_import cc_import the export's per-target
	//     projection emits for cross-element find_package
	//     consumers (Phase 6).
	if got := len(pkg.Targets); got != 4 {
		t.Fatalf("Targets = %d, want 4 (cc_library + install_directory + cmake_config_bundle + hello_import)", got)
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

// TestToIR_RefusesBinaryWhenAllSourcesElided covers the
// all-sources-elided refusal: an EXECUTABLE whose only compiled
// source is a build-dir-rooted file with no recovered generator edge
// (no trace → OutToGenrule empty, so the re-wire can't fire) would
// otherwise emit a `cc_binary` with `srcs = []`, which Bazel rejects
// at build time. Rather than ship that silently-broken rule, the
// lowerer refuses: a typed *failure.Error in strict mode, a recorded
// rejection + skipped target in diagnostic mode.
func TestToIR_RefusesBinaryWhenAllSourcesElided(t *testing.T) {
	mk := func() *fileapi.Reply {
		return &fileapi.Reply{
			Codemodel: fileapi.Codemodel{
				Paths: fileapi.CodemodelPaths{
					Source: "/src",
					Build:  "/tmp/convert-element-build-abc123",
				},
				Configurations: []fileapi.Configuration{{
					Name:    "Release",
					Targets: []fileapi.ConfigTargetRef{{Name: "compile_x", Id: "compile_x::@1"}},
				}},
			},
			Targets: map[string]fileapi.Target{
				"compile_x::@1": {
					Name: "compile_x",
					Type: "EXECUTABLE",
					Sources: []fileapi.TargetSource{
						// Only source: a build-dir generated file with
						// no recovered generator edge.
						{Path: "/tmp/convert-element-build-abc123/compile_x.cpp", CompileGroupIndex: 0},
					},
					CompileGroups: []fileapi.CompileGroup{{
						Language:      "CXX",
						SourceIndexes: []int{0},
					}},
				},
			},
		}
	}

	// Strict mode: typed *failure.Error with the AllSourcesElided code.
	if _, err := lower.ToIR(mk(), nil, lower.Options{HostSourceRoot: "/src"}); err == nil {
		t.Fatalf("ToIR (strict) = nil error; want all-sources-elided refusal")
	} else {
		var fe *failure.Error
		if !errors.As(err, &fe) {
			t.Fatalf("err = %v (%T), want *failure.Error", err, err)
		}
		if fe.Code != failure.AllSourcesElided {
			t.Errorf("err.Code = %q, want %q", fe.Code, failure.AllSourcesElided)
		}
	}

	// Diagnostic mode: target skipped, rejection recorded.
	rc := rejection.New()
	pkg, err := lower.ToIR(mk(), nil, lower.Options{HostSourceRoot: "/src", Rejections: rc})
	if err != nil {
		t.Fatalf("ToIR (diagnostic) = %v; want collect-and-continue", err)
	}
	for _, tgt := range pkg.Targets {
		if tgt.Name == "compile_x" {
			t.Errorf("compile_x emitted despite all-sources-elided; Srcs=%v", tgt.Srcs)
		}
	}
	found := false
	for _, r := range rc.Items() {
		if r.Code == failure.AllSourcesElided {
			found = true
		}
	}
	if !found {
		t.Errorf("no all-sources-elided rejection recorded; got %+v", rc.Items())
	}
}

// TestToIR_RewiresElidedBuildDirSourceToGeneratorEdge covers the
// positive half of the all-sources-elided fix: a build-dir-rooted
// compiled source that DOES match a recovered generator output (here
// an execute_process `cmake -E touch` produces it; in the field it's
// eigen's configure_file-generated compile_<snippet>.cpp) is wired
// into srcs via that genrule edge instead of being elided. The
// resulting cc_binary references the package-relative generated source
// and is NOT refused, NOT tagged cmake-elided-build-dir-source.
func TestToIR_RewiresElidedBuildDirSourceToGeneratorEdge(t *testing.T) {
	r := &fileapi.Reply{
		Codemodel: fileapi.Codemodel{
			Paths: fileapi.CodemodelPaths{Source: "/src", Build: "/build"},
			Configurations: []fileapi.Configuration{{
				Name:    "Release",
				Targets: []fileapi.ConfigTargetRef{{Name: "compile_x", Id: "compile_x::@1"}},
			}},
		},
		Targets: map[string]fileapi.Target{
			"compile_x::@1": {
				Name: "compile_x",
				Type: "EXECUTABLE",
				Sources: []fileapi.TargetSource{
					// Only source: a build-dir file produced by the
					// recovered generator below.
					{Path: "/build/compile_x.cpp", CompileGroupIndex: 0},
				},
				CompileGroups: []fileapi.CompileGroup{{
					Language:      "CXX",
					SourceIndexes: []int{0},
				}},
			},
		},
	}
	traceRaw := []byte(
		`{"args":["COMMAND","cmake","-E","touch","/build/compile_x.cpp"],"cmd":"execute_process","file":"/src/CMakeLists.txt","line":3}` + "\n",
	)
	pkg, err := lower.ToIR(r, nil, lower.Options{
		HostSourceRoot: "/src",
		BuildDir:       "/build",
		TraceRaw:       traceRaw,
	})
	if err != nil {
		t.Fatalf("ToIR: %v", err)
	}
	var bin *ir.Target
	for i := range pkg.Targets {
		if pkg.Targets[i].Name == "compile_x" {
			bin = &pkg.Targets[i]
			break
		}
	}
	if bin == nil {
		t.Fatalf("compile_x not in pkg.Targets (should be re-wired, not refused)")
	}
	if !contains(bin.Srcs, "compile_x.cpp") {
		t.Errorf("Srcs = %v, want the generated source wired in as compile_x.cpp", bin.Srcs)
	}
	if contains(bin.Tags, "cmake-elided-build-dir-source") {
		t.Errorf("Tags = %v, source was elided instead of re-wired", bin.Tags)
	}
	if !contains(bin.Tags, "has-cmake-codegen") {
		t.Errorf("Tags = %v, want has-cmake-codegen (consumes a recovered generator output)", bin.Tags)
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

// TestToIR_PlatformConditionalSrcs_ArmSorted pins the
// byte-stability fix: multiple conditional sources for the same
// OS surface in sorted order under PerPlatform["srcs"][key], not
// trace-insertion order. emit's strList renders the arm contents
// verbatim, so the sort must happen at IR-build time.
func TestToIR_PlatformConditionalSrcs_ArmSorted(t *testing.T) {
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
					{Path: "z.c", CompileGroupIndex: 0},
					{Path: "a.c", CompileGroupIndex: 0},
					{Path: "m.c", CompileGroupIndex: 0},
				},
				CompileGroups: []fileapi.CompileGroup{{
					Language:      "C",
					SourceIndexes: []int{0, 1, 2},
				}},
			},
		},
	}
	// All three sources added inside the same conditional,
	// in non-sorted order. Result should be sorted.
	trace := `
{"args":["CMAKE_SYSTEM_NAME","STREQUAL","Linux"],"cmd":"if","file":"/src/CMakeLists.txt","line":5}
{"args":["foo","PRIVATE","z.c"],"cmd":"target_sources","file":"/src/CMakeLists.txt","line":6}
{"args":["foo","PRIVATE","a.c"],"cmd":"target_sources","file":"/src/CMakeLists.txt","line":7}
{"args":["foo","PRIVATE","m.c"],"cmd":"target_sources","file":"/src/CMakeLists.txt","line":8}
{"args":[],"cmd":"endif","file":"/src/CMakeLists.txt","line":9}
`
	pkg, err := lower.ToIR(r, nil, lower.Options{
		HostSourceRoot: "/src",
		TraceRaw:       []byte(trace),
	})
	if err != nil {
		t.Fatalf("ToIR: %v", err)
	}
	arm := pkg.Targets[0].PerPlatform["srcs"]["@platforms//os:linux"]
	want := []string{"a.c", "m.c", "z.c"}
	if !equal(arm, want) {
		t.Errorf("PerPlatform[srcs][linux] = %v, want %v (sorted)", arm, want)
	}
}

// TestToIR_PlatformConditionalSrcs_MultiLanguage pins that
// when splitCompileGroups moves srcs into per-language sub-
// libraries, the platform-conditional partition still applies
// to each sub-library. The wrapper's irt.Srcs is empty after
// the split (so the wrapper has no partition to do), but the
// per-language subs carry the actual sources and should
// surface platform-conditional ones under PerPlatform["srcs"].
func TestToIR_PlatformConditionalSrcs_MultiLanguage(t *testing.T) {
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
					{Path: "shared.cpp", CompileGroupIndex: 1},
					{Path: "linux.cpp", CompileGroupIndex: 1},
				},
				CompileGroups: []fileapi.CompileGroup{
					{Language: "C", SourceIndexes: []int{0, 1}},
					{Language: "CXX", SourceIndexes: []int{2, 3}},
				},
			},
		},
	}
	trace := `
{"args":["CMAKE_SYSTEM_NAME","STREQUAL","Linux"],"cmd":"if","file":"/src/CMakeLists.txt","line":5}
{"args":["foo","PRIVATE","linux.c"],"cmd":"target_sources","file":"/src/CMakeLists.txt","line":6}
{"args":["foo","PRIVATE","linux.cpp"],"cmd":"target_sources","file":"/src/CMakeLists.txt","line":7}
{"args":[],"cmd":"endif","file":"/src/CMakeLists.txt","line":8}
`
	pkg, err := lower.ToIR(r, nil, lower.Options{
		HostSourceRoot: "/src",
		TraceRaw:       []byte(trace),
	})
	if err != nil {
		t.Fatalf("ToIR: %v", err)
	}
	// Multi-language target produces a wrapper + per-language
	// sub-libraries (foo, foo_c, foo_cxx). Locate each sub
	// and check its PerPlatform["srcs"] arm.
	var fooC, fooCXX *ir.Target
	for i := range pkg.Targets {
		switch pkg.Targets[i].Name {
		case "foo_c":
			fooC = &pkg.Targets[i]
		case "foo_cxx":
			fooCXX = &pkg.Targets[i]
		}
	}
	if fooC == nil || fooCXX == nil {
		t.Fatalf("missing per-language subs; got targets %+v", pkg.Targets)
	}
	if !equal(fooC.Srcs, []string{"shared.c"}) {
		t.Errorf("foo_c.Srcs = %v, want [shared.c]", fooC.Srcs)
	}
	if arm := fooC.PerPlatform["srcs"]["@platforms//os:linux"]; !equal(arm, []string{"linux.c"}) {
		t.Errorf("foo_c.PerPlatform[srcs][linux] = %v, want [linux.c]", arm)
	}
	if !equal(fooCXX.Srcs, []string{"shared.cpp"}) {
		t.Errorf("foo_cxx.Srcs = %v, want [shared.cpp]", fooCXX.Srcs)
	}
	if arm := fooCXX.PerPlatform["srcs"]["@platforms//os:linux"]; !equal(arm, []string{"linux.cpp"}) {
		t.Errorf("foo_cxx.PerPlatform[srcs][linux] = %v, want [linux.cpp]", arm)
	}
}

// TestToIR_PlatformConditionalSrcs_Tier2Recovery covers #217
// Tier 2 (this PR): cmake configured for Linux, the trace
// records the entered LINUX arm of an if(LINUX)/elseif(WIN32)
// block, and Tier 2 recovers win.c from the skipped WIN32 arm
// by parsing the on-disk CMakeLists.txt. The combined Tier-1
// + Tier-2 partition must place linux.c under
// @platforms//os:linux AND win.c under @platforms//os:windows
// — both arms partitioned, so a bazel reconfigure for either
// platform finds its sources.
func TestToIR_PlatformConditionalSrcs_Tier2Recovery(t *testing.T) {
	// Stage a real on-disk CMakeLists.txt the Tier-2 driver
	// can read. The reply's Codemodel.Paths.Source points at
	// the same dir so the trace's `file` paths resolve
	// directly (no host-vs-trace remap needed).
	dir := t.TempDir()
	cmakeText := `add_library(app STATIC)
if(LINUX)
  target_sources(app PRIVATE linux.c)
elseif(WIN32)
  target_sources(app PRIVATE win.c)
endif()
`
	if err := os.WriteFile(filepath.Join(dir, "CMakeLists.txt"), []byte(cmakeText), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	// Stage linux.c on disk so lower's missing-source elide
	// doesn't drop it. win.c stays absent — Tier 2 places it
	// into PerPlatform unconditionally because it never went
	// through the elide check (Tier 2 sources don't live in
	// the codemodel's flat list).
	if err := os.WriteFile(filepath.Join(dir, "linux.c"), []byte("int main(){return 0;}\n"), 0o644); err != nil {
		t.Fatalf("WriteFile linux.c: %v", err)
	}

	r := &fileapi.Reply{
		Codemodel: fileapi.Codemodel{
			Paths: fileapi.CodemodelPaths{
				Source: dir,
				Build:  filepath.Join(dir, "build"),
			},
			Configurations: []fileapi.Configuration{{
				Name:    "Release",
				Targets: []fileapi.ConfigTargetRef{{Name: "app", Id: "app::@1"}},
			}},
		},
		Targets: map[string]fileapi.Target{
			"app::@1": {
				Name: "app",
				Type: "STATIC_LIBRARY",
				// Only linux.c is in the codemodel — cmake on a
				// Linux configure didn't see win.c.
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
	trace := []byte(`
{"args":["LINUX"],"cmd":"if","file":"` + dir + `/CMakeLists.txt","line":2}
{"args":["app","PRIVATE","linux.c"],"cmd":"target_sources","file":"` + dir + `/CMakeLists.txt","line":3}
{"args":["WIN32"],"cmd":"elseif","file":"` + dir + `/CMakeLists.txt","line":4}
{"args":[],"cmd":"endif","file":"` + dir + `/CMakeLists.txt","line":6}
`)
	pkg, err := lower.ToIR(r, nil, lower.Options{
		HostSourceRoot: dir,
		TraceRaw:       trace,
	})
	if err != nil {
		t.Fatalf("ToIR: %v", err)
	}
	if len(pkg.Targets) != 1 {
		t.Fatalf("Targets = %d, want 1", len(pkg.Targets))
	}
	tgt := pkg.Targets[0]
	// Tier 1 moves linux.c into PerPlatform[srcs][linux];
	// Tier 2 injects win.c into PerPlatform[srcs][windows].
	if arm := tgt.PerPlatform["srcs"]["@platforms//os:linux"]; !equal(arm, []string{"linux.c"}) {
		t.Errorf("PerPlatform[srcs][linux] = %v, want [linux.c]", arm)
	}
	if arm := tgt.PerPlatform["srcs"]["@platforms//os:windows"]; !equal(arm, []string{"win.c"}) {
		t.Errorf("PerPlatform[srcs][windows] = %v, want [win.c] (Tier 2 recovery)", arm)
	}
	if len(tgt.Srcs) != 0 {
		t.Errorf("Srcs = %v, want [] (linux.c partitioned out, win.c never in flat)", tgt.Srcs)
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

// TestToIR_RefusesOutOfTreeAbsoluteSource covers #221: a target
// that names a source via an absolute path outside both the
// project source tree and the build tree produces a Bazel label
// Bazel rejects at load time. Refusing at convert-time with
// failure.UnsupportedSourcePath surfaces the underlying cmake
// misuse before any broken BUILD lands.
func TestToIR_RefusesOutOfTreeAbsoluteSource(t *testing.T) {
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
					{Path: "/external/vendor/bar.c", CompileGroupIndex: 0},
				},
				CompileGroups: []fileapi.CompileGroup{{
					Language:      "C",
					SourceIndexes: []int{0},
				}},
			},
		},
	}
	_, err := lower.ToIR(r, nil, lower.Options{HostSourceRoot: "/src"})
	if err == nil {
		t.Fatal("ToIR succeeded; expected failure.UnsupportedSourcePath")
	}
	var fe *failure.Error
	if !errors.As(err, &fe) || fe.Code != failure.UnsupportedSourcePath {
		t.Errorf("got err=%v; want failure code %q", err, failure.UnsupportedSourcePath)
	}
}

// TestToIR_NormalizesInTreeAbsoluteSource pins that an
// absolute path under cmakeSrc is silently rewritten to the
// project-relative form (matching the documented contract on
// TargetSource.Path) rather than landing as an absolute label.
// Some cmake versions / call shapes record absolute paths
// even for in-tree files; the converter should be tolerant.
func TestToIR_NormalizesInTreeAbsoluteSource(t *testing.T) {
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
					{Path: "/src/sub/foo.c", CompileGroupIndex: 0},
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
	if !equal(pkg.Targets[0].Srcs, []string{"sub/foo.c"}) {
		t.Errorf("Srcs = %v, want [sub/foo.c] (in-tree absolute should normalize to package-relative)", pkg.Targets[0].Srcs)
	}
}

// TestToIR_StripsDotSlashSourcePrefix pins that a `./`-prefixed
// source path is silently normalized to the bare form so the
// emitted label is valid. cmake usually normalizes these but
// pathological inputs can survive; refusing would over-strict,
// stripping is a no-op fix.
func TestToIR_StripsDotSlashSourcePrefix(t *testing.T) {
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
					{Path: "./foo.c", CompileGroupIndex: 0},
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
	if !equal(pkg.Targets[0].Srcs, []string{"foo.c"}) {
		t.Errorf("Srcs = %v, want [foo.c] (./ prefix should be stripped)", pkg.Targets[0].Srcs)
	}
}

// TestToIR_RefusesDotDotEscapingSource pins that a source path
// containing `..` segments — which would either generate an
// out-of-package Bazel label or silently shift the file to a
// different package — is refused at convert-time.
func TestToIR_RefusesDotDotEscapingSource(t *testing.T) {
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
					{Path: "../sibling/foo.c", CompileGroupIndex: 0},
				},
				CompileGroups: []fileapi.CompileGroup{{
					Language:      "C",
					SourceIndexes: []int{0},
				}},
			},
		},
	}
	_, err := lower.ToIR(r, nil, lower.Options{HostSourceRoot: "/src"})
	if err == nil {
		t.Fatal("ToIR succeeded; expected failure.UnsupportedSourcePath")
	}
	var fe *failure.Error
	if !errors.As(err, &fe) || fe.Code != failure.UnsupportedSourcePath {
		t.Errorf("got err=%v; want failure code %q", err, failure.UnsupportedSourcePath)
	}
}

// TestToIR_ElidedPrefixInclude covers #219: when an include
// path resolves outside both cmakeSrc and cmakeBuild but lies
// under the synth-prefix tree (hostPrefix — a cross-element
// import via find_package), the current code silently drops it
// because the producing element provides headers through a
// cc_library dep, not an include path. Surfacing this as an
// audit tag lets operators see when cross-element include
// propagation is happening so they can verify the consuming
// target actually has a dep on the producer.
func TestToIR_ElidedPrefixInclude(t *testing.T) {
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
					{Path: "foo.c", CompileGroupIndex: 0},
				},
				CompileGroups: []fileapi.CompileGroup{{
					Language:      "C",
					SourceIndexes: []int{0},
					Includes: []fileapi.CompileInclude{
						{Path: "/synth/usr/include/external_dep"},
					},
				}},
			},
		},
	}
	pkg, err := lower.ToIR(r, nil, lower.Options{
		HostSourceRoot: "/src",
		HostPrefixDir:  "/synth",
	})
	if err != nil {
		t.Fatalf("ToIR: %v", err)
	}
	tgt := pkg.Targets[0]
	want := "cmake-elided-prefix-include=usr/include/external_dep"
	if !contains(tgt.Tags, want) {
		t.Errorf("Tags = %v, want to contain %q", tgt.Tags, want)
	}
	// The dropped path should not surface in Includes.
	for _, inc := range tgt.Includes {
		if strings.Contains(inc, "external_dep") {
			t.Errorf("Includes = %v, prefix-tree include leaked through", tgt.Includes)
		}
	}
}

// TestToIR_ElidedPrefixIncludeNoBasenameCollision pins the
// payload de-collision fix: two different paths under
// hostPrefix that share a trailing basename emit distinct tags
// (operators querying for one shouldn't silently match the
// other). The payload is the hostPrefix-relative form so each
// drop is uniquely identifiable.
func TestToIR_ElidedPrefixIncludeNoBasenameCollision(t *testing.T) {
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
					{Path: "foo.c", CompileGroupIndex: 0},
				},
				CompileGroups: []fileapi.CompileGroup{{
					Language:      "C",
					SourceIndexes: []int{0},
					Includes: []fileapi.CompileInclude{
						{Path: "/synth/usr/include/foo"},
						{Path: "/synth/local/include/foo"},
					},
				}},
			},
		},
	}
	pkg, err := lower.ToIR(r, nil, lower.Options{
		HostSourceRoot: "/src",
		HostPrefixDir:  "/synth",
	})
	if err != nil {
		t.Fatalf("ToIR: %v", err)
	}
	tgt := pkg.Targets[0]
	want := []string{
		"cmake-elided-prefix-include=usr/include/foo",
		"cmake-elided-prefix-include=local/include/foo",
	}
	for _, w := range want {
		if !contains(tgt.Tags, w) {
			t.Errorf("Tags = %v, want to contain %q (no basename-collision dedup)", tgt.Tags, w)
		}
	}
}

// TestToIR_NoElidedPrefixIncludeForSystemPath pins the inverse
// guard: drops of include paths under known system prefixes
// (/usr/include, /usr/local/include, etc.) — which the
// compiler's default search path already covers — should NOT
// fire the audit tag. Tagging every find_package-using project
// would create noise.
func TestToIR_NoElidedPrefixIncludeForSystemPath(t *testing.T) {
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
					{Path: "foo.c", CompileGroupIndex: 0},
				},
				CompileGroups: []fileapi.CompileGroup{{
					Language:      "C",
					SourceIndexes: []int{0},
					Includes: []fileapi.CompileInclude{
						{Path: "/usr/include"},
					},
				}},
			},
		},
	}
	// No HostPrefixDir set; the /usr/include drop should be
	// silent (no audit tag).
	pkg, err := lower.ToIR(r, nil, lower.Options{HostSourceRoot: "/src"})
	if err != nil {
		t.Fatalf("ToIR: %v", err)
	}
	tgt := pkg.Targets[0]
	for _, tag := range tgt.Tags {
		if strings.HasPrefix(tag, "cmake-elided-prefix-include=") {
			t.Errorf("Tags = %v, expected no cmake-elided-prefix-include tag for system path", tgt.Tags)
		}
	}
}

// TestToIR_DropsEmptyRelativeInclude pins the converter's
// drop of include entries whose source-relative form is "".
// `target_include_directories(${CMAKE_CURRENT_SOURCE_DIR})`
// resolves to the cmake source root itself; the relative-to-
// source form is the empty string. Bazel rejects
// `includes = [""]` ("resolves to the workspace root, which
// would allow this rule and all of its transitive dependents
// to include any file in your workspace"), so the converted
// BUILD.bazel won't even analyze. Same-package consumers
// already see this target's headers via hdrs+deps without
// an explicit include dir, so dropping the entry is the
// idiomatic shape.
func TestToIR_DropsEmptyRelativeInclude(t *testing.T) {
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
					{Path: "foo.c", CompileGroupIndex: 0},
				},
				CompileGroups: []fileapi.CompileGroup{{
					Language:      "C",
					SourceIndexes: []int{0},
					Includes: []fileapi.CompileInclude{
						// The cmake source root itself —
						// rel == "" after relativization.
						{Path: "/src"},
					},
				}},
			},
		},
	}
	pkg, err := lower.ToIR(r, nil, lower.Options{HostSourceRoot: "/src"})
	if err != nil {
		t.Fatalf("ToIR: %v", err)
	}
	tgt := pkg.Targets[0]
	for _, inc := range tgt.Includes {
		if inc == "" {
			t.Errorf("Includes = %v, contains empty entry; Bazel rejects includes=[\"\"]", tgt.Includes)
		}
	}
}

// TestToIR_ElidedLinkFragment covers #220: when an abs-path
// `libraries`-role link fragment escapes both the imports
// manifest (LookupLinkPath) AND the find_package attribution
// path, the current code silently dropped it. The new audit
// tag names the dropped library so operators can either add
// it to the imports manifest or recognize the unresolved dep.
func TestToIR_ElidedLinkFragment(t *testing.T) {
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
				// EXECUTABLE so the codemodel's Link block
				// is exercised (STATIC_LIBRARY targets don't
				// carry Link.CommandFragments in cmake's
				// codemodel — static archives don't link).
				Type: "EXECUTABLE",
				Sources: []fileapi.TargetSource{
					{Path: "main.c", CompileGroupIndex: 0},
				},
				CompileGroups: []fileapi.CompileGroup{{
					Language:      "C",
					SourceIndexes: []int{0},
				}},
				Link: &fileapi.TargetLink{
					Language: "C",
					CommandFragments: []fileapi.CommandFragment{
						{Fragment: "/opt/vendor/lib/libmystery.so", Role: "libraries"},
					},
				},
			},
		},
	}
	pkg, err := lower.ToIR(r, nil, lower.Options{HostSourceRoot: "/src"})
	if err != nil {
		t.Fatalf("ToIR: %v", err)
	}
	tgt := pkg.Targets[0]
	want := "cmake-elided-link-fragment=/opt/vendor/lib/libmystery.so"
	if !contains(tgt.Tags, want) {
		t.Errorf("Tags = %v, want to contain %q", tgt.Tags, want)
	}
}

// TestToIR_ElidedLinkFragmentNoBasenameCollision pins the
// Multi-arch system library paths
// (/usr/lib/x86_64-linux-gnu/libz.so vs
// /usr/lib/i386-linux-gnu/libz.so) now lift to a single
// `-lz` linkopt — the toolchain's library search path covers
// both. The dedup is intentional: both paths point at the
// same library name. (Pre-lift behaviour emitted separate
// elided tags to avoid the basename dedup hiding distinct
// vendored paths; the lift handles the system-lib case by
// routing them all through one linkopts entry instead.)
func TestToIR_SystemLibsLiftToLinkOpts(t *testing.T) {
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
				Type: "EXECUTABLE",
				Sources: []fileapi.TargetSource{
					{Path: "main.c", CompileGroupIndex: 0},
				},
				CompileGroups: []fileapi.CompileGroup{{
					Language:      "C",
					SourceIndexes: []int{0},
				}},
				Link: &fileapi.TargetLink{
					Language: "C",
					CommandFragments: []fileapi.CommandFragment{
						{Fragment: "/usr/lib/x86_64-linux-gnu/libz.so", Role: "libraries"},
						{Fragment: "/usr/lib/i386-linux-gnu/libz.so", Role: "libraries"},
					},
				},
			},
		},
	}
	pkg, err := lower.ToIR(r, nil, lower.Options{HostSourceRoot: "/src"})
	if err != nil {
		t.Fatalf("ToIR: %v", err)
	}
	tgt := pkg.Targets[0]
	if !contains(tgt.LinkOpts, "-lz") {
		t.Errorf("LinkOpts = %v, want to contain %q", tgt.LinkOpts, "-lz")
	}
	// Both paths collapse to one `-lz`; the dedup is
	// intentional now (they target the same library).
	count := 0
	for _, l := range tgt.LinkOpts {
		if l == "-lz" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("LinkOpts contained -lz %d times, want exactly 1; %v", count, tgt.LinkOpts)
	}
	// The cmake-elided-link-fragment tag must NOT fire for
	// system libs — that elision was the pre-lift behaviour.
	for _, tag := range tgt.Tags {
		if contains([]string{
			"cmake-elided-link-fragment=/usr/lib/x86_64-linux-gnu/libz.so",
			"cmake-elided-link-fragment=/usr/lib/i386-linux-gnu/libz.so",
		}, tag) {
			t.Errorf("system lib should NOT be elided post-lift; got tag %q", tag)
		}
	}
}

// TestToIR_NonSystemLibPathStaysElided pins the conservative
// half of the system-lib lift: a /opt/vendor/lib/... path (not
// in the toolchain's default library search) keeps the elided
// tag because the bare -l<name> wouldn't resolve.
func TestToIR_NonSystemLibPathStaysElided(t *testing.T) {
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
				Type: "EXECUTABLE",
				Sources: []fileapi.TargetSource{
					{Path: "main.c", CompileGroupIndex: 0},
				},
				CompileGroups: []fileapi.CompileGroup{{
					Language:      "C",
					SourceIndexes: []int{0},
				}},
				Link: &fileapi.TargetLink{
					Language: "C",
					CommandFragments: []fileapi.CommandFragment{
						{Fragment: "/opt/vendor/lib/libmystery.so", Role: "libraries"},
					},
				},
			},
		},
	}
	pkg, err := lower.ToIR(r, nil, lower.Options{HostSourceRoot: "/src"})
	if err != nil {
		t.Fatalf("ToIR: %v", err)
	}
	tgt := pkg.Targets[0]
	if !contains(tgt.Tags, "cmake-elided-link-fragment=/opt/vendor/lib/libmystery.so") {
		t.Errorf("Tags = %v, want to contain elided tag for non-system path", tgt.Tags)
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
