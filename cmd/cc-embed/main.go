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
	"strings"
)

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
	if nulTerminate && !binary {
		return fmt.Errorf("--nul-terminate only makes sense with --binary")
	}
	data, err := os.ReadFile(input)
	if err != nil {
		return fmt.Errorf("read input: %w", err)
	}

	header, source := encode(data, name, binary, nulTerminate, exportSymbol, exportHeader)

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
// is the tool's own (valid C, deterministic).
func encode(data []byte, name string, binary, nulTerminate bool, exportSymbol, exportHeader string) (header, source string) {
	var h strings.Builder
	guard := name + "_h"
	fmt.Fprintf(&h, "#ifndef %s\n#define %s\n\n", guard, guard)
	if exportHeader != "" {
		fmt.Fprintf(&h, "#include \"%s\"\n\n", exportHeader)
	}

	var s strings.Builder
	fmt.Fprintf(&s, "#include \"%s.h\"\n\n", name)

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
// returns and tabs get their C escapes; other bytes pass through (text
// input — use --binary for arbitrary bytes / NULs).
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
			b.WriteByte(c)
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
