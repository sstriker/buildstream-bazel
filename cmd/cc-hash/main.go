// cc-hash is the Bazel-time file-hashing tool the cc_hash rule invokes: it
// computes a cryptographic digest of an input file and writes a header that
// `#define`s the digest as a C string, so a consumer that #includes the
// header sees the same compile-time constant cmake's vtk_hash_source would
// have produced — without running cmake (or the converter) at build time.
//
// It is the Bazel-native replacement for the "hash a file into a generated
// header" cmake -P idiom (VTK's vtkHashSource.cmake). Lowering that idiom to
// this tool + the cc_hash rule means the converted project needs neither
// cmake nor the converter at build time, and — unlike --cmake-script-bake,
// which freezes the digest at convert time — the digest auto-refreshes when
// the input file changes, because the hash is recomputed by this action
// (the transition end-state for codegen; docs/research/codegen-idiom-coverage.md).
//
// Faithfulness contract: the emitted header is byte-for-byte what
// vtkHashSource.cmake writes —
//
//	#ifndef <NAME>
//	 #define <NAME> "<digest>"
//	#endif
//
// where <NAME> is --name verbatim and <digest> is the lowercase-hex digest of
// the input file's bytes under --algorithm, matching cmake's file(<ALGO> …).
//
//	cc-hash --input=<f> --name=<NAME> --algorithm=SHA256 --header-out=<h>
package main

import (
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"flag"
	"fmt"
	"hash"
	"os"
	"regexp"
	"strings"
)

// cIdentifierRe matches a valid C identifier — the form --name must take,
// since it's emitted verbatim as both the `#define` name and the
// include-guard macro.
var cIdentifierRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func main() {
	var (
		input     = flag.String("input", "", "path to the file to hash (required)")
		name      = flag.String("name", "", "C #define name for the digest (required)")
		algorithm = flag.String("algorithm", "MD5", "digest: MD5, SHA1, SHA224, SHA256, SHA384 or SHA512")
		headerOut = flag.String("header-out", "", "path to write the generated header (required)")
	)
	flag.Parse()

	if err := run(*input, *name, *algorithm, *headerOut); err != nil {
		fmt.Fprintf(os.Stderr, "cc-hash: %v\n", err)
		os.Exit(1)
	}
}

// newHasher returns the hash.Hash for a cmake file(<ALGO> …) algorithm name
// (case-insensitive, mirroring cmake's acceptance of the documented spellings).
func newHasher(algorithm string) (hash.Hash, error) {
	switch strings.ToUpper(algorithm) {
	case "MD5":
		return md5.New(), nil
	case "SHA1":
		return sha1.New(), nil
	case "SHA224":
		return sha256.New224(), nil
	case "SHA256":
		return sha256.New(), nil
	case "SHA384":
		return sha512.New384(), nil
	case "SHA512":
		return sha512.New(), nil
	default:
		return nil, fmt.Errorf("unsupported --algorithm %q (want MD5, SHA1, SHA224, SHA256, SHA384 or SHA512)", algorithm)
	}
}

func run(input, name, algorithm, headerOut string) error {
	if input == "" || name == "" || headerOut == "" {
		return fmt.Errorf("--input, --name and --header-out are all required")
	}
	if !cIdentifierRe.MatchString(name) {
		// name is injected verbatim as a C #define name and an include-guard
		// macro — reject anything that isn't a valid identifier so the failure
		// is a clear error here, not invalid generated C later (and so a
		// hostile value can't inject into the generated source).
		return fmt.Errorf("--name %q is not a valid C identifier ([A-Za-z_][A-Za-z0-9_]*)", name)
	}
	h, err := newHasher(algorithm)
	if err != nil {
		return err
	}

	data, err := os.ReadFile(input)
	if err != nil {
		return fmt.Errorf("read input: %w", err)
	}
	h.Write(data)
	digest := hex.EncodeToString(h.Sum(nil))

	// Byte-for-byte vtkHashSource.cmake's output: the leading space before
	// `#define` and the trailing newline are reproduced so a faithfulness
	// diff against the cmake-generated header is empty.
	header := fmt.Sprintf("#ifndef %s\n #define %s \"%s\"\n#endif\n", name, name, digest)
	if err := os.WriteFile(headerOut, []byte(header), 0o644); err != nil {
		return fmt.Errorf("write header: %w", err)
	}
	return nil
}
