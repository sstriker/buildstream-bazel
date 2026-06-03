// cc-embed is the Bazel-time file-embedding tool the cc_embed rule
// invokes: it turns an arbitrary input file into a C source + header that
// expose its bytes as a named symbol, so the data links into the binary
// instead of being read from the filesystem at runtime.
//
// It is the Bazel-native replacement for the common "embed a file as a C
// array" cmake -P idiom (VTK's vtkEncodeString.cmake — 702 call sites in
// VTK alone; the same pattern recurs across LLVM / Qt / game engines).
// Lowering that idiom to this tool + the cc_embed rule means the converted
// project needs neither cmake nor the converter at build time — the
// transition end-state for codegen (docs/research/codegen-idiom-coverage.md).
//
// Faithfulness contract: the emitted symbol NAME is taken verbatim from
// --name (so consumers that #include the header and reference the symbol
// resolve unchanged), and the symbol's runtime value equals the input
// file's bytes. The exact generated-source FORMATTING need not match any
// particular cmake encoder byte-for-byte — only the symbol set (nm) and
// the runtime value are load-bearing.
//
// Two modes mirror vtkEncodeString's:
//
//	# string mode (default) — `const char *NAME = "...";` (for text:
//	# shaders, templates). Newlines/quotes/backslashes are escaped.
//	cc-embed --input=<f> --name=<sym> --header-out=<h> --source-out=<c>
//
//	# binary mode — `const unsigned char NAME[N] = {0x..,...};` (for any
//	# bytes; --nul-terminate appends a trailing NUL, e.g. to embed a file
//	# as a C string that exceeds compiler string-literal limits).
//	cc-embed --binary [--nul-terminate] --input=<f> --name=<sym> ...
//
// Optional --export-symbol / --export-header mirror vtk_encode_string's
// EXPORT_SYMBOL / EXPORT_HEADER (a visibility macro on the declaration +
// the header that provides it).
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// cIdentifierRe matches a valid C identifier (the form --name must take,
// since it's emitted verbatim as both a symbol and an include-guard macro).
var cIdentifierRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func main() {
	var (
		input        = flag.String("input", "", "path to the file to embed (required)")
		name         = flag.String("name", "", "C symbol name for the embedded data (required)")
		headerOut    = flag.String("header-out", "", "path to write the generated header (required)")
		sourceOut    = flag.String("source-out", "", "path to write the generated source (required)")
		binary       = flag.Bool("binary", false, "emit an unsigned char[] byte array instead of a C string")
		nulTerminate = flag.Bool("nul-terminate", false, "append a trailing NUL byte (binary mode only)")
		exportSymbol = flag.String("export-symbol", "", "visibility/export macro placed before the declaration")
		exportHeader = flag.String("export-header", "", "header included for the export macro")
	)
	flag.Parse()

	if err := run(*input, *name, *headerOut, *sourceOut, *binary, *nulTerminate, *exportSymbol, *exportHeader); err != nil {
		fmt.Fprintf(os.Stderr, "cc-embed: %v\n", err)
		os.Exit(1)
	}
}

func run(input, name, headerOut, sourceOut string, binary, nulTerminate bool, exportSymbol, exportHeader string) error {
	if input == "" || name == "" || headerOut == "" || sourceOut == "" {
		return fmt.Errorf("--input, --name, --header-out and --source-out are all required")
	}
	if !cIdentifierRe.MatchString(name) {
		// name is injected verbatim as a C identifier and an include-guard
		// macro fragment — reject anything that isn't a valid identifier so
		// the failure is a clear error here, not invalid generated C later
		// (and so a hostile value can't inject into the generated source).
		return fmt.Errorf("--name %q is not a valid C identifier ([A-Za-z_][A-Za-z0-9_]*)", name)
	}
	if nulTerminate && !binary {
		return fmt.Errorf("--nul-terminate only makes sense with --binary")
	}
	if (exportSymbol == "") != (exportHeader == "") {
		return fmt.Errorf("--export-symbol and --export-header must be set together")
	}
	// The generated source self-includes the header by its actual basename
	// (not "<name>.h"), so the symbol name and the output filename are
	// decoupled — any out_header name compiles.
	headerInclude := filepath.Base(headerOut)
	// These three land verbatim inside `#include "..."` / before `extern`.
	// They're trusted (BUILD-file) inputs, but reject the characters that
	// would produce invalid C or inject extra lines, so misuse fails here
	// with a clear message rather than as an obscure compiler error.
	for _, v := range []struct {
		what, val, bad string
	}{
		{"--header-out basename", headerInclude, "\"\n\r"},
		{"--export-header", exportHeader, "\"\n\r"},
		{"--export-symbol", exportSymbol, "\n\r"},
	} {
		if strings.ContainsAny(v.val, v.bad) {
			return fmt.Errorf("%s %q contains a character that would break the generated source", v.what, v.val)
		}
	}

	data, err := os.ReadFile(input)
	if err != nil {
		return fmt.Errorf("read input: %w", err)
	}
	if binary && len(data) == 0 && !nulTerminate {
		// An empty input in binary mode would emit `const unsigned char x[0]`,
		// which is non-standard C++ (zero-length array); reject it with a clear
		// message. --nul-terminate makes it a 1-element array, and string mode
		// handles empty input fine (`const char *x = ""`).
		return fmt.Errorf("--binary on an empty input would emit a zero-length array (non-standard C++); use --nul-terminate or a non-empty input")
	}

	header, source := encode(data, name, headerInclude, binary, nulTerminate, exportSymbol, exportHeader)

	if err := os.WriteFile(headerOut, []byte(header), 0o644); err != nil {
		return fmt.Errorf("write header: %w", err)
	}
	if err := os.WriteFile(sourceOut, []byte(source), 0o644); err != nil {
		return fmt.Errorf("write source: %w", err)
	}
	return nil
}

