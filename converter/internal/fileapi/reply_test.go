package fileapi_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/internal/fileapi"
)

const helloWorldFixture = "../../testdata/fileapi/hello-world"

func TestLoad_HelloWorld(t *testing.T) {
	r, err := fileapi.Load(helloWorldFixture)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	t.Run("index", func(t *testing.T) {
		if r.Index.CMake.Generator.Name != "Ninja" {
			t.Errorf("generator = %q, want Ninja", r.Index.CMake.Generator.Name)
		}
		if r.Index.CMake.Generator.MultiConfig {
			t.Errorf("expected single-config generator")
		}
		if r.Index.CMake.Version.Major < 3 {
			t.Errorf("cmake version = %s, want >=3", r.Index.CMake.Version.String)
		}
		// Must reference all four kinds we care about.
		kinds := map[string]bool{}
		for _, o := range r.Index.Objects {
			kinds[o.Kind] = true
		}
		for _, k := range []string{"codemodel", "toolchains", "cmakeFiles", "cache"} {
			if !kinds[k] {
				t.Errorf("index missing kind %q", k)
			}
		}
	})

	t.Run("codemodel", func(t *testing.T) {
		if got := len(r.Codemodel.Configurations); got != 1 {
			t.Fatalf("configurations = %d, want 1 (Release)", got)
		}
		cfg := r.Codemodel.Configurations[0]
		if cfg.Name != "Release" {
			t.Errorf("config name = %q, want Release", cfg.Name)
		}
		if got := len(cfg.Targets); got != 1 {
			t.Fatalf("config targets = %d, want 1", got)
		}
		if cfg.Targets[0].Name != "hello" {
			t.Errorf("target name = %q, want hello", cfg.Targets[0].Name)
		}
		if !strings.HasPrefix(cfg.Targets[0].Id, "hello::@") {
			t.Errorf("target id = %q, want hello::@<hash>", cfg.Targets[0].Id)
		}
		if !strings.HasSuffix(filepath.ToSlash(r.Codemodel.Paths.Source), "/hello-world") {
			t.Errorf("codemodel source path = %q, want endswith /hello-world", r.Codemodel.Paths.Source)
		}
	})

	t.Run("target_hello", func(t *testing.T) {
		var helloId string
		for _, tref := range r.Codemodel.Configurations[0].Targets {
			if tref.Name == "hello" {
				helloId = tref.Id
			}
		}
		if helloId == "" {
			t.Fatal("hello target not found in codemodel")
		}
		tgt, ok := r.Targets[helloId]
		if !ok {
			t.Fatalf("Targets[%q] missing", helloId)
		}
		if tgt.Type != "STATIC_LIBRARY" {
			t.Errorf("type = %q, want STATIC_LIBRARY", tgt.Type)
		}
		if got := len(tgt.Sources); got != 1 {
			t.Fatalf("sources = %d, want 1", got)
		}
		if tgt.Sources[0].Path != "hello.c" {
			t.Errorf("source[0].path = %q, want hello.c", tgt.Sources[0].Path)
		}
		if tgt.Sources[0].IsGenerated {
			t.Errorf("source[0].isGenerated = true; hello.c is hand-written")
		}
		if got := len(tgt.CompileGroups); got != 1 {
			t.Fatalf("compileGroups = %d, want 1", got)
		}
		cg := tgt.CompileGroups[0]
		if cg.Language != "C" {
			t.Errorf("compileGroup language = %q, want C", cg.Language)
		}
		if got := len(cg.Includes); got != 1 {
			t.Fatalf("compileGroup includes = %d, want 1", got)
		}
		if !strings.HasSuffix(filepath.ToSlash(cg.Includes[0].Path), "/hello-world/include") {
			t.Errorf("include path = %q, want endswith /hello-world/include", cg.Includes[0].Path)
		}
		if got := len(tgt.Artifacts); got != 1 {
			t.Fatalf("artifacts = %d, want 1", got)
		}
		if tgt.Artifacts[0].Path != "libhello.a" {
			t.Errorf("artifact path = %q, want libhello.a", tgt.Artifacts[0].Path)
		}
		if tgt.Install == nil {
			t.Fatal("install block missing; CMakeLists declares install(TARGETS)")
		}
		if got := len(tgt.Install.Destinations); got != 1 {
			t.Fatalf("install destinations = %d, want 1", got)
		}
		if tgt.Install.Destinations[0].Path != "lib" {
			t.Errorf("install dest = %q, want lib", tgt.Install.Destinations[0].Path)
		}
	})

	t.Run("toolchains", func(t *testing.T) {
		if got := len(r.Toolchains.Toolchains); got < 1 {
			t.Fatalf("toolchains = %d, want >=1", got)
		}
		// Hello-world is C-only.
		var c *fileapi.ToolchainEnt
		for i := range r.Toolchains.Toolchains {
			if r.Toolchains.Toolchains[i].Language == "C" {
				c = &r.Toolchains.Toolchains[i]
			}
		}
		if c == nil {
			t.Fatal("no C toolchain entry")
		}
		if c.Compiler.Id == "" {
			t.Errorf("C compiler id empty")
		}
		if len(c.Compiler.Implicit.IncludeDirectories) == 0 {
			t.Errorf("C compiler implicit includes empty")
		}
	})

	t.Run("cmakeFiles", func(t *testing.T) {
		if len(r.CMakeFiles.Inputs) == 0 {
			t.Fatal("cmakeFiles.inputs empty")
		}
		// Top-level CMakeLists.txt must appear non-external.
		var seen bool
		for _, in := range r.CMakeFiles.Inputs {
			if in.Path == "CMakeLists.txt" && !in.IsExternal {
				seen = true
			}
		}
		if !seen {
			t.Errorf("expected non-external CMakeLists.txt in cmakeFiles.inputs")
		}
	})

	t.Run("cache_lookup", func(t *testing.T) {
		// CMAKE_PROJECT_NAME should always be set.
		e := r.Cache.Get("CMAKE_PROJECT_NAME")
		if e == nil {
			t.Fatal("CMAKE_PROJECT_NAME not in cache")
		}
		if e.Value != "hello" {
			t.Errorf("CMAKE_PROJECT_NAME = %q, want hello", e.Value)
		}
		if r.Cache.Get("DEFINITELY_NOT_A_REAL_CACHE_VAR") != nil {
			t.Errorf("missing entry returned non-nil")
		}
	})
}

