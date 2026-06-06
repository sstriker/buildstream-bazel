package todos

import (
	"os"
	"strings"
)

// DefaultPreamble is the built-in operator preamble: the standing
// guidance the AI post-pass reads before working the todo list. It
// encodes this repo's transition-to-plain-Bazel intent and ships the
// brotli worked example. Operators override it with
// --conversion-todos-preamble=<file> (see LoadPreamble).
func DefaultPreamble() Preamble {
	return Preamble{
		Intent: "This project is being moved off cmake onto plain, idiomatic Bazel. " +
			"Author the target a Bazel maintainer would write by hand — a native " +
			"Bazel rule driving the *built artifact* — not a wrapper that re-invokes " +
			"`cmake -P` or shells out to the cmake harness. Prefer bazel_skylib " +
			"diff_test / sh_test over re-running cmake.",
		Rules: strings.Join([]string{
			"(1) Author into the designated authored-output file — never the " +
				"converter-owned BUILD.bazel.out nor the stage-b-derived BUILD.bazel " +
				"(the converter regenerates BUILD.bazel.out wholesale and stage-b " +
				"overwrites BUILD.bazel from it).",
			"(2) One reusable macro per shared unit, instantiated N times — not N " +
				"near-duplicate targets.",
			"(3) Preserve the recovered `verification` as the test's assertion.",
			"(4) Your output crosses the same trust boundary as mechanical output: " +
				"it must pass the render gates (buildifier -mode=diff no-op, gazelle " +
				"roundtrip, bazel build/test) — it is not trusted on faith.",
		}, " "),
		Example: "28 add_test(… COMMAND cmake -P run_test.cmake <input>) share one " +
			"runner whose contract is \"compress then decompress <input> with the " +
			"built brotli CLI and assert the result equals <input>.\" Author ONE " +
			"brotli_roundtrip_test macro wrapping a diff_test (or sh_test) over " +
			"//:brotli, then instantiate it over the input list — one prompt, one " +
			"macro, 28 cheap call sites.",
	}
}

// LoadPreamble returns the operator-supplied preamble read from path,
// or the built-in DefaultPreamble when path is empty. An override file
// is read verbatim as the whole preamble block (its bytes become Text),
// so operators write prose, not JSON.
func LoadPreamble(path string) (Preamble, error) {
	if path == "" {
		return DefaultPreamble(), nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return Preamble{}, err
	}
	return Preamble{Text: strings.TrimRight(string(b), "\n")}, nil
}
