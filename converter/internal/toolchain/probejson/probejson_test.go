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
