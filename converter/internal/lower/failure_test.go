package lower_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/sstriker/cmake-to-bazel/converter/internal/failure"
	"github.com/sstriker/cmake-to-bazel/converter/internal/fileapi"
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
