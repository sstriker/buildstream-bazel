package probejson

import (
	"reflect"
	"strings"
	"testing"

	"github.com/sstriker/cmake-to-bazel/converter/internal/fileapi"
	"github.com/sstriker/cmake-to-bazel/converter/internal/toolchain"
)

func TestMarshalUnmarshal_RoundTrip(t *testing.T) {
	wantVariant := toolchain.Variant{
		Name: "asan",
		CacheVars: map[string]string{
			"CMAKE_BUILD_TYPE": "Debug",
			"CMAKE_C_FLAGS":    "-fsanitize=address",
		},
	}
	wantReply := &fileapi.Reply{
		Path: "/this/should/not/round-trip",
		Cache: fileapi.Cache{
			Entries: []fileapi.CacheEntry{
				{Name: "CMAKE_C_FLAGS", Value: "-fsanitize=address"},
				{Name: "CMAKE_BUILD_TYPE", Value: "Debug"},
			},
		},
		Targets:     map[string]fileapi.Target{},
		Directories: map[string]fileapi.Directory{},
	}

	body, err := Marshal(wantVariant, wantReply)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if !strings.Contains(string(body), `"schemaVersion": 1`) {
		t.Errorf("schemaVersion not embedded: %s", body)
	}

	gotVariant, gotReply, err := Unmarshal(body)
	if err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !reflect.DeepEqual(gotVariant, wantVariant) {
		t.Errorf("variant round-trip failed:\n got: %+v\nwant: %+v", gotVariant, wantVariant)
	}
	if gotReply.Path != "" {
		t.Errorf("Path should not round-trip; got %q", gotReply.Path)
	}
	if !reflect.DeepEqual(gotReply.Cache, wantReply.Cache) {
		t.Errorf("Cache round-trip failed:\n got: %+v\nwant: %+v", gotReply.Cache, wantReply.Cache)
	}
}

