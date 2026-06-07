package lower

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// reanchorBuildDirCopyGenrule fixes grpc's "cd <build>/protos && protoc -I .
// src/proto/x.proto" shape: the producerless build-dir copy is dropped, the
// cwd-relative reads re-anchor to the source-tree twins, the output-root dir is
// $(RULEDIR)-anchored, the in-tree plugin is $(execpath)-ed, and a host-tool
// basename phantom is dropped.
func TestReanchorBuildDirCopyGenrule(t *testing.T) {
	raw := "cd /b/protos && /host/bin/protoc-31.1.0 --grpc_out=:gens --cpp_out=gens " +
		"--plugin=protoc-gen-grpc=grpc_cpp_plugin -I . src/proto/x.proto"
	// after rewriteGenruleCmd: cd stripped, buildDir prefixes gone, host tool abs.
	rewritten := "/host/bin/protoc-31.1.0 --grpc_out=:gens --cpp_out=gens " +
		"--plugin=protoc-gen-grpc=grpc_cpp_plugin -I . src/proto/x.proto"
	srcs := []string{
		"grpc_cpp_plugin",
		"protoc-31.1.0",            // host-tool basename phantom
		"protos/src/proto/x.proto", // build-dir copy (twin of source-tree)
		"src/proto/x.proto",        // source-tree twin
	}
	outs := []string{"gens/src/proto/x.pb.cc", "gens/src/proto/x.pb.h"}

	cmd, kept, tools := reanchorBuildDirCopyGenrule(raw, rewritten, srcs, outs, "", "/b", "elements/grpc")

	wantCmd := "/host/bin/protoc-31.1.0 --grpc_out=:$(RULEDIR)/gens --cpp_out=$(RULEDIR)/gens " +
		"--plugin=protoc-gen-grpc=$(execpath grpc_cpp_plugin) -I elements/grpc elements/grpc/src/proto/x.proto"
	if cmd != wantCmd {
		t.Errorf("cmd:\n got  %q\n want %q", cmd, wantCmd)
	}
	want := []string{"src/proto/x.proto"}
	if !reflect.DeepEqual(kept, want) {
		t.Errorf("kept srcs = %v; want %v", kept, want)
	}
	if wantTools := []string{"grpc_cpp_plugin"}; !reflect.DeepEqual(tools, wantTools) {
		t.Errorf("tools = %v; want %v", tools, wantTools)
	}
}

// No build-dir-copy cd → untouched (the corpus norm).
func TestReanchorBuildDirCopyGenrule_NoOp(t *testing.T) {
	raw := "protoc --cpp_out=gens x.proto"
	srcs := []string{"x.proto"}
	cmd, kept, _ := reanchorBuildDirCopyGenrule(raw, raw, srcs, []string{"gens/x.pb.cc"}, "", "/b", "elements/grpc")
	if cmd != raw || !reflect.DeepEqual(kept, srcs) {
		t.Errorf("expected no-op; got cmd=%q kept=%v", cmd, kept)
	}
}

// protoImportClosure walks transitive `import "..."` deps that exist on disk.
func TestProtoImportClosure(t *testing.T) {
	dir := t.TempDir()
	write := func(rel, body string) {
		p := filepath.Join(dir, rel)
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("a/svc.proto", "syntax=\"proto3\";\nimport \"a/msg.proto\";\nimport \"google/protobuf/any.proto\";\n")
	write("a/msg.proto", "syntax=\"proto3\";\nimport \"a/base.proto\";\n")
	write("a/base.proto", "syntax=\"proto3\";\n")

	got := protoImportClosure([]string{"a/svc.proto"}, dir)
	want := []string{"a/base.proto", "a/msg.proto"} // sorted; the WKT (not on disk) is skipped
	if !reflect.DeepEqual(got, want) {
		t.Errorf("imports = %v; want %v", got, want)
	}
}

// isGeneratedOutputRoot: a dir holding a genrule output is a gen root.
func TestIsGeneratedOutputRoot(t *testing.T) {
	o2g := map[string]string{"gens/src/proto/x.pb.cc": "gen_x"}
	if !isGeneratedOutputRoot("gens", o2g) {
		t.Error("gens should be a generated-output root")
	}
	if isGeneratedOutputRoot("include", o2g) {
		t.Error("include is not a generated-output root")
	}
}
