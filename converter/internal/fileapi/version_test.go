package fileapi_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sstriker/buildstream-bazel/converter/internal/fileapi"
)

// TestLoad_RejectsUnsupportedSchemaMajor confirms the loader fails
// fast and clearly when a future cmake bumps any object kind's
// schema major beyond what we know how to parse. This is the
// compatibility tripwire that supersedes a pure cmake-binary
// version check.
func TestLoad_RejectsUnsupportedSchemaMajor(t *testing.T) {
	for _, kind := range []string{"codemodel", "cache", "toolchains", "cmakeFiles"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			writeFutureSchemaReply(t, dir, kind, fileapi.SupportedObjectMajors[kind]+1)
			_, err := fileapi.Load(dir)
			if err == nil {
				t.Fatalf("Load: expected error for unsupported %s major, got nil", kind)
			}
			if !strings.Contains(err.Error(), kind) ||
				!strings.Contains(err.Error(), "not supported") {
				t.Errorf("Load: error %q missing kind/diagnostic", err)
			}
		})
	}
}

// TestLoad_AcceptsUnknownObjectKinds confirms a future cmake adding a
// new object kind doesn't break loading — the loader skips kinds it
// doesn't recognise.
func TestLoad_AcceptsUnknownObjectKinds(t *testing.T) {
	dir := t.TempDir()
	// Write the four supported kinds at their pinned majors plus one
	// future kind. Should load without error.
	writeMinimalReply(t, dir)
	idxPath := filepath.Join(dir, mustGlob(t, dir, "index-*.json"))
	body, _ := os.ReadFile(idxPath)
	patched := strings.Replace(string(body),
		`"objects" : [`,
		`"objects" : [
		{ "kind": "futureKind", "version": { "major": 1, "minor": 0 }, "jsonFile": "futureKind-1.json" },`,
		1)
	if err := os.WriteFile(idxPath, []byte(patched), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := fileapi.Load(dir); err != nil {
		t.Fatalf("Load: unexpected error with unknown kind present: %v", err)
	}
}

// writeFutureSchemaReply stages a minimal-but-valid reply directory
// with one of the four standard kinds set to a major version one past
// the supported max. Used to exercise the per-kind major-version
// rejection path without recording a real cmake fixture.
func writeFutureSchemaReply(t *testing.T, dir, futureKind string, futureMajor int) {
	t.Helper()
	writeMinimalReply(t, dir)

	idxPath := filepath.Join(dir, mustGlob(t, dir, "index-*.json"))
	body, err := os.ReadFile(idxPath)
	if err != nil {
		t.Fatal(err)
	}
	// Bump the targeted kind's major. The string replacement is
	// scoped enough to be safe: we look for `"kind": "<kind>"` then
	// the next "major" key.
	want := `"kind" : "` + futureKind + `"`
	off := strings.Index(string(body), want)
	if off < 0 {
		t.Fatalf("no kind block for %q in index", futureKind)
	}
	tail := string(body)[off:]
	majorIdx := strings.Index(tail, `"major"`)
	if majorIdx < 0 {
		t.Fatalf("no major field after kind %q", futureKind)
	}
	colon := strings.Index(tail[majorIdx:], ":")
	if colon < 0 {
		t.Fatalf("malformed major field for %q", futureKind)
	}
	startOfNum := off + majorIdx + colon + 1
	endOfNum := startOfNum
	for endOfNum < len(body) && (body[endOfNum] == ' ' || body[endOfNum] == '\t') {
		endOfNum++
	}
	digitStart := endOfNum
	for endOfNum < len(body) && body[endOfNum] >= '0' && body[endOfNum] <= '9' {
		endOfNum++
	}
	bumped := append([]byte{}, body[:digitStart]...)
	bumped = append(bumped, []byte{byte('0' + futureMajor)}...)
	bumped = append(bumped, body[endOfNum:]...)
	if err := os.WriteFile(idxPath, bumped, 0o644); err != nil {
		t.Fatal(err)
	}
}

// writeMinimalReply stages a synthetic four-kind reply with all
// majors at their supported values. Object payloads are just
// well-formed enough for loadIndex + readJSON to succeed.
func writeMinimalReply(t *testing.T, dir string) {
	t.Helper()
	writeFile(t, dir, "codemodel-v2.json", `{"kind":"codemodel","version":{"major":2,"minor":0},"paths":{"source":"/src","build":"/b"},"configurations":[]}`)
	writeFile(t, dir, "cache-v2.json", `{"kind":"cache","version":{"major":2,"minor":0},"entries":[]}`)
	writeFile(t, dir, "toolchains-v1.json", `{"kind":"toolchains","version":{"major":1,"minor":0},"toolchains":[]}`)
	writeFile(t, dir, "cmakeFiles-v1.json", `{"kind":"cmakeFiles","version":{"major":1,"minor":0},"paths":{"source":"/src","build":"/b"},"inputs":[]}`)
	writeFile(t, dir, "index-2026.json", `{
		"cmake": { "generator": { "name": "Ninja", "multiConfig": false }, "paths": { "cmake": "/usr/bin/cmake", "ctest": "/usr/bin/ctest", "cpack": "/usr/bin/cpack", "root": "/usr" }, "version": { "major": 3, "minor": 28, "patch": 3, "string": "3.28.3", "suffix": "", "isDirty": false } },
		"objects" : [
			{ "kind" : "codemodel", "version": { "major": 2, "minor": 0 }, "jsonFile": "codemodel-v2.json" },
			{ "kind" : "cache", "version": { "major": 2, "minor": 0 }, "jsonFile": "cache-v2.json" },
			{ "kind" : "toolchains", "version": { "major": 1, "minor": 0 }, "jsonFile": "toolchains-v1.json" },
			{ "kind" : "cmakeFiles", "version": { "major": 1, "minor": 0 }, "jsonFile": "cmakeFiles-v1.json" }
		]
	}`)
}

func writeFile(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

func mustGlob(t *testing.T, dir, pattern string) string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, pattern))
	if err != nil || len(matches) == 0 {
		t.Fatalf("glob %s: %v (got %v)", pattern, err, matches)
	}
	return filepath.Base(matches[0])
}
