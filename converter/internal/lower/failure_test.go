package lower_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/internal/failure"
	"github.com/sstriker/buildstream-bazel/converter/internal/fileapi"
	"github.com/sstriker/buildstream-bazel/converter/internal/lower"
	"github.com/sstriker/buildstream-bazel/converter/ir"
)

// Surface tests: each Tier-1 code emitted by lower has at least one synthetic
// reply that triggers it. Codes documented in docs/failure-schema.md must be
// either exercised here or marked (M2)/reserved in the doc.

func TestFailure_UnsupportedTargetType(t *testing.T) {
	// UTILITY is silently skipped (the underlying add_custom_command
	// is recovered separately). OBJECT_LIBRARY is now supported via
	// alwayslink=True. Use a fabricated target type unknown to the
	// switch to exercise the unsupported-target-type emission point.
	r := &fileapi.Reply{
		Codemodel: fileapi.Codemodel{
			Configurations: []fileapi.Configuration{{
				Name:    "Release",
				Targets: []fileapi.ConfigTargetRef{{Name: "obj", Id: "obj::@1"}},
			}},
		},
		Targets: map[string]fileapi.Target{
			"obj::@1": {
				Name: "obj",
				Type: "GLOBAL_TARGET",
			},
		},
	}
	_, err := lower.ToIR(r, nil, lower.Options{})
	assertCode(t, err, failure.UnsupportedTargetType)
}

func TestFailure_UnsupportedCustomCommand_GeneratedSource(t *testing.T) {
	r := &fileapi.Reply{
		Codemodel: fileapi.Codemodel{
			Configurations: []fileapi.Configuration{{
				Name:    "Release",
				Targets: []fileapi.ConfigTargetRef{{Name: "lib", Id: "lib::@1"}},
			}},
		},
		Targets: map[string]fileapi.Target{
			"lib::@1": {
				Name: "lib",
				Type: "STATIC_LIBRARY",
				Sources: []fileapi.TargetSource{{
					Path:        "generated.c",
					IsGenerated: true,
				}},
			},
		},
	}
	_, err := lower.ToIR(r, nil, lower.Options{})
	assertCode(t, err, failure.UnsupportedCustomCommand)
}

func TestFailure_FileAPIMalformed_DanglingTargetRef(t *testing.T) {
	r := &fileapi.Reply{
		Codemodel: fileapi.Codemodel{
			Configurations: []fileapi.Configuration{{
				Name: "Release",
				Targets: []fileapi.ConfigTargetRef{{
					Name: "ghost", Id: "ghost::@nonexistent",
				}},
			}},
		},
		Targets: map[string]fileapi.Target{}, // ref not present
	}
	_, err := lower.ToIR(r, nil, lower.Options{})
	assertCode(t, err, failure.FileAPIMalformed)
}

func TestFailure_UnsupportedTargetType_MultiConfig(t *testing.T) {
	// Phase 5: multi-config is a supported path — the fold projects each
	// config's deltas into //config:<name> select() arms. A codemodel
	// whose configs share the same target set (the common case: same
	// targets, flags differ per config) is NOT a Tier-1 refusal in strict
	// mode anymore; ToIR returns a package.
	shared := &fileapi.Reply{
		Codemodel: fileapi.Codemodel{
			Configurations: []fileapi.Configuration{
				{Name: "Release", Targets: []fileapi.ConfigTargetRef{{Name: "lib", Id: "lib::@1"}}},
				{Name: "Debug", Targets: []fileapi.ConfigTargetRef{{Name: "lib", Id: "lib::@1"}}},
			},
		},
		Targets: map[string]fileapi.Target{
			"lib::@1": {Name: "lib", Type: "STATIC_LIBRARY"},
		},
	}
	if _, err := lower.ToIR(shared, nil, lower.Options{}); err != nil {
		t.Fatalf("strict-mode multi-config with a shared target set should convert, got: %v", err)
	}

	// The one residual the first-config-primary fold can't recover is a
	// target built only in a non-primary configuration — strict mode still
	// refuses that as genuine intent loss.
	configOnly := &fileapi.Reply{
		Codemodel: fileapi.Codemodel{
			Configurations: []fileapi.Configuration{
				{Name: "Release", Targets: []fileapi.ConfigTargetRef{{Name: "lib", Id: "lib::@1"}}},
				{Name: "Debug", Targets: []fileapi.ConfigTargetRef{
					{Name: "lib", Id: "lib::@1"},
					{Name: "debug_only", Id: "debug_only::@2"},
				}},
			},
		},
		Targets: map[string]fileapi.Target{
			"lib::@1": {Name: "lib", Type: "STATIC_LIBRARY"},
		},
	}
	_, err := lower.ToIR(configOnly, nil, lower.Options{})
	assertCode(t, err, failure.UnsupportedTargetType)
}

