// orchestrate walks a BuildStream-style element tree, runs convert-element
// on every kind:cmake element in dependency-first order, and stages the
// outputs under <out>/.
//
// M5: per-element conversions are wrapped in a real REAPI Action +
// ActionCache flow. --cas selects the cache substrate:
//
//	--cas=local:<path>    in-process filesystem CAS+AC (default; offline).
//	--cas=grpc://host:port  REAPI gRPC endpoint; --cas-* flags configure
//	                      TLS / token credentials.
//
// Independent orchestrator instances pointed at the same gRPC CAS share
// cache hits via standard ActionCache lookups. M3b will plug remote
// execution into the same Action shapes; M5 itself still runs the
// converter locally.
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/sstriker/cmake-to-bazel/internal/cas"
	"github.com/sstriker/cmake-to-bazel/internal/reapi"
	"github.com/sstriker/cmake-to-bazel/orchestrator/internal/element"
	"github.com/sstriker/cmake-to-bazel/orchestrator/internal/orchestrator"
)

func main() {
	fs := flag.NewFlagSet("orchestrate", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)

	var (
		fdsdkRoot              = fs.String("fdsdk-root", "", "path to BuildStream root containing elements/")
		elementsDir            = fs.String("elements-dir", "elements", "subdirectory under --fdsdk-root holding .bst files")
		out                    = fs.String("out", "out", "output root for converted elements + manifest")
		sourcesBase            = fs.String("sources-base", "", "directory containing pre-staged source trees per element name (overrides per-element kind:local sources)")
		converterBinary        = fs.String("converter", "convert-element", "convert-element binary path or PATH name")
		casFlag                = fs.String("cas", "", "cache substrate: local:<path> | grpc://host:port | grpcs://host:port (default: local:<out>/cache)")
		casInstance            = fs.String("cas-instance", "", "REAPI instance_name (gRPC mode only)")
		casCert                = fs.String("cas-tls-cert", "", "client certificate file for mTLS (gRPC mode only)")
		casKey                 = fs.String("cas-tls-key", "", "client private key file for mTLS (gRPC mode only)")
		casCA                  = fs.String("cas-ca", "", "trust-root CA bundle (gRPC mode only)")
		casToken               = fs.String("cas-token-file", "", "file containing a bearer token (gRPC mode only)")
		remoteExec             = fs.String("execute", "", "remote execution endpoint: grpc://host:port | grpcs://host:port. when set, conversions submit a REAPI Action instead of forking convert-element locally")
		remoteExecInst         = fs.String("execute-instance", "", "REAPI Execute instance_name; defaults to --cas-instance")
		concurrency            = fs.Int("concurrency", 0, "max in-flight per-element conversions (<=0 = NumCPU). Topology is preserved; deps still land before dependents.")
		sourceCAS              = fs.String("source-cas", "", "REAPI Remote Asset endpoint for kind:remote-asset sources (e.g. grpc://host:port). Reuses --cas-* TLS / token plumbing.")
		sourceCASInst          = fs.String("source-cas-instance", "", "Remote Asset instance_name; defaults to --cas-instance")
		platformFlag           = fs.String("platform", "", "Action.platform overrides as comma-separated name=value (e.g. cmake-version=3.30.0,ninja-version=1.12.0). Overrides built-in defaults; the orchestrator MUST agree with workers on platform values to share cache hits.")
		elemTimeout            = fs.Duration("element-timeout", 0, "per-element pipeline cap (e.g. 30m, 2h). Zero = orchestrator default (30m). Mirrored into Action.timeout for remote workers.")
		toolchainCMake         = fs.String("toolchain-cmake-file", "", "CMake toolchain file (typically derive-toolchain's toolchain.cmake) passed to every per-element converter invocation. Skips cmake's compiler-detection probe — measurable per-conversion latency win.")
		platformsJSON          = fs.String("platforms-json", "", "JSON manifest declaring the target platform matrix to fan out per-element conversions across. Each entry pairs a Bazel platform name with its constraint_value labels and REAPI Action.Platform properties; with N>1 entries every kind:cmake element renders one unified BUILD.bazel with `select()` blocks for per-platform divergence. Empty (default) keeps the single-platform behaviour driven by --platform.")
		bazelBuild             = fs.String("bazel-build", "", "after a successful conversion, run `bazel build <target>` inside --out (e.g. //... or //elements/components/foo:bar). Requires bazel or bazelisk on PATH.")
		collectToolchainSignal = fs.Bool("collect-toolchain-signal", false, "ask each element's converter to copy its cmake File API reply into <out>/elements/<name>/toolchain-signal/. Off by default. Capture-only today: the on-disk format is the foundation for unify-toolchains' future consumer (queued under ROADMAP.md Next as 'Element-signal consumption in the unifier'); enabling this now lets a project accumulate signal so the unifier folds it on first use.")
		logFormat              = fs.String("log-format", "text", "structured-log handler: text (human-readable, default) or json (one record per line for ingestion)")
	)

	if err := fs.Parse(os.Args[1:]); err != nil {
		os.Exit(64)
	}
	if *fdsdkRoot == "" {
		fmt.Fprintln(os.Stderr, "orchestrate: --fdsdk-root is required")
		fs.Usage()
		os.Exit(64)
	}

	proj, err := element.ReadProject(*fdsdkRoot, *elementsDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "orchestrate: %v\n", err)
		os.Exit(1)
	}
	g, err := element.BuildGraph(proj)
	if err != nil {
		fmt.Fprintf(os.Stderr, "orchestrate: %v\n", err)
		os.Exit(1)
	}

	ctx := context.Background()
	store, closer, err := openStore(ctx, *casFlag, *out, casOpts{
		Instance:    *casInstance,
		TLSCertFile: *casCert,
		TLSKeyFile:  *casKey,
		CAFile:      *casCA,
		TokenFile:   *casToken,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "orchestrate: cas: %v\n", err)
		os.Exit(1)
	}
	if closer != nil {
		defer closer()
	}

	var sourceAsset *cas.RemoteAsset
	if *sourceCAS != "" {
		instance := *sourceCASInst
		if instance == "" {
			instance = *casInstance
		}
		ra, err := cas.NewRemoteAsset(ctx, cas.RemoteAssetConfig{
			Endpoint:     *sourceCAS,
			InstanceName: instance,
			Insecure:     strings.HasPrefix(*sourceCAS, "grpc://"),
			TLSCertFile:  *casCert,
			TLSKeyFile:   *casKey,
			CAFile:       *casCA,
			TokenFile:    *casToken,
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "orchestrate: source-cas: %v\n", err)
			os.Exit(1)
		}
		defer ra.Close()
		sourceAsset = ra
	}

	var executor reapi.Executor
	if *remoteExec != "" {
		instance := *remoteExecInst
		if instance == "" {
			instance = *casInstance
		}
		ex, exCloser, err := openExecutor(*remoteExec, instance, casOpts{
			TLSCertFile: *casCert,
			TLSKeyFile:  *casKey,
			CAFile:      *casCA,
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "orchestrate: execute: %v\n", err)
			os.Exit(1)
		}
		defer exCloser()
		executor = ex
	}

	platform, err := parsePlatform(*platformFlag)
	if err != nil {
		fmt.Fprintf(os.Stderr, "orchestrate: --platform: %v\n", err)
		os.Exit(64)
	}

	logger, err := buildLogger(*logFormat)
	if err != nil {
		fmt.Fprintf(os.Stderr, "orchestrate: %v\n", err)
		os.Exit(64)
	}

	res, err := orchestrator.Run(ctx, orchestrator.Options{
		Project:                proj,
		Graph:                  g,
		Out:                    *out,
		SourcesBase:            *sourcesBase,
		ConverterBinary:        *converterBinary,
		Store:                  store,
		Executor:               executor,
		Concurrency:            *concurrency,
		SourceAsset:            sourceAsset,
		Platform:               platform,
		PerElementTimeout:      *elemTimeout,
		ToolchainCMakeFile:     *toolchainCMake,
		CollectToolchainSignal: *collectToolchainSignal,
		PlatformsJSON:          *platformsJSON,
		Logger:                 logger,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "orchestrate: %v\n", err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "converted %d, failed %d (cache hits %d, misses %d)\n",
		len(res.Converted), len(res.Failed), len(res.CacheHits), len(res.CacheMisses))
	if len(res.Failed) > 0 {
		os.Exit(2)
	}
	if *bazelBuild != "" {
		if err := runBazelBuild(ctx, *out, *bazelBuild, logger); err != nil {
			fmt.Fprintf(os.Stderr, "orchestrate: bazel build: %v\n", err)
			os.Exit(2)
		}
	}
}

