//go:build buildbarn

// buildbarn_test exercises the REAPI Execute path (reapi.Executor)
// against a real Buildbarn deployment brought up via
// deploy/buildbarn/docker-compose.yml: it submits Actions through the
// scheduler at :8983 and verifies the worker actually runs them.
//
// Not wired into CI. Its `e2e-buildbarn-execute` gate retired with
// the orchestrator absorption (docs/design/orchestrator-absorption.md):
// the orchestrator was reapi.Executor's production consumer, and the
// write-a + Bazel path drives remote execution through Bazel's own
// REAPI client (covered by the e2e-meta-buildbarn-re gate), not
// reapi.Executor. Kept as a manually-runnable `-tags=buildbarn` test;
// whether reapi.Executor is still worth carrying is a follow-up.
//
// Two tests:
//
//   - TestE2E_Buildbarn_ExecuteSyntheticAction: trivial /bin/sh -c
//     action validates the protocol round trip (CAS upload, scheduler
//     accept, worker materialize+exec+package, ActionResult).
//
//   - TestE2E_Buildbarn_ExecuteRealConvertElement: drives a real
//     conversion (cmake configure + convert-element-cmake) inside the
//     custom worker image. Requires the worker image to have cmake /
//     ninja / bwrap pre-installed; see deploy/buildbarn/runner/Dockerfile.
//
// Gated behind the `buildbarn` build tag and skips at runtime if
// the scheduler endpoint isn't reachable.
package reapi_test

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/sstriker/buildstream-bazel/internal/cas"
	"github.com/sstriker/buildstream-bazel/internal/reapi"
)

