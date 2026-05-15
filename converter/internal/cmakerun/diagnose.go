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
			"    2. Pin the orchestrator's cmake to a 3.x release (the Makefile's CMAKE_VERSION pin is 3.28.3); cmake 3.x emits a deprecation warning but still resolves LOCATION.\n" +
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