func TestUnmarshal_RejectsUnknownSchemaVersion(t *testing.T) {
	body := []byte(`{"schemaVersion": 999, "variant": {"name": "foo"}, "reply": {}}`)
	_, _, err := Unmarshal(body)
	if err == nil {
		t.Fatal("expected schemaVersion error; got nil")
	}
	if !strings.Contains(err.Error(), "schemaVersion") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestMarshal_NilReplyRejected(t *testing.T) {
	_, err := Marshal(toolchain.Variant{Name: "x"}, nil)
	if err == nil {
		t.Fatal("expected error for nil reply")
	}
}

// TestMarshal_ScrubsAbsolutePaths: cmake's File API records
// absolute source/build paths in Codemodel.Paths,
// CMakeFiles.Paths, every Target.Paths, every Directory.Paths,
// and Configuration.Directories[]. Leaving them in probe.json
// makes the artifact host-specific (sandbox roots, tmp suffixes)
// even after Cache filtering. Marshal must zero them all so the
// per-cell artifact compares byte-equal across hosts that ran
// the same cmake graph.
func TestMarshal_ScrubsAbsolutePaths(t *testing.T) {
	const recordedSrc = "/var/build/recorder-host/source"
	const recordedBuild = "/var/build/recorder-host/build"

	reply := &fileapi.Reply{
		Codemodel: fileapi.Codemodel{
			Paths: fileapi.CodemodelPaths{
				Source: recordedSrc,
				Build:  recordedBuild,
			},
			Configurations: []fileapi.Configuration{{
				Name: "Release",
				Directories: []fileapi.ConfigDirectory{{
					Source: recordedSrc,
					Build:  recordedBuild,
				}},
			}},
		},
		CMakeFiles: fileapi.CMakeFiles{
			Paths: fileapi.CMakeFilePaths{
				Source: recordedSrc,
				Build:  recordedBuild,
			},
		},
		Targets: map[string]fileapi.Target{
			"hello": {
				Name: "hello",
				Paths: fileapi.TargetPaths{
					Source: recordedSrc,
					Build:  recordedBuild,
				},
			},
		},
		Directories: map[string]fileapi.Directory{
			"directory-Release.json": func() fileapi.Directory {
				var d fileapi.Directory
				d.Paths.Source = recordedSrc
				d.Paths.Build = recordedBuild
				return d
			}(),
		},
	}

	body, err := Marshal(toolchain.Variant{Name: "baseline"}, reply)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	got := string(body)
	for _, leak := range []string{recordedSrc, recordedBuild, "/var/build/recorder-host"} {
		if strings.Contains(got, leak) {
			t.Errorf("absolute path %q leaked into probe.json:\n%s", leak, got)
		}
	}

	// And the original input must NOT have been mutated (the
	// scrub functions return value/map copies — verify the
	// caller's Reply still carries the recorded paths).
	if reply.Codemodel.Paths.Build != recordedBuild {
		t.Errorf("Marshal mutated input Reply.Codemodel.Paths.Build")
	}
	if reply.CMakeFiles.Paths.Build != recordedBuild {
		t.Errorf("Marshal mutated input Reply.CMakeFiles.Paths.Build")
	}
	if reply.Targets["hello"].Paths.Build != recordedBuild {
		t.Errorf("Marshal mutated input Reply.Targets[hello].Paths.Build")
	}
	if reply.Directories["directory-Release.json"].Paths.Build != recordedBuild {
		t.Errorf("Marshal mutated input Reply.Directories[..].Paths.Build")
	}
	if reply.Codemodel.Configurations[0].Directories[0].Build != recordedBuild {
		t.Errorf("Marshal mutated input Reply.Codemodel.Configurations[0].Directories[0].Build")
	}
}

// TestMarshal_FiltersVolatileCacheEntries: the build-dir-derived
// cache entries cmake's File API emits (CMAKE_BINARY_DIR, every
// *_BINARY_DIR, CMAKE_FIND_PACKAGE_REDIRECTS_DIR, log-file paths
// inside the build dir, etc.) carry per-run absolute paths.
// Letting them through would make probe.json non-deterministic
// across runs even when the underlying cmake graph is byte-
// identical. Marshal must drop them before serialization.
func TestMarshal_FiltersVolatileCacheEntries(t *testing.T) {
	reply := &fileapi.Reply{
		Cache: fileapi.Cache{
			Entries: []fileapi.CacheEntry{
				{Name: "CMAKE_BINARY_DIR", Value: "/tmp/sandbox-abc/build"},
				{Name: "CMAKE_HOME_DIRECTORY", Value: "/tmp/sandbox-abc/source"},
				{Name: "PROJECT_BINARY_DIR", Value: "/tmp/sandbox-abc/build"},
				{Name: "FOO_BINARY_DIR", Value: "/tmp/sandbox-abc/build/foo"},
				{Name: "PROJECT_SOURCE_DIR", Value: "/tmp/sandbox-abc/source"},
				{Name: "CMAKE_FIND_PACKAGE_REDIRECTS_DIR", Value: "/tmp/sandbox-abc/build/CMakeFiles/pkgRedirects"},
				// Derived path-bearing var: name is benign but value
				// references the build dir as a substring; should drop.
				{Name: "DERIVED_LOG_PATH", Value: "log at /tmp/sandbox-abc/build/log.txt"},
				// Stable entries: should round-trip.
				{Name: "PROJECT_VERSION", Value: "1.2.3"},
				{Name: "BUILD_SHARED_LIBS", Value: "ON"},
				{Name: "CMAKE_C_COMPILER_ID", Value: "GNU"},
			},
		},
	}
	body, err := Marshal(toolchain.Variant{Name: "baseline"}, reply)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	got := string(body)

	// Volatile entries gone.
	for _, dropped := range []string{
		"CMAKE_BINARY_DIR",
		"CMAKE_HOME_DIRECTORY",
		"PROJECT_BINARY_DIR",
		"FOO_BINARY_DIR",
		"PROJECT_SOURCE_DIR",
		"CMAKE_FIND_PACKAGE_REDIRECTS_DIR",
		"DERIVED_LOG_PATH",
	} {
		if strings.Contains(got, `"`+dropped+`"`) {
			t.Errorf("%s should have been filtered out:\n%s", dropped, got)
		}
	}
	// And the absolute path itself shouldn't appear anywhere in
	// the serialized cache (defense in depth — even if a name is
	// missed, the value-side filter catches its embedded path).
	if strings.Contains(got, "/tmp/sandbox-abc") {
		t.Errorf("build-dir path /tmp/sandbox-abc leaked into probe.json:\n%s", got)
	}

	// Stable entries survive.
	for _, kept := range []string{"PROJECT_VERSION", "BUILD_SHARED_LIBS", "CMAKE_C_COMPILER_ID"} {
		if !strings.Contains(got, `"`+kept+`"`) {
			t.Errorf("expected %s to survive the filter; got:\n%s", kept, got)
		}
	}
}