// encode renders the header + source for the embedded data. The symbol
// name and the data's value are the load-bearing contract; the formatting
// is the tool's own (valid C, deterministic). headerInclude is the path
// the source `#include`s (the header's basename) — kept separate from the
// symbol so the output filename isn't constrained by the symbol name.
func encode(data []byte, name, headerInclude string, binary, nulTerminate bool, exportSymbol, exportHeader string) (header, source string) {
	var h strings.Builder
	guard := name + "_h"
	if strings.HasPrefix(guard, "_") {
		// A guard starting with "_" is in C/C++'s implementation-reserved
		// identifier space (leading underscore at file scope); prefix it so
		// it never begins with an underscore. Guards for normal (non-leading-
		// underscore) names are unchanged.
		guard = "CCEMBED" + guard
	}
	fmt.Fprintf(&h, "#ifndef %s\n#define %s\n\n", guard, guard)
	if exportHeader != "" {
		fmt.Fprintf(&h, "#include \"%s\"\n\n", exportHeader)
	}

	var s strings.Builder
	fmt.Fprintf(&s, "#include \"%s\"\n\n", headerInclude)

	decl := ""
	if exportSymbol != "" {
		decl = exportSymbol + " "
	}

	if binary {
		bytes := data
		if nulTerminate {
			bytes = append(append([]byte(nil), data...), 0)
		}
		fmt.Fprintf(&h, "%sextern const unsigned char %s[%d];\n\n#endif\n", decl, name, len(bytes))
		fmt.Fprintf(&s, "const unsigned char %s[%d] = {\n%s\n};\n", name, len(bytes), formatByteArray(bytes))
		return h.String(), s.String()
	}

	fmt.Fprintf(&h, "%sextern const char *%s;\n\n#endif\n", decl, name)
	fmt.Fprintf(&s, "const char *%s =\n\"%s\";\n", name, escapeCString(data))
	return h.String(), s.String()
}

// escapeCString escapes data into the body of a C string literal whose
// runtime value equals data. Backslash and double-quote are escaped;
// newlines become an escaped \n plus a physical line break with a
// re-opened string literal (concatenated adjacent literals — keeps the
// generated source readable and within line-length sanity); carriage
// returns and tabs get their C escapes. Every other non-printable-ASCII
// byte — control chars < 0x20 and the 0x7f-0xff range — is emitted as a
// fixed-width 3-digit octal escape (\NNN): control bytes so the generated
// .cxx is valid C (a raw NUL/control byte would corrupt it), and high
// bytes so the runtime value is byte-for-byte faithful regardless of the
// compiler's source/execution charset (a raw >= 0x80 byte is otherwise
// subject to locale-dependent re-encoding). Only printable ASCII passes
// through. (A NUL in string mode still truncates the C-string runtime
// value — use --binary to embed arbitrary bytes including NULs.)
func escapeCString(data []byte) string {
	var b strings.Builder
	for _, c := range data {
		switch c {
		case '\\':
			b.WriteString(`\\`)
		case '"':
			b.WriteString(`\"`)
		case '\n':
			b.WriteString("\\n\"\n\"")
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			// Escape every non-printable-ASCII byte as fixed 3-digit octal:
			// control bytes (< 0x20) AND 0x7f-0xff. The high range matters for
			// byte-for-byte faithfulness — a raw >= 0x80 byte in the source is
			// subject to the compiler's source/execution charset (locale-
			// dependent re-encoding); \NNN pins the exact byte regardless of
			// toolchain. Only printable ASCII (0x20-0x7e) passes through.
			// Fixed 3 octal digits: a C octal escape consumes at most 3
			// digits, so \NNN never swallows a following literal digit.
			if c < 0x20 || c >= 0x7f {
				fmt.Fprintf(&b, "\\%03o", c)
			} else {
				b.WriteByte(c)
			}
		}
	}
	return b.String()
}

// formatByteArray renders bytes as comma-separated 0xNN hex literals,
// wrapped at a fixed column count for readable output.
func formatByteArray(data []byte) string {
	const perLine = 16
	var b strings.Builder
	for i, c := range data {
		if i > 0 {
			b.WriteByte(',')
			if i%perLine == 0 {
				b.WriteByte('\n')
			}
		}
		fmt.Fprintf(&b, "0x%02x", c)
	}
	return b.String()
}