// TestFailure_UnsupportedExecuteProcess_Stamp covers the
// stamp-pattern refusal: a CMakeLists.txt that runs `git
// rev-parse HEAD` at configure time to populate a version
// macro. The classifier flags this as Stamp; v1 has no
// repo-rule analog wired so the converter refuses the call
// with a typed Tier-1 failure listing the offending location.
func TestFailure_UnsupportedExecuteProcess_Stamp(t *testing.T) {
	r := &fileapi.Reply{
		Codemodel: fileapi.Codemodel{
			Paths: fileapi.CodemodelPaths{Source: "/src"},
			Configurations: []fileapi.Configuration{{
				Name: "Release",
			}},
		},
	}
	traceRaw := []byte(
		`{"args":["COMMAND","git","rev-parse","HEAD","OUTPUT_VARIABLE","GIT_SHA"],"cmd":"execute_process","file":"/src/CMakeLists.txt","line":4}` + "\n",
	)
	_, err := lower.ToIR(r, nil, lower.Options{TraceRaw: traceRaw})
	assertCode(t, err, failure.UnsupportedExecuteProcess)
}

// TestFailure_UnsupportedExecuteProcess_Aggregates asserts
// that multiple unliftable calls in one project produce a
// single failure with all locations listed, sorted by file
// name then numeric line so the output is stable across
// cmake's evaluation-order drift. Trace input is in
// reverse-line order (line 12, line 7, line 2) and one is
// in a different file; the rendered message must list them
// in (CMakeLists.txt:2, :7, :12, sub/CMakeLists.txt:5)
// order — strictly numeric within each file, not
// lexicographic (which would put :12 before :2).
func TestFailure_UnsupportedExecuteProcess_Aggregates(t *testing.T) {
	r := &fileapi.Reply{
		Codemodel: fileapi.Codemodel{
			Paths: fileapi.CodemodelPaths{Source: "/src"},
			Configurations: []fileapi.Configuration{{
				Name: "Release",
			}},
		},
	}
	traceRaw := []byte(
		`{"args":["COMMAND","git","describe","--tags","OUTPUT_VARIABLE","V12"],"cmd":"execute_process","file":"/src/CMakeLists.txt","line":12}` + "\n" +
			`{"args":["COMMAND","uname","-m","OUTPUT_VARIABLE","ARCH"],"cmd":"execute_process","file":"/src/CMakeLists.txt","line":7}` + "\n" +
			`{"args":["COMMAND","git","rev-parse","HEAD","OUTPUT_VARIABLE","GIT_SHA"],"cmd":"execute_process","file":"/src/CMakeLists.txt","line":2}` + "\n" +
			`{"args":["COMMAND","hostname","OUTPUT_VARIABLE","H"],"cmd":"execute_process","file":"/src/sub/CMakeLists.txt","line":5}` + "\n",
	)
	_, err := lower.ToIR(r, nil, lower.Options{TraceRaw: traceRaw})
	if err == nil {
		t.Fatal("expected error")
	}
	assertCode(t, err, failure.UnsupportedExecuteProcess)
	msg := err.Error()
	// Numeric line sort within file: :2 < :7 < :12. Then
	// the secondary file (sub/CMakeLists.txt) follows. A
	// lexicographic sort on rendered "file:line" strings
	// would put ":12" before ":2"; the numeric comparator
	// gets the order right.
	want := []string{
		"/src/CMakeLists.txt:2",
		"/src/CMakeLists.txt:7",
		"/src/CMakeLists.txt:12",
		"/src/sub/CMakeLists.txt:5",
	}
	prev := -1
	for _, w := range want {
		idx := strings.Index(msg, w)
		if idx < 0 {
			t.Errorf("missing location %q in message:\n%s", w, msg)
			continue
		}
		if idx < prev {
			t.Errorf("location %q at offset %d appears before its predecessor (sort regression):\n%s", w, idx, msg)
		}
		prev = idx
	}
	if !strings.Contains(msg, "[stamp]") || !strings.Contains(msg, "[probe]") {
		t.Errorf("expected both bucket labels in failure message; got: %s", msg)
	}
}