// runBazelBuild invokes `bazel build <target>` inside out (the
// orchestrator's output dir, which now carries a self-contained
// MODULE.bazel + per-element packages). Streams bazel's own
// stdout/stderr through to the operator. Picks bazelisk when present
// so version pins from a .bazelversion or BAZEL_VERSION env var win.
func runBazelBuild(ctx context.Context, out, target string, logger *slog.Logger) error {
	bazel := lookupBazelBinary()
	if bazel == "" {
		return fmt.Errorf("bazel/bazelisk not on PATH")
	}
	logger.Info("bazel build", "binary", bazel, "target", target, "out", out)
	cmd := exec.CommandContext(ctx, bazel, "build", target)
	cmd.Dir = out
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func lookupBazelBinary() string {
	for _, name := range []string{"bazelisk", "bazel"} {
		if p, err := exec.LookPath(name); err == nil {
			return p
		}
	}
	return ""
}

// buildLogger returns a slog.Logger backed by the requested handler kind.
// Both handlers write to stderr; the orchestrator's per-element subprocess
// stdout/stderr capture stays on the same fd, so operators see log
// records and converter output interleaved in chronological order.
func buildLogger(format string) (*slog.Logger, error) {
	switch strings.ToLower(format) {
	case "text", "":
		return slog.New(slog.NewTextHandler(os.Stderr, nil)), nil
	case "json":
		return slog.New(slog.NewJSONHandler(os.Stderr, nil)), nil
	default:
		return nil, fmt.Errorf("--log-format must be text or json, got %q", format)
	}
}

type casOpts struct {
	Instance    string
	TLSCertFile string
	TLSKeyFile  string
	CAFile      string
	TokenFile   string
}

// openExecutor parses --execute and returns a REAPI Executor wired
// to its own gRPC connection. The returned closer must be called when
// orchestration is done. Returns an error on bad endpoint scheme or
// dial failure.
func openExecutor(endpoint, instance string, opts casOpts) (reapi.Executor, func(), error) {
	addr, scheme := splitEndpoint(endpoint)
	var dialOpts []grpc.DialOption
	switch scheme {
	case "grpc":
		dialOpts = append(dialOpts, grpc.WithTransportCredentials(insecure.NewCredentials()))
	case "grpcs":
		tc, err := buildExecuteTLS(opts)
		if err != nil {
			return nil, nil, fmt.Errorf("execute tls: %w", err)
		}
		dialOpts = append(dialOpts, grpc.WithTransportCredentials(credentials.NewTLS(tc)))
	default:
		return nil, nil, fmt.Errorf("--execute: unknown scheme %q (want grpc:// or grpcs://)", endpoint)
	}
	conn, err := grpc.NewClient(addr, dialOpts...)
	if err != nil {
		return nil, nil, fmt.Errorf("execute dial %s: %w", addr, err)
	}
	return reapi.NewGRPCExecutor(conn, instance), func() { _ = conn.Close() }, nil
}

func splitEndpoint(raw string) (string, string) {
	switch {
	case strings.HasPrefix(raw, "grpc://"):
		return strings.TrimPrefix(raw, "grpc://"), "grpc"
	case strings.HasPrefix(raw, "grpcs://"):
		return strings.TrimPrefix(raw, "grpcs://"), "grpcs"
	default:
		return raw, ""
	}
}

// buildExecuteTLS mirrors cas.GRPCStore's TLS plumbing for the
// Executor's separate connection. Reading the same flags twice keeps
// the surface symmetric: same cert/key/ca apply to both transports.
func buildExecuteTLS(opts casOpts) (*tls.Config, error) {
	tc := &tls.Config{}
	if opts.CAFile != "" {
		pem, err := os.ReadFile(opts.CAFile)
		if err != nil {
			return nil, err
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("ca %s: no certs parsed", opts.CAFile)
		}
		tc.RootCAs = pool
	}
	if opts.TLSCertFile != "" {
		cert, err := tls.LoadX509KeyPair(opts.TLSCertFile, opts.TLSKeyFile)
		if err != nil {
			return nil, err
		}
		tc.Certificates = []tls.Certificate{cert}
	}
	return tc, nil
}

// parsePlatform parses a --platform "name=value,name=value" string
// into a slice of PlatformProperty, suitable for Options.Platform. An
// empty string returns nil so the orchestrator falls back to
// defaultPlatform. Whitespace around names and values is trimmed.
func parsePlatform(raw string) ([]reapi.PlatformProperty, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	pairs := strings.Split(raw, ",")
	out := make([]reapi.PlatformProperty, 0, len(pairs))
	for _, pair := range pairs {
		kv := strings.SplitN(pair, "=", 2)
		if len(kv) != 2 {
			return nil, fmt.Errorf("expected name=value, got %q", pair)
		}
		name := strings.TrimSpace(kv[0])
		value := strings.TrimSpace(kv[1])
		if name == "" {
			return nil, fmt.Errorf("empty platform property name in %q", pair)
		}
		out = append(out, reapi.PlatformProperty{Name: name, Value: value})
	}
	return out, nil
}

// openStore parses the --cas flag and returns a Store ready for the
// orchestrator. Returns a non-nil closer for gRPC stores; LocalStore
// has no resources to release.
func openStore(ctx context.Context, casFlag, out string, opts casOpts) (cas.Store, func(), error) {
	if casFlag == "" {
		casFlag = "local:" + out + "/cache"
	}
	switch {
	case strings.HasPrefix(casFlag, "local:"):
		path := strings.TrimPrefix(casFlag, "local:")
		s, err := cas.NewLocalStore(path)
		if err != nil {
			return nil, nil, err
		}
		return s, nil, nil
	case strings.HasPrefix(casFlag, "grpc://"), strings.HasPrefix(casFlag, "grpcs://"):
		s, err := cas.NewGRPCStore(ctx, cas.GRPCConfig{
			Endpoint:     casFlag,
			InstanceName: opts.Instance,
			TLSCertFile:  opts.TLSCertFile,
			TLSKeyFile:   opts.TLSKeyFile,
			CAFile:       opts.CAFile,
			TokenFile:    opts.TokenFile,
		})
		if err != nil {
			return nil, nil, err
		}
		return s, func() { _ = s.Close() }, nil
	default:
		return nil, nil, fmt.Errorf("unknown --cas scheme %q (want local:<path> | grpc://... | grpcs://...)", casFlag)
	}
}