func TestE2E_Buildbarn_ExecuteSyntheticAction(t *testing.T) {
	casEndpoint := envOr("BUILDBARN_CAS_ENDPOINT", "127.0.0.1:8980")
	execEndpoint := envOr("BUILDBARN_EXEC_ENDPOINT", "127.0.0.1:8983")

	if err := probe(casEndpoint); err != nil {
		t.Skipf("Buildbarn CAS %s unreachable: %v\n  bring up with: docker compose -f deploy/buildbarn/docker-compose.yml up -d", casEndpoint, err)
	}
	if err := probe(execEndpoint); err != nil {
		t.Skipf("Buildbarn scheduler %s unreachable: %v", execEndpoint, err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	store, err := cas.NewGRPCStore(ctx, cas.GRPCConfig{
		Endpoint: "grpc://" + casEndpoint,
		Insecure: true,
	})
	if err != nil {
		t.Fatalf("dial cas: %v", err)
	}
	defer store.Close()

	conn, err := grpc.NewClient(execEndpoint, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial execute: %v", err)
	}
	defer conn.Close()
	executor := reapi.NewGRPCExecutor(conn, "")

	// Synthesize a minimal Action: empty input root, Command runs
	// /bin/sh -c "echo hello > BUILD.bazel", declares BUILD.bazel
	// as the only output. Platform properties match
	// deploy/buildbarn/config/worker.jsonnet so the scheduler
	// dispatches to our worker pool.
	emptyDir := &repb.Directory{}
	dirBlob, err := cas.MarshalDeterministic(emptyDir)
	if err != nil {
		t.Fatalf("marshal empty dir: %v", err)
	}
	emptyDigest := cas.DigestOf(dirBlob)
	if err := store.PutBlob(ctx, emptyDigest, dirBlob); err != nil {
		t.Fatalf("upload empty dir: %v", err)
	}

	cmd := &repb.Command{
		Arguments:   []string{"/bin/sh", "-c", "echo hello > BUILD.bazel"},
		OutputPaths: []string{"BUILD.bazel"},
		Platform: &repb.Platform{
			Properties: []*repb.Platform_Property{
				{Name: "Arch", Value: "x86_64"},
				{Name: "OSFamily", Value: "linux"},
				{Name: "bwrap-version", Value: "0.8.0"},
				{Name: "cmake-version", Value: "3.28.3"},
				{Name: "ninja-version", Value: "1.11.1"},
			},
		},
	}
	cmdDigest, cmdBlob, err := cas.DigestProto(cmd)
	if err != nil {
		t.Fatalf("digest cmd: %v", err)
	}
	if err := store.PutBlob(ctx, cmdDigest, cmdBlob); err != nil {
		t.Fatalf("upload cmd: %v", err)
	}

	action := &repb.Action{
		CommandDigest:   cmdDigest,
		InputRootDigest: emptyDigest,
		Platform:        cmd.Platform,
	}
	actionDigest, actionBlob, err := cas.DigestProto(action)
	if err != nil {
		t.Fatalf("digest action: %v", err)
	}
	if err := store.PutBlob(ctx, actionDigest, actionBlob); err != nil {
		t.Fatalf("upload action: %v", err)
	}

	built := &reapi.BuiltAction{
		Action:        action,
		ActionDigest:  actionDigest,
		ActionBlob:    actionBlob,
		Command:       cmd,
		CommandDigest: cmdDigest,
		CommandBlob:   cmdBlob,
		InputRoot: &reapi.InputRoot{
			Root:        emptyDir,
			RootDigest:  emptyDigest,
			Directories: map[string]*repb.Directory{emptyDigest.Hash: emptyDir},
			Files:       nil,
		},
		OutputPaths: []string{"BUILD.bazel"},
	}

	ar, err := executor.Execute(ctx, store, built)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if ar.ExitCode != 0 {
		t.Fatalf("ExitCode = %d, want 0", ar.ExitCode)
	}
	if len(ar.OutputFiles) != 1 || ar.OutputFiles[0].Path != "BUILD.bazel" {
		t.Fatalf("OutputFiles = %+v, want [BUILD.bazel]", ar.OutputFiles)
	}

	// Worker actually wrote "hello\n"; fetch from CAS and verify.
	body, err := store.GetBlob(ctx, ar.OutputFiles[0].Digest)
	if err != nil {
		t.Fatalf("fetch output blob: %v", err)
	}
	if string(body) != "hello\n" {
		t.Errorf("output body = %q, want %q", body, "hello\n")
	}
}

// TestE2E_Buildbarn_ExecuteRealConvertElement submits a genuine
// convert-element-cmake Action through the worker — proves the custom worker
// image (deploy/buildbarn/runner/Dockerfile) actually has cmake /
// ninja / bwrap reachable on the runner's PATH and the conversion
// pipeline can run end-to-end inside the container.
//
// The previous synthetic-action test only validated that bytes flow
// through the protocol; this one validates that real cmake actually
// configures and convert-element-cmake actually emits BUILD.bazel + a cmake-
// config bundle.
//
// Skips if convert-element-cmake is not present at build/bin/convert-element-cmake
// (operator must `make converter` first).
func TestE2E_Buildbarn_ExecuteRealConvertElement(t *testing.T) {
	casEndpoint := envOr("BUILDBARN_CAS_ENDPOINT", "127.0.0.1:8980")
	execEndpoint := envOr("BUILDBARN_EXEC_ENDPOINT", "127.0.0.1:8983")

	if err := probe(casEndpoint); err != nil {
		t.Skipf("Buildbarn CAS %s unreachable: %v\n  bring up with: docker compose -f deploy/buildbarn/docker-compose.yml up -d", casEndpoint, err)
	}
	if err := probe(execEndpoint); err != nil {
		t.Skipf("Buildbarn scheduler %s unreachable: %v", execEndpoint, err)
	}

	// Locate the convert-element-cmake binary built by `make converter`.
	repoRoot, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	converterBin := filepath.Join(repoRoot, "build", "bin", "convert-element-cmake")
	if _, err := os.Stat(converterBin); err != nil {
		t.Skipf("convert-element-cmake binary not built (%s): run `make converter` first", converterBin)
	}

	shadowDir := filepath.Join(repoRoot, "converter", "testdata", "sample-projects", "hello-world")
	if _, err := os.Stat(shadowDir); err != nil {
		t.Fatalf("hello-world fixture missing: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	store, err := cas.NewGRPCStore(ctx, cas.GRPCConfig{
		Endpoint: "grpc://" + casEndpoint,
		Insecure: true,
	})
	if err != nil {
		t.Fatalf("dial cas: %v", err)
	}
	defer store.Close()

	conn, err := grpc.NewClient(execEndpoint, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial execute: %v", err)
	}
	defer conn.Close()
	executor := reapi.NewGRPCExecutor(conn, "")

	built, err := reapi.Build(reapi.Inputs{
		ShadowDir:    shadowDir,
		ConverterBin: converterBin,
		Platform: []reapi.PlatformProperty{
			{Name: "Arch", Value: "x86_64"},
			{Name: "OSFamily", Value: "linux"},
			{Name: "bwrap-version", Value: "0.8.0"},
			{Name: "cmake-version", Value: "3.28.3"},
			{Name: "ninja-version", Value: "1.11.1"},
		},
		// PATH must be set explicitly: REAPI Actions run with only
		// the env vars declared in Command.environment_variables, NOT
		// the worker's container PATH. Without this, convert-element-cmake's
		// child cmake invocation fails with
		//   cmakerun: cmake not on PATH: exec: "cmake": executable
		//   file not found in $PATH
		// even though our custom runner image symlinks cmake into
		// /usr/local/bin. The runner image ships cmake at /opt/cmake/bin
		// + a /usr/local/bin/cmake symlink; ninja and bwrap are in
		// /usr/bin (apt-installed); we cover both.
		EnvVars: map[string]string{
			"PATH": "/usr/local/bin:/usr/bin:/bin",
		},
		Timeout: 4 * time.Minute,
	})
	if err != nil {
		t.Fatalf("reapi.Build: %v", err)
	}

	ar, err := executor.Execute(ctx, store, built)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if ar.ExitCode != 0 {
		// Pull the stderr blob from CAS so the failure message is
		// self-explanatory; otherwise CI just shows the digest hash.
		stderr := "<no stderr digest reported>"
		if ar.StderrDigest != nil {
			if body, gerr := store.GetBlob(ctx, ar.StderrDigest); gerr == nil {
				stderr = string(body)
			} else {
				stderr = fmt.Sprintf("<fetch failed: %v>", gerr)
			}
		}
		t.Fatalf("convert-element-cmake ExitCode = %d, want 0\n  stderr digest: %v\n  stderr:\n%s",
			ar.ExitCode, ar.StderrDigest, stderr)
	}

	// The converter's canonical outputs: BUILD.bazel at root, cmake-config/
	// bundle as a directory. Assert both are present in the ActionResult.
	wantOutputs := map[string]bool{"BUILD.bazel": true, "cmake-config": true}
	for _, f := range ar.OutputFiles {
		delete(wantOutputs, f.Path)
	}
	for _, d := range ar.OutputDirectories {
		delete(wantOutputs, d.Path)
	}
	if len(wantOutputs) > 0 {
		missing := make([]string, 0, len(wantOutputs))
		for p := range wantOutputs {
			missing = append(missing, p)
		}
		t.Errorf("ActionResult missing outputs: %v\n  files=%+v\n  dirs=%+v", missing, ar.OutputFiles, ar.OutputDirectories)
	}

	// Spot-check BUILD.bazel content: hello-world's converted package
	// should at minimum declare a target named "hello".
	for _, f := range ar.OutputFiles {
		if f.Path != "BUILD.bazel" {
			continue
		}
		body, err := store.GetBlob(ctx, f.Digest)
		if err != nil {
			t.Fatalf("fetch BUILD.bazel: %v", err)
		}
		if !strings.Contains(string(body), `name = "hello"`) {
			t.Errorf("BUILD.bazel missing hello target:\n%s", body)
		}
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return strings.TrimSpace(v)
	}
	return def
}

func probe(addr string) error {
	c, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		return err
	}
	return c.Close()
}
