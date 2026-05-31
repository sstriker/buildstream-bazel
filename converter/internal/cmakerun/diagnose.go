package cmakerun

import (
	"fmt"
	"io"
	"strings"
)

// boundedBuffer is an io.Writer that retains at most `limit`
// bytes of trailing output. Once the buffer is full, the
// oldest bytes are discarded as new ones come in. Used to
// keep cmake's stderr tail around for post-mortem pattern
// detection without ballooning memory for projects whose
// configure step emits megabytes of progress noise.
type boundedBuffer struct {
	buf   []byte
	limit int
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	b.buf = append(b.buf, p...)
	if extra := len(b.buf) - b.limit; extra > 0 {
		b.buf = b.buf[extra:]
	}
	return len(p), nil
}

func (b *boundedBuffer) Bytes() []byte { return b.buf }

// Compile-time confirmation that boundedBuffer satisfies io.Writer.
var _ io.Writer = (*boundedBuffer)(nil)

// annotateConfigureFailure wraps cmake's exit error with a hint
// when the captured stderr matches a well-known incompatibility
// pattern. The base error is returned unwrapped (no annotation)
// when no pattern matches so unrelated failures don't get
// misleading guidance.
//
// The hint becomes part of the failure.ConfigureFailed message
// surfaced by convert-element-cmake's exit path, so operators
// see it in the same stderr stream as the cmake error itself
// without having to consult docs for a separate decoder ring.
func annotateConfigureFailure(baseErr error, stderr []byte) error {
	for _, pat := range configureHints {
		if pat.match(stderr) {
			return fmt.Errorf("cmakerun: cmake failed: %w\n\n[hint] %s", baseErr, pat.hint)
		}
	}
	return fmt.Errorf("cmakerun: cmake failed: %w", baseErr)
}

// configureHint pairs a stderr-pattern detector with the
// operator-facing remediation note it should print.
type configureHint struct {
	hint  string
	match func([]byte) bool
}

// configureHints is the ordered list of recognised cmake
// failure shapes. First match wins; keep narrowest patterns
// first if a future hint risks overlapping with another.
var configureHints = []configureHint{
	{
		// cmake's try_compile records the expected output
		// location of the just-compiled test artifact and then
		// reads it back to derive properties (CHECK_TYPE_SIZE,
		// CHECK_C_SOURCE_COMPILES, CHECK_SYMBOL_EXISTS, etc.).
		// When the recorded path doesn't exist, cmake emits
		//
		//   CMake Error: Recorded try_compile output location
		//   doesn't exist: <build>/CMakeFiles/CMakeScratch/
		//   TryCompile-XXXXXX/cmTC_YYYYY
		//
		// Common cause: an external toolchain file sets
		// `set(CMAKE_TRY_COMPILE_TARGET_TYPE STATIC_LIBRARY)`
		// to skip linker time on user-code try_compile probes,
		// but the cmake target-type-vs-output-name mapping for
		// the static-lib case drifts in some cmake releases —
		// the recorded location is `cmTC_YYYYY` (no prefix /
		// suffix) while the actual artifact lands at
		// `libcmTC_YYYYY.a`. The convert-element-cmake
		// derive-toolchain output deliberately doesn't set
		// CMAKE_TRY_COMPILE_TARGET_TYPE for exactly this
		// reason (see converter/internal/emit/cmaketoolchain/
		// emit.go's comment block); operators threading in
		// their own toolchain via --toolchain-cmake-file may
		// hit this when the toolchain forces the static-lib
		// shape. See #205.
		hint: "cmake's try_compile recorded an output path that doesn't exist. Most common trigger: the active toolchain file sets `CMAKE_TRY_COMPILE_TARGET_TYPE STATIC_LIBRARY`, which mismatches cmake's recorded-vs-actual output naming for the static-lib case in some releases.\n" +
			"  Workarounds (in preference order):\n" +
			"    1. Remove `set(CMAKE_TRY_COMPILE_TARGET_TYPE STATIC_LIBRARY)` from the toolchain file passed via --toolchain-cmake-file; let cmake try_compile produce an executable (the converter's own derive-toolchain output already does this).\n" +
			"    2. Pre-seed the cmake cache with the feature-detection results the project's try_compile probes — set e.g. `CHECK_TYPE_SIZE_<X>`, `CHECK_C_SOURCE_COMPILES_<X>`, `HAVE_<X>` via -DCACHE entries so cmake skips the try_compile altogether.\n" +
			"    3. Try a newer cmake release; the recorded-vs-actual mismatch has been fixed in some upstreams.",
		match: matchTryCompileMissingOutput,
	},
	{
		// cmake 4.x removed the OLD behaviour of CMP0026,
		// so legacy packages that read
		// `get_target_property(<var> <tgt> LOCATION)` (the
		// pre-3.0 idiom for resolving an executable target
		// to its on-disk path) fatal-error at configure
		// time. The fix lives one level up: rewrite the
		// pattern to `$<TARGET_FILE:<tgt>>` in
		// CMakeLists.txt and *.cmake files before
		// convert-element-cmake runs, e.g. via Bazel's
		// http_archive(patch_cmds = …) or an in-place sed
		// pass over the unpacked source. cmake 3.x with
		// `-DCMAKE_POLICY_DEFAULT_CMP0026=OLD` works as a
		// stopgap but only if `cmake_minimum_required()`
		// hasn't already forced CMP0026 to NEW; in cmake
		// 4.x the policy is gone entirely and the override
		// is rejected with "policy CMP0026 was removed".
		// See docs/cmake-conversion-deltas.md for the full
		// recipe.
		hint: "cmake 4.x removed the OLD behaviour of CMP0026; legacy `get_target_property(<var> <tgt> LOCATION)` calls now fatal-error.\n" +
			"  Workarounds (in preference order):\n" +
			"    1. Patch the unpacked source so each call becomes `set(<var> $<TARGET_FILE:<tgt>>)`. With Bazel's http_archive, pass this through patch_cmds:\n" +
			"         find . \\( -name CMakeLists.txt -o -name '*.cmake' \\) -exec sed -i -E 's/get_target_property\\(([^ ]+) +([^ ]+) +LOCATION\\)/set(\\1 $<TARGET_FILE:\\2>)/g' {} +\n" +
			"    2. Re-run convert-element-cmake with --cmp0026-shim. The shim wraps get_target_property to translate LOCATION queries into $<TARGET_FILE:<tgt>> at configure time without touching the source tree. Caveat: the wrapper returns a generator expression rather than a configure-time-resolved path, so projects that string-compose LOCATION values at configure time (e.g. into a message() call) will see literal `$<TARGET_FILE:foo>` text. See #208.\n" +
			"    3. Override the orchestrator's cmake to a 3.x release (the Makefile's CMAKE_VERSION pin now tracks cmake 4.x; set e.g. CMAKE_VERSION=3.28.3 to downgrade); cmake 3.x emits a deprecation warning but still resolves LOCATION.\n" +
			"  See docs/cmake-conversion-deltas.md for the catalogue entry.",
		match: matchCMP0026,
	},
}

