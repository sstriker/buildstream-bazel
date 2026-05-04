// srckey-registry is the CLI wrapper around
// internal/srckeyregistry. Used by post-build wrapper scripts
// to register trace + make-db artifacts under their element's
// srckey, and by write-a (in PR4) to look up cache hits at
// render time.
//
// Subcommands:
//
//	srckey-registry register --dir=<reg> --srckey=<K> --name=<N> --file=<path>
//	srckey-registry lookup   --dir=<reg> --srckey=<K> --name=<N> --out=<path>
//	srckey-registry has      --dir=<reg> --srckey=<K> --name=<N>
//
// Exit codes:
//
//	0 — success (register stored; lookup hit; has => present).
//	1 — I/O error or bad arguments.
//	2 — lookup miss / has => not present (distinct from error so
//	     callers can shell-conditional on the cache state).
package main

import (
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/sstriker/cmake-to-bazel/internal/srckeyregistry"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(1)
	}
	switch os.Args[1] {
	case "register":
		os.Exit(runRegister(os.Args[2:]))
	case "lookup":
		os.Exit(runLookup(os.Args[2:]))
	case "has":
		os.Exit(runHas(os.Args[2:]))
	case "-h", "--help", "help":
		usage()
		os.Exit(0)
	default:
		fmt.Fprintf(os.Stderr, "srckey-registry: unknown subcommand %q\n", os.Args[1])
		usage()
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: srckey-registry <subcommand> [flags]")
	fmt.Fprintln(os.Stderr, "  register --dir=<reg> --srckey=<K> --name=<N> --file=<path>")
	fmt.Fprintln(os.Stderr, "  lookup   --dir=<reg> --srckey=<K> --name=<N> --out=<path>")
	fmt.Fprintln(os.Stderr, "  has      --dir=<reg> --srckey=<K> --name=<N>")
}

func runRegister(args []string) int {
	fs := flag.NewFlagSet("register", flag.ContinueOnError)
	dir := fs.String("dir", "", "registry root directory")
	srckey := fs.String("srckey", "", "element srckey (hex sha256 of breakdown)")
	name := fs.String("name", "", "artifact name (e.g. trace.log)")
	file := fs.String("file", "", "path to the artifact file to register")
	if err := fs.Parse(args); err != nil {
		return 1
	}
	if *dir == "" || *srckey == "" || *name == "" || *file == "" {
		fmt.Fprintln(os.Stderr, "register: --dir, --srckey, --name, --file are all required")
		return 1
	}
	r, err := srckeyregistry.New(*dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "register: %v\n", err)
		return 1
	}
	body, err := os.ReadFile(*file)
	if err != nil {
		fmt.Fprintf(os.Stderr, "register: read %s: %v\n", *file, err)
		return 1
	}
	if err := r.Register(*srckey, *name, body); err != nil {
		fmt.Fprintf(os.Stderr, "register: %v\n", err)
		return 1
	}
	return 0
}

func runLookup(args []string) int {
	fs := flag.NewFlagSet("lookup", flag.ContinueOnError)
	dir := fs.String("dir", "", "registry root directory")
	srckey := fs.String("srckey", "", "element srckey")
	name := fs.String("name", "", "artifact name")
	out := fs.String("out", "", "path to write the artifact bytes (default: stdout)")
	if err := fs.Parse(args); err != nil {
		return 1
	}
	if *dir == "" || *srckey == "" || *name == "" {
		fmt.Fprintln(os.Stderr, "lookup: --dir, --srckey, --name are all required")
		return 1
	}
	r, err := srckeyregistry.New(*dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "lookup: %v\n", err)
		return 1
	}
	body, ok, err := r.Lookup(*srckey, *name)
	if err != nil {
		fmt.Fprintf(os.Stderr, "lookup: %v\n", err)
		return 1
	}
	if !ok {
		// Miss is exit code 2 — callers shell-conditional on
		// it. Empty stdout to avoid feeding garbage downstream.
		return 2
	}
	if *out == "" {
		_, _ = io.Copy(os.Stdout, asReader(body))
		return 0
	}
	if err := os.WriteFile(*out, body, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "lookup: write %s: %v\n", *out, err)
		return 1
	}
	return 0
}

func runHas(args []string) int {
	fs := flag.NewFlagSet("has", flag.ContinueOnError)
	dir := fs.String("dir", "", "registry root directory")
	srckey := fs.String("srckey", "", "element srckey")
	name := fs.String("name", "", "artifact name")
	if err := fs.Parse(args); err != nil {
		return 1
	}
	if *dir == "" || *srckey == "" || *name == "" {
		fmt.Fprintln(os.Stderr, "has: --dir, --srckey, --name are all required")
		return 1
	}
	r, err := srckeyregistry.New(*dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "has: %v\n", err)
		return 1
	}
	present, err := r.Has(*srckey, *name)
	if err != nil {
		fmt.Fprintf(os.Stderr, "has: %v\n", err)
		return 1
	}
	if !present {
		return 2
	}
	return 0
}

// asReader wraps a []byte in a minimal io.Reader; trims the
// io/bytes import dance.
type byteReader struct {
	b []byte
	n int
}

func (r *byteReader) Read(p []byte) (int, error) {
	if r.n >= len(r.b) {
		return 0, io.EOF
	}
	n := copy(p, r.b[r.n:])
	r.n += n
	return n, nil
}

func asReader(b []byte) io.Reader { return &byteReader{b: b} }