// TestFallback_UnsupportedExecuteProcess_EnumeratesPerTargetStubs
// covers the Phase B fallback path post-Step-2.5: with
// UnsupportedExecuteProcessFallback set, classifier refusals
// no longer exit Tier-1. Instead ToIR returns a placeholder
// ir.Package whose targets are per-target stubs derived from
// the codemodel's Install destinations (cc_import for
// STATIC/SHARED, sh_binary for EXECUTABLE), preceded by a
// single extract genrule that untars install_tree.tar into
// the install paths the stubs reference.
func TestFallback_UnsupportedExecuteProcess_EnumeratesPerTargetStubs(t *testing.T) {
	staticLib := fileapi.Target{
		Name:       "thelib",
		Type:       "STATIC_LIBRARY",
		NameOnDisk: "libthelib.a",
		Install: &fileapi.TargetInstall{
			Destinations: []fileapi.TargetInstallDest{{Path: "lib"}},
		},
	}
	sharedLib := fileapi.Target{
		Name:       "shared",
		Type:       "SHARED_LIBRARY",
		NameOnDisk: "libshared.so.1",
		Install: &fileapi.TargetInstall{
			Destinations: []fileapi.TargetInstallDest{{Path: "lib"}},
		},
	}
	exe := fileapi.Target{
		Name:       "thetool",
		Type:       "EXECUTABLE",
		NameOnDisk: "thetool",
		Install: &fileapi.TargetInstall{
			Destinations: []fileapi.TargetInstallDest{{Path: "bin"}},
		},
	}
	internalLib := fileapi.Target{
		Name:       "internal",
		Type:       "STATIC_LIBRARY",
		NameOnDisk: "libinternal.a",
		// No Install — not exposed across the round-2
		// boundary; placeholder must skip.
	}
	utility := fileapi.Target{Name: "u", Type: "UTILITY"}

	r := &fileapi.Reply{
		Codemodel: fileapi.Codemodel{
			Paths: fileapi.CodemodelPaths{Source: "/src"},
			Configurations: []fileapi.Configuration{{
				Name: "Release",
				Targets: []fileapi.ConfigTargetRef{
					{Name: "thelib", Id: "thelib::@1"},
					{Name: "shared", Id: "shared::@2"},
					{Name: "thetool", Id: "thetool::@3"},
					{Name: "internal", Id: "internal::@4"},
					{Name: "u", Id: "u::@5"},
				},
			}},
		},
		Targets: map[string]fileapi.Target{
			"thelib::@1":   staticLib,
			"shared::@2":   sharedLib,
			"thetool::@3":  exe,
			"internal::@4": internalLib,
			"u::@5":        utility,
		},
	}
	traceRaw := []byte(
		`{"args":["COMMAND","uname","-m","OUTPUT_VARIABLE","ARCH"],"cmd":"execute_process","file":"/src/CMakeLists.txt","line":7}` + "\n",
	)
	pkg, err := lower.ToIR(r, nil, lower.Options{
		TraceRaw:                          traceRaw,
		UnsupportedExecuteProcessFallback: true,
	})
	if err != nil {
		t.Fatalf("expected nil error in fallback mode; got %v", err)
	}
	if pkg == nil {
		t.Fatal("expected placeholder ir.Package, got nil")
	}

	// Index by name. UTILITY target skipped, internal
	// target (no install) skipped → only the 3 exposed
	// targets + the extract genrule should be present.
	byName := map[string]ir.Target{}
	for _, target := range pkg.Targets {
		byName[target.Name] = target
	}
	// pick_file targets project each artefact out of the install-
	// root TreeArtifact (replacing the old _install_tree_extract
	// tar-untar genrule). One per unique install path.
	for _, pn := range []string{"_pick_lib_libthelib_a", "_pick_lib_libshared_so_1", "_pick_bin_thetool"} {
		if _, ok := byName[pn]; !ok {
			t.Errorf("expected pick_file %q; got %+v", pn, byName)
		}
	}
	if _, ok := byName["thelib"]; !ok {
		t.Errorf("expected thelib stub; got %+v", byName)
	}
	if _, ok := byName["shared"]; !ok {
		t.Errorf("expected shared stub; got %+v", byName)
	}
	if _, ok := byName["thetool"]; !ok {
		t.Errorf("expected thetool stub; got %+v", byName)
	}
	if _, ok := byName["internal"]; ok {
		t.Errorf("internal (no install) should be skipped from placeholder; got %+v", byName)
	}
	if _, ok := byName["u"]; ok {
		t.Errorf("UTILITY target should be skipped from placeholder; got %+v", byName)
	}

	// STATIC_LIBRARY → cc_import + static_library.
	thelib := byName["thelib"]
	if thelib.Kind != ir.KindCCImport {
		t.Errorf("thelib kind: %v want cc_import", thelib.Kind)
	}
	if thelib.StaticLibrary != ":_pick_lib_libthelib_a" {
		t.Errorf("thelib StaticLibrary: %q want :_pick_lib_libthelib_a", thelib.StaticLibrary)
	}
	if thelib.SharedLibrary != "" {
		t.Errorf("thelib SharedLibrary should be empty for STATIC; got %q", thelib.SharedLibrary)
	}

	// SHARED_LIBRARY → cc_import + shared_library.
	shared := byName["shared"]
	if shared.Kind != ir.KindCCImport {
		t.Errorf("shared kind: %v want cc_import", shared.Kind)
	}
	if shared.SharedLibrary != ":_pick_lib_libshared_so_1" {
		t.Errorf("shared SharedLibrary: %q want :_pick_lib_libshared_so_1", shared.SharedLibrary)
	}

	// EXECUTABLE → sh_binary + srcs[0] = bin path.
	thetool := byName["thetool"]
	if thetool.Kind != ir.KindShBinary {
		t.Errorf("thetool kind: %v want sh_binary", thetool.Kind)
	}
	if len(thetool.Srcs) != 1 || thetool.Srcs[0] != ":_pick_bin_thetool" {
		t.Errorf("thetool Srcs: %v want [:_pick_bin_thetool]", thetool.Srcs)
	}

	// pick_file targets: each projects one tree-relative path out
	// of the install-root TreeArtifact via the same-package install
	// target (":_trace_build" default when no package path is set).
	wantPicks := map[string]string{
		"_pick_lib_libthelib_a":    "lib/libthelib.a",
		"_pick_lib_libshared_so_1": "lib/libshared.so.1",
		"_pick_bin_thetool":        "bin/thetool",
	}
	for name, wantPath := range wantPicks {
		p := byName[name]
		if p.Kind != ir.KindPickFile {
			t.Errorf("%s kind: %v want pick_file", name, p.Kind)
		}
		if p.PickPath != wantPath {
			t.Errorf("%s PickPath: %q want %q", name, p.PickPath, wantPath)
		}
		if p.PickSrc != ":_trace_build" {
			t.Errorf("%s PickSrc: %q want :_trace_build", name, p.PickSrc)
		}
	}

	// Marker tags + visibility on every emitted target.
	for _, target := range pkg.Targets {
		hasMarker := false
		for _, tag := range target.Tags {
			if tag == "cmake-codegen-execute-process-fallback" {
				hasMarker = true
				break
			}
		}
		if !hasMarker {
			t.Errorf("target %q missing cmake-codegen-execute-process-fallback tag; tags=%v", target.Name, target.Tags)
		}
	}
	// Per-target stubs are public; the pick_file projections are private.
	for name, want := range map[string]string{
		"thelib":                   "//visibility:public",
		"shared":                   "//visibility:public",
		"thetool":                  "//visibility:public",
		"_pick_lib_libthelib_a":    "//visibility:private",
		"_pick_lib_libshared_so_1": "//visibility:private",
		"_pick_bin_thetool":        "//visibility:private",
	} {
		got := byName[name]
		if len(got.Visibility) != 1 || got.Visibility[0] != want {
			t.Errorf("%s visibility: %v want [%s]", name, got.Visibility, want)
		}
	}
}

