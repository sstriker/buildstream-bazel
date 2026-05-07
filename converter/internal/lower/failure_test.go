package lower_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/sstriker/cmake-to-bazel/converter/internal/failure"
	"github.com/sstriker/cmake-to-bazel/converter/internal/fileapi"
	"github.com/sstriker/cmake-to-bazel/converter/internal/ir"
	"github.com/sstriker/cmake-to-bazel/converter/internal/lower"
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
	// M1 supports exactly one configuration. Codemodel with two trips the
	// blanket reject in lower.ToIR. Doc lists this under
	// `unsupported-target-type` until M2 adds multi-config support.
	r := &fileapi.Reply{
		Codemodel: fileapi.Codemodel{
			Configurations: []fileapi.Configuration{
				{Name: "Release"},
				{Name: "Debug"},
			},
		},
	}
	_, err := lower.ToIR(r, nil, lower.Options{})
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

// TestFallback_UnsupportedExecuteProcess_EnumeratesCodemodelTargets
// covers the Phase B fallback path: with
// UnsupportedExecuteProcessFallback set, classifier refusals
// (uname / git in this case) no longer exit Tier-1; ToIR
// returns a placeholder ir.Package whose targets are the
// codemodel's non-UTILITY targets as empty stubs (right Kind,
// public visibility, marker tag, no srcs).
func TestFallback_UnsupportedExecuteProcess_EnumeratesCodemodelTargets(t *testing.T) {
	r := &fileapi.Reply{
		Codemodel: fileapi.Codemodel{
			Paths: fileapi.CodemodelPaths{Source: "/src"},
			Configurations: []fileapi.Configuration{{
				Name: "Release",
				Targets: []fileapi.ConfigTargetRef{
					{Name: "thelib", Id: "thelib::@1"},
					{Name: "thetool", Id: "thetool::@2"},
					{Name: "noisy_utility", Id: "noisy_utility::@3"},
				},
			}},
		},
		Targets: map[string]fileapi.Target{
			"thelib::@1":        {Name: "thelib", Type: "STATIC_LIBRARY"},
			"thetool::@2":       {Name: "thetool", Type: "EXECUTABLE"},
			"noisy_utility::@3": {Name: "noisy_utility", Type: "UTILITY"},
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

	// Per-target stubs: STATIC_LIBRARY → cc_library +
	// linkstatic, EXECUTABLE → cc_binary, UTILITY → skipped.
	gotKinds := map[string]ir.Kind{}
	for _, target := range pkg.Targets {
		gotKinds[target.Name] = target.Kind
	}
	if len(gotKinds) != 2 {
		t.Errorf("expected 2 stub targets (UTILITY skipped); got %+v", gotKinds)
	}
	if gotKinds["thelib"] != ir.KindCCLibrary {
		t.Errorf("thelib kind: %v want cc_library", gotKinds["thelib"])
	}
	if gotKinds["thetool"] != ir.KindCCBinary {
		t.Errorf("thetool kind: %v want cc_binary", gotKinds["thetool"])
	}
	if _, present := gotKinds["noisy_utility"]; present {
		t.Errorf("UTILITY target should be skipped from placeholder; got %+v", gotKinds)
	}

	// Each stub carries the marker tag so audit queries can
	// distinguish placeholder targets from fully-converted
	// ones; visibility is public so downstream consumers can
	// reference :thelib / :thetool the same way they would
	// against a native-rendered element.
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
		if len(target.Visibility) != 1 || target.Visibility[0] != "//visibility:public" {
			t.Errorf("target %q visibility: %v want [//visibility:public]", target.Name, target.Visibility)
		}
		// Stubs are intentionally empty bodies — the
		// per-target install-destination wiring is queued
		// behind IR support for cc_import attributes
		// (Step 2.5 in docs/design/cmake-execute-process-round2-fallback.md).
		if len(target.Srcs) != 0 {
			t.Errorf("target %q srcs should be empty in v1 placeholder; got %v", target.Name, target.Srcs)
		}
	}

	// STATIC_LIBRARY stub carries Linkstatic for fidelity
	// with native render's STATIC_LIBRARY → cc_library +
	// linkstatic dispatch.
	for _, target := range pkg.Targets {
		if target.Name == "thelib" && !target.Linkstatic {
			t.Errorf("thelib placeholder should carry Linkstatic=true (mirrors native render's STATIC_LIBRARY shape)")
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
