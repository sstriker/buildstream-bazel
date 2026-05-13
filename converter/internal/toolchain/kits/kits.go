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
// Schema source:
// https://github.com/microsoft/vscode-cmake-tools/blob/main/docs/kits.md
//
// Field decoding focuses on the three items that affect cmake
// configure invocations and lift cleanly into Variant.CacheVars:
// compilers (C/CXX), toolchainFile, and cmakeSettings. The
// schema's `environmentVariables` map is NOT consumed today:
// cmakerun.Configure builds a fixed env for determinism, and
// Variant has no env-var carrier. Wiring kit env vars through to
// cmake invocations is a future change that would require a
// Variant.Env field plus cmakerun.Options.Env merge plumbing —
// out of scope here. Display-only fields (name aliases, isTrusted,
// etc.) are also ignored.
package kits

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"

	"github.com/sstriker/buildstream-bazel/converter/internal/toolchain"
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
	// seen maps a sanitized name back to the FIRST original kit
	// name that produced it, so a collision error can name both
	// kits unambiguously. Distinct kit names like "GCC 13" and
	// "GCC-13" both sanitize to "gcc-13"; downstream uses
	// Variant.Name as a map key (Observe's
	// ResolvedToolchain.Variants) and a Bazel target name —
	// silent overwrite would lose data and produce duplicate
	// targets. Reject up front.
	seen := map[string]string{}
	for i, k := range kits {
		if k.Name == "" {
			return nil, fmt.Errorf("kits: kit at index %d has empty name", i)
		}
		sanitized := sanitizeKitName(k.Name)
		if sanitized == "" {
			return nil, fmt.Errorf("kits: kit %q at index %d sanitizes to empty Variant.Name (only non-alphanumeric runs?)", k.Name, i)
		}
		if prev, dup := seen[sanitized]; dup {
			return nil, fmt.Errorf("kits: kits %q and %q both sanitize to %q; rename one so they produce distinct Variant.Name values", prev, k.Name, sanitized)
		}
		seen[sanitized] = k.Name
		v := toolchain.Variant{Name: sanitized}
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

// kit is one entry in the kits.json array. environmentVariables
// is intentionally NOT decoded — we don't propagate env vars to
// cmakerun today (see package doc); decoding it would imply
// support that doesn't exist.
type kit struct {
	Name          string            `json:"name"`
	Compilers     map[string]string `json:"compilers"`
	ToolchainFile string            `json:"toolchainFile"`
	CmakeSettings map[string]any    `json:"cmakeSettings"`
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
		// without trailing zeros when they fit in int64. Out-of-
		// range floats fall through to %g; the float→int64 cast
		// itself is implementation-defined for values past
		// math.MaxInt64 / math.MinInt64, so the bounds check
		// guards against silent corruption.
		if x >= math.MinInt64 && x <= math.MaxInt64 && x == float64(int64(x)) {
			return strconv.FormatInt(int64(x), 10), nil
		}
		return strconv.FormatFloat(x, 'g', -1, 64), nil
	case nil:
		return "", nil
	default:
		return "", fmt.Errorf("unsupported cmakeSettings value type %T", v)
	}
}

// sanitizeKitName lowercases and substitutes runs of characters
// outside [a-z0-9_] with a single hyphen so the Variant name is
// filesystem- and Bazel-label-safe. Underscores survive verbatim
// because cmake / Bazel both accept them.
//
//	"Clang 15 (x86_64)" → "clang-15-x86_64"
//	"GCC-13!"           → "gcc-13"
//	"  spaces  "        → "spaces"
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