// TestFallback_PopulatesHdrsFromFileSets covers the FileSet
// HEADERS path: a STATIC_LIBRARY whose target_sources(...
// FILE_SET HEADERS BASE_DIRS include FILES include/foo.h)
// has its public header surfaced as cc_import.hdrs in the
// placeholder, and the same path appears in the extract
// genrule's outs so the file is at least produced. (A
// follow-on extends the placeholder to also export an include
// path — the cc_import emitter doesn't render `includes`
// today, so bare-bracket #include <foo.h> still won't resolve.
// Consumers using `install_tree/include/foo.h` directly do
// resolve through the current shape.)
func TestFallback_PopulatesHdrsFromFileSets(t *testing.T) {
	idx0 := 0
	thelib := fileapi.Target{
		Name:       "thelib",
		Type:       "STATIC_LIBRARY",
		NameOnDisk: "libthelib.a",
		Paths:      fileapi.TargetPaths{Source: "/src"},
		Install: &fileapi.TargetInstall{
			Destinations: []fileapi.TargetInstallDest{{Path: "lib"}},
		},
		FileSets: []fileapi.TargetFileSet{{
			Name:            "thelib_headers",
			Type:            "HEADERS",
			Visibility:      "PUBLIC",
			BaseDirectories: []string{"/src/include"},
		}},
		Sources: []fileapi.TargetSource{
			{Path: "src/lib.c"},
			{Path: "include/thelib.h", FileSetIndex: &idx0},
			{Path: "include/sub/internal.h", FileSetIndex: &idx0},
		},
	}

	r := &fileapi.Reply{
		Codemodel: fileapi.Codemodel{
			Paths: fileapi.CodemodelPaths{Source: "/src"},
			Configurations: []fileapi.Configuration{{
				Name: "Release",
				Targets: []fileapi.ConfigTargetRef{
					{Name: "thelib", Id: "thelib::@1"},
				},
			}},
		},
		Targets: map[string]fileapi.Target{
			"thelib::@1": thelib,
		},
	}
	traceRaw := []byte(
		`{"args":["COMMAND","uname","-m","OUTPUT_VARIABLE","ARCH"],"cmd":"execute_process","file":"/src/CMakeLists.txt","line":7}` + "\n",
	)
	pkg, err := lower.ToIR(r, nil, lower.Options{
		TraceRaw:                          traceRaw,
		UnsupportedExecuteProcessFallback: true,
	})
	if err != nil {
		t.Fatalf("expected fallback success; got %v", err)
	}

	byName := map[string]ir.Target{}
	for _, target := range pkg.Targets {
		byName[target.Name] = target
	}
	thelibStub := byName["thelib"]
	// hdrs now reference per-header pick_file labels (sorted by
	// the tree-relative path before name derivation).
	wantHdrs := []string{
		":_pick_include_sub_internal_h",
		":_pick_include_thelib_h",
	}
	if len(thelibStub.Hdrs) != len(wantHdrs) {
		t.Fatalf("hdrs: %v want %v", thelibStub.Hdrs, wantHdrs)
	}
	for i, want := range wantHdrs {
		if thelibStub.Hdrs[i] != want {
			t.Errorf("hdrs[%d]: %q want %q", i, thelibStub.Hdrs[i], want)
		}
	}

	// One pick_file per artefact + header path so the cc_import's
	// static_library / hdrs labels resolve out of the install root.
	wantPicks := map[string]string{
		"_pick_lib_libthelib_a":        "lib/libthelib.a",
		"_pick_include_thelib_h":       "include/thelib.h",
		"_pick_include_sub_internal_h": "include/sub/internal.h",
	}
	for name, wantPath := range wantPicks {
		p := byName[name]
		if p.Kind != ir.KindPickFile {
			t.Errorf("%s kind: %v want pick_file", name, p.Kind)
		}
		if p.PickPath != wantPath {
			t.Errorf("%s PickPath: %q want %q", name, p.PickPath, wantPath)
		}
	}
}

