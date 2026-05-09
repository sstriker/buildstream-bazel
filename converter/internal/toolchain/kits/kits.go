// Package kits parses VSCode CMake-Tools cmake-kits.json and lifts
// each kit into a toolchain.Variant. Kits describe (compiler driver,
// optional toolchain file, optional env vars) tuples — the compiler
// axis of the variant matrix.
//
// Why kits are a Variant source: VSCode users already maintain
// cmake-kits.json as the registry of compilers/cross-toolchains
// available on their machine. Reading it directly avoids re-
// declaring the compiler axis in our discovery layer. Combined
// with `presets` (build-type axis), the cross-product at
// VariantMatrix produces the full probe matrix without extra
// operator effort.
//
// Schema source: https://github.com/microsoft/vscode-cmake-tools
// /blob/main/docs/kits.md . Field decoding focuses on the four
// items that affect cmake configure invocations: compilers (C/CXX),
// toolchainFile, cmakeSettings, environmentVariables. Display-only
// fields (name aliases, isTrusted, etc.) are ignored.
package kits

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/sstriker/cmake-to-bazel/converter/internal/toolchain"
)

// LoadFile reads cmake-kits.json and returns one Variant per kit.
// Missing file → (nil, nil) so callers can call LoadFile
// unconditionally.
func LoadFile(path string) ([]toolchain.Variant, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("kits: read %s: %w", path, err)
	}
	return Parse(body)
}

// Parse decodes raw JSON into []Variant.
func Parse(body []byte) ([]toolchain.Variant, error) {
	var kits []kit
	if err := json.Unmarshal(body, &kits); err != nil {
		return nil, fmt.Errorf("kits: parse: %w", err)
	}
	out := make([]toolchain.Variant, 0, len(kits))
	for i, k := range kits {
		if k.Name == "" {
			return nil, fmt.Errorf("kits: kit at index %d has empty name", i)
		}
		v := toolchain.Variant{Name: sanitizeKitName(k.Name)}
		cache := map[string]string{}
		if c := k.Compilers["C"]; c != "" {
			cache["CMAKE_C_COMPILER"] = c
		}
		if cxx := k.Compilers["CXX"]; cxx != "" {
			cache["CMAKE_CXX_COMPILER"] = cxx
		}
		if k.ToolchainFile != "" {
			cache["CMAKE_TOOLCHAIN_FILE"] = k.ToolchainFile
		}
		// cmakeSettings flow into cacheVariables verbatim — they're
		// the kit's project-cmake -D pass-throughs.
		for ck, cv := range k.CmakeSettings {
			s, err := stringify(cv)
			if err != nil {
				return nil, fmt.Errorf("kits: %q: cmakeSettings.%s: %w", k.Name, ck, err)
			}
			cache[ck] = s
		}
		if len(cache) > 0 {
			v.CacheVars = cache
		}
		out = append(out, v)
	}
	return out, nil
}

// kit is one entry in the kits.json array.
type kit struct {
	Name            string            `json:"name"`
	Compilers       map[string]string `json:"compilers"`
	ToolchainFile   string            `json:"toolchainFile"`
	CmakeSettings   map[string]any    `json:"cmakeSettings"`
	EnvironmentVars map[string]string `json:"environmentVariables"`
}

// stringify coerces a JSON-decoded cmakeSettings value to a string.
// String, bool, and number are all valid cmake -D RHS shapes.
func stringify(v any) (string, error) {
	switch x := v.(type) {
	case string:
		return x, nil
	case bool:
		if x {
			return "ON", nil
		}
		return "OFF", nil
	case float64:
		// JSON numbers come through as float64 — render integers
		// without trailing zeros.
		if x == float64(int64(x)) {
			return fmt.Sprintf("%d", int64(x)), nil
		}
		return fmt.Sprintf("%g", x), nil
	case nil:
		return "", nil
	default:
		return "", fmt.Errorf("unsupported cmakeSettings value type %T", v)
	}
}

// sanitizeKitName lowercases and substitutes non-alphanumeric runs
// with hyphens so the Variant name is filesystem- and Bazel-label-
// safe. "Clang 15 (x86_64)" → "clang-15-x86-64".
func sanitizeKitName(name string) string {
	var b strings.Builder
	prevDash := true // suppress leading dash
	for _, r := range strings.ToLower(name) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			prevDash = false
		case r == '_':
			b.WriteRune(r)
			prevDash = false
		default:
			if !prevDash {
				b.WriteRune('-')
				prevDash = true
			}
		}
	}
	out := b.String()
	return strings.TrimRight(out, "-")
}