func TestLoad_MissingDir(t *testing.T) {
	if _, err := fileapi.Load("/nonexistent/path/zzz"); err == nil {
		t.Errorf("expected error for missing reply dir")
	}
}

// TestLoad_SingleConfig_NoTargetsByConfig confirms single-config
// replies leave TargetsByConfig nil — the single Configurations
// entry's data lives in Targets and there's no per-config split
// to surface.
func TestLoad_SingleConfig_NoTargetsByConfig(t *testing.T) {
	r, err := fileapi.Load(helloWorldFixture)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if r.TargetsByConfig != nil {
		t.Errorf("TargetsByConfig should be nil for single-config reply; got %d entries", len(r.TargetsByConfig))
	}
}

// TestLoad_MultiConfig_TargetsByConfigPopulated stages a synthetic
// two-config reply (Release + Debug) for one target and checks
// that:
//   - TargetsByConfig carries entries for both configs
//   - Targets carries the primary (first-declared) config's data,
//     not the iteration-last one
func TestLoad_MultiConfig_TargetsByConfigPopulated(t *testing.T) {
	dir := t.TempDir()
	// Two configurations declared in the codemodel, with one target
	// per config. The target JSON content differs by config so the
	// test can tell which slot ended up where.
	writeFile(t, dir, "target-foo-Release.json", `{"name":"foo","id":"foo::@","type":"STATIC_LIBRARY","nameOnDisk":"libfoo-release.a","paths":{"source":"/src","build":"/b"},"backtraceGraph":{"commands":[],"files":[],"nodes":[]}}`)
	writeFile(t, dir, "target-foo-Debug.json", `{"name":"foo","id":"foo::@","type":"STATIC_LIBRARY","nameOnDisk":"libfoo-debug.a","paths":{"source":"/src","build":"/b"},"backtraceGraph":{"commands":[],"files":[],"nodes":[]}}`)
	writeFile(t, dir, "codemodel-v2.json", `{
		"kind":"codemodel","version":{"major":2,"minor":0},
		"paths":{"source":"/src","build":"/b"},
		"configurations":[
			{"name":"Release","targets":[{"name":"foo","id":"foo::@","jsonFile":"target-foo-Release.json"}],"directories":[],"projects":[]},
			{"name":"Debug","targets":[{"name":"foo","id":"foo::@","jsonFile":"target-foo-Debug.json"}],"directories":[],"projects":[]}
		]
	}`)
	writeFile(t, dir, "cache-v2.json", `{"kind":"cache","version":{"major":2,"minor":0},"entries":[]}`)
	writeFile(t, dir, "toolchains-v1.json", `{"kind":"toolchains","version":{"major":1,"minor":0},"toolchains":[]}`)
	writeFile(t, dir, "cmakeFiles-v1.json", `{"kind":"cmakeFiles","version":{"major":1,"minor":0},"paths":{"source":"/src","build":"/b"},"inputs":[]}`)
	writeFile(t, dir, "index-2026.json", `{
		"cmake":{"generator":{"name":"Ninja Multi-Config","multiConfig":true},"paths":{"cmake":"/usr/bin/cmake","ctest":"/usr/bin/ctest","cpack":"/usr/bin/cpack","root":"/usr"},"version":{"major":3,"minor":28,"patch":3,"string":"3.28.3","suffix":"","isDirty":false}},
		"objects":[
			{"kind":"codemodel","version":{"major":2,"minor":0},"jsonFile":"codemodel-v2.json"},
			{"kind":"cache","version":{"major":2,"minor":0},"jsonFile":"cache-v2.json"},
			{"kind":"toolchains","version":{"major":1,"minor":0},"jsonFile":"toolchains-v1.json"},
			{"kind":"cmakeFiles","version":{"major":1,"minor":0},"jsonFile":"cmakeFiles-v1.json"}
		]
	}`)

	r, err := fileapi.Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	// Targets carries the primary config (Release, first-declared).
	if got := r.Targets["foo::@"].NameOnDisk; got != "libfoo-release.a" {
		t.Errorf("Targets[foo].NameOnDisk = %q, want libfoo-release.a (primary)", got)
	}

	// TargetsByConfig carries both.
	if r.TargetsByConfig == nil {
		t.Fatalf("TargetsByConfig nil under multi-config")
	}
	byCfg, ok := r.TargetsByConfig["foo::@"]
	if !ok {
		t.Fatalf("TargetsByConfig missing entry for foo")
	}
	if got := byCfg["Release"].NameOnDisk; got != "libfoo-release.a" {
		t.Errorf("TargetsByConfig[foo][Release].NameOnDisk = %q", got)
	}
	if got := byCfg["Debug"].NameOnDisk; got != "libfoo-debug.a" {
		t.Errorf("TargetsByConfig[foo][Debug].NameOnDisk = %q", got)
	}
}