// TestFallback_NoRefusals_NoEffect asserts that the fallback
// flag is harmless when there are no execute_process refusals
// — ToIR proceeds with the native lowering path unchanged.
// Guards against the flag accidentally short-circuiting
// projects that don't need fallback.
func TestFallback_NoRefusals_NoEffect(t *testing.T) {
	r := &fileapi.Reply{
		Codemodel: fileapi.Codemodel{
			Paths: fileapi.CodemodelPaths{Source: "/src"},
			Configurations: []fileapi.Configuration{{
				Name: "Release",
				Targets: []fileapi.ConfigTargetRef{
					{Name: "lib", Id: "lib::@1"},
				},
			}},
		},
		Targets: map[string]fileapi.Target{
			"lib::@1": {Name: "lib", Type: "STATIC_LIBRARY"},
		},
	}
	pkg, err := lower.ToIR(r, nil, lower.Options{
		UnsupportedExecuteProcessFallback: true,
	})
	if err != nil {
		t.Fatalf("expected success; got %v", err)
	}
	if pkg == nil {
		t.Fatal("expected ir.Package")
	}
	// Native lowering path produced one cc_library; no
	// fallback marker (the flag was set but had no
	// refusals to act on, so the native shape stands).
	if len(pkg.Targets) != 1 {
		t.Errorf("expected 1 target, got %d", len(pkg.Targets))
	}
	for _, target := range pkg.Targets {
		for _, tag := range target.Tags {
			if tag == "cmake-codegen-execute-process-fallback" {
				t.Errorf("target %q should not carry fallback marker when there were no refusals; tags=%v", target.Name, target.Tags)
			}
		}
	}
}

func assertCode(t *testing.T, err error, want failure.Code) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected Tier-1 error with code %q, got nil", want)
	}
	var fe *failure.Error
	if !errors.As(err, &fe) {
		t.Fatalf("err = %v (%T), want *failure.Error", err, err)
	}
	if fe.Code != want {
		t.Errorf("code = %q, want %q", fe.Code, want)
	}
}