// matchCMP0026 reports whether the recorded stderr names the
// CMP0026 / LOCATION-read fatal pattern. cmake emits both
// strings together for this specific failure; matching on the
// "LOCATION property may not be read" sentinel is narrower
// than just "CMP0026" (which can also surface from
// cmake_policy() interrogations that aren't actually broken).
//
// The match is intentionally cmake-wording-tied: it keys on the
// exact sentinel cmake 3.x / 4.x emit today. A future release
// that rephrases the diagnostic silently stops firing the hint
// — the converter's behaviour stays correct (the underlying
// configure error still surfaces), only the [hint] annotation
// is missed. Re-test against the latest cmake when bumping the
// pinned version; refresh the sentinel here if the wording
// changes.
func matchCMP0026(stderr []byte) bool {
	s := string(stderr)
	if !strings.Contains(s, "LOCATION property may not be read") {
		return false
	}
	return strings.Contains(s, "CMP0026") || strings.Contains(s, "add_custom_command") || strings.Contains(s, "get_target_property")
}

// matchPolicyFloorRemoved reports whether the recorded stderr is the
// cmake 4.x fatal that fires when a project's cmake_minimum_required
// declares a floor below 3.5 — the compatibility cmake 4 dropped. cmake
// prints the sentinel together with the exact remediation
// (CMAKE_POLICY_VERSION_MINIMUM), e.g.:
//
//	CMake Error at CMakeLists.txt:1 (cmake_minimum_required):
//	  Compatibility with CMake < 3.5 has been removed from CMake.
//	  Or, add -DCMAKE_POLICY_VERSION_MINIMUM=3.5 to try configuring anyway.
//
// Configure keys an automatic one-shot retry (with the policy bump) on
// this match. Matching on the "Compatibility with CMake <" + "has been
// removed" pair keeps it narrower than a bare "CMAKE_POLICY_VERSION_MINIMUM"
// scan (which the remediation line for unrelated policy errors could also
// carry). cmake-wording-tied like matchCMP0026: a rephrase silently stops
// the retry, leaving the underlying failure to surface normally — re-test
// when bumping the pinned cmake.
func matchPolicyFloorRemoved(stderr []byte) bool {
	s := string(stderr)
	return strings.Contains(s, "Compatibility with CMake <") &&
		strings.Contains(s, "has been removed")
}

// matchTryCompileMissingOutput recognises the cmake try_compile
// "Recorded try_compile output location doesn't exist" diagnostic
// — most often surfaced by toolchain files that force
// CMAKE_TRY_COMPILE_TARGET_TYPE to STATIC_LIBRARY in cmake
// releases where the recorded vs actual output name disagrees.
// See #205.
//
// The sentinel is cmake's exact wording; an upstream rephrase
// would silently stop firing the hint without affecting the
// underlying failure surface. Re-test when bumping the pinned
// cmake.
func matchTryCompileMissingOutput(stderr []byte) bool {
	return strings.Contains(string(stderr), "Recorded try_compile output location doesn't exist")
}
