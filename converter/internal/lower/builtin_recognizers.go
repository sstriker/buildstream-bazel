package lower

import (
	"embed"
	"fmt"
	"sort"
	"strings"
	"sync"
)

// builtinRecognizerFS holds the first-party codegen recognizers as embedded
// Starlark — the same .star form an operator writes, so there is ONE recognizer
// model. They're shipped as data (overridable by name via --recognizers) rather
// than compiled Go, so adding/teaching a generator is editing a .star, not the
// converter binary.
//
//go:embed builtinrecognizers/*.star
var builtinRecognizerFS embed.FS

var (
	builtinRecognizersOnce sync.Once
	builtinRecognizersList []CodegenRecognizer
)

// builtinRecognizers compiles the embedded built-in recognizers once. A compile
// error is a first-party bug (these ship in the binary) and panics — surfaced by
// TestBuiltinRecognizers_Compile in CI. Loaded in filename order; the built-in
// matches are mutually exclusive (protoc excludes --grpc_out, grpc_only excludes
// --cpp_out, grpc_cpp needs both), so order is immaterial.
func builtinRecognizers() []CodegenRecognizer {
	builtinRecognizersOnce.Do(func() {
		const dir = "builtinrecognizers"
		entries, err := builtinRecognizerFS.ReadDir(dir)
		if err != nil {
			panic(fmt.Sprintf("lower: reading embedded recognizers: %v", err))
		}
		var names []string
		for _, e := range entries {
			if !e.IsDir() && strings.HasSuffix(e.Name(), ".star") {
				names = append(names, e.Name())
			}
		}
		sort.Strings(names)
		for _, n := range names {
			src, err := builtinRecognizerFS.ReadFile(dir + "/" + n)
			if err != nil {
				panic(fmt.Sprintf("lower: reading embedded recognizer %q: %v", n, err))
			}
			// Compile with the bare filename as path so Name() is "starlark:<base>"
			// (e.g. "starlark:protoc") — the key an operator --recognizers file of
			// the same name shadows.
			r, err := compileStarlarkRecognizer(n, src)
			if err != nil {
				panic(fmt.Sprintf("lower: built-in recognizer %q failed to compile: %v", n, err))
			}
			builtinRecognizersList = append(builtinRecognizersList, r)
		}
	})
	return builtinRecognizersList
}
